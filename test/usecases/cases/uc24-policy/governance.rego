# UC24: the tenants with the 2025 contract stay on the 1.0 series of their applications;
# everything else follows the rules of manent.yml.
package manent.governance

import rego.v1

import data.manent.lib.rules

deny contains {"code": "contract", "message": sprintf("tenant %s has the 2025 contract: only 1.0.x", [input.tenant.name])} if {
	input.tenant.tags.contract == "2025"
	not startswith(input.artifact.version, "1.0.")
}

decision := object.union(rules.decision, {"deny": deny}) if count(deny) > 0

decision := rules.decision if count(deny) == 0
