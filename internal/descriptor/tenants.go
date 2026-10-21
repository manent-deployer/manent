package descriptor

import (
	"fmt"
	"regexp"
)

// tenantNameRe validates tenant names: they become part of workflow IDs, directories and keys.
var tenantNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidTenantName checks a tenant name.
func ValidTenantName(name string) error {
	if !tenantNameRe.MatchString(name) {
		return fmt.Errorf("invalid tenant name %q: lowercase letters, digits and '-', at most 40", name)
	}
	return nil
}
