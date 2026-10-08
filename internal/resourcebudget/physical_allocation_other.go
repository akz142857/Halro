//go:build !linux

package resourcebudget

import "os"

func allocatePhysicalRange(*os.File, int64, int64) error { return ErrPhysicalAllocation }
func retirePhysicalTail(*os.File, int64, int64) error    { return ErrPhysicalAllocation }
