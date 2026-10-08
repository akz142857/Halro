//go:build linux

package replication

import (
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
)

func captureReceivedBirth(f *os.File) (resourcebudget.PhysicalIdentity, error) {
	return resourcebudget.CapturePhysicalIdentity(f)
}
