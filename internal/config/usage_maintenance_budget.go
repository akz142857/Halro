package config

import (
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/usage"
)

// UsageMaintenanceIOMinimum is the IO class a Usage disk cycle needs at its
// peak: the private reference job (accounting workspace plus both projection
// mappings, charged for its whole lifetime) holding a date sweep's retained
// workspace, beside the owners resident in IO for the Runtime lifetime. An
// activated install opens the selected Governance projection whether or not
// memory_state_required is set, so that resident is always counted; the
// physical Usage catch-up work is held only with the physical terminal
// reserve. Transient IO work (pricing, archive readers) is extra: it delays a
// cycle, it does not make one impossible.
func (b ResourceBudget) UsageMaintenanceIOMinimum() (ram, disk int64, err error) {
	mapping, err := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes)
	if err != nil {
		return 0, 0, err
	}
	sweepRAM, sweepDisk := usage.DateMaintenanceWorkspace(b.UsageArchiveDiskBytes)
	ramParts := []int64{b.AccountingRecoveryRAMBytes, mapping, sweepRAM, governance.TypedProjectionResidentRAMBytes}
	if b.PhysicalTerminalReserve {
		ramParts = append(ramParts, usage.PhysicalCatchupActiveRAMBytes)
	}
	for _, part := range ramParts {
		if part < 0 || part > math.MaxInt64-ram {
			return 0, 0, fmt.Errorf("Usage maintenance IO RAM overflows")
		}
		ram += part
	}
	for _, part := range []int64{b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, sweepDisk} {
		if part < 0 || part > math.MaxInt64-disk {
			return 0, 0, fmt.Errorf("Usage maintenance IO Disk overflows")
		}
		disk += part
	}
	return ram, disk, nil
}

// ValidateStandaloneUsageMaintenance refuses a budget the Usage disk cycle
// cannot fit: it would start, then refuse that cycle every round, and Usage
// export stops behind date maintenance. Config applies it to Standalone only.
func (b ResourceBudget) ValidateStandaloneUsageMaintenance() error {
	if b.LedgerProjectionDiskBytes <= 0 || b.UsageProjectionDiskBytes <= 0 {
		return nil // no Usage reference job can be admitted at all
	}
	ram, disk, err := b.UsageMaintenanceIOMinimum()
	if err != nil {
		return err
	}
	normalRAM, normalDisk := b.TotalRAMBytes-b.RecoveryReserveRAMBytes, b.TotalDiskBytes-b.RecoveryReserveDiskBytes
	if ram > b.IORAMBytes || ram > normalRAM || disk > b.IODiskBytes || disk > normalDisk {
		return fmt.Errorf("io_ram_bytes and io_disk_bytes must fit the Usage disk cycle: at least %d RAM and %d Disk bytes (reference job, date sweep workspace under usage_archive_disk_bytes and IO residents), within normal shared capacity", ram, disk)
	}
	return nil
}
