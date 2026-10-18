# Default policy, used when the revision defines no manent.governance package: the governance
# rules of manent.yml applied as data (data.manent.rules).
package manent.governance

import rego.v1

import data.manent.lib.rules

decision := rules.decision
