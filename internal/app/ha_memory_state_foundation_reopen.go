package app

import (
	"context"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/usage"
)

// This is a startup-only handoff. No service or transport owns the foundation
// before all derived owners exist; Close joins them before the native sources.
func (f *haSelectedRuntimeFoundation) reopenSelectedGeneration(ctx context.Context) (*statecut.ReopenedHA, error) {
	if f == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reopened != nil || f.ctx.Err() != nil || f.closeErr != nil {
		return nil, authenticatedstate.ErrClosed
	}
	b, c, limits := f.bootstrap, f.owners.Components, f.cfg.ResourceBudget
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err == nil {
			f.fees = append(f.fees, fee)
		}
		return fee, err
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
	head, err := c.Metadata.EpochHead()
	if err != nil {
		return nil, err
	}
	dir, identity := haMemoryStateDirectory(f.cfg), b.Evidence.Identity
	appendBytes := func(n int64) int64 { return min(n/2, 16<<20) }
	var physicalRecovery *ledger.PhysicalStartupRecoveryOptions
	if limits.PhysicalTerminalReserve {
		if f.growth == nil {
			return nil, authenticatedstate.ErrSealIneligible
		}
		physicalRecovery = &ledger.PhysicalStartupRecoveryOptions{GrowthBytes: limits.LedgerAppendDiskBytes, GrowthFee: f.growth.credit, Capacity: f.growth, CompletionRAM: limits.CompletionRAMBytes}
	}
	r, err := statecut.ReopenHASelectedGeneration(ctx, statecut.HAReopenOptions{Sources: f.owners, PhysicalRecovery: physicalRecovery, Components: statecut.ReopenOptions{
		LedgerDirectory: filepath.Join(dir, "ledger"), LedgerDisk: lc,
		LedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: c.Budget, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)},
		UsageDisk:     uc, UsageOptions: usage.AuthenticatedUsageOptions{Identity: identity, UsageKey: b.LedgerKey, Budget: c.Budget, RootDirectory: filepath.Join(dir, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes)},
		GovernanceDisk: gc, GovernanceOptions: governance.AuthenticatedGovernanceOptions{Identity: identity, RootKey: b.GovernanceKey, Budget: c.Budget, RootDirectory: filepath.Join(dir, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes)},
		DirectoryPath: filepath.Join(dir, "directory"), DirectoryOptions: ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: c.Budget, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes), DiskReservation: f.directoryDisk},
		MetadataDirectory: b.MetadataRoot, MetadataJournalPath: b.MetadataJournal, MetadataKey: b.LedgerKey,
		MetadataOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: identity, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration, Budget: c.Budget, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: appendBytes(limits.MetadataStoreDiskBytes)}, Evidence: b.Evidence, NativeHead: head, RootDiskReservation: f.rootDisk, JournalDiskReservation: f.journalDisk, MaxJournalBytes: f.owners.Baseline.Claims().New.MaxFileBytes},
	}})
	f.reopened = r
	return r, err
}
