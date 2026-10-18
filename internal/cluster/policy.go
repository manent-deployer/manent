package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PolicyRecord is a governance policy revision stored in the database.
type PolicyRecord struct {
	Revision    int64
	SHA256      string
	Status      string
	Author      string
	Comment     string
	ReviewedBy  string
	CreatedAt   time.Time
	ActivatedAt *time.Time
	// Tests is the test report (JSON), Files the files of the revision (JSON object); Files
	// is empty in listings.
	Tests string
	Files string
}

// FreezeRecord is a freeze window created at runtime; Data is the window in JSON.
type FreezeRecord struct {
	Name      string
	Data      string
	CreatedBy string
	CreatedAt time.Time
}

// Policy revision states, as in service.
const (
	policyDraft      = "draft"
	policyActive     = "active"
	policySuperseded = "superseded"
)

// ErrPolicyConflict means another revision was activated meanwhile.
var ErrPolicyConflict = errors.New("another policy revision was activated meanwhile")

// PolicyStore stores the governance policy revisions and the runtime freeze windows.
type PolicyStore interface {
	// CreatePolicy stores a draft revision and returns its number.
	CreatePolicy(ctx context.Context, r PolicyRecord) (int64, error)
	// Policies lists the revisions, newest first, without files.
	Policies(ctx context.Context) ([]PolicyRecord, error)
	// Policy returns a revision with its files, nil if missing.
	Policy(ctx context.Context, revision int64) (*PolicyRecord, error)
	// ActivePolicy returns the active revision, 0 if none.
	ActivePolicy(ctx context.Context) (int64, error)
	// ActivatePolicy makes revision active if the active one is still expected (0 = none):
	// otherwise ErrPolicyConflict. reviewer, if set, is recorded as who reviewed a draft.
	ActivatePolicy(ctx context.Context, revision, expected int64, reviewer string) error
	CreateFreeze(ctx context.Context, f FreezeRecord) error
	// DeleteFreeze returns false if the window does not exist.
	DeleteFreeze(ctx context.Context, name string) (bool, error)
	ListFreezes(ctx context.Context) ([]FreezeRecord, error)
	// NotifyPolicy tells the other processes that policy or freezes changed (Postgres NOTIFY;
	// nothing on SQLite, where they poll).
	NotifyPolicy(ctx context.Context) error
	// ListenPolicy calls fn at every notification until ctx ends; on SQLite it returns at once.
	ListenPolicy(ctx context.Context, fn func()) error
}

var _ PolicyStore = (*SQL)(nil)

// policyChannel is the Postgres notification channel of policy changes.
const policyChannel = "manent_policy"

// policyTables returns the creation statements of the policy tables.
func (r *SQL) policyTables(postgres bool) []string {
	id := "revision INTEGER PRIMARY KEY AUTOINCREMENT"
	if postgres {
		id = "revision BIGSERIAL PRIMARY KEY"
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + r.policies + ` (
			` + id + `,
			sha256       TEXT NOT NULL,
			status       TEXT NOT NULL,
			author       TEXT NOT NULL,
			comment      TEXT NOT NULL,
			reviewed_by  TEXT NOT NULL,
			created_at   BIGINT NOT NULL,
			activated_at BIGINT,
			tests        TEXT NOT NULL,
			files        TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + r.policyActive + ` (
			id           INTEGER PRIMARY KEY,
			revision     BIGINT NOT NULL,
			activated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + r.freezes + ` (
			name       TEXT PRIMARY KEY,
			data       TEXT NOT NULL,
			created_by TEXT NOT NULL,
			created_at BIGINT NOT NULL)`,
	}
}

// CreatePolicy implements PolicyStore.
func (r *SQL) CreatePolicy(ctx context.Context, p PolicyRecord) (int64, error) {
	var rev int64
	err := r.db.QueryRowContext(ctx, `INSERT INTO `+r.policies+` (sha256, status, author, comment, reviewed_by, created_at, tests, files)
		VALUES ($1, $2, $3, $4, '', $5, $6, $7) RETURNING revision`,
		p.SHA256, policyDraft, p.Author, p.Comment, ms(p.CreatedAt), p.Tests, p.Files).Scan(&rev)
	return rev, err
}

// Policies implements PolicyStore.
func (r *SQL) Policies(ctx context.Context) ([]PolicyRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT revision, sha256, status, author, comment, reviewed_by, created_at, activated_at, tests
		FROM `+r.policies+` ORDER BY revision DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyRecord
	for rows.Next() {
		p, err := scanPolicy(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Policy implements PolicyStore.
func (r *SQL) Policy(ctx context.Context, revision int64) (*PolicyRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT revision, sha256, status, author, comment, reviewed_by, created_at, activated_at, tests, files
		FROM `+r.policies+` WHERE revision = $1`, revision)
	p, err := scanPolicy(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func scanPolicy(s interface{ Scan(...any) error }, files bool) (PolicyRecord, error) {
	var p PolicyRecord
	var created int64
	var activated sql.NullInt64
	dst := []any{&p.Revision, &p.SHA256, &p.Status, &p.Author, &p.Comment, &p.ReviewedBy, &created, &activated, &p.Tests}
	if files {
		dst = append(dst, &p.Files)
	}
	if err := s.Scan(dst...); err != nil {
		return p, err
	}
	p.CreatedAt, p.ActivatedAt = time.UnixMilli(created).UTC(), fromMs(activated)
	return p, nil
}

// ActivePolicy implements PolicyStore.
func (r *SQL) ActivePolicy(ctx context.Context) (int64, error) {
	var rev int64
	err := r.db.QueryRowContext(ctx, `SELECT revision FROM `+r.policyActive+` WHERE id = 1`).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// ActivatePolicy implements PolicyStore: pointer and states change in one transaction, and
// the pointer is replaced only if it still holds the expected revision.
func (r *SQL) ActivatePolicy(ctx context.Context, revision, expected int64, reviewer string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := ms(r.now())
	t := r.policyActiveBare
	res, err := tx.ExecContext(ctx, `INSERT INTO `+r.policyActive+` (id, revision, activated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET revision = excluded.revision, activated_at = excluded.activated_at
		WHERE `+t+`.revision = $3`, revision, now, expected)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err == nil {
			err = ErrPolicyConflict
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+r.policies+` SET status = $1 WHERE status = $2`, policySuperseded, policyActive); err != nil {
		return err
	}
	q := `UPDATE ` + r.policies + ` SET status = $1, activated_at = $2 WHERE revision = $3`
	args := []any{policyActive, now, revision}
	if reviewer != "" {
		q = `UPDATE ` + r.policies + ` SET status = $1, activated_at = $2, reviewed_by = $4 WHERE revision = $3`
		args = append(args, reviewer)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateFreeze implements PolicyStore.
func (r *SQL) CreateFreeze(ctx context.Context, f FreezeRecord) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO `+r.freezes+` (name, data, created_by, created_at) VALUES ($1, $2, $3, $4)`,
		f.Name, f.Data, f.CreatedBy, ms(f.CreatedAt))
	return err
}

// DeleteFreeze implements PolicyStore.
func (r *SQL) DeleteFreeze(ctx context.Context, name string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM `+r.freezes+` WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListFreezes implements PolicyStore.
func (r *SQL) ListFreezes(ctx context.Context) ([]FreezeRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, data, created_by, created_at FROM `+r.freezes+` ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FreezeRecord
	for rows.Next() {
		var f FreezeRecord
		var at int64
		if err := rows.Scan(&f.Name, &f.Data, &f.CreatedBy, &at); err != nil {
			return nil, err
		}
		f.CreatedAt = time.UnixMilli(at).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// NotifyPolicy implements PolicyStore.
func (r *SQL) NotifyPolicy(ctx context.Context) error {
	if !r.postgres {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `SELECT pg_notify($1, '')`, policyChannel)
	return err
}

// ListenPolicy implements PolicyStore with a dedicated connection, opened again after an
// error.
func (r *SQL) ListenPolicy(ctx context.Context, fn func()) error {
	if !r.postgres {
		return nil
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := r.listen(ctx, fn)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			backoff = min(backoff*2, time.Minute)
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
	return nil
}

func (r *SQL) listen(ctx context.Context, fn func()) error {
	conn, err := pgx.Connect(ctx, r.dsn)
	if err != nil {
		return fmt.Errorf("policy notifications: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN "+policyChannel); err != nil {
		return err
	}
	// A change made while not listening is picked up by the next poll; one call here
	// shortens the wait.
	fn()
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		fn()
	}
}
