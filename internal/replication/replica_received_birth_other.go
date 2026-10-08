//go:build !linux && !darwin

package replication

import (
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
)

func captureReceivedBirth(*os.File) (resourcebudget.PhysicalIdentity, error) {
	return resourcebudget.PhysicalIdentity{}, ErrMemberAuthority
}
