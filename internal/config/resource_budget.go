package config

import (
	"fmt"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// ResourceBudget configures shared logical RAM, disk and optional unconfirmed
// byte envelopes. These explicit
// envelopes require peak calibration; they are estimates, not allocator limits,
// physical block reservations or settlement guarantees.
const DefaultProviderObjectReferenceDiskBytes int64 = 32 << 20

type ResourceBudget struct {
	// Require authenticated seven-root Runtime selection. This explicit engine
	// contract permits fixed COW/scratch workspaces instead of historical bbolt
	// mapping estimates; an absent selection must refuse before legacy DB open.
	MemoryStateRequired bool `yaml:"memory_state_required"`
	// Explicit opt-in: selected Standalone native/COW/root destination
	// preallocation. Unsupported OS/FS or missing ownership refuses startup;
	// this does not grant HA physical confirmation capacity.
	PhysicalTerminalReserve bool `yaml:"physical_terminal_reserve"`
	// Zero preserves the legacy startup path without whole-tree reconciliation.
	// A positive value reserves a separate observed persistent baseline; it is
	// deliberately additional to each consumer's growth/replacement ceiling.
	PersistentInventoryDiskBytes int64 `yaml:"persistent_inventory_disk_bytes"`
	// Additional native Ledger append and auxiliary-product capacity for one
	// Runtime lifetime. Published or uncertain products retain capacity. The
	// existing tree is charged independently by PersistentInventoryDiskBytes.
	LedgerAppendDiskBytes         int64 `yaml:"ledger_append_disk_bytes"`
	AccountingRecoveryRAMBytes    int64 `yaml:"accounting_recovery_ram_bytes"`
	LedgerSegmentIndexDiskBytes   int64 `yaml:"ledger_segment_index_disk_bytes"`
	LedgerProjectionDiskBytes     int64 `yaml:"ledger_projection_disk_bytes"`
	UsageProjectionDiskBytes      int64 `yaml:"usage_projection_disk_bytes"`
	UsageArchiveDiskBytes         int64 `yaml:"usage_archive_disk_bytes"`
	UsageArchiveReadRAMBytes      int64 `yaml:"usage_archive_read_ram_bytes"`
	GovernanceProjectionDiskBytes int64 `yaml:"governance_projection_disk_bytes"`
	GovernanceLogDiskBytes        int64 `yaml:"governance_log_disk_bytes"`
	AuditLogDiskBytes             int64 `yaml:"audit_log_disk_bytes"`
	MetadataStoreDiskBytes        int64 `yaml:"metadata_store_disk_bytes"`
	// Selected COW reserves can exceed the bounded legacy bbolt unlock/export
	// image. Zero preserves the original full Metadata mapping estimate.
	LegacyMetadataFileDiskBytes int64 `yaml:"legacy_metadata_file_disk_bytes"`
	MetadataJournalDiskBytes    int64 `yaml:"metadata_journal_disk_bytes"`
	OrderingJournalDiskBytes    int64 `yaml:"ordering_journal_disk_bytes"`
	// Each actual backup index/report, private staging tree and encrypted
	// output has its own finite ceiling and claim under the same Manager.
	// Different physical files never share one still-live reservation; their
	// concurrent claims require aggregate Recovery/Total Disk headroom.
	BackupWorkspaceDiskBytes         int64 `yaml:"backup_workspace_disk_bytes"`
	RestoreWorkspaceDiskBytes        int64 `yaml:"restore_workspace_disk_bytes"`
	ProviderObjectReferenceDiskBytes int64 `yaml:"provider_object_reference_disk_bytes"`
	TotalRAMBytes                    int64 `yaml:"total_ram_bytes"`
	TotalDiskBytes                   int64 `yaml:"total_disk_bytes"`
	TotalUnconfirmedBytes            int64 `yaml:"total_unconfirmed_bytes"`
	ConfigurationDiskBytes           int64 `yaml:"configuration_disk_bytes"`
	ActiveDiskBytes                  int64 `yaml:"active_disk_bytes"`
	CacheDiskBytes                   int64 `yaml:"cache_disk_bytes"`
	IODiskBytes                      int64 `yaml:"io_disk_bytes"`
	IOUnconfirmedBytes               int64 `yaml:"io_unconfirmed_bytes"`
	BackgroundDiskBytes              int64 `yaml:"background_disk_bytes"`
	CompletionDiskBytes              int64 `yaml:"completion_disk_bytes"`
	CompletionUnconfirmedBytes       int64 `yaml:"completion_unconfirmed_bytes"`
	RecoveryDiskBytes                int64 `yaml:"recovery_disk_bytes"`
	RecoveryReserveDiskBytes         int64 `yaml:"recovery_reserve_disk_bytes"`
	RecoveryUnconfirmedBytes         int64 `yaml:"recovery_unconfirmed_bytes"`
	RecoveryReserveUnconfirmedBytes  int64 `yaml:"recovery_reserve_unconfirmed_bytes"`
	UnconfirmedMaxEntries            int   `yaml:"unconfirmed_max_entries"`
	ConfigurationRAMBytes            int64 `yaml:"configuration_ram_bytes"`
	ActiveRAMBytes                   int64 `yaml:"active_ram_bytes"`
	CacheRAMBytes                    int64 `yaml:"cache_ram_bytes"`
	IORAMBytes                       int64 `yaml:"io_ram_bytes"`
	BackgroundRAMBytes               int64 `yaml:"background_ram_bytes"`
	CompletionRAMBytes               int64 `yaml:"completion_ram_bytes"`
	RecoveryRAMBytes                 int64 `yaml:"recovery_ram_bytes"`
	ControlRAMBytes                  int64 `yaml:"control_ram_bytes"`
	RecoveryReserveRAMBytes          int64 `yaml:"recovery_reserve_ram_bytes"`
	MaxClaims                        int   `yaml:"max_claims"`
	RecoveryClaims                   int   `yaml:"recovery_claims"`
	MaxOwners                        int   `yaml:"max_owners"`
	RequestRAMBytes                  int64 `yaml:"request_ram_bytes"`
	DeferredRAMBytes                 int64 `yaml:"deferred_ram_bytes"`
	CaptureRAMBytes                  int64 `yaml:"capture_ram_bytes"`
	ReplicationCandidateRAMBytes     int64 `yaml:"replication_candidate_ram_bytes"`
	ReplicationQueueRAMBytes         int64 `yaml:"replication_queue_ram_bytes"`
	ReplicationReadRAMBytes          int64 `yaml:"replication_read_ram_bytes"`
	ReplicationReaderRAMBytes        int64 `yaml:"replication_reader_ram_bytes"`
	AuditLogRAMBytes                 int64 `yaml:"audit_log_ram_bytes"`
	GovernanceLogRAMBytes            int64 `yaml:"governance_log_ram_bytes"`
	PricingLifetimeRAMBytes          int64 `yaml:"pricing_lifetime_ram_bytes"`
	PricingIORAMBytes                int64 `yaml:"pricing_io_ram_bytes"`
}

func (b ResourceBudget) Limits(maxAge time.Duration) resourcebudget.Limits {
	return resourcebudget.Limits{
		Total:           resourcebudget.Cost{RAM: b.TotalRAMBytes, Disk: b.TotalDiskBytes, Unconfirmed: b.TotalUnconfirmedBytes},
		RecoveryReserve: resourcebudget.Cost{RAM: b.RecoveryReserveRAMBytes, Disk: b.RecoveryReserveDiskBytes, Unconfirmed: b.RecoveryReserveUnconfirmedBytes},
		Classes: [resourcebudget.ClassCount]resourcebudget.Cost{
			{RAM: b.ConfigurationRAMBytes, Disk: b.ConfigurationDiskBytes},
			{RAM: b.ActiveRAMBytes, Disk: b.ActiveDiskBytes},
			{RAM: b.CacheRAMBytes, Disk: b.CacheDiskBytes},
			{RAM: b.IORAMBytes, Disk: b.IODiskBytes, Unconfirmed: b.IOUnconfirmedBytes},
			{RAM: b.BackgroundRAMBytes, Disk: b.BackgroundDiskBytes},
			{RAM: b.CompletionRAMBytes, Disk: b.CompletionDiskBytes, Unconfirmed: b.CompletionUnconfirmedBytes},
			{RAM: b.RecoveryRAMBytes, Disk: b.RecoveryDiskBytes, Unconfirmed: b.RecoveryUnconfirmedBytes}, {RAM: b.ControlRAMBytes},
		},
		MaxClaims: b.MaxClaims, RecoveryClaims: b.RecoveryClaims, MaxOwners: b.MaxOwners, MaxPressureAge: maxAge,
	}
}

// ValidateTerminalReplication checks the exact request ladder rather than
// silently admitting a shorter terminal vector than the Gateway can execute.
func (b ResourceBudget) ValidateTerminalReplication(maxAttempts int) error {
	if b.CompletionUnconfirmedBytes == 0 {
		return nil
	}
	count := maxAttempts + 1
	if maxAttempts < 1 || count > resourcebudget.MaxCompletionBatch ||
		b.CompletionUnconfirmedBytes < int64(count)*ledger.TerminalReplicationFrameBytes || b.UnconfirmedMaxEntries < count ||
		b.MaxClaims-b.RecoveryClaims < count+3 || b.IORAMBytes < 8192 {
		return fmt.Errorf("completion_unconfirmed_bytes must cover all %d terminal Ledger frames, their fixed claims/control owner and HA entry slots", count)
	}
	return nil
}

func (b ResourceBudget) Validate(pressure *ResourcePressure, maxRequestBytes int64) error {
	if b.PhysicalTerminalReserve && (b.LedgerAppendDiskBytes <= 0 || b.LedgerProjectionDiskBytes <= 0 || b.PersistentInventoryDiskBytes <= 0 || b.CompletionRAMBytes <= 0 || b.CompletionDiskBytes <= 0) {
		return fmt.Errorf("physical_terminal_reserve requires selected Ledger/native persistent and Completion RAM/Disk ceilings")
	}
	if pressure == nil {
		return fmt.Errorf("requires calibrated resource_pressure")
	}
	if b.ReplicationCandidateRAMBytes < 0 || b.ReplicationQueueRAMBytes < 0 || b.ReplicationReadRAMBytes < 0 || b.ReplicationReaderRAMBytes < 0 {
		return fmt.Errorf("replication_candidate_ram_bytes, replication_queue_ram_bytes replication_read_ram_bytes and replication_reader_ram_bytes cannot be negative")
	}
	if b.TotalUnconfirmedBytes < 0 || b.IOUnconfirmedBytes < 0 || b.CompletionUnconfirmedBytes < 0 || b.RecoveryUnconfirmedBytes < 0 || b.RecoveryReserveUnconfirmedBytes < 0 || b.UnconfirmedMaxEntries < 0 {
		return fmt.Errorf("unconfirmed byte envelopes cannot be negative")
	}
	unconfirmedEnabled := b.TotalUnconfirmedBytes > 0 || b.IOUnconfirmedBytes > 0 || b.RecoveryUnconfirmedBytes > 0 || b.RecoveryReserveUnconfirmedBytes > 0 || b.UnconfirmedMaxEntries > 0
	if unconfirmedEnabled && (b.TotalUnconfirmedBytes == 0 || b.IOUnconfirmedBytes < replicationlimits.MaxEncodedFrameBytes || b.RecoveryUnconfirmedBytes == 0 || b.RecoveryReserveUnconfirmedBytes == 0 || b.RecoveryUnconfirmedBytes < b.RecoveryReserveUnconfirmedBytes || b.RecoveryReserveUnconfirmedBytes >= b.TotalUnconfirmedBytes || b.IOUnconfirmedBytes > b.TotalUnconfirmedBytes-b.RecoveryReserveUnconfirmedBytes || b.UnconfirmedMaxEntries < 1 || b.UnconfirmedMaxEntries > 65536) {
		return fmt.Errorf("unconfirmed byte budgets require a total ceiling, IO capacity for one maximum frame, a separately reserved Recovery suffix capacity and 1-65536 release entries")
	}
	if b.CompletionUnconfirmedBytes > 0 && (!unconfirmedEnabled || b.CompletionUnconfirmedBytes > b.TotalUnconfirmedBytes-b.RecoveryReserveUnconfirmedBytes) {
		return fmt.Errorf("completion_unconfirmed_bytes requires the same HA unconfirmed budget and must fit its non-Recovery capacity")
	}
	// Recovery may need to retain an existing suffix larger than the newly
	// configured append limit. Its index table therefore uses the hard bounded
	// coordinator maximum whenever this feature is enabled.
	unconfirmedMetadataRAM := int64(0)
	if unconfirmedEnabled {
		unconfirmedMetadataRAM = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	}
	derivedDisk, derivedOwners := int64(0), 0
	inventoryRAM := int64(0)
	if b.PersistentInventoryDiskBytes < 0 || b.PersistentInventoryDiskBytes > 1<<50 {
		return fmt.Errorf("persistent_inventory_disk_bytes must be zero or a bounded positive startup inventory ceiling")
	}
	if b.PersistentInventoryDiskBytes > 0 {
		inventoryRAM = diskinventory.WorkspaceRAMBytes
	}
	if b.LedgerAppendDiskBytes < 0 || b.LedgerAppendDiskBytes > 1<<50 || b.LedgerAppendDiskBytes > 0 && b.PersistentInventoryDiskBytes == 0 {
		return fmt.Errorf("ledger_append_disk_bytes must be zero or bounded positive native append/auxiliary capacity with persistent_inventory_disk_bytes for existing native bytes")
	}
	if err := b.validateLegacyMetadataFileCeiling(); err != nil {
		return err
	}
	metadataMappingRAM, err := metadatalimits.MappingRAMBytes(b.LegacyMetadataFileCeiling())
	if err != nil {
		return err
	}
	projectionMappingRAM, err := b.projectionWorkingRAM()
	if err != nil || projectionMappingRAM > math.MaxInt64-metadataMappingRAM {
		return fmt.Errorf("Metadata and projection mapping RAM aggregate overflows: %v", err)
	}
	mappingRAM := metadataMappingRAM + projectionMappingRAM
	backupMappingRAM, err := b.backupWorkingRAM()
	if err != nil {
		return err
	}
	for _, value := range []int64{b.PersistentInventoryDiskBytes, b.LedgerAppendDiskBytes, b.LedgerSegmentIndexDiskBytes, b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.UsageArchiveDiskBytes, b.GovernanceProjectionDiskBytes, b.GovernanceLogDiskBytes, b.AuditLogDiskBytes, b.MetadataStoreDiskBytes, b.MetadataJournalDiskBytes, b.OrderingJournalDiskBytes} {
		if value < 0 || value > math.MaxInt64-derivedDisk {
			return fmt.Errorf("Ledger segment index, metadata store, persistent projection, Usage archive and Governance/Audit/Metadata/Ordering journal ceilings must be non-negative and sum without overflow")
		}
		if value > 0 {
			derivedDisk += value
			derivedOwners++
		}
	}
	if b.BackupWorkspaceDiskBytes < 0 || b.BackupWorkspaceDiskBytes > 1<<50 || b.RestoreWorkspaceDiskBytes < 0 {
		return fmt.Errorf("backup_workspace_disk_bytes must be zero or a finite ceiling at most 1PiB; restore_workspace_disk_bytes cannot be negative")
	}
	if b.ProviderObjectReferenceDiskBytes < 0 || b.ProviderObjectReferenceDiskBytes > 1<<40 ||
		b.ProviderObjectReferenceDiskBytes > 0 && (b.ProviderObjectReferenceDiskBytes < 480 || b.ProviderObjectReferenceDiskBytes > b.IODiskBytes) {
		return fmt.Errorf("provider_object_reference_disk_bytes must be zero or a bounded positive file ceiling that fits io_disk_bytes")
	}
	if b.UsageArchiveReadRAMBytes < 0 || b.UsageArchiveReadRAMBytes > math.MaxInt64/usage.ArchiveReaderSlots {
		return fmt.Errorf("usage_archive_read_ram_bytes must be non-negative and fit all concurrent readers without overflow")
	}
	transientDisk := b.BackupWorkspaceDiskBytes
	if b.RestoreWorkspaceDiskBytes > math.MaxInt64-transientDisk {
		return fmt.Errorf("backup and restore workspace disk ceilings overflow")
	}
	transientDisk += b.RestoreWorkspaceDiskBytes
	recoveryDiskClaims := 5 + derivedOwners
	fixedControlClaims := 5 + derivedOwners
	if unconfirmedEnabled {
		// The release table and temporary suffix permit overlap while startup
		// folds per-index byte totals into one owner; promotion uses the same
		// temporary second slot after startup.
		recoveryDiskClaims += 2
		fixedControlClaims += 2
	}
	if b.BackupWorkspaceDiskBytes > 0 {
		recoveryDiskClaims++
		fixedControlClaims++
	}
	if b.RestoreWorkspaceDiskBytes > 0 {
		recoveryDiskClaims++
		fixedControlClaims++
	}
	if b.UsageArchiveDiskBytes > 0 {
		// Archive scratch can coexist with its persistent tree owner during
		// startup recovery and consumes a separate normal claim at runtime.
		if b.IODiskBytes <= 0 {
			return fmt.Errorf("usage_archive_disk_bytes requires a positive io_disk_bytes envelope for Runtime scratch reservations")
		}
		recoveryDiskClaims++
		fixedControlClaims++
	}
	if b.UsageArchiveReadRAMBytes > 0 {
		allReadersRAM := b.UsageArchiveReadRAMBytes * usage.ArchiveReaderSlots
		if b.UsageArchiveReadRAMBytes > b.IORAMBytes || allReadersRAM > b.RecoveryRAMBytes {
			return fmt.Errorf("usage_archive_read_ram_bytes must fit both IO and startup Recovery classes")
		}
		recoveryDiskClaims += usage.ArchiveReaderSlots
		fixedControlClaims += usage.ArchiveReaderSlots
	}
	if derivedOwners > 0 && (derivedDisk > b.TotalDiskBytes || derivedDisk > b.BackgroundDiskBytes || derivedDisk > b.RecoveryDiskBytes || derivedDisk > b.RecoveryReserveDiskBytes || b.RecoveryClaims < recoveryDiskClaims) {
		return fmt.Errorf("configured Ledger index/projections, Usage archive and Governance/Audit/Metadata/Ordering journal ceilings must fit Total, Background, startup Recovery and Recovery reserve Disk envelopes plus dedicated Recovery claims")
	}
	if transientDisk > 0 && (derivedDisk > math.MaxInt64-transientDisk || derivedDisk+transientDisk > b.TotalDiskBytes || derivedDisk+transientDisk > b.RecoveryDiskBytes || derivedDisk+transientDisk > b.RecoveryReserveDiskBytes || b.RecoveryClaims < recoveryDiskClaims) {
		return fmt.Errorf("backup/restore workspace and persistent startup ceilings must fit Total, Recovery and Recovery reserve Disk envelopes plus dedicated Recovery claims")
	}
	g, p, io, a := b.GovernanceLogRAMBytes, b.PricingLifetimeRAMBytes, b.PricingIORAMBytes, b.AccountingRecoveryRAMBytes
	fixedControl := int64(fixedControlClaims) * resourcebudget.ClaimRAMBytes
	archiveReadRAM := b.UsageArchiveReadRAMBytes
	recoveryWorkRAM, recoveryOverflow := int64(0), false
	for _, value := range []int64{g, b.AuditLogRAMBytes, p, io, a, archiveReadRAM, unconfirmedMetadataRAM, mappingRAM, backupMappingRAM, inventoryRAM} {
		if value < 0 || value > math.MaxInt64-recoveryWorkRAM {
			recoveryOverflow = true
			break
		}
		recoveryWorkRAM += value
	}
	recoveryReserveNeed := recoveryWorkRAM
	if !recoveryOverflow && fixedControl > math.MaxInt64-recoveryReserveNeed {
		recoveryOverflow = true
	} else if !recoveryOverflow {
		recoveryReserveNeed += fixedControl
	}
	if b.AuditLogRAMBytes < auditlimits.MinCacheRAMBytes || g < governancelimits.MinCacheRAMBytes || p < pricinglimits.MinLifetimeRAMBytes || io < pricinglimits.MinIORAMBytes || a < accountinglimits.MinRecoveryRAMBytes || recoveryOverflow || b.RecoveryClaims < recoveryDiskClaims ||
		recoveryWorkRAM > b.RecoveryRAMBytes || recoveryReserveNeed > b.RecoveryReserveRAMBytes {
		return fmt.Errorf("Audit, Governance, pricing and accounting_recovery_ram_bytes require calibrated envelopes and five dedicated Recovery claims including both startup workspaces")
	}

	_, err = resourcebudget.New(b.Limits(pressure.MaxAge.Value()))
	if err != nil {
		return fmt.Errorf("invalid shared RAM, class, control or recovery limits")
	}
	if derivedDisk > b.TotalDiskBytes-b.RecoveryReserveDiskBytes {
		return fmt.Errorf("configured persistent projections, Usage archive and Governance/Audit/Metadata/Ordering journal ceilings cannot fit normal shared Disk after retaining the Recovery reserve")
	}
	normalDisk := b.TotalDiskBytes - b.RecoveryReserveDiskBytes
	if derivedDisk > normalDisk || b.CompletionDiskBytes > normalDisk-derivedDisk {
		return fmt.Errorf("completion_disk_bytes must fit normal shared Disk alongside persistent owners")
	}
	if b.RequestRAMBytes <= 0 || b.RequestRAMBytes < maxRequestBytes || b.DeferredRAMBytes <= 0 || b.CaptureRAMBytes < 0 {
		return fmt.Errorf("request_ram_bytes must cover the body ceiling and deferred_ram_bytes must be positive; both require measured working-set amplification")
	}
	// Check both single-envelope costs without starting or renewing a lease.
	normal := b.TotalRAMBytes - b.RecoveryReserveRAMBytes - resourcebudget.ClaimRAMBytes
	if mappingRAM > b.BackgroundRAMBytes || mappingRAM > normal {
		return fmt.Errorf("Metadata and projection mapping/freelist RAM must fit Background and normal shared capacity")
	}
	normal -= mappingRAM
	normalClaimsRequired := 3 + derivedOwners // pricing IO, one request, one bounded Admin Audit IO job and every persistent disk owner after recovery handoff
	if unconfirmedEnabled {
		normalClaimsRequired++ // one aggregate Primary owner after append permits merge
	}
	if b.CompletionRAMBytes > 0 || b.CompletionDiskBytes > 0 {
		normalClaimsRequired++ // the request reserves its completion credit atomically
	}
	if b.UsageArchiveDiskBytes > 0 {
		normalClaimsRequired += usage.ArchiveReaderSlots // one possible disk owner per concurrent archive operation
	}
	if b.UsageArchiveReadRAMBytes > 0 {
		normalClaimsRequired += usage.ArchiveReaderSlots
	}
	if b.MaxClaims-b.RecoveryClaims < normalClaimsRequired || b.PricingIORAMBytes < pricinglimits.MinIORAMBytes || b.PricingIORAMBytes > b.IORAMBytes || b.PricingIORAMBytes > normal || b.RequestRAMBytes > normal-b.PricingIORAMBytes-resourcebudget.ClaimRAMBytes {
		return fmt.Errorf("pricing_io_ram_bytes must cover native wide-row working memory and fit IO/shared capacity alongside one request with normal claims")
	}
	completionRAM := b.CompletionRAMBytes
	if b.MemoryStateRequired && completionRAM > 0 {
		// Selected requests reserve a finite terminal profile. The Completion
		// class ceiling also funds independent checkpoint and recovery owners.
		completionRAM = 0
		for _, slot := range ledger.CompletionWorkspaceRequirements() {
			completionRAM += slot.RAM
		}
		if completionRAM > b.CompletionRAMBytes {
			return fmt.Errorf("completion_ram_bytes must fund the selected request terminal workspace of %d bytes", completionRAM)
		}
	}
	if completionRAM > 0 && (completionRAM > normal || b.RequestRAMBytes > normal-completionRAM-resourcebudget.ClaimRAMBytes || b.RequestRAMBytes > normal-b.PricingIORAMBytes-completionRAM-2*resourcebudget.ClaimRAMBytes) {
		return fmt.Errorf("completion_ram_bytes must fit shared normal RAM alongside one request and pricing IO")
	}
	if archiveReadRAM > 0 {
		allReadersRAM := archiveReadRAM * usage.ArchiveReaderSlots
		claimRAM := int64(usage.ArchiveReaderSlots+2) * resourcebudget.ClaimRAMBytes
		if allReadersRAM > b.IORAMBytes || allReadersRAM > normal || b.PricingIORAMBytes > normal-allReadersRAM || claimRAM > normal-allReadersRAM-b.PricingIORAMBytes || b.RequestRAMBytes > normal-allReadersRAM-b.PricingIORAMBytes-claimRAM {
			return fmt.Errorf("usage_archive_read_ram_bytes must fit all archive readers alongside pricing IO and one request")
		}
	}
	backgroundWorkRAM := b.BackgroundRAMBytes - mappingRAM
	if b.RequestRAMBytes > b.ActiveRAMBytes || b.DeferredRAMBytes > backgroundWorkRAM || b.RequestRAMBytes > normal || b.DeferredRAMBytes > normal {
		return fmt.Errorf("request or deferred envelope cannot fit its class and shared normal capacity")
	}
	if b.CaptureRAMBytes > 0 && (b.CaptureRAMBytes < b.RequestRAMBytes || b.CaptureRAMBytes > backgroundWorkRAM || b.CaptureRAMBytes > normal || b.MaxOwners < 3) {
		return fmt.Errorf("capture_ram_bytes must cover request_ram_bytes, fit shared Background capacity and allow at least 3 owners")
	}
	return nil
}

// Two candidates plus an additional reconstruction, three session queue/read
// owners, one aggregate NativeSource reader and one native Governance cache
// plus one fixed pricing owner, pricing startup workspace and accounting recovery
// workspace fit dedicated Recovery, including before startup pressure is known.
func (b ResourceBudget) ValidateReplicationCandidates() error {
	if err := b.validateHAPhysicalWorkMinimum(); err != nil {
		return err
	}
	if b.BackupWorkspaceDiskBytes < 0 || b.BackupWorkspaceDiskBytes > 1<<50 {
		return fmt.Errorf("backup_workspace_disk_bytes must be zero or a finite ceiling at most 1PiB")
	}
	if b.PersistentInventoryDiskBytes < 0 || b.PersistentInventoryDiskBytes > 1<<50 {
		return fmt.Errorf("persistent_inventory_disk_bytes must be zero or a bounded positive startup inventory ceiling")
	}
	inventoryRAM := int64(0)
	if b.PersistentInventoryDiskBytes > 0 {
		inventoryRAM = diskinventory.WorkspaceRAMBytes
	}
	if b.LedgerAppendDiskBytes < 0 || b.LedgerAppendDiskBytes > 1<<50 || b.LedgerAppendDiskBytes > 0 && b.PersistentInventoryDiskBytes == 0 {
		return fmt.Errorf("ledger_append_disk_bytes must be zero or bounded positive native append/auxiliary capacity with persistent_inventory_disk_bytes for existing native bytes")
	}
	n, q, r, d := b.ReplicationCandidateRAMBytes, b.ReplicationQueueRAMBytes, b.ReplicationReadRAMBytes, b.ReplicationReaderRAMBytes
	if b.UsageArchiveReadRAMBytes < 0 || b.UsageArchiveReadRAMBytes > math.MaxInt64/usage.ArchiveReaderSlots {
		return fmt.Errorf("usage_archive_read_ram_bytes must be non-negative and fit all concurrent readers without overflow")
	}
	g := b.GovernanceLogRAMBytes
	if err := b.validateLegacyMetadataFileCeiling(); err != nil {
		return err
	}
	metadataMappingRAM, err := metadatalimits.MappingRAMBytes(b.LegacyMetadataFileCeiling())
	if err != nil {
		return err
	}
	projectionMappingRAM, err := b.projectionWorkingRAM()
	if err != nil || projectionMappingRAM > math.MaxInt64-metadataMappingRAM {
		return fmt.Errorf("Metadata and projection mapping RAM aggregate overflows: %v", err)
	}
	mappingRAM := metadataMappingRAM + projectionMappingRAM
	backupMappingRAM, err := b.backupWorkingRAM()
	if err != nil {
		return err
	}
	control := int64(12 * resourcebudget.ClaimRAMBytes)
	minimumClaims := 12
	unconfirmedMetadataRAM := int64(0)
	if b.UnconfirmedMaxEntries > 0 {
		unconfirmedMetadataRAM = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	}
	if b.UnconfirmedMaxEntries > 0 {
		control += 2 * resourcebudget.ClaimRAMBytes
		minimumClaims += 2 // fixed release table and temporary aggregate suffix owner
	}
	for _, value := range []int64{b.PersistentInventoryDiskBytes, b.LedgerAppendDiskBytes, b.LedgerSegmentIndexDiskBytes, b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.UsageArchiveDiskBytes, b.GovernanceProjectionDiskBytes, b.GovernanceLogDiskBytes, b.AuditLogDiskBytes, b.MetadataStoreDiskBytes, b.MetadataJournalDiskBytes, b.OrderingJournalDiskBytes} {
		if value > 0 {
			control += resourcebudget.ClaimRAMBytes
			minimumClaims++
		}
	}
	if b.BackupWorkspaceDiskBytes > 0 {
		control += resourcebudget.ClaimRAMBytes
		minimumClaims++
	}
	if b.RestoreWorkspaceDiskBytes > 0 {
		control += resourcebudget.ClaimRAMBytes
		minimumClaims++
	}
	if b.UsageArchiveDiskBytes > 0 {
		if b.IODiskBytes <= 0 {
			return fmt.Errorf("usage_archive_disk_bytes requires a positive io_disk_bytes envelope for Runtime scratch reservations")
		}
		control += resourcebudget.ClaimRAMBytes
		minimumClaims++ // one startup archive transient owner can overlap persistent claims
	}
	if b.UsageArchiveReadRAMBytes > 0 {
		control += int64(usage.ArchiveReaderSlots) * resourcebudget.ClaimRAMBytes
		minimumClaims += usage.ArchiveReaderSlots
	}
	p, io, a := b.PricingLifetimeRAMBytes, b.PricingIORAMBytes, b.AccountingRecoveryRAMBytes
	archiveReadRAM := b.UsageArchiveReadRAMBytes * usage.ArchiveReaderSlots
	if n < replicationlimits.MinCandidateRAMBytes || q < replicationlimits.MinQueueRAMBytes || r < replicationlimits.MinReadRAMBytes || d < replicationlimits.MinNativeReaderRAMBytes || g < governancelimits.MinCacheRAMBytes || b.AuditLogRAMBytes < auditlimits.MinCacheRAMBytes || p < pricinglimits.MinLifetimeRAMBytes || io < pricinglimits.MinIORAMBytes || a < accountinglimits.MinRecoveryRAMBytes {
		return fmt.Errorf("replication_candidate_ram_bytes, replication_queue_ram_bytes, replication_read_ram_bytes, replication_reader_ram_bytes and Audit/Governance/pricing/accounting require calibrated RAM envelopes")
	}
	base := int64(0)
	for _, value := range []int64{d, g, b.AuditLogRAMBytes, p, io, a, archiveReadRAM, unconfirmedMetadataRAM, mappingRAM, backupMappingRAM, inventoryRAM} {
		if value < 0 || value > math.MaxInt64-base {
			return fmt.Errorf("replication recovery RAM envelopes overflow")
		}
		base += value
	}
	if control > math.MaxInt64-base {
		return fmt.Errorf("replication recovery RAM control envelope overflows")
	}
	ceiling := (math.MaxInt64 - base - control) / 3
	if r > ceiling || q > ceiling-r || n > ceiling-r-q || b.RecoveryClaims < minimumClaims ||
		b.RecoveryRAMBytes < 3*(n+q+r)+base || b.RecoveryReserveRAMBytes < 3*(n+q+r)+base+control {
		return fmt.Errorf("replication RAM requires Recovery capacity/claims for three reconstructions, three session queue/read owners, one native reader, both native retry caches, pricing lifetime and startup pricing/accounting workspaces")
	}
	return nil
}

// The capture limit is applied after encoding. This lower bound rejects
// obviously undersized envelopes; it is not a sufficient peak-memory estimate.
func (b ResourceBudget) ValidateCapture(capture FailureCapture) error {
	if !capture.Enabled {
		return nil
	}
	sideBytes := capture.ByteLimit()
	if sideBytes < 1024 || sideBytes > 1<<20 {
		return fmt.Errorf("failure capture side byte limit is invalid")
	}
	if b.CaptureRAMBytes <= 0 || b.CaptureRAMBytes < int64(sideBytes)*3 || b.MaxOwners < 3 {
		return fmt.Errorf("enabled failure capture requires capture_ram_bytes covering all 3 side limits and at least 3 owners; encoding/sealing amplification still needs calibration")
	}
	return nil
}
