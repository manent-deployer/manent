package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
)

// Inventory ricostruisce dallo storico dei deploy la release in esercizio in ogni
// applicazione/ambiente. Non considera i dry-run. Lo stato è tenuto in una cache del processo
// (inventoryCache): ogni chiamata legge solo i workflow nuovi e quelli non ancora conclusi.
func (e *Engine) Inventory(_ context.Context, f InventoryFilter) ([]InventoryEntry, error) {
	e.inv.mu.Lock()
	defer e.inv.mu.Unlock()
	if err := e.refreshInventory(); err != nil {
		return nil, err
	}
	out := []InventoryEntry{}
	for _, k := range slices.Sorted(maps.Keys(e.inv.entries)) {
		ie := *e.inv.entries[k]
		if (f.Application != "" && ie.Application != f.Application) || (f.Environment != "" && ie.Environment != f.Environment) {
			continue
		}
		ie.Drift = ie.LastFailure != nil && progressStarted(ie.LastFailure.RolloutProgress)
		if ie.Current != nil || ie.LastFailure != nil {
			out = append(out, ie)
		}
	}
	return out, nil
}

// inventoryRebuild è l'intervallo dopo il quale l'inventario viene ricostruito da zero: copre
// ciò che la lettura incrementale non vede, come i workflow eliminati dalla retention.
const inventoryRebuild = 10 * time.Minute

// inventoryCache è l'inventario del processo. Si ricostruisce scorrendo lo storico (al più
// maxListScan workflow) al primo uso e ogni inventoryRebuild; tra una ricostruzione e l'altra
// legge i workflow creati dopo l'ultimo letto (indice di created_at) e rilegge per ID quelli
// non ancora conclusi.
type inventoryCache struct {
	mu      sync.Mutex
	builtAt time.Time
	entries map[string]*InventoryEntry
	// mark è la data di creazione più recente letta; recent sono i workflow letti creati negli
	// ultimi inventoryOverlap prima di mark, da non rileggere.
	mark   time.Time
	recent map[string]time.Time
	// pending sono i workflow letti e non ancora conclusi.
	pending map[string]bool
}

// inventoryOverlap è il margine con cui si rileggono i workflow creati poco prima dell'ultimo
// letto: created_at è assegnato all'inserimento, quindi una transazione più lenta può rendere
// visibile un workflow con una data anteriore a quelli già letti.
const inventoryOverlap = 5 * time.Second

// refreshInventory aggiorna la cache; va chiamata con e.inv.mu bloccato.
func (e *Engine) refreshInventory() error {
	c := &e.inv
	if c.entries == nil || time.Since(c.builtAt) >= inventoryRebuild {
		return e.rebuildInventory()
	}
	names := dbos.WithFilterName(workflowName, approvalWorkflowName, afterWorkflowName)
	if len(c.pending) > 0 {
		ws, err := dbos.ListWorkflows(e.client, names, dbos.WithFilterWorkflowIDs(slices.Collect(maps.Keys(c.pending))...),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		for _, w := range ws {
			c.apply(w)
		}
	}
	const page = 500
	from := c.mark.Add(-inventoryOverlap)
	for {
		ws, err := dbos.ListWorkflows(e.client, names, dbos.WithFilterCreatedAfter(from), dbos.WithFilterLimit(page),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		fresh := 0
		for _, w := range ws {
			from = w.CreatedAt
			if _, ok := c.recent[w.ID]; ok {
				continue
			}
			fresh++
			c.see(w)
			c.apply(w)
		}
		if len(ws) < page {
			break
		}
		if fresh == 0 {
			// Una pagina intera già letta: si prosegue dal millisecondo successivo.
			from = from.Add(time.Millisecond)
		}
	}
	for id, at := range c.recent {
		if at.Before(c.mark.Add(-inventoryOverlap)) {
			delete(c.recent, id)
		}
	}
	return nil
}

// see registra un workflow letto per la lettura incrementale successiva.
func (c *inventoryCache) see(w dbos.WorkflowStatus) {
	if w.CreatedAt.After(c.mark) {
		c.mark = w.CreatedAt
	}
	if !w.CreatedAt.Before(c.mark.Add(-inventoryOverlap)) {
		c.recent[w.ID] = w.CreatedAt
	}
}

// rebuildInventory ricostruisce la cache scorrendo lo storico dal più recente.
func (e *Engine) rebuildInventory() error {
	c := &e.inv
	c.entries, c.pending, c.recent, c.mark = map[string]*InventoryEntry{}, map[string]bool{}, map[string]time.Time{}, time.Time{}
	err := e.scanRaw(nil, func(w dbos.WorkflowStatus) bool {
		c.see(w)
		c.apply(w)
		return true
	})
	if err != nil {
		c.entries = nil
		return err
	}
	c.builtAt = time.Now()
	return nil
}

// apply aggiorna l'inventario con un workflow letto: un deploy riuscito diventa la release in
// esercizio se è il più recente; un deploy fallito o annullato è l'ultimo fallimento se è più
// recente della release in esercizio. I workflow non conclusi restano da rileggere.
func (c *inventoryCache) apply(w dbos.WorkflowStatus) {
	delete(c.pending, w.ID)
	if superseded(w) {
		return
	}
	s := toStatus(w)
	if s.DryRun || s.Application == "" {
		return
	}
	if !Terminal(s.State) {
		c.pending[w.ID] = true
		return
	}
	k := s.Application + "/" + s.Environment
	ie := c.entries[k]
	if ie == nil {
		ie = &InventoryEntry{Application: s.Application, Environment: s.Environment}
		c.entries[k] = ie
	}
	switch s.State {
	case string(dbos.WorkflowStatusSuccess):
		if ie.Current == nil || s.CreatedAt.After(ie.Current.CreatedAt) {
			ie.Current = &s
			if ie.LastFailure != nil && !ie.LastFailure.CreatedAt.After(s.CreatedAt) {
				ie.LastFailure = nil
			}
		}
	case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusCancelled), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		if (ie.Current == nil || s.CreatedAt.After(ie.Current.CreatedAt)) &&
			(ie.LastFailure == nil || s.CreatedAt.After(ie.LastFailure.CreatedAt)) {
			ie.LastFailure = &s
		}
	}
}

// findRelease restituisce il deploy più recente concluso con successo (non dry-run)
// nell'applicazione/ambiente che soddisfa match, o nil.
func (e *Engine) findRelease(_ context.Context, application, environment string, match func(Status) bool) (*Status, error) {
	var found *Status
	err := e.scan(nil, func(s Status) bool {
		if s.Application == application && s.Environment == environment && !s.DryRun &&
			s.State == string(dbos.WorkflowStatusSuccess) && match(s) {
			found = &s
			return false
		}
		return true
	})
	return found, err
}

// loadRequest legge l'input di un deploy registrato.
func (e *Engine) loadRequest(id string) (Request, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return Request{}, err
	}
	if len(ws) == 0 {
		return Request{}, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	var req Request
	if err := decodeInput(ws[0].Input, &req); err != nil {
		return req, fmt.Errorf("input del deploy %s non leggibile: %w", id, err)
	}
	return req, nil
}

// applyRedeploy prepara il descrittore del nuovo deploy a partire da quello registrato.
func applyRedeploy(r Redeploy, d *descriptor.Descriptor, src *Status) {
	d.ID, d.DryRun, d.FreezeOverride, d.Schedule = r.ID, r.DryRun, r.FreezeOverride, nil
	// Le dipendenze valevano per il deploy di origine, già concluso.
	d.After, d.AfterTimeout = nil, ""
	if src.SHA256 != "" {
		// Viene rilasciato esattamente l'artefatto già rilasciato: lo step qa lo verifica.
		d.Artifact.SHA256 = src.SHA256
	}
}

// Promote rilascia in To lo stesso artefatto (nome, versione e sha256) rilasciato in From, con
// il descrittore del deploy di origine. I target sono propri di ogni ambiente e non vengono
// copiati. La promozione è un deploy come gli altri: valgono policy, approvazioni e freeze.
func (e *Engine) Promote(ctx context.Context, r PromoteRequest) (*Submission, error) {
	if r.Application == "" || r.From == "" || r.To == "" || r.From == r.To {
		return nil, fmt.Errorf("%w: application, from e to obbligatori, from diverso da to", ErrInvalidRequest)
	}
	src, err := e.findRelease(ctx, r.Application, r.From, func(s Status) bool { return r.Version == "" || s.Version == r.Version })
	if err != nil {
		return nil, err
	}
	if src == nil {
		v := cmp.Or(r.Version, "alcuna versione")
		return nil, fmt.Errorf("%w: %s: %s non risulta rilasciata con successo in %s", ErrInvalidRequest, r.Application, v, r.From)
	}
	req, err := e.loadRequest(src.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	d.Environment = r.To
	d.Deploy.Targets = nil
	if r.Deploy != nil {
		d.Deploy = *r.Deploy
	}
	applyRedeploy(r.Redeploy, &d, src)
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return e.submit(ctx, &d, submitMeta{kind: KindPromotion, source: src.ID})
}

// Rollback rilascia di nuovo, con il suo descrittore, una versione già rilasciata con successo
// nell'ambiente: di default la più recente diversa da quella in esercizio. È ammesso solo per
// i tipi di artefatto per cui la policy di rollback lo consente (non per le migration).
//
// Se dopo l'ultimo deploy riuscito un deploy è fallito lasciando i target modificati (senza
// rollback automatico completato, o con un rollout interrotto dopo il primo stadio), la
// versione dell'ultimo deploy riuscito non è più garantita sui target: il rollback di default
// la ripristina, e la si può indicare con --to.
func (e *Engine) Rollback(ctx context.Context, r RollbackRequest) (*Submission, error) {
	if r.Application == "" || r.Environment == "" {
		return nil, fmt.Errorf("%w: application ed environment obbligatori", ErrInvalidRequest)
	}
	var current, target, failed *Status
	err := e.scan(nil, func(s Status) bool {
		if s.Application != r.Application || s.Environment != r.Environment || s.DryRun {
			return true
		}
		if s.State != string(dbos.WorkflowStatusSuccess) {
			if current == nil && failed == nil && leftChanged(s) {
				failed = &s
			}
			return true
		}
		if current == nil {
			current = &s
			if failed != nil && (r.Version == "" || r.Version == s.Version) {
				target = &s
				return false
			}
			return true
		}
		if s.Version != current.Version && (r.Version == "" || s.Version == r.Version) {
			target = &s
			return false
		}
		return true
	})
	switch {
	case err != nil:
		return nil, err
	case current == nil:
		return nil, fmt.Errorf("%w: nessun deploy riuscito di %s in %s", ErrInvalidRequest, r.Application, r.Environment)
	case target == nil && r.Version == current.Version:
		return nil, fmt.Errorf("%w: la versione %s è già in esercizio in %s", ErrInvalidRequest, r.Version, r.Environment)
	case target == nil:
		v := cmp.Or(r.Version, "una versione precedente a "+current.Version)
		return nil, fmt.Errorf("%w: %s non risulta rilasciata con successo in %s", ErrInvalidRequest, v, r.Environment)
	}
	req, err := e.loadRequest(target.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	if e.rollback != nil {
		if err := e.rollback.Allowed(d.Artifact.Type); err != nil {
			return nil, fmt.Errorf("%w: rollback manuale non ammesso: %w", ErrInvalidRequest, err)
		}
	}
	applyRedeploy(r.Redeploy, &d, target)
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return e.submit(ctx, &d, submitMeta{kind: KindRollback, source: target.ID})
}

// leftChanged dice se un deploy concluso senza successo può aver lasciato i target su una
// release diversa da quella dell'ultimo deploy riuscito: fallito senza rollback automatico o
// con un rollback fallito, oppure rollout interrotto dopo almeno uno stadio. Un rollback
// skipped significa che non c'era nulla da compensare; un deploy annullato prima di partire
// (es. pianificato) non conta.
func leftChanged(s Status) bool {
	switch s.State {
	case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		return (s.Rollback != RollbackCompleted && s.Rollback != RollbackSkipped) || progressStarted(s.RolloutProgress)
	case string(dbos.WorkflowStatusCancelled):
		return progressStarted(s.RolloutProgress)
	}
	return false
}

// Freezes elenca le finestre di freeze attive e future per l'applicazione e l'ambiente.
func (e *Engine) Freezes(_ context.Context, application, environment string) ([]governance.WindowStatus, error) {
	if e.governance == nil {
		return []governance.WindowStatus{}, nil
	}
	return e.governance.Windows(application, environment, e.now()), nil
}
