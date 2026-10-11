// Package testdb fornisce ai test un database di sistema DBOS isolato: SQLite in una
// directory temporanea oppure, se MANENT_TEST_DATABASE_URL indica un server Postgres, un
// database nuovo creato per il singolo test ed eliminato alla fine.
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

// EnvURL è la variabile con l'URL di un server Postgres per i test (es.
// postgres://postgres:postgres@localhost:5432/postgres). L'utente deve poter creare database.
const EnvURL = "MANENT_TEST_DATABASE_URL"

// Postgres indica se i test usano Postgres.
func Postgres() bool { return os.Getenv(EnvURL) != "" }

// URL restituisce l'URL di un database vuoto riservato al test.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		return "sqlite:" + filepath.Join(t.TempDir(), "dbos.db")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s non valido: %v", EnvURL, err)
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
		t.Fatalf("creazione del database di test: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer db.Close()
		// Le connessioni di DBOS possono essere ancora aperte: FORCE le chiude (Postgres 13+).
		_, _ = db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}
