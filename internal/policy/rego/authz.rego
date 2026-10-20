# Library of manent for the permission policies (package manent.authz), imported with
# "import data.manent.lib.authz".
package manent.lib.authz

import rego.v1

import data.manent.lib

# binding_matches reports whether a binding (the shape of rbac.bindings) concerns the identity:
# one of its subjects matches the identity's subject, or one of its groups one of the identity's
# groups. Patterns use "*" as in manent.yml.
binding_matches(b, identity) if lib.match_any(object.get(b, "subjects", []), identity.subject)

binding_matches(b, identity) if {
	some g in object.get(identity, "groups", [])
	lib.match_any(object.get(b, "groups", []), g)
}

# bindings are the grants of data.manent.rbac.bindings that concern the identity of the input,
# exactly as the builtin engine computes them from manent.yml.
bindings contains g if {
	some i, b in data.manent.rbac.bindings
	binding_matches(b, input.identity)
	g := object.union(
		{"name": sprintf("binding %d", [i]), "role": b.role, "applications": b.applications, "environments": b.environments},
		_tenants(b),
	)
}

_tenants(b) := {"tenants": b.tenants} if {
	object.get(b, "tenants", "") != ""
} else := {}
