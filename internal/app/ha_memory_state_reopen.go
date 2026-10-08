package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

func openHAMemoryActivationRead(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, bootstrapPath string) (a *HAMemoryRuntimeActivation, reopened *statecut.ReopenedHA, resultErr error) {
	a = &HAMemoryRuntimeActivation{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, a.Close())
			resultErr = &HAMemoryActivationFailure{Owner: a, Cause: resultErr}
		}
	}()
	m, err := openHAMetadataMaintenance(ctx, cfg, keys, guard, manager, logger, resourcebudget.Permit{}, resourcebudget.Permit{})
	a.maintenance = m
	if err != nil {
		return a, nil, err
	}
	member, _, err := qualifyHAMemoryActivationSources(ctx, m, manager, keys.Ledger)
	if err != nil {
		return a, nil, err
	}
	carrier, _, err := readHAMetadataCarrier(ctx, filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "carrier.json"), m.member.clusterKey[:], member)
	if err != nil {
		return a, nil, err
	}
	v, err := vault.New(keys.Master)
	if err != nil {
		return a, nil, err
	}
	defer v.Close()
	b, err := readHAMemoryStateBootstrapAt(ctx, cfg, bootstrapPath, v, manager, member, m.authority, carrier.Preparation.Evidence)
	if err != nil || b == nil {
		return a, nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	a.bootstrap = *b
	c := &a.owners.Components
	*c = statecut.StandaloneOwners{Budget: manager, Guard: guard, NativeLedger: m.nativeLedger, NativeGovernance: m.nativeGovernance, Audit: m.nativeAudit, Metadata: m.baseline, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration}
	a.owners.Member, a.owners.Ordering, a.owners.Source, a.owners.Baseline = m.member.publisher, m.member.journal, m.member.source, m.authority
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		return a, nil, err
	}
	a.fees = append(a.fees, fee)
	c.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, keys.Ledger, statecut.HardLimits(), manager, fee)
	if err != nil {
		return a, nil, err
	}
	c.Lease, err = m.nativeLedger.BeginReplicaReadMaintenance(ctx)
	if err != nil {
		return a, nil, err
	}
	reopened, err = reopenHAMemoryActivation(ctx, a)
	a.reopened = reopened
	return a, reopened, err
}

// Reopening an initial activation borrows this offline source generation. The
// caller still owns Member/native/lease/publisher; this grants no business role.
func reopenHAMemoryActivation(ctx context.Context, a *HAMemoryRuntimeActivation) (*statecut.ReopenedHA, error) {
	if a == nil || a.maintenance == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	m, b := a.maintenance, a.bootstrap
	cfg, manager := m.cfg, a.owners.Components.Budget
	if cfg.ResourceBudget == nil || manager == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	limits := cfg.ResourceBudget
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		p, e := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if e == nil {
			a.fees = append(a.fees, p)
		}
		return p, e
	}
	lc, err := acquire(limits.LedgerProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	uc, err := acquire(limits.UsageProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	gc, err := acquire(limits.GovernanceProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	dc, err := acquire(limits.LedgerSegmentIndexDiskBytes)
	if err != nil {
		return nil, err
	}
	// metadataLog is the legacy archived format1 stream. The actual successor
	// descriptor belongs to the independently qualified format2 baseline.
	nativeHead, err := m.baseline.EpochHead()
	if err != nil {
		return nil, err
	}
	appendBytes := func(n int64) int64 { return min(n/2, 16<<20) }
	dir, identity := haMemoryStateDirectory(cfg), b.Evidence.Identity
	sources := a.owners
	sources.Components.Ledger, sources.Components.Usage, sources.Components.Governance, sources.Components.Directory = nil, nil, nil, nil
	if sources.Components.Publisher == nil {
		return nil, errors.New("HA activation publisher is absent")
	}
	return statecut.ReopenHA(ctx, statecut.HAReopenOptions{Sources: sources, Components: statecut.ReopenOptions{
		LedgerDirectory: filepath.Join(dir, "ledger"), LedgerDisk: lc,
		LedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)},
		UsageDisk:     uc, UsageOptions: usage.AuthenticatedUsageOptions{Identity: identity, UsageKey: b.LedgerKey, Budget: manager, RootDirectory: filepath.Join(dir, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes)},
		GovernanceDisk: gc, GovernanceOptions: governance.AuthenticatedGovernanceOptions{Identity: identity, RootKey: b.GovernanceKey, Budget: manager, RootDirectory: filepath.Join(dir, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes)},
		DirectoryPath: filepath.Join(dir, "directory"), DirectoryOptions: ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes), DiskReservation: dc},
		MetadataDirectory: b.MetadataRoot, MetadataJournalPath: b.MetadataJournal, MetadataKey: b.LedgerKey,
		MetadataOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: identity, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: appendBytes(limits.MetadataStoreDiskBytes)}, Evidence: b.Evidence, NativeHead: nativeHead, RootDiskReservation: m.rootDisk, JournalDiskReservation: m.journalDisk, MaxJournalBytes: m.authority.Claims().New.MaxFileBytes},
	}})
}
