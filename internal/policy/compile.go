package policy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"
	"github.com/open-policy-agent/opa/v1/tester"

	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/service"
)

// DefaultQuery is the query of the decision.
const DefaultQuery = "data.manent.governance.decision"

// DefaultEvalTimeout limits a single evaluation.
const DefaultEvalTimeout = 200 * time.Millisecond

// CompileOptions configures the compilation of a revision.
type CompileOptions struct {
	// Query is the decision query (default DefaultQuery).
	Query string
	// Rules are the governance.rules of manent.yml: they become data.manent.rules when the
	// revision does not define it.
	Rules []governance.Rule
	// EvalTimeout limits every evaluation (default DefaultEvalTimeout).
	EvalTimeout time.Duration
	// RequireTests rejects a revision without tests (*_test.rego).
	RequireTests bool
	// Authz, if set, compiles also the permission policy (rbac.engine: opa).
	Authz *AuthzOptions
}

// Compiled is a compiled revision, ready to evaluate. It is immutable and safe for concurrent use.
type Compiled struct {
	// Revision is the label of the revision (e.g. "13:3f2a9c1e5b7d"), SHA256 its hash.
	Revision string
	SHA256   string
	// Windows are the freeze windows of the revision (data.manent.freezes).
	Windows []governance.Window
	query   rego.PreparedEvalQuery
	timeout time.Duration
	// authz holds the permission queries (nil without rbac.engine: opa).
	authz *authzQueries
}

// Compile parses, compiles and checks a revision: forbidden built-ins and reserved packages
// are errors; the revision's tests must pass and the decision must be defined and valid on a
// set of sample inputs. label names the revision ("" = its hash). It returns the test results
// even on error, when the tests ran.
func Compile(ctx context.Context, files Files, label string, o CompileOptions) (*Compiled, []service.PolicyTest, error) {
	if err := files.Check(); err != nil {
		return nil, nil, err
	}
	sha := files.Hash()
	if label == "" {
		label = sha[:12]
	}
	query := cmp.Or(o.Query, DefaultQuery)
	if _, err := ast.ParseBody(query); err != nil {
		return nil, nil, fmt.Errorf("invalid query %q: %w", query, err)
	}
	data, err := files.Data()
	if err != nil {
		return nil, nil, err
	}
	if _, ok := lookup(data, "manent", "rules"); !ok {
		setPath(data, RulesData(o.Rules), "manent", "rules")
	}
	if o.Authz != nil {
		if _, ok := lookup(data, "manent", "rbac"); !ok {
			setPath(data, RBACData(o.Authz.Roles, o.Authz.Bindings), "manent", "rbac")
		}
	}
	windows, err := revisionWindows(data)
	if err != nil {
		return nil, nil, err
	}
	caps := capabilities()
	parse := func(includeTests bool) (map[string]*ast.Module, bool, error) {
		mods := map[string]*ast.Module{}
		governs, authorizes := false, false
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if !strings.HasSuffix(p, ".rego") || (isTest(p) && !includeTests) {
				continue
			}
			m, err := ast.ParseModuleWithOpts(p, files[p], ast.ParserOptions{RegoVersion: ast.RegoV1, Capabilities: caps})
			if err != nil {
				return nil, false, err
			}
			pkg := m.Package.Path.String()
			if pkg == reservedPackage || strings.HasPrefix(pkg, reservedPackage+".") {
				return nil, false, fmt.Errorf("%s: package %s is reserved to the manent library", p, strings.TrimPrefix(pkg, "data."))
			}
			if pkg == "data.manent.governance" && !isTest(p) {
				governs = true
			}
			if pkg == "data.manent.authz" && !isTest(p) {
				authorizes = true
			}
			mods[p] = m
		}
		lib := slices.Clone(libModules)
		if !governs {
			lib = append(lib, defaultModule)
		}
		if o.Authz != nil {
			lib = append(lib, authzLibModule)
			if !authorizes {
				lib = append(lib, authzDefaultModule)
			}
		}
		for _, p := range lib {
			src, err := libFS.ReadFile(p)
			if err != nil {
				return nil, false, err
			}
			m, err := ast.ParseModuleWithOpts("manent/"+p, string(src), ast.ParserOptions{RegoVersion: ast.RegoV1, Capabilities: caps})
			if err != nil {
				return nil, false, fmt.Errorf("manent library %s: %w", p, err)
			}
			mods["manent/"+p] = m
		}
		return mods, governs, nil
	}
	mods, _, err := parse(false)
	if err != nil {
		return nil, nil, err
	}
	base := []func(*rego.Rego){
		rego.Store(inmem.NewFromObject(data)), rego.Capabilities(caps),
		rego.SetRegoVersion(ast.RegoV1), rego.Strict(true), rego.StrictBuiltinErrors(true),
	}
	for _, p := range slices.Sorted(maps.Keys(mods)) {
		base = append(base, rego.ParsedModule(mods[p]))
	}
	for _, b := range builtins {
		base = append(base, b.opt)
	}
	pq, err := rego.New(append(slices.Clone(base), rego.Query(query))...).PrepareForEval(ctx)
	if err != nil {
		return nil, nil, err
	}
	c := &Compiled{Revision: label, SHA256: sha, Windows: windows, query: pq, timeout: cmp.Or(o.EvalTimeout, DefaultEvalTimeout)}
	if o.Authz != nil {
		roles, err := revisionRoles(data)
		if err != nil {
			return nil, nil, err
		}
		if c.authz, err = prepareAuthz(ctx, base, roles); err != nil {
			return nil, nil, err
		}
	}
	tests, err := runTests(ctx, files, data, parse, o.RequireTests)
	if err != nil {
		return nil, tests, err
	}
	for _, in := range sampleInputs() {
		dec, err := c.Evaluate(ctx, in)
		if err == nil {
			err = dec.Check()
		}
		if err != nil {
			return nil, tests, fmt.Errorf("sample evaluation (phase %s, kind %s): %w", in.Phase, in.Deployment.Kind, err)
		}
	}
	if c.authz != nil {
		if err := c.checkAuthzSamples(ctx); err != nil {
			return nil, tests, err
		}
	}
	return c, tests, nil
}

// runTests runs the revision's tests (*_test.rego) with its modules and data.
func runTests(ctx context.Context, files Files, data map[string]any,
	parse func(bool) (map[string]*ast.Module, bool, error), required bool) ([]service.PolicyTest, error) {
	hasTests := false
	for p := range files {
		hasTests = hasTests || isTest(p)
	}
	if !hasTests {
		if required {
			return nil, errors.New("the revision has no tests (*_test.rego): governance.opa.store.tests is required")
		}
		return nil, nil
	}
	mods, _, err := parse(true)
	if err != nil {
		return nil, err
	}
	compiler := ast.NewCompiler().WithCapabilities(capabilities()).WithDefaultRegoVersion(ast.RegoV1)
	ch, err := tester.NewRunner().
		SetCompiler(compiler).
		SetStore(inmem.NewFromObject(data)).
		AddCustomBuiltins(testBuiltins()).
		SetDefaultRegoVersion(ast.RegoV1).
		RaiseBuiltinErrors(true).
		SetModules(mods).
		RunTests(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []service.PolicyTest
	failed := 0
	for r := range ch {
		t := service.PolicyTest{Name: strings.TrimPrefix(r.Package, "data.") + "." + r.Name, Pass: r.Pass()}
		if r.Error != nil {
			t.Error = r.Error.Error()
		} else if r.Fail {
			t.Error = "failed"
		}
		if !t.Pass {
			failed++
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b service.PolicyTest) int { return strings.Compare(a.Name, b.Name) })
	if failed > 0 {
		return out, fmt.Errorf("%d of %d policy tests failed", failed, len(out))
	}
	return out, nil
}

// Evaluate evaluates the decision on a prepared input (governance.Decide prepares it).
func (c *Compiled) Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	v, err := ast.InterfaceToValue(in)
	if err != nil {
		return governance.Decision{}, err
	}
	rs, err := c.query.Eval(ctx, rego.EvalParsedInput(v))
	if err != nil {
		return governance.Decision{}, err
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return governance.Decision{}, errors.New("the policy decision is undefined: the query must always produce a value (add a default)")
	}
	dec, err := decodeDecision(rs[0].Expressions[0].Value)
	if err != nil {
		return governance.Decision{}, err
	}
	dec.Revision = c.Revision
	return dec, nil
}

// rawDecision is the decision as the policy writes it: durations as strings, denials as
// objects or strings.
type rawDecision struct {
	Rule         string            `json:"rule"`
	Deny         []json.RawMessage `json:"deny"`
	Requires     []string          `json:"requires"`
	Approvals    *rawApprovals     `json:"approvals"`
	Change       *rawChange        `json:"change"`
	FreezeExempt []string          `json:"freeze_exempt"`
	Notes        []string          `json:"notes"`
}

type rawApprovals struct {
	Required  int                   `json:"required"`
	AllowSelf bool                  `json:"allow_self"`
	Timeout   string                `json:"timeout"`
	When      string                `json:"when"`
	Approvers *governance.Approvers `json:"approvers"`
}

type rawChange struct {
	Approval bool   `json:"approval"`
	Timeout  string `json:"timeout"`
}

// decodeDecision checks the shape of the decision: unknown keys and wrong types are errors.
func decodeDecision(v any) (governance.Decision, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return governance.Decision{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raw rawDecision
	if err := dec.Decode(&raw); err != nil {
		return governance.Decision{}, fmt.Errorf("invalid policy decision: %w", err)
	}
	out := governance.Decision{Rule: raw.Rule, Requires: raw.Requires, FreezeExempt: raw.FreezeExempt, Notes: raw.Notes}
	for _, d := range raw.Deny {
		var den governance.Denial
		var msg string
		if err := json.Unmarshal(d, &msg); err == nil {
			den.Message = msg
		} else {
			dd := json.NewDecoder(bytes.NewReader(d))
			dd.DisallowUnknownFields()
			if err := dd.Decode(&den); err != nil {
				return governance.Decision{}, fmt.Errorf("invalid policy decision: deny: %w", err)
			}
		}
		out.Deny = append(out.Deny, den)
	}
	if a := raw.Approvals; a != nil && a.Required != 0 {
		t, err := parseDuration(a.Timeout, governance.DefaultApprovalTimeout)
		if err != nil {
			return governance.Decision{}, fmt.Errorf("invalid policy decision: approvals.timeout: %w", err)
		}
		out.Approvals = &governance.Approvals{Required: a.Required, AllowSelf: a.AllowSelf, Timeout: t, When: a.When, Approvers: a.Approvers}
	}
	if c := raw.Change; c != nil {
		t, err := parseDuration(c.Timeout, 72*time.Hour)
		if err != nil {
			return governance.Decision{}, fmt.Errorf("invalid policy decision: change.timeout: %w", err)
		}
		out.Change = &governance.Change{Approval: c.Approval, Timeout: t}
	}
	return out, nil
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

// RulesData converts the rules of manent.yml into data.manent.rules for manent.lib.rules.
func RulesData(rules []governance.Rule) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		m := map[string]any{"name": r.Name, "applications": anySlice(r.Applications), "environments": anySlice(r.Environments)}
		if r.Tenants != "" {
			m["tenants"] = r.Tenants
		}
		if len(r.Requires) > 0 {
			m["requires"] = anySlice(r.Requires)
		}
		if a := r.Approvals; a.Required > 0 {
			am := map[string]any{"required": json.Number(fmt.Sprint(a.Required)), "allow_self": a.AllowSelf}
			if a.Timeout > 0 {
				am["timeout"] = a.Timeout.String()
			}
			if a.When != "" {
				am["when"] = a.When
			}
			if ap := a.Approvers; ap != nil {
				am["approvers"] = map[string]any{"groups": anySlice(ap.Groups), "subjects": anySlice(ap.Subjects)}
			}
			m["approvals"] = am
		}
		if c := r.Change; c != nil {
			cm := map[string]any{"approval": c.Approval}
			if c.Timeout > 0 {
				cm["timeout"] = c.Timeout.String()
			}
			m["change"] = cm
		}
		out = append(out, m)
	}
	return out
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// lookup reads a path of the data document.
func lookup(data map[string]any, keys ...string) (any, bool) {
	var cur any = data
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath writes a value at a path of the data document, creating the objects.
func setPath(data map[string]any, v any, keys ...string) {
	cur := data
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = v
}

// rawWindow is a freeze window in data.manent.freezes, with the schema of governance.freezes.
type rawWindow struct {
	Name         string    `json:"name"`
	Reason       string    `json:"reason"`
	Applications []string  `json:"applications"`
	Environments []string  `json:"environments"`
	Regions      []string  `json:"regions"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Cron         string    `json:"cron"`
	Duration     string    `json:"duration"`
	Timezone     string    `json:"timezone"`
	Hard         bool      `json:"hard"`
	Tenants      string    `json:"tenants"`
}

// revisionWindows reads the freeze windows of the revision (data.manent.freezes).
func revisionWindows(data map[string]any) ([]governance.Window, error) {
	v, ok := lookup(data, "manent", "freezes")
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raws []rawWindow
	if err := dec.Decode(&raws); err != nil {
		return nil, fmt.Errorf("data.manent.freezes: %w", err)
	}
	ws := make([]governance.Window, len(raws))
	for i, r := range raws {
		d, err := parseDuration(r.Duration, 0)
		if err != nil {
			return nil, fmt.Errorf("data.manent.freezes[%d].duration: %w", i, err)
		}
		ws[i] = governance.Window{Name: r.Name, Reason: r.Reason, Applications: r.Applications, Environments: r.Environments,
			Regions: r.Regions, From: r.From, To: r.To, Cron: r.Cron, Duration: d, Timezone: r.Timezone, Hard: r.Hard, Tenants: r.Tenants}
	}
	if err := errors.Join(governance.PrepareWindows(ws, "data.manent.freezes")...); err != nil {
		return nil, err
	}
	return ws, nil
}

// sampleInputs are the inputs a revision must decide at load: every phase and kind.
func sampleInputs() []governance.Input {
	now := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	var out []governance.Input
	for _, phase := range []string{governance.PhaseSubmit, governance.PhaseStart, governance.PhaseStage, governance.PhaseTick} {
		for _, kind := range []string{service.KindDeploy, service.KindPromotion, service.KindRollback} {
			out = append(out, governance.Input{
				Schema: governance.InputSchema, Phase: phase, Now: now, Application: "sample", Environment: "prod",
				Deployment:  governance.InputDeployment{Kind: kind},
				Artifact:    governance.InputArtifact{Name: "sample", Version: "1.0.0", Type: "application"},
				RequestedBy: governance.InputIdentity{ID: "oidc:sample", Method: "oidc", Name: "sample"},
				Freezes:     governance.InputFreezes{Active: []governance.WindowStatus{}},
			})
		}
	}
	return out
}
