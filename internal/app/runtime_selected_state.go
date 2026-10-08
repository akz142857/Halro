package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

const memoryStateBootstrapLimit = 64 << 10

func validMemoryStateBootstrapVersion(b memoryStateBootstrap) bool {
	if b.Version == 1 {
		return b.MetadataSlot == 0 && b.VaultVersion == 0 && b.VaultFingerprint == ""
	}
	return b.Version == 2 && b.MetadataSlot < selectedRotationMetadataSlots && b.VaultVersion != 0 && len(b.VaultFingerprint) == 71
}

// The encrypted bootstrap is published by explicit offline migration. Its
// identity/key/backup provenance is separate from the selected content roots.
// Changing paths or moving a Standalone directory into HA cannot reuse it.
type memoryStateBootstrap struct {
	Version       uint8
	Authority     authenticatedstate.SealEligibility
	InstanceID    string
	KeyGeneration uint64
	// Vault versions describe DEK envelopes; KeyGeneration belongs to the
	// authenticated native namespace and survives DEK rotation.
	MetadataSlot                          uint8
	VaultVersion                          uint64
	VaultFingerprint                      string
	Evidence                              metadatastate.BaselineEvidence
	Binding                               metadatajournal.BaselineBinding
	LedgerPath, AuditPath, GovernancePath string
	LedgerKey, AuditKey, GovernanceKey    []byte
}

type runtimeSelectedState struct {
	closeOnce                           sync.Once
	closeErr                            error
	generation                          *statecut.NativeGeneration
	services                            *statecut.SelectedServices
	publisher                           *authenticatedstate.SealCandidatePublisher
	physicalSealDisk                    resourcebudget.Permit
	physicalCaches                      [4]*resourcebudget.PhysicalCandidate
	physicalCacheWork                   [4]resourcebudget.Permit
	checkpointCaches                    statecut.StartupCacheCandidates
	cacheSlots                          [2]statecut.StartupCacheCandidates
	cacheAuditDisk, cacheGovernanceDisk resourcebudget.Permit
	repackDisk                          resourcebudget.Permit
	bootstrap                           memoryStateBootstrap
	// Surviving files remain charged for this actual Runtime budget lifetime.
	fees           []resourcebudget.Permit
	pressureCancel context.CancelFunc
	pressureDone   chan error
}

func (r *Runtime) selectedBusinessHealth(ctx context.Context) error {
	if r.haSelectedState != nil {
		return r.haSelectedState.businessHealthError(ctx)
	}
	if r.selectedState == nil {
		return nil
	}
	if r.selectedState.generation == nil || r.status.Load() != ledger.AccountingHealthy {
		return authenticatedstate.ErrRecovery
	}
	return r.selectedState.generation.BusinessHealthError(ctx)
}

func memoryStateBootstrapPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "memory-state.bootstrap")
}

func memoryStateDirectory(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "memory-state")
}

func memoryStateMetadataPaths(cfg config.Config, slot uint8) (string, string) {
	directory := memoryStateDirectory(cfg)
	if slot > 0 {
		return filepath.Join(directory, fmt.Sprintf("metadata-root.%d", slot)), filepath.Join(directory, fmt.Sprintf("metadata.%d.journal", slot))
	}
	return filepath.Join(directory, "metadata-root"), filepath.Join(directory, "metadata.journal")
}

func readMemoryStateBootstrap(ctx context.Context, cfg config.Config, v *vault.Vault, manager *resourcebudget.Manager) (*memoryStateBootstrap, error) {
	return readMemoryStateBootstrapAt(ctx, cfg, v, manager, memoryStateBootstrapPath(cfg))
}

func readMemoryStateBootstrapAt(ctx context.Context, cfg config.Config, v *vault.Vault, manager *resourcebudget.Manager, path string) (*memoryStateBootstrap, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > memoryStateBootstrapLimit || manager == nil || cfg.Replication != nil {
		return nil, errors.Join(errors.New("invalid memory-state bootstrap or role/budget"), err)
	}
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 3 * memoryStateBootstrapLimit})
	if err != nil {
		return nil, err
	}
	defer fee.Release()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.Join(ownedfile.ErrReplaced, err, ownedfile.CloseFile(f))
	}
	raw, err := io.ReadAll(io.LimitReader(f, memoryStateBootstrapLimit+1))
	err = errors.Join(err, ownedfile.CloseFile(f))
	defer clear(raw)
	if err != nil || len(raw) > memoryStateBootstrapLimit {
		return nil, errors.Join(authenticatedstate.ErrLimit, err)
	}
	plain, err := v.DecryptCredential("system:memory-state-bootstrap", "system", "halro:memory-state:bootstrap:v1", raw)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	var b memoryStateBootstrap
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, authenticatedstate.ErrCorrupt
	}
	if !validMemoryStateBootstrapVersion(b) || b.InstanceID == "" || b.KeyGeneration == 0 || b.Authority.SchemaVersion != 1 || b.Authority.ApplyVersion != 1 || b.Authority.IndexVersion != 1 || b.Authority.KeyDerivation != 1 || b.Authority.Role != authenticatedstate.SealStandalone || b.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated || b.Authority.Cluster != ([32]byte{}) || b.Authority.Member != ([32]byte{}) || b.Authority.Term != 0 || b.Authority.OrderingIndex != 0 || b.Authority.OrderingOffset != 0 || b.Authority.OrderingMAC != ([32]byte{}) || b.Authority.Instance == ([32]byte{}) || b.Authority.Incarnation == ([16]byte{}) || b.Evidence.Identity != b.Authority.Instance || b.Evidence.InstanceID != b.InstanceID || b.Evidence.KeyGeneration != b.KeyGeneration || b.LedgerPath != cfg.LedgerPath() || b.AuditPath != cfg.AuditPath() || b.GovernancePath != cfg.GovernancePath() || len(b.LedgerKey) != 32 || len(b.AuditKey) != 32 || len(b.GovernanceKey) != 32 {
		clear(b.LedgerKey)
		clear(b.AuditKey)
		clear(b.GovernanceKey)
		return nil, authenticatedstate.ErrSealIneligible
	}
	return &b, ctx.Err()
}

func (s *runtimeSelectedState) close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		err := s.generation.Close()
		for i, target := range s.physicalCaches {
			if closeErr := target.ClosePreserving(); closeErr != nil {
				err = errors.Join(err, closeErr)
			} else if s.physicalCacheWork[i] != (resourcebudget.Permit{}) {
				err = errors.Join(err, s.physicalCacheWork[i].Release())
				s.physicalCacheWork[i] = resourcebudget.Permit{}
			}
		}
		for _, caches := range s.cacheSlots {
			err = errors.Join(err, caches.Audit.CloseWithPersistentReservation(s.cacheAuditDisk), caches.Governance.CloseWithPersistentReservation(s.cacheGovernanceDisk))
		}
		if s.checkpointCaches.Audit != nil {
			err = errors.Join(err, s.checkpointCaches.Audit.Close())
		}
		if s.checkpointCaches.Governance != nil {
			err = errors.Join(err, s.checkpointCaches.Governance.Close())
		}
		if s.publisher != nil {
			err = errors.Join(err, s.publisher.Close())
		}
		if err == nil {
			clear(s.bootstrap.LedgerKey)
			clear(s.bootstrap.AuditKey)
			clear(s.bootstrap.GovernanceKey)
		}
		if s.pressureCancel != nil {
			s.pressureCancel()
			<-s.pressureDone
			s.pressureCancel = nil
		}
		s.closeErr = err
	})
	return s.closeErr
}

func bindSelectedRuntimeUsage(ctx context.Context, cfg config.Config, s *runtimeSelectedState, pressure *resourcePressureRuntime) (*runtimeUsage, *usage.Collector, *usage.Exporter, error) {
	if s == nil || s.generation == nil || s.services == nil {
		return nil, nil, nil, authenticatedstate.ErrSealIneligible
	}
	o := &runtimeUsageOwner{selected: true, derivative: s.generation.Derivative, disk: s.generation.Owners.Usage, collector: s.services.Collector, native: s.generation.Owners.NativeLedger, resourcePressure: pressure}
	o.maintenanceCtx, o.maintenanceCancel = context.WithCancel(context.Background())
	exporter, err := o.bindArchive(ctx, cfg, o.native, s.bootstrap.LedgerKey)
	if err != nil {
		return nil, nil, nil, errors.Join(err, o.close())
	}
	return &runtimeUsage{disk: o.disk, owner: o}, o.collector, exporter, nil
}

func tryOpenSelectedRuntimeState(ctx context.Context, cfg config.Config, v *vault.Vault, guard *lock.Lock, pressure *resourcePressureRuntime, appendDisk *runtimeLedgerAppendDiskOwner, observe func(context.Context, startupaudit.Progress) error) (result *runtimeSelectedState, resultErr error) {
	manager := pressure.logicalBudget()
	if _, err := os.Lstat(memoryStateBootstrapPath(cfg)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if manager == nil || pressure.monitor == nil {
		return nil, errors.New("selected Runtime requires its actual shared pressure monitor and budget")
	}
	s := &runtimeSelectedState{}
	defer func() {
		if resultErr != nil {
			resultErr = &memorySelectedStateFailure{Cause: errors.Join(resultErr, s.close()), Owner: s}
			result = nil
		}
	}()
	if rotation, _ := ctx.Value(selectedRotationPressureKey{}).(*selectedRotationPressureOwner); rotation != nil {
		if rotation.pressure != pressure || rotation.owner.pressureCancel == nil || manager.Snapshot().Pressure != resourcebudget.Healthy {
			return nil, resourcebudget.ErrPressure
		}
	} else if err := startSelectedPressureMonitor(ctx, cfg, manager, pressure, s); err != nil {
		return nil, err
	}
	b, err := readMemoryStateBootstrap(ctx, cfg, v, manager)
	if err != nil || b == nil {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	s.bootstrap = *b
	if cfg.ResourceBudget == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := openSelectedRuntimeGeneration(ctx, cfg, guard, manager, appendDisk, s, observe); err != nil {
		return nil, err
	}
	if err := pressure.setSelectedCompletionProfile(ledger.CompletionWorkspaceRequirements()); err != nil {
		return nil, err
	}
	return s, nil
}

func openSelectedRuntimeGeneration(ctx context.Context, cfg config.Config, guard *lock.Lock, manager *resourcebudget.Manager, appendDisk *runtimeLedgerAppendDiskOwner, s *runtimeSelectedState, observe func(context.Context, startupaudit.Progress) error) error {
	b := &s.bootstrap
	if cfg.ResourceBudget == nil || manager == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return authenticatedstate.ErrSealIneligible
	}
	limits := cfg.ResourceBudget
	profile := ledger.CompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	if limits.CompletionRAMBytes < completionRAM || limits.MaxOwners < 2 || limits.MaxClaims-limits.RecoveryClaims < len(profile)+2 {
		return fmt.Errorf("selected Runtime requires completion_ram_bytes >= %d and %d normal claims for the actual terminal source/COW workspace", completionRAM, len(profile)+2)
	}
	for _, ceiling := range []int64{limits.LedgerProjectionDiskBytes, limits.UsageProjectionDiskBytes, limits.GovernanceProjectionDiskBytes, limits.MetadataStoreDiskBytes, limits.MetadataJournalDiskBytes, limits.LedgerSegmentIndexDiskBytes} {
		if ceiling < 4<<20 {
			return errors.New("selected Runtime requires explicit root/journal/directory Disk ceilings of at least 4 MiB")
		}
	}
	directory := memoryStateDirectory(cfg)
	if err := validateRuntimeDerivedPath(ctx, cfg, directory); err != nil {
		return err
	}
	for _, path := range []string{memoryStateBootstrapPath(cfg), filepath.Join(cfg.Storage.DataDir, "active.seal"), filepath.Join(cfg.Storage.DataDir, "active.seal.pending")} {
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return err
		}
	}
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err == nil {
			s.fees = append(s.fees, fee)
		}
		return fee, err
	}
	sealBytes := int64(32 << 10)
	if limits.PhysicalTerminalReserve {
		sealBytes = authenticatedstate.PhysicalSealDiskBytes
	}
	sealFee, err := acquire(sealBytes)
	if err != nil {
		return err
	}
	s.physicalSealDisk = sealFee
	if limits.PhysicalTerminalReserve {
		s.publisher, err = authenticatedstate.OpenPhysicalSealCandidatePublisher(ctx, cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, sealFee, b.Authority)
	} else {
		s.publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, sealFee)
	}
	if err != nil {
		return err
	}
	selected, found, err := s.publisher.Current()
	if err != nil || !found {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	// A Standalone has no externally confirmed prefix. The offline bootstrap
	// fixes its independent identity, versions and authenticated epoch binding;
	// the signed common publication supplies saved positions. Each actual native
	// constructor verifies that prefix and its suffix, and all historical audits
	// must finish before these positions authorize business writes.
	authority := b.Authority
	copy(authority.Positions[:], selected.Positions[:])
	if err := authenticatedstate.ValidateSealEligibility(selected, authority); err != nil {
		return err
	}
	lc, err := acquire(limits.LedgerProjectionDiskBytes)
	if err != nil {
		return err
	}
	uc, err := acquire(limits.UsageProjectionDiskBytes)
	if err != nil {
		return err
	}
	gc, err := acquire(limits.GovernanceProjectionDiskBytes)
	if err != nil {
		return err
	}
	mc, err := acquire(limits.MetadataStoreDiskBytes)
	if err != nil {
		return err
	}
	jc, err := acquire(limits.MetadataJournalDiskBytes)
	if err != nil {
		return err
	}
	dc, err := acquire(limits.LedgerSegmentIndexDiskBytes)
	if err != nil {
		return err
	}
	appendBytes := func(ceiling int64) int64 { return min(ceiling/2, 16<<20) }
	ledgerOptions := ledger.Options{ChainKey: b.LedgerKey, QueueCapacity: cfg.Usage.WALQueueCapacity, MaxBatch: cfg.Usage.WALMaxBatch, FlushInterval: cfg.Usage.WALFlushInterval.Value(), ReserveAppendDisk: appendDisk.reserve, MaxSegmentDirectoryIndexBytes: ledgerSegmentIndexShareBytes(cfg)}
	if appendDisk != nil {
		ledgerOptions.ReserveNativeFileDisk = appendDisk.reserveNativeFile
	}
	if cfg.Usage.Durability == "strict" {
		ledgerOptions.MaxBatch, ledgerOptions.FlushInterval = 1, 0
	}
	var physicalRecovery *ledger.PhysicalStartupRecoveryOptions
	if limits.PhysicalTerminalReserve {
		if appendDisk == nil {
			return resourcebudget.ErrInvalid
		}
		// The restored actual native owner retains this existing aggregate
		// credit. Reclassify before that retain; a shared claim cannot transfer
		// its class while another live target owner holds it.
		if err := appendDisk.publish(); err != nil {
			return err
		}
		physicalRecovery = &ledger.PhysicalStartupRecoveryOptions{GrowthBytes: limits.LedgerAppendDiskBytes, GrowthFee: appendDisk.credit, Capacity: appendDisk, CompletionRAM: limits.CompletionRAMBytes, PublicationRequiresNoDebt: s.publisher.PhysicalPublicationRequiresNoDebt()}
	}
	cacheSlot := selected.Revision % 2
	metadataDirectory, metadataJournalPath := memoryStateMetadataPaths(cfg, b.MetadataSlot)
	s.generation, err = statecut.OpenSelectedGeneration(ctx, statecut.NativeGenerationOptions{
		SelectedReopenOptions: statecut.SelectedReopenOptions{ReopenOptions: statecut.ReopenOptions{
			Sources: statecut.StandaloneOwners{Budget: manager, Guard: guard, Publisher: s.publisher, InstanceID: b.InstanceID, KeyGeneration: b.KeyGeneration}, Authority: authority,
			LedgerDirectory: filepath.Join(directory, "ledger"), LedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: authority.Instance, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)}, LedgerDisk: lc,
			DirectoryPath: filepath.Join(directory, "directory"), DirectoryOptions: ledger.AuthenticatedDirectoryOptions{Identity: authority.Instance, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes), DiskReservation: dc},
			UsageOptions: usage.AuthenticatedUsageOptions{Identity: authority.Instance, UsageKey: b.LedgerKey, Budget: manager, RootDirectory: filepath.Join(directory, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes), PhysicalTerminalReserve: limits.PhysicalTerminalReserve}, UsageDisk: uc,
			GovernanceOptions: governance.AuthenticatedGovernanceOptions{Identity: authority.Instance, RootKey: b.GovernanceKey, Budget: manager, RootDirectory: filepath.Join(directory, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes)}, GovernanceDisk: gc,
			MetadataDirectory: metadataDirectory, MetadataJournalPath: metadataJournalPath, MetadataKey: b.LedgerKey,
			MetadataOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: authority.Instance, InstanceID: b.InstanceID, KeyGeneration: b.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: appendBytes(limits.MetadataStoreDiskBytes)}, Evidence: b.Evidence, RootDiskReservation: mc, JournalDiskReservation: jc, MaxJournalBytes: limits.MetadataJournalDiskBytes},
		}, PhysicalRecovery: physicalRecovery, MetadataBinding: b.Binding, AuditOptions: startupaudit.Options{Deadline: 10 * time.Minute, ProgressTimeout: 30 * time.Second, ObserveProgress: observe}},
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		AuditCachePath: filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("memory-audit-%d.cache", cacheSlot)), GovernanceCachePath: filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("memory-governance-%d.cache", cacheSlot)),
		LedgerKey: b.LedgerKey, AuditKey: b.AuditKey, GovernanceKey: b.GovernanceKey, LedgerNative: ledgerOptions,
		AuditNative:      audit.Options{CacheBudget: manager, CacheRAMBytes: max(limits.AuditLogRAMBytes, auditlimits.MinCacheRAMBytes), MaxDiskBytes: limits.AuditLogDiskBytes},
		GovernanceNative: governance.Options{CacheBudget: manager, CacheRAMBytes: max(limits.GovernanceLogRAMBytes, governancelimits.MinCacheRAMBytes), MaxDiskBytes: limits.GovernanceLogDiskBytes},
	})
	if err != nil {
		return err
	}
	if limits.PhysicalTerminalReserve && s.publisher.PhysicalPublicationRequiresNoDebt() {
		if err := s.generation.WaitStartupAudit(ctx); err != nil {
			return err
		}
		// The selected Ledger factory already proved its original root equals the
		// complete authenticated native head and both pinned recovery streams empty.
		if err := s.publisher.PreserveUnqualifiedPhysicalSealAfterNoDebt(ctx, s.generation.Seal); err != nil {
			return err
		}
		if err := s.publisher.EnablePhysicalSealPreparation(ctx, s.generation.Seal, s.physicalSealDisk); err != nil {
			return err
		}
	}
	if limits.PhysicalTerminalReserve {
		// These four permanent target owners must be paid outside the temporary
		// common-publication workspace. Startup recovery later consumes these
		// exact authenticated inodes; it cannot retain a publication child loan.
		if err := s.generation.WaitStartupAudit(ctx); err != nil {
			return err
		}
		if err := s.publisher.EnablePhysicalSealPreparation(ctx, s.generation.Seal, s.physicalSealDisk); err != nil {
			return err
		}
		if err := s.preparePhysicalCacheTargets(ctx, cfg); err != nil {
			return fmt.Errorf("prepare original physical startup cache targets: %w", err)
		}
	}
	if selectedRotationColdSourceRequested(ctx) {
		if err := qualifySelectedRotationColdSource(ctx, s); err != nil {
			return err
		}
		return appendDisk.publish()
	}
	s.services, err = s.generation.StartSelectedServices(ctx, statecut.SelectedServicesOptions{Authority: authority, Timezone: cfg.Usage.Timezone, QueueCapacity: cfg.Usage.AnalyticsQueueCapacity, PhysicalRecoveryCaches: func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		return prepareSelectedCheckpointCaches(ctx, cfg, s)
	}})
	if err != nil {
		return fmt.Errorf("start selected business services: %w", err)
	}
	if b.Version == 2 {
		keyring, err := s.services.Metadata.VaultKeyring()
		if err != nil || keyring.ActiveKeyVersion != b.VaultVersion || keyring.ActiveFingerprint != b.VaultFingerprint {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
	}
	if err := appendDisk.publish(); err != nil {
		return err
	}
	return nil
}

var ErrMemoryStateActivationPending = errors.New("memory-state activation is pending; resume the explicit offline migration before startup")

func memoryRuntimeActivated(cfg config.Config) (bool, error) {
	if _, err := os.Lstat(memoryStateBootstrapPath(cfg) + ".pending"); err == nil {
		return false, ErrMemoryStateActivationPending
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(memoryStateBootstrapPath(cfg)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if pending, err := memoryMigrationNamespacePresent(cfg); err != nil {
		return false, err
	} else if pending {
		return false, ErrMemoryStateActivationPending
	}
	return false, nil
}

type memorySelectedStateFailure struct {
	Cause error
	Owner *runtimeSelectedState
}

func (e *memorySelectedStateFailure) Error() string { return e.Cause.Error() }
func (e *memorySelectedStateFailure) Unwrap() error { return e.Cause }

// One actual source lifetime owns its original Monitor across every nested reader.
func startSelectedPressureMonitor(ctx context.Context, cfg config.Config, manager *resourcebudget.Manager, pressure *resourcePressureRuntime, s *runtimeSelectedState) error {
	// Keep real sampling alive through startup, business and checkpoint. A
	// startup-only monitor would invalidate pressure before the next DAO call.
	monitorCtx, cancelMonitor := context.WithCancel(context.Background())
	s.pressureCancel, s.pressureDone = cancelMonitor, make(chan error, 1)
	go func() { s.pressureDone <- pressure.monitor.Run(monitorCtx, nil) }()
	ticker := time.NewTicker(cfg.ResourcePressure.SampleInterval.Value())
	defer ticker.Stop()
	for manager.Snapshot().Pressure != resourcebudget.Healthy {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			sample := pressure.monitor.Snapshot()
			if sample.Samples > 0 && sample.Issues & ^resourcebudget.Cooling != 0 {
				return fmt.Errorf("%w (signals=%#x, runtime_bytes=%d, disk_available_bytes=%d, inodes=%d, cgroup_depth=%d)", resourcebudget.ErrPressure, sample.Issues, sample.Sample.RuntimeUsed, sample.Sample.DiskAvailable, sample.Sample.InodesAvailable, sample.Sample.GroupCount)
			}
		}
	}
	return nil
}
