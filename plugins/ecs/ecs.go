// Package ecs rilascia un'immagine di container su un servizio Amazon ECS (Fargate o EC2):
// registra una nuova revisione della task definition in esercizio con la sola immagine
// cambiata, aggiorna il servizio e attende che il deployment di ECS sia completato.
//
// L'artefatto è un'immagine di un registry (sorgente oci://registry/repository:tag), sempre
// avviata fissata al digest. Servizio, cluster, rete, ruoli e il resto della task definition
// restano di chi gestisce l'infrastruttura (Terraform, CloudFormation): il plugin cambia solo
// l'immagine e scrive versione e deploy nei tag della revisione, da cui legge la release in
// esercizio e torna alla precedente. Ogni target è una region, con il cluster.
package ecs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name è il nome con cui il plugin si registra.
const Name = "ecs"

// Parametri del descrittore (deploy.params) letti dal plugin, oltre a quelli delle
// credenziali di awsauth.
const (
	ParamCluster        = "cluster"         // cluster ECS (default "default"); il target può indicarlo come region/cluster
	ParamService        = "service"         // servizio ECS (default: nome dell'applicazione)
	ParamContainer      = "container"       // container a cui cambiare l'immagine (default: l'unico, o quello col nome dell'applicazione)
	ParamWaitTimeout    = "wait_timeout"    // attesa del deployment (default 15m)
	ParamSearchRevision = "search_revision" // revisioni esaminate per trovare quella di una versione (default 100)
	defaultCluster      = "default"
	defaultWaitTimeout  = 15 * time.Minute
	defaultSearch       = 100
)

// Tag scritti sulla revisione della task definition.
const (
	TagApplication = "manent.application"
	TagEnvironment = "manent.environment"
	TagVersion     = "manent.version"
	TagDeployment  = "manent.deployment"
)

// DetailTaskDefinition è il prefisso delle chiavi di Release.Details con la task definition
// della release su ciascun target ("task_definition@<target>").
const DetailTaskDefinition = "task_definition"

// ErrNotFound indica un servizio o una task definition inesistente.
var ErrNotFound = errors.New("non trovato")

// Service è lo stato di un servizio ECS.
type Service struct {
	Name           string
	Status         string
	TaskDefinition string
	DesiredCount   int32
	RunningCount   int32
	// Controller è il tipo di deployment controller: ECS, CODE_DEPLOY o EXTERNAL.
	Controller  string
	Deployments []Deployment
}

// Deployment è un deployment di ECS in corso o concluso sul servizio.
type Deployment struct {
	ID             string
	Status         string // PRIMARY, ACTIVE, INACTIVE
	TaskDefinition string
	RolloutState   string // IN_PROGRESS, COMPLETED, FAILED
	RolloutReason  string
	Desired        int32
	Running        int32
	Pending        int32
	Failed         int32
}

// TaskDefinition è una revisione di task definition. Base conserva la definizione completa
// letta dall'API, da cui il client registra la revisione successiva.
type TaskDefinition struct {
	ARN        string
	Family     string
	Revision   int32
	Containers []Container
	Tags       map[string]string
	Base       any
}

// Container è un container della task definition.
type Container struct {
	Name  string
	Image string
}

// API sono le operazioni su ECS usate dal plugin, in una region.
type API interface {
	DescribeService(ctx context.Context, cluster, service string) (*Service, error)
	DescribeTaskDefinition(ctx context.Context, arn string) (*TaskDefinition, error)
	// RegisterRevision registra una nuova revisione di base con l'immagine del container
	// cambiata e i tag aggiunti a quelli esistenti.
	RegisterRevision(ctx context.Context, base *TaskDefinition, container, image string, tags map[string]string) (*TaskDefinition, error)
	UpdateService(ctx context.Context, cluster, service, taskDefinition string) error
	// ListRevisions restituisce gli ARN delle revisioni attive della famiglia, dalla più recente.
	ListRevisions(ctx context.Context, family string, limit int) ([]string, error)
}

// Connector apre l'accesso a ECS in una region con le credenziali della richiesta.
type Connector interface {
	Connect(ctx context.Context, region string, req *plugin.Request) (API, error)
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
	return &Plugin{connector: c, types: types, logger: logger, poll: 5 * time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - endpoint: endpoint alternativo dell'API ECS (es. LocalStack)
//   - artifact_types: tipi supportati (default ["ecs"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	endpoint, err := awsauth.Endpoint(cfg)
	if err != nil {
		return nil, err
	}
	return New(SDKConnector{Endpoint: endpoint}, types, logger), nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// target è un target interpretato: region e cluster.
type target struct {
	host    string
	region  string
	cluster string
}

// parseTarget interpreta l'host del target: "region" oppure "region/cluster".
func parseTarget(t plugin.Target, req *plugin.Request) (target, error) {
	region, cluster, ok := strings.Cut(t.Host, "/")
	if !ok || cluster == "" {
		cluster = req.Params[ParamCluster]
	}
	if cluster == "" {
		cluster = defaultCluster
	}
	if !awsauth.ValidRegion(region) {
		return target{}, fmt.Errorf("target %q: atteso region o region/cluster (es. eu-west-1/prod)", t.Host)
	}
	return target{host: t.Host, region: region, cluster: cluster}, nil
}

func serviceName(req *plugin.Request) string {
	if s := req.Params[ParamService]; s != "" {
		return s
	}
	return req.Application
}

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e diventa un tag della task definition: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("l'artefatto non è un'immagine di un registry: indicare artifact.source oci://registry/repository:tag"))
	}
	if serviceName(req) == "" {
		errs = append(errs, fmt.Errorf("%s obbligatorio", ParamService))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if _, err := searchLimit(req); err != nil {
		errs = append(errs, err)
	}
	if err := awsauth.Validate(req.Params); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("nessuna region tra i target"))
	}
	for _, t := range req.Targets {
		if _, err := parseTarget(t, req); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func searchLimit(req *plugin.Request) (int, error) {
	raw := req.Params[ParamSearchRevision]
	if raw == "" {
		return defaultSearch, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s deve essere un intero positivo", ParamSearchRevision)
	}
	return n, nil
}

// chooseContainer sceglie il container a cui cambiare l'immagine.
func chooseContainer(td *TaskDefinition, req *plugin.Request) (string, error) {
	names := make([]string, len(td.Containers))
	for i, c := range td.Containers {
		names[i] = c.Name
	}
	want := req.Params[ParamContainer]
	switch {
	case want != "":
	case len(names) == 1:
		return names[0], nil
	default:
		want = req.Application
	}
	if slices.Contains(names, want) {
		return want, nil
	}
	return "", fmt.Errorf("container %q assente dalla task definition %s (container: %s): indicare %q", want, td.ARN, strings.Join(names, ", "), ParamContainer)
}

func (td *TaskDefinition) image(container string) string {
	for _, c := range td.Containers {
		if c.Name == container {
			return c.Image
		}
	}
	return ""
}

// state è lo stato di un target letto prima di deploy, dry-run e rollback.
type state struct {
	target    target
	api       API
	service   *Service
	current   *TaskDefinition
	container string
}

// read apre l'accesso alla region e legge servizio e task definition in esercizio.
func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	tg, err := parseTarget(t, req)
	if err != nil {
		return nil, err
	}
	api, err := p.connector.Connect(ctx, tg.region, req)
	if err != nil {
		return nil, err
	}
	svc, err := api.DescribeService(ctx, tg.cluster, serviceName(req))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("servizio %s assente dal cluster %s: va creato con l'infrastruttura (es. Terraform), manent ne aggiorna l'immagine", serviceName(req), tg.cluster)
	}
	if err != nil {
		return nil, fmt.Errorf("lettura del servizio: %w", err)
	}
	if svc.Status != "" && svc.Status != "ACTIVE" {
		return nil, fmt.Errorf("servizio %s nello stato %s", svc.Name, svc.Status)
	}
	if svc.Controller != "" && svc.Controller != "ECS" {
		return nil, fmt.Errorf("servizio %s con deployment controller %s: il plugin gestisce solo il controller ECS", svc.Name, svc.Controller)
	}
	td, err := api.DescribeTaskDefinition(ctx, svc.TaskDefinition)
	if err != nil {
		return nil, fmt.Errorf("lettura della task definition %s: %w", svc.TaskDefinition, err)
	}
	container, err := chooseContainer(td, req)
	if err != nil {
		return nil, err
	}
	return &state{target: tg, api: api, service: svc, current: td, container: container}, nil
}

func (s *state) details(req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.target.region, "cluster": s.target.cluster, "service": serviceName(req), "container": s.container}
	if v := s.current.Tags[TagVersion]; v != "" {
		d["previous"] = v
	}
	return d
}

// Deploy rilascia l'immagine su ogni target in sequenza; si ferma al primo errore. È
// idempotente: se il servizio usa già la revisione registrata da questo deploy, si limita ad
// attendere il completamento.
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
			return res, fmt.Errorf("target %s: %w", t.Host, err)
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
	details := s.details(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.target.region, "cluster", s.target.cluster, "service", serviceName(req))
	td := s.current
	if td.Tags[TagDeployment] != req.DeploymentID || td.image(s.container) != image {
		tags := map[string]string{
			TagApplication: req.Application, TagEnvironment: req.Environment,
			TagVersion: req.Artifact.Version, TagDeployment: req.DeploymentID,
		}
		if td, err = s.api.RegisterRevision(ctx, s.current, s.container, image, tags); err != nil {
			return details, fmt.Errorf("registrazione della task definition: %w", err)
		}
		log.InfoContext(ctx, "task definition registrata", "task_definition", td.ARN, "image", image)
		if err := s.api.UpdateService(ctx, s.target.cluster, serviceName(req), td.ARN); err != nil {
			return details, fmt.Errorf("aggiornamento del servizio: %w", err)
		}
	}
	details[DetailTaskDefinition] = td.ARN
	if err := p.wait(ctx, s, td.ARN, req, log); err != nil {
		return details, err
	}
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version, "task_definition", td.ARN)
	return details, nil
}

// wait attende che il deployment del servizio con la task definition arn sia completato.
// Un deployment FAILED (circuit breaker di ECS) è un errore immediato.
func (p *Plugin) wait(ctx context.Context, s *state, arn string, req *plugin.Request, log *slog.Logger) error {
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		svc, err := s.api.DescribeService(ctx, s.target.cluster, serviceName(req))
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("deployment non completato entro %s (%s)", timeout, last)
			}
			return fmt.Errorf("lettura del servizio: %w", err)
		}
		done, st, err := completed(svc, arn)
		if err != nil {
			return err
		}
		if done {
			log.InfoContext(ctx, "deployment completato", "task_definition", arn, "running", svc.RunningCount)
			return nil
		}
		if st != last {
			log.InfoContext(ctx, "deployment in corso", "state", st)
			last = st
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("deployment non completato entro %s (%s)", timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// completed dice se il deployment della task definition arn è concluso con successo.
func completed(svc *Service, arn string) (bool, string, error) {
	var d *Deployment
	for i := range svc.Deployments {
		if svc.Deployments[i].TaskDefinition == arn && svc.Deployments[i].Status == "PRIMARY" {
			d = &svc.Deployments[i]
		}
	}
	if d == nil {
		if svc.TaskDefinition != arn {
			return false, "", fmt.Errorf("il servizio usa ora %s: deployment sostituito da un altro aggiornamento", svc.TaskDefinition)
		}
		return false, "in attesa del deployment", nil
	}
	progress := fmt.Sprintf("%d/%d task in esecuzione, %d in avvio, %d falliti", d.Running, d.Desired, d.Pending, d.Failed)
	switch d.RolloutState {
	case "FAILED":
		return false, "", fmt.Errorf("deployment fallito: %s (%s)", d.RolloutReason, progress)
	case "COMPLETED":
		return true, progress, nil
	case "":
		// Servizi senza stato del rollout: completato quando resta il solo deployment primario
		// con tutti i task in esecuzione.
		if len(svc.Deployments) == 1 && d.Running == d.Desired {
			return true, progress, nil
		}
	}
	if old := len(svc.Deployments) - 1; old > 0 {
		progress += fmt.Sprintf(", %d deployment precedenti in arresto", old)
	}
	return false, progress, nil
}

// CurrentRelease legge su ogni target la versione dai tag della task definition in esercizio.
// Un servizio la cui task definition non ha il tag (mai rilasciato da manent) non ha release;
// versioni diverse tra i target sono un errore.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "tag della task definition"}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		v := s.current.Tags[TagVersion]
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailTaskDefinition+"@"+t.Host] = s.current.ARN
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
	return nil, fmt.Errorf("versioni diverse sui target: %s", strings.Join(desc, "; "))
}

// Rollback riporta ogni servizio alla task definition della release indicata: quella
// registrata nei dettagli della release o, in mancanza, la più recente della famiglia con il
// tag della versione. Prosegue anche se un target fallisce. È idempotente.
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
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
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
	details := s.details(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.target.region, "cluster", s.target.cluster, "service", serviceName(req), "rollback", true)
	arn := previous.Details[DetailTaskDefinition+"@"+t.Host]
	if arn == "" {
		if arn, err = p.findRevision(ctx, s, req, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailTaskDefinition] = arn
	if s.service.TaskDefinition != arn {
		if err := s.api.UpdateService(ctx, s.target.cluster, serviceName(req), arn); err != nil {
			return details, fmt.Errorf("aggiornamento del servizio: %w", err)
		}
	}
	if err := p.wait(ctx, s, arn, req, log); err != nil {
		return details, err
	}
	log.WarnContext(ctx, "release precedente ripristinata", "version", previous.Version, "task_definition", arn)
	return details, nil
}

// findRevision cerca, tra le revisioni più recenti della famiglia, quella con il tag della versione.
func (p *Plugin) findRevision(ctx context.Context, s *state, req *plugin.Request, version string) (string, error) {
	limit, _ := searchLimit(req)
	arns, err := s.api.ListRevisions(ctx, s.current.Family, limit)
	if err != nil {
		return "", fmt.Errorf("elenco delle revisioni di %s: %w", s.current.Family, err)
	}
	for _, arn := range arns {
		td, err := s.api.DescribeTaskDefinition(ctx, arn)
		if err != nil {
			return "", fmt.Errorf("lettura della task definition %s: %w", arn, err)
		}
		if td.Tags[TagVersion] == version && td.Tags[TagApplication] == req.Application && td.Tags[TagEnvironment] == req.Environment {
			return arn, nil
		}
	}
	return "", fmt.Errorf("nessuna revisione di %s con %s=%s tra le ultime %d", s.current.Family, TagVersion, version, limit)
}

// DryRun verifica su ogni target, senza modificarlo, che servizio, task definition e
// container esistano e che le credenziali permettano di leggerli.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		details := s.details(req)
		details[DetailTaskDefinition] = s.current.ARN
		if image, err := plugin.ImageReference(req.Artifact); err == nil {
			details["image"] = image
		}
		details["running"] = fmt.Sprintf("%d/%d", s.service.RunningCount, s.service.DesiredCount)
		p.logger.InfoContext(ctx, "servizio pronto per il deploy", "deployment", req.DeploymentID, "region", s.target.region, "service", serviceName(req), "dry_run", true)
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}
