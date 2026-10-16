// Package jboss rilascia applicazioni Java EE / Jakarta EE (war, ear) su JBoss EAP e WildFly
// tramite l'API HTTP di management, in standalone o in domain mode.
//
// Ogni versione diventa un deployment distinto, "<applicazione>-<versione>.<ext>", con lo
// stesso runtime-name (default "<applicazione>.<ext>"): il passaggio dalla versione in
// esercizio alla nuova è un'operazione composite (undeploy della vecchia, deploy della nuova)
// che il server applica in modo atomico. Le versioni precedenti restano nel repository dei
// contenuti, disattivate, fino a keep_releases: il rollback riattiva la precedente senza
// ricaricare nulla.
//
// Sospensione (graceful), reload e restart del server sono controllati con parametri booleani.
package jboss

import (
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name è il nome con cui il plugin si registra.
const Name = "jboss"

// Parametri del descrittore (deploy.params) letti dal plugin.
const (
	ParamUsername       = "username"        // utente di management
	ParamPasswordSecret = "password_secret" // segreto con la password di management
	ParamTLS            = "tls"             // https verso l'interfaccia di management (default true)
	ParamServerGroups   = "server_groups"   // domain mode: server group, separati da virgola
	ParamRuntimeName    = "runtime_name"    // default "<applicazione>.<ext>"
	ParamSuspend        = "suspend"         // sospende il server prima del cambio di versione
	ParamSuspendTimeout = "suspend_timeout" // attesa delle richieste in corso (default 60s)
	ParamReload         = "reload"          // reload del server dopo il cambio di versione
	ParamRestart        = "restart"         // restart della JVM dopo il cambio di versione
	ParamKeepReleases   = "keep_releases"   // versioni conservate, compresa l'attiva (default 3, minimo 2)
	ParamWaitTimeout    = "wait_timeout"    // attesa del server dopo reload/restart (default 5m)
)

// DetailDeployment è la chiave di Release.Details con il nome del deployment.
const DetailDeployment = "deployment"

const (
	defaultKeepReleases   = 3
	defaultSuspendTimeout = 60 * time.Second
	defaultWaitTimeout    = 5 * time.Minute
)

// extensions sono le estensioni di artefatto ammesse.
var extensions = []string{".war", ".ear", ".jar", ".rar", ".sar"}

// Plugin implementa plugin.Deployer, DryRunner, ReleaseInspector e Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	// poll è l'intervallo delle letture durante le attese.
	poll time.Duration
}

// New crea il plugin con il connector e i tipi di artefatto indicati.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - artifact_types: tipi supportati (default ["war", "ear"])
//   - ca_file: CA aggiuntive per l'interfaccia di management in https
//   - insecure_skip_tls_verify: non verifica il certificato (solo sviluppo)
//   - request_timeout: durata massima di una chiamata (default 10m, per i caricamenti)
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, "war", "ear")
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if f, _ := cfg["ca_file"].(string); f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: nessun certificato PEM", f)
		}
		tlsCfg.RootCAs = pool
	}
	if v, _ := cfg["insecure_skip_tls_verify"].(bool); v {
		tlsCfg.InsecureSkipVerify = true
	}
	timeout := 10 * time.Minute
	if s, _ := cfg["request_timeout"].(string); s != "" {
		if timeout, err = time.ParseDuration(s); err != nil || timeout <= 0 {
			return nil, fmt.Errorf("request_timeout %q non valido", s)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	// Un client che non segue i redirect: le credenziali vanno solo all'host indicato.
	hc := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return New(HTTPConnector{HTTP: hc}, types, logger), nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// options sono i parametri del deploy già letti e validati.
type options struct {
	username, passwordSecret string
	tls                      bool
	groups                   []string
	runtimeName, prefix, ext string
	suspend, reload, restart bool
	suspendTimeout, wait     time.Duration
	keep                     int
}

// parse legge e valida i parametri della richiesta.
func parse(req *plugin.Request) (options, error) {
	var errs []error
	o := options{username: req.Params[ParamUsername], passwordSecret: req.Params[ParamPasswordSecret], prefix: req.Application + "-"}
	if o.username == "" || o.passwordSecret == "" {
		errs = append(errs, fmt.Errorf("%s e %s obbligatori", ParamUsername, ParamPasswordSecret))
	}
	boolean := func(key string, def bool) bool {
		raw := req.Params[key]
		if raw == "" {
			return def
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q non valido: atteso true o false", key, raw))
		}
		return v
	}
	o.tls = boolean(ParamTLS, true)
	o.suspend = boolean(ParamSuspend, false)
	o.reload = boolean(ParamReload, false)
	o.restart = boolean(ParamRestart, false)
	if o.reload && o.restart {
		errs = append(errs, fmt.Errorf("%s e %s sono alternativi: il restart ricarica già il server", ParamReload, ParamRestart))
	}
	for _, g := range strings.Split(req.Params[ParamServerGroups], ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.groups = append(o.groups, g)
		}
	}
	var err error
	if o.suspendTimeout, err = pluginutil.Duration(req.Params, ParamSuspendTimeout, defaultSuspendTimeout); err != nil {
		errs = append(errs, err)
	}
	if o.wait, err = pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	o.keep = defaultKeepReleases
	if raw := req.Params[ParamKeepReleases]; raw != "" {
		if o.keep, err = strconv.Atoi(raw); err != nil || o.keep < 2 {
			errs = append(errs, fmt.Errorf("%s %q non valido: almeno 2, per conservare la versione precedente", ParamKeepReleases, raw))
		}
	}
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e diventa parte del nome del deployment: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if !pluginutil.VersionRe.MatchString(req.Application) {
		errs = append(errs, fmt.Errorf("application %q non valida come nome di deployment", req.Application))
	}
	o.ext = artifactExt(req.Artifact)
	if o.ext == "" {
		errs = append(errs, fmt.Errorf("estensione dell'artefatto non riconosciuta: ammesse %s (dal file o da artifact.type)", strings.Join(extensions, ", ")))
	}
	o.runtimeName = cmp.Or(req.Params[ParamRuntimeName], req.Application+o.ext)
	if strings.ContainsAny(o.runtimeName, "/\\") || !slices.Contains(extensions, strings.ToLower(filepath.Ext(o.runtimeName))) {
		errs = append(errs, fmt.Errorf("%s %q non valido: un nome di file con estensione %s", ParamRuntimeName, o.runtimeName, strings.Join(extensions, ", ")))
	}
	for _, t := range req.Targets {
		if t.Host == "" || strings.ContainsAny(t.Host, "/?#@") {
			errs = append(errs, fmt.Errorf("target %q non valido: host dell'interfaccia di management", t.Host))
		}
	}
	return o, errors.Join(errs...)
}

// artifactExt restituisce l'estensione dell'artefatto: dal file scaricato o dal tipo.
func artifactExt(a plugin.Artifact) string {
	if e := strings.ToLower(filepath.Ext(a.LocalPath)); slices.Contains(extensions, e) {
		return e
	}
	if e := "." + strings.ToLower(a.Type); slices.Contains(extensions, e) {
		return e
	}
	return ""
}

// deploymentName è il nome del deployment di una versione.
func (o options) deploymentName(version string) string { return o.prefix + version + o.ext }

// versionOf ricava la versione dal nome di un deployment gestito da manent.
func (o options) versionOf(name string) (string, bool) {
	if !strings.HasPrefix(name, o.prefix) || !strings.HasSuffix(name, o.ext) {
		return "", false
	}
	v := strings.TrimSuffix(strings.TrimPrefix(name, o.prefix), o.ext)
	return v, v != ""
}

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	_, err := parse(req)
	return err
}

// Deploy rilascia la versione su ogni target in sequenza e si ferma al primo errore. Un
// target già sulla versione richiesta non viene toccato: l'operazione è idempotente.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	sum, err := contentHash(req.Artifact.LocalPath)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, o, plugin.StatusDeployed, func(s *session) (map[string]string, error) {
		return p.release(ctx, s, req, o, o.deploymentName(req.Artifact.Version), sum)
	})
}

// Rollback riattiva su ogni target il deployment della release precedente, ancora presente
// nel repository dei contenuti.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	name := previous.Details[DetailDeployment]
	if name == "" {
		name = o.deploymentName(previous.Version)
	}
	return p.each(ctx, req, o, plugin.StatusRolledBack, func(s *session) (map[string]string, error) {
		return p.release(ctx, s, req, o, name, "")
	})
}

// CurrentRelease legge dal primo target il deployment attivo con il runtime-name
// dell'applicazione; nil se non ce n'è.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	if len(req.Targets) == 0 {
		return nil, errors.New("nessun target")
	}
	s, err := p.open(ctx, req, o, req.Targets[0])
	if err != nil {
		return nil, err
	}
	deps, err := s.deployments(ctx)
	if err != nil {
		return nil, err
	}
	cur := active(deps, o.runtimeName)
	if cur == nil {
		return nil, nil
	}
	v, ok := o.versionOf(cur.Name)
	if !ok {
		// Deployment non creato da manent (es. rilasciato a mano): la versione non è nota, ma il
		// rollback può comunque riattivarlo per nome.
		v = cur.Name
	}
	return &plugin.Release{Version: v, Details: map[string]string{DetailDeployment: cur.Name}}, nil
}

// DryRun verifica credenziali, modalità, server group e stato di ogni target, senza modifiche.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, o, plugin.StatusReady, func(s *session) (map[string]string, error) {
		details := map[string]string{"mode": s.mode()}
		if !s.domain {
			state, err := s.attribute(ctx, nil, "server-state")
			if err != nil {
				return details, err
			}
			details["server_state"] = state
			if state != "running" {
				return details, fmt.Errorf("server nello stato %q", state)
			}
		}
		deps, err := s.deployments(ctx)
		if err != nil {
			return details, err
		}
		if cur := active(deps, o.runtimeName); cur != nil {
			details["current"] = cur.Name
		}
		details[DetailDeployment] = o.deploymentName(req.Artifact.Version)
		return details, nil
	})
}

// each apre una sessione per target ed esegue fn, fermandosi al primo errore.
func (p *Plugin) each(ctx context.Context, req *plugin.Request, o options, status string, fn func(*session) (map[string]string, error)) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		s, err := p.open(ctx, req, o, t)
		var details map[string]string
		if err == nil {
			details, err = fn(s)
		}
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: status, Details: details})
	}
	return res, nil
}

// session è la connessione a un'istanza standalone o a un domain controller.
type session struct {
	c      Client
	domain bool
	groups []string
	host   string
	log    *slog.Logger
	poll   time.Duration
}

func (s *session) mode() string {
	if s.domain {
		return "domain"
	}
	return "standalone"
}

// open si connette al target, riconosce la modalità e verifica i server group.
func (p *Plugin) open(ctx context.Context, req *plugin.Request, o options, t plugin.Target) (*session, error) {
	if req.Secrets == nil {
		return nil, errors.New("secret resolver non disponibile")
	}
	pass, err := req.Secrets.Secret(ctx, o.passwordSecret)
	if err != nil {
		return nil, fmt.Errorf("segreto %s: %w", o.passwordSecret, err)
	}
	scheme, port := "https", 9993
	if !o.tls {
		scheme, port = "http", 9990
	}
	if t.Port != 0 {
		port = t.Port
	}
	base := scheme + "://" + net.JoinHostPort(t.Host, strconv.Itoa(port))
	c, err := p.connector.Connect(ctx, base, o.username, pass)
	if err != nil {
		return nil, fmt.Errorf("connessione a %s: %w", base, err)
	}
	s := &session{c: c, groups: o.groups, host: t.Host, poll: p.poll,
		log: p.logger.With("deployment", req.DeploymentID, "host", t.Host)}
	launch, err := s.attribute(ctx, nil, "launch-type")
	if err != nil {
		return nil, fmt.Errorf("interfaccia di management %s: %w", base, err)
	}
	switch launch {
	case "DOMAIN":
		s.domain = true
		if len(o.groups) == 0 {
			return nil, fmt.Errorf("%s è un domain controller: indicare %s", t.Host, ParamServerGroups)
		}
		for _, g := range o.groups {
			if _, err := s.c.Execute(ctx, op("read-resource", Address{{"server-group", g}})); err != nil {
				return nil, fmt.Errorf("server group %s: %w", g, err)
			}
		}
	case "STANDALONE":
		if len(o.groups) > 0 {
			return nil, fmt.Errorf("%s è un server standalone: %s vale solo in domain mode", t.Host, ParamServerGroups)
		}
	default:
		return nil, fmt.Errorf("launch-type %q non supportato", launch)
	}
	return s, nil
}

// attribute legge un attributo stringa di una risorsa.
func (s *session) attribute(ctx context.Context, addr Address, name string) (string, error) {
	raw, err := s.c.Execute(ctx, op("read-attribute", addr, "name", name))
	if err != nil {
		return "", err
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("attributo %s non leggibile: %s", name, raw)
	}
	return v, nil
}

// deployment è un deployment del server (o del dominio).
type deployment struct {
	Name        string
	RuntimeName string
	Hash        string
	// Enabled: attivo (in domain mode, in tutti i server group indicati).
	Enabled bool
	// Groups sono i server group a cui è assegnato (domain mode), con lo stato di attivazione.
	Groups map[string]bool
}

type dmrDeployment struct {
	RuntimeName string `json:"runtime-name"`
	Enabled     *bool  `json:"enabled"`
	Content     []struct {
		Hash struct {
			Bytes string `json:"BYTES_VALUE"`
		} `json:"hash"`
	} `json:"content"`
}

// deployments legge i deployment; in domain mode anche le assegnazioni ai server group.
func (s *session) deployments(ctx context.Context) (map[string]*deployment, error) {
	read := func(addr Address) (map[string]dmrDeployment, error) {
		raw, err := s.c.Execute(ctx, op("read-children-resources", addr, "child-type", "deployment"))
		if err != nil {
			return nil, err
		}
		out := map[string]dmrDeployment{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("elenco dei deployment non leggibile: %w", err)
		}
		return out, nil
	}
	root, err := read(nil)
	if err != nil {
		return nil, err
	}
	out := map[string]*deployment{}
	for name, d := range root {
		dep := &deployment{Name: name, RuntimeName: cmp.Or(d.RuntimeName, name), Enabled: d.Enabled != nil && *d.Enabled}
		if len(d.Content) > 0 {
			dep.Hash = d.Content[0].Hash.Bytes
		}
		out[name] = dep
	}
	if !s.domain {
		return out, nil
	}
	for _, d := range out {
		d.Groups, d.Enabled = map[string]bool{}, true
	}
	for _, g := range s.groups {
		assigned, err := read(Address{{"server-group", g}})
		if err != nil {
			return nil, fmt.Errorf("server group %s: %w", g, err)
		}
		for name, a := range assigned {
			if d := out[name]; d != nil {
				d.Groups[g] = a.Enabled != nil && *a.Enabled
			}
		}
	}
	for _, d := range out {
		for _, g := range s.groups {
			d.Enabled = d.Enabled && d.Groups[g]
		}
	}
	return out, nil
}

// active restituisce il deployment attivo con il runtime-name indicato. In domain mode, se le
// assegnazioni non sono allineate, quello attivo in almeno un server group.
func active(deps map[string]*deployment, runtimeName string) *deployment {
	var partial *deployment
	for _, name := range slices.Sorted(maps.Keys(deps)) {
		d := deps[name]
		if d.RuntimeName != runtimeName {
			continue
		}
		if d.Enabled {
			return d
		}
		for _, on := range d.Groups {
			if on && partial == nil {
				partial = d
			}
		}
	}
	return partial
}

// release porta il target sul deployment name. Con sum (deploy) carica il contenuto se manca;
// senza (rollback) il deployment deve esistere già.
func (p *Plugin) release(ctx context.Context, s *session, req *plugin.Request, o options, name, sum string) (map[string]string, error) {
	details := map[string]string{"mode": s.mode(), DetailDeployment: name, "runtime_name": o.runtimeName}
	deps, err := s.deployments(ctx)
	if err != nil {
		return details, err
	}
	cur := active(deps, o.runtimeName)
	if cur != nil {
		details["previous"] = cur.Name
		if v, ok := o.versionOf(cur.Name); ok {
			details["previous_version"] = v
		}
		if cur.Name == name && cur.Enabled && (sum == "" || cur.Hash == sum) {
			s.log.InfoContext(ctx, "deployment già attivo", "name", name)
			details["unchanged"] = "true"
			return details, nil
		}
	}
	target := deps[name]
	switch {
	case target == nil && sum == "":
		return details, fmt.Errorf("il deployment %s non è più presente: impossibile riattivarlo", name)
	case target != nil && sum != "" && target.Hash != sum:
		// Stesso nome ma contenuto diverso (es. artefatto ricostruito con la stessa versione):
		// mai sostituire il contenuto del deployment attivo senza passare dal cambio di versione.
		if target == cur {
			return details, fmt.Errorf("il deployment attivo %s ha un contenuto diverso dall'artefatto: usare una nuova versione", name)
		}
		if err := s.remove(ctx, target); err != nil {
			return details, err
		}
		target = nil
	}
	if target == nil {
		if err := s.add(ctx, req.Artifact.LocalPath, name, o.runtimeName); err != nil {
			return details, err
		}
		s.log.InfoContext(ctx, "contenuto caricato", "name", name, "runtime_name", o.runtimeName)
		if deps, err = s.deployments(ctx); err != nil {
			return details, err
		}
		target = deps[name]
		if target == nil {
			return details, fmt.Errorf("deployment %s non presente dopo il caricamento", name)
		}
	}

	suspended := false
	if o.suspend {
		if err := s.suspend(ctx, o.suspendTimeout); err != nil {
			return details, err
		}
		suspended = true
		details["suspended"] = "true"
	}
	if err := s.swap(ctx, deps, o.runtimeName, target); err != nil {
		if suspended {
			// Il cambio è annullato dal server: si rimette in servizio la versione di prima.
			if rerr := s.resume(ctx); rerr != nil {
				s.log.ErrorContext(ctx, "resume dopo un cambio di versione fallito", "error", rerr)
				err = fmt.Errorf("%w; resume fallito: %v", err, rerr)
			}
		}
		return details, err
	}
	s.log.InfoContext(ctx, "versione attivata", "name", name, "previous", details["previous"])
	switch {
	case o.restart:
		if err := s.restartServer(ctx, o.wait); err != nil {
			return details, err
		}
		details["restarted"] = "true"
	case o.reload:
		if err := s.reloadServer(ctx, o.wait); err != nil {
			return details, err
		}
		details["reloaded"] = "true"
	case suspended:
		if err := s.resume(ctx); err != nil {
			return details, err
		}
	}
	if sum != "" {
		if err := s.cleanup(ctx, o, name, details["previous"]); err != nil {
			// La pulizia non fa fallire un rilascio riuscito.
			s.log.WarnContext(ctx, "pulizia dei deployment precedenti fallita", "error", err)
		}
	}
	return details, nil
}

// add carica il contenuto e crea il deployment, non attivo.
func (s *session) add(ctx context.Context, path, name, runtimeName string) error {
	hash, err := s.c.Upload(ctx, path)
	if err != nil {
		return fmt.Errorf("caricamento di %s: %w", filepath.Base(path), err)
	}
	content := []map[string]any{{"hash": map[string]string{"BYTES_VALUE": hash}}}
	params := []any{"content", content, "runtime-name", runtimeName}
	if !s.domain {
		params = append(params, "enabled", false)
	}
	if _, err := s.c.Execute(ctx, op("add", Address{{"deployment", name}}, params...)); err != nil {
		return err
	}
	for _, g := range s.groups {
		if _, err := s.c.Execute(ctx, op("add", Address{{"server-group", g}, {"deployment", name}}, "enabled", false)); err != nil {
			return fmt.Errorf("server group %s: %w", g, err)
		}
	}
	return nil
}

// swap disattiva i deployment con lo stesso runtime-name e attiva target, in un'unica
// operazione composite: se un passo fallisce il server annulla anche gli altri.
func (s *session) swap(ctx context.Context, deps map[string]*deployment, runtimeName string, target *deployment) error {
	var steps []Operation
	addrs := func(name string) []Address {
		if !s.domain {
			return []Address{{{"deployment", name}}}
		}
		out := make([]Address, len(s.groups))
		for i, g := range s.groups {
			out[i] = Address{{"server-group", g}, {"deployment", name}}
		}
		return out
	}
	for _, name := range slices.Sorted(maps.Keys(deps)) {
		d := deps[name]
		if d == target || d.RuntimeName != runtimeName {
			continue
		}
		for i, a := range addrs(name) {
			if !s.domain && d.Enabled || s.domain && d.Groups[s.groups[i]] {
				steps = append(steps, op("undeploy", a))
			}
		}
	}
	for i, a := range addrs(target.Name) {
		if s.domain {
			if _, ok := target.Groups[s.groups[i]]; !ok {
				steps = append(steps, op("add", a, "enabled", true))
				continue
			}
			if target.Groups[s.groups[i]] {
				continue
			}
		}
		steps = append(steps, op("deploy", a))
	}
	_, err := s.c.Execute(ctx, op("composite", nil, "steps", steps))
	if err != nil {
		return fmt.Errorf("cambio di versione: %w", err)
	}
	return nil
}

// suspend sospende il server (o i server group): le nuove richieste ricevono 503 e quelle in
// corso hanno suspend_timeout per terminare.
func (s *session) suspend(ctx context.Context, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if s.domain {
		for _, g := range s.groups {
			if _, err := s.c.Execute(ctx, op("suspend-servers", Address{{"server-group", g}}, "suspend-timeout", secs)); err != nil {
				return fmt.Errorf("suspend del server group %s: %w", g, err)
			}
		}
		s.log.InfoContext(ctx, "server group sospesi", "groups", s.groups)
		return nil
	}
	if _, err := s.c.Execute(ctx, op("suspend", nil, "suspend-timeout", secs)); err != nil {
		return fmt.Errorf("suspend: %w", err)
	}
	// Su un'istanza standalone l'operazione può restituire prima della fine delle richieste:
	// si attende lo stato SUSPENDED, al più suspend_timeout.
	deadline := time.Now().Add(timeout)
	for {
		state, err := s.attribute(ctx, nil, "suspend-state")
		if err == nil && state == "SUSPENDED" {
			s.log.InfoContext(ctx, "server sospeso")
			return nil
		}
		if time.Now().After(deadline) {
			s.log.WarnContext(ctx, "server non ancora sospeso allo scadere di suspend_timeout: si prosegue", "state", state)
			return nil
		}
		if err := sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

// resume rimette in servizio il server (o i server group).
func (s *session) resume(ctx context.Context) error {
	if s.domain {
		for _, g := range s.groups {
			if _, err := s.c.Execute(ctx, op("resume-servers", Address{{"server-group", g}})); err != nil {
				return fmt.Errorf("resume del server group %s: %w", g, err)
			}
		}
		return nil
	}
	if _, err := s.c.Execute(ctx, op("resume", nil)); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// reloadServer ricarica la configurazione e i servizi senza riavviare la JVM.
func (s *session) reloadServer(ctx context.Context, wait time.Duration) error {
	if s.domain {
		return s.groupOp(ctx, "reload-servers")
	}
	if _, err := s.c.Execute(ctx, op("reload", nil)); err != nil && !disconnected(err) {
		return fmt.Errorf("reload: %w", err)
	}
	return s.waitRunning(ctx, wait)
}

// restartServer riavvia la JVM: in standalone con shutdown(restart=true), che richiede un
// avvio tramite standalone.sh o un servizio che lo gestisce.
func (s *session) restartServer(ctx context.Context, wait time.Duration) error {
	if s.domain {
		return s.groupOp(ctx, "restart-servers")
	}
	if _, err := s.c.Execute(ctx, op("shutdown", nil, "restart", true)); err != nil && !disconnected(err) {
		return fmt.Errorf("restart: %w", err)
	}
	return s.waitRunning(ctx, wait)
}

// groupOp esegue un'operazione bloccante su ogni server group.
func (s *session) groupOp(ctx context.Context, name string) error {
	for _, g := range s.groups {
		if _, err := s.c.Execute(ctx, op(name, Address{{"server-group", g}}, "blocking", true)); err != nil {
			return fmt.Errorf("%s del server group %s: %w", name, g, err)
		}
	}
	s.log.InfoContext(ctx, name+" completato", "groups", s.groups)
	return nil
}

// waitRunning attende che il server torni nello stato running; durante il riavvio
// l'interfaccia di management non risponde.
func (s *session) waitRunning(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	// Il server impiega un attimo a lasciare lo stato running.
	if err := sleep(ctx, s.poll); err != nil {
		return err
	}
	for {
		state, err := s.attribute(ctx, nil, "server-state")
		if err == nil && state == "running" {
			s.log.InfoContext(ctx, "server di nuovo in esecuzione")
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("server non tornato in esecuzione entro %s: %w", wait, err)
			}
			return fmt.Errorf("server non tornato in esecuzione entro %s: stato %q", wait, state)
		}
		if err := sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

// remove elimina un deployment non attivo (in domain mode prima dai server group).
func (s *session) remove(ctx context.Context, d *deployment) error {
	for g := range d.Groups {
		if _, err := s.c.Execute(ctx, op("remove", Address{{"server-group", g}, {"deployment", d.Name}})); err != nil {
			return fmt.Errorf("rimozione di %s dal server group %s: %w", d.Name, g, err)
		}
	}
	if _, err := s.c.Execute(ctx, op("remove", Address{{"deployment", d.Name}})); err != nil {
		return fmt.Errorf("rimozione di %s: %w", d.Name, err)
	}
	return nil
}

// cleanup elimina le versioni gestite da manent oltre keep_releases, dalla più vecchia; non
// tocca mai la versione attivata, la precedente né un deployment attivo.
func (s *session) cleanup(ctx context.Context, o options, current, previous string) error {
	deps, err := s.deployments(ctx)
	if err != nil {
		return err
	}
	var old []*deployment
	for _, d := range deps {
		if _, ok := o.versionOf(d.Name); !ok || d.RuntimeName != o.runtimeName || d.Name == current || d.Name == previous || d.Enabled {
			continue
		}
		if slices.Contains(slices.Collect(maps.Values(d.Groups)), true) {
			continue
		}
		old = append(old, d)
	}
	keep := o.keep - 1
	if previous != "" {
		keep--
	}
	if len(old) <= keep {
		return nil
	}
	slices.SortFunc(old, func(a, b *deployment) int {
		va, _ := o.versionOf(a.Name)
		vb, _ := o.versionOf(b.Name)
		return compareVersions(va, vb)
	})
	for _, d := range old[:len(old)-keep] {
		if err := s.remove(ctx, d); err != nil {
			return err
		}
		s.log.InfoContext(ctx, "deployment precedente rimosso", "name", d.Name)
	}
	return nil
}

// contentHash è l'hash del contenuto come lo calcola il server (SHA-1, in base64 nel DMR).
func contentHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("artefatto: %w", err)
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("artefatto: %w", err)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// disconnected indica un errore di rete: reload e shutdown possono chiudere la connessione
// prima della risposta.
func disconnected(err error) bool {
	var oe *OperationError
	return !errors.As(err, &oe)
}

// compareVersions confronta due versioni a segmenti numerici e testuali (1.10 > 1.9).
func compareVersions(a, b string) int {
	sa, sb := splitVersion(a), splitVersion(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		na, ea := strconv.Atoi(sa[i])
		nb, eb := strconv.Atoi(sb[i])
		var c int
		if ea == nil && eb == nil {
			c = cmp.Compare(na, nb)
		} else {
			c = strings.Compare(sa[i], sb[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(sa), len(sb))
}

func splitVersion(v string) []string {
	var out []string
	cur, digit := "", false
	for _, r := range v {
		if !unicode.IsDigit(r) && !unicode.IsLetter(r) {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if cur != "" && unicode.IsDigit(r) != digit {
			out = append(out, cur)
			cur = ""
		}
		cur += string(r)
		digit = unicode.IsDigit(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
