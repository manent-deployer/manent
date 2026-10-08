// Package plugin definisce il contratto pubblico che ogni plugin di deploy deve rispettare.
// I team che sviluppano nuovi plugin dipendono solo da questo package.
package plugin

import (
	"context"
	"log/slog"
)

// Deployer è l'interfaccia che ogni plugin di deploy deve implementare.
type Deployer interface {
	// Name restituisce il nome univoco del plugin (es. "remote-copy").
	Name() string
	// Supports indica se il plugin gestisce il tipo di artefatto indicato.
	Supports(artifactType string) bool
	// Validate verifica la richiesta prima del deploy, senza effetti collaterali.
	Validate(req *Request) error
	// Deploy esegue il deploy. Deve essere idempotente: in caso di recovery può essere rieseguito.
	Deploy(ctx context.Context, req *Request) (*Result, error)
}

// Factory costruisce un Deployer a partire dalla sua configurazione applicativa.
type Factory func(cfg map[string]any, logger *slog.Logger) (Deployer, error)

// SecretResolver permette al plugin di leggere un segreto per nome solo quando serve,
// così il valore non viene mai persistito negli output degli step.
type SecretResolver interface {
	Secret(ctx context.Context, name string) (string, error)
}

// Artifact descrive l'artefatto già recuperato e disponibile in locale.
type Artifact struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	LocalPath string `json:"local_path"`
	SHA256    string `json:"sha256"`
}

// Target identifica una destinazione del deploy.
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
}

// Request contiene tutto ciò che serve al plugin per eseguire il deploy.
type Request struct {
	DeploymentID string
	Application  string
	Environment  string
	Artifact     Artifact
	Targets      []Target
	// Params sono parametri specifici del plugin, presi dal descrittore.
	Params  map[string]string
	Secrets SecretResolver
}

// TargetResult è l'esito del deploy su un singolo target.
type TargetResult struct {
	Host    string            `json:"host"`
	Status  string            `json:"status"`
	Details map[string]string `json:"details,omitempty"`
}

// Result è l'esito complessivo restituito dal plugin. Deve essere serializzabile in JSON.
type Result struct {
	Plugin  string         `json:"plugin"`
	Targets []TargetResult `json:"targets"`
}

// Stati possibili di un TargetResult.
const (
	StatusDeployed = "deployed"
	StatusFailed   = "failed"
)
