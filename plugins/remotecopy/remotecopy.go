// Package remotecopy è il primo plugin di deploy: copia l'artefatto su server remoti via SSH/SFTP,
// ne verifica l'integrità e, opzionalmente, esegue un comando post-copia.
package remotecopy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Name è il nome con cui il plugin si registra.
const Name = "remote-copy"

// Parametri del descrittore (deploy.params) letti dal plugin.
const (
	ParamDestDir        = "dest_dir"        // obbligatorio: directory remota di destinazione
	ParamPostCommand    = "post_command"    // opzionale: comando eseguito dopo la copia
	ParamSSHKeySecret   = "ssh_key_secret"  // nome del segreto con la chiave privata PEM
	ParamPasswordSecret = "password_secret" // in alternativa: nome del segreto con la password
	ParamVerify         = "verify_checksum" // "false" per disattivare la verifica sha256 remota
)

// Auth contiene le credenziali risolte al momento della connessione.
type Auth struct {
	PrivateKey string
	Password   string
}

// Session è una connessione verso un singolo host remoto.
type Session interface {
	Upload(ctx context.Context, localPath, remotePath string) error
	Run(ctx context.Context, cmd string) (string, error)
	Close() error
}

// Dialer apre sessioni verso gli host remoti; astratto per poterlo sostituire nei test.
type Dialer interface {
	Dial(ctx context.Context, target plugin.Target, auth Auth) (Session, error)
}

// Plugin implementa plugin.Deployer.
type Plugin struct {
	dialer Dialer
	types  []string
	logger *slog.Logger
}

// New crea il plugin con il dialer indicato e i tipi di artefatto supportati.
func New(dialer Dialer, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{dialer: dialer, types: types, logger: logger}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - known_hosts: percorso del file known_hosts (default ~/.ssh/known_hosts)
//   - insecure_ignore_host_key: true per saltare la verifica della host key (solo sviluppo)
//   - artifact_types: lista dei tipi supportati (default ["application"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	knownHosts, _ := cfg["known_hosts"].(string)
	insecure, _ := cfg["insecure_ignore_host_key"].(bool)
	types := []string{"application"}
	if raw, ok := cfg["artifact_types"].([]any); ok {
		types = types[:0]
		for _, t := range raw {
			s, ok := t.(string)
			if !ok {
				return nil, fmt.Errorf("artifact_types deve contenere stringhe")
			}
			types = append(types, s)
		}
	}
	return New(&SSHDialer{KnownHostsPath: knownHosts, InsecureIgnoreHostKey: insecure}, types, logger), nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool {
	for _, t := range p.types {
		if t == artifactType {
			return true
		}
	}
	return false
}

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if req.Params[ParamDestDir] == "" {
		errs = append(errs, fmt.Errorf("parametro %q obbligatorio", ParamDestDir))
	}
	if req.Params[ParamSSHKeySecret] == "" && req.Params[ParamPasswordSecret] == "" {
		errs = append(errs, fmt.Errorf("indicare %q o %q", ParamSSHKeySecret, ParamPasswordSecret))
	}
	if req.Artifact.LocalPath == "" {
		errs = append(errs, errors.New("artefatto locale mancante (step artifacts non eseguito?)"))
	}
	for _, t := range req.Targets {
		if t.User == "" {
			errs = append(errs, fmt.Errorf("target %s: user obbligatorio", t.Host))
		}
	}
	return errors.Join(errs...)
}

// Deploy copia l'artefatto su ogni target in sequenza; si ferma al primo errore.
// La copia sovrascrive il file remoto, quindi l'operazione è idempotente.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	remote := path.Join(req.Params[ParamDestDir], path.Base(req.Artifact.LocalPath))
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, auth, req, remote)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request, remote string) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()

	if _, err := sess.Run(ctx, "mkdir -p "+shellQuote(req.Params[ParamDestDir])); err != nil {
		return nil, fmt.Errorf("creazione directory remota: %w", err)
	}
	if err := sess.Upload(ctx, req.Artifact.LocalPath, remote); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	log.InfoContext(ctx, "artefatto copiato", "remote_path", remote)
	details := map[string]string{"remote_path": remote}

	if req.Params[ParamVerify] != "false" && req.Artifact.SHA256 != "" {
		out, err := sess.Run(ctx, "sha256sum "+shellQuote(remote))
		if err != nil {
			return nil, fmt.Errorf("verifica checksum: %w", err)
		}
		got, _, _ := strings.Cut(strings.TrimSpace(out), " ")
		if !strings.EqualFold(got, req.Artifact.SHA256) {
			return nil, fmt.Errorf("checksum remoto %s diverso da %s", got, req.Artifact.SHA256)
		}
		details["checksum"] = "verified"
	}
	if cmd := req.Params[ParamPostCommand]; cmd != "" {
		out, err := sess.Run(ctx, cmd)
		if err != nil {
			return nil, fmt.Errorf("post_command: %w", err)
		}
		log.InfoContext(ctx, "post_command eseguito", "output", strings.TrimSpace(out))
		details["post_command"] = "ok"
	}
	return details, nil
}

// resolveAuth legge le credenziali dai segreti solo al momento del deploy.
func (p *Plugin) resolveAuth(ctx context.Context, req *plugin.Request) (Auth, error) {
	var a Auth
	var err error
	if name := req.Params[ParamSSHKeySecret]; name != "" {
		if a.PrivateKey, err = req.Secrets.Secret(ctx, name); err != nil {
			return a, err
		}
	}
	if name := req.Params[ParamPasswordSecret]; name != "" {
		if a.Password, err = req.Secrets.Secret(ctx, name); err != nil {
			return a, err
		}
	}
	return a, nil
}

// shellQuote racchiude un argomento tra apici singoli per la shell remota.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
