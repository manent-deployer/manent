# UC25: permissions in Rego. The bindings of manent.yml stay (data.manent.rbac); the team owns
# uc25 and the applications of the tenants tagged with its name, the on-call shifts
# (oncall/data.json, written by the case) expire with until, and contractors never change
# production.
package manent.authz

import rego.v1

import data.manent.lib.authz

grants := authz.bindings | team | owners | oncall

team contains {"name": "team-uc25", "role": "deployer", "applications": ["uc25/uc25-*"], "environments": ["*"]} if {
	"team-uc25" in input.identity.groups
}

owners contains g if {
	some group in input.identity.groups
	startswith(group, "team-")
	g := {
		"name": sprintf("owner %s", [group]),
		"role": "deployer",
		"applications": ["*/uc24-*"],
		"environments": ["*"],
		"tenants": sprintf("owner=%s", [group]),
	}
}

oncall contains g if {
	some shift in data.oncall.shifts
	shift.subject == input.identity.subject
	g := {
		"name": sprintf("oncall %s", [shift.subject]),
		"actions": ["deployment.read", "deployment.cancel"],
		"applications": ["*/*"],
		"environments": ["prod"],
		"until": shift.until,
	}
}

deny contains "contractors cannot change production" if {
	input.identity.claims.employee_type == "contractor"
	startswith(input.resource.environment, "prod")
}
