package policy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/service"
)

// Engines and sources.
const (
	EngineBuiltin = "builtin"
	EngineOPA     = "opa"

	SourceStore  = "store"
	SourceFile   = "file"
	SourceBundle = "bundle"
	SourceConsul = "consul"
)

// Defaults of the refresh.
const (
	DefaultRefresh      = 10 * time.Second
	DefaultMaxStaleness = 10 * time.Minute
)

// Source provides the revisions of a source other than the store (file, bundle, consul).
type Source interface {
	// Fetch returns the files of the revision in force and a label of their origin (e.g. the
	// bundle revision); changed false (and nil files) means "same as the last fetch".
	Fetch(ctx context.Context) (files Files, origin string, changed bool, err error)
}

// Options configures the Runtime.
type Options struct {
	// Engine is builtin (default) or opa.
	Engine string
	// Static are rules and windows of manent.yml.
	Static *governance.Policy
	// Store holds revisions (source store) and runtime freezes; nil without database.
	Store cluster.PolicyStore
	// SourceName and Source: store, or another source with its implementation.
	SourceName string
	Source     Source
	Compile    CompileOptions
	// Review requires approving a draft with an identity other than its author.
	Review bool
	// Refresh is the interval between two reads of the source and of the freezes.
	Refresh time.Duration
	// MaxStaleness: beyond this without a successful refresh, real deploys are refused.
	MaxStaleness time.Duration
	Logger       *slog.Logger
	// Now replaces the clock in tests.
	Now func() time.Time
	// AuthzCache and AuthzCacheSize configure the cache of the permission grants
	// (rbac.opa.cache, rbac.opa.cache_size).
	AuthzCache     time.Duration
	AuthzCacheSize int
}

// Runtime is the governance policy in force in the process: it implements engine.Governance
// and engine.PolicyAdmin. Every refresh replaces atomically the compiled revision and the
// freezes; an invalid revision never replaces the one in force.
type Runtime struct {
	o       Options
	log     *slog.Logger
	now     func() time.Time
	state   atomic.Pointer[state]
	refresh chan struct{}
	// mu serializes the refreshes.
	mu sync.Mutex
	// cache keeps compiled revisions by hash, for EvaluateRevision and impacts.
	cacheMu sync.Mutex
	cache   map[string]*Compiled
	metrics *collector
	// grants caches the permission grants per revision and identity.
	grants *grantsCache
}

// state is the policy in force.
type state struct {
	compiled *Compiled
	// storeRev is the store revision of compiled (0 = default policy).
	storeRev int64
	// origin is the label of the source revision (bundle revision, Consul index, ...).
	origin   string
	dynamic  []governance.Window
	loadedAt time.Time
	// okAt is the last refresh that reached the sources.
	okAt time.Time
	// lastErr is the last refresh error.
	lastErr error
}

// New creates the Runtime; Start loads the policy and keeps it up to date.
func New(o Options) (*Runtime, error) {
	o.Engine = cmp.Or(o.Engine, EngineBuiltin)
	if o.Engine != EngineBuiltin && o.Engine != EngineOPA {
		return nil, fmt.Errorf("governance.engine %q not supported (builtin|opa)", o.Engine)
	}
	if o.Engine == EngineOPA {
		o.SourceName = cmp.Or(o.SourceName, SourceStore)
		if o.SourceName != SourceStore && o.Source == nil {
			return nil, fmt.Errorf("governance.opa.source %s: missing source", o.SourceName)
		}
	}
	if o.Static == nil {
		o.Static = &governance.Policy{}
	}
	o.Refresh = cmp.Or(o.Refresh, DefaultRefresh)
	o.MaxStaleness = cmp.Or(o.MaxStaleness, DefaultMaxStaleness)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Compile.Rules == nil {
		o.Compile.Rules = o.Static.Rules
	}
	r := &Runtime{o: o, log: o.Logger.With("component", "policy"), now: o.Now, refresh: make(chan struct{}, 1), cache: map[string]*Compiled{}, grants: newGrantsCache(o)}
	r.metrics = newCollector(r)
	r.state.Store(&state{})
	return r, nil
}

// Start loads policy and freezes, then refreshes them every Refresh (and at every database
// notification) until ctx ends. A failed first load does not stop the process: Ready reports
// it and real deploys are refused.
func (r *Runtime) Start(ctx context.Context) {
	if err := r.Refresh(ctx); err != nil {
		r.log.Error("policy di governance non caricata all'avvio", "error", err)
	}
	if r.o.Store != nil {
		go func() {
			_ = r.o.Store.ListenPolicy(ctx, r.Trigger)
		}()
	}
	go func() {
		t := time.NewTicker(r.o.Refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-r.refresh:
			}
			if err := r.Refresh(ctx); err != nil {
				r.log.Warn("aggiornamento della policy di governance fallito", "error", err)
			}
		}
	}()
}

// SetStore sets the database store; call it before Start.
func (r *Runtime) SetStore(s cluster.PolicyStore) { r.o.Store = s }

// HasStore reports whether the runtime reaches the database store.
func (r *Runtime) HasStore() bool { return r.o.Store != nil }

// Frozen returns the first window active at t for the application and environment, without
// regions, exemptions and overrides (dry-run of custom steps).
func (r *Runtime) Frozen(application, environment string, t time.Time) *governance.WindowStatus {
	if active := governance.ActiveWindows(r.windows(r.state.Load()), application, environment, "", t); len(active) > 0 {
		return &active[0]
	}
	return nil
}

// Trigger asks for a refresh as soon as possible.
func (r *Runtime) Trigger() {
	select {
	case r.refresh <- struct{}{}:
	default:
	}
}

// Ready reports whether the process can apply the governance policy.
func (r *Runtime) Ready(context.Context) error {
	_, err := r.current()
	return err
}

// Engine returns the engine (builtin or opa).
func (r *Runtime) Engine() string { return r.o.Engine }

// current returns the state in force, or ErrUnavailable.
func (r *Runtime) current() (*state, error) {
	st := r.state.Load()
	if r.o.Engine == EngineOPA && st.compiled == nil {
		msg := "no governance policy loaded"
		if st.lastErr != nil {
			msg += ": " + st.lastErr.Error()
		}
		return nil, fmt.Errorf("%w: %s", governance.ErrUnavailable, msg)
	}
	needsRefresh := r.o.Engine == EngineOPA || r.o.Store != nil
	if needsRefresh && !st.okAt.IsZero() && r.now().Sub(st.okAt) > r.o.MaxStaleness {
		return nil, fmt.Errorf("%w: policy not refreshed since %s (max_staleness %s): %v", governance.ErrUnavailable,
			st.okAt.Format(time.RFC3339), r.o.MaxStaleness, st.lastErr)
	}
	if needsRefresh && st.okAt.IsZero() && r.o.Engine == EngineBuiltin && st.lastErr != nil {
		// Built-in rules without the runtime freezes ever read: the windows created at runtime
		// would be ignored.
		return nil, fmt.Errorf("%w: runtime freezes not readable: %v", governance.ErrUnavailable, st.lastErr)
	}
	return st, nil
}

// Revision returns the label of the policy in force.
func (r *Runtime) Revision() string {
	st := r.state.Load()
	if st.compiled != nil {
		return st.compiled.Revision
	}
	if r.o.Engine == EngineOPA {
		return ""
	}
	return r.o.Static.Revision()
}

// windows are the freeze windows in force: manent.yml, then those of the revision, then the
// runtime ones.
func (r *Runtime) windows(st *state) []governance.Window {
	var rev []governance.Window
	if st.compiled != nil {
		rev = st.compiled.Windows
	}
	return slices.Concat(r.o.Static.Freezes, rev, st.dynamic)
}

// Evaluate implements engine.Governance.
func (r *Runtime) Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error) {
	start := r.now()
	st, err := r.current()
	if err != nil {
		r.metrics.evaluation(in.Phase, "unavailable", 0)
		return governance.Decision{}, err
	}
	dec, err := r.decide(ctx, st.compiled, st, in)
	outcome := "allowed"
	switch {
	case err != nil:
		outcome = "error"
	case len(dec.Deny) > 0:
		outcome = "denied"
	case dec.Freeze != nil:
		outcome = "frozen"
	}
	r.metrics.evaluation(in.Phase, outcome, r.now().Sub(start))
	return dec, err
}

// decide evaluates with the compiled revision c (nil: built-in rules).
func (r *Runtime) decide(ctx context.Context, c *Compiled, st *state, in governance.Input) (governance.Decision, error) {
	windows := r.windows(st)
	if c == nil {
		return governance.Decide(ctx, in, windows, func(_ context.Context, in governance.Input) (governance.Decision, error) {
			return r.o.Static.RuleDecisionFor(in), nil
		})
	}
	if c != st.compiled {
		windows = slices.Concat(r.o.Static.Freezes, c.Windows, st.dynamic)
	}
	return governance.Decide(ctx, in, windows, c.Evaluate)
}

// Windows implements engine.Governance.
func (r *Runtime) Windows(application, environment string, t time.Time) []governance.WindowStatus {
	return governance.ListWindows(r.windows(r.state.Load()), application, environment, t)
}

// Refresh reads freezes and policy from the sources and replaces them if changed.
func (r *Runtime) Refresh(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.state.Load()
	next := *cur
	var errs []error
	if r.o.Store != nil {
		ws, err := r.readFreezes(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("runtime freezes: %w", err))
		} else {
			next.dynamic = ws
		}
	}
	if r.o.Engine == EngineOPA {
		if err := r.refreshPolicy(ctx, cur, &next); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	next.lastErr = err
	if err == nil {
		next.okAt = r.now()
	}
	r.state.Store(&next)
	if err != nil {
		r.metrics.loadError()
	}
	return err
}

// refreshPolicy loads the revision in force from the source, if changed. A compilation error
// keeps the previous revision.
func (r *Runtime) refreshPolicy(ctx context.Context, cur *state, next *state) error {
	var files Files
	var label, origin string
	var storeRev int64
	if r.o.SourceName == SourceStore {
		if r.o.Store == nil {
			return errors.New("governance.opa.source store requires the database (dbos.database_url)")
		}
		rev, err := r.o.Store.ActivePolicy(ctx)
		if err != nil {
			return fmt.Errorf("active policy revision: %w", err)
		}
		if rev == cur.storeRev && cur.compiled != nil {
			return nil
		}
		storeRev = rev
		if rev == 0 {
			files, label = Files{}, "default"
		} else {
			rec, err := r.o.Store.Policy(ctx, rev)
			if err != nil {
				return fmt.Errorf("policy revision %d: %w", rev, err)
			}
			if rec == nil {
				return fmt.Errorf("active policy revision %d not found", rev)
			}
			if err := json.Unmarshal([]byte(rec.Files), &files); err != nil {
				return fmt.Errorf("policy revision %d: %w", rev, err)
			}
			label = strconv.FormatInt(rev, 10)
		}
	} else {
		f, o, changed, err := r.o.Source.Fetch(ctx)
		if err != nil {
			return fmt.Errorf("policy source %s: %w", r.o.SourceName, err)
		}
		if !changed && cur.compiled != nil {
			return nil
		}
		if f == nil {
			return fmt.Errorf("policy source %s: no revision", r.o.SourceName)
		}
		files, origin, label = f, o, r.o.SourceName
	}
	c, err := r.compile(ctx, files, label)
	if err != nil {
		r.log.Error("revisione della policy rifiutata: resta in vigore la precedente", "source", r.o.SourceName,
			"revision", label, "previous", revisionOf(cur.compiled), "error", err)
		return fmt.Errorf("policy revision %s rejected: %w", label, err)
	}
	if cur.compiled != nil && c.SHA256 == cur.compiled.SHA256 {
		next.storeRev, next.origin = storeRev, origin
		return nil
	}
	next.compiled, next.storeRev, next.origin, next.loadedAt = c, storeRev, origin, r.now()
	r.log.Info("revisione della policy attiva", "source", r.o.SourceName, "revision", c.Revision, "sha256", c.SHA256,
		"origin", origin, "previous", revisionOf(cur.compiled))
	return nil
}

func revisionOf(c *Compiled) string {
	if c == nil {
		return ""
	}
	return c.Revision
}

// compile compiles a revision, reusing the cache by hash; label prefixes the hash.
func (r *Runtime) compile(ctx context.Context, files Files, label string) (*Compiled, error) {
	sha := files.Hash()
	full := label + ":" + sha[:12]
	r.cacheMu.Lock()
	c, ok := r.cache[full]
	r.cacheMu.Unlock()
	if ok {
		return c, nil
	}
	c, _, err := Compile(ctx, files, full, r.o.Compile)
	if err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	if len(r.cache) >= 8 {
		clear(r.cache)
	}
	r.cache[full] = c
	r.cacheMu.Unlock()
	return c, nil
}

// storedWindow is a runtime freeze window as stored.
type storedWindow struct {
	Name         string    `json:"name"`
	Reason       string    `json:"reason,omitempty"`
	Applications []string  `json:"applications"`
	Environments []string  `json:"environments"`
	Regions      []string  `json:"regions,omitempty"`
	Tenants      string    `json:"tenants,omitempty"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Hard         bool      `json:"hard,omitempty"`
}

// readFreezes reads the runtime freeze windows (the expired ones are not listed by Windows).
func (r *Runtime) readFreezes(ctx context.Context) ([]governance.Window, error) {
	recs, err := r.o.Store.ListFreezes(ctx)
	if err != nil {
		return nil, err
	}
	var ws []governance.Window
	for _, rec := range recs {
		var sw storedWindow
		if err := json.Unmarshal([]byte(rec.Data), &sw); err != nil {
			r.log.Warn("freeze illeggibile ignorato", "freeze", rec.Name, "error", err)
			continue
		}
		w := governance.Window{Name: rec.Name, Reason: sw.Reason, Applications: sw.Applications, Environments: sw.Environments,
			Regions: sw.Regions, Tenants: sw.Tenants, Artifacts: sw.Artifacts, From: sw.From, To: sw.To, Hard: sw.Hard, CreatedBy: rec.CreatedBy, CreatedAt: rec.CreatedAt}
		ws = append(ws, w)
	}
	if errs := governance.PrepareWindows(ws, "freeze"); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return ws, nil
}

// requireStore returns the store or an error.
func (r *Runtime) requireStore() (cluster.PolicyStore, error) {
	if r.o.Store == nil {
		return nil, fmt.Errorf("%w: the database is not available", service.ErrInvalidRequest)
	}
	return r.o.Store, nil
}

// CreateFreeze implements engine.PolicyAdmin.
func (r *Runtime) CreateFreeze(ctx context.Context, w governance.Window) error {
	store, err := r.requireStore()
	if err != nil {
		return err
	}
	for _, x := range r.windows(r.state.Load()) {
		if x.Name == w.Name {
			return fmt.Errorf("%w: a freeze window named %s already exists", service.ErrInvalidRequest, w.Name)
		}
	}
	data, err := json.Marshal(storedWindow{Name: w.Name, Reason: w.Reason, Applications: w.Applications, Environments: w.Environments,
		Regions: w.Regions, Tenants: w.Tenants, Artifacts: w.Artifacts, From: w.From, To: w.To, Hard: w.Hard})
	if err != nil {
		return err
	}
	if err := store.CreateFreeze(ctx, cluster.FreezeRecord{Name: w.Name, Data: string(data), CreatedBy: w.CreatedBy, CreatedAt: w.CreatedAt}); err != nil {
		return fmt.Errorf("creating freeze %s (the name may already exist): %w", w.Name, err)
	}
	r.changed(ctx)
	return nil
}

// DeleteFreeze implements engine.PolicyAdmin.
func (r *Runtime) DeleteFreeze(ctx context.Context, name string) error {
	store, err := r.requireStore()
	if err != nil {
		return err
	}
	ok, err := store.DeleteFreeze(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: freeze %s (only windows created at runtime can be deleted)", service.ErrNotFound, name)
	}
	r.changed(ctx)
	return nil
}

// changed notifies the other processes and refreshes this one at once.
func (r *Runtime) changed(ctx context.Context) {
	if err := r.o.Store.NotifyPolicy(ctx); err != nil {
		r.log.Warn("notifica della modifica della policy fallita: gli altri processi la vedranno al prossimo aggiornamento", "error", err)
	}
	if err := r.Refresh(ctx); err != nil {
		r.log.Warn("aggiornamento della policy di governance fallito", "error", err)
	}
}

// List implements engine.PolicyAdmin.
func (r *Runtime) List(ctx context.Context) (*service.PolicyList, error) {
	st := r.state.Load()
	out := &service.PolicyList{Engine: r.o.Engine, Current: r.Revision(), Revisions: []service.PolicyRevision{}}
	if r.o.Engine == EngineOPA {
		out.Source = r.o.SourceName
		if !st.loadedAt.IsZero() {
			at := st.loadedAt
			out.LoadedAt = &at
		}
	}
	if r.o.Engine != EngineOPA || r.o.SourceName != SourceStore || r.o.Store == nil {
		return out, nil
	}
	recs, err := r.o.Store.Policies(ctx)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		out.Revisions = append(out.Revisions, toRevision(rec))
	}
	return out, nil
}

// toRevision converts a stored revision.
func toRevision(rec cluster.PolicyRecord) service.PolicyRevision {
	rev := service.PolicyRevision{
		Revision: rec.Revision, SHA256: rec.SHA256, Status: rec.Status, Author: rec.Author, Comment: rec.Comment,
		ReviewedBy: rec.ReviewedBy, CreatedAt: rec.CreatedAt, ActivatedAt: rec.ActivatedAt,
	}
	if rec.Tests != "" {
		_ = json.Unmarshal([]byte(rec.Tests), &rev.Tests)
	}
	if rec.Files != "" {
		_ = json.Unmarshal([]byte(rec.Files), &rev.Files)
	}
	return rev
}

// requireRevisions checks that revisions are managed by manent (source store).
func (r *Runtime) requireRevisions() (cluster.PolicyStore, error) {
	if r.o.Engine != EngineOPA || r.o.SourceName != SourceStore {
		return nil, fmt.Errorf("%w: policy revisions are managed by manent only with governance.engine opa and governance.opa.source store (engine %s, source %s)",
			service.ErrInvalidRequest, r.o.Engine, cmp.Or(r.o.SourceName, "-"))
	}
	return r.requireStore()
}

// Get implements engine.PolicyAdmin.
func (r *Runtime) Get(ctx context.Context, revision int64) (*service.PolicyRevision, error) {
	store, err := r.requireRevisions()
	if err != nil {
		return nil, err
	}
	rec, err := store.Policy(ctx, revision)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: policy revision %d", service.ErrNotFound, revision)
	}
	rev := toRevision(*rec)
	return &rev, nil
}

// Push implements engine.PolicyAdmin: the revision is compiled, tested and evaluated on the
// sample inputs before being stored as a draft.
func (r *Runtime) Push(ctx context.Context, files map[string]string, comment, author string) (*service.PolicyRevision, error) {
	store, err := r.requireRevisions()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: the revision has no files", service.ErrInvalidRequest)
	}
	c, tests, err := Compile(ctx, files, "draft", r.o.Compile)
	if err != nil {
		return nil, fmt.Errorf("%w: %w%s", service.ErrInvalidRequest, err, testReport(tests))
	}
	fb, _ := json.Marshal(files)
	tb, _ := json.Marshal(tests)
	now := r.now().UTC()
	rev, err := store.CreatePolicy(ctx, cluster.PolicyRecord{SHA256: c.SHA256, Author: author, Comment: comment, CreatedAt: now, Tests: string(tb), Files: string(fb)})
	if err != nil {
		return nil, err
	}
	return &service.PolicyRevision{Revision: rev, SHA256: c.SHA256, Status: service.PolicyDraft, Author: author, Comment: comment, CreatedAt: now, Tests: tests}, nil
}

// testReport lists the failed tests, for the push error.
func testReport(tests []service.PolicyTest) string {
	s := ""
	for _, t := range tests {
		if !t.Pass {
			s += "\n- " + t.Name + ": " + t.Error
		}
	}
	return s
}

// Activate implements engine.PolicyAdmin.
func (r *Runtime) Activate(ctx context.Context, revision int64, by string, draft bool) error {
	store, err := r.requireRevisions()
	if err != nil {
		return err
	}
	rec, err := store.Policy(ctx, revision)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("%w: policy revision %d", service.ErrNotFound, revision)
	}
	switch {
	case rec.Status == service.PolicyActive:
		return fmt.Errorf("%w: revision %d is already active", service.ErrInvalidRequest, revision)
	case draft && rec.Status != service.PolicyDraft:
		return fmt.Errorf("%w: revision %d is not a draft: use activate to go back to it", service.ErrInvalidRequest, revision)
	case !draft && rec.Status == service.PolicyDraft:
		return fmt.Errorf("%w: revision %d is a draft: it must be approved", service.ErrInvalidRequest, revision)
	case draft && r.o.Review && rec.Author == by:
		return fmt.Errorf("%w: %s pushed revision %d and cannot approve it (governance.opa.store.review)", service.ErrForbidden, by, revision)
	}
	var files Files
	if err := json.Unmarshal([]byte(rec.Files), &files); err != nil {
		return err
	}
	if _, err := r.compile(ctx, files, strconv.FormatInt(revision, 10)); err != nil {
		return fmt.Errorf("%w: revision %d does not compile with this version of manent: %w", service.ErrInvalidRequest, revision, err)
	}
	expected, err := store.ActivePolicy(ctx)
	if err != nil {
		return err
	}
	reviewer := ""
	if draft {
		reviewer = by
	}
	if err := store.ActivatePolicy(ctx, revision, expected, reviewer); err != nil {
		if errors.Is(err, cluster.ErrPolicyConflict) {
			return fmt.Errorf("%w: %w, retry", service.ErrInvalidRequest, err)
		}
		return err
	}
	r.changed(ctx)
	return nil
}

// ForceActivate activates any revision without the state and four-eyes checks (break-glass
// of manent-server admin, on the database): the revision must still compile.
func (r *Runtime) ForceActivate(ctx context.Context, revision int64, by string) error {
	store, err := r.requireRevisions()
	if err != nil {
		return err
	}
	rec, err := store.Policy(ctx, revision)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("%w: policy revision %d", service.ErrNotFound, revision)
	}
	var files Files
	if err := json.Unmarshal([]byte(rec.Files), &files); err != nil {
		return err
	}
	if _, err := r.compile(ctx, files, strconv.FormatInt(revision, 10)); err != nil {
		return fmt.Errorf("revision %d does not compile with this version of manent: %w", revision, err)
	}
	expected, err := store.ActivePolicy(ctx)
	if err != nil {
		return err
	}
	if err := store.ActivatePolicy(ctx, revision, expected, "break-glass:"+by); err != nil {
		return err
	}
	r.changed(ctx)
	return nil
}

// EvaluateRevision implements engine.PolicyAdmin.
func (r *Runtime) EvaluateRevision(ctx context.Context, revision int64, in governance.Input) (governance.Decision, error) {
	st := r.state.Load()
	if revision == 0 {
		cur, err := r.current()
		if err != nil {
			return governance.Decision{}, err
		}
		return r.decide(ctx, cur.compiled, cur, in)
	}
	c, err := r.compiledRevision(ctx, revision)
	if err != nil {
		return governance.Decision{}, err
	}
	return r.decide(ctx, c, st, in)
}

// compiledRevision compiles a stored revision (0 = the one in force).
func (r *Runtime) compiledRevision(ctx context.Context, revision int64) (*Compiled, error) {
	if revision == 0 {
		cur, err := r.current()
		if err != nil {
			return nil, err
		}
		return cur.compiled, nil
	}
	store, err := r.requireRevisions()
	if err != nil {
		return nil, err
	}
	rec, err := store.Policy(ctx, revision)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: policy revision %d", service.ErrNotFound, revision)
	}
	var files Files
	if err := json.Unmarshal([]byte(rec.Files), &files); err != nil {
		return nil, err
	}
	return r.compile(ctx, files, strconv.FormatInt(revision, 10))
}
