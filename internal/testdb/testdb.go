// Package testdb gives tests an isolated DBOS system database: SQLite in a temporary
// directory or, if MANENT_TEST_DATABASE_URL points to a Postgres server, a new database
// created for the single test and dropped at the end.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvURL is the variable with the URL of a Postgres server for tests (e.g.
// postgres://postgres:postgres@localhost:5432/postgres). The user must be able to create databases.
const EnvURL = "MANENT_TEST_DATABASE_URL"

// Postgres reports whether tests use Postgres.
func Postgres() bool { return os.Getenv(EnvURL) != "" }

// URL returns the URL of an empty database reserved for the test.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		return "sqlite:" + filepath.Join(t.TempDir(), "dbos.db")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("invalid %s: %v", EnvURL, err)
	}
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "manent_test_" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating the test database: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer db.Close()
		// DBOS connections may still be open: FORCE closes them (Postgres 13+).
		_, _ = db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}
