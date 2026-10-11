// Package container rilascia un'immagine di container su server o VM via SSH, con Docker o
// Podman: scarica l'immagine fissata al digest, sostituisce il container dell'applicazione e
// attende che sia in esecuzione (e "healthy", se l'immagine ha un HEALTHCHECK).
//
// L'artefatto è un'immagine di un registry (sorgente oci://registry/repository:tag): manent ne
// risolve il digest, che diventa lo sha256 dell'artefatto, e il plugin avvia sempre
// "repository@sha256:<digest>". Versione e immagine sono scritte nelle etichette del
// container, da cui il plugin legge la release in esercizio e torna alla precedente.
package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

// Name è il nome con cui il plugin si registra.
const Name = "container"

// Parametri del descrittore (deploy.params) letti dal plugin.
const (
	ParamRuntime        = "runtime"        // docker (default) o podman
	ParamContainerName  = "container_name" // default: nome dell'applicazione
	ParamRunArgs        = "run_args"       // argomenti aggiuntivi di "run" (porte, volumi, env-file, rete)
	ParamCommand        = "command"        // argomenti passati al container dopo l'immagine
	ParamSSHKeySecret   = "ssh_key_secret"
	ParamPasswordSecret = "password_secret"
	ParamSudo           = "sudo"                     // "true": comandi del runtime con sudo -n
	ParamRegistryUser   = "registry_username"        // login al registry prima del pull
	ParamRegistrySecret = "registry_password_secret" // segreto con la password del registry
	ParamWaitTimeout    = "wait_timeout"             // attesa dell'avvio (default 60s)
	ParamStableFor      = "stable_for"               // senza HEALTHCHECK: tempo in esecuzione senza riavvii (default 5s)
)

// Etichette scritte sul container.
const (
	LabelApplication = "manent.application"
	LabelEnvironment = "manent.environment"
	LabelVersion     = "manent.version"
	LabelDeployment  = "manent.deployment"
)

// DetailImage è la chiave di Release.Details con l'immagine della release.
const DetailImage = "image"

const (
	defaultWaitTimeout = 60 * time.Second
	defaultStableFor   = 5 * time.Second
)

var (
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

// Plugin implementa plugin.Deployer, DryRunner, ReleaseInspector e Rollbacker.
type Plugin struct {
	dialer remotecopy.Dialer
	types  []string
	logger *slog.Logger
	// poll è l'intervallo dei controlli durante l'attesa dell'avvio.
	poll time.Duration
}

// New crea il plugin con il dialer indicato e i tipi di artefatto supportati.
func New(dialer remotecopy.Dialer, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{dialer: dialer, types: types, logger: logger, poll: time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - known_hosts, insecure_ignore_host_key: come remote-copy
//   - artifact_types: tipi supportati (default ["container"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	knownHosts, _ := cfg["known_hosts"].(string)
	insecure, _ := cfg["insecure_ignore_host_key"].(bool)
	types := []string{"container"}
	if raw, ok := cfg["artifact_types"].([]any); ok {
		types = types[:0]
		for _, t := range raw {
			s, ok := t.(string)
			if !ok {
				return nil, errors.New("artifact_types deve contenere stringhe")
			}
			types = append(types, s)
		}
	}
	return New(&remotecopy.SSHDialer{KnownHostsPath: knownHosts, InsecureIgnoreHostKey: insecure}, types, logger), nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if r := req.Params[ParamRuntime]; r != "" && r != "docker" && r != "podman" {
		errs = append(errs, fmt.Errorf("%s %q non valido (docker|podman)", ParamRuntime, r))
	}
	if !nameRe.MatchString(containerName(req)) {
		errs = append(errs, fmt.Errorf("%s %q non valido: ammessi lettere, cifre, '_', '.', '-'", ParamContainerName, containerName(req)))
	}
	if !versionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e diventa un'etichetta del container: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if req.Params[ParamSSHKeySecret] == "" && req.Params[ParamPasswordSecret] == "" {
		errs = append(errs, fmt.Errorf("indicare %q o %q", ParamSSHKeySecret, ParamPasswordSecret))
	}
	if (req.Params[ParamRegistryUser] == "") != (req.Params[ParamRegistrySecret] == "") {
		errs = append(errs, fmt.Errorf("%q e %q vanno indicati insieme", ParamRegistryUser, ParamRegistrySecret))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("l'artefatto non è un'immagine di un registry: indicare artifact.source oci://registry/repository:tag"))
	}
	for _, k := range []string{ParamWaitTimeout, ParamStableFor} {
		if _, err := duration(req, k, 0); err != nil {
			errs = append(errs, err)
		}
	}
	for _, t := range req.Targets {
		if t.User == "" {
			errs = append(errs, fmt.Errorf("target %s: user obbligatorio", t.Host))
		}
	}
	return errors.Join(errs...)
}

func containerName(req *plugin.Request) string {
	if n := req.Params[ParamContainerName]; n != "" {
		return n
	}
	return req.Application
}

func duration(req *plugin.Request, key string, def time.Duration) (time.Duration, error) {
	raw := req.Params[key]
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q non valido: atteso una durata positiva (es. 90s)", key, raw)
	}
	return d, nil
}

// runtime restituisce il comando del runtime, con sudo se richiesto.
func runtime(req *plugin.Request) string {
	r := req.Params[ParamRuntime]
	if r == "" {
		r = "docker"
	}
	if req.Params[ParamSudo] == "true" {
		return "sudo -n " + r
	}
	return r
}

// running descrive il container dell'applicazione su un target.
type running struct {
	Version string
	Image   string
	Status  string
}

// inspect legge versione, immagine e stato del container; nil se non esiste.
func inspect(ctx context.Context, sess remotecopy.Session, rt, name string) (*running, error) {
	q := remotecopy.ShellQuote
	format := `{{index .Config.Labels "` + LabelVersion + `"}}|{{.Config.Image}}|{{.State.Status}}`
	out, err := sess.Run(ctx, "if "+rt+" container inspect "+q(name)+" >/dev/null 2>&1; then "+
		rt+" container inspect -f "+q(format)+" "+q(name)+"; fi")
	if err != nil {
		return nil, fmt.Errorf("lettura del container %s: %w", name, err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return nil, nil
	}
	parts := strings.SplitN(line, "|", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("stato del container %s non leggibile: %q", name, line)
	}
	return &running{Version: parts[0], Image: parts[1], Status: parts[2]}, nil
}

// Deploy rilascia l'immagine su ogni target in sequenza; si ferma al primo errore. Se il
// container in esercizio ha già la stessa immagine e versione non viene riavviato, quindi
// l'operazione è idempotente.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.release(ctx, t, auth, req, image, req.Artifact.Version, false)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

// release porta il container di un target all'immagine e alla versione indicate.
func (p *Plugin) release(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request, image, version string, rollback bool) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "rollback", rollback)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()

	rt, name, q := runtime(req), containerName(req), remotecopy.ShellQuote
	details := map[string]string{"container": name, DetailImage: image}
	cur, err := inspect(ctx, sess, rt, name)
	if err != nil {
		return details, err
	}
	if cur != nil {
		details["previous_version"], details["previous_image"] = cur.Version, cur.Image
		if cur.Image == image && cur.Version == version && cur.Status == "running" {
			// Già sulla release richiesta (es. retry o recovery): non viene riavviato se è pronto.
			if state, err := p.wait(ctx, sess, rt, name, req); err == nil {
				log.InfoContext(ctx, "container già sulla release richiesta", "container", name, "version", version)
				details["unchanged"], details["state"] = "true", state
				return details, nil
			}
			log.WarnContext(ctx, "container sulla release richiesta ma non pronto: viene ricreato", "container", name)
		}
	}
	if err := p.login(ctx, sess, rt, req, image); err != nil {
		return details, err
	}
	out, err := sess.Run(ctx, rt+" pull "+q(image))
	if err != nil {
		return details, fmt.Errorf("pull di %s: %w", image, err)
	}
	log.InfoContext(ctx, "immagine scaricata", "image", image, "output", lastLine(out))
	run := []string{rt, "run", "-d", "--name", q(name), "--restart", "unless-stopped",
		"--label", q(LabelApplication + "=" + req.Application),
		"--label", q(LabelEnvironment + "=" + req.Environment),
		"--label", q(LabelVersion + "=" + version),
		"--label", q(LabelDeployment + "=" + req.DeploymentID)}
	if a := req.Params[ParamRunArgs]; a != "" {
		run = append(run, a)
	}
	run = append(run, q(image))
	if c := req.Params[ParamCommand]; c != "" {
		run = append(run, c)
	}
	// Il container precedente viene rimosso solo dopo il pull riuscito, per ridurre il fermo.
	if _, err := sess.Run(ctx, rt+" rm -f "+q(name)+" >/dev/null 2>&1; "+strings.Join(run, " ")); err != nil {
		return details, fmt.Errorf("avvio del container %s: %w", name, err)
	}
	log.InfoContext(ctx, "container avviato", "container", name, "image", image, "version", version)
	state, err := p.wait(ctx, sess, rt, name, req)
	details["state"] = state
	if err != nil {
		if logs, lerr := sess.Run(ctx, rt+" logs --tail 20 "+q(name)+" 2>&1"); lerr == nil && strings.TrimSpace(logs) != "" {
			log.WarnContext(ctx, "ultime righe del log del container", "container", name, "logs", strings.TrimSpace(logs))
		}
		return details, err
	}
	log.InfoContext(ctx, "container pronto", "container", name, "state", state)
	return details, nil
}

// login esegue il login al registry dell'immagine passando la password sullo stdin.
func (p *Plugin) login(ctx context.Context, sess remotecopy.Session, rt string, req *plugin.Request, image string) error {
	user := req.Params[ParamRegistryUser]
	if user == "" {
		return nil
	}
	sr, ok := sess.(remotecopy.StdinRunner)
	if !ok {
		return errors.New("la sessione SSH non supporta il login al registry")
	}
	pw, err := req.Secrets.Secret(ctx, req.Params[ParamRegistrySecret])
	if err != nil {
		return err
	}
	registry, _, _ := strings.Cut(image, "/")
	q := remotecopy.ShellQuote
	if _, err := sr.RunStdin(ctx, rt+" login -u "+q(user)+" --password-stdin "+q(registry), strings.NewReader(pw)); err != nil {
		return fmt.Errorf("login al registry %s: %w", registry, err)
	}
	return nil
}

// wait attende che il container sia in esecuzione: con un HEALTHCHECK finché è "healthy",
// altrimenti per stable_for senza riavvii. Un container uscito o "unhealthy" è un errore.
func (p *Plugin) wait(ctx context.Context, sess remotecopy.Session, rt, name string, req *plugin.Request) (string, error) {
	timeout, _ := duration(req, ParamWaitTimeout, defaultWaitTimeout)
	stable, _ := duration(req, ParamStableFor, defaultStableFor)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := remotecopy.ShellQuote
	format := `{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}|{{.RestartCount}}`
	var since time.Time
	restarts, last := "", ""
	for {
		out, err := sess.Run(ctx, rt+" container inspect -f "+q(format)+" "+q(name))
		if err != nil {
			if ctx.Err() != nil {
				return last, fmt.Errorf("container %s non pronto entro %s (stato %s)", name, timeout, last)
			}
			return last, fmt.Errorf("stato del container %s: %w", name, err)
		}
		parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		status, health, count := parts[0], parts[1], parts[2]
		last = strings.Trim(status+" "+health, " ")
		switch {
		case status == "exited" || status == "dead":
			return last, fmt.Errorf("il container %s è terminato (stato %s)", name, status)
		case health == "unhealthy":
			return last, fmt.Errorf("il container %s è unhealthy", name)
		case status == "running" && health == "healthy":
			return last, nil
		case status == "running" && health == "":
			if since.IsZero() || count != restarts {
				since, restarts = time.Now(), count
			} else if time.Since(since) >= stable {
				return last, nil
			}
		default:
			since = time.Time{}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("container %s non pronto entro %s (stato %s)", name, timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// CurrentRelease legge su ogni target versione e immagine del container in esercizio. Un
// target senza container (es. appena aggiunto) viene ignorato; release diverse tra i target
// sono un errore, perché non sarebbe chiaro a quale tornare.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	type release struct{ version, image string }
	found := map[release][]string{}
	for _, t := range req.Targets {
		cur, err := p.currentOn(ctx, t, auth, req)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		if cur == nil || cur.Version == "" {
			continue
		}
		r := release{cur.Version, cur.Image}
		found[r] = append(found[r], t.Host)
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for r, hosts := range found {
			return &plugin.Release{Version: r.version, Details: map[string]string{
				DetailImage: r.image, "source": "etichette del container", "targets": strings.Join(hosts, ","),
			}}, nil
		}
	}
	var desc []string
	for r, hosts := range found {
		desc = append(desc, r.version+" ("+r.image+") su "+strings.Join(hosts, ","))
	}
	slices.Sort(desc)
	return nil, fmt.Errorf("release diverse sui target: %s", strings.Join(desc, "; "))
}

func (p *Plugin) currentOn(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request) (*running, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()
	return inspect(ctx, sess, runtime(req), containerName(req))
}

// Rollback riporta il container di ogni target all'immagine della release indicata (letta
// dalle etichette prima del deploy). Prosegue anche se un target fallisce, per ripristinare il
// maggior numero di target, e riporta gli errori alla fine. È idempotente.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	image := previous.Details[DetailImage]
	if image == "" || !strings.Contains(image, "@sha256:") {
		return nil, fmt.Errorf("release %s senza immagine fissata al digest: rollback impossibile", previous.Version)
	}
	if !versionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("versione %q non valida", previous.Version)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.release(ctx, t, auth, req, image, previous.Version, true)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

// DryRun verifica su ogni target, senza modificarlo, che il deploy sia eseguibile: connessione,
// runtime raggiungibile con i permessi dell'utente e release in esercizio. Il pull non viene
// eseguito. A differenza di Deploy controlla tutti i target.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.check(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) check(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request) (map[string]string, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connessione: %w", err)
	}
	defer sess.Close()
	rt := runtime(req)
	details := map[string]string{"container": containerName(req)}
	out, err := sess.Run(ctx, rt+" version --format '{{.Server.Version}}'")
	if err != nil {
		return details, fmt.Errorf("runtime %s non utilizzabile dall'utente %s: %w", rt, t.User, err)
	}
	details["runtime_version"] = strings.TrimSpace(out)
	cur, err := inspect(ctx, sess, rt, containerName(req))
	if err != nil {
		return details, err
	}
	if cur != nil {
		details["current_version"], details["current_image"] = cur.Version, cur.Image
	}
	if image, err := plugin.ImageReference(req.Artifact); err == nil {
		details[DetailImage] = image
	}
	details["pull"] = "skipped"
	p.logger.InfoContext(ctx, "target pronto per il deploy", "deployment", req.DeploymentID, "host", t.Host, "dry_run", true)
	return details, nil
}

// resolveAuth legge le credenziali SSH dai segreti solo al momento dell'uso.
func (p *Plugin) resolveAuth(ctx context.Context, req *plugin.Request) (remotecopy.Auth, error) {
	var a remotecopy.Auth
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

// lastLine restituisce l'ultima riga non vuota dell'output (es. il digest stampato dal pull).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
