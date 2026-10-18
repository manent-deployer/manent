package manent.governance_test

import rego.v1

import data.manent.governance

base := {"application": "uc23-api", "environment": "prod", "now": "2026-10-19T10:00:00Z", "deployment": {"kind": "deploy"}, "artifact": {"version": "2.0.0"}}

test_prod_needs_the_cab if {
	d := governance.decision with input as base
	d.approvals.approvers.groups == ["cab"]
}

test_downgrade_denied if {
	d := governance.decision with input as object.union(base, {"artifact": {"version": "1.0.0"}, "current": {"version": "1.1.0"}})
	count(d.deny) == 1
}

test_other_applications_keep_the_rules if {
	d := governance.decision with input as object.union(base, {"application": "uc2-api"})
	d.rule == "uc2-produzione"
}
