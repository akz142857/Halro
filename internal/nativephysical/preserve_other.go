//go:build !linux

package nativephysical

import "github.com/akz142857/Halro/internal/resourcebudget"

func renameOpaque(string, string) error { return resourcebudget.ErrPhysicalAllocation }
