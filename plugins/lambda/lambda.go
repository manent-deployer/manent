// Package lambda rilascia il codice di una funzione AWS Lambda: aggiorna il codice (immagine
// di container o pacchetto .zip), pubblica una versione immutabile e sposta su di essa un
// alias. La release in esercizio è la versione puntata dall'alias; il rollback riporta
// l'alias alla versione precedente, senza ricaricare il codice.
//
// La funzione (ruolo, memoria, variabili, trigger) resta di chi gestisce l'infrastruttura
// (Terraform, CloudFormation, SAM): il plugin cambia solo il codice. Chi invoca la funzione
// deve usare l'alias (ARN qualificato), non $LATEST. Ogni target è una region.
package lambda

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
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
const Name = "lambda"

// Parametri del descrittore (deploy.params) letti dal plugin, oltre a quelli delle
// credenziali di awsauth.
const (
	ParamFunction      = "function"     // nome della funzione (default: nome dell'applicazione)
	ParamAlias         = "alias"        // alias spostato sulla nuova versione (default "live")
	ParamWaitTimeout   = "wait_timeout" // attesa dell'aggiornamento e della versione (default 10m)
	defaultAlias       = "live"
	defaultWaitTimeout = 10 * time.Minute
	// maxZipSize è il limite di Lambda per il caricamento diretto di un .zip.
	maxZipSize = 50 << 20
)

// Tipi di pacchetto di una funzione.
const (
	PackageImage = "Image"
	PackageZip   = "Zip"
)

// DetailFunctionVersion è il prefisso delle chiavi di Release.Details con la versione della
// funzione su ciascun target ("function_version@<target>").
const DetailFunctionVersion = "function_version"

// ErrNotFound indica una funzione, una versione o un alias inesistente.
var ErrNotFound = errors.New("non trovato")

var (
	functionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	aliasRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	numericRe  = regexp.MustCompile(`^[0-9]+$`)
	ecrRe      = regexp.MustCompile(`^([0-9]{12}\.dkr\.ecr\.)([a-z0-9-]+)(\.amazonaws\.com(\.cn)?/)`)
)

// Function è la configurazione di una funzione o di una sua versione.
type Function struct {
	Name             string
	PackageType      string
	State            string // Pending, Active, Inactive, Failed
	StateReason      string
	LastUpdateStatus string // InProgress, Successful, Failed
	LastUpdateReason string
	CodeSha256       string
	Version          string
	Description      string
}

// Alias è un alias della funzione.
type Alias struct {
	Name            string
	FunctionVersion string
	Description     string
}

// Code è il nuovo codice: un'immagine o un pacchetto .zip.
type Code struct {
	ImageURI string
	ZipFile  []byte
}

// API sono le operazioni su Lambda usate dal plugin, in una region.
type API interface {
	// GetFunction legge la configurazione della funzione o, con qualifier, di una versione.
	GetFunction(ctx context.Context, name, qualifier string) (*Function, error)
	UpdateCode(ctx context.Context, name string, code Code, dryRun bool) (*Function, error)
	PublishVersion(ctx context.Context, name, codeSha256, description string) (*Function, error)
	GetAlias(ctx context.Context, name, alias string) (*Alias, error)
	// PutAlias crea l'alias o lo sposta sulla versione, azzerando eventuali pesi di traffico.
	PutAlias(ctx context.Context, name, alias, version, description string, create bool) error
	ListVersions(ctx context.Context, name string) ([]Function, error)
}

// Connector apre l'accesso a Lambda in una region con le credenziali della richiesta.
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
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - endpoint: endpoint alternativo dell'API Lambda (es. LocalStack)
//   - artifact_types: tipi supportati (default ["lambda"])
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

func functionName(req *plugin.Request) string {
	if f := req.Params[ParamFunction]; f != "" {
		return f
	}
	return req.Application
}

func aliasName(req *plugin.Request) string {
	if a := req.Params[ParamAlias]; a != "" {
		return a
	}
	return defaultAlias
}

func isImage(a plugin.Artifact) bool { return strings.HasSuffix(a.LocalPath, plugin.ImageRefExt) }

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e identifica la versione della funzione: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if req.Artifact.LocalPath != "" && !isImage(req.Artifact) && !strings.HasSuffix(strings.ToLower(req.Artifact.LocalPath), ".zip") {
		errs = append(errs, errors.New("l'artefatto deve essere un pacchetto .zip o un'immagine di un registry (artifact.source oci://…)"))
	}
	if !functionRe.MatchString(functionName(req)) {
		errs = append(errs, fmt.Errorf("%s %q non valido: ammessi lettere, cifre, '_', '-' (al più 64)", ParamFunction, functionName(req)))
	}
	if a := aliasName(req); !aliasRe.MatchString(a) || numericRe.MatchString(a) {
		errs = append(errs, fmt.Errorf("%s %q non valido: lettere, cifre, '_', '-', non solo cifre", ParamAlias, a))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if err := awsauth.Validate(req.Params); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("nessuna region tra i target"))
	}
	for _, t := range req.Targets {
		if !awsauth.ValidRegion(t.Host) {
			errs = append(errs, fmt.Errorf("target %q: atteso il nome di una region (es. eu-west-1)", t.Host))
		}
	}
	return errors.Join(errs...)
}

// regionalImage riporta un'immagine ECR alla region del target: Lambda accetta solo immagini
// di un repository della stessa region, e con la replica di ECR il digest è lo stesso.
func regionalImage(ref, region string) string {
	return ecrRe.ReplaceAllString(ref, "${1}"+region+"${3}")
}

// code prepara il codice per la region: l'immagine fissata al digest o il contenuto del .zip.
func code(a plugin.Artifact, region string) (Code, error) {
	if isImage(a) {
		ref, err := plugin.ImageReference(a)
		if err != nil {
			return Code{}, err
		}
		return Code{ImageURI: regionalImage(ref, region)}, nil
	}
	st, err := os.Stat(a.LocalPath)
	if err != nil {
		return Code{}, err
	}
	if st.Size() > maxZipSize {
		return Code{}, fmt.Errorf("pacchetto di %d MiB: Lambda accetta al più 50 MiB caricati direttamente, usare un'immagine di container", st.Size()>>20)
	}
	b, err := os.ReadFile(a.LocalPath)
	if err != nil {
		return Code{}, err
	}
	return Code{ZipFile: b}, nil
}

// description è la descrizione di versioni e alias: da qui il plugin rilegge la release.
func description(version, deploymentID string) string {
	return "manent version=" + version + " deployment=" + deploymentID
}

// parseDescription legge versione e deploy da una descrizione scritta da manent.
func parseDescription(s string) (version, deploymentID string) {
	rest, ok := strings.CutPrefix(s, "manent ")
	if !ok {
		return "", ""
	}
	for _, f := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(f, "version="); ok {
			version = v
		}
		if v, ok := strings.CutPrefix(f, "deployment="); ok {
			deploymentID = v
		}
	}
	return version, deploymentID
}

// state è lo stato di un target letto prima di deploy, dry-run e rollback.
type state struct {
	region string
	api    API
	fn     *Function
	alias  *Alias
}

func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	if !awsauth.ValidRegion(t.Host) {
		return nil, fmt.Errorf("region %q non valida", t.Host)
	}
	api, err := p.connector.Connect(ctx, t.Host, req)
	if err != nil {
		return nil, err
	}
	name := functionName(req)
	fn, err := api.GetFunction(ctx, name, "")
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("funzione %s assente: va creata con l'infrastruttura (es. Terraform), manent ne aggiorna il codice", name)
	}
	if err != nil {
		return nil, fmt.Errorf("lettura della funzione %s: %w", name, err)
	}
	want := PackageZip
	if isImage(req.Artifact) {
		want = PackageImage
	}
	if req.Artifact.LocalPath != "" && fn.PackageType != "" && fn.PackageType != want {
		return nil, fmt.Errorf("la funzione %s usa pacchetti %s, l'artefatto è di tipo %s", name, fn.PackageType, want)
	}
	alias, err := api.GetAlias(ctx, name, aliasName(req))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("lettura dell'alias %s: %w", aliasName(req), err)
	}
	return &state{region: t.Host, api: api, fn: fn, alias: alias}, nil
}

func (s *state) details(req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.region, "function": functionName(req), "alias": aliasName(req)}
	if s.alias != nil {
		if v, _ := parseDescription(s.alias.Description); v != "" {
			d["previous"] = v
		}
	}
	return d
}

// Deploy rilascia il codice su ogni region in sequenza; si ferma al primo errore. È
// idempotente: se l'alias punta già alla versione pubblicata da questo deploy non fa nulla, e
// ripubblicare lo stesso codice restituisce la versione esistente.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	name := functionName(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.region, "function", name)
	if s.alias != nil {
		if _, dep := parseDescription(s.alias.Description); dep == req.DeploymentID {
			details[DetailFunctionVersion] = s.alias.FunctionVersion
			log.InfoContext(ctx, "alias già sulla versione di questo deploy", "alias", aliasName(req), "function_version", s.alias.FunctionVersion)
			return details, nil
		}
	}
	c, err := code(req.Artifact, s.region)
	if err != nil {
		return details, err
	}
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Un aggiornamento in corso (es. della configurazione) farebbe fallire quello del codice;
	// uno fallito in precedenza no.
	if _, err := p.waitFunction(ctx, s.api, name, "", timeout, false, log); err != nil {
		return details, err
	}
	if _, err := s.api.UpdateCode(ctx, name, c, false); err != nil {
		return details, fmt.Errorf("aggiornamento del codice: %w", err)
	}
	if c.ImageURI != "" {
		details["image"] = c.ImageURI
	}
	fn, err := p.waitFunction(ctx, s.api, name, "", timeout, true, log)
	if err != nil {
		return details, err
	}
	if c.ZipFile != nil {
		if want, err := hexToBase64(req.Artifact.SHA256); err == nil && want != "" && fn.CodeSha256 != want {
			return details, fmt.Errorf("sha256 del codice caricato (%s) diverso da quello dell'artefatto (%s)", fn.CodeSha256, want)
		}
	}
	// CodeSha256 impedisce di pubblicare codice cambiato nel frattempo da un altro aggiornamento.
	v, err := s.api.PublishVersion(ctx, name, fn.CodeSha256, description(req.Artifact.Version, req.DeploymentID))
	if err != nil {
		return details, fmt.Errorf("pubblicazione della versione: %w", err)
	}
	details[DetailFunctionVersion] = v.Version
	if _, err := p.waitFunction(ctx, s.api, name, v.Version, timeout, true, log); err != nil {
		return details, err
	}
	if err := s.api.PutAlias(ctx, name, aliasName(req), v.Version, description(req.Artifact.Version, req.DeploymentID), s.alias == nil); err != nil {
		return details, fmt.Errorf("aggiornamento dell'alias %s: %w", aliasName(req), err)
	}
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version, "alias", aliasName(req), "function_version", v.Version)
	return details, nil
}

func hexToBase64(h string) (string, error) {
	if h == "" {
		return "", nil
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// waitFunction attende che la funzione (o la versione qualifier) sia attiva e senza
// aggiornamenti in corso. Con strict, un aggiornamento fallito è un errore immediato.
func (p *Plugin) waitFunction(ctx context.Context, api API, name, qualifier string, timeout time.Duration, strict bool, log *slog.Logger) (*Function, error) {
	what := "funzione " + name
	if qualifier != "" {
		what += " versione " + qualifier
	}
	last := ""
	for {
		fn, err := api.GetFunction(ctx, name, qualifier)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s non pronta entro %s (%s)", what, timeout, last)
			}
			return nil, fmt.Errorf("lettura della %s: %w", what, err)
		}
		switch {
		case fn.State == "Failed":
			return nil, fmt.Errorf("%s nello stato Failed: %s", what, fn.StateReason)
		case strict && fn.LastUpdateStatus == "Failed":
			return nil, fmt.Errorf("aggiornamento della %s fallito: %s", what, fn.LastUpdateReason)
		case fn.State != "Pending" && fn.LastUpdateStatus != "InProgress":
			return fn, nil
		}
		st := fn.State + "/" + fn.LastUpdateStatus
		if st != last {
			log.InfoContext(ctx, "in attesa della funzione", "function_version", qualifier, "state", fn.State, "last_update", fn.LastUpdateStatus)
			last = st
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s non pronta entro %s (%s)", what, timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// CurrentRelease legge su ogni region la versione puntata dall'alias. Un alias assente o non
// creato da manent non ha release; versioni diverse tra le region sono un errore.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "alias " + aliasName(req)}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if s.alias == nil {
			continue
		}
		v, _ := parseDescription(s.alias.Description)
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailFunctionVersion+"@"+t.Host] = s.alias.FunctionVersion
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

// Rollback riporta l'alias di ogni region alla versione della funzione della release
// indicata: quella nei dettagli della release o, in mancanza, la più recente pubblicata con
// quella versione. Prosegue anche se una region fallisce. È idempotente.
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
	details := s.details(req)
	name := functionName(req)
	if s.alias == nil {
		return details, fmt.Errorf("alias %s assente: nessuna versione da ripristinare", aliasName(req))
	}
	fv := previous.Details[DetailFunctionVersion+"@"+t.Host]
	depID := ""
	if fv != "" {
		v, err := s.api.GetFunction(ctx, name, fv)
		if err != nil {
			return details, fmt.Errorf("lettura della versione %s: %w", fv, err)
		}
		_, depID = parseDescription(v.Description)
	} else {
		if fv, depID, err = p.findVersion(ctx, s.api, name, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailFunctionVersion] = fv
	if s.alias.FunctionVersion != fv {
		if err := s.api.PutAlias(ctx, name, aliasName(req), fv, description(previous.Version, depID), false); err != nil {
			return details, fmt.Errorf("aggiornamento dell'alias %s: %w", aliasName(req), err)
		}
	}
	p.logger.WarnContext(ctx, "release precedente ripristinata", "deployment", req.DeploymentID, "region", s.region,
		"function", name, "version", previous.Version, "function_version", fv, "rollback", true)
	return details, nil
}

// findVersion cerca la versione della funzione più recente pubblicata con la versione indicata.
func (p *Plugin) findVersion(ctx context.Context, api API, name, version string) (string, string, error) {
	list, err := api.ListVersions(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("elenco delle versioni di %s: %w", name, err)
	}
	best, bestN, depID := "", -1, ""
	for _, f := range list {
		v, dep := parseDescription(f.Description)
		n, err := strconv.Atoi(f.Version)
		if v != version || err != nil || n <= bestN {
			continue
		}
		best, bestN, depID = f.Version, n, dep
	}
	if best == "" {
		return "", "", fmt.Errorf("nessuna versione di %s pubblicata con la versione %s", name, version)
	}
	return best, depID, nil
}

// DryRun verifica su ogni region, senza modificarla, che funzione e tipo di pacchetto siano
// quelli attesi e che le credenziali permettano di aggiornare il codice (UpdateFunctionCode
// con DryRun).
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	details["package_type"] = s.fn.PackageType
	if s.alias == nil {
		details["alias_status"] = "da creare"
	}
	c, err := code(req.Artifact, s.region)
	if err != nil {
		return details, err
	}
	if c.ImageURI != "" {
		details["image"] = c.ImageURI
	}
	if _, err := s.api.UpdateCode(ctx, functionName(req), c, true); err != nil {
		return details, fmt.Errorf("verifica dell'aggiornamento del codice: %w", err)
	}
	p.logger.InfoContext(ctx, "funzione pronta per il deploy", "deployment", req.DeploymentID, "region", s.region, "function", functionName(req), "dry_run", true)
	return details, nil
}
