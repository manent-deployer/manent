package app

import (
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/testdb"
)

// Un'API avviata prima dei worker su un database nuovo crea lo schema DBOS invece di fallire.
func TestNewClientSuDatabaseNuovo(t *testing.T) {
	cfg := config.Default()
	cfg.DBOS.DatabaseURL = testdb.URL(t)
	cfg.App.Name = "manent-test"
	a, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := dbos.ListWorkflows(a.dbosClient(), dbos.WithFilterLimit(1)); err != nil {
		t.Fatal(err)
	}
}
