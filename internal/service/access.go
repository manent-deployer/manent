package service

import "context"

// AccessService descrive l'identità di chi chiama e cosa può fare: serve alle interfacce
// (UI, "manent whoami") per mostrare solo le azioni consentite. Il controllo vero resta nel
// servizio, che rifiuta comunque un'azione non permessa.
type AccessService interface {
	// Me restituisce l'identità della richiesta e i permessi che la riguardano.
	Me(ctx context.Context) (*Identity, error)
}

// Identity è l'identità di una richiesta con i permessi che le si applicano.
type Identity struct {
	// Subject è l'identificativo "<metodo>:<nome>" usato da RBAC e audit (es. "oidc:alice").
	Subject string   `json:"subject"`
	Name    string   `json:"name"`
	Method  string   `json:"method"`
	Groups  []string `json:"groups,omitempty"`
	// AuthEnabled è false con auth.mode none: tutto è consentito e l'identità è anonima.
	AuthEnabled bool `json:"auth_enabled"`
	// Grants sono i ruoli assegnati all'identità, con le applicazioni e gli ambienti su cui
	// valgono (pattern con "*").
	Grants []Grant `json:"grants"`
}

// Grant è un ruolo assegnato su un insieme di applicazioni e ambienti.
type Grant struct {
	Role         string   `json:"role"`
	Actions      []string `json:"actions"`
	Applications []string `json:"applications"`
	Environments []string `json:"environments"`
}
