package app

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

type selectedSeedBuildOwner struct {
	owners      statecut.HAOwners
	fees        []resourcebudget.Permit
	bootstrap   *haMemoryStateBootstrap
	publication statecut.Publication
	failure     error
	inventory   *diskinventory.Baseline
	growth      *runtimeLedgerAppendDiskOwner
}
type selectedSeedBuildFailure struct {
	Owner *selectedSeedBuildOwner
	Cause error
}

func (e *selectedSeedBuildFailure) Error() string {
	return fmt.Sprintf("selected seed cold target: %v", e.Cause)
}
func (e *selectedSeedBuildFailure) Unwrap() error { return e.Cause }
func (o *selectedSeedBuildOwner) Close() error {
	if o == nil {
		return nil
	}
	if o.failure != nil {
		return o.failure
	}
	c := o.owners.Components
	for _, close := range []func() error{
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
			if c.Metadata != nil {
				return c.Metadata.Close()
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
			if c.Directory != nil {
				return c.Directory.Close()
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
			if c.NativeLedger != nil {
				return c.NativeLedger.Close()
			}
			return nil
		},
		func() error {
			if c.Audit != nil {
				return c.Audit.Close()
			}
			return nil
		},
		func() error {
			if c.NativeGovernance != nil {
				return c.NativeGovernance.Close()
			}
			return nil
		},
		func() error {
			if o.owners.Source != nil {
				return o.owners.Source.Close()
			}
			return nil
		},
		func() error {
			if o.owners.Ordering != nil {
				return o.owners.Ordering.Close()
			}
			return nil
		},
		func() error {
			if c.Publisher != nil {
				return c.Publisher.Close()
			}
			return nil
		},
	} {
		if e := close(); e != nil {
			o.failure = e
			return e
		}
	}
	if err := o.inventory.Close(); err != nil {
		o.failure = err
		return err
	}
	if err := o.growth.close(); err != nil {
		o.failure = err
		return err
	}
	if o.owners.Member != nil {
		o.owners.Member.Close()
	}
	o.bootstrap.clearKeys()
	return nil
}

// This constructor is never a business source. Only the authenticated complete
// format2 Seed declaration can supply its expected original Metadata root.
// It builds independent cold target owners and joins them before publication.
func buildSelectedSeedReplica(ctx context.Context, cfg config.Config, final config.Config, m SeedManifest, master, key []byte, guard *lock.Lock, manager *resourcebudget.Manager) (result replication.MemberState, resultErr error) {
	o := &selectedSeedBuildOwner{}
	defer func() {
		if e := o.Close(); e != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, e)
		}
		if resultErr != nil {
			resultErr = &selectedSeedBuildFailure{o, resultErr}
		}
	}()
	if err := validateSelectedSeedDeclaration(cfg, m, key); err != nil {
		return result, err
	}
	s := m.Selected

	archiveSuffix, err := selectedSeedArchiveSuffix(cfg)
	if err != nil {
		return result, err
	}
	promotion, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), key, manager)
	if errors.Is(err, os.ErrNotExist) {
		promotion = nil
		err = nil
	}
	if err != nil {
		return result, err
	}
	var qualify func([]byte, []byte, replication.MemberState) (*replication.MetadataBaselineAuthority, error)
	if promotion != nil {
		qualify = func(raw, key []byte, _ replication.MemberState) (*replication.MetadataBaselineAuthority, error) {
			return replication.QualifyMetadataBaselineCarrier(raw, key, promotion.Claims().Before)
		}
	}
	sourceCfg := cfg
	sourceCfg.Storage.DataDir = s.SourceDataDir
	sourceReplication := *cfg.Replication
	sourceReplication.NodeID = m.SourceNode
	sourceCfg.Replication = &sourceReplication
	carrier, authority, err := readHAMetadataCarrierWithQualifier(ctx, filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "carrier.json"), key, s.SourceMember, qualify)
	if err != nil || authority == nil || authority.Descriptor().AuthorityDigest != s.BaselineDigest {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	v, err := vault.New(master)
	if err != nil {
		return result, err
	}
	defer v.Close()
	raw, err := readBoundedRegularFile(ctx, haMemoryStateBootstrapPath(cfg), memoryStateBootstrapLimit)
	if err != nil {
		return result, err
	}
	plain, err := v.DecryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, raw)
	clear(raw)
	if err != nil {
		return result, err
	}
	defer clear(plain)
	if len(plain) > memoryStateBootstrapLimit {
		return result, authenticatedstate.ErrLimit
	}
	b := &haMemoryStateBootstrap{}
	o.bootstrap = b
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(b); err != nil {
		return result, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return result, authenticatedstate.ErrCorrupt
	}

	limits := cfg.ResourceBudget
	acquire := func(n int64) (resourcebudget.Permit, error) {
		p, e := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: n})
		if e == nil {
			o.fees = append(o.fees, p)
		}
		return p, e
	}
	if _, err = acquire(limits.OrderingJournalDiskBytes); err != nil {
		return result, err
	}
	o.owners.Ordering, err = replication.OpenExistingOrderingJournalWithOptions(cfg.OrderingJournalPath(), key, m.ClusterID, m.Incarnation, m.Index, s.SourceMember.OrderingHeadMAC, orderingJournalOptions(cfg))
	if err != nil {
		return result, err
	}
	if promotion != nil {
		authority, err = replication.QualifyMetadataBaselineAfterPromotion(carrier.SignedClaims, key, s.SourceMember, promotion, o.owners.Ordering)
		if err != nil {
			return result, err
		}
	}
	if err = b.validate(sourceCfg, s.SourceMember, authority, carrier.Preparation.Evidence); err != nil {
		return result, err
	}
	result = s.SourceMember
	result.NodeID = m.TargetNode
	result.Role = replication.RoleReplica
	result.Peers = nil
	for _, peer := range cfg.Replication.Peers {
		result.Peers = append(result.Peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	if err = result.Validate(); err != nil {
		return result, err
	}
	if err = authority.ValidateMember(result); err != nil {
		return result, err
	}
	pressure := &resourcePressureRuntime{budget: manager}
	o.growth, err = prepareRuntimeLedgerAppendDisk(ctx, cfg, pressure)
	if err != nil {
		return result, err
	}
	o.inventory, err = preparePersistentInventory(ctx, cfg, guard, pressure)
	if err != nil {
		return result, err
	}
	if err = o.growth.publish(); err != nil {
		return result, err
	}
	c := &o.owners.Components
	*c = statecut.StandaloneOwners{Budget: manager, Guard: guard, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration}
	rootDisk, err := acquire(limits.MetadataStoreDiskBytes)
	if err != nil {
		return result, err
	}
	journalDisk, err := acquire(limits.MetadataJournalDiskBytes)
	if err != nil {
		return result, err
	}
	nativeKey, err := hkdf.Key(sha256.New, b.LedgerKey, b.Binding.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return result, err
	}
	defer clear(nativeKey)
	o.owners.Baseline = authority
	o.owners.Source, err = replication.NewNativeSource(replication.NativeSourceOptions{LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(), MetadataPath: filepathMetadataJournal(cfg), LedgerKey: b.LedgerKey, AuditKey: b.AuditKey, GovernanceKey: b.GovernanceKey, MetadataKey: b.LegacyMetadataKey, ProviderObjectDir: providerObjectSourceDir(cfg), LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg), ReaderBudget: manager, ReaderRAMBytes: replicationReaderRAM(cfg, manager) + replication.MetadataBaselineReaderRAMBytes, ReserveLedgerFileDisk: o.growth.reserveNativeFile, MetadataBaseline: &replication.MetadataBaselineSourceOptions{Authority: authority, Path: filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal"), Key: nativeKey}})
	if err != nil {
		return result, err
	}
	if err = o.owners.Source.ActivateMetadataBaseline(o.owners.Ordering, authority.Claims().Index+1); err != nil {
		return result, err
	}
	if err = replication.ReconstructRangeEach(m.ClusterID, m.Incarnation, 1, m.Index, o.owners.Ordering, o.owners.Source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
		return result, err
	}
	var metadataRoot authenticatedstate.SealRoot
	for _, r := range s.SourceSeal.Roots[:s.SourceSeal.RootCount] {
		if r.Component == authenticatedstate.SealComponentMetadata {
			metadataRoot = r
		}
	}
	pos := s.SourceSeal.Positions[1]
	if metadataRoot.Component != authenticatedstate.SealComponentMetadata || s.MetadataHead.Epoch != pos.Epoch || s.MetadataHead.Sequence != pos.Sequence || uint64(s.MetadataHead.Offset) != pos.Offset || s.MetadataHead.Hash != pos.Hash || s.MetadataHead.FileRevision != 1 || s.MetadataHead.TrimmedThrough != 0 {
		return result, authenticatedstate.ErrSealIneligible
	}
	saved := authenticatedstate.RootIdentity{Scope: metadataRoot.State.Scope, Cut: metadataRoot.State.Cut, Digest: metadataRoot.State.Digest, ID: metadataRoot.State.PackID, Revision: metadataRoot.State.Revision, Rows: metadataRoot.Rows}
	appendBytes := func(n int64) int64 { return min(n/2, 16<<20) }
	c.Metadata, _, err = metadatastate.OpenFromRoot(ctx, b.LedgerKey, filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root"), filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal"), metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: b.Evidence.Identity, InstanceID: b.Evidence.InstanceID, KeyGeneration: b.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: appendBytes(limits.MetadataStoreDiskBytes)}, Evidence: b.Evidence, SavedRoot: saved, SelectedSealRoot: &metadataRoot, SavedHead: s.MetadataHead, NativeHead: s.MetadataHead, PermittedHead: s.MetadataHead, RootDiskReservation: rootDisk, JournalDiskReservation: journalDisk, MaxJournalBytes: authority.Claims().New.MaxFileBytes})
	if err != nil {
		return result, err
	}
	if err = c.Metadata.VerifyHAMaintenanceRoot(ctx); err != nil {
		return result, err
	}
	report, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, cfg.LedgerPath(), b.LedgerKey, nil)
	if err != nil || partial || report.ChecksumOnly != 0 {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	c.NativeLedger, err = ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{Replica: true, ChainKey: b.LedgerKey, ReserveAppendDisk: o.growth.reserve, ReserveNativeFileDisk: o.growth.reserveNativeFile, MaxSegmentDirectoryIndexBytes: ledgerSegmentIndexShareBytes(cfg)})
	if err != nil {
		return result, err
	}
	c.Lease, err = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
	if err != nil {
		return result, err
	}
	if _, err = acquire(limits.AuditLogDiskBytes); err != nil {
		return result, err
	}
	if _, err = acquire(limits.GovernanceLogDiskBytes); err != nil {
		return result, err
	}
	c.Audit, err = openConfiguredAuditLog(cfg, cfg.AuditPath(), b.AuditKey, audit.Options{Replica: true, MaxDiskBytes: limits.AuditLogDiskBytes}, manager)
	if err != nil {
		return result, err
	}
	c.NativeGovernance, err = governance.OpenWithOptions(cfg.GovernancePath(), b.GovernanceKey, governance.Options{Replica: true, MaxDiskBytes: limits.GovernanceLogDiskBytes, CacheBudget: manager, CacheRAMBytes: limits.GovernanceLogRAMBytes})
	if err != nil {
		return result, err
	}
	// Retain approved originals in the private staging tree. No contents here
	// license deletion, and a previously present target namespace is refused.
	original := haMemoryStateDirectory(cfg)
	archive := original + archiveSuffix
	if err = renameMigrationWithoutReplace(original, archive); err != nil {
		return result, err
	}
	if err = durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return result, err
	}
	if err = selectedSeedFault(ctx, "source_preserved"); err != nil {
		return result, err
	}
	if err = os.Mkdir(original, 0700); err != nil {
		return result, err
	}
	c.Ledger, err = ledger.BuildPackedAuthenticatedLedgerProjection(ctx, cfg.LedgerPath(), b.LedgerKey, filepath.Join(original, "ledger"), report, ledger.AuthenticatedLedgerOptions{Identity: b.Evidence.Identity, Budget: manager, MaxDiskBytes: limits.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerProjectionDiskBytes)})
	if err != nil {
		return result, err
	}
	c.Usage, _, err = usage.BuildPackedTypedHAUsageProjection(ctx, usage.AuthenticatedUsageOptions{Native: c.NativeLedger, Owner: c.Ledger, Identity: b.Evidence.Identity, UsageKey: b.LedgerKey, Budget: manager, RootDirectory: filepath.Join(original, "usage"), RootMaxDiskBytes: limits.UsageProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.UsageProjectionDiskBytes), ScratchPath: filepath.Join(original, "usage-reducer.db"), ScratchMaxDiskBytes: limits.UsageProjectionDiskBytes}, c.Lease)
	if err != nil {
		return result, err
	}
	c.Governance, _, err = governance.BuildPackedTypedHAGovernanceProjection(ctx, governance.AuthenticatedGovernanceOptions{Native: c.NativeGovernance, Identity: b.Evidence.Identity, RootKey: b.GovernanceKey, Budget: manager, RootDirectory: filepath.Join(original, "governance"), RootMaxDiskBytes: limits.GovernanceProjectionDiskBytes, RootMaxAppendBytes: appendBytes(limits.GovernanceProjectionDiskBytes), ProjectionPath: filepath.Join(original, "governance-reducer.db"), ProjectionMaxDiskBytes: limits.GovernanceProjectionDiskBytes})
	if err != nil {
		return result, err
	}
	c.Directory, err = ledger.BuildHAAuthenticatedDirectory(ctx, c.NativeLedger, c.Lease, c.Ledger, filepath.Join(original, "directory"), ledger.AuthenticatedDirectoryOptions{Identity: b.Evidence.Identity, Budget: manager, MaxDiskBytes: limits.LedgerSegmentIndexDiskBytes, MaxAppendBytes: appendBytes(limits.LedgerSegmentIndexDiskBytes)})
	if err != nil {
		return result, err
	}
	if err = preserveSelectedSeedReceivedControls(ctx, cfg, archive); err != nil {
		return result, err
	}
	if err = replication.WriteState(cfg.ReplicationStatePath(), result, key); err != nil {
		return result, err
	}
	o.owners.Member, err = replication.NewStatePublisher(cfg.ReplicationStatePath(), key, result)
	if err != nil {
		return result, err
	}
	sealDisk, err := acquire(32 << 10)
	if err != nil {
		return result, err
	}
	c.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, sealDisk)
	if err != nil {
		return result, err
	}
	if s.SourceSeal.Generation == ^uint64(0) {
		return result, authenticatedstate.ErrSealSequence
	}
	base := s.SourceSeal
	base.Generation++
	base.Revision = 1
	o.publication, err = statecut.PublishHA(ctx, o.owners, base)
	if err != nil {
		return result, err
	}
	if o.publication.Seal.Role != authenticatedstate.SealReplica || o.publication.Seal.Member != sha256.Sum256([]byte(result.NodeID)) || o.publication.Seal.OrderingIndex != m.Index {
		return result, authenticatedstate.ErrSealIneligible
	}
	if err = selectedSeedFault(ctx, "replica_seal_durable"); err != nil {
		return result, err
	}
	if _, err = acquire(2*memoryStateBootstrapLimit + haMemoryUnlockLimit + 4096); err != nil {
		return result, err
	}
	if err = preserveSelectedSeedGroupControls(ctx, cfg, archive); err != nil {
		return result, err
	}
	if err = bindSelectedSeedClusterKey(ctx, cfg, m, master, key, archiveSuffix); err != nil {
		return result, err
	}
	if err = bindSelectedSeedBootstrap(ctx, cfg, final, m, b, authority, result, master, v, archiveSuffix); err != nil {
		return result, err
	}
	if err = selectedSeedFault(ctx, "bootstrap_bound"); err != nil {
		return result, err
	}
	return result, nil
}
