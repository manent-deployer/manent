package authz

import (
	"context"
	"sync"
)

// scopeKey è la chiave del context per deploymentScope.
type scopeKey struct{}

// deploymentScope ricorda applicazione e ambiente dei deploy già letti con Status nello stesso
// context. Applicazione e ambiente di un deploy non cambiano: le letture successive (log) dello
// stesso deploy verificano il permesso senza rileggere lo stato dal database.
type deploymentScope struct {
	mu sync.Mutex
	m  map[string]target
}

// WithDeploymentScope restituisce un context in cui il Guard ricorda i deploy letti. Serve a
// chi legge ripetutamente lo stesso deploy, come lo stream SSE: il permesso viene comunque
// verificato a ogni lettura, sulla policy corrente.
func WithDeploymentScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &deploymentScope{m: map[string]target{}})
}

// remember registra il deploy letto, se il context ha uno scope.
func remember(ctx context.Context, id, application, environment string) {
	if s, ok := ctx.Value(scopeKey{}).(*deploymentScope); ok {
		s.mu.Lock()
		s.m[id] = target{application: application, environment: environment, deployment: id}
		s.mu.Unlock()
	}
}

// recall restituisce il deploy già letto nello scope del context.
func recall(ctx context.Context, id string) (target, bool) {
	s, ok := ctx.Value(scopeKey{}).(*deploymentScope)
	if !ok {
		return target{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[id]
	return t, ok
}
