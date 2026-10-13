// Package cluster tiene il registro degli executor (worker) di manent: heartbeat, arresto,
// rivendicazione dei workflow di un nodo perso e lease per le attività che un solo worker
// alla volta deve svolgere (recovery automatica, retention).
//
// Il registro sta nello stesso database di DBOS, in tabelle proprie: senza DBOS Conductor è
// l'unico modo per sapere quali executor sono ancora attivi.
package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// Driver database/sql per Postgres ("pgx") e SQLite ("sqlite").
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Executor è un worker registrato.
type Executor struct {
	ID string `json:"id"`
	// Version è la versione applicativa dei workflow (app.version): un worker riprende solo
	// i workflow della propria versione.
	Version string `json:"version"`
	// Build è la versione del binario di manent.
	Build    string `json:"build,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// Region è la region del worker (dbos.region), se configurata.
	Region      string     `json:"region,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	// RecoveredBy è chi ha rivendicato i workflow dell'executor dopo la sua perdita.
	RecoveredBy string     `json:"recovered_by,omitempty"`
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
}

// Stale indica un executor che non invia heartbeat da almeno after, o che si è arrestato.
func (e Executor) Stale(now time.Time, after time.Duration) bool {
	return e.StoppedAt != nil || now.Sub(e.HeartbeatAt) >= after
}

// Registry è il registro degli executor.
//
// Registrazione all'avvio e rivendicazione si escludono a vicenda: Claim riesce solo su un
// executor che non invia heartbeat, e Register attende che una recovery in corso dei suoi
// workflow sia conclusa. Così un nodo che torna proprio mentre viene recuperato non riprende
// (con il recovery di DBOS) workflow che un altro worker sta già rimettendo in coda.
type Registry interface {
	// Register registra l'avvio dell'executor e azzera arresto e rivendicazione precedenti.
	// Restituisce false se è in corso la recovery dei suoi workflow: va ripetuto più tardi.
	// Restituisce *InUseError se lo stesso ID è registrato da un altro avvio che non si è
	// arrestato e ha inviato un heartbeat da meno di staleAfter: due processi con lo stesso ID
	// riprenderebbero l'uno i workflow dell'altro. Va chiamato prima di avviare DBOS.
	Register(ctx context.Context, e Executor, staleAfter time.Duration) (bool, error)
	// Heartbeat aggiorna l'executor avviato a startedAt. Restituisce chi ne ha rivendicato i
	// workflow, se è successo mentre era irraggiungibile, o ErrReplaced se un altro avvio si è
	// registrato con lo stesso ID.
	Heartbeat(ctx context.Context, id string, startedAt time.Time) (recoveredBy string, err error)
	// Stopped registra l'arresto ordinato dell'executor avviato a startedAt.
	Stopped(ctx context.Context, id string, startedAt time.Time) error
	// List restituisce gli executor registrati, ordinati per ID.
	List(ctx context.Context) ([]Executor, error)
	// Claim rivendica i workflow di un executor perso: riesce solo se l'executor si è
	// arrestato o non invia heartbeat da prima di staleBefore, e nessuno lo ha già
	// rivendicato (o la rivendicazione è rimasta incompiuta oltre ClaimTimeout). Con force
	// riesce comunque, anche su un executor mai registrato.
	Claim(ctx context.Context, id, by string, staleBefore time.Time, force bool) (bool, error)
	// ClaimDone segna conclusa la recovery dei workflow dell'executor.
	ClaimDone(ctx context.Context, id string) error
	// Lease acquisisce o rinnova per ttl il lease name: riesce se è libero, scaduto o già
	// di holder.
	Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	Close() error
}

// InUseError indica un executor_id già registrato da un altro processo attivo.
type InUseError struct {
	ID, Hostname           string
	StartedAt, HeartbeatAt time.Time
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("executor_id %q già in uso da un worker attivo (host %s, avviato %s, ultimo heartbeat %s): ogni worker deve avere un dbos.executor_id univoco (es. DBOS__VMID)",
		e.ID, e.Hostname, e.StartedAt.Format(time.RFC3339), e.HeartbeatAt.Format(time.RFC3339))
}

// ErrReplaced indica che un altro avvio si è registrato con lo stesso executor_id.
var ErrReplaced = errors.New("un altro processo si è registrato con lo stesso executor_id")

// ClaimTimeout è il tempo dopo il quale una recovery non segnata come conclusa (es. per un
// crash di chi la eseguiva) non blocca più la registrazione dell'executor.
const ClaimTimeout = 2 * time.Minute

// SQL è il Registry su Postgres o SQLite.
type SQL struct {
	db *sql.DB
	// executors e leases sono i nomi qualificati delle tabelle; exBare e leaseBare quelli
	// usati per riferire la riga esistente in ON CONFLICT.
	executors, leases, exBare, leaseBare string
	now                                  func() time.Time
}

var _ Registry = (*SQL)(nil)

// Open apre il registro sul database di DBOS (database_url) e crea le tabelle se mancano:
// su Postgres nello schema "manent", su SQLite con il prefisso "manent_".
func Open(ctx context.Context, databaseURL string) (*SQL, error) {
	driver, dsn, postgres, err := dataSource(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("registro executor: %w", err)
	}
	r := &SQL{db: db, now: time.Now}
	if postgres {
		db.SetMaxOpenConns(4)
		r.executors, r.leases, r.exBare, r.leaseBare = "manent.executors", "manent.leases", "executors", "leases"
	} else {
		// SQLite: una connessione, così le scritture non competono tra loro nel processo.
		db.SetMaxOpenConns(1)
		r.executors, r.leases = "manent_executors", "manent_leases"
		r.exBare, r.leaseBare = r.executors, r.leases
	}
	if err := r.migrate(ctx, postgres); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// dataSource traduce l'URL di DBOS nel driver e nella DSN di database/sql.
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
			return "", "", false, errors.New("registro executor: SQLite in memoria non supportato")
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		// Il file è condiviso con DBOS: si attende il lock invece di fallire subito.
		return "sqlite", path + sep + "_pragma=busy_timeout(10000)", false, nil
	}
	return "", "", false, fmt.Errorf("registro executor: database_url %q non supportato", url)
}

// migrate crea schema e tabelle. Più worker avviati insieme possono andare in conflitto sulla
// creazione dello schema Postgres: si riprova.
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
	}
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
	return fmt.Errorf("registro executor: creazione tabelle: %w", err)
}

// addColumns aggiunge le colonne introdotte dopo la prima versione del registro.
func (r *SQL) addColumns(ctx context.Context, postgres bool) error {
	if postgres {
		_, err := r.db.ExecContext(ctx, `ALTER TABLE `+r.executors+` ADD COLUMN IF NOT EXISTS region TEXT`)
		return err
	}
	// SQLite non ha ADD COLUMN IF NOT EXISTS: si controlla prima lo schema.
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('`+r.executors+`') WHERE name = 'region'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := r.db.ExecContext(ctx, `ALTER TABLE `+r.executors+` ADD COLUMN region TEXT`)
	return err
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

// Register implementa Registry. La registrazione sostituisce la riga esistente solo se
// appartiene allo stesso avvio letto prima (started_at): se nel frattempo un altro processo si è
// registrato, si rilegge e si rivaluta.
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
			// Un executor rivendicato è già stato dichiarato perso: anche se risponde ancora, la
			// nuova registrazione segue le regole della recovery.
			if !stopped.Valid && !recoveredBy.Valid && started != ms(e.StartedAt) && now.Sub(time.UnixMilli(heartbeat)) < staleAfter {
				return false, &InUseError{ID: e.ID, Hostname: hostname, StartedAt: time.UnixMilli(started).UTC(), HeartbeatAt: time.UnixMilli(heartbeat).UTC()}
			}
		}
		res, err := r.db.ExecContext(ctx, `INSERT INTO `+r.executors+` (executor_id, version, build, hostname, started_at, heartbeat_at, region)
			VALUES ($1, $2, $3, $4, $5, $6, $8)
			ON CONFLICT (executor_id) DO UPDATE SET
				version = excluded.version, build = excluded.build, hostname = excluded.hostname, region = excluded.region,
				started_at = excluded.started_at, heartbeat_at = excluded.heartbeat_at,
				stopped_at = NULL, recovered_by = NULL, recovered_at = NULL, recovery_done_at = NULL
			WHERE `+t+`.started_at = $9
			  AND (`+t+`.recovered_by IS NULL OR `+t+`.recovery_done_at IS NOT NULL OR `+t+`.recovered_at < $7)`,
			e.ID, e.Version, e.Build, e.Hostname, ms(e.StartedAt), ms(now), ms(now.Add(-ClaimTimeout)), e.Region, prev)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			return n == 1, err
		}
		// Nessuna riga aggiornata: recovery in corso (la riga è quella letta) o un altro avvio
		// registrato nel frattempo (si rivaluta).
		var now2 int64
		err = r.db.QueryRowContext(ctx, `SELECT started_at FROM `+r.executors+` WHERE executor_id = $1`, e.ID).Scan(&now2)
		if err == nil && now2 == prev {
			return false, nil
		}
	}
	return false, fmt.Errorf("registro executor: registrazione di %q contesa da un altro processo", e.ID)
}

// Heartbeat implementa Registry.
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

// Stopped implementa Registry.
func (r *SQL) Stopped(ctx context.Context, id string, startedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET stopped_at = $3 WHERE executor_id = $1 AND started_at = $2`,
		id, ms(startedAt), ms(r.now()))
	return err
}

// List implementa Registry.
func (r *SQL) List(ctx context.Context) ([]Executor, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT executor_id, version, build, hostname, started_at, heartbeat_at,
		stopped_at, recovered_by, recovered_at, region FROM `+r.executors+` ORDER BY executor_id`)
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
		)
		if err := rows.Scan(&e.ID, &e.Version, &e.Build, &e.Hostname, &started, &heartbeat, &stopped, &recoveredBy, &recoveredT, &region); err != nil {
			return nil, err
		}
		e.StartedAt, e.HeartbeatAt = time.UnixMilli(started).UTC(), time.UnixMilli(heartbeat).UTC()
		e.StoppedAt, e.RecoveredBy, e.RecoveredAt = fromMs(stopped), recoveredBy.String, fromMs(recoveredT)
		e.Region = region.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// Claim implementa Registry. Un executor assente dal registro è rivendicabile solo con force.
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
		// Executor mai registrato (es. perso prima dell'introduzione del registro).
		var exists int
		err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+r.executors+` WHERE executor_id = $1`, id).Scan(&exists)
		return err == nil && exists == 0, err
	}
	return n == 1, nil
}

// ClaimDone implementa Registry.
func (r *SQL) ClaimDone(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE `+r.executors+` SET recovery_done_at = $2 WHERE executor_id = $1 AND recovered_by IS NOT NULL`, id, ms(r.now()))
	return err
}

// Lease implementa Registry.
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

// Close chiude la connessione.
func (r *SQL) Close() error { return r.db.Close() }
