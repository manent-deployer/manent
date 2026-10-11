// Package remotecopy è il primo plugin di deploy: copia l'artefatto su server remoti via SSH/SFTP,
// ne verifica l'integrità e, opzionalmente, esegue un comando post-copia.
//
// Con il layout "releases" ogni versione ha la sua directory (<dest_dir>/releases/<versione>) e
// il symlink <dest_dir>/current indica quella in esercizio: il plugin sa quindi leggere la
// release corrente e tornare alla precedente (rollback).
package remotecopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
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
	// ParamLayout è "flat" (default: file in dest_dir) o "releases" (necessario per il rollback).
	ParamLayout = "layout"
	// ParamKeepReleases è il numero di release conservate con il layout releases (default 5, minimo 2).
	ParamKeepReleases = "keep_releases"
	// ParamPrepareCommand, solo con il layout releases, è eseguito prima dello switch (es. estrazione).
	ParamPrepareCommand = "prepare_command"
	// ParamRollbackCommand è eseguito dopo il rollback su ogni target (default: post_command).
	ParamRollbackCommand = "rollback_command"
)

// Layout della directory di destinazione.
const (
	LayoutFlat     = "flat"
	LayoutReleases = "releases"
)

// defaultKeepReleases è il numero di release conservate se keep_releases non è indicato.
const defaultKeepReleases = 5

// releaseRe limita le versioni, che con il layout releases diventano nomi di directory remote.
var releaseRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

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

// StdinRunner è implementato dalle sessioni che possono passare dati sullo stdin di un comando
// (es. una password), senza farli comparire nella riga di comando remota.
type StdinRunner interface {
	RunStdin(ctx context.Context, cmd string, stdin io.Reader) (string, error)
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
	switch layout(req) {
	case LayoutFlat:
		if req.Params[ParamPrepareCommand] != "" {
			errs = append(errs, fmt.Errorf("%q richiede layout %q", ParamPrepareCommand, LayoutReleases))
		}
	case LayoutReleases:
		if !releaseRe.MatchString(req.Artifact.Version) {
			errs = append(errs, fmt.Errorf("con layout %q artifact.version è obbligatorio e diventa il nome della directory: ammessi lettere, cifre, '.', '_', '+', '-'", LayoutReleases))
		}
		if _, err := keepReleases(req); err != nil {
			errs = append(errs, err)
		}
	default:
		errs = append(errs, fmt.Errorf("%s %q non valido (%s|%s)", ParamLayout, req.Params[ParamLayout], LayoutFlat, LayoutReleases))
	}
	return errors.Join(errs...)
}

// layout restituisce il layout richiesto.
func layout(req *plugin.Request) string {
	if l := req.Params[ParamLayout]; l != "" {
		return l
	}
	return LayoutFlat
}

// keepReleases legge keep_releases: almeno 2, per conservare sempre la release precedente.
func keepReleases(req *plugin.Request) (int, error) {
	raw := req.Params[ParamKeepReleases]
	if raw == "" {
		return defaultKeepReleases, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2 {
		return 0, fmt.Errorf("%s deve essere un intero >= 2", ParamKeepReleases)
	}
	return n, nil
}

// paths calcola i percorsi remoti dell'artefatto e, con il layout releases, della release.
func paths(req *plugin.Request) (remote, releaseDir string) {
	dest := req.Params[ParamDestDir]
	if layout(req) == LayoutReleases {
		releaseDir = path.Join(dest, LayoutReleases, req.Artifact.Version)
		return path.Join(releaseDir, path.Base(req.Artifact.LocalPath)), releaseDir
	}
	return path.Join(dest, path.Base(req.Artifact.LocalPath)), ""
}

// Deploy copia l'artefatto su ogni target in sequenza; si ferma al primo errore.
// La copia sovrascrive il file remoto, quindi l'operazione è idempotente.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()

	dest := req.Params[ParamDestDir]
	remote, releaseDir := paths(req)
	details := map[string]string{"remote_path": remote}
	var previous string
	if releaseDir != "" {
		// La release corrente, letta prima dello switch, non viene mai eliminata dalla pulizia.
		if previous, err = readCurrent(ctx, sess, dest); err != nil {
			return nil, err
		}
	}
	if _, err := sess.Run(ctx, "mkdir -p "+shellQuote(path.Dir(remote))); err != nil {
		return nil, fmt.Errorf("creazione directory remota: %w", err)
	}
	if err := sess.Upload(ctx, req.Artifact.LocalPath, remote); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	log.InfoContext(ctx, "artefatto copiato", "remote_path", remote)

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
	env := ""
	if releaseDir != "" {
		env = commandEnv(dest, releaseDir, remote, req.Artifact.Version)
		if cmd := req.Params[ParamPrepareCommand]; cmd != "" {
			if err := p.runCommand(ctx, log, sess, "prepare_command", env+cmd); err != nil {
				return nil, err
			}
			details["prepare_command"] = "ok"
		}
		if err := switchCurrent(ctx, sess, dest, req.Artifact.Version); err != nil {
			return nil, err
		}
		details["release"], details["previous"] = req.Artifact.Version, previous
		log.InfoContext(ctx, "release attivata", "release", req.Artifact.Version, "previous", previous)
	}
	if cmd := req.Params[ParamPostCommand]; cmd != "" {
		if err := p.runCommand(ctx, log, sess, "post_command", env+cmd); err != nil {
			return nil, err
		}
		details["post_command"] = "ok"
	}
	if releaseDir != "" {
		keep, _ := keepReleases(req)
		if _, err := sess.Run(ctx, pruneCommand(dest, keep, req.Artifact.Version, previous)); err != nil {
			// La pulizia non compromette il deploy: le release in più restano sul target.
			log.WarnContext(ctx, "pulizia delle release precedenti fallita", "error", err)
		}
	}
	return details, nil
}

// runCommand esegue un comando configurato e ne registra l'output.
func (p *Plugin) runCommand(ctx context.Context, log *slog.Logger, sess Session, name, cmd string) error {
	out, err := sess.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	log.InfoContext(ctx, name+" eseguito", "output", strings.TrimSpace(out))
	return nil
}

// currentLink è il symlink alla release in esercizio, relativo a dest_dir.
const currentLink = "current"

// readCurrent restituisce la versione indicata dal symlink current, o "" se non esiste.
func readCurrent(ctx context.Context, sess Session, dest string) (string, error) {
	link := shellQuote(path.Join(dest, currentLink))
	out, err := sess.Run(ctx, "if [ -L "+link+" ]; then readlink "+link+"; fi")
	if err != nil {
		return "", fmt.Errorf("lettura della release corrente: %w", err)
	}
	if v := strings.TrimSpace(out); v != "" {
		return path.Base(v), nil
	}
	return "", nil
}

// switchCurrent punta current alla release indicata con uno scambio atomico del symlink
// (rename di un symlink temporaneo; richiede "mv -T" di GNU coreutils).
func switchCurrent(ctx context.Context, sess Session, dest, version string) error {
	tmp := shellQuote(path.Join(dest, ".current.tmp"))
	cmd := "ln -sfn " + shellQuote(path.Join(LayoutReleases, version)) + " " + tmp +
		" && mv -Tf " + tmp + " " + shellQuote(path.Join(dest, currentLink))
	if _, err := sess.Run(ctx, cmd); err != nil {
		return fmt.Errorf("switch della release %s: %w", version, err)
	}
	return nil
}

// pruneCommand elimina le release più vecchie (per data di modifica) oltre keep, senza mai
// toccare quella appena attivata e quella precedente.
func pruneCommand(dest string, keep int, current, previous string) string {
	exclude := []string{current}
	if previous != "" && previous != current {
		exclude = append(exclude, previous)
	}
	grep := "grep -vxF"
	for _, e := range exclude {
		grep += " -e " + shellQuote(e)
	}
	return "cd " + shellQuote(path.Join(dest, LayoutReleases)) + " && ls -1t | " + grep +
		" | tail -n +" + strconv.Itoa(keep-len(exclude)+1) +
		` | while IFS= read -r r; do rm -rf -- "$r"; done`
}

// commandEnv restituisce le assegnazioni delle variabili passate ai comandi configurati.
// MANENT_ARTIFACT è vuota nel rollback, quando il nome del file della release non è noto.
func commandEnv(dest, releaseDir, artifact, version string) string {
	vars := []string{
		"MANENT_RELEASE_DIR=" + shellQuote(releaseDir),
		"MANENT_ARTIFACT=" + shellQuote(artifact),
		"MANENT_VERSION=" + shellQuote(version),
		"MANENT_CURRENT=" + shellQuote(path.Join(dest, currentLink)),
	}
	return strings.Join(vars, " ") + "; export MANENT_RELEASE_DIR MANENT_ARTIFACT MANENT_VERSION MANENT_CURRENT; "
}

// CurrentRelease legge su ogni target la release indicata da current (layout releases).
// Un target senza current (es. appena aggiunto) viene ignorato; versioni diverse tra i target
// sono un errore, perché non sarebbe chiaro a quale release tornare.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	if layout(req) != LayoutReleases {
		return nil, fmt.Errorf("la release in esercizio si legge solo con %s %q", ParamLayout, LayoutReleases)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for _, t := range req.Targets {
		v, err := p.currentOn(ctx, t, auth, req.Params[ParamDestDir])
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		if v != "" {
			found[v] = append(found[v], t.Host)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v, hosts := range found {
			return &plugin.Release{Version: v, Details: map[string]string{"source": "symlink " + currentLink, "targets": strings.Join(hosts, ",")}}, nil
		}
	}
	var desc []string
	for _, v := range slices.Sorted(maps.Keys(found)) {
		desc = append(desc, v+" su "+strings.Join(found[v], ","))
	}
	return nil, fmt.Errorf("release diverse sui target: %s", strings.Join(desc, "; "))
}

func (p *Plugin) currentOn(ctx context.Context, t plugin.Target, auth Auth, dest string) (string, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return "", fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()
	return readCurrent(ctx, sess, dest)
}

// Rollback riporta current alla release indicata su tutti i target ed esegue rollback_command
// (o, se assente, post_command). Prosegue anche se un target fallisce, per ripristinare il
// maggior numero di target, e riporta gli errori alla fine. È idempotente.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if layout(req) != LayoutReleases {
		return nil, fmt.Errorf("il rollback richiede %s %q", ParamLayout, LayoutReleases)
	}
	if !releaseRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("versione %q non valida come nome di release", previous.Version)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.rollbackTarget(ctx, t, auth, req, previous.Version)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request, version string) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "rollback", true)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()

	dest := req.Params[ParamDestDir]
	releaseDir := path.Join(dest, LayoutReleases, version)
	if _, err := sess.Run(ctx, "test -d "+shellQuote(releaseDir)); err != nil {
		return nil, fmt.Errorf("release %s assente sul target: %w", version, err)
	}
	if err := switchCurrent(ctx, sess, dest, version); err != nil {
		return nil, err
	}
	details := map[string]string{"release": version}
	log.WarnContext(ctx, "release precedente ripristinata", "release", version)
	cmd := req.Params[ParamRollbackCommand]
	if cmd == "" {
		cmd = req.Params[ParamPostCommand]
	}
	if cmd != "" {
		// Le variabili indicano la release ripristinata.
		env := commandEnv(dest, releaseDir, "", version)
		if err := p.runCommand(ctx, log, sess, "rollback_command", env+cmd); err != nil {
			return details, err
		}
		details["rollback_command"] = "ok"
	}
	return details, nil
}

// DryRun verifica su ogni target, senza modificarlo, che il deploy sia eseguibile: connessione
// e credenziali, directory di destinazione scrivibile (o creabile) e, se serve, sha256sum.
// Il post_command non viene eseguito. A differenza di Deploy controlla tutti i target.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "dry_run", true)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()

	remote, releaseDir := paths(req)
	details := map[string]string{"remote_path": remote}
	if _, err := sess.Run(ctx, writableDirCheck(req.Params[ParamDestDir])); err != nil {
		return details, fmt.Errorf("directory remota non scrivibile: %w", err)
	}
	details["dest_dir"] = "writable"
	if releaseDir != "" {
		cur, err := readCurrent(ctx, sess, req.Params[ParamDestDir])
		if err != nil {
			return details, err
		}
		details["current"] = cur
		if req.Params[ParamPrepareCommand] != "" {
			details["prepare_command"] = "skipped"
		}
	}
	if req.Params[ParamVerify] != "false" && req.Artifact.SHA256 != "" {
		if _, err := sess.Run(ctx, "command -v sha256sum"); err != nil {
			return details, fmt.Errorf("sha256sum non disponibile: %w", err)
		}
		details["checksum"] = "available"
	}
	if req.Params[ParamPostCommand] != "" {
		details["post_command"] = "skipped"
	}
	log.InfoContext(ctx, "target pronto per il deploy", "remote_path", remote)
	return details, nil
}

// writableDirCheck restituisce un comando che, senza creare nulla, risale alla prima directory
// esistente del percorso (quella in cui "mkdir -p" creerebbe le mancanti) e ne verifica la scrittura.
func writableDirCheck(dir string) string {
	return "d=" + shellQuote(dir) + `; while [ ! -e "$d" ]; do d=$(dirname "$d"); done; test -d "$d" && test -w "$d"`
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

// ShellQuote racchiude un argomento tra apici singoli per la shell remota.
func ShellQuote(s string) string { return shellQuote(s) }

// shellQuote racchiude un argomento tra apici singoli per la shell remota.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
