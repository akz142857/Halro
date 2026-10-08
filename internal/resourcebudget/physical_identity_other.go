//go:build !linux

package resourcebudget

import "os"

func capturePhysicalIdentity(*os.File) (PhysicalIdentity, error) {
	return PhysicalIdentity{}, ErrPhysicalAllocation
}

func physicalAllocatedBytes(*os.File) (int64, error) { return 0, ErrPhysicalAllocation }
