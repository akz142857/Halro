//go:build !linux

package replication

import "github.com/akz142857/Halro/internal/resourcebudget"

func renameOpaquePhysicalPublication(from, to string) error {
	return resourcebudget.ErrPhysicalAllocation
}
