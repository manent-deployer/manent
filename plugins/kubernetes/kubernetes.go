// Package kubernetes rilascia manifest su uno o più cluster Kubernetes con server-side apply,
// attende il completamento del rollout dei workload (Deployment, StatefulSet, DaemonSet) e
// conserva nel cluster lo storico delle release, da cui legge la release in esercizio e torna
// alla precedente.
//
// L'artefatto è un file di manifest (YAML o JSON, anche con più documenti), un archivio
// .tar.gz di manifest o un chart Helm impacchettato: è quello verificato con lo sha256 e
// promosso tra gli ambienti. Un chart viene renderizzato con "helm template" per ogni cluster
// e i suoi oggetti seguono lo stesso percorso dei manifest. Ogni target è il server API di un
// cluster. Il plugin parla direttamente con l'API REST, senza kubectl né client-go.
package kubernetes

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Name è il nome con cui il plugin si registra.
const Name = "kubernetes"

// Parametri del descrittore (deploy.params) letti dal plugin.
const (
	ParamNamespace      = "namespace"            // namespace degli oggetti senza namespace e dello storico (default "default")
	ParamTokenSecret    = "token_secret"         // segreto con il bearer token
	ParamCertSecret     = "client_cert_secret"   // in alternativa: certificato client PEM
	ParamKeySecret      = "client_key_secret"    // e relativa chiave privata PEM
	ParamCASecret       = "ca_secret"            // CA del cluster (PEM); default: ca_file del plugin o CA di sistema
	ParamWaitTimeout    = "wait_timeout"         // attesa del rollout dei workload (default 5m)
	ParamPrune          = "prune"                // "true": elimina gli oggetti della release precedente non più presenti
	ParamKeepReleases   = "keep_releases"        // release conservate nello storico (default 5, minimo 2)
	ParamRequireDigest  = "require_image_digest" // "true": le immagini dei workload devono essere fissate al digest
	ParamVarPrefix      = "var."                 // var.NOME sostituisce ${NOME} nei manifest
	defaultNamespace    = "default"
	defaultWaitTimeout  = 5 * time.Minute
	defaultKeepReleases = 5
)

// Etichette e annotazioni scritte da manent.
const (
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelApplication    = "manent.io/application"
	LabelEnvironment    = "manent.io/environment"
	LabelRole           = "manent.io/role"
	AnnotationVersion   = "manent.io/version"
	AnnotationDeploy    = "manent.io/deployment"
	managedBy           = "manent"
	roleRelease         = "release"
	rolePointer         = "current"
	dataVersion         = "version"
	dataRelease         = "release"
	dataObjects         = "objects"
	dataSHA256          = "sha256"
	dataDeployment      = "deployment"
	binaryObjects       = "objects.json.gz"
	configMapAPIVersion = "v1"
)

// DetailRelease è la chiave di Release.Details con il nome della ConfigMap della release.
const DetailRelease = "release"

var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// Plugin implementa plugin.Deployer, DryRunner, ReleaseInspector e Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	poll      time.Duration
	helm      Helm
}

// New crea il plugin con il connettore indicato e i tipi di artefatto supportati.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory costruisce il plugin dalla configurazione applicativa:
//   - ca_file: CA del cluster (es. quella del service account, con manent nel cluster)
//   - insecure_skip_tls_verify: true per non verificare il certificato (solo sviluppo)
//   - artifact_types: tipi supportati (default ["kubernetes"])
//   - helm: command (default "helm"), sha256 del binario, timeout del rendering (default 2m)
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	caFile, _ := cfg["ca_file"].(string)
	insecure, _ := cfg["insecure_skip_tls_verify"].(bool)
	types := []string{"kubernetes"}
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
	p := New(HTTPConnector{CAFile: caFile, InsecureSkipTLSVerify: insecure}, types, logger)
	if raw, ok := cfg["helm"]; ok {
		hc, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("helm deve essere una mappa (command, sha256, timeout)")
		}
		p.helm.Command, _ = hc["command"].(string)
		p.helm.SHA256, _ = hc["sha256"].(string)
		if t, ok := hc["timeout"].(string); ok {
			d, err := time.ParseDuration(t)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("helm.timeout %q non valido", t)
			}
			p.helm.Timeout = d
		}
	}
	return p, nil
}

// Name restituisce il nome del plugin.
func (p *Plugin) Name() string { return Name }

// Supports indica se il tipo di artefatto è gestito.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// Validate controlla parametri e target prima di qualsiasi connessione.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !versionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version è obbligatorio e identifica la release nello storico: ammessi lettere, cifre, '.', '_', '+', '-'"))
	}
	if ns := namespace(req); !dnsLabelRe.MatchString(ns) {
		errs = append(errs, fmt.Errorf("%s %q non valido", ParamNamespace, ns))
	}
	hasToken := req.Params[ParamTokenSecret] != ""
	hasCert := req.Params[ParamCertSecret] != "" || req.Params[ParamKeySecret] != ""
	switch {
	case hasToken && hasCert:
		errs = append(errs, fmt.Errorf("indicare %q oppure %q e %q, non entrambi", ParamTokenSecret, ParamCertSecret, ParamKeySecret))
	case !hasToken && !hasCert:
		errs = append(errs, fmt.Errorf("indicare %q oppure %q e %q", ParamTokenSecret, ParamCertSecret, ParamKeySecret))
	case hasCert && (req.Params[ParamCertSecret] == "" || req.Params[ParamKeySecret] == ""):
		errs = append(errs, fmt.Errorf("%q e %q vanno indicati insieme", ParamCertSecret, ParamKeySecret))
	}
	if _, err := waitTimeout(req); err != nil {
		errs = append(errs, err)
	}
	if _, err := keepReleases(req); err != nil {
		errs = append(errs, err)
	}
	if strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("l'artefatto è un'immagine: il plugin kubernetes rilascia manifest (file .yaml o archivio .tar.gz) o chart Helm (.tgz)"))
	}
	errs = append(errs, validateHelm(req.Params)...)
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("nessun cluster tra i target"))
	}
	return errors.Join(errs...)
}

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func namespace(req *plugin.Request) string {
	if ns := req.Params[ParamNamespace]; ns != "" {
		return ns
	}
	return defaultNamespace
}

func waitTimeout(req *plugin.Request) (time.Duration, error) {
	raw := req.Params[ParamWaitTimeout]
	if raw == "" {
		return defaultWaitTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q non valido: atteso una durata positiva (es. 10m)", ParamWaitTimeout, raw)
	}
	return d, nil
}

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

// connect apre l'accesso al cluster del target con le credenziali risolte dai segreti.
func (p *Plugin) connect(ctx context.Context, t plugin.Target, req *plugin.Request) (API, error) {
	var c Credentials
	var err error
	read := func(param string, dst *string) {
		if name := req.Params[param]; name != "" && err == nil {
			*dst, err = req.Secrets.Secret(ctx, name)
		}
	}
	read(ParamTokenSecret, &c.Token)
	read(ParamCertSecret, &c.ClientCert)
	read(ParamKeySecret, &c.ClientKey)
	read(ParamCASecret, &c.CA)
	if err != nil {
		return nil, err
	}
	c.Token = strings.TrimSpace(c.Token)
	api, err := p.connector.Connect(ctx, serverURL(t.Host, t.Port), c)
	if err != nil {
		return nil, fmt.Errorf("connessione al cluster: %w", err)
	}
	return api, nil
}

// names calcola i nomi delle ConfigMap dello storico: il puntatore alla release in esercizio
// e quella di ogni versione.
type names struct{ pointer, prefix string }

func historyNames(req *plugin.Request) names {
	base := dnsName("manent-" + req.Application + "-" + req.Environment)
	return names{pointer: base, prefix: base}
}

func (n names) release(version string) string { return dnsName(n.prefix + "-" + version) }

// dnsName rende un nome valido per Kubernetes (minuscole, cifre, '-', al più 63 caratteri):
// i nomi troppo lunghi sono accorciati con un hash, così restano univoci.
func dnsName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 || out != strings.ToLower(s) {
		sum := sha256.Sum256([]byte(s))
		h := hex.EncodeToString(sum[:])[:8]
		if len(out) > 54 {
			out = strings.TrimRight(out[:54], "-")
		}
		out += "-" + h
	}
	return out
}

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

// labelValue rende un valore valido per un'etichetta: invariato se lo è già, altrimenti
// normalizzato come un nome (con hash, per restare univoco).
func labelValue(s string) string {
	if labelValueRe.MatchString(s) {
		return s
	}
	return dnsName(s)
}

// prepared sono gli oggetti da applicare su un cluster.
type prepared struct {
	objects []Object
	refs    []ObjectRef
	// newNamespaces sono i Namespace creati dai manifest.
	newNamespaces []string
	// hooksSkipped sono gli hook del chart Helm non applicati.
	hooksSkipped []string
}

// prepare legge, interpreta e completa gli oggetti per il cluster: namespace di default,
// etichette e annotazioni di manent, controllo dei digest delle immagini.
func (p *Plugin) prepare(ctx context.Context, api API, req *plugin.Request) (*prepared, error) {
	objs, hooks, err := p.objects(ctx, api, req)
	if err != nil {
		return nil, err
	}
	pr := &prepared{hooksSkipped: hooks}
	seen := map[string]bool{}
	var errs []error
	for _, o := range objs {
		_, namespaced, err := api.Resource(ctx, o.APIVersion(), o.Kind())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.Ref(), err))
			continue
		}
		md := o.metadata()
		if namespaced && o.Namespace() == "" {
			md["namespace"] = namespace(req)
		}
		if !namespaced {
			delete(md, "namespace")
		}
		if o.Kind() == "Namespace" && o.APIVersion() == "v1" {
			pr.newNamespaces = append(pr.newNamespaces, o.Name())
		}
		o.setMap("labels", LabelManagedBy, managedBy)
		o.setMap("labels", LabelApplication, labelValue(req.Application))
		o.setMap("labels", LabelEnvironment, labelValue(req.Environment))
		o.setMap("annotations", AnnotationVersion, req.Artifact.Version)
		o.setMap("annotations", AnnotationDeploy, req.DeploymentID)
		if req.Params[ParamRequireDigest] == "true" {
			for _, img := range images(o) {
				if !strings.Contains(img, "@sha256:") {
					errs = append(errs, fmt.Errorf("%s: immagine %s non fissata al digest (%s)", o.Ref(), img, ParamRequireDigest))
				}
			}
		}
		ref := refOf(o)
		if seen[ref.String()] {
			errs = append(errs, fmt.Errorf("%s presente più volte nei manifest", o.Ref()))
		}
		seen[ref.String()] = true
		pr.objects = append(pr.objects, o)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sortObjects(pr.objects)
	for _, o := range pr.objects {
		pr.refs = append(pr.refs, refOf(o))
	}
	return pr, nil
}

// objects legge gli oggetti dell'artefatto: renderizza il chart Helm per il cluster, oppure
// legge i manifest e ne sostituisce le variabili. Per un chart restituisce anche gli hook tolti.
func (p *Plugin) objects(ctx context.Context, api API, req *plugin.Request) ([]Object, []string, error) {
	chart, err := isChart(req.Artifact.LocalPath)
	if err != nil {
		return nil, nil, err
	}
	if chart {
		kv, err := api.Version(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("versione del cluster: %w", err)
		}
		gvs, err := api.GroupVersions(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("API del cluster: %w", err)
		}
		raw, err := p.helm.Render(ctx, req, kv, gvs)
		if err != nil {
			return nil, nil, err
		}
		objs, err := parseObjects(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("manifest generati dal chart: %w", err)
		}
		objs, hooks := filterHooks(objs, req.Params)
		if len(objs) == 0 {
			return nil, nil, errors.New("il chart non genera oggetti da applicare")
		}
		return objs, hooks, nil
	}
	raw, err := loadManifests(req.Artifact.LocalPath)
	if err != nil {
		return nil, nil, err
	}
	vars := map[string]string{
		"MANENT_APPLICATION": req.Application, "MANENT_ENVIRONMENT": req.Environment,
		"MANENT_VERSION": req.Artifact.Version, "MANENT_NAMESPACE": namespace(req), "MANENT_DEPLOYMENT": req.DeploymentID,
	}
	for k, v := range req.Params {
		if name, ok := strings.CutPrefix(k, ParamVarPrefix); ok && name != "" {
			vars[name] = v
		}
	}
	objs, err := parseObjects(render(raw, vars))
	return objs, nil, err
}

// Deploy rilascia i manifest su ogni cluster in sequenza; si ferma al primo errore. Il
// server-side apply è idempotente, quindi il deploy può essere rieseguito.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "cluster", t.Host)
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return nil, err
	}
	pr, err := p.prepare(ctx, api, req)
	if err != nil {
		return nil, err
	}
	n := historyNames(req)
	ns := namespace(req)
	details := map[string]string{"namespace": ns, "objects": strconv.Itoa(len(pr.objects)), DetailRelease: n.release(req.Artifact.Version)}
	if len(pr.hooksSkipped) > 0 {
		details[DetailHooksSkipped] = strings.Join(pr.hooksSkipped, ",")
		log.WarnContext(ctx, "hook del chart non applicati", "hooks", pr.hooksSkipped)
	}
	prev, err := readPointer(ctx, api, ns, n)
	if err != nil {
		return details, err
	}
	if prev != nil {
		details["previous"] = prev.version
	}
	// I Namespace vanno creati prima dello storico, che può stare in uno di essi.
	for _, o := range pr.objects {
		if o.Kind() == "Namespace" {
			if _, err := api.Apply(ctx, o, false); err != nil {
				return details, err
			}
		}
	}
	// La release è salvata prima di applicare gli oggetti: un rollback deve poter sapere
	// cosa è stato creato anche se il deploy si ferma a metà.
	if err := saveRelease(ctx, api, ns, n, req, pr); err != nil {
		return details, err
	}
	for _, o := range pr.objects {
		if o.Kind() == "Namespace" {
			continue
		}
		if _, err := api.Apply(ctx, o, false); err != nil {
			return details, err
		}
	}
	log.InfoContext(ctx, "manifest applicati", "objects", len(pr.objects), "namespace", ns, "version", req.Artifact.Version)
	if err := p.waitRollout(ctx, api, pr.objects, req, log); err != nil {
		return details, err
	}
	if req.Params[ParamPrune] == "true" && prev != nil && prev.version != req.Artifact.Version {
		removed, err := prune(ctx, api, prev.objects, pr.refs)
		if err != nil {
			return details, err
		}
		if len(removed) > 0 {
			details["pruned"] = strings.Join(removed, ",")
			log.InfoContext(ctx, "oggetti della release precedente eliminati", "objects", removed)
		}
	}
	if err := writePointer(ctx, api, ns, n, req, req.Artifact.Version); err != nil {
		return details, err
	}
	p.pruneHistory(ctx, api, ns, n, req, prev, log)
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version)
	return details, nil
}

// waitRollout attende che i workload applicati abbiano completato il rollout.
func (p *Plugin) waitRollout(ctx context.Context, api API, objs []Object, req *plugin.Request, log *slog.Logger) error {
	timeout, _ := waitTimeout(req)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, o := range objs {
		if !workload(o) {
			continue
		}
		last := ""
		for {
			cur, err := api.Get(ctx, o.APIVersion(), o.Kind(), o.Namespace(), o.Name())
			if err != nil {
				if ctx.Err() != nil {
					return fmt.Errorf("%s: rollout non completato entro %s (%s)", o.Ref(), timeout, last)
				}
				return fmt.Errorf("%s: %w", o.Ref(), err)
			}
			ok, state, err := ready(cur)
			if err != nil {
				return fmt.Errorf("%s: %w", o.Ref(), err)
			}
			if ok {
				log.InfoContext(ctx, "rollout completato", "object", o.Ref())
				break
			}
			if state != last {
				log.InfoContext(ctx, "rollout in corso", "object", o.Ref(), "state", state)
				last = state
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: rollout non completato entro %s (%s)", o.Ref(), timeout, last)
			case <-time.After(p.poll):
			}
		}
	}
	return nil
}

func num(o Object, keys ...string) int {
	var cur any = map[string]any(o)
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = m[k]
	}
	switch v := cur.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// ready dice se il rollout del workload è completo; un errore indica un rollout fallito
// (es. Deployment oltre il progressDeadlineSeconds).
func ready(o Object) (bool, string, error) {
	gen, observed := num(o, "metadata", "generation"), num(o, "status", "observedGeneration")
	if observed < gen {
		return false, "in attesa del controller", nil
	}
	replicas := 1
	if spec, _ := o["spec"].(map[string]any); spec != nil {
		if _, ok := spec["replicas"]; ok {
			replicas = num(o, "spec", "replicas")
		}
	}
	switch o.Kind() {
	case "Deployment":
		status, _ := o["status"].(map[string]any)
		conds, _ := status["conditions"].([]any)
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Progressing" && m["status"] == "False" && m["reason"] == "ProgressDeadlineExceeded" {
				return false, "", fmt.Errorf("rollout bloccato: %v", m["message"])
			}
		}
		updated, total, available := num(o, "status", "updatedReplicas"), num(o, "status", "replicas"), num(o, "status", "availableReplicas")
		switch {
		case updated < replicas:
			return false, fmt.Sprintf("%d/%d repliche aggiornate", updated, replicas), nil
		case total > updated:
			return false, fmt.Sprintf("%d repliche precedenti in terminazione", total-updated), nil
		case available < updated:
			return false, fmt.Sprintf("%d/%d repliche disponibili", available, replicas), nil
		}
	case "StatefulSet":
		updated, readyN := num(o, "status", "updatedReplicas"), num(o, "status", "readyReplicas")
		strategy := o.str("spec", "updateStrategy", "type")
		if strategy != "OnDelete" && updated < replicas {
			return false, fmt.Sprintf("%d/%d repliche aggiornate", updated, replicas), nil
		}
		if readyN < replicas {
			return false, fmt.Sprintf("%d/%d repliche pronte", readyN, replicas), nil
		}
		if cur, upd := o.str("status", "currentRevision"), o.str("status", "updateRevision"); strategy != "OnDelete" && upd != "" && cur != upd {
			return false, "revisione in aggiornamento", nil
		}
	case "DaemonSet":
		desired := num(o, "status", "desiredNumberScheduled")
		if u := num(o, "status", "updatedNumberScheduled"); u < desired {
			return false, fmt.Sprintf("%d/%d nodi aggiornati", u, desired), nil
		}
		if a := num(o, "status", "numberAvailable"); a < desired {
			return false, fmt.Sprintf("%d/%d nodi disponibili", a, desired), nil
		}
	}
	return true, "completato", nil
}

// release è una release letta dallo storico.
type release struct {
	version string
	name    string
	objects []ObjectRef
}

// readPointer legge la release in esercizio (nil se non ce n'è una) con i suoi oggetti.
func readPointer(ctx context.Context, api API, ns string, n names) (*release, error) {
	cm, err := api.Get(ctx, configMapAPIVersion, "ConfigMap", ns, n.pointer)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lettura della release in esercizio: %w", err)
	}
	v := cm.str("data", dataVersion)
	if v == "" {
		return nil, nil
	}
	rel, err := readRelease(ctx, api, ns, n, v)
	if errors.Is(err, ErrNotFound) {
		// Storico incompleto (es. ConfigMap eliminata a mano): si conosce solo la versione.
		return &release{version: v, name: n.release(v)}, nil
	}
	return rel, err
}

// readRelease legge una release dello storico, oggetti compresi.
func readRelease(ctx context.Context, api API, ns string, n names, version string) (*release, error) {
	cm, err := api.Get(ctx, configMapAPIVersion, "ConfigMap", ns, n.release(version))
	if err != nil {
		return nil, err
	}
	rel := &release{version: cm.str("data", dataVersion), name: n.release(version)}
	if raw := cm.str("data", dataObjects); raw != "" {
		if err := json.Unmarshal([]byte(raw), &rel.objects); err != nil {
			return nil, fmt.Errorf("storico della release %s non leggibile: %w", version, err)
		}
	}
	return rel, nil
}

// loadObjects legge dallo storico gli oggetti applicati da una release.
func loadObjects(ctx context.Context, api API, ns string, n names, version string) ([]Object, error) {
	cm, err := api.Get(ctx, configMapAPIVersion, "ConfigMap", ns, n.release(version))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("release %s assente dallo storico del cluster", version)
		}
		return nil, err
	}
	enc := cm.str("binaryData", binaryObjects)
	if enc == "" {
		return nil, fmt.Errorf("release %s senza manifest nello storico", version)
	}
	gz, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(zr, 32<<20))
	if err != nil {
		return nil, err
	}
	var objs []Object
	if err := json.Unmarshal(data, &objs); err != nil {
		return nil, fmt.Errorf("manifest della release %s non leggibili: %w", version, err)
	}
	return objs, nil
}

func historyLabels(req *plugin.Request, role string) map[string]any {
	return map[string]any{
		LabelManagedBy:   managedBy,
		LabelApplication: labelValue(req.Application),
		LabelEnvironment: labelValue(req.Environment),
		LabelRole:        role,
	}
}

// saveRelease salva nello storico gli oggetti della release (compressi in binaryData, per
// restare sotto il limite di 1 MiB di una ConfigMap).
func saveRelease(ctx context.Context, api API, ns string, n names, req *plugin.Request, pr *prepared) error {
	data, err := json.Marshal(pr.objects)
	if err != nil {
		return err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(data); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if gz.Len() > 900<<10 {
		return fmt.Errorf("manifest troppo grandi per lo storico (%d byte compressi, massimo 900 KiB)", gz.Len())
	}
	refs, err := json.Marshal(pr.refs)
	if err != nil {
		return err
	}
	cm := Object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": n.release(req.Artifact.Version), "namespace": ns,
			"labels":      historyLabels(req, roleRelease),
			"annotations": map[string]any{AnnotationVersion: req.Artifact.Version, AnnotationDeploy: req.DeploymentID},
		},
		"data": map[string]any{
			dataVersion: req.Artifact.Version, dataObjects: string(refs), dataSHA256: req.Artifact.SHA256, dataDeployment: req.DeploymentID,
		},
		"binaryData": map[string]any{binaryObjects: base64.StdEncoding.EncodeToString(gz.Bytes())},
	}
	if _, err := api.Apply(ctx, cm, false); err != nil {
		return fmt.Errorf("salvataggio della release nello storico: %w", err)
	}
	return nil
}

// writePointer indica la release in esercizio.
func writePointer(ctx context.Context, api API, ns string, n names, req *plugin.Request, version string) error {
	cm := Object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": n.pointer, "namespace": ns, "labels": historyLabels(req, rolePointer)},
		"data":     map[string]any{dataVersion: version, dataRelease: n.release(version), dataDeployment: req.DeploymentID},
	}
	if _, err := api.Apply(ctx, cm, false); err != nil {
		return fmt.Errorf("aggiornamento della release in esercizio: %w", err)
	}
	return nil
}

// prune elimina gli oggetti della release precedente che la nuova non contiene più.
func prune(ctx context.Context, api API, previous, current []ObjectRef) ([]string, error) {
	keep := map[string]bool{}
	for _, r := range current {
		keep[r.String()] = true
	}
	var removed []string
	for i := len(previous) - 1; i >= 0; i-- {
		r := previous[i]
		if keep[r.String()] || r.Kind == "Namespace" {
			// I Namespace non vengono mai eliminati: cancellerebbero tutto il loro contenuto.
			continue
		}
		if err := api.Delete(ctx, r.APIVersion, r.Kind, r.Namespace, r.Name); err != nil {
			return removed, fmt.Errorf("eliminazione di %s: %w", r, err)
		}
		removed = append(removed, r.String())
	}
	return removed, nil
}

// pruneHistory elimina le release più vecchie oltre keep_releases, senza mai toccare quella
// in esercizio e la precedente. Un errore non compromette il deploy.
func (p *Plugin) pruneHistory(ctx context.Context, api API, ns string, n names, req *plugin.Request, prev *release, log *slog.Logger) {
	keep, _ := keepReleases(req)
	sel := LabelManagedBy + "=" + managedBy + "," + LabelRole + "=" + roleRelease + "," +
		LabelApplication + "=" + labelValue(req.Application) + "," + LabelEnvironment + "=" + labelValue(req.Environment)
	list, err := api.List(ctx, configMapAPIVersion, "ConfigMap", ns, sel)
	if err != nil {
		log.WarnContext(ctx, "pulizia dello storico fallita", "error", err)
		return
	}
	protect := map[string]bool{n.release(req.Artifact.Version): true}
	if prev != nil {
		protect[n.release(prev.version)] = true
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].str("metadata", "creationTimestamp") > list[j].str("metadata", "creationTimestamp")
	})
	kept := 0
	for _, cm := range list {
		if protect[cm.Name()] {
			continue
		}
		if kept < keep-len(protect) {
			kept++
			continue
		}
		if err := api.Delete(ctx, configMapAPIVersion, "ConfigMap", ns, cm.Name()); err != nil {
			log.WarnContext(ctx, "pulizia dello storico fallita", "release", cm.Name(), "error", err)
		}
	}
}

// CurrentRelease legge su ogni cluster la release in esercizio dallo storico. Un cluster senza
// storico (es. appena aggiunto) viene ignorato; release diverse tra i cluster sono un errore.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	n := historyNames(req)
	found := map[string][]string{}
	for _, t := range req.Targets {
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		rel, err := readPointer(ctx, api, namespace(req), n)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if rel != nil {
			found[rel.version] = append(found[rel.version], t.Host)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v, hosts := range found {
			return &plugin.Release{Version: v, Details: map[string]string{
				DetailRelease: n.release(v), "source": "storico nel cluster", "targets": strings.Join(hosts, ","),
			}}, nil
		}
	}
	var desc []string
	for v, hosts := range found {
		desc = append(desc, v+" su "+strings.Join(hosts, ","))
	}
	sort.Strings(desc)
	return nil, fmt.Errorf("release diverse sui cluster: %s", strings.Join(desc, "; "))
}

// Rollback riapplica su ogni cluster gli oggetti della release indicata, salvati nello
// storico, ne attende il rollout e, con prune, elimina gli oggetti creati solo dalla release
// fallita. Prosegue anche se un cluster fallisce e riporta gli errori alla fine. È idempotente.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !versionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("versione %q non valida", previous.Version)
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.rollbackTarget(ctx, t, req, previous.Version)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("cluster %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, req *plugin.Request, version string) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "cluster", t.Host, "rollback", true)
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return nil, err
	}
	ns, n := namespace(req), historyNames(req)
	details := map[string]string{DetailRelease: n.release(version)}
	objs, err := loadObjects(ctx, api, ns, n, version)
	if err != nil {
		return details, err
	}
	refs := make([]ObjectRef, len(objs))
	for i, o := range objs {
		refs[i] = refOf(o)
		if _, err := api.Apply(ctx, o, false); err != nil {
			return details, err
		}
	}
	if err := p.waitRollout(ctx, api, objs, req, log); err != nil {
		return details, err
	}
	if req.Params[ParamPrune] == "true" && version != req.Artifact.Version {
		if failed, err := readRelease(ctx, api, ns, n, req.Artifact.Version); err == nil {
			removed, err := prune(ctx, api, failed.objects, refs)
			if err != nil {
				return details, err
			}
			if len(removed) > 0 {
				details["pruned"] = strings.Join(removed, ",")
			}
		} else if !errors.Is(err, ErrNotFound) {
			return details, err
		}
	}
	if err := writePointer(ctx, api, ns, n, req, version); err != nil {
		return details, err
	}
	log.WarnContext(ctx, "release precedente ripristinata", "version", version)
	return details, nil
}

// DryRun verifica su ogni cluster, senza modificarlo, che il deploy sia eseguibile: credenziali,
// tipi degli oggetti e un apply con dryRun=All, che passa da validazione, admission e RBAC del
// cluster. Gli oggetti in un namespace creato dagli stessi manifest non sono verificabili.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("cluster %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := map[string]string{"namespace": namespace(req)}
	v, err := api.Version(ctx)
	if err != nil {
		return details, fmt.Errorf("accesso al cluster: %w", err)
	}
	details["cluster_version"] = v
	pr, err := p.prepare(ctx, api, req)
	if err != nil {
		return details, err
	}
	missing := map[string]bool{}
	for _, ns := range pr.newNamespaces {
		if _, err := api.Get(ctx, "v1", "Namespace", "", ns); errors.Is(err, ErrNotFound) {
			missing[ns] = true
		}
	}
	checked, skipped := 0, 0
	for _, o := range pr.objects {
		if missing[o.Namespace()] {
			skipped++
			continue
		}
		if _, err := api.Apply(ctx, o, true); err != nil {
			return details, err
		}
		checked++
	}
	details["objects_checked"], details["objects_skipped"] = strconv.Itoa(checked), strconv.Itoa(skipped)
	if len(pr.hooksSkipped) > 0 {
		details[DetailHooksSkipped] = strings.Join(pr.hooksSkipped, ",")
	}
	if rel, err := readPointer(ctx, api, namespace(req), historyNames(req)); err == nil && rel != nil {
		details["current"] = rel.version
	}
	p.logger.InfoContext(ctx, "cluster pronto per il deploy", "deployment", req.DeploymentID, "cluster", t.Host, "dry_run", true, "objects", checked)
	return details, nil
}
