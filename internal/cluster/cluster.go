// Package cluster keeps the registry of manent executors (workers): heartbeats, shutdown,
// claiming the workflows of a lost node and leases for the tasks only one worker at a time
// must perform (automatic recovery, retention, reconciliation). It also stores the state read
// from the targets by the reconciliation.
//
// The registry lives in the DBOS database, in its own tables: without DBOS Conductor it is the
// only way to know which executors are still alive.
package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	// database/sql drivers for Postgres ("pgx") and SQLite ("sqlite").
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Executor is a registered worker.
type Executor struct {
	ID string `json:"id"`
	// Version is the application version of the workflows (app.version): a worker only resumes
	// workflows of its own version.
	Version string `json:"version"`
	// Build is the manent binary version.
	Build    string `json:"build,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// Region is the worker's region (dbos.region), if configured.
	Region      string     `json:"region,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	// RecoveredBy is who claimed the executor's workflows after it was lost.
	RecoveredBy string     `json:"recovered_by,omitempty"`
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
	// Queues are the DBOS queues the executor dequeues from.
	Queues []string `json:"queues,omitempty"`
	// Resources is the last resource sample sent by the executor, in JSON
	// (service.WorkerResources): the registry stores it without interpreting it.
	Resources json.RawMessage `json:"resources,omitempty"`
}

// Stale reports an executor that has sent no heartbeat for at least after, or that stopped.
func (e Executor) Stale(now time.Time, after time.Duration) bool {
	return e.StoppedAt != nil || now.Sub(e.HeartbeatAt) >= after
}

// Registry is the executor registry.
//
// Startup registration and claiming exclude each other: Claim succeeds only on an executor
// that sends no heartbeat, and Register waits until a running recovery of its workflows ends.
// So a node that comes back right while it is being recovered does not resume (through DBOS
// recovery) workflows another worker is already re-enqueuing.
type Registry interface {
	// Register records the executor's startup and clears previous shutdown and claim.
	// It returns false if a recovery of its workflows is running: retry later.
	// It returns *InUseError if the same ID is registered by another startup that has not
	// stopped and sent a heartbeat less than staleAfter ago: two processes with the same ID would
	// resume each other's workflows. Call it before starting DBOS.
	Register(ctx context.Context, e Executor, staleAfter time.Duration) (bool, error)
	// Heartbeat updates the executor started at startedAt. It returns who claimed its workflows,
	// if that happened while it was unreachable, or ErrReplaced if another startup registered
	// with the same ID.
	Heartbeat(ctx context.Context, id string, startedAt time.Time) (recoveredBy string, err error)
	// Report stores the resource sample (JSON) of the executor started at startedAt.
	Report(ctx context.Context, id string, startedAt time.Time, resources json.RawMessage) error
	// Stopped records the orderly shutdown of the executor started at startedAt.
	Stopped(ctx context.Context, id string, startedAt time.Time) error
	// List returns the registered executors, ordered by ID.
	List(ctx context.Context) ([]Executor, error)
	// Claim claims the workflows of a lost executor: it succeeds only if the executor stopped or
	// has sent no heartbeat since before staleBefore, and nobody claimed it already (or the claim
	// stayed unfinished beyond ClaimTimeout). With force it always succeeds, even on an executor
	// never registered.
	Claim(ctx context.Context, id, by string, staleBefore time.Time, force bool) (bool, error)
	// ClaimDone marks the recovery of the executor's workflows as done.
	ClaimDone(ctx context.Context, id string) error
	// Lease acquires or renews lease name for ttl: it succeeds if it is free, expired or already
	// held by holder.
	Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	Close() error
}

// TargetsRecord is the state read from the targets of an application/environment by one
// reconciliation scope: "" for the workers without a region, the region otherwise.
type TargetsRecord struct {
	Application string
	Environment string
	// Tenant is the tenant of the deploy read ("" for untenanted deploys).
	Tenant string
	// Artifact is the artifact of the release read.
	Artifact  string
	Scope     string
	CheckedAt time.Time
	// Data is the state, in JSON (service.TargetsState).
	Data string
}

// TargetStore stores the last state read from the targets, one record per
// application/environment and scope.
type TargetStore interface {
	SaveTargets(ctx context.Context, r TargetsRecord) error
	// ListTargets returns all records, ordered by application, environment and scope.
	ListTargets(ctx context.Context) ([]TargetsRecord, error)
}

// InUseError means an executor_id already registered by another live process.
type InUseError struct {
	ID, Hostname           string
	StartedAt, HeartbeatAt time.Time
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("executor_id %q already in use by an active worker (host %s, started %s, last heartbeat %s): every worker must have a unique dbos.executor_id (e.g. DBOS__VMID)",
		e.ID, e.Hostname, e.StartedAt.Format(time.RFC3339), e.HeartbeatAt.Format(time.RFC3339))
}

// ErrReplaced means another startup registered with the same executor_id.
var ErrReplaced = errors.New("another process registered with the same executor_id")

// ClaimTimeout is the time after which a recovery not marked as done (e.g. because whoever
// ran it crashed) no longer blocks the executor's registration.
const ClaimTimeout = 2 * time.Minute

// SQL is the Registry on Postgres or SQLite.
type SQL struct {
	db *sql.DB
	// executors, leases and targets are the qualified table names; exBare and leaseBare the ones
	// used to refer to the existing row in ON CONFLICT.
	executors, leases, targets, exBare, leaseBare string
	// policies, policyActive and freezes are the policy tables; policyActiveBare the name used
	// in ON CONFLICT.
	policies, policyActive, freezes, policyActiveBare string
	// postgres and dsn serve the policy notifications (LISTEN/NOTIFY).
	postgres bool
	dsn      string
	now      func() time.Time
}

var (
	_ Registry    = (*SQL)(nil)
	_ TargetStore = (*SQL)(nil)
)

// Open opens the registry on the DBOS database (database_url) and creates the tables if
// missing: on Postgres in the "manent" schema, on SQLite with the "manent_" prefix.
func Open(ctx context.Context, databaseURL string) (*SQL, error) {
	driver, dsn, postgres, err := dataSource(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("executor registry: %w", err)
	}
	r := &SQL{db: db, now: time.Now, postgres: postgres, dsn: dsn}
	if postgres {
		db.SetMaxOpenConns(4)
		r.executors, r.leases, r.targets, r.exBare, r.leaseBare = "manent.executors", "manent.leases", "manent.target_states", "executors", "leases"
		r.policies, r.policyActive, r.freezes, r.policyActiveBare = "manent.policy_revisions", "manent.policy_active", "manent.freeze_windows", "policy_active"
	} else {
		// SQLite: one connection, so writes do not compete within the process.
		db.SetMaxOpenConns(1)
		r.executors, r.leases, r.targets = "manent_executors", "manent_leases", "manent_target_states"
		r.exBare, r.leaseBare = r.executors, r.leases
		r.policies, r.policyActive, r.freezes = "manent_policy_revisions", "manent_policy_active", "manent_freeze_windows"
		r.policyActiveBare = r.policyActive
	}
	if err := r.migrate(ctx, postgres); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// dataSource maps the DBOS URL to the database/sql driver and DSN.
func dataSource(url string) (driver, dsn string, postgres bool, err error) {
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"), strings.Contains(url, "host="), strings.Contains(url, "dbname="):
		return "pgx", url, true, nil
	case strings.HasPrefix(url, "sqlite:"), strings.HasPrefix(url, "sqlite3:"):
		_, path, _ := strings.Cut(url, ":")
		if strings.HasPrefix(path, "///") {
			path = path[2:]
		}
		if path == "" || strings.Contains(path, ":memory:") {
			return "", "", false, errors.New("executor registry: in-memory SQLite not supported")
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		// The file is shared with DBOS: wait for the lock instead of failing at once.
		return "sqlite", path + sep + "_pragma=busy_timeout(10000)", false, nil
	}
	return "", "", false, fmt.Errorf("executor registry: database_url %q not supported", url)
}

// migrate creates schema and tables. Several workers started together can conflict on
// creating the Postgres schema: retry.
func (r *SQL) migrate(ctx context.Context, postgres bool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + r.executors + ` (
			executor_id  TEXT PRIMARY KEY,
			version      TEXT NOT NULL,
			build        TEXT NOT NULL,
			hostname     TEXT NOT NULL,
			started_at   BIGINT NOT NULL,
			heartbeat_at BIGINT NOT NULL,
			stopped_at   BIGINT,
			recovered_by TEXT,
			recovered_at BIGINT,
			recovery_done_at BIGINT)`,
		`CREATE TABLE IF NOT EXISTS ` + r.leases + ` (
			name       TEXT PRIMARY KEY,
			holder     TEXT NOT NULL,
			expires_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + r.targets + ` (
			application TEXT NOT NULL,
			environment TEXT NOT NULL,
			scope       TEXT NOT NULL,
			checked_at  BIGINT NOT NULL,
			data        TEXT NOT NULL,
			PRIMARY KEY (application, environment, scope))`,
	}
	stmts = append(stmts, r.policyTables(postgres)...)
	if postgres {
		stmts = append([]string{`CREATE SCHEMA IF NOT EXISTS manent`}, stmts...)
	}
	var err error
	for attempt := range 4 {
		if err = r.exec(ctx, stmts); err == nil {
			if err = r.addColumns(ctx, postgres); err == nil {
				return nil
			}
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return fmt.Errorf("executor registry: creating tables: %w", err)
}

// addedColumns are the executor columns introduced after the first registry version.
var addedColumns = []string{"region", "queues", "resources"}

// addColumns adds the columns introduced after the first registry version.
func (r *SQL) addColumns(ctx context.Context, postgres bool) error {
	for _, col := range addedColumns {
		if postgres {
			if _, err := r.db.ExecContext(ctx, `ALTER TABLE `+r.executors+` ADD COLUMN IF NOT EXISTS `+col+` TEXT`); err != nil {
				return err
			}
			continue
		}
		// SQLite has no ADD COLUMN IF NOT EXISTS: check the schema first.
		var n int
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('`+r.executors+`') WHERE name = '`+col+`'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := r.db.ExecContext(ctx, `ALTER TABLE `+r.executors+` ADD COLUMN `+col+` TEXT`); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *SQL) exec(ctx context.Context, stmts []string) error {
	for _, s := range stmts {
		if _, err := r.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

// Register implements Registry. Registration replaces the existing row only if it belongs to
// the same startup read before (started_at): if another process registered meanwhile, read
// again and reevaluate.
func (r *SQL) Register(ctx context.Context, e Executor, staleAfter time.Duration) (bool, error) {
	t := r.exBare
	for range 3 {
		now := r.now()
		var (
			started, heartbeat int64
			stopped            sql.NullInt64
			hostname           string
			recoveredBy        sql.NullString
		)
		prev := int64(-1)
		err := r.db.QueryRowContext(ctx, `SELECT started_at, heartbeat_at, stopped_at, hostname, recovered_by FROM `+r.executors+` WHERE executor_id = $1`, e.ID).
			Scan(&started, &heartbeat, &stopped, &hostname, &recoveredBy)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return false, err
		default:
			prev = started
			// A claimed executor has already been declared lost: even if it still answers, the new
			// registration follows the recovery rules.
			if !stopped.Valid && !recoveredBy.Valid && started != ms(e.StartedAt) && now.Sub(time.UnixMilli(heartbeat)) < staleAfter {
				return false, &InUseError{ID: e.ID, Hostname: hostname, StartedAt: time.UnixMilli(started).UTC(), HeartbeatAt: time.UnixMilli(heartbeat).UTC()}
			}
		}
		res, err := r.db.ExecContext(ctx, `INSERT INTO `+r.executors+` (executor_id, version, build, hostname, started_at, heartbeat_at, region, queues, resources)
			VALUES ($1, $2, $3, $4, $5, $6, $8, $10, $11)
			ON CONFLICT (executor_id) DO UPDATE SET
				version = excluded.version, build = excluded.build, hostname = excluded.hostname, region = excluded.region,
				queues = excluded.queues, resources = excluded.resources,
				started_at = excluded.started_at, heartbeat_at = excluded.heartbeat_at,
				stopped_at = NULL, recovered_by = NULL, recovered_at = NULL, recovery_done_at = NULL
			WHERE `+t+`.started_at = $9
			  AND (`+t+`.recovered_by IS NULL OR `+t+`.recovery_done_at IS NOT NULL OR `+t+`.recovered_at < $7)`,
			e.ID, e.Version, e.Build, e.Hostname, ms(e.StartedAt), ms(now), ms(now.Add(-ClaimTimeout)), e.Region, prev,
			nullJSON(e.Queues), nullString(e.Resources))
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			return n == 1, err
		}
		// No row updated: recovery running (the row is the one read) or another startup registered
		// meanwhile (reevaluate).
		var now2 int64
		err = r.db.QueryRowContext(ctx, `SELECT started_at FROM `+r.executors+` WHERE executor_id = $1`, e.ID).Scan(&now2)
		if err == nil && now2 == prev {
			return false, nil
		}
	}
	return false, fmt.Errorf("executor registry: registration of %q contended by another process", e.ID)
}

// Heartbeat implements Registry.
func (r *SQL) Heartbeat(ctx context.Context, id string, startedAt time.Time) (string, error) {
	if _, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET heartbeat_at = $3 WHERE executor_id = $1 AND started_at = $2`,
		id, ms(startedAt), ms(r.now())); err != nil {
		return "", err
	}
	var by sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT recovered_by FROM `+r.executors+` WHERE executor_id = $1 AND started_at = $2`, id, ms(startedAt)).Scan(&by)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrReplaced
	}
	return by.String, err
}

// Report implements Registry.
func (r *SQL) Report(ctx context.Context, id string, startedAt time.Time, resources json.RawMessage) error {
	_, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET resources = $3 WHERE executor_id = $1 AND started_at = $2`,
		id, ms(startedAt), nullString(resources))
	return err
}

// nullJSON encodes queues as a JSON array, NULL if empty.
func nullJSON(queues []string) sql.NullString {
	if len(queues) == 0 {
		return sql.NullString{}
	}
	b, _ := json.Marshal(queues)
	return sql.NullString{String: string(b), Valid: true}
}

// nullString stores a JSON document, NULL if empty.
func nullString(b json.RawMessage) sql.NullString {
	return sql.NullString{String: string(b), Valid: len(b) > 0}
}

// Stopped implements Registry.
func (r *SQL) Stopped(ctx context.Context, id string, startedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET stopped_at = $3 WHERE executor_id = $1 AND started_at = $2`,
		id, ms(startedAt), ms(r.now()))
	return err
}

// List implements Registry.
func (r *SQL) List(ctx context.Context) ([]Executor, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT executor_id, version, build, hostname, started_at, heartbeat_at,
		stopped_at, recovered_by, recovered_at, region, queues, resources FROM `+r.executors+` ORDER BY executor_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Executor{}
	for rows.Next() {
		var (
			e                   Executor
			started, heartbeat  int64
			stopped, recoveredT sql.NullInt64
			recoveredBy, region sql.NullString
			queues, resources   sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Version, &e.Build, &e.Hostname, &started, &heartbeat, &stopped, &recoveredBy, &recoveredT, &region, &queues, &resources); err != nil {
			return nil, err
		}
		if queues.Valid {
			// A malformed value (never written by manent) leaves the list empty.
			_ = json.Unmarshal([]byte(queues.String), &e.Queues)
		}
		if resources.Valid && json.Valid([]byte(resources.String)) {
			e.Resources = json.RawMessage(resources.String)
		}
		e.StartedAt, e.HeartbeatAt = time.UnixMilli(started).UTC(), time.UnixMilli(heartbeat).UTC()
		e.StoppedAt, e.RecoveredBy, e.RecoveredAt = fromMs(stopped), recoveredBy.String, fromMs(recoveredT)
		e.Region = region.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// Claim implements Registry. An executor missing from the registry can be claimed only with force.
func (r *SQL) Claim(ctx context.Context, id, by string, staleBefore time.Time, force bool) (bool, error) {
	f := 0
	if force {
		f = 1
	}
	res, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET recovered_by = $2, recovered_at = $3, recovery_done_at = NULL
		WHERE executor_id = $1
		  AND (recovered_by IS NULL OR (recovery_done_at IS NULL AND recovered_at < $6) OR $5 = 1)
		  AND (heartbeat_at < $4 OR stopped_at IS NOT NULL OR $5 = 1)`, id, by, ms(r.now()), ms(staleBefore), f, ms(r.now().Add(-ClaimTimeout)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 && force {
		// Executor never registered (e.g. lost before the registry existed).
		var exists int
		err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+r.executors+` WHERE executor_id = $1`, id).Scan(&exists)
		return err == nil && exists == 0, err
	}
	return n == 1, nil
}

// ClaimDone implements Registry.
func (r *SQL) ClaimDone(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET recovery_done_at = $2 WHERE executor_id = $1 AND recovered_by IS NOT NULL`, id, ms(r.now()))
	return err
}

// Lease implements Registry.
func (r *SQL) Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := r.now()
	t := r.leaseBare
	res, err := r.db.ExecContext(ctx, `INSERT INTO `+r.leases+` (name, holder, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE `+t+`.holder = excluded.holder OR `+t+`.expires_at < $4`, name, holder, ms(now.Add(ttl)), ms(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SaveTargets implements TargetStore: it replaces the record of the same scope.
func (r *SQL) SaveTargets(ctx context.Context, t TargetsRecord) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO `+r.targets+` (application, environment, scope, checked_at, data) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (application, environment, scope) DO UPDATE SET checked_at = excluded.checked_at, data = excluded.data`,
		storedApplication(t.Application, t.Artifact), t.Environment, storedScope(t.Scope, t.Tenant), ms(t.CheckedAt), t.Data)
	return err
}

// artifactSep separates application and artifact in the stored application, as in the queue
// partition keys: '+' is not allowed in names, and the records without an artifact
// stay valid.
const artifactSep = "+"

func storedApplication(application, artifact string) string {
	if artifact == "" {
		return application
	}
	return application + artifactSep + artifact
}

// tenantSep separates scope and tenant in the stored scope: the key of the table stays
// (application, environment, scope) and the records written before tenants stay valid.
const tenantSep = "|"

func storedScope(scope, tenant string) string {
	if tenant == "" {
		return scope
	}
	return scope + tenantSep + tenant
}

// ListTargets implements TargetStore.
func (r *SQL) ListTargets(ctx context.Context) ([]TargetsRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT application, environment, scope, checked_at, data FROM `+r.targets+` ORDER BY application, environment, scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetsRecord
	for rows.Next() {
		var t TargetsRecord
		var at int64
		if err := rows.Scan(&t.Application, &t.Environment, &t.Scope, &at, &t.Data); err != nil {
			return nil, err
		}
		t.Scope, t.Tenant, _ = strings.Cut(t.Scope, tenantSep)
		t.Application, t.Artifact, _ = strings.Cut(t.Application, artifactSep)
		t.CheckedAt = time.UnixMilli(at).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// Close closes the connection.
func (r *SQL) Close() error { return r.db.Close() }
