package config

import (
	"fmt"
	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/scratchstate"
)

// LegacyMetadataFileCeiling bounds every actual bbolt bootstrap/staging file.
// The selected Metadata page bank keeps its separate full disk ceiling.
func (b ResourceBudget) LegacyMetadataFileCeiling() int64 {
	if b.MemoryStateRequired && b.LegacyMetadataFileDiskBytes > 0 {
		return b.LegacyMetadataFileDiskBytes
	}
	return b.MetadataStoreDiskBytes
}

func (b ResourceBudget) validateLegacyMetadataFileCeiling() error {
	if b.LegacyMetadataFileDiskBytes < 0 || b.LegacyMetadataFileDiskBytes > 1<<50 ||
		b.LegacyMetadataFileDiskBytes > 0 && (!b.MemoryStateRequired || b.MetadataStoreDiskBytes <= 0 || b.LegacyMetadataFileDiskBytes > b.MetadataStoreDiskBytes) {
		return fmt.Errorf("legacy_metadata_file_disk_bytes requires memory_state_required and a positive ceiling no greater than metadata_store_disk_bytes")
	}
	return nil
}

func (b ResourceBudget) projectionWorkingRAM() (int64, error) {
	if !b.MemoryStateRequired {
		return accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes)
	}
	// Cold domain reducers are sequential and own one actual 64MiB scratch
	// engine, native codec/reducer workspace and complete pack builder. All
	// live/read-only COW views still acquire their own real IO/read permits.
	return scratchstate.WorkspaceRAMBytes + accountinglimits.MinRecoveryRAMBytes, nil
}

func (b ResourceBudget) backupWorkingRAM() (int64, error) {
	if !b.MemoryStateRequired {
		return accountinglimits.ProjectionMappingRAMBytes(b.BackupWorkspaceDiskBytes)
	}
	if b.BackupWorkspaceDiskBytes == 0 {
		return 0, nil
	}
	// Archive IO is streamed. Actual legacy Metadata mapping remains separately
	// charged by MetadataStoreDiskBytes; archive disk is not a mapped DB.
	return accountinglimits.MinRecoveryRAMBytes, nil
}
