package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// HAMemoryRuntimeActivation owns one fresh offline maintenance generation.
// Its transport is never served. Close joins descriptors and preserves all
// selected/native files and their persistent fees; it never rolls back facts.
type HAMemoryRuntimeActivation struct {
	Receipt                        authenticatedstate.SealPublicationReceipt `json:"receipt"`
	BootstrapPath                  string                                    `json:"bootstrap_path"`
	mu                             sync.Mutex
	maintenance                    *HAMetadataMaintenance
	owners                         statecut.HAOwners
	caches                         statecut.StartupCacheCandidates
	bootstrap                      haMemoryStateBootstrap
	reopened                       *statecut.ReopenedHA
	fees                           []resourcebudget.Permit
	work                           resourcebudget.Permit
	bootstrapOwner, bootstrapAlias *ownedfile.Owner
	bootstrapFile                  *os.File
	bootstrapFileUncertain         bool
	bootstrapBytes                 []byte
	unlock                         *haMemoryUnlockOwner
	closed                         bool
	closeErr                       error
}

type HAMemoryActivationFailure struct {
	Owner *HAMemoryRuntimeActivation
	Cause error
}

func (e *HAMemoryActivationFailure) Error() string { return e.Cause.Error() }
func (e *HAMemoryActivationFailure) Unwrap() error { return e.Cause }

func (a *HAMemoryRuntimeActivation) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return a.closeErr
	}
	a.closed = true
	if err := a.unlock.Close(); err != nil {
		a.closeErr = errors.Join(a.closeErr, err)
		return a.closeErr
	}
	if a.bootstrapFile != nil && !a.bootstrapFileUncertain {
		if err := ownedfile.CloseFile(a.bootstrapFile); err != nil {
			a.bootstrapFileUncertain = true
			a.closeErr = errors.Join(a.closeErr, err, ownedfile.ErrRetirement)
		} else {
			a.bootstrapFile = nil
		}
	}
	if a.closeErr != nil {
		return a.closeErr
	}
	c := a.owners.Components
	for _, closeOwner := range []func() error{
		func() error {
			if a.reopened != nil {
				return a.reopened.Close()
			}
			return nil
		},
		func() error {
			if c.Usage != nil {
				return c.Usage.Close()
			}
			return nil
		},
		func() error {
			if c.Governance != nil {
				return c.Governance.Close()
			}
			return nil
		},
		func() error {
			if c.Directory != nil {
				return c.Directory.Close()
			}
			return nil
		},
		func() error {
			if c.Ledger != nil {
				return c.Ledger.Close()
			}
			return nil
		},
		func() error {
			if c.Lease != nil {
				return c.Lease.Close()
			}
			return nil
		},
		func() error {
			if a.caches.Audit != nil {
				return a.caches.Audit.Close()
			}
			return nil
		},
		func() error {
			if a.caches.Governance != nil {
				return a.caches.Governance.Close()
			}
			return nil
		},
		func() error {
			if c.Publisher != nil {
				return c.Publisher.Close()
			}
			return nil
		},
		func() error {
			if a.maintenance != nil {
				return a.maintenance.Close()
			}
			return nil
		},
	} {
		if err := closeOwner(); err != nil {
			// A derived job may still borrow its native source. Keep the parent
			// keys, descriptors and original work credit until that real owner
			// can prove retirement; Close errors do not authorize the next join.
			a.closeErr = err
			return err
		}
	}
	if a.closeErr != nil {
		return a.closeErr
	}
	if a.work != (resourcebudget.Permit{}) {
		a.closeErr = a.work.Release()
		if a.closeErr != nil {
			return a.closeErr
		}
		a.work = resourcebudget.Permit{}
	}
	a.bootstrap.clearKeys()
	clear(a.bootstrapBytes)
	a.bootstrapBytes = nil
	return nil
}

// ActivateHAMemoryRuntime is a separate offline step AFTER the real group has
// confirmed/applied the format2 boundary. This initial transition requires all
// three ordering positions to equal that boundary; an excluded suffix cannot
// enter first-build roots. Later Runtime reopening uses its role-permitted cut.
func ActivateHAMemoryRuntime(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (*HAMemoryRuntimeActivation, error) {
	return activateHAMemoryRuntime(ctx, cfg, keys, guard, manager, logger, nil)
}

type HAMemoryActivationOptions struct{ Restart bool }

func ActivateHAMemoryRuntimeWithOptions(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, options HAMemoryActivationOptions) (*HAMemoryRuntimeActivation, error) {
	return activateHAMemoryRuntimeWithOptions(ctx, cfg, keys, guard, manager, logger, options, nil)
}

func activateHAMemoryRuntime(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, phase func(string) error) (result *HAMemoryRuntimeActivation, resultErr error) {
	return activateHAMemoryRuntimeWithOptions(ctx, cfg, keys, guard, manager, logger, HAMemoryActivationOptions{}, phase)
}

func activateHAMemoryRuntimeWithOptions(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, options HAMemoryActivationOptions, phase func(string) error) (result *HAMemoryRuntimeActivation, resultErr error) {
	if cfg.Replication == nil || cfg.ResourceBudget == nil || guard == nil || manager == nil || logger == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := keys.validate(); err != nil {
		return nil, err
	}
	if options.Restart {
		if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "active.seal")); err == nil {
			return resumeHAMemoryPublishedActivation(ctx, cfg, keys, guard, manager, logger)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	a := &HAMemoryRuntimeActivation{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, a.Close())
			result = a
			resultErr = &HAMemoryActivationFailure{Owner: a, Cause: resultErr}
		}
	}()
	for _, path := range []string{memoryStateBootstrapPath(cfg), haMemoryStateBootstrapPath(cfg), haMemoryStateBootstrapPath(cfg) + ".pending", haMemoryStateDirectory(cfg), filepath.Join(cfg.Storage.DataDir, "active.seal"), filepath.Join(cfg.Storage.DataDir, "active.seal.pending")} {
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) && !(options.Restart && (path == haMemoryStateDirectory(cfg) || path == haMemoryStateBootstrapPath(cfg)+".pending" || path == filepath.Join(cfg.Storage.DataDir, "active.seal.pending"))) {
			return nil, errors.Join(os.ErrExist, err)
		}
	}
	var err error
	a.work, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: 4 * memoryStateBootstrapLimit})
	if err != nil {
		return nil, err
	}
	a.maintenance, err = openHAMetadataMaintenance(ctx, cfg, keys, guard, manager, logger, resourcebudget.Permit{}, resourcebudget.Permit{})
	if err != nil {
		return nil, err
	}
	m := a.maintenance
	member, report, err := qualifyHAMemoryActivationSources(ctx, m, manager, keys.Ledger)
	if err != nil {
		return nil, err
	}
	carrier, _, err := readHAMetadataCarrier(ctx, filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "carrier.json"), m.member.clusterKey[:], member)
	if err != nil {
		return nil, err
	}
	identity := carrier.Preparation.Evidence.Identity
	a.bootstrap = haMemoryStateBootstrap{Version: 2, ClusterID: member.ClusterID, Incarnation: member.Incarnation, NodeID: member.NodeID, SourceAuthorityDigest: m.authority.Descriptor().AuthorityDigest, Evidence: carrier.Preparation.Evidence, Binding: m.authority.Claims().New.Baseline, LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(), OrderingPath: cfg.OrderingJournalPath(), MetadataRoot: filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root"), MetadataJournal: filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal"), LedgerKey: bytes.Clone(keys.Ledger), AuditKey: bytes.Clone(keys.Audit), GovernanceKey: bytes.Clone(keys.Governance), LegacyMetadataKey: bytes.Clone(keys.Metadata)}
	if err := a.bootstrap.validate(cfg, member, m.authority, carrier.Preparation.Evidence); err != nil {
		return nil, err
	}
	v, err := vault.New(keys.Master)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(m.metadata, v); err != nil {
		return nil, err
	}
	for _, source := range []struct {
		key  []byte
		load func() ([]byte, error)
	}{
		{keys.Ledger, func() ([]byte, error) { return loadLedgerHMACKey(m.metadata, v, keys.Master) }},
		{keys.Audit, func() ([]byte, error) { return loadAuditHMACKey(m.metadata, v, keys.Master) }},
		{keys.Metadata, func() ([]byte, error) { return loadMetadataJournalHMACKey(m.metadata, v, keys.Master) }},
		{keys.Governance, func() ([]byte, error) { return vault.DeriveGovernanceHMACKey(keys.Ledger) }},
	} {
		actual, err := source.load()
		matched := bytes.Equal(actual, source.key)
		clear(actual)
		if err != nil || !matched {
			return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
	}
	checkpoint := func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if phase != nil {
			return phase(name)
		}
		return nil
	}
	if err := checkpoint("sources-qualified"); err != nil {
		return nil, err
	}
	// Even an empty native stream must use the keys certified by the actual
	// Vault. Authenticate every independent key before moving opaque inputs.
	if options.Restart {
		fee, err := preserveHAMemoryActivationCandidates(ctx, cfg, m, keys.Ledger, guard, manager)
		if fee != (resourcebudget.Permit{}) {
			a.fees = append(a.fees, fee)
		}
		if err != nil {
			return nil, err
		}
	}
	acquire := func(disk int64) (resourcebudget.Permit, error) {
		fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: disk})
		if err == nil {
			a.fees = append(a.fees, fee)
		}
		return fee, err
	}
	if _, err := acquire(4096 + 2*memoryStateBootstrapLimit); err != nil {
		return nil, err
	}
	directory := haMemoryStateDirectory(cfg)
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return nil, err
	}
	if err := checkpoint("namespace-created"); err != nil {
		return nil, err
	}
	sealFee, err := acquire(32 << 10)
	if err != nil {
		return nil, err
	}
	c := &a.owners.Components
	*c = statecut.StandaloneOwners{Budget: manager, Guard: guard, NativeLedger: m.nativeLedger, NativeGovernance: m.nativeGovernance, Audit: m.nativeAudit, Metadata: m.baseline, InstanceID: carrier.Preparation.Evidence.InstanceID, KeyGeneration: carrier.Preparation.Evidence.KeyGeneration}
	if err := createHAMemoryUnlockCarrier(ctx, a, keys.Master, manager, checkpoint); err != nil {
		return nil, err
	}
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		if err := checkpoint("unlock-carrier-synced"); err != nil {
			return nil, err
		}
	}
	a.owners.Member, a.owners.Ordering, a.owners.Source, a.owners.Baseline = m.member.publisher, m.member.journal, m.member.source, m.authority
	c.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, keys.Ledger, statecut.HardLimits(), manager, sealFee)
	if err != nil {
		return nil, err
	}
	c.Lease, err = m.nativeLedger.BeginReplicaReadMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	limits := cfg.ResourceBudget
	appendBytes := func(bytes int64) int64 { return min(bytes/2, 16<<20) }
	c.Ledger, err = ledger.BuildPackedAuthenticatedLedgerProjection(ctx, cfg.LedgerPath(), keys.Ledger, filepath.Join(directory, "ledger"), report, ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)})
	if err != nil {
		return nil, fmt.Errorf("build actual HA Ledger root: %w", err)
	}
	c.Usage, _, err = usage.BuildPackedTypedHAUsageProjection(ctx, usage.AuthenticatedUsageOptions{Native: c.NativeLedger, Owner: c.Ledger, Identity: identity, UsageKey: keys.Ledger, Budget: manager, RootDirectory: filepath.Join(directory, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes), ScratchPath: filepath.Join(directory, "usage-reducer.db"), ScratchMaxDiskBytes: limits.UsageProjectionDiskBytes}, c.Lease)
	if err != nil {
		return nil, fmt.Errorf("build actual HA Usage root: %w", err)
	}
	c.Governance, _, err = governance.BuildPackedTypedHAGovernanceProjection(ctx, governance.AuthenticatedGovernanceOptions{Native: c.NativeGovernance, Identity: identity, RootKey: keys.Governance, Budget: manager, RootDirectory: filepath.Join(directory, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes), ProjectionPath: filepath.Join(directory, "governance-reducer.db"), ProjectionMaxDiskBytes: limits.GovernanceProjectionDiskBytes})
	if err != nil {
		return nil, fmt.Errorf("build actual HA Governance root: %w", err)
	}
	c.Directory, err = ledger.BuildHAAuthenticatedDirectory(ctx, c.NativeLedger, c.Lease, c.Ledger, filepath.Join(directory, "directory"), ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes)})
	if err != nil {
		return nil, fmt.Errorf("build actual HA Directory root: %w", err)
	}
	a.caches, err = persistHAMemoryStartupCaches(ctx, *c, cfg.Storage.DataDir, identity, &a.fees)
	if err != nil {
		return nil, err
	}
	if err := checkpoint("seven-roots-built"); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(a.bootstrap)
	if err != nil {
		return nil, err
	}
	if len(plain) > memoryStateBootstrapLimit {
		clear(plain)
		return nil, authenticatedstate.ErrLimit
	}
	a.bootstrapBytes, err = v.EncryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, plain)
	clear(plain)
	if err != nil {
		return nil, err
	}
	if len(a.bootstrapBytes) > memoryStateBootstrapLimit {
		return nil, authenticatedstate.ErrLimit
	}
	a.bootstrapFile, a.bootstrapOwner, err = ownedfile.Create(haMemoryStateBootstrapPath(cfg)+".pending", os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := a.bootstrapFile.Write(a.bootstrapBytes); err != nil {
		return nil, err
	}
	if err := a.bootstrapFile.Sync(); err != nil {
		return nil, err
	}
	if err := a.bootstrapOwner.VerifyFile(a.bootstrapFile); err != nil {
		return nil, err
	}
	if err := ownedfile.CloseFile(a.bootstrapFile); err != nil {
		a.bootstrapFileUncertain = true
		a.closeErr = errors.Join(err, ownedfile.ErrRetirement)
		return nil, a.closeErr
	}
	a.bootstrapFile = nil
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return nil, err
	}
	if err := checkpoint("bootstrap-pending-synced"); err != nil {
		return nil, err
	}
	hard := statecut.HardLimits()
	publication, err := statecut.PublishHAWithStartupCaches(ctx, a.owners, authenticatedstate.CandidateSeal{Generation: 1, Revision: 1, MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight}, a.caches)
	a.Receipt = publication.Receipt
	if err != nil {
		return nil, err
	}
	if err := checkpoint("common-seal-published"); err != nil {
		return nil, err
	}
	err = a.bootstrapOwner.RenameCaptured(haMemoryStateBootstrapPath(cfg))
	if err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return nil, err
	}
	if err := checkpoint("bootstrap-activated"); err != nil {
		return nil, err
	}
	a.BootstrapPath = haMemoryStateBootstrapPath(cfg)
	return a, nil
}

func qualifyHAMemoryActivationSources(ctx context.Context, m *HAMetadataMaintenance, manager *resourcebudget.Manager, ledgerKey []byte) (member replication.MemberState, report ledger.ChainReport, resultErr error) {
	if m == nil || m.member == nil || m.authority == nil {
		return member, report, authenticatedstate.ErrSealIneligible
	}
	claims := m.authority.Claims()
	resultErr = m.member.publisher.WithReadAuthority(ctx, manager, func(ctx context.Context, actual replication.MemberState, authorize func(func() error) error) error {
		if actual.Term != claims.Term || actual.PromisedTerm != claims.Term || actual.ClusterID != claims.ClusterID || actual.Incarnation != claims.Incarnation || actual.Role != m.member.role || actual.Role == replication.RolePrimary && actual.NodeID != claims.Primary || actual.Role != replication.RolePrimary && actual.Role != replication.RoleReplica || actual.DurableIndex != claims.Index+1 || actual.ConfirmedIndex != actual.DurableIndex || actual.AppliedIndex != actual.DurableIndex || actual.Projection.Index != actual.AppliedIndex || actual.Projection.MetadataEpoch != claims.New.Baseline.Epoch || actual.Projection.MetadataSequence != 0 {
			return replication.ErrMemberAuthority
		}
		return m.member.journal.WithFrozenHead(ctx, manager, func(ctx context.Context, head replication.FrozenOrderingHead) error {
			if head.Index != actual.DurableIndex || head.MAC != actual.OrderingHeadMAC || head.Term != actual.Term {
				return replication.ErrMemberAuthority
			}
			boundary, err := m.member.journal.Record(claims.Index + 1)
			if err != nil {
				return err
			}
			if err := m.member.source.ValidateMetadataBaseline(boundary); err != nil {
				return err
			}
			cursors, err := m.member.journal.StoreCursorsThroughContext(ctx, actual.DurableIndex)
			if err != nil {
				return err
			}
			for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
				cursor, err := m.member.source.Cursor(store)
				if err != nil || cursor != cursors[store-replication.StoreLedger] {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				if _, err := m.member.source.HeadAt(store, cursor); err != nil {
					return err
				}
			}
			if err := replication.ReconstructRangeEach(actual.ClusterID, actual.Incarnation, 1, actual.DurableIndex, m.member.journal, m.member.source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
				return err
			}
			var partial bool
			report, partial, err = ledger.InspectReplayAuthenticatedContext(ctx, m.cfg.LedgerPath(), ledgerKey, nil)
			if err != nil || partial {
				return errors.Join(ledger.ErrCorrupt, err)
			}
			return authorize(func() error { member = actual; return ctx.Err() })
		})
	})
	return
}

func persistHAMemoryStartupCaches(ctx context.Context, c statecut.StandaloneOwners, directory string, identity [32]byte, fees *[]resourcebudget.Permit) (result statecut.StartupCacheCandidates, resultErr error) {
	ac, err := c.Audit.CaptureHAStartupCache(ctx, c.Budget, identity, c.Audit.Summary())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, ac.Close()) }()
	gc, err := c.NativeGovernance.CaptureHAStartupCache(ctx, c.Budget, identity, c.NativeGovernance.Summary())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, gc.Close()) }()
	ad, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: audit.MaxStartupCacheBytes})
	if err != nil {
		return result, err
	}
	*fees = append(*fees, ad)
	gd, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: governance.MaxStartupCacheBytes})
	if err != nil {
		return result, err
	}
	*fees = append(*fees, gd)
	result.Audit, err = ac.PersistNew(ctx, filepath.Join(directory, "memory-audit-1.cache"), audit.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, DiskReservation: ad, Identity: identity, Head: ac.Head()})
	if err != nil {
		return result, err
	}
	result.Governance, err = gc.PersistNew(ctx, filepath.Join(directory, "memory-governance-1.cache"), governance.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, DiskReservation: gd, Identity: identity, Head: gc.Head()})
	return result, err
}
