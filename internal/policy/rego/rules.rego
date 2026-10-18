# The governance rules of manent.yml as data: data.manent.rules has the schema of
# governance.rules (durations as strings, e.g. "72h"). The first rule matching application
# and environment wins; without a match there are no obligations.
package manent.lib.rules

import rego.v1

import data.manent.lib

default_timeout := "72h"

matching contains i if {
	some i, r in data.manent.rules
	lib.match_any(r.applications, input.application)
	lib.match_any(r.environments, input.environment)
}

rule := data.manent.rules[min(matching)] if count(matching) > 0

default approvals := null

approvals := object.union({"timeout": default_timeout}, rule.approvals) if {
	rule.approvals.required > 0
}

default change := null

change := object.union({"timeout": default_timeout}, rule.change) if rule.change

default decision := {}

decision := {
	"rule": object.get(rule, "name", ""),
	"requires": object.get(rule, "requires", []),
	"approvals": approvals,
	"change": change,
} if rule
