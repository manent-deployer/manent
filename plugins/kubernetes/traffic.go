package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Parameters of percentage rollouts (rollout with strategy: traffic).
const (
	ParamTrafficRouter         = "traffic_router"          // gateway-api (default) or istio
	ParamTrafficRoute          = "traffic_route"           // HTTPRoute (or GRPCRoute) or VirtualService that splits the traffic
	ParamTrafficRouteKind      = "traffic_route_kind"      // gateway-api: HTTPRoute (default) or GRPCRoute
	ParamTrafficRouteNamespace = "traffic_route_namespace" // route namespace (default: namespace)
	ParamTrafficService        = "traffic_service"         // gateway-api: base name of the slots' Services (<name>-a, <name>-b)
	ParamTrafficRename         = "traffic_rename"          // "false": the chart already names objects per slot (manent.slot), manent only adds labels
)

// Supported routers.
const (
	RouterGatewayAPI = "gateway-api"
	RouterIstio      = "istio"
)

// LabelTrack is the slot label on objects and pods.
const LabelTrack = "manent.io/track"

const (
	gatewayAPIVersion = "gateway.networking.k8s.io/v1"
	istioAPIVersion   = "networking.istio.io/v1"
)

// Keys of the state recorded by PrepareSlots, per cluster ("<key>@<host>").
const (
	stateStable        = "stable"
	stateStableVersion = "stable_version"
	stateCandidate     = "candidate"
)

func stateKey(k string, t plugin.Target) string { return k + "@" + t.Host }

// slotted are the object kinds duplicated per slot, with the name suffix.
var slotted = []string{"Deployment", "StatefulSet", "DaemonSet", "Service", "HorizontalPodAutoscaler", "PodDisruptionBudget"}

func validateTraffic(req *plugin.Request) []error {
	if req.Traffic == nil {
		return nil
	}
	var errs []error
	switch r := router(req); r {
	case RouterGatewayAPI:
		if !dnsLabelRe.MatchString(req.Params[ParamTrafficService]) {
			errs = append(errs, fmt.Errorf("%s is required with %s %s (base name of the Services <name>-a and <name>-b)", ParamTrafficService, ParamTrafficRouter, r))
		}
		if k := req.Params[ParamTrafficRouteKind]; k != "" && k != "HTTPRoute" && k != "GRPCRoute" {
			errs = append(errs, fmt.Errorf("invalid %s %q (HTTPRoute|GRPCRoute)", ParamTrafficRouteKind, k))
		}
	case RouterIstio:
	default:
		errs = append(errs, fmt.Errorf("invalid %s %q (%s|%s)", ParamTrafficRouter, r, RouterGatewayAPI, RouterIstio))
	}
	if !dnsLabelRe.MatchString(req.Params[ParamTrafficRoute]) {
		errs = append(errs, fmt.Errorf("%s is required: the route that splits traffic between the slots", ParamTrafficRoute))
	}
	if ns := req.Params[ParamTrafficRouteNamespace]; ns != "" && !dnsLabelRe.MatchString(ns) {
		errs = append(errs, fmt.Errorf("invalid %s %q", ParamTrafficRouteNamespace, ns))
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

// slotObjects duplicates the objects for the slot: -<slot> suffix to the names of workloads,
// Services, HPAs and PDBs, manent.io/track label on objects, selectors and pods. With Istio
// the Services stay shared by both slots (the router picks the subset from the pod label).
func slotObjects(objs []Object, slot string, services, rename bool) ([]Object, error) {
	renamed := map[string]bool{}
	for _, o := range objs {
		if rename && slices.Contains(slotted, o.Kind()) && (services || o.Kind() != "Service") {
			renamed[o.Kind()+"/"+o.Name()] = true
		}
	}
	suffix := "-" + slot
	var errs []error
	for _, o := range objs {
		o.setMap("labels", LabelTrack, slot)
		if renamed[o.Kind()+"/"+o.Name()] {
			name := o.Name() + suffix
			if len(name) > 63 {
				errs = append(errs, fmt.Errorf("%s: name %q too long for the slot (maximum 63 characters)", o.Ref(), name))
			}
			o.metadata()["name"] = name
		} else if !slices.Contains(slotted, o.Kind()) {
			continue
		}
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
			// With Istio the Service is shared by both slots: the selector does not change.
			if sel, ok := spec["selector"].(map[string]any); ok && len(sel) > 0 && services {
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

// setPath sets a value in nested maps, creating them if missing.
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

// PrepareSlots implements plugin.SlotDeployer: it reads from every cluster the slot in service.
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

// slots returns the stable and candidate slots of a cluster from the recorded state.
func slots(req *plugin.Request, t plugin.Target) (string, string, error) {
	if req.Traffic == nil {
		return "", "", errors.New("percentage release without slot state")
	}
	cand := req.Traffic.State[stateKey(stateCandidate, t)]
	if cand != "a" && cand != "b" {
		return "", "", fmt.Errorf("cluster %s: unknown candidate slot (incomplete release state)", t.Host)
	}
	return req.Traffic.State[stateKey(stateStable, t)], cand, nil
}

// DeployCandidate implements plugin.SlotDeployer: it applies the objects of the candidate
// slot and waits for their rollout, without touching the route or the release in service.
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
	// A Helm chart receives the slot in .Values.manent.slot, to use it in names if needed.
	sreq := *req
	sreq.Params = maps.Clone(req.Params)
	sreq.Params[ParamHelmValuePrefix+"manent.slot"] = cand
	pr, err := p.prepare(ctx, api, &sreq)
	if err != nil {
		return nil, err
	}
	if pr.objects, err = slotObjects(pr.objects, cand, router(req) != RouterIstio, req.Params[ParamTrafficRename] != "false"); err != nil {
		return nil, err
	}
	stable, _, _ := slots(req, t)
	if req.Traffic.ProportionalReplicas && stable != "" {
		startSmall(pr.objects)
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
	log.InfoContext(ctx, "candidate version applied in the slot, without traffic", "objects", len(pr.objects), "version", req.Artifact.Version)
	if err := p.waitRollout(ctx, api, pr.objects, req, log); err != nil {
		return details, err
	}
	return details, nil
}

// SetCandidateWeight implements plugin.TrafficRouter.
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
		p.logger.InfoContext(ctx, "route weights updated", "deployment", req.DeploymentID, "cluster", t.Host, "candidate", cand, "percent", percent)
	}
	return nil
}

// CandidateWeight implements plugin.TrafficRouter: -1 if the clusters disagree.
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

// RouteTesters implements plugin.TesterRouter: the route rule for the testers' header leads
// to the candidate.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if req.Traffic == nil || req.Traffic.TesterHeader == "" {
		return nil, errors.New("rollout.traffic.testers.header is required with the kubernetes plugin")
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

// PromoteCandidate implements plugin.SlotDeployer: all traffic to the candidate and history
// updated, with the replaced release for a possible return.
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
		p.logger.InfoContext(ctx, "candidate version promoted", "deployment", req.DeploymentID, "cluster", t.Host, "slot", cand, "version", req.Artifact.Version)
	}
	return nil
}

// RevertCandidate implements plugin.SlotDeployer: traffic and history go back to the stable
// version and the candidate is scaled to zero. Without a stable version (first release) there
// is nothing to go back to and the candidate stays as is.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	var errs []error
	for _, t := range req.Targets {
		stable, cand, err := slots(req, t)
		if err != nil {
			return err
		}
		if stable == "" {
			p.logger.WarnContext(ctx, "no stable version to go back to", "deployment", req.DeploymentID, "cluster", t.Host)
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
		// Already promoted: the history goes back to the stable release.
		if err := writePointerData(ctx, api, ns, n, req, req.Traffic.State[stateKey(stateStableVersion, t)], map[string]any{dataSlot: stable}); err != nil {
			return err
		}
	}
	if err := p.scaleRelease(ctx, api, req, req.Artifact.Version, cand); err != nil {
		return err
	}
	p.logger.WarnContext(ctx, "traffic back on the stable version, candidate scaled down", "deployment", req.DeploymentID, "cluster", t.Host, "slot", stable)
	return nil
}

// RetirePrevious implements plugin.SlotDeployer: it scales the replaced slot to zero, only if
// the release in service is still the one promoted by this rollout.
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
			p.logger.InfoContext(ctx, "replaced slot not scaled down: the release in service changed", "deployment", req.DeploymentID, "cluster", t.Host)
			continue
		}
		if err := p.scaleRelease(ctx, api, req, req.Traffic.State[stateKey(stateStableVersion, t)], stable); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
		p.logger.InfoContext(ctx, "replaced slot scaled down", "deployment", req.DeploymentID, "cluster", t.Host, "slot", stable)
	}
	return nil
}

// scaleRelease brings to zero the replicas of the release's workloads in the given slot and
// deletes its HPAs, which would otherwise scale them back up. Objects stay: the next release
// in the slot applies them again.
func (p *Plugin) scaleRelease(ctx context.Context, api API, req *plugin.Request, version, slot string) error {
	if version == "" || slot == "" {
		return nil
	}
	rel, err := readRelease(ctx, api, namespace(req), historyNames(req), version)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// The release was applied in the slot: all its workloads belong to the slot.
	for _, r := range rel.objects {
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

// AnnotationReplicas keeps the manifest replicas of a workload started small.
const AnnotationReplicas = "manent.io/replicas"

// startSmall starts with one replica the candidate's workloads not managed by an HPA,
// annotating the manifest replicas: ScaleCandidate reaches them in proportion to traffic.
func startSmall(objs []Object) {
	scaled := map[string]bool{}
	for _, o := range objs {
		if o.Kind() == "HorizontalPodAutoscaler" {
			scaled[o.str("spec", "scaleTargetRef", "kind")+"/"+o.str("spec", "scaleTargetRef", "name")] = true
		}
	}
	for _, o := range objs {
		if (o.Kind() != "Deployment" && o.Kind() != "StatefulSet") || scaled[o.Kind()+"/"+o.Name()] {
			continue
		}
		want := 1
		if spec, ok := o["spec"].(map[string]any); ok {
			if _, set := spec["replicas"]; set {
				want = num(o, "spec", "replicas")
			}
			spec["replicas"] = 1
		}
		o.setMap("annotations", AnnotationReplicas, strconv.Itoa(want))
	}
}

// ScaleCandidate implements plugin.SlotScaler: it brings the candidate's workloads started
// small to ceil(replicas × percent / 100) and waits for their availability.
func (p *Plugin) ScaleCandidate(ctx context.Context, req *plugin.Request, percent int) error {
	for _, t := range req.Targets {
		if err := p.scaleCandidate(ctx, t, req, percent); err != nil {
			return fmt.Errorf("cluster %s: %w", t.Host, err)
		}
	}
	return nil
}

func (p *Plugin) scaleCandidate(ctx context.Context, t plugin.Target, req *plugin.Request, percent int) error {
	api, err := p.connect(ctx, t, req)
	if err != nil {
		return err
	}
	objs, err := loadObjects(ctx, api, namespace(req), historyNames(req), req.Artifact.Version)
	if err != nil {
		return err
	}
	var scaled []Object
	for _, o := range objs {
		raw := o.str("metadata", "annotations", AnnotationReplicas)
		if raw == "" {
			continue
		}
		full, _ := strconv.Atoi(raw)
		n := max(1, int(math.Ceil(float64(full)*float64(percent)/100)))
		if _, err := api.Patch(ctx, o.APIVersion(), o.Kind(), o.Namespace(), o.Name(), map[string]any{"spec": map[string]any{"replicas": n}}); err != nil {
			return err
		}
		scaled = append(scaled, o)
		p.logger.InfoContext(ctx, "candidate scaled", "deployment", req.DeploymentID, "cluster", t.Host, "object", o.Ref(), "replicas", n, "percent", percent)
	}
	return p.waitRollout(ctx, api, scaled, req, p.logger.With("deployment", req.DeploymentID, "cluster", t.Host))
}

func (p *Plugin) route(ctx context.Context, api API, req *plugin.Request) (Object, error) {
	ed := p.editor(req)
	o, err := api.Get(ctx, ed.apiVersion(), ed.kind(req), routeNamespace(req), req.Params[ParamTrafficRoute])
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%s %s/%s missing: it must be created with the infrastructure, manent changes only its weights", ed.kind(req), routeNamespace(req), req.Params[ParamTrafficRoute])
	}
	return o, err
}

// setWeights updates the route weights: percent to the candidate, the rest to the stable.
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

// routeEditor reads and changes the weights of a route kind.
type routeEditor interface {
	apiVersion() string
	kind(req *plugin.Request) string
	weight(route Object, req *plugin.Request, cand string) (int, error)
	setWeights(route Object, req *plugin.Request, stable, cand string, percent int) (map[string]any, bool, error)
	testers(route Object, req *plugin.Request, slot string) (map[string]any, error)
}

func (p *Plugin) editor(req *plugin.Request) routeEditor {
	if router(req) == RouterIstio {
		return istioEditor{}
	}
	return gatewayEditor{service: req.Params[ParamTrafficService]}
}

// istioEditor works on an Istio VirtualService: the routes to subsets a and b (defined by a
// DestinationRule on the manent.io/track label) have their weights; the http entry with a
// match on the testers' header leads only to the candidate. Services are shared by both slots.
type istioEditor struct{}

func (istioEditor) apiVersion() string { return istioAPIVersion }

func (istioEditor) kind(*plugin.Request) string { return "VirtualService" }

// istioTester reports an http entry with a match on the testers' header.
func istioTester(entry map[string]any, header string) bool {
	if header == "" {
		return false
	}
	name, _, _ := strings.Cut(header, ":")
	name = strings.TrimSpace(name)
	matches, _ := entry["match"].([]any)
	for _, m := range matches {
		mm, _ := m.(map[string]any)
		hs, _ := mm["headers"].(map[string]any)
		for k := range hs {
			if strings.EqualFold(k, name) {
				return true
			}
		}
	}
	return false
}

func subsetOf(route any) (map[string]any, string) {
	m, _ := route.(map[string]any)
	dest, _ := m["destination"].(map[string]any)
	s, _ := dest["subset"].(string)
	return m, s
}

func (istioEditor) weight(vs Object, req *plugin.Request, cand string) (int, error) {
	w := -2
	for _, e := range rulesOf(vs, "http") {
		entry, _ := e.(map[string]any)
		if istioTester(entry, testerHeader(req)) {
			continue
		}
		routes, _ := entry["route"].([]any)
		var c, total int
		found := false
		for _, r := range routes {
			m, subset := subsetOf(r)
			if subset != "a" && subset != "b" {
				continue
			}
			wt := 100
			if v, ok := m["weight"]; ok {
				wt = num64(v)
			} else if len(routes) > 1 {
				wt = 0
			}
			total += wt
			if subset == cand {
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
		return 0, errors.New("the VirtualService has no routes to subsets a and b")
	}
	return w, nil
}

func (istioEditor) setWeights(vs Object, req *plugin.Request, stable, cand string, percent int) (map[string]any, bool, error) {
	entries := rulesOf(vs, "http")
	found, changed := false, false
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if istioTester(entry, testerHeader(req)) {
			continue
		}
		routes, _ := entry["route"].([]any)
		for _, r := range routes {
			m, subset := subsetOf(r)
			var want int
			switch subset {
			case cand:
				want = percent
			case stable:
				want = 100 - percent
			case otherSlot(cand):
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
		return nil, false, fmt.Errorf("VirtualService %s has no routes to subsets a and b", vs.Name())
	}
	return routePatch(vs, "http", entries), changed, nil
}

func (istioEditor) testers(vs Object, req *plugin.Request, slot string) (map[string]any, error) {
	entries := rulesOf(vs, "http")
	found := false
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if !istioTester(entry, req.Traffic.TesterHeader) {
			continue
		}
		found = true
		routes, _ := entry["route"].([]any)
		route := map[string]any{}
		if len(routes) > 0 {
			if m, ok := routes[0].(map[string]any); ok {
				for k, v := range m {
					route[k] = v
				}
			}
		}
		dest := map[string]any{}
		if d, ok := route["destination"].(map[string]any); ok {
			for k, v := range d {
				dest[k] = v
			}
		}
		dest["subset"] = slot
		route["destination"] = dest
		delete(route, "weight")
		entry["route"] = []any{route}
	}
	if !found {
		return nil, fmt.Errorf("VirtualService %s has no http entry matching the testers header (%s)", vs.Name(), req.Traffic.TesterHeader)
	}
	return routePatch(vs, "http", entries), nil
}

// gatewayEditor works on a Gateway API HTTPRoute (or GRPCRoute): the rules have the
// <service>-a and <service>-b backendRefs with their weights; the rule with a match on the
// testers' header leads only to the candidate.
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

// testerRule reports a rule with a match on the testers' header.
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
		return 0, fmt.Errorf("the route has no backendRefs %s-a and %s-b", g.service, g.service)
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
				// Without a stable version the free slot receives no traffic.
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
		return nil, false, fmt.Errorf("route %s has no backendRefs %s-a and %s-b", route.Name(), g.service, g.service)
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
		return nil, fmt.Errorf("route %s has no rule matching the testers header (%s)", route.Name(), req.Traffic.TesterHeader)
	}
	return routePatch(route, "rules", rules), nil
}

// routePatch replaces the rule list, with the resourceVersion read: if the route changed
// meanwhile the server rejects the patch and the step is retried.
func routePatch(route Object, key string, rules []any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"resourceVersion": route.str("metadata", "resourceVersion")},
		"spec":     map[string]any{key: rules},
	}
}
