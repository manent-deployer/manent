package manent.authz_test

import rego.v1

import data.manent.authz

dave := {"subject": "oidc:dave", "method": "oidc", "name": "dave", "groups": ["team-uc25"], "claims": {"employee_type": "employee"}}

carol := object.union(dave, {"subject": "oidc:carol", "name": "carol", "claims": {"employee_type": "contractor"}})

test_contractor_denied_in_prod if {
	reasons := authz.deny with input as {"identity": carol, "action": "deployment.submit", "resource": {"application": "uc25-api", "environment": "prod"}}
	"contractors cannot change production" in reasons
}

test_employee_not_denied if {
	reasons := authz.deny with input as {"identity": dave, "action": "deployment.submit", "resource": {"application": "uc25-api", "environment": "prod"}}
	count(reasons) == 0
}

test_team_owns_its_tenants if {
	gs := authz.grants with input as {"identity": dave}
	some g in gs
	g.tenants == "owner=team-uc25"
}
