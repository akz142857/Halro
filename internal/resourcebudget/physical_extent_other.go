//go:build !linux

package resourcebudget

import "os"

func CheckPhysicalTail(*os.File, int64, int64) error { return ErrPhysicalAllocation }

func CheckPhysicalRange(file *os.File, start, end int64) error { return ErrPhysicalAllocation }
