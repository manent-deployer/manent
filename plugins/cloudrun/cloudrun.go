// Package cloudrun rilascia un'immagine di container su un servizio Google Cloud Run: cambia
// l'immagine nel template del servizio, che crea una nuova revisione, e manda tutto il
// traffico all'ultima revisione pronta. La release in esercizio è la revisione che riceve il
// traffico; il rollback sposta il traffico sulla revisione precedente, senza ricrearla.
//
// L'artefatto è un'immagine di un registry (sorgente oci://registry/repository:tag), sempre
// avviata fissata al digest. Il servizio (rete, service account, scalabilità, variabili) resta
// di chi gestisce l'infrastruttura (Terraform): il plugin cambia solo l'immagine e scrive
// versione e deploy nelle annotazioni della revisione. Ogni target è una region.
package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name è il nome con cui il plugin si registra.
const Name = "cloudrun"

// Parametri del descrittore (deploy.params) letti dal plugin.
const (
	ParamProject           = "project"            // progetto Google Cloud (obbligatorio)
	ParamService           = "service"            // servizio Cloud Run (default: nome dell'applicazione)
	ParamContainer         = "container"          // container a cui cambiare l'immagine (default: l'unico, o quello col nome dell'applicazione)
	ParamCredentialsSecret = "credentials_secret" // segreto con la chiave JSON di un service account; default: Application Default Credentials
	ParamWaitTimeout       = "wait_timeout"       // attesa della revisione e del traffico (default 10m)
	defaultWaitTimeout     = 10 * time.Minute
)

// Annotazioni scritte sul template, quindi su ogni revisione.
const (
	AnnotationVersion    = "manent.io/version"
	AnnotationDeployment = "manent.io/deployment"
)

// Tipi di destinazione del traffico.
const (
	trafficLatest   = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
	trafficRevision = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
)

// DetailRevision è il prefisso delle chiavi di Release.Details con la revisione della release
// su ciascun target ("revision@<target>").
const DetailRevision = "revision"

// ErrNotFound indica un servizio o una revisione inesistente.
var ErrNotFound = errors.New("non trovato")

var (
	regionRe  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	projectRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	serviceRe = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,47}[a-z0-9])?$`)
)

// Object è una risorsa dell'API (servizio o revisione) come JSON generico: il servizio viene
// riscritto per intero, quindi i campi che il plugin non conosce restano invariati.
type Object = map[string]any

// Operation è un'operazione a lunga durata dell'API.
type Operation struct {
	Name  string     `json:"name"`
	Done  bool       `json:"done"`
	Error *OpFailure `json:"error,omitempty"`
}

// OpFailure è l'errore di un'operazione conclusa.
type OpFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// API sono le operazioni su Cloud Run (Admin API v2) usate dal plugin. I nomi sono quelli
// completi delle risorse (projects/…/locations/…/services/…).
type API interface {
	GetService(ctx context.Context, name string) (Object, error)
	UpdateService(ctx context.Context, name string, svc Object, validateOnly bool) (*Operation, error)
	GetOperation(ctx context.Context, name string) (*Operation, error)
	GetRevision(ctx context.Context, name string) (Object, error)
	ListRevisions(ctx context.Context, service string) ([]Object, error)
}

// Connector apre l'accesso all'API con le credenziali della richiesta.
type Connector interface {
	Connect(ctx context.Context, req *plugin.Request) (API, error)
}

// Plugin implementa plugin.Deployer, DryRunner, ReleaseInspector e Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	poll      time.Duration
}

// New crea il plugin con il connettore indicato e i tipi di artefatto supportati.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - endpoint: endpoint dell'API (default https://run.googleapis.com)
//   - artifact_types: tipi supportati (default ["cloudrun"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	endpoint := DefaultEndpoint
	if raw, ok := cfg["endpoint"]; ok {
		s, ok := raw.(string)
		if !ok || !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
			return nil, errors.New("endpoint deve essere un URL http(s)")
		}
		endpoint = s
	}
	return New(&HTTPConnector{Endpoint: endpoint}, types, logger), nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

func serviceName(req *plugin.Request) string {
	if s := req.Params[ParamService]; s != "" {
		return s
	}
	return req.Application
}

// resource è il nome completo del servizio nella region.
func resource(req *plugin.Request, region string) string {
	return "projects/" + req.Params[ParamProject] + "/locations/" + region + "/services/" + serviceName(req)
}

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e diventa un'annotazione della revisione: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("l'artefatto non è un'immagine di un registry: indicare artifact.source oci://registry/repository:tag"))
	}
	if pr := req.Params[ParamProject]; !projectRe.MatchString(pr) {
		errs = append(errs, fmt.Errorf("%s %q non valido: indicare l'ID del progetto Google Cloud", ParamProject, pr))
	}
	if s := serviceName(req); !serviceRe.MatchString(s) {
		errs = append(errs, fmt.Errorf("servizio %q non valido: minuscole, cifre e '-', al più 49 caratteri (parametro %q)", s, ParamService))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("nessuna region tra i target"))
	}
	for _, t := range req.Targets {
		if !regionRe.MatchString(t.Host) {
			errs = append(errs, fmt.Errorf("target %q: atteso il nome di una region (es. europe-west1)", t.Host))
		}
	}
	return errors.Join(errs...)
}

// get legge un valore annidato di o.
func get(o Object, keys ...string) any {
	var cur any = o
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func str(o Object, keys ...string) string {
	s, _ := get(o, keys...).(string)
	return s
}

// child restituisce la mappa o[key], creandola se manca.
func child(o Object, key string) Object {
	m, ok := o[key].(map[string]any)
	if !ok {
		m = Object{}
		o[key] = m
	}
	return m
}

// short restituisce l'ultimo elemento del nome di una risorsa.
func short(name string) string { return name[strings.LastIndexByte(name, '/')+1:] }

// containers restituisce i container del template del servizio.
func containers(svc Object) []Object {
	raw, _ := get(svc, "template", "containers").([]any)
	var out []Object
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// chooseContainer sceglie il container a cui cambiare l'immagine.
func chooseContainer(svc Object, req *plugin.Request) (Object, error) {
	list := containers(svc)
	names := make([]string, len(list))
	for i, c := range list {
		names[i] = str(c, "name")
	}
	want := req.Params[ParamContainer]
	switch {
	case want != "":
	case len(list) == 1:
		return list[0], nil
	default:
		want = req.Application
	}
	if i := slices.Index(names, want); i >= 0 {
		return list[i], nil
	}
	return nil, fmt.Errorf("container %q assente dal servizio (container: %s): indicare %q", want, strings.Join(names, ", "), ParamContainer)
}

// serving restituisce la revisione che riceve tutto il traffico ("" se nessuna).
func serving(svc Object) (string, error) {
	statuses, _ := svc["trafficStatuses"].([]any)
	var active []string
	rev := ""
	for _, s := range statuses {
		m, _ := s.(map[string]any)
		pct, _ := m["percent"].(float64)
		if pct <= 0 {
			continue
		}
		r, _ := m["revision"].(string)
		if m["type"] == trafficLatest || r == "" {
			r = str(svc, "latestReadyRevision")
		}
		r = short(r)
		active = append(active, fmt.Sprintf("%s %.0f%%", r, pct))
		if pct >= 100 {
			rev = r
		}
	}
	if len(active) > 1 {
		return "", fmt.Errorf("traffico diviso tra più revisioni (%s)", strings.Join(active, ", "))
	}
	return rev, nil
}

// latestOnly indica se il servizio manda già tutto il traffico all'ultima revisione.
func latestOnly(svc Object) bool {
	tr, _ := svc["traffic"].([]any)
	if len(tr) != 1 {
		return false
	}
	m, _ := tr[0].(map[string]any)
	pct, _ := m["percent"].(float64)
	return m["type"] == trafficLatest && pct == 100
}

// state è lo stato di un target letto prima di deploy, dry-run e rollback.
type state struct {
	region string
	name   string
	api    API
	svc    Object
}

func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	if !regionRe.MatchString(t.Host) {
		return nil, fmt.Errorf("region %q non valida", t.Host)
	}
	api, err := p.connector.Connect(ctx, req)
	if err != nil {
		return nil, err
	}
	name := resource(req, t.Host)
	svc, err := api.GetService(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("servizio %s assente: va creato con l'infrastruttura (es. Terraform), manent ne aggiorna l'immagine", name)
	}
	if err != nil {
		return nil, fmt.Errorf("lettura del servizio: %w", err)
	}
	return &state{region: t.Host, name: name, api: api, svc: svc}, nil
}

// version legge la versione di manent dalla revisione che riceve il traffico ("" se non c'è).
func (s *state) version(ctx context.Context) (string, string, error) {
	rev, err := serving(s.svc)
	if err != nil || rev == "" {
		return "", "", err
	}
	r, err := s.api.GetRevision(ctx, s.name+"/revisions/"+rev)
	if err != nil {
		return "", rev, fmt.Errorf("lettura della revisione %s: %w", rev, err)
	}
	return str(r, "annotations", AnnotationVersion), rev, nil
}

func (s *state) details(ctx context.Context, req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.region, "service": serviceName(req)}
	if v, _, err := s.version(ctx); err == nil && v != "" {
		d["previous"] = v
	}
	return d
}

// withImage restituisce il servizio con l'immagine e le annotazioni della release e tutto il
// traffico sull'ultima revisione.
func withImage(svc Object, req *plugin.Request, image string) (Object, error) {
	c, err := chooseContainer(svc, req)
	if err != nil {
		return nil, err
	}
	c["image"] = image
	tmpl := child(svc, "template")
	ann := child(tmpl, "annotations")
	ann[AnnotationVersion], ann[AnnotationDeployment] = req.Artifact.Version, req.DeploymentID
	// Un nome di revisione fisso impedirebbe di crearne una nuova: lo genera Cloud Run.
	delete(tmpl, "revision")
	svc["traffic"] = []any{Object{"type": trafficLatest, "percent": 100}}
	return svc, nil
}

// Deploy rilascia l'immagine su ogni region in sequenza; si ferma al primo errore. È
// idempotente: un template già aggiornato da questo deploy non viene riscritto.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, req, image)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(ctx, req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.region, "service", serviceName(req))
	c, err := chooseContainer(s.svc, req)
	if err != nil {
		return details, err
	}
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	applied := str(c, "image") == image && str(s.svc, "template", "annotations", AnnotationDeployment) == req.DeploymentID && latestOnly(s.svc)
	if !applied {
		svc, err := withImage(s.svc, req, image)
		if err != nil {
			return details, err
		}
		op, err := s.api.UpdateService(ctx, s.name, svc, false)
		if err != nil {
			return details, fmt.Errorf("aggiornamento del servizio: %w", err)
		}
		log.InfoContext(ctx, "servizio aggiornato, in attesa della revisione", "image", image)
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	svc, err := s.api.GetService(ctx, s.name)
	if err != nil {
		return details, fmt.Errorf("lettura del servizio: %w", err)
	}
	if st := str(svc, "terminalCondition", "state"); st != "" && st != "CONDITION_SUCCEEDED" {
		return details, fmt.Errorf("servizio nello stato %s: %s", st, str(svc, "terminalCondition", "message"))
	}
	created, ready := short(str(svc, "latestCreatedRevision")), short(str(svc, "latestReadyRevision"))
	if created != ready {
		return details, fmt.Errorf("revisione %s non pronta (in esercizio resta %s)", created, ready)
	}
	details[DetailRevision] = ready
	if u := str(svc, "uri"); u != "" {
		details["url"] = u
	}
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version, "revision", ready)
	return details, nil
}

// wait attende la conclusione dell'operazione; un'operazione fallita (es. revisione che non
// si avvia) è un errore.
func (p *Plugin) wait(ctx context.Context, api API, op *Operation, timeout time.Duration) error {
	for !op.Done {
		select {
		case <-ctx.Done():
			return fmt.Errorf("operazione %s non conclusa entro %s", short(op.Name), timeout)
		case <-time.After(p.poll):
		}
		next, err := api.GetOperation(ctx, op.Name)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("operazione %s non conclusa entro %s", short(op.Name), timeout)
			}
			return fmt.Errorf("lettura dell'operazione: %w", err)
		}
		op = next
	}
	if op.Error != nil {
		return fmt.Errorf("operazione fallita: %s", op.Error.Message)
	}
	return nil
}

// CurrentRelease legge su ogni region la versione della revisione che riceve il traffico. Una
// revisione senza l'annotazione di manent non ha release; versioni diverse sono un errore.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "revisione che riceve il traffico"}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		v, rev, err := s.version(ctx)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailRevision+"@"+t.Host] = rev
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v := range found {
			return &plugin.Release{Version: v, Details: details}, nil
		}
	}
	var desc []string
	for v, hosts := range found {
		desc = append(desc, v+" su "+strings.Join(hosts, ","))
	}
	sort.Strings(desc)
	return nil, fmt.Errorf("versioni diverse tra le region: %s", strings.Join(desc, "; "))
}

// Rollback manda tutto il traffico di ogni region alla revisione della release indicata:
// quella nei dettagli della release o, in mancanza, la più recente con quella versione.
// Prosegue anche se una region fallisce. È idempotente.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !pluginutil.VersionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("versione %q non valida", previous.Version)
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.rollbackTarget(ctx, t, req, previous)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, req *plugin.Request, previous plugin.Release) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := map[string]string{"region": s.region, "service": serviceName(req)}
	rev := previous.Details[DetailRevision+"@"+t.Host]
	if rev == "" {
		if rev, err = p.findRevision(ctx, s, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailRevision] = rev
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if cur, err := serving(s.svc); err != nil || cur != rev || latestOnly(s.svc) {
		s.svc["traffic"] = []any{Object{"type": trafficRevision, "revision": rev, "percent": 100}}
		op, err := s.api.UpdateService(ctx, s.name, s.svc, false)
		if err != nil {
			return details, fmt.Errorf("spostamento del traffico: %w", err)
		}
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	p.logger.WarnContext(ctx, "release precedente ripristinata", "deployment", req.DeploymentID, "region", s.region,
		"service", serviceName(req), "version", previous.Version, "revision", rev, "rollback", true)
	return details, nil
}

// findRevision cerca la revisione più recente con la versione indicata.
func (p *Plugin) findRevision(ctx context.Context, s *state, version string) (string, error) {
	list, err := s.api.ListRevisions(ctx, s.name)
	if err != nil {
		return "", fmt.Errorf("elenco delle revisioni: %w", err)
	}
	best, bestTime := "", ""
	for _, r := range list {
		if str(r, "annotations", AnnotationVersion) != version {
			continue
		}
		// createTime è RFC 3339 in UTC: l'ordine lessicografico è quello temporale.
		if ct := str(r, "createTime"); best == "" || ct > bestTime {
			best, bestTime = short(str(r, "name")), ct
		}
	}
	if best == "" {
		return "", fmt.Errorf("nessuna revisione del servizio con la versione %s", version)
	}
	return best, nil
}

// DryRun verifica su ogni region, senza modificarla, che servizio e container esistano e che
// l'aggiornamento sia accettato (validateOnly: validazione e permessi dell'API).
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, _ := plugin.ImageReference(req.Artifact)
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, req, image)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(ctx, req)
	c, err := chooseContainer(s.svc, req)
	if err != nil {
		return details, err
	}
	details["container"] = str(c, "name")
	if image == "" {
		return details, nil
	}
	details["image"] = image
	svc, err := withImage(s.svc, req, image)
	if err != nil {
		return details, err
	}
	if _, err := s.api.UpdateService(ctx, s.name, svc, true); err != nil {
		return details, fmt.Errorf("verifica dell'aggiornamento: %w", err)
	}
	p.logger.InfoContext(ctx, "servizio pronto per il deploy", "deployment", req.DeploymentID, "region", s.region, "service", serviceName(req), "dry_run", true)
	return details, nil
}
