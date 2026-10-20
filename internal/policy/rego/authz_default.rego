# Default permission policy, used when the revision defines no manent.authz package: the
# bindings of manent.yml applied as data (data.manent.rbac), without denials.
package manent.authz

import rego.v1

import data.manent.lib.authz

grants := authz.bindings

deny := set()
