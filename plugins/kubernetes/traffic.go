package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Parametri dei rilasci per percentuale (rollout con strategy: traffic).
const (
	ParamTrafficRouter         = "traffic_router"          // gateway-api (default) o istio
	ParamTrafficRoute          = "traffic_route"           // HTTPRoute (o GRPCRoute) o VirtualService che divide il traffico
	ParamTrafficRouteKind      = "traffic_route_kind"      // gateway-api: HTTPRoute (default) o GRPCRoute
	ParamTrafficRouteNamespace = "traffic_route_namespace" // namespace della route (default: namespace)
	ParamTrafficService        = "traffic_service"         // gateway-api: nome base dei Service degli slot (<nome>-a, <nome>-b)
)

// Router supportati.
const (
	RouterGatewayAPI = "gateway-api"
	RouterIstio      = "istio"
)

// LabelTrack è l'etichetta dello slot sugli oggetti e sui pod.
const LabelTrack = "manent.io/track"

const gatewayAPIVersion = "gateway.networking.k8s.io/v1"

// Chiavi dello stato registrato da PrepareSlots, per cluster ("<chiave>@<host>").
const (
	stateStable        = "stable"
	stateStableVersion = "stable_version"
	stateCandidate     = "candidate"
)

func stateKey(k string, t plugin.Target) string { return k + "@" + t.Host }

// slotted sono i tipi di oggetto duplicati per slot, con il suffisso del nome.
var slotted = []string{"Deployment", "StatefulSet", "DaemonSet", "Service", "HorizontalPodAutoscaler", "PodDisruptionBudget"}

func validateTraffic(req *plugin.Request) []error {
	if req.Traffic == nil {
		return nil
	}
	var errs []error
	switch r := router(req); r {
	case RouterGatewayAPI:
		if !dnsLabelRe.MatchString(req.Params[ParamTrafficService]) {
			errs = append(errs, fmt.Errorf("%s obbligatorio con %s %s (nome base dei Service <nome>-a e <nome>-b)", ParamTrafficService, ParamTrafficRouter, r))
		}
		if k := req.Params[ParamTrafficRouteKind]; k != "" && k != "HTTPRoute" && k != "GRPCRoute" {
			errs = append(errs, fmt.Errorf("%s %q non valido (HTTPRoute|GRPCRoute)", ParamTrafficRouteKind, k))
		}
	default:
		errs = append(errs, fmt.Errorf("%s %q non valido (%s)", ParamTrafficRouter, r, RouterGatewayAPI))
	}
	if !dnsLabelRe.MatchString(req.Params[ParamTrafficRoute]) {
		errs = append(errs, fmt.Errorf("%s obbligatorio: la route che divide il traffico tra gli slot", ParamTrafficRoute))
	}
	if ns := req.Params[ParamTrafficRouteNamespace]; ns != "" && !dnsLabelRe.MatchString(ns) {
		errs = append(errs, fmt.Errorf("%s %q non valido", ParamTrafficRouteNamespace, ns))
	}
	return errs
}

func router(req *plugin.Request) string {
	if r := req.Params[ParamTrafficRouter]; r != "" {
		return r
	}
	return RouterGatewayAPI
}

func routeNamespace(req *plugin.Request) string {
	if ns := req.Params[ParamTrafficRouteNamespace]; ns != "" {
		return ns
	}
	return namespace(req)
}

// slotObjects duplica gli oggetti per lo slot: suffisso -<slot> ai nomi di workload, Service,
// HPA e PDB, etichetta manent.io/track su oggetti, selettori e pod. Con Istio i Service restano
// comuni ai due slot (il router sceglie il subset dall'etichetta dei pod).
func slotObjects(objs []Object, slot string, services bool) ([]Object, error) {
	renamed := map[string]bool{}
	for _, o := range objs {
		if slices.Contains(slotted, o.Kind()) && (services || o.Kind() != "Service") {
			renamed[o.Kind()+"/"+o.Name()] = true
		}
	}
	suffix := "-" + slot
	var errs []error
	for _, o := range objs {
		o.setMap("labels", LabelTrack, slot)
		if !renamed[o.Kind()+"/"+o.Name()] {
			continue
		}
		name := o.Name() + suffix
		if len(name) > 63 {
			errs = append(errs, fmt.Errorf("%s: nome %q troppo lungo per lo slot (massimo 63 caratteri)", o.Ref(), name))
		}
		o.metadata()["name"] = name
		spec, _ := o["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		switch o.Kind() {
		case "Deployment", "StatefulSet", "DaemonSet":
			setPath(spec, slot, "selector", "matchLabels", LabelTrack)
			setPath(spec, slot, "template", "metadata", "labels", LabelTrack)
			if sn, _ := spec["serviceName"].(string); sn != "" && renamed["Service/"+sn] {
				spec["serviceName"] = sn + suffix
			}
		case "Service":
			if sel, ok := spec["selector"].(map[string]any); ok && len(sel) > 0 {
				sel[LabelTrack] = slot
			}
		case "PodDisruptionBudget":
			setPath(spec, slot, "selector", "matchLabels", LabelTrack)
		case "HorizontalPodAutoscaler":
			if ref, ok := spec["scaleTargetRef"].(map[string]any); ok {
				if k, _ := ref["kind"].(string); renamed[k+"/"+fmt.Sprint(ref["name"])] {
					ref["name"] = fmt.Sprint(ref["name"]) + suffix
				}
			}
		}
	}
	return objs, errors.Join(errs...)
}

// setPath imposta un valore in mappe annidate, creandole se mancano.
func setPath(m map[string]any, value any, keys ...string) {
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = value
}

// PrepareSlots implementa plugin.SlotDeployer: legge da ogni cluster lo slot in esercizio.
func (p *Plugin) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	state := map[string]string{}
	stables, cands := map[string]bool{}, map[string]bool{}
	has := false
	for _, t := range req.Targets {
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		rel, err := readPointer(ctx, api, namespace(req), historyNames(req))
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		stable := ""
		if rel != nil && rel.slot != "" {
			stable = rel.slot
			state[stateKey(stateStableVersion, t)] = rel.version
			has = true
		}
		cand := otherSlot(stable)
		state[stateKey(stateStable, t)], state[stateKey(stateCandidate, t)] = stable, cand
		stables[stable], cands[cand] = true, true
	}
	if len(stables) == 1 && len(cands) == 1 {
		for s := range stables {
			state[stateStable] = s
		}
		for c := range cands {
			state[stateCandidate] = c
		}
	}
	state[plugin.StateHasStable] = strconv.FormatBool(has)
	return state, nil
}

func otherSlot(s string) string {
	if s == "a" {
		return "b"
	}
	return "a"
}

// slots restituisce slot stabile e candidato di un cluster dallo stato registrato.
func slots(req *plugin.Request, t plugin.Target) (string, string, error) {
	if req.Traffic == nil {
		return "", "", errors.New("rilascio per percentuale senza stato degli slot")
	}
	cand := req.Traffic.State[stateKey(stateCandidate, t)]
	if cand != "a" && cand != "b" {
		return "", "", fmt.Errorf("cluster %s: slot candidato sconosciuto (stato del rilascio incompleto)", t.Host)
	}
	return req.Traffic.State[stateKey(stateStable, t)], cand, nil
}

// DeployCandidate implementa plugin.SlotDeployer: applica gli oggetti dello slot candidato e
// ne attende il rollout, senza toccare la route né la release in esercizio.
func (p *Plugin) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployCandidate(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployCandidate(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	_, cand, err := slots(req, t)
	if err != nil {
		return nil, err
	}
	log := p.logger.With("deployment", req.DeploymentID, "cluster", t.Host, "slot", cand)
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return nil, err
	}
	pr, err := p.prepare(ctx, api, req)
	if err != nil {
		return nil, err
	}
	if pr.objects, err = slotObjects(pr.objects, cand, router(req) != RouterIstio); err != nil {
		return nil, err
	}
	pr.refs = pr.refs[:0]
	for _, o := range pr.objects {
		pr.refs = append(pr.refs, refOf(o))
	}
	ns, n := namespace(req), historyNames(req)
	details := map[string]string{"namespace": ns, "slot": cand, "objects": strconv.Itoa(len(pr.objects)), DetailRelease: n.release(req.Artifact.Version)}
	for _, o := range pr.objects {
		if o.Kind() == "Namespace" {
			if _, err := api.Apply(ctx, o, false); err != nil {
				return details, err
			}
		}
	}
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
	log.InfoContext(ctx, "versione candidata applicata nello slot, senza traffico", "objects", len(pr.objects), "version", req.Artifact.Version)
	if err := p.waitRollout(ctx, api, pr.objects, req, log); err != nil {
		return details, err
	}
	return details, nil
}

// SetCandidateWeight implementa plugin.TrafficRouter.
func (p *Plugin) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	for _, t := range req.Targets {
		stable, cand, err := slots(req, t)
		if err != nil {
			return err
		}
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if err := p.setWeights(ctx, api, req, stable, cand, percent); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		p.logger.InfoContext(ctx, "pesi della route aggiornati", "deployment", req.DeploymentID, "cluster", t.Host, "candidate", cand, "percent", percent)
	}
	return nil
}

// CandidateWeight implementa plugin.TrafficRouter: -1 se i cluster non sono concordi.
func (p *Plugin) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	weight := -2
	for _, t := range req.Targets {
		_, cand, err := slots(req, t)
		if err != nil {
			return 0, err
		}
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return 0, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		route, err := p.route(ctx, api, req)
		if err != nil {
			return 0, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		w, err := p.editor(req).weight(route, req, cand)
		if err != nil {
			return 0, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if weight != -2 && w != weight {
			return -1, nil
		}
		weight = w
	}
	return max(weight, 0), nil
}

// RouteTesters implementa plugin.TesterRouter: la regola della route per l'header dei tester
// porta alla candidata.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if req.Traffic == nil || req.Traffic.TesterHeader == "" {
		return nil, errors.New("rollout.traffic.testers.header obbligatorio con il plugin kubernetes")
	}
	for _, t := range req.Targets {
		_, cand, err := slots(req, t)
		if err != nil {
			return nil, err
		}
		if err := p.routeTesters(ctx, t, req, cand); err != nil {
			return nil, fmt.Errorf("cluster %s: %w", t.Host, err)
		}
	}
	return map[string]string{"tester_header": req.Traffic.TesterHeader}, nil
}

func (p *Plugin) routeTesters(ctx context.Context, t plugin.Target, req *plugin.Request, slot string) error {
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return err
	}
	route, err := p.route(ctx, api, req)
	if err != nil {
		return err
	}
	patch, err := p.editor(req).testers(route, req, slot)
	if err != nil {
		return err
	}
	_, err = api.Patch(ctx, route.APIVersion(), route.Kind(), route.Namespace(), route.Name(), patch)
	return err
}

// PromoteCandidate implementa plugin.SlotDeployer: tutto il traffico alla candidata e
// storico aggiornato, con la release sostituita per un eventuale ritorno.
func (p *Plugin) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	for _, t := range req.Targets {
		stable, cand, err := slots(req, t)
		if err != nil {
			return err
		}
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if err := p.setWeights(ctx, api, req, stable, cand, 100); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		ns, n := namespace(req), historyNames(req)
		prev, err := readPointer(ctx, api, ns, n)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if prev != nil && prev.version == req.Artifact.Version && prev.slot == cand {
			continue
		}
		data := map[string]any{dataSlot: cand}
		if stable != "" {
			data[dataPreviousVersion], data[dataPreviousSlot] = req.Traffic.State[stateKey(stateStableVersion, t)], stable
		}
		if err := writePointerData(ctx, api, ns, n, req, req.Artifact.Version, data); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		p.pruneHistory(ctx, api, ns, n, req, prev, p.logger)
		p.logger.InfoContext(ctx, "versione candidata promossa", "deployment", req.DeploymentID, "cluster", t.Host, "slot", cand, "version", req.Artifact.Version)
	}
	return nil
}

// RevertCandidate implementa plugin.SlotDeployer: traffico e storico tornano alla versione
// stabile e la candidata viene ridimensionata a zero. Senza una versione stabile (primo
// rilascio) non c'è nulla a cui tornare e la candidata resta com'è.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	var errs []error
	for _, t := range req.Targets {
		stable, cand, err := slots(req, t)
		if err != nil {
			return err
		}
		if stable == "" {
			p.logger.WarnContext(ctx, "nessuna versione stabile a cui tornare", "deployment", req.DeploymentID, "cluster", t.Host)
			continue
		}
		if err := p.revertTarget(ctx, t, req, stable, cand); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", t.Host, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Plugin) revertTarget(ctx context.Context, t plugin.Target, req *plugin.Request, stable, cand string) error {
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return err
	}
	if err := p.setWeights(ctx, api, req, stable, cand, 0); err != nil {
		return err
	}
	if req.Traffic.TesterHeader != "" {
		if err := p.routeTesters(ctx, t, req, stable); err != nil {
			return err
		}
	}
	ns, n := namespace(req), historyNames(req)
	cur, err := readPointer(ctx, api, ns, n)
	if err != nil {
		return err
	}
	if cur != nil && cur.version == req.Artifact.Version && cur.slot == cand {
		// Già promossa: lo storico torna alla release stabile.
		if err := writePointerData(ctx, api, ns, n, req, req.Traffic.State[stateKey(stateStableVersion, t)], map[string]any{dataSlot: stable}); err != nil {
			return err
		}
	}
	if err := p.scaleRelease(ctx, api, req, req.Artifact.Version, cand); err != nil {
		return err
	}
	p.logger.WarnContext(ctx, "traffico tornato alla versione stabile, candidata ridimensionata", "deployment", req.DeploymentID, "cluster", t.Host, "slot", stable)
	return nil
}

// RetirePrevious implementa plugin.SlotDeployer: ridimensiona a zero lo slot sostituito, solo se
// la release in esercizio è ancora quella promossa da questo rilascio.
func (p *Plugin) RetirePrevious(ctx context.Context, req *plugin.Request) error {
	for _, t := range req.Targets {
		stable, cand, err := slots(req, t)
		if err != nil {
			return err
		}
		if stable == "" {
			continue
		}
		api, err := p.connect(ctx, t, req)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		cur, err := readPointer(ctx, api, namespace(req), historyNames(req))
		if err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		if cur == nil || cur.slot != cand || cur.version != req.Artifact.Version {
			p.logger.InfoContext(ctx, "slot sostituito non ridimensionato: la release in esercizio è cambiata", "deployment", req.DeploymentID, "cluster", t.Host)
			continue
		}
		if err := p.scaleRelease(ctx, api, req, req.Traffic.State[stateKey(stateStableVersion, t)], stable); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		p.logger.InfoContext(ctx, "slot sostituito ridimensionato", "deployment", req.DeploymentID, "cluster", t.Host, "slot", stable)
	}
	return nil
}

// scaleRelease porta a zero le repliche dei workload della release nello slot indicato ed
// elimina i suoi HPA, che altrimenti le riporterebbero su. Gli oggetti restano: il prossimo
// rilascio nello slot li riapplica.
func (p *Plugin) scaleRelease(ctx context.Context, api API, req *plugin.Request, version, slot string) error {
	if version == "" {
		return nil
	}
	rel, err := readRelease(ctx, api, namespace(req), historyNames(req), version)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range rel.objects {
		if !strings.HasSuffix(r.Name, "-"+slot) {
			continue
		}
		switch r.Kind {
		case "Deployment", "StatefulSet":
			if _, err := api.Patch(ctx, r.APIVersion, r.Kind, r.Namespace, r.Name, map[string]any{"spec": map[string]any{"replicas": 0}}); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		case "HorizontalPodAutoscaler":
			if err := api.Delete(ctx, r.APIVersion, r.Kind, r.Namespace, r.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Plugin) route(ctx context.Context, api API, req *plugin.Request) (Object, error) {
	ed := p.editor(req)
	o, err := api.Get(ctx, ed.apiVersion(), ed.kind(req), routeNamespace(req), req.Params[ParamTrafficRoute])
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%s %s/%s assente: va creata con l'infrastruttura, manent ne cambia solo i pesi", ed.kind(req), routeNamespace(req), req.Params[ParamTrafficRoute])
	}
	return o, err
}

// setWeights aggiorna i pesi della route: percent alla candidata, il resto alla stabile.
func (p *Plugin) setWeights(ctx context.Context, api API, req *plugin.Request, stable, cand string, percent int) error {
	route, err := p.route(ctx, api, req)
	if err != nil {
		return err
	}
	patch, changed, err := p.editor(req).setWeights(route, req, stable, cand, percent)
	if err != nil || !changed {
		return err
	}
	_, err = api.Patch(ctx, route.APIVersion(), route.Kind(), route.Namespace(), route.Name(), patch)
	return err
}

// routeEditor legge e cambia i pesi di un tipo di route.
type routeEditor interface {
	apiVersion() string
	kind(req *plugin.Request) string
	weight(route Object, req *plugin.Request, cand string) (int, error)
	setWeights(route Object, req *plugin.Request, stable, cand string, percent int) (map[string]any, bool, error)
	testers(route Object, req *plugin.Request, slot string) (map[string]any, error)
}

func (p *Plugin) editor(req *plugin.Request) routeEditor {
	return gatewayEditor{service: req.Params[ParamTrafficService]}
}

// gatewayEditor lavora su una HTTPRoute (o GRPCRoute) della Gateway API: le regole hanno i
// backendRefs <service>-a e <service>-b con i loro pesi; la regola con un match sull'header
// dei tester porta solo alla candidata.
type gatewayEditor struct{ service string }

func (gatewayEditor) apiVersion() string { return gatewayAPIVersion }

func (gatewayEditor) kind(req *plugin.Request) string {
	if k := req.Params[ParamTrafficRouteKind]; k != "" {
		return k
	}
	return "HTTPRoute"
}

func rulesOf(route Object, key string) []any {
	spec, _ := route["spec"].(map[string]any)
	rules, _ := spec[key].([]any)
	return rules
}

// testerRule indica una regola con un match sull'header dei tester.
func testerRule(rule map[string]any, header string) bool {
	if header == "" {
		return false
	}
	name, _, _ := strings.Cut(header, ":")
	name = strings.TrimSpace(name)
	matches, _ := rule["matches"].([]any)
	for _, m := range matches {
		mm, _ := m.(map[string]any)
		hs, _ := mm["headers"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if n, _ := hm["name"].(string); strings.EqualFold(n, name) {
				return true
			}
		}
	}
	return false
}

func num64(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func (g gatewayEditor) weight(route Object, req *plugin.Request, cand string) (int, error) {
	w := -2
	for _, r := range rulesOf(route, "rules") {
		rule, _ := r.(map[string]any)
		if testerRule(rule, testerHeader(req)) {
			continue
		}
		refs, _ := rule["backendRefs"].([]any)
		var c, total int
		found := false
		for _, ref := range refs {
			m, _ := ref.(map[string]any)
			name, _ := m["name"].(string)
			if name != g.service+"-a" && name != g.service+"-b" {
				continue
			}
			wt := 1
			if v, ok := m["weight"]; ok {
				wt = num64(v)
			}
			total += wt
			if name == g.service+"-"+cand {
				c += wt
			}
			found = true
		}
		if !found {
			continue
		}
		pct := 0
		if total > 0 {
			pct = int(math.Round(float64(c) * 100 / float64(total)))
		}
		if w != -2 && pct != w {
			return -1, nil
		}
		w = pct
	}
	if w == -2 {
		return 0, fmt.Errorf("la route non ha backendRefs %s-a e %s-b", g.service, g.service)
	}
	return w, nil
}

func testerHeader(req *plugin.Request) string {
	if req.Traffic == nil {
		return ""
	}
	return req.Traffic.TesterHeader
}

func (g gatewayEditor) setWeights(route Object, req *plugin.Request, stable, cand string, percent int) (map[string]any, bool, error) {
	rules := rulesOf(route, "rules")
	header := testerHeader(req)
	found, changed := false, false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if testerRule(rule, header) {
			continue
		}
		refs, _ := rule["backendRefs"].([]any)
		for _, ref := range refs {
			m, _ := ref.(map[string]any)
			name, _ := m["name"].(string)
			var want int
			switch name {
			case g.service + "-" + cand:
				want = percent
			case g.service + "-" + stable:
				want = 100 - percent
			case g.service + "-" + otherSlot(cand):
				// Senza versione stabile lo slot libero non riceve traffico.
				want = 0
			default:
				continue
			}
			found = true
			if cur, ok := m["weight"]; !ok || num64(cur) != want {
				m["weight"] = want
				changed = true
			}
		}
	}
	if !found {
		return nil, false, fmt.Errorf("la route %s non ha backendRefs %s-a e %s-b", route.Name(), g.service, g.service)
	}
	return routePatch(route, "rules", rules), changed, nil
}

func (g gatewayEditor) testers(route Object, req *plugin.Request, slot string) (map[string]any, error) {
	rules := rulesOf(route, "rules")
	found := false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if !testerRule(rule, req.Traffic.TesterHeader) {
			continue
		}
		found = true
		refs, _ := rule["backendRefs"].([]any)
		ref := map[string]any{}
		if len(refs) > 0 {
			if m, ok := refs[0].(map[string]any); ok {
				for k, v := range m {
					ref[k] = v
				}
			}
		}
		ref["name"] = g.service + "-" + slot
		delete(ref, "weight")
		rule["backendRefs"] = []any{ref}
	}
	if !found {
		return nil, fmt.Errorf("la route %s non ha una regola con match sull'header dei tester (%s)", route.Name(), req.Traffic.TesterHeader)
	}
	return routePatch(route, "rules", rules), nil
}

// routePatch sostituisce la lista delle regole, con il resourceVersion letto: se la route è
// cambiata nel frattempo il server rifiuta la patch e lo step viene ritentato.
func routePatch(route Object, key string, rules []any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"resourceVersion": route.str("metadata", "resourceVersion")},
		"spec":     map[string]any{key: rules},
	}
}
