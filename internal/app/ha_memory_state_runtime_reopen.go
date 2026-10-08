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
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

func openHARuntimeSavedPrefix(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, bootstrapPath string) (a *HAMemoryRuntimeActivation, reopened *statecut.ReopenedHA, resultErr error) {
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
	member, err := captureHARuntimeReopenSources(ctx, m, manager)
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
	reopened, err = reopenHARuntimeSavedPrefix(ctx, a)
	a.reopened = reopened
	return a, reopened, err
}

// This general Runtime preparation owns a fresh private source generation.
// It authenticates the saved common prefix independently of a later allowed
// native suffix. It grants no business role before joined advance/publication.
func reopenHARuntimeSavedPrefix(ctx context.Context, a *HAMemoryRuntimeActivation) (*statecut.ReopenedHA, error) {
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
	return statecut.ReopenHAPrefix(ctx, statecut.HAReopenOptions{Sources: sources, Components: statecut.ReopenOptions{
		LedgerDirectory: filepath.Join(dir, "ledger"), LedgerDisk: lc,
		LedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)},
		UsageDisk:     uc, UsageOptions: usage.AuthenticatedUsageOptions{Identity: identity, UsageKey: b.LedgerKey, Budget: manager, RootDirectory: filepath.Join(dir, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes)},
		GovernanceDisk: gc, GovernanceOptions: governance.AuthenticatedGovernanceOptions{Identity: identity, RootKey: b.GovernanceKey, Budget: manager, RootDirectory: filepath.Join(dir, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes)},
		DirectoryPath: filepath.Join(dir, "directory"), DirectoryOptions: ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes), DiskReservation: dc},
		MetadataDirectory: b.MetadataRoot, MetadataJournalPath: b.MetadataJournal, MetadataKey: b.LedgerKey,
		MetadataOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: identity, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: appendBytes(limits.MetadataStoreDiskBytes)}, Evidence: b.Evidence, NativeHead: nativeHead, RootDiskReservation: m.rootDisk, JournalDiskReservation: m.journalDisk, MaxJournalBytes: m.authority.Claims().New.MaxFileBytes},
	}})
}

// captureHARuntimeReopenSources authenticates actual Member and complete
// Ordering/native bytes before bootstrap or any candidate root is opened.
// Dynamic permission remains actual Primary durable or Replica applied; the
// signed carrier's old N+1 is a minimum boundary, never a current-cut grant.
func captureHARuntimeReopenSources(ctx context.Context, m *HAMetadataMaintenance, manager *resourcebudget.Manager) (member replication.MemberState, resultErr error) {
	if m == nil || m.member == nil || m.authority == nil || manager == nil {
		return member, authenticatedstate.ErrSealIneligible
	}
	claims := m.authority.Claims()
	resultErr = m.member.publisher.WithReadAuthority(ctx, manager, func(ctx context.Context, actual replication.MemberState, authorize func(func() error) error) error {
		if actual.ClusterID != claims.ClusterID || actual.Incarnation != claims.Incarnation || actual.Term != claims.Term || actual.PromisedTerm != claims.Term || actual.Role != m.member.role || actual.Role == replication.RolePrimary && actual.NodeID != claims.Primary || actual.Role != replication.RolePrimary && actual.Role != replication.RoleReplica || actual.ConfirmedIndex < claims.Index+1 || actual.AppliedIndex > actual.ConfirmedIndex || actual.ConfirmedIndex > actual.DurableIndex || actual.Projection.Index != actual.AppliedIndex {
			return replication.ErrMemberAuthority
		}
		return m.member.journal.WithFrozenHead(ctx, manager, func(ctx context.Context, head replication.FrozenOrderingHead) error {
			if head.Header.ClusterID != actual.ClusterID || head.Header.Incarnation != actual.Incarnation || head.Index != actual.DurableIndex || head.MAC != actual.OrderingHeadMAC || head.Term != actual.Term {
				return replication.ErrMemberAuthority
			}
			boundary, err := m.member.journal.Record(claims.Index + 1)
			if err != nil {
				return err
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != m.authority.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if err := m.member.source.ValidateMetadataBaseline(boundary); err != nil {
				return err
			}
			applied, err := m.member.journal.StoreCursorsThroughContext(ctx, actual.AppliedIndex)
			if err != nil || applied[3].Generation != actual.Projection.MetadataEpoch || applied[3].Sequence != actual.Projection.MetadataSequence {
				return errors.Join(replication.ErrMemberAuthority, err)
			}
			durable, err := m.member.journal.StoreCursorsThroughContext(ctx, actual.DurableIndex)
			if err != nil {
				return err
			}
			for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
				if _, err := m.member.source.HeadAt(store, durable[store-replication.StoreLedger]); err != nil {
					return err
				}
			}
			if err := replication.ReconstructRangeEach(actual.ClusterID, actual.Incarnation, 1, actual.DurableIndex, m.member.journal, m.member.source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
				return err
			}
			return authorize(func() error { member = actual; return ctx.Err() })
		})
	})
	return
}
