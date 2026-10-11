// Package authz applica i permessi (RBAC) per applicazione e ambiente. Guard avvolge il
// servizio dell'engine, quindi le stesse regole valgono per l'API e per la CLI.
package authz

import (
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/glob"
)

// Action è un'operazione soggetta a permesso.
type Action string

// Azioni disponibili.
const (
	DeploymentRead   Action = "deployment.read"
	DeploymentSubmit Action = "deployment.submit"
	DeploymentDryRun Action = "deployment.dry-run"
	DeploymentCancel Action = "deployment.cancel"
	// DeploymentRollback consente il rollback manuale a una versione precedente.
	DeploymentRollback Action = "deployment.rollback"
	// DeploymentApprove consente di approvare o respingere un deploy.
	DeploymentApprove Action = "deployment.approve"
	// FreezeOverride consente un deploy durante una finestra di freeze.
	FreezeOverride Action = "deployment.freeze-override"
	ScheduleRead   Action = "schedule.read"
	ScheduleWrite  Action = "schedule.write"
	// All vale per tutte le azioni.
	All Action = "*"
)

// Actions elenca le azioni valide nella definizione di un ruolo.
var Actions = []Action{
	DeploymentRead, DeploymentSubmit, DeploymentDryRun, DeploymentCancel, DeploymentRollback,
	DeploymentApprove, FreezeOverride, ScheduleRead, ScheduleWrite, All,
}

// BuiltinRoles sono i ruoli predefiniti; la configurazione può aggiungerne altri.
// Approvazioni e deroghe al freeze non sono concesse da deployer e operator: chi rilascia non
// deve potersi approvare da solo con un altro ruolo.
var BuiltinRoles = map[string][]Action{
	"viewer":   {DeploymentRead, ScheduleRead},
	"tester":   {DeploymentRead, ScheduleRead, DeploymentDryRun},
	"approver": {DeploymentRead, ScheduleRead, DeploymentApprove},
	"deployer": {DeploymentRead, ScheduleRead, DeploymentDryRun, DeploymentSubmit, DeploymentCancel, DeploymentRollback},
	"operator": {DeploymentRead, ScheduleRead, DeploymentDryRun, DeploymentSubmit, DeploymentCancel, DeploymentRollback, ScheduleWrite},
	"admin":    {All},
}

// Binding assegna un ruolo a utenti o gruppi su un insieme di applicazioni e ambienti.
// Tutti i campi accettano pattern con "*".
type Binding struct {
	// Subjects sono identità nella forma "<metodo>:<nome>" (es. "oidc:alice", "token:ci", "cli:*").
	Subjects []string `yaml:"subjects"`
	// Groups sono gruppi OIDC o dei token statici.
	Groups       []string `yaml:"groups"`
	Role         string   `yaml:"role"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
}

// Policy decide se un'identità può eseguire un'azione su un'applicazione/ambiente.
type Policy struct {
	// Disabled concede tutto (autenticazione disattivata).
	Disabled bool
	Roles    map[string][]Action
	Bindings []Binding
}

// NewPolicy unisce i ruoli predefiniti a quelli configurati e verifica i binding.
func NewPolicy(roles map[string][]Action, bindings []Binding) (Policy, error) {
	p := Policy{Roles: maps.Clone(BuiltinRoles), Bindings: bindings}
	for name, acts := range roles {
		if _, ok := BuiltinRoles[name]; ok {
			return p, fmt.Errorf("rbac.roles: il ruolo %q è predefinito e non si può ridefinire", name)
		}
		for _, a := range acts {
			if !slices.Contains(Actions, a) {
				return p, fmt.Errorf("rbac.roles.%s: azione %q sconosciuta", name, a)
			}
		}
		p.Roles[name] = acts
	}
	for i, b := range bindings {
		if _, ok := p.Roles[b.Role]; !ok {
			return p, fmt.Errorf("rbac.bindings[%d]: ruolo %q sconosciuto", i, b.Role)
		}
		if len(b.Subjects) == 0 && len(b.Groups) == 0 {
			return p, fmt.Errorf("rbac.bindings[%d]: indicare subjects o groups", i)
		}
		if len(b.Applications) == 0 || len(b.Environments) == 0 {
			return p, fmt.Errorf("rbac.bindings[%d]: applications ed environments obbligatori (\"*\" per tutti)", i)
		}
	}
	return p, nil
}

// Allowed indica se p può eseguire a sull'applicazione e sull'ambiente indicati.
// Senza identità l'esito è sempre negativo, salvo policy disattivata.
func (pol Policy) Allowed(p *auth.Principal, a Action, application, environment string) bool {
	if pol.Disabled {
		return true
	}
	if p == nil {
		return false
	}
	for _, b := range pol.Bindings {
		if !pol.grants(b.Role, a) || !b.matches(p) {
			continue
		}
		if glob.Any(b.Applications, application) && glob.Any(b.Environments, environment) {
			return true
		}
	}
	return false
}

func (pol Policy) grants(role string, a Action) bool {
	acts := pol.Roles[role]
	return slices.Contains(acts, All) || slices.Contains(acts, a)
}

// matches indica se il binding si applica all'identità.
func (b Binding) matches(p *auth.Principal) bool {
	if glob.Any(b.Subjects, p.String()) {
		return true
	}
	for _, g := range p.Groups {
		if glob.Any(b.Groups, g) {
			return true
		}
	}
	return false
}
