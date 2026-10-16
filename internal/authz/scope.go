package authz

import (
	"context"
	"sync"
)

// scopeKey is the context key of deploymentScope.
type scopeKey struct{}

// deploymentScope remembers application and environment of the deploys already read with
// Status in the same context. A deploy's application and environment never change: later
// reads (logs) of the same deploy check the permission without reading the status again.
type deploymentScope struct {
	mu sync.Mutex
	m  map[string]target
}

// WithDeploymentScope returns a context in which the Guard remembers the deploys read. It is
// for whoever reads the same deploy repeatedly, like the SSE stream: the permission is still
// checked at every read, against the current policy.
func WithDeploymentScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &deploymentScope{m: map[string]target{}})
}

// remember records the deploy read, if the context has a scope.
func remember(ctx context.Context, id, application, environment string) {
	if s, ok := ctx.Value(scopeKey{}).(*deploymentScope); ok {
		s.mu.Lock()
		s.m[id] = target{application: application, environment: environment, deployment: id}
		s.mu.Unlock()
	}
}

// recall returns the deploy already read in the context's scope.
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
