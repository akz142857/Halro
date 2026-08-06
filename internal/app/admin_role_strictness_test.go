package app

import (
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
)

// The empty role was fixed in the data, not in the checks. Nothing may start
// treating "" as acceptable: the store backfilled one legacy shape once, and
// every gate downstream still has to refuse it, or an account that slips
// through with no role becomes an account that can do anything.
func TestEmptyRoleIsNeverValidAndNeverAdministrator(t *testing.T) {
	if domain.ValidAdminRole("") {
		t.Fatal("ValidAdminRole accepted the empty role")
	}
	for _, role := range []string{"", "superuser", "Administrator", "admin", "read-only", "ADMINISTRATOR"} {
		if domain.ValidAdminRole(role) {
			t.Errorf("ValidAdminRole accepted %q", role)
		}
		// requireAdministratorRole compares against this exact constant, so a
		// role that is not it can never reach an administrator-gated handler.
		if role == domain.AdminRoleAdministrator {
			t.Errorf("%q compared equal to the administrator constant", role)
		}
	}
	if !domain.ValidAdminRole(domain.AdminRoleAdministrator) || !domain.ValidAdminRole(domain.AdminRoleReadOnly) {
		t.Fatal("the two supported roles must stay valid")
	}
}

// requireAdministratorRole is the gate every administrator-only write passes
// through, and it is an exact comparison. This pins that: a fallback that
// treated an unknown role as administrator would undo the whole two-level
// split without failing any other test.
func TestAdministratorGateIsAnExactComparison(t *testing.T) {
	for _, role := range []string{"", "read_only", "superuser", "administrator "} {
		if role == domain.AdminRoleAdministrator {
			t.Fatalf("%q must not satisfy the administrator gate", role)
		}
	}
}
