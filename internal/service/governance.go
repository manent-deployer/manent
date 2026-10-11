package service

import (
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// Esiti di un'approvazione.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// Decision è la decisione di un approvatore.
type Decision struct {
	Approver string    `json:"approver"`
	Approve  bool      `json:"approve"`
	Comment  string    `json:"comment,omitempty"`
	Time     time.Time `json:"time"`
}

// DecisionRequest è la decisione richiesta tramite CLI o API: l'approvatore è l'identità
// nel context.
type DecisionRequest struct {
	Approve bool   `json:"approve"`
	Comment string `json:"comment,omitempty"`
}

// ApprovalState è lo stato di un'approvazione.
type ApprovalState struct {
	Status    string     `json:"status"`
	Required  int        `json:"required"`
	AllowSelf bool       `json:"allow_self,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	Decisions []Decision `json:"decisions,omitempty"`
}

// InventoryFilter filtra l'inventario.
type InventoryFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// InventoryEntry è lo stato di un'applicazione in un ambiente.
type InventoryEntry struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	// Current è l'ultimo deploy concluso con successo.
	Current *Status `json:"current,omitempty"`
	// LastFailure è l'ultimo deploy fallito o annullato, se successivo a Current: senza un
	// rollback completato l'ambiente potrebbe non essere nello stato di Current.
	LastFailure *Status `json:"last_failure,omitempty"`
	// Drift indica un rollout fermato dopo aver aggiornato una parte delle region o AZ:
	// l'ambiente ha versioni diverse (dettaglio in LastFailure.RolloutProgress).
	Drift bool `json:"drift,omitempty"`
}

// Redeploy sono le opzioni comuni a promozioni e rollback manuali.
type Redeploy struct {
	// ID opzionale del nuovo deploy (chiave di idempotenza).
	ID     string `json:"id,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// FreezeOverride, se presente, è il motivo della deroga a un freeze attivo.
	FreezeOverride string `json:"freeze_override,omitempty"`
}

// PromoteRequest chiede di rilasciare in To l'artefatto già rilasciato con successo in From.
type PromoteRequest struct {
	Application string `json:"application"`
	From        string `json:"from"`
	To          string `json:"to"`
	// Version, se indicata, sceglie la versione da promuovere; default quella in esercizio in From.
	Version string `json:"version,omitempty"`
	// Deploy sostituisce la sezione deploy del descrittore di origine; senza, i target di
	// origine non vengono copiati e arrivano dalla configurazione di ambiente di To.
	Deploy *descriptor.Deploy `json:"deploy,omitempty"`
	Redeploy
}

// RollbackRequest chiede di tornare a una versione rilasciata in precedenza.
type RollbackRequest struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	// Version, se indicata, è la versione a cui tornare; default la precedente a quella in esercizio.
	Version string `json:"version,omitempty"`
	Redeploy
}
