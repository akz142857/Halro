package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// MemoryRuntimeActivation retains the physical candidate and all fees, also on
// partial publication. Close joins descriptors but preserves authoritative and
// selected files; a failed activation never reverts to an old database.
type MemoryRuntimeActivation struct {
	Receipt                        authenticatedstate.SealPublicationReceipt `json:"receipt"`
	BootstrapPath                  string                                    `json:"bootstrap_path"`
	owners                         statecut.StandaloneOwners
	preparation                    *metadatastate.MigrationPreparation
	caches                         statecut.StartupCacheCandidates
	fees                           []resourcebudget.Permit
	bootstrapOwner, bootstrapAlias *ownedfile.Owner
}

func (a *MemoryRuntimeActivation) Close() error {
	if a == nil {
		return nil
	}
	var err error
	for _, close := range []func() error{
		func() error {
			if a.owners.Usage != nil {
				return a.owners.Usage.Close()
			}
			return nil
		},
		func() error {
			if a.owners.Governance != nil {
				return a.owners.Governance.Close()
			}
			return nil
		},
		func() error {
			if a.owners.Directory != nil {
				return a.owners.Directory.Close()
			}
			return nil
		},
		func() error {
			if a.owners.Ledger != nil {
				return a.owners.Ledger.Close()
			}
			return nil
		},
		a.preparation.Close,
		func() error {
			if a.owners.Lease != nil {
				return a.owners.Lease.Close()
			}
			return nil
		},
		func() error {
			if a.owners.NativeLedger != nil {
				return a.owners.NativeLedger.Close()
			}
			return nil
		},
		func() error {
			if a.owners.NativeGovernance != nil {
				return a.owners.NativeGovernance.Close()
			}
			return nil
		},
		func() error {
			if a.owners.Audit != nil {
				return a.owners.Audit.Close()
			}
			return nil
		},
		func() error {
			if a.owners.Publisher != nil {
				return a.owners.Publisher.Close()
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
	} {
		err = errors.Join(err, close())
	}
	return err
}

// ActivatePreparedMemoryRuntime completes an explicit offline Standalone
// migration: authenticate current native sources, construct all seven roots,
// persist native caches, publish the common seal, then activate the encrypted
// bootstrap. The actual locked preparation must live at the reserved directory.
// The original bbolt DB and format1 journal remain as the migration backup; no
// ordinary startup automatically converts or trusts them.
func ActivatePreparedMemoryRuntime(ctx context.Context, cfg config.Config, p *metadatastate.MigrationPreparation, guard *lock.Lock, manager *resourcebudget.Manager, ledgerKey, sourceJournalKey []byte) (result *MemoryRuntimeActivation, resultErr error) {
	if cfg.Replication != nil || cfg.ResourceBudget == nil || p == nil || manager == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || len(ledgerKey) != 32 || len(sourceJournalKey) != 32 {
		return nil, authenticatedstate.ErrSealIneligible
	}
	a := &MemoryRuntimeActivation{preparation: p, owners: statecut.StandaloneOwners{Budget: manager, Guard: guard}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, a.Close())
			result = a
		}
	}()
	for _, path := range []string{memoryStateBootstrapPath(cfg), memoryStateBootstrapPath(cfg) + ".pending", filepath.Join(cfg.Storage.DataDir, "active.seal"), filepath.Join(cfg.Storage.DataDir, "active.seal.pending")} {
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(os.ErrExist, err)
		}
	}
	directory := memoryStateDirectory(cfg)
	if err := validateRuntimeDerivedPath(ctx, cfg, directory); err != nil {
		return nil, err
	}
	old, err := boltstore.OpenReadOnlyWithMappingBudget(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), manager)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, old.Close()) }()
	masterKey, err := unlockMasterKey(ctx, cfg, old)
	if err != nil {
		return nil, err
	}
	defer clear(masterKey)
	v, err := vault.New(masterKey)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(old, v); err != nil {
		return nil, err
	}
	actualLedgerKey, err := loadLedgerHMACKey(old, v, masterKey)
	if err != nil {
		return nil, err
	}
	defer clear(actualLedgerKey)
	actualJournalKey, err := loadMetadataJournalHMACKey(old, v, masterKey)
	if err != nil {
		return nil, err
	}
	defer clear(actualJournalKey)
	if !bytes.Equal(actualLedgerKey, ledgerKey) || !bytes.Equal(actualJournalKey, sourceJournalKey) {
		return nil, metadatastate.ErrSource
	}
	auditKey, err := loadAuditHMACKey(old, v, masterKey)
	if err != nil {
		return nil, err
	}
	defer clear(auditKey)
	govKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		return nil, err
	}
	defer clear(govKey)
	info, err := old.Info()
	if err != nil {
		return nil, err
	}
	if info.SchemaVersion != p.Receipt.Evidence.Metadata.SchemaVersion || info.MetadataJournalEpoch != p.Receipt.SourceHead.Epoch || info.MetadataJournalSequence != p.Receipt.SourceHead.Sequence {
		return nil, metadatastate.ErrSource
	}
	head, err := metadatajournal.ProbeExactContext(ctx, old.MetadataJournalPath(), sourceJournalKey, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil || head != p.Receipt.SourceHead {
		return nil, errors.Join(metadatastate.ErrSource, err)
	}
	if err := old.Close(); err != nil {
		return nil, err
	}
	limits := cfg.ResourceBudget
	appendBytes := func(ceiling int64) int64 { return min(ceiling/2, 16<<20) }
	identity := p.Receipt.Evidence.Identity
	a.owners.InstanceID, a.owners.KeyGeneration = p.Receipt.Evidence.InstanceID, p.Receipt.Evidence.KeyGeneration
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err == nil {
			a.fees = append(a.fees, fee)
		}
		return fee, err
	}
	sealFee, err := acquire(32 << 10)
	if err != nil {
		return nil, err
	}
	a.owners.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, ledgerKey, statecut.HardLimits(), manager, sealFee)
	if err != nil {
		return nil, err
	}
	report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), ledgerKey, nil)
	if err != nil || partial {
		return nil, errors.Join(ledger.ErrCorrupt, err)
	}
	if _, err := governance.Verify(cfg.GovernancePath(), govKey); err != nil {
		return nil, err
	}
	if _, err := audit.VerifyWithVisitorContext(ctx, cfg.AuditPath(), auditKey, limits.AuditLogDiskBytes, nil); err != nil {
		return nil, err
	}
	a.owners.NativeLedger, err = ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{ChainKey: ledgerKey})
	if err != nil {
		return nil, err
	}
	a.owners.Lease, err = a.owners.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	a.owners.Audit, err = audit.OpenWithOptions(cfg.AuditPath(), auditKey, audit.Options{CacheBudget: manager, CacheRAMBytes: max(limits.AuditLogRAMBytes, auditlimits.MinCacheRAMBytes), MaxDiskBytes: limits.AuditLogDiskBytes})
	if err != nil {
		return nil, err
	}
	a.owners.NativeGovernance, err = governance.OpenWithOptions(cfg.GovernancePath(), govKey, governance.Options{CacheBudget: manager, CacheRAMBytes: max(limits.GovernanceLogRAMBytes, governancelimits.MinCacheRAMBytes), MaxDiskBytes: limits.GovernanceLogDiskBytes})
	if err != nil {
		return nil, err
	}
	a.owners.Ledger, err = ledger.BuildPackedAuthenticatedLedgerProjection(ctx, cfg.LedgerPath(), ledgerKey, filepath.Join(directory, "ledger"), report, ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)})
	if err != nil {
		return nil, fmt.Errorf("prepare Ledger root (disk ceiling %d): %w", limits.LedgerProjectionDiskBytes, err)
	}
	a.owners.Usage, _, err = usage.BuildPackedTypedUsageProjection(ctx, usage.AuthenticatedUsageOptions{Native: a.owners.NativeLedger, Owner: a.owners.Ledger, Identity: identity, UsageKey: ledgerKey, Budget: manager, RootDirectory: filepath.Join(directory, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes), ScratchPath: filepath.Join(directory, "usage-reducer.db"), ScratchMaxDiskBytes: limits.UsageProjectionDiskBytes}, a.owners.Lease)
	if err != nil {
		return nil, fmt.Errorf("prepare Usage root (root and scratch disk ceiling %d): %w", limits.UsageProjectionDiskBytes, err)
	}
	a.owners.Governance, _, err = governance.BuildPackedTypedGovernanceProjection(ctx, governance.AuthenticatedGovernanceOptions{Native: a.owners.NativeGovernance, Identity: identity, RootKey: govKey, Budget: manager, RootDirectory: filepath.Join(directory, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes), ProjectionPath: filepath.Join(directory, "governance-reducer.db"), ProjectionMaxDiskBytes: limits.GovernanceProjectionDiskBytes})
	if err != nil {
		return nil, fmt.Errorf("prepare Governance root (root and scratch disk ceiling %d): %w", limits.GovernanceProjectionDiskBytes, err)
	}
	a.owners.Directory, err = ledger.BuildAuthenticatedDirectory(ctx, a.owners.NativeLedger, a.owners.Lease, a.owners.Ledger, filepath.Join(directory, "directory"), ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes)})
	if err != nil {
		return nil, err
	}
	static := authenticatedstate.SealEligibility{Instance: identity, SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated}
	if _, err := rand.Read(static.Incarnation[:]); err != nil {
		return nil, err
	}
	b := memoryStateBootstrap{Version: 1, Authority: static, InstanceID: a.owners.InstanceID, KeyGeneration: a.owners.KeyGeneration, Evidence: p.Receipt.Evidence, Binding: p.Receipt.Head.Baseline, LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(), LedgerKey: ledgerKey, AuditKey: auditKey, GovernanceKey: govKey}
	plain, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	encrypted, err := v.EncryptCredential("system:memory-state-bootstrap", "system", "halro:memory-state:bootstrap:v1", plain)
	if err != nil {
		return nil, err
	}
	defer clear(encrypted)
	if len(encrypted) > memoryStateBootstrapLimit {
		return nil, authenticatedstate.ErrLimit
	}
	if _, err := acquire(2 * memoryStateBootstrapLimit); err != nil {
		return nil, err
	}
	file, owner, err := ownedfile.Create(memoryStateBootstrapPath(cfg)+".pending", os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	a.bootstrapOwner = owner
	_, writeErr := file.Write(encrypted)
	if err := errors.Join(writeErr, file.Sync(), owner.VerifyFile(file), ownedfile.CloseFile(file), durable.SyncDirectory(cfg.Storage.DataDir)); err != nil {
		return nil, err
	}
	err = p.WithBaseline(ctx, func(baseline *metadatastate.Baseline) error {
		if err := baseline.ValidatePublicationPaths(filepath.Join(directory, "metadata-root"), filepath.Join(directory, "metadata.journal")); err != nil {
			return err
		}
		a.owners.Metadata = baseline
		authority, err := statecut.CurrentStandaloneAuthority(ctx, a.owners, static)
		if err != nil {
			return err
		}
		a.caches, err = persistRuntimeStartupCaches(ctx, a.owners, cfg.Storage.DataDir, 1, identity, &a.fees)
		if err != nil {
			return err
		}
		hard := statecut.HardLimits()
		publication, err := statecut.PublishStandaloneWithStartupCaches(ctx, a.owners, authenticatedstate.CandidateSeal{Generation: 1, Revision: 1, MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight}, authority, a.caches)
		a.Receipt = publication.Receipt
		return err
	})
	if err != nil {
		return nil, err
	}
	a.bootstrapAlias, err = owner.LinkCaptured(memoryStateBootstrapPath(cfg))
	if err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return nil, err
	}
	if err := owner.Retire(); err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return nil, err
	}
	a.BootstrapPath = memoryStateBootstrapPath(cfg)
	return a, nil
}

func persistRuntimeStartupCaches(ctx context.Context, o statecut.StandaloneOwners, directory string, slot uint64, identity [32]byte, fees *[]resourcebudget.Permit) (result statecut.StartupCacheCandidates, resultErr error) {
	ac, err := o.Audit.CaptureStartupCache(ctx, o.Budget, identity)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, ac.Close()) }()
	gc, err := o.NativeGovernance.CaptureStartupCache(ctx, o.Budget, identity)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, gc.Close()) }()
	ad, err := o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: audit.MaxStartupCacheBytes})
	if err != nil {
		return result, err
	}
	*fees = append(*fees, ad)
	gd, err := o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: governance.MaxStartupCacheBytes})
	if err != nil {
		return result, err
	}
	*fees = append(*fees, gd)
	result.Audit, err = ac.PersistNew(ctx, filepath.Join(directory, fmt.Sprintf("memory-audit-%d.cache", slot)), audit.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, DiskReservation: ad, Identity: identity, Head: ac.Head()})
	if err != nil {
		return result, err
	}
	result.Governance, err = gc.PersistNew(ctx, filepath.Join(directory, fmt.Sprintf("memory-governance-%d.cache", slot)), governance.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, DiskReservation: gd, Identity: identity, Head: gc.Head()})
	return result, err
}
