package plugin

import (
	"context"
	"errors"
)

// TrafficRequest sono i dati di un rilascio per percentuale di traffico (rollout con strategy:
// traffic): la nuova versione, la candidata, viene rilasciata accanto a quella in esercizio,
// la stabile, e il traffico si sposta per gradini.
type TrafficRequest struct {
	// State è lo stato registrato da PrepareSlots prima del rilascio (es. slot o versione
	// stabile di ogni target): ogni chiamata successiva lo riceve identico, anche dopo una
	// recovery. Non deve contenere segreti.
	State map[string]string `json:"state,omitempty"`
	// TesterHeader è l'header ("nome: valore") delle richieste dei tester.
	TesterHeader string `json:"tester_header,omitempty"`
	// TesterTag è l'etichetta della candidata per i router che danno un URL ai tester.
	TesterTag string `json:"tester_tag,omitempty"`
}

// StateHasStable è la chiave di TrafficRequest.State che PrepareSlots imposta a "true" se esiste
// una versione stabile; senza, il primo rilascio va direttamente al 100%.
const StateHasStable = "has_stable"

// SlotDeployer è un'interfaccia opzionale del Deployer: rilascia la nuova versione accanto a
// quella in esercizio, senza traffico, e la promuove o la ritira. Tutti i metodi devono essere
// idempotenti: possono essere rieseguiti da retry e recovery.
type SlotDeployer interface {
	// PrepareSlots legge lo stato dei target prima del rilascio (versione stabile, slot
	// libero). Non deve modificare nulla. L'esito arriva in Request.Traffic.State.
	PrepareSlots(ctx context.Context, req *Request) (map[string]string, error)
	// DeployCandidate rilascia la candidata senza spostare il traffico.
	DeployCandidate(ctx context.Context, req *Request) (*Result, error)
	// PromoteCandidate rende la candidata la release in esercizio (tutto il traffico e storico).
	PromoteCandidate(ctx context.Context, req *Request) error
	// RevertCandidate riporta tutto il traffico e lo storico alla versione stabile e ritira la
	// candidata, anche se era già stata promossa.
	RevertCandidate(ctx context.Context, req *Request) error
	// RetirePrevious ridimensiona la versione sostituita, dopo la promozione. Non deve toccare
	// la versione in esercizio, anche se nel frattempo un altro rilascio l'ha cambiata.
	RetirePrevious(ctx context.Context, req *Request) error
}

// TrafficRouter è un'interfaccia opzionale del Deployer: sposta il traffico tra la versione
// stabile e la candidata.
type TrafficRouter interface {
	// SetCandidateWeight manda alla candidata percent% del traffico, il resto alla stabile.
	SetCandidateWeight(ctx context.Context, req *Request, percent int) error
	// CandidateWeight legge la quota della candidata; -1 se i target non sono concordi.
	CandidateWeight(ctx context.Context, req *Request) (int, error)
}

// TesterRouter è un'interfaccia opzionale del TrafficRouter: instrada verso la candidata le
// sole richieste dei tester. Restituisce i dettagli da mostrare (es. l'URL dei tester).
type TesterRouter interface {
	RouteTesters(ctx context.Context, req *Request) (map[string]string, error)
}

// SlotScaler è un'interfaccia opzionale dello SlotDeployer: dimensiona la candidata in
// proporzione alla quota di traffico (rollout.traffic.replicas: proportional).
type SlotScaler interface {
	ScaleCandidate(ctx context.Context, req *Request, percent int) error
}

// TrafficSupport sono le interfacce dei rilasci per percentuale supportate da un plugin.
type TrafficSupport struct {
	Slots, Router, Testers, Scale bool
}

// TrafficChecker è implementato dai Deployer che conoscono il supporto ai rilasci per
// percentuale solo a runtime (es. un plugin esterno, che lo dichiara all'avvio del processo).
// I loro metodi di SlotDeployer, TrafficRouter, TesterRouter e SlotScaler restituiscono
// ErrTrafficUnsupported se il plugin non li implementa.
type TrafficChecker interface {
	TrafficSupported(ctx context.Context) (TrafficSupport, error)
}

// ErrTrafficUnsupported indica un plugin che non supporta un'operazione dei rilasci per percentuale.
var ErrTrafficUnsupported = errors.New("operazione dei rilasci per percentuale non supportata dal plugin")

// SupportsTraffic indica le interfacce dei rilasci per percentuale supportate dal Deployer.
func SupportsTraffic(ctx context.Context, d Deployer) (TrafficSupport, error) {
	if c, ok := d.(TrafficChecker); ok {
		return c.TrafficSupported(ctx)
	}
	var s TrafficSupport
	_, s.Slots = d.(SlotDeployer)
	_, s.Router = d.(TrafficRouter)
	_, s.Testers = d.(TesterRouter)
	_, s.Scale = d.(SlotScaler)
	return s, nil
}
