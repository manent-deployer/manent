package plugin

import (
	"context"
	"errors"
	"fmt"
)

// Release è una versione dell'applicazione in esercizio su un ambiente.
// Viene persistita da DBOS: Details non deve contenere segreti.
type Release struct {
	Version string            `json:"version"`
	Details map[string]string `json:"details,omitempty"`
}

// Rollbacker è un'interfaccia opzionale del Deployer: riporta i target alla release indicata,
// quella in esercizio prima del deploy. Viene chiamato quando un deploy fallisce e la policy
// di rollback lo prevede. Deve essere idempotente: può essere rieseguito da retry o recovery,
// anche su target che il deploy non aveva ancora modificato.
type Rollbacker interface {
	Rollback(ctx context.Context, req *Request, previous Release) (*Result, error)
}

// RollbackChecker è implementato dai Deployer che conoscono il supporto al rollback solo a
// runtime (es. un plugin esterno, che lo dichiara all'avvio del processo).
type RollbackChecker interface {
	RollbackSupported(ctx context.Context) (bool, error)
}

// ErrRollbackUnsupported indica un plugin che non supporta il rollback.
var ErrRollbackUnsupported = errors.New("rollback non supportato dal plugin")

// CanRollback indica se il Deployer supporta il rollback.
func CanRollback(ctx context.Context, d Deployer) (bool, error) {
	if c, ok := d.(RollbackChecker); ok {
		return c.RollbackSupported(ctx)
	}
	_, ok := d.(Rollbacker)
	return ok, nil
}

// ReleaseInspector è un'interfaccia opzionale del Deployer: legge dai target la release
// attualmente in esercizio, senza modificarli. Restituisce nil se non ce n'è nessuna
// (primo deploy). È usata dal locator "plugin".
type ReleaseInspector interface {
	CurrentRelease(ctx context.Context, req *Request) (*Release, error)
}

// ReleaseLocator individua, prima del deploy, la release in esercizio a cui tornare in caso
// di rollback. Le implementazioni sono strategie diverse: leggerla dai target tramite il plugin
// (PluginLocator) o chiederla a un sistema aziendale che registra i rilasci in esercizio.
// Restituisce nil se non esiste una release precedente.
type ReleaseLocator interface {
	Current(ctx context.Context, dep Deployer, req *Request) (*Release, error)
}

// PluginLocatorName è il nome del locator che interroga il plugin di deploy.
const PluginLocatorName = "plugin"

// PluginLocator legge la release in esercizio dai target tramite il ReleaseInspector del plugin.
type PluginLocator struct{}

// Current delega al plugin; errore se il plugin non sa leggere la release in esercizio.
func (PluginLocator) Current(ctx context.Context, dep Deployer, req *Request) (*Release, error) {
	ins, ok := dep.(ReleaseInspector)
	if !ok {
		return nil, fmt.Errorf("il plugin %q non sa leggere la release in esercizio: usare un altro locator", dep.Name())
	}
	return ins.CurrentRelease(ctx, req)
}
