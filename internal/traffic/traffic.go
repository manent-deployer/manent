// Package traffic integra manent con i load balancer e i router del traffico: svuota i target
// da un pool prima di aggiornarli e li rimette in servizio dopo (Pool), e sposta il traffico
// tra due gruppi di target, gli slot, nei rilasci per percentuale (Router).
//
// I provider che parlano solo HTTP o SSH stanno qui; quelli che usano un SDK (es. AWS) stanno
// tra i plugin e si registrano in Providers da internal/app.
package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// Stati di un membro del pool.
const (
	StateActive   = "active"
	StateDraining = "draining"
	StateDisabled = "disabled"
)

// Member è lo stato di un membro del pool.
type Member struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Healthy bool   `json:"healthy"`
}

// InService indica un membro attivo e sano, che riceve traffico.
func (m Member) InService() bool { return m.State == StateActive && m.Healthy }

// Pool è il pool di un load balancer. Tutti i metodi sono idempotenti: possono essere ripetuti
// da retry e recovery.
type Pool interface {
	// Members restituisce lo stato di tutti i membri del pool, per il controllo di capacità;
	// nil se il provider non lo conosce.
	Members(ctx context.Context) ([]Member, error)
	// Drain smette di mandare nuove richieste ai membri; quelle in corso proseguono.
	Drain(ctx context.Context, members []string) error
	// Connections restituisce le connessioni ancora attive di ogni membro.
	Connections(ctx context.Context, members []string) (map[string]int, error)
	// Restore rimette i membri in servizio.
	Restore(ctx context.Context, members []string) error
	// Healthy indica, per ogni membro, se è in servizio e sano per il load balancer.
	Healthy(ctx context.Context, members []string) (map[string]bool, error)
}

// DrainDelayer è implementato dai pool che non sanno contare le connessioni: dopo Drain si
// attende sempre almeno DrainDelay (es. il tempo che il load balancer impiega ad accorgersi
// di un health check rosso).
type DrainDelayer interface {
	DrainDelay() time.Duration
}

// Router sposta il traffico tra gli slot: ogni slot è il gruppo di target con l'etichetta slot.
type Router interface {
	// Weights restituisce la quota di traffico di ogni slot (0-100).
	Weights(ctx context.Context) (map[string]int, error)
	// SetWeights imposta le quote; idempotente.
	SetWeights(ctx context.Context, weights map[string]int) error
}

// TesterRouter è implementato dai router che sanno instradare verso uno slot le sole
// richieste dei tester.
type TesterRouter interface {
	RouteTesters(ctx context.Context, slot string) (map[string]string, error)
}

// Env è ciò che serve per costruire un provider.
type Env struct {
	// Settings sono le impostazioni del provider (traffic.pool.settings o traffic.router.settings).
	Settings map[string]string
	// Member calcola il nome del membro di un target.
	Member func(descriptor.Target) string
	// Targets sono tutti i target del deploy (per i provider che lavorano sui target).
	Targets []descriptor.Target
	// Secrets risolve i segreti per nome, al momento dell'uso.
	Secrets plugin.SecretResolver
	// DeploymentID identifica il deploy nelle chiamate (es. nome della sessione STS).
	DeploymentID string
	// Dialer apre le sessioni SSH (provider basati sui target).
	Dialer  Dialer
	Logger  *slog.Logger
	Timeout time.Duration
}

// PoolFactory costruisce un pool; ValidatePool ne verifica le impostazioni senza effetti.
type (
	PoolFactory   func(ctx context.Context, env Env) (Pool, error)
	RouterFactory func(ctx context.Context, env Env) (Router, error)
	Validator     func(settings map[string]string) error
)

// Provider è un provider registrato: costruttore e verifica delle impostazioni.
type Provider[F any] struct {
	New      F
	Validate Validator
}

// Providers sono i provider disponibili, per nome.
type Providers struct {
	Pools   map[string]Provider[PoolFactory]
	Routers map[string]Provider[RouterFactory]
}

// Default restituisce i provider di questo package; quelli basati su un SDK si aggiungono da
// internal/app.
func Default() *Providers {
	return &Providers{
		Pools: map[string]Provider[PoolFactory]{
			"haproxy":     {New: NewHAProxyPool, Validate: validateHAProxy},
			"health-flag": {New: NewHealthFlagPool, Validate: validateHealthFlag},
			"f5":          {New: NewF5Pool, Validate: validateF5},
			"nginx-plus":  {New: NewNginxPlusPool, Validate: validateNginxPlus},
			"nginx":       {New: NewNginxPool, Validate: validateNginx},
		},
		Routers: map[string]Provider[RouterFactory]{
			"haproxy":    {New: NewHAProxyRouter, Validate: validateHAProxy},
			"f5":         {New: NewF5Router, Validate: validateF5},
			"nginx-plus": {New: NewNginxPlusRouter, Validate: validateNginxPlus},
		},
	}
}

// Check verifica la configurazione di traffico di un ambiente: provider noti e impostazioni
// valide. Non contatta i sistemi.
func (p *Providers) Check(c *descriptor.TrafficConfig) error {
	if c == nil {
		return nil
	}
	var errs []error
	if c.Pool != nil {
		pr, ok := p.Pools[c.Pool.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("traffic.pool.provider %q sconosciuto (%s)", c.Pool.Provider, strings.Join(slices.Sorted(maps.Keys(p.Pools)), ", ")))
		} else if pr.Validate != nil {
			if err := pr.Validate(c.Pool.Settings); err != nil {
				errs = append(errs, fmt.Errorf("traffic.pool (%s): %w", c.Pool.Provider, err))
			}
		}
	}
	if c.Router != nil {
		pr, ok := p.Routers[c.Router.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("traffic.router.provider %q sconosciuto (%s)", c.Router.Provider, strings.Join(slices.Sorted(maps.Keys(p.Routers)), ", ")))
		} else if pr.Validate != nil {
			if err := pr.Validate(c.Router.Settings); err != nil {
				errs = append(errs, fmt.Errorf("traffic.router (%s): %w", c.Router.Provider, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Pool costruisce il pool dell'ambiente.
func (p *Providers) Pool(ctx context.Context, c *descriptor.PoolConfig, env Env) (Pool, error) {
	pr, ok := p.Pools[c.Provider]
	if !ok {
		return nil, fmt.Errorf("traffic.pool.provider %q sconosciuto", c.Provider)
	}
	env.Settings = c.Settings
	env.Member = c.MemberName
	return pr.New(ctx, env)
}

// Router costruisce il router dell'ambiente.
func (p *Providers) Router(ctx context.Context, c *descriptor.RouterConfig, env Env) (Router, error) {
	pr, ok := p.Routers[c.Provider]
	if !ok {
		return nil, fmt.Errorf("traffic.router.provider %q sconosciuto", c.Provider)
	}
	env.Settings = c.Settings
	// Il nome del server di un target nei router dei load balancer (impostazione member,
	// default "{host}").
	tmpl := c.Settings["member"]
	env.Member = func(t descriptor.Target) string { return descriptor.ExpandTarget(tmpl, t) }
	return pr.New(ctx, env)
}

// Capacity verifica che, togliendo i membri indicati, restino in servizio almeno minHealthy
// membri sani. I membri già fuori servizio non contano.
func Capacity(all []Member, drain []string, minHealthy int) error {
	in := 0
	for _, m := range all {
		if m.InService() && !slices.Contains(drain, m.Name) {
			in++
		}
	}
	if in < minHealthy {
		return fmt.Errorf("capacità insufficiente: svuotando %s resterebbero in servizio %d membri sani su %d, minimo %d",
			strings.Join(drain, ", "), in, len(all), minHealthy)
	}
	return nil
}

// Missing restituisce i membri indicati che il pool non contiene.
func Missing(all []Member, names []string) []string {
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(all, func(m Member) bool { return m.Name == n }) {
			out = append(out, n)
		}
	}
	return out
}

// httpClient costruisce il client HTTP dei provider: TLS 1.2+, CA dedicata (ca_file),
// nessun redirect.
func httpClient(settings map[string]string, timeout time.Duration) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if f := settings["ca_file"]; f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: nessun certificato", f)
		}
		tc.RootCAs = pool
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// checkURL verifica un URL di un'API di gestione: https, salvo allow_http.
func checkURL(settings map[string]string, key, u string) error {
	switch {
	case strings.HasPrefix(u, "https://"):
		return nil
	case strings.HasPrefix(u, "http://") && settings["allow_http"] == "true":
		return nil
	}
	return fmt.Errorf("%s %q: serve un URL https (http solo con allow_http: \"true\")", key, u)
}

// splitList legge una lista separata da virgole.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
