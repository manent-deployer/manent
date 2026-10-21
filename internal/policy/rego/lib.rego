# Library of manent for governance policies: deterministic helpers, imported with
# "import data.manent.lib". The time of the evaluation is input.now, never the clock.
package manent.lib

import rego.v1

# match_any reports whether value matches one of the patterns ("*" stands for any sequence of
# characters, as in manent.yml).
match_any(patterns, value) := manent.glob_any(patterns, value)

# semver_less reports whether version a precedes b: "v" prefix and missing parts are allowed,
# numeric parts compare as numbers and a pre-release precedes its release.
semver_less(a, b) := manent.semver_less(a, b)

# local_time splits an RFC 3339 instant in a time zone (IANA name): year, month, day,
# weekday (0 = Sunday), hour and minute.
local_time(t, tz) := manent.local_time(t, tz)

# business_hours reports an instant from 9:00 to 18:00, Monday to Friday, in the time zone.
business_hours(t, tz) if {
	local := manent.local_time(t, tz)
	local.weekday >= 1
	local.weekday <= 5
	local.hour >= 9
	local.hour < 18
}

# tenant_matches reports whether a tenant selector ("tier=gold, name in (acme, globex)")
# matches the tenant of the input ({} without tenant: never).
tenant_matches(selector, tenant) := manent.tenant_matches(selector, tenant)

# app_matches reports whether an application selector ("criticality=high, tenant.division
# in (commerce)") matches the application of the input: its tags, "application", "tenant" and
# the tenant tags with the "tenant." prefix.
app_matches(selector, doc) := manent.app_matches(selector, doc)

# duration_ns converts a duration ("72h", "90m") to nanoseconds.
duration_ns(d) := manent.duration_ns(d)
