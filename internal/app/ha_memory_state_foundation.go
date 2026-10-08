package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

// The actual business startup foundation selects before opening any native
// mutable owner or legacy DB. It owns these descriptors and their publisher;
// no ordinary Ledger/Governance projection or bbolt metadata image is opened.
type haSelectedRuntimeFoundation struct {
	promotionRecovery         *replication.HAPromotionAuthority
	peerPromotionRecovery     *replication.HAPromotionPeerAuthority
	peerMaintenance           *ledger.MaintenanceLease
	promotionAudit            *haSelectedPromotionAudit
	promotionPriorCoordinator *replication.PrimaryCoordinator
	ctx                       context.Context
	cancel                    context.CancelFunc
	lifecycle                 sync.Mutex
	closing                   bool
	callbacks                 sync.WaitGroup
	activeCallbacks           int
	transportOnce             sync.Once
	transportErr              error
	sender                    *deferredPeerSender
	metadataJournal           *metadatajournal.Log // borrowed from actual Replica Baseline
	applyPhase                func(string)         // private deterministic lifecycle test seam
	metadataStore             *boltstore.Store     // borrowed from actual ReopenedHA close owner
	applyErr                  error                // retains actual failed apply/IO cleanup owners
	business                  *haSelectedBusinessServices
	recovery                  atomic.Bool
	reopened                  *statecut.ReopenedHA
	directoryDisk             resourcebudget.Permit
	mu                        sync.Mutex
	cfg                       config.Config
	bootstrap                 haMemoryStateBootstrap
	owners                    statecut.HAOwners
	replication               *replicationRuntime
	growth                    *runtimeLedgerAppendDiskOwner
	inventory                 *diskinventory.Baseline
	fees                      []resourcebudget.Permit
	cacheDisks                [haSelectedCacheSlots][2]resourcebudget.Permit
	cachePendingDisk          resourcebudget.Permit
	auditCache                *audit.StartupCache
	governanceCache           *governance.StartupCache
	auditCachePath            string
	governanceCachePath       string
	readDebt                  *os.File
	closeErr                  error
	rootDisk, journalDisk     resourcebudget.Permit
	orderingDisk              resourcebudget.Permit
	auditNativeDisk           resourcebudget.Permit
	governanceNativeDisk      resourcebudget.Permit
	physicalSealDisk          resourcebudget.Permit
	physicalPublication       *replication.PhysicalPublication
	physicalCaches            *haPhysicalCacheOwner
	sharedPhysicalWork        *resourcebudget.SharedCompletableWorkspace
	pressureCancel            context.CancelFunc
	pressureDone              chan struct{}
	audits                    [3]*startupauditOwner
}

// A concrete alias keeps joined audit owners reachable without changing their
// package's finite deadline/progress capability.
type startupauditOwner = audit.StartupAudit

func (f *haSelectedRuntimeFoundation) Close() error {
	if f == nil {
		return nil
	}
	f.cancel()
	f.lifecycle.Lock()
	f.closing = true
	f.lifecycle.Unlock()
	// Withdraw producers and join real session callbacks before retiring any
	// derived descriptor those callbacks can still borrow.
	f.transportOnce.Do(func() {
		if r := f.replication; r != nil {
			r.stopPrimaryCompletion()
			if r.coordinator != nil {
				r.coordinator.Stop()
			}
			if r.manager != nil {
				f.transportErr = r.manager.Close()
			}
		}
	})
	f.callbacks.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	stage := func(name string, err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("close HA foundation %s: %w", name, err)
	}
	for _, auditor := range f.audits {
		if auditor != nil {
			auditor.Cancel()
			_ = auditor.Wait(context.Background())
		}
	}
	if f.transportErr != nil {
		f.closeErr = errors.Join(f.closeErr, f.transportErr)
		return f.closeErr
	}
	if f.replication != nil {
		if q := f.replication.peerPhysical.Load(); q != nil {
			if err := q.closeJoined(context.Background()); err != nil {
				f.closeErr = errors.Join(f.closeErr, err)
				return f.closeErr
			}
		}
	}
	if err := f.promotionAudit.Close(); err != nil {
		f.closeErr = errors.Join(f.closeErr, err)
		return f.closeErr
	}
	if err := f.business.close(context.Background()); err != nil {
		f.closeErr = errors.Join(f.closeErr, err)
		return f.closeErr
	}
	if f.reopened != nil {
		if err := f.reopened.CloseWithMetadataSource(f.owners.Components.Metadata); err != nil {
			f.closeErr = errors.Join(f.closeErr, err)
			return f.closeErr
		}
	}
	if f.replication != nil {
		f.closeErr = errors.Join(f.closeErr, stage("replication", f.replication.close()))
	}
	if f.closeErr != nil {
		return f.closeErr
	}
	c := f.owners.Components
	if c.Metadata != nil && f.reopened == nil {
		f.closeErr = errors.Join(f.closeErr, stage("Metadata source", c.Metadata.Close()))
	}
	if c.Lease != nil {
		f.closeErr = errors.Join(f.closeErr, stage("native lease", c.Lease.Close()))
	}
	if f.peerMaintenance != nil {
		f.closeErr = errors.Join(f.closeErr, f.peerMaintenance.Close())
	}
	if c.NativeLedger != nil {
		f.closeErr = errors.Join(f.closeErr, stage("native Ledger", c.NativeLedger.Close()))
	}
	if c.Audit != nil {
		f.closeErr = errors.Join(f.closeErr, stage("Audit", c.Audit.Close()))
	}
	if c.NativeGovernance != nil {
		f.closeErr = errors.Join(f.closeErr, stage("Governance source", c.NativeGovernance.Close()))
	}
	if f.auditCache != nil {
		f.closeErr = errors.Join(f.closeErr, f.auditCache.Close())
	}
	if f.governanceCache != nil {
		f.closeErr = errors.Join(f.closeErr, f.governanceCache.Close())
	}
	if f.closeErr != nil {
		return f.closeErr
	}
	if f.promotionPriorCoordinator != nil {
		if err := f.promotionPriorCoordinator.ReleaseUnconfirmedForShutdown(); err != nil {
			f.closeErr = errors.Join(f.closeErr, err)
			return f.closeErr
		}
	}
	if c.Publisher != nil {
		if err := f.physicalCaches.close(); err != nil {
			f.closeErr = errors.Join(f.closeErr, err)
			return f.closeErr
		}
		f.closeErr = errors.Join(f.closeErr, c.Publisher.Close())
	}
	f.closeErr = errors.Join(f.closeErr, f.growth.close(), f.inventory.Close())
	if f.closeErr == nil {
		// Components retire only their own member tickets. This foundation is
		// the final RAM owner after actual business, roots and native jobs join.
		f.closeErr = errors.Join(f.closeErr, f.sharedPhysicalWork.Close())
	}
	if f.closeErr == nil {
		f.bootstrap.clearKeys()
		// Sampling remains live through the final native/control retirements.
		// An unknown retirement retains this original owner for later repair.
		if f.pressureCancel != nil {
			f.pressureCancel()
			<-f.pressureDone
			f.pressureCancel = nil
		}
	}
	// Persistent files still exist. Their fixed ceilings remain in this Manager.
	return f.closeErr
}

type haSelectedRuntimeFoundationFailure struct {
	Owner *haSelectedRuntimeFoundation
	Cause error
}

func (e *haSelectedRuntimeFoundationFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedRuntimeFoundationFailure) Unwrap() error { return e.Cause }

func openHASelectedRuntimeFoundation(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (*haSelectedRuntimeFoundation, error) {
	return openHASelectedRuntimeFoundationMode(ctx, cfg, master, guard, manager, logger, nil)
}
func openHASelectedRuntimeFoundationMode(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, promotionRecovery *replication.HAPromotionAuthority) (*haSelectedRuntimeFoundation, error) {
	return openHASelectedRuntimeFoundationTransitionMode(ctx, cfg, master, guard, manager, logger, promotionRecovery, nil)
}
func openHASelectedRuntimeFoundationTransitionMode(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, promotionRecovery *replication.HAPromotionAuthority, peerRecovery *replication.HAPromotionPeerAuthority) (f *haSelectedRuntimeFoundation, resultErr error) {
	return openHASelectedRuntimeFoundationPeerCompletionMode(ctx, cfg, master, guard, manager, logger, promotionRecovery, peerRecovery, nil)
}
func openHASelectedRuntimeFoundationPeerCompletionMode(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, promotionRecovery *replication.HAPromotionAuthority, peerRecovery *replication.HAPromotionPeerAuthority, completion *replication.HAPromotionPeerCompletion) (f *haSelectedRuntimeFoundation, resultErr error) {
	if cfg.Replication == nil || cfg.ResourceBudget == nil || manager == nil || logger == nil || len(master) != 32 || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := checkHAGroupKeyRotationPending(ctx, cfg, master, manager); err != nil {
		return nil, err
	}
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	f = &haSelectedRuntimeFoundation{cfg: cfg, ctx: ownerCtx, cancel: ownerCancel, promotionRecovery: promotionRecovery, peerPromotionRecovery: peerRecovery}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, f.Close())
			resultErr = &haSelectedRuntimeFoundationFailure{Owner: f, Cause: resultErr}
		}
	}()
	member, clusterKey, err := readHAGroupMemberForOpen(ctx, cfg, master)
	if err != nil {
		return f, err
	}
	defer clear(clusterKey[:])
	actualMember := member
	var promotion *replication.HAPromotionAuthority
	if peerRecovery != nil {
		if promotionRecovery != nil || cfg.ResourceBudget.PhysicalTerminalReserve || completion != nil && completion.Peer() != peerRecovery || completion == nil && peerRecovery.ValidateRecoveryMember(actualMember) != nil {
			return f, authenticatedstate.ErrSealIneligible
		}
	} else if promotionRecovery != nil {
		promotion = promotionRecovery
	} else {
		promotion, err = readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), clusterKey[:], manager)
		if errors.Is(err, os.ErrNotExist) {
			promotion = nil
			err = nil
		}
		if err != nil {
			return f, err
		}
	}
	var qualify func([]byte, []byte, replication.MemberState) (*replication.MetadataBaselineAuthority, error)
	if peerRecovery != nil {
		qualify = func(raw, key []byte, m replication.MemberState) (*replication.MetadataBaselineAuthority, error) {
			if completion != nil {
				m = peerRecovery.Claims().Before
			}
			return replication.QualifyMetadataBaselineForPeerRecovery(raw, key, m, peerRecovery)
		}
	} else if promotion != nil {
		qualify = func(raw, key []byte, m replication.MemberState) (*replication.MetadataBaselineAuthority, error) {
			if promotionRecovery != nil {
				return replication.QualifyMetadataBaselineForPromotionRecovery(raw, key, m, promotion)
			}
			// The baseline binding is authenticated first; actual new-term Ordering
			// qualification below precedes any native/root permission.
			before := promotion.Claims().Before
			return replication.QualifyMetadataBaselineCarrier(raw, key, before)
		}
	}
	workspace := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
	carrier, authority, err := readHAMetadataCarrierWithQualifier(ctx, filepath.Join(workspace, "carrier.json"), clusterKey[:], actualMember, qualify)
	if err != nil {
		return f, err
	}
	b, err := readHAGroupBootstrapForOpen(ctx, cfg, master, manager, member, authority, carrier.Preparation.Evidence)
	if err != nil || b == nil {
		return f, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	f.bootstrap = *b
	claims := authority.Claims()
	if claims.New.MaxFileBytes != cfg.ResourceBudget.MetadataJournalDiskBytes {
		return f, authenticatedstate.ErrSealIneligible
	}
	pressure := &resourcePressureRuntime{budget: manager}
	f.growth, err = prepareRuntimeLedgerAppendDisk(ctx, cfg, pressure)
	if err != nil {
		return f, err
	}
	f.inventory, err = preparePersistentInventory(ctx, cfg, guard, pressure)
	if err != nil {
		return f, err
	}
	if err = f.growth.publish(); err != nil {
		return f, err
	}
	acquire := func(n int64) (resourcebudget.Permit, error) {
		p, e := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: n})
		if e == nil {
			f.fees = append(f.fees, p)
		}
		return p, e
	}
	limits := cfg.ResourceBudget
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		if _, err = acquire(haMemoryUnlockLimit); err != nil {
			return f, err
		}
	}
	for i, n := range []int64{limits.OrderingJournalDiskBytes, limits.AuditLogDiskBytes, limits.GovernanceLogDiskBytes} {
		var disk resourcebudget.Permit
		if disk, err = acquire(n); err != nil {
			return f, err
		}
		if i == 0 {
			f.orderingDisk = disk
		} else if i == 1 {
			f.auditNativeDisk = disk
		} else {
			f.governanceNativeDisk = disk
		}
	}
	for slot := range haSelectedCacheSlots {
		f.cacheDisks[slot][0], err = acquire(audit.MaxStartupCacheBytes)
		if err != nil {
			return f, err
		}
		f.cacheDisks[slot][1], err = acquire(governance.MaxStartupCacheBytes)
		if err != nil {
			return f, err
		}
	}
	if !limits.PhysicalTerminalReserve {
		f.cachePendingDisk, err = acquire(haSelectedCacheSlots * (audit.MaxStartupCacheBytes + governance.MaxStartupCacheBytes))
		if err != nil {
			return f, err
		}
	}
	f.rootDisk, err = acquire(limits.MetadataStoreDiskBytes)
	if err != nil {
		return f, err
	}
	f.journalDisk, err = acquire(claims.New.MaxFileBytes)
	if err != nil {
		return f, err
	}
	sealBytes := int64(32 << 10)
	if limits.PhysicalTerminalReserve {
		sealBytes = authenticatedstate.PhysicalSealDiskBytes
	}
	pubFee, err := acquire(sealBytes)
	if err != nil {
		return f, err
	}
	c := &f.owners.Components
	*c = statecut.StandaloneOwners{Budget: manager, Guard: guard, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration}
	f.physicalSealDisk = pubFee
	if limits.PhysicalTerminalReserve {
		c.Publisher, err = authenticatedstate.OpenPhysicalSealCandidateReader(ctx, cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, pubFee)
	} else {
		c.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, pubFee)
	}
	if err != nil {
		return f, err
	}
	// An already published successor selects the ordinary new-term path only
	// after this actual keyed common reader; the actual ordering/carrier qualifier
	// below still supplies every role and source permission.
	if promotionRecovery != nil && actualMember.Term == promotionRecovery.Claims().Proposal.Term {
		current, found, e := c.Publisher.CurrentContext(ctx)
		if e != nil {
			return f, e
		}
		if found && current.Role == authenticatedstate.SealPrimary && current.Term == actualMember.Term && current.OrderingIndex == actualMember.DurableIndex && current.OrderingMAC == actualMember.OrderingHeadMAC {
			promotionRecovery = nil
			f.promotionRecovery = nil
		}
	}
	journal, err := replication.OpenExistingOrderingJournalWithOptions(cfg.OrderingJournalPath(), clusterKey[:], member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, orderingJournalOptions(cfg))
	if err != nil {
		return f, err
	}
	if completion != nil {
		if err = completion.ValidateRecoveryMember(actualMember, journal); err != nil {
			return f, errors.Join(err, journal.Close())
		}
		current, found, e := c.Publisher.CurrentContext(ctx)
		if e != nil {
			return f, errors.Join(e, journal.Close())
		}
		if found && current.Role == authenticatedstate.SealReplica && current.Term == actualMember.Term && current.OrderingIndex == actualMember.AppliedIndex && current.OrderingMAC == actualMember.OrderingHeadMAC && actualMember.AppliedIndex == peerRecovery.Claims().Before.AppliedIndex+1 {
			peerRecovery = nil
			f.peerPromotionRecovery = nil
			promotion = completion.Candidate()
		}
	}
	if promotion != nil && promotionRecovery == nil {
		authority, err = replication.QualifyMetadataBaselineAfterPromotion(carrier.SignedClaims, clusterKey[:], actualMember, promotion, journal)
		if err != nil {
			return f, errors.Join(err, journal.Close())
		}
	}
	if peerRecovery != nil {
		member = peerRecovery.Claims().Before
	} else if promotionRecovery != nil {
		member = promotionRecovery.Claims().Before
	}
	r := &replicationRuntime{role: member.Role, clusterKey: clusterKey, journal: journal, fatal: make(chan error, 1), objectSourceDir: providerObjectSourceDir(cfg)}
	f.replication = r
	r.publisher, err = replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], actualMember)
	if err != nil {
		return f, err
	}
	newKey, err := hkdf.Key(sha256.New, b.LedgerKey, claims.New.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return f, err
	}
	defer clear(newKey)
	r.source, err = replication.NewNativeSource(replication.NativeSourceOptions{LedgerPath: b.LedgerPath, AuditPath: b.AuditPath, GovernancePath: b.GovernancePath, MetadataPath: filepathMetadataJournal(cfg), LedgerKey: b.LedgerKey, AuditKey: b.AuditKey, GovernanceKey: b.GovernanceKey, MetadataKey: b.LegacyMetadataKey, ProviderObjectDir: providerObjectSourceDir(cfg), LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg), ReaderBudget: manager, ReaderRAMBytes: replicationReaderRAM(cfg, manager) + replication.MetadataBaselineReaderRAMBytes, ReserveLedgerFileDisk: f.growth.reserveNativeFile, MetadataBaseline: &replication.MetadataBaselineSourceOptions{Authority: authority, Path: b.MetadataJournal, Key: newKey}})
	if err != nil {
		return f, err
	}
	f.owners.Member, f.owners.Ordering, f.owners.Source, f.owners.Baseline = r.publisher, journal, r.source, authority
	f.owners.PromotionRecovery = promotionRecovery
	f.owners.PeerPromotionRecovery = peerRecovery
	if peerRecovery != nil {
		f.owners.PeerPromotionCompletion = completion
	}
	// Activate the successor only through the actual MACed N+1 ordering record;
	// a bound file/header alone never establishes the epoch transition.
	if err = r.source.ActivateMetadataBaseline(journal, claims.Index+1); err != nil {
		return f, err
	}
	if head, _, _ := journal.Head(); head > actualMember.DurableIndex {
		if actualMember.Role != replication.RoleReplica || promotionRecovery != nil || peerRecovery != nil {
			return f, replication.ErrMemberAuthority
		}
		actualMember, err = f.recoverReceivedOrdering(ctx)
		if err != nil {
			return f, err
		}
		member = actualMember
	}
	// Independent actual selection precedes every mutable native/root factory.
	var initial statecut.HASelectedCut
	if err = f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error { initial = cut; return ctx.Err() }); err != nil {
		return f, err
	}
	if err = replication.ReconstructRangeEach(actualMember.ClusterID, actualMember.Incarnation, 1, actualMember.DurableIndex, journal, r.source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
		return f, err
	}
	if limits.PhysicalTerminalReserve {
		if err = statecut.WithHASelectedCut(ctx, f.owners, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if cut.Seal != initial.Seal || cut.Authority != initial.Authority || cut.AllowedIndex != initial.AllowedIndex || cut.Allowed != initial.Allowed || cut.Heads != initial.Heads {
				return authenticatedstate.ErrSealIneligible
			}
			if err := c.Publisher.Close(); err != nil {
				return err
			}
			var err error
			c.Publisher, err = authenticatedstate.OpenHAQualifiedPhysicalSealCandidatePublisher(ctx, cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, pubFee, cut.Seal, cut.Authority)
			return err
		}); err != nil {
			return f, fmt.Errorf("HA independently qualified physical common reader: %w", err)
		}
	}
	if limits.PhysicalTerminalReserve {
		f.sharedPhysicalWork, err = manager.InstallSharedCompletableWorkspace(limits.HAPhysicalBackgroundSharedProfiles())
		if err != nil {
			return f, fmt.Errorf("HA original background shared RAM owner: %w", err)
		}
	}
	if member.Role == replication.RoleReplica && member.ConfirmedIndex > member.AppliedIndex {
		if err = f.restoreConfirmedSelectedStartup(ctx, member); err != nil {
			return f, err
		}
	} else if err = f.openSelectedNativeOwners(ctx, member, initial); err != nil {
		return f, err
	}
	return f, nil
}

func (f *haSelectedRuntimeFoundation) openSelectedNativeOwners(ctx context.Context, member replication.MemberState, initial statecut.HASelectedCut) error {
	cfg, b, r := f.cfg, &f.bootstrap, f.replication
	c, manager, limits, guard := &f.owners.Components, f.owners.Components.Budget, f.cfg.ResourceBudget, f.owners.Components.Guard
	journal, claims := r.journal, f.owners.Baseline.Claims()
	var err error
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, e := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if e == nil {
			f.fees = append(f.fees, fee)
		}
		return fee, e
	}
	if err := f.prepareHAPhysicalMemberOrdering(ctx); err != nil {
		return err
	}
	sender := &deferredPeerSender{}
	f.sender = sender
	if member.Role == replication.RolePrimary && f.peerPromotionRecovery == nil {
		peers := make([]string, 0, len(member.Peers))
		for _, p := range member.Peers {
			peers = append(peers, p.Name)
		}
		outbound, e := replication.NewConnectionOutbound(peers, sender)
		if e != nil {
			return e
		}
		window := replication.DefaultPrimaryWindowLimits()
		window.CandidateBudget = manager
		window.CandidateRAMBytes = limits.ReplicationCandidateRAMBytes
		if limits.TotalUnconfirmedBytes > 0 {
			window.UnconfirmedBudget = manager
			window.MaxUnconfirmedEntries = limits.UnconfirmedMaxEntries
			window.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
			window.RecoveryReserveUnconfirmedBytes = limits.RecoveryReserveUnconfirmedBytes
		}
		r.coordinator, err = replication.NewPrimaryCoordinatorFromSource(member.ClusterID, member.Incarnation, member.NodeID, member.Term, member.ConfirmedIndex, peers, journal, outbound, r.publisher.PublishPrimary, r.source, window)
		if err != nil {
			return err
		}
	}
	dir := haMemoryStateDirectory(cfg)
	directoryDisk, err := acquire(limits.LedgerSegmentIndexDiskBytes)
	if err != nil {
		return err
	}
	f.directoryDisk = directoryDisk
	directoryOptions := ledger.AuthenticatedDirectoryOptions{Identity: b.Evidence.Identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: min(limits.LedgerSegmentIndexDiskBytes/2, 16<<20), DiskReservation: directoryDisk}
	ledgerOptions := ledger.Options{ChainKey: b.LedgerKey, Replica: member.Role == replication.RoleReplica, QueueCapacity: cfg.Usage.WALQueueCapacity, MaxBatch: cfg.Usage.WALMaxBatch, FlushInterval: cfg.Usage.WALFlushInterval.Value(), ReserveAppendDisk: f.growth.reserve, ReserveNativeFileDisk: f.growth.reserveNativeFile, MaxSegmentDirectoryIndexBytes: ledgerSegmentIndexShareBytes(cfg)}
	if r.coordinator != nil {
		r.ledgerHooks(&ledgerOptions)
	} else if f.peerPromotionRecovery != nil && member.Role == replication.RolePrimary {
		ledgerOptions.BeforeDurable = func(ledger.DurableBatch) (ledger.AppendAdmission, error) {
			return nil, authenticatedstate.ErrSealIneligible
		}
		ledgerOptions.AfterDurable = func(ledger.DurableBatch) (uint64, error) { return 0, authenticatedstate.ErrSealIneligible }
		ledgerOptions.WaitConfirmed = func(context.Context, uint64) error { return authenticatedstate.ErrSealIneligible }
	}
	c.NativeLedger, _, err = ledger.OpenWithHASelectedSeal(ctx, b.LedgerPath, ledger.NewStatus(), ledgerOptions, ledger.HASelectedSealOpenOptions{WithLiveAuthority: f.withLiveLedgerAuthority, SelectedSealOpenOptions: ledger.SelectedSealOpenOptions{Publisher: c.Publisher, Guard: guard, Directory: filepath.Join(dir, "directory"), DirectoryOptions: directoryOptions}, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			return visit(ctx, cut.Authority, ledger.HASelectedLedgerPosition{Generation: cut.Allowed[0].Generation, Sequence: cut.Allowed[0].Sequence, Hash: cut.Heads[0]})
		})
	}})
	if err != nil {
		return fmt.Errorf("HA selected native Ledger: %w", err)
	}
	if member.Role == replication.RoleReplica {
		c.Lease, err = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
	} else {
		c.Lease, err = c.NativeLedger.BeginMaintenance(ctx)
	}
	if err != nil {
		return err
	}
	if err = f.readSelectedStartupCaches(ctx, initial); err != nil {
		return err
	}
	auditOptions := audit.Options{Replica: member.Role == replication.RoleReplica, MaxDiskBytes: limits.AuditLogDiskBytes, CacheBudget: manager, CacheRAMBytes: limits.AuditLogRAMBytes}
	if r.coordinator != nil {
		actual := r.auditOptions()
		auditOptions.BeforeDurable, auditOptions.AfterDurable = actual.BeforeDurable, actual.AfterDurable
	} else if f.peerPromotionRecovery != nil && member.Role == replication.RolePrimary {
		auditOptions.BeforeDurable = func(audit.DurableBatch) (audit.AppendAdmission, error) {
			return nil, authenticatedstate.ErrSealIneligible
		}
		auditOptions.AfterDurable = func(audit.DurableBatch) error { return authenticatedstate.ErrSealIneligible }
	}
	c.Audit, _, err = audit.OpenWithHASelectedSeal(ctx, b.AuditPath, b.AuditKey, auditOptions, audit.HASelectedSealOpenOptions{SelectedSealOpenOptions: audit.SelectedSealOpenOptions{Publisher: c.Publisher, Guard: guard, Cache: f.auditCache, Identity: b.Evidence.Identity}, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, audit.HASelectedNativePosition) error) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			return visit(ctx, cut.Authority, audit.HASelectedNativePosition{Sequence: cut.Allowed[1].Sequence, Hash: cut.Heads[1]})
		})
	}})
	if err != nil {
		return fmt.Errorf("HA selected native Audit: %w", err)
	}
	govOptions := governance.Options{Replica: member.Role == replication.RoleReplica, MaxDiskBytes: limits.GovernanceLogDiskBytes, CacheBudget: manager, CacheRAMBytes: limits.GovernanceLogRAMBytes}
	if r.coordinator != nil {
		actual := r.governanceOptions()
		govOptions.BeforeDurable, govOptions.AfterDurable = actual.BeforeDurable, actual.AfterDurable
	} else if f.peerPromotionRecovery != nil && member.Role == replication.RolePrimary {
		govOptions.BeforeDurable = func(governance.DurableBatch) (governance.AppendAdmission, error) {
			return nil, authenticatedstate.ErrSealIneligible
		}
		govOptions.AfterDurable = func(governance.DurableBatch) error { return authenticatedstate.ErrSealIneligible }
	}
	c.NativeGovernance, _, err = governance.OpenWithHASelectedSeal(ctx, b.GovernancePath, b.GovernanceKey, govOptions, governance.HASelectedSealOpenOptions{WithApplyAuthority: f.withConfirmedGovernanceAuthority, WithReadAuthority: f.withLiveGovernanceAuthority, SelectedSealOpenOptions: governance.SelectedSealOpenOptions{Publisher: c.Publisher, Guard: guard, Cache: f.governanceCache, Identity: b.Evidence.Identity}, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			return visit(ctx, cut.Authority, governance.HASelectedNativePosition{Sequence: cut.Allowed[2].Sequence, Hash: cut.Heads[2]})
		})
	}})
	if err != nil {
		return fmt.Errorf("HA selected native Governance: %w", err)
	}
	metadataOptions := metadatastate.HASelectedSourceOpenOptions{PhysicalTerminalReserve: limits.PhysicalTerminalReserve, WithConfirmedPhysicalApplyAuthority: func(ctx context.Context, actual *metadatastate.Baseline, expected metadatajournal.Head, through uint64, visit func(context.Context) error) error {
		return statecut.WithHAConfirmedMetadataApplyAuthority(ctx, f.owners, actual, expected, through, visit)
	}, OpenOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: b.Evidence.Identity, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: min(limits.MetadataStoreDiskBytes/2, 16<<20)}, Evidence: b.Evidence, RootDiskReservation: f.rootDisk, JournalDiskReservation: f.journalDisk}, Publisher: c.Publisher, Guard: guard, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, metadatastate.HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			return visit(ctx, cut.Authority, metadatastate.HASelectedMetadataPosition{Epoch: cut.Allowed[3].Generation, Sequence: cut.Allowed[3].Sequence, Hash: cut.Heads[3]}, claims.New)
		})
	}}
	if f.peerPromotionRecovery != nil {
		c.Metadata, _, err = metadatastate.OpenHAPeerTransitionSourceRoot(ctx, b.LedgerKey, b.MetadataRoot, b.MetadataJournal, metadatastate.HAPeerTransitionSourceOpenOptions{OpenOptions: metadataOptions.OpenOptions, Publisher: c.Publisher, Guard: guard, PeerAuthority: f.peerPromotionRecovery, WithAuthority: metadataOptions.WithAuthority})
	} else {
		c.Metadata, _, err = metadatastate.OpenHASelectedSourceRoot(ctx, b.LedgerKey, b.MetadataRoot, b.MetadataJournal, metadataOptions)
	}
	if err != nil {
		return fmt.Errorf("HA selected native Metadata: %w", err)
	}
	if f.peerPromotionRecovery != nil {
		if f.peerPromotionRecovery.Claims().Promise != nil {
			f.metadataJournal, err = c.Metadata.ConfigurePeerReplicaJournal(ctx, b.LedgerKey, claims.New)
		}
	} else if member.Role == replication.RolePrimary {
		err = c.Metadata.ConfigureHAPrimaryJournal(ctx, claims.New, func(batch metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) {
			if q := r.peerPhysical.Load(); q != nil {
				if err := q.beforePrimarySource(batch.Context, 3, batch.FirstSequence, batch.LastSequence, len(batch.Frames)); err != nil {
					return nil, err
				}
			}
			return r.coordinator.ReserveNativeSourceUnconfirmedContext(batch.Context, len(batch.Frames))
		}, func(batch metadatajournal.DurableBatch) (uint64, error) {
			commit, e := r.coordinator.RecordMetadataBatch(batch)
			return commit.Index, e
		}, r.coordinator.WaitConfirmed)
	} else {
		f.metadataJournal, err = c.Metadata.ConfigureHAReplicaJournal(ctx, b.LedgerKey, claims.New)
	}
	if err != nil {
		return err
	}
	if initial.Authority.Instance != b.Evidence.Identity {
		return authenticatedstate.ErrSealIneligible
	}
	return nil
}

func (f *haSelectedRuntimeFoundation) readCacheFile(ctx context.Context, path string, visit func(*os.File, int64) error) (resultErr error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() {
		if err := ownedfile.CloseFile(file); err != nil {
			f.readDebt = file
			f.closeErr = errors.Join(f.closeErr, ownedfile.ErrRetirement, err)
			resultErr = errors.Join(resultErr, f.closeErr)
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() != info.Size() {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return visit(file, actual.Size())
}

type haFoundationCutScopeKey struct{}
type haFoundationCutScope struct {
	owner     *haSelectedRuntimeFoundation
	cut       statecut.HASelectedCut
	active    atomic.Bool
	mu        sync.Mutex
	borrowers sync.WaitGroup
	count     int
	failure   error
}

// The private context loan is minted only inside this owner's real
// Member/Ordering/source freeze. Nested native factories borrow that same
// freeze; they cannot replace it with a copied cut or escaped callback.
func (f *haSelectedRuntimeFoundation) withSelectedCut(ctx context.Context, visit func(context.Context, statecut.HASelectedCut) error) error {
	if scope, ok := ctx.Value(haFoundationCutScopeKey{}).(*haFoundationCutScope); ok && scope.owner == f {
		scope.mu.Lock()
		if !scope.active.Load() || scope.count >= 8 || ctx.Err() != nil || f.ctx.Err() != nil {
			scope.mu.Unlock()
			return authenticatedstate.ErrSealIneligible
		}
		scope.count++
		scope.borrowers.Add(1)
		scope.mu.Unlock()
		err := visit(ctx, scope.cut)
		scope.mu.Lock()
		scope.count--
		scope.failure = errors.Join(scope.failure, err)
		scope.mu.Unlock()
		scope.borrowers.Done()
		return err
	}
	f.lifecycle.Lock()
	if f.closing || f.activeCallbacks >= 4 {
		f.lifecycle.Unlock()
		return authenticatedstate.ErrClosed
	}
	credit, err := f.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		f.lifecycle.Unlock()
		return err
	}
	f.activeCallbacks++
	f.callbacks.Add(1)
	f.lifecycle.Unlock()
	defer func() {
		f.lifecycle.Lock()
		f.activeCallbacks--
		if err := credit.Release(); err != nil {
			f.closeErr = errors.Join(f.closeErr, err)
			f.fees = append(f.fees, credit)
		}
		f.lifecycle.Unlock()
		f.callbacks.Done()
	}()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	qualify := func(ctx context.Context, visit func(context.Context, statecut.HASelectedCut) error) error {
		if job, ok := ctx.Value(haSelectedConfirmedJobKey{}).(*haSelectedConfirmedJob); ok && job.owner == f && f.replication != nil && f.replication.receiver != nil {
			return statecut.WithHAConfirmedSelectedCut(ctx, f.owners, f.replication.receiver, job.prefix, visit)
		}
		if f.peerPromotionRecovery != nil {
			return statecut.WithHAPeerPromotionSelectedCut(ctx, f.owners, visit)
		}
		return statecut.WithHASelectedCut(ctx, f.owners, visit)
	}
	return qualify(ctx, func(ctx context.Context, cut statecut.HASelectedCut) (resultErr error) {
		scope := &haFoundationCutScope{owner: f, cut: cut}
		scope.active.Store(true)
		defer func() {
			scope.mu.Lock()
			scope.active.Store(false)
			scope.mu.Unlock()
			scope.borrowers.Wait()
			scope.mu.Lock()
			resultErr = errors.Join(resultErr, scope.failure)
			scope.mu.Unlock()
		}()
		return visit(context.WithValue(ctx, haFoundationCutScopeKey{}, scope), cut)
	})
}

// Invoked only by the actual selected native's live source job, after it owns
// native.mu. This callback grants only the actual single Ledger read prefix.
func (f *haSelectedRuntimeFoundation) withLiveLedgerAuthority(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error) error {
	// The actual native source job already owns its fixed IO source workspace;
	// these four bounded callback slots retain only scalar lifecycle metadata.
	f.lifecycle.Lock()
	if f.closing || f.activeCallbacks >= 4 {
		f.lifecycle.Unlock()
		return authenticatedstate.ErrClosed
	}
	f.activeCallbacks++
	f.callbacks.Add(1)
	f.lifecycle.Unlock()
	defer func() { f.lifecycle.Lock(); f.activeCallbacks--; f.lifecycle.Unlock(); f.callbacks.Done() }()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	if job, ok := ctx.Value(haSelectedConfirmedJobKey{}).(*haSelectedConfirmedJob); ok && job.owner == f {
		return statecut.WithHAConfirmedLedgerApplyAuthority(ctx, f.owners, f.replication.receiver, job.prefix, visit)
	}
	return statecut.WithHALedgerReadAuthority(ctx, f.owners, visit)
}

func (f *haSelectedRuntimeFoundation) withLiveGovernanceAuthority(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
	return f.withGovernanceAuthority(ctx, false, visit)
}

func (f *haSelectedRuntimeFoundation) withConfirmedGovernanceAuthority(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
	return f.withGovernanceAuthority(ctx, true, visit)
}

func (f *haSelectedRuntimeFoundation) withGovernanceAuthority(ctx context.Context, applying bool, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
	f.lifecycle.Lock()
	if f.closing || f.activeCallbacks >= 4 {
		f.lifecycle.Unlock()
		return authenticatedstate.ErrClosed
	}
	f.activeCallbacks++
	f.callbacks.Add(1)
	f.lifecycle.Unlock()
	defer func() { f.lifecycle.Lock(); f.activeCallbacks--; f.lifecycle.Unlock(); f.callbacks.Done() }()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	if applying {
		job, ok := ctx.Value(haSelectedConfirmedJobKey{}).(*haSelectedConfirmedJob)
		if !ok || job == nil || job.owner != f || f.replication == nil || f.replication.receiver == nil {
			return authenticatedstate.ErrSealIneligible
		}
		return statecut.WithHAGovernanceConfirmedApplyAuthority(ctx, f.owners, f.replication.receiver, job.prefix, visit)
	}
	return statecut.WithHAGovernanceReadAuthority(ctx, f.owners, visit)
}
