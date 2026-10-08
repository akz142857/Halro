//go:build linux

package app

import "github.com/akz142857/Halro/internal/config"

// This profile declares the simultaneous HTTP, Directory, Usage and Metadata
// completion envelopes. The dedicated runner's physical memory cap stays 2GiB;
// neither its filesystem size nor the individual append limits are reduced.
func physicalTerminalBudgetFixture() config.ResourceBudget {
	b := selectedRuntimeBudgetFixture()
	b.TotalRAMBytes = 2 << 30
	// The same fixture also performs the offline legacy migration, whose
	// declared mappings and backup workspace remain charged in that phase.
	b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 896<<20, 896<<20
	b.CompletionRAMBytes = 1 << 30
	b.LedgerSegmentIndexDiskBytes = 48 << 20
	// A full request and Roll can leave up to seven source frames waiting for
	// the original Usage catch-up owner. Keep its existing 16MiB append bound.
	b.UsageProjectionDiskBytes = 128 << 20
	return b
}
