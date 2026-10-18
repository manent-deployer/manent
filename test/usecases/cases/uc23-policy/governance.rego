# UC23: governance of the uc23-* applications in Rego; the other applications keep the rules
# of manent.yml (data.manent.rules), so the other use cases do not change.
package manent.governance

import rego.v1

import data.manent.lib
import data.manent.lib.rules

uc23 if startswith(input.application, "uc23-")

prod if lib.match_any(["prod", "waves"], input.environment)

default approvals := null

# In production one approval, only by the CAB: the admins do not approve uc23 deploys.
approvals := {"required": 1, "timeout": "1h", "approvers": {"groups": ["cab"]}} if {
	input.environment == "prod"
	input.deployment.kind != "rollback"
}

deny contains {"code": "downgrade", "message": sprintf("%s is older than %s, in service: use manent rollback", [input.artifact.version, input.current.version])} if {
	prod
	input.deployment.kind == "deploy"
	lib.semver_less(input.artifact.version, input.current.version)
}

notes contains "uc23: governance in Rego" if uc23

decision := {
	"rule": "uc23",
	"deny": deny,
	"approvals": approvals,
	"notes": notes,
} if uc23

decision := rules.decision if not uc23
