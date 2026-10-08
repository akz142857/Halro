package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const HAMetadataBaselineDirectory = "ha-metadata-baseline"
const maxHAMetadataCarrierBytes = 32 << 10

type HAMetadataKeys struct{ Master, Ledger, Audit, Governance, Metadata []byte }

func (k HAMetadataKeys) validate() error {
	for _, key := range [][]byte{k.Master, k.Ledger, k.Audit, k.Governance, k.Metadata} {
		if len(key) != 32 {
			return errors.New("HA Metadata preparation requires all five independent 32-byte keys")
		}
	}
	return nil
}

type haMetadataCarrier struct {
	Version      uint8                          `json:"version"`
	SignedClaims []byte                         `json:"signed_claims"`
	Preparation  metadatastate.MigrationReceipt `json:"preparation"`
}

type HAMetadataPreparation struct {
	Candidate    *metadatastate.MigrationPreparation
	carrierOwner *ownedfile.Owner
	carrierFee   resourcebudget.Permit
	CarrierPath  string
	closeErr     error
	inventory    *diskinventory.Baseline
	workspaceFee resourcebudget.Permit
}

// HAMetadataPreparationFailure keeps actual cleanup-only owners reachable
// through callers such as the CLI that can return only an error.
type HAMetadataPreparationFailure struct {
	Owner *HAMetadataPreparation
	Cause error
}

func (e *HAMetadataPreparationFailure) Error() string { return e.Cause.Error() }
func (e *HAMetadataPreparationFailure) Unwrap() error { return e.Cause }

func (p *HAMetadataPreparation) Close() error {
	if p == nil {
		return nil
	}
	p.closeErr = errors.Join(p.closeErr, p.Candidate.Close(), p.inventory.Close())
	return p.closeErr
}
func (p *HAMetadataPreparation) Discard() error {
	if p == nil {
		return nil
	}
	p.closeErr = errors.Join(p.closeErr, p.Candidate.Discard(), p.inventory.Close())
	if p.closeErr != nil {
		return errors.Join(ownedfile.ErrRetirement, p.closeErr)
	}
	if err := p.carrierOwner.Retire(); err != nil {
		p.closeErr = errors.Join(ownedfile.ErrRetirement, err)
		return p.closeErr
	}
	if p.carrierFee != (resourcebudget.Permit{}) {
		p.closeErr = errors.Join(p.closeErr, p.carrierFee.Release())
		if p.closeErr == nil {
			p.carrierFee = resourcebudget.Permit{}
		}
	}
	return p.closeErr
}

// PrepareHAMetadataBaseline is the offline stopped-Primary producer. It reads
// the real member, verifies continuous ordering and all four exact native cuts,
// and signs only the private VerifiedArchive-derived owner's frozen receipt.
// It never establishes a new incarnation or claims network confirmation.
func PrepareHAMetadataBaseline(ctx context.Context, cfg config.Config, keys HAMetadataKeys, archive *backup.VerifiedArchive, workspace string, guard *lock.Lock, o metadatastate.BaselineOptions) (*HAMetadataPreparation, error) {
	return prepareHAMetadataBaseline(ctx, cfg, keys, archive, workspace, guard, o, HAMetadataPrepareOptions{}, false, nil)
}

func prepareHAMetadataBaseline(ctx context.Context, cfg config.Config, keys HAMetadataKeys, archive *backup.VerifiedArchive, workspace string, guard *lock.Lock, o metadatastate.BaselineOptions, options HAMetadataPrepareOptions, createWorkspace bool, phase func(string) error) (result *HAMetadataPreparation, resultErr error) {
	if cfg.Replication == nil || cfg.ResourceBudget == nil || o.Budget == nil || archive == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || filepath.Clean(workspace) != filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory) {
		return nil, errors.New("HA Metadata preparation requires the stopped member's actual lock, budget and reserved private workspace")
	}
	if o.Identity == ([32]byte{}) || o.InstanceID == "" || len(o.InstanceID) > 128 || o.KeyGeneration == 0 || o.MaxSourceBytes <= 0 || o.MaxDiskBytes <= 0 || o.MaxAppendBytes <= 0 || o.MaxAppendBytes > o.MaxDiskBytes {
		return nil, metadatastate.ErrSource
	}
	if err := keys.validate(); err != nil {
		return nil, err
	}
	member, clusterKey, err := readMemberState(cfg, keys.Master)
	if err != nil {
		return nil, err
	}
	defer clear(clusterKey[:])
	if member.Role != replication.RolePrimary || member.Term != member.PromisedTerm || member.DurableIndex == 0 {
		return nil, errors.New("HA Metadata preparation requires an established Primary with no pending term transition")
	}
	inspection, err := replication.InspectOrderingJournalWithLimit(ctx, cfg.OrderingJournalPath(), clusterKey[:], member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, member.DurableIndex, cfg.ResourceBudget.OrderingJournalDiskBytes)
	if err != nil {
		return nil, err
	}
	if inspection.Head.Index != member.DurableIndex || inspection.Head.Term != member.Term || inspection.Head.MAC != member.OrderingHeadMAC {
		return nil, errors.New("HA Metadata preparation requires member state at the complete continuous ordering head")
	}
	source, err := replication.NewNativeSource(replication.NativeSourceOptions{LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(), MetadataPath: filepathMetadataJournal(cfg), LedgerKey: keys.Ledger, AuditKey: keys.Audit, GovernanceKey: keys.Governance, MetadataKey: keys.Metadata, ProviderObjectDir: providerObjectSourceDir(cfg), LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg), ReaderBudget: o.Budget, ReaderRAMBytes: replicationReaderRAM(cfg, o.Budget)})
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, source.Close())
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.Discard())
		}
	}()
	claims := replication.MetadataBaselineClaims{Version: 1, ClusterID: member.ClusterID, Incarnation: member.Incarnation, Primary: member.NodeID, Term: member.Term, Index: member.DurableIndex, OrderingHead: member.OrderingHeadMAC, Cursors: inspection.Head.Stores, Members: []string{member.NodeID}}
	for _, peer := range member.Peers {
		claims.Members = append(claims.Members, peer.Name)
	}
	sort.Strings(claims.Members)
	for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
		cursor, err := source.Cursor(store)
		if err != nil {
			return nil, err
		}
		if cursor != claims.Cursors[store-replication.StoreLedger] {
			return nil, fmt.Errorf("HA Metadata preparation store %d is not at the exact old ordering cut", store)
		}
		claims.Heads[store-replication.StoreLedger], err = source.HeadAt(store, cursor)
		if err != nil {
			return nil, err
		}
	}
	old := claims.Cursors[replication.StoreMetadata-replication.StoreLedger]
	if archive.Summary().Metadata.MetadataJournalEpoch != old.Generation || archive.Summary().Metadata.MetadataJournalSequence != old.Sequence {
		return nil, errors.New("HA Metadata backup does not materialize the actual old Metadata cut")
	}
	journal, err := replication.OpenExistingOrderingJournalWithOptions(cfg.OrderingJournalPath(), clusterKey[:], member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, orderingJournalOptions(cfg))
	if err != nil {
		return nil, err
	}
	verifyErr := replication.ReconstructRangeEach(member.ClusterID, member.Incarnation, 1, member.DurableIndex, journal, source, func(replication.LocalCommit) error { return ctx.Err() })
	if err := errors.Join(verifyErr, journal.Close()); err != nil {
		return nil, err
	}
	// Revalidate the actual independently verified archive descriptor and
	// member after the complete native reconstruction, before any namespace
	// transition. Exported candidate receipts cannot supply these permissions.
	archiveDigest, err := archive.CiphertextDigest()
	if err != nil {
		return nil, err
	}
	archiveManifest, err := archive.ManifestDigest()
	if err != nil {
		return nil, err
	}
	freshMember, freshKey, err := readMemberState(cfg, keys.Master)
	clear(freshKey[:])
	if err != nil || !reflect.DeepEqual(member, freshMember) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return nil, errors.Join(err, metadatastate.ErrSource)
	}
	p := &HAMetadataPreparation{CarrierPath: filepath.Join(workspace, "carrier.json")}
	defer func() {
		if resultErr != nil {
			cleanup := p.Discard()
			resultErr = errors.Join(resultErr, cleanup)
			if cleanup != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
				result = p
				resultErr = &HAMetadataPreparationFailure{Owner: p, Cause: errors.Join(ownedfile.ErrRetirement, resultErr)}
			}
		}
	}()
	if err := haMetadataPreparePhase(phase, "prepare-source-qualified"); err != nil {
		return p, err
	}
	if createWorkspace {
		p.inventory, err = p.prepareWorkspace(ctx, cfg, workspace, guard, o.Budget, options, phase)
		if err != nil {
			return p, err
		}
	}
	p.Candidate, err = metadatastate.PrepareFormat2FromBackup(ctx, archive, keys.Ledger, keys.Metadata, cfg.Storage.DataDir, workspace, guard, o, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		return p, err
	}
	if err := haMetadataPreparePhase(phase, "prepare-candidate-created"); err != nil {
		return p, err
	}
	// The actual owned freeze, not the exported mutable receipt, qualifies this
	// independently scoped baseline. No node-local rows receive business trust.
	err = p.Candidate.WithBaseline(ctx, func(b *metadatastate.Baseline) error {
		return b.WithFrozenSealReference(ctx, o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, func(_ context.Context, ref metadatastate.AuthenticatedMetadataSealReference, head metadatajournal.Head) error {
			if ref != p.Candidate.Receipt.Reference || head != p.Candidate.Receipt.Head || head.Sequence != 0 || p.Candidate.Receipt.SourceHead.Hash != claims.Heads[replication.StoreMetadata-replication.StoreLedger] {
				return metadatastate.ErrSource
			}
			claims.New = metadatajournal.BoundReplicationAuthority{Baseline: head.Baseline, FileRevision: head.FileRevision, MaxFileBytes: cfg.ResourceBudget.MetadataJournalDiskBytes}
			claims.HeaderHash = head.Hash
			return nil
		})
	})
	if err != nil {
		return p, err
	}
	p.carrierFee, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: maxHAMetadataCarrierBytes, Disk: maxHAMetadataCarrierBytes})
	if err != nil {
		return p, err
	}
	receipt := p.Candidate.Receipt
	preparationBytes, err := json.Marshal(receipt)
	if err != nil {
		return p, err
	}
	if receipt.Evidence.CiphertextDigest != archiveDigest || receipt.Evidence.ManifestDigest != archiveManifest {
		return p, metadatastate.ErrSource
	}
	claims.PreparationDigest = sha256.Sum256(preparationBytes)
	claims.BackupID, claims.ManifestDigest, claims.CiphertextDigest = receipt.Evidence.BackupID, receipt.Evidence.ManifestDigest, receipt.Evidence.CiphertextDigest
	signed, err := replication.SignMetadataBaselineCarrier(claims, clusterKey[:])
	if err != nil {
		return p, err
	}
	carrierBytes, err := json.Marshal(haMetadataCarrier{Version: 1, SignedClaims: signed, Preparation: receipt})
	if err != nil || len(carrierBytes) > maxHAMetadataCarrierBytes {
		return p, errors.Join(err, errors.New("HA Metadata carrier exceeds fixed bound"))
	}
	if err := haMetadataPreparePhase(phase, "prepare-carrier-ready"); err != nil {
		return p, err
	}
	file, owner, err := ownedfile.Create(p.CarrierPath, os.O_WRONLY, 0600)
	if err != nil {
		return p, err
	}
	p.carrierOwner = owner
	_, writeErr := file.Write(carrierBytes)
	if err := errors.Join(writeErr, file.Sync(), ownedfile.CloseFile(file), durable.SyncDirectory(workspace)); err != nil {
		return p, err
	}
	if err := haMetadataPreparePhase(phase, "prepare-carrier-synced"); err != nil {
		return p, err
	}
	if err := p.carrierFee.ReleasePart(resourcebudget.Cost{RAM: maxHAMetadataCarrierBytes}); err != nil {
		return p, err
	}
	result = p
	return result, nil
}

func readHAMetadataCarrier(ctx context.Context, path string, key []byte, member replication.MemberState) (haMetadataCarrier, *replication.MetadataBaselineAuthority, error) {
	return readHAMetadataCarrierWithQualifier(ctx, path, key, member, nil)
}
func readHAMetadataCarrierWithQualifier(ctx context.Context, path string, key []byte, member replication.MemberState, qualify func([]byte, []byte, replication.MemberState) (*replication.MetadataBaselineAuthority, error)) (haMetadataCarrier, *replication.MetadataBaselineAuthority, error) {
	if err := ctx.Err(); err != nil {
		return haMetadataCarrier{}, nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return haMetadataCarrier{}, nil, err
	}
	info, err := file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !info.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(info, named) || info.Size() <= 0 || info.Size() > maxHAMetadataCarrierBytes {
		return haMetadataCarrier{}, nil, errors.Join(err, nameErr, errors.New("HA Metadata carrier physical file is ineligible"), ownedfile.CloseFile(file))
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxHAMetadataCarrierBytes+1))
	after, statErr := file.Stat()
	named, nameErr = os.Lstat(path)
	closeErr := ownedfile.CloseFile(file)
	if readErr != nil || statErr != nil || nameErr != nil || closeErr != nil || !os.SameFile(info, after) || !os.SameFile(info, named) || after.Size() != info.Size() || named.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
		return haMetadataCarrier{}, nil, errors.Join(readErr, statErr, nameErr, closeErr, ownedfile.ErrReplaced)
	}
	var c haMetadataCarrier
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, nil, errors.New("HA Metadata carrier contains trailing data")
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, raw) {
		return c, nil, errors.New("HA Metadata carrier is not canonical")
	}
	if c.Version != 1 {
		return c, nil, errors.New("unsupported HA Metadata carrier version")
	}
	if qualify == nil {
		qualify = replication.QualifyMetadataBaselineCarrier
	}
	authority, err := qualify(c.SignedClaims, key, member)
	if err != nil {
		return c, nil, err
	}
	prepared, err := json.Marshal(c.Preparation)
	claims := authority.Claims()
	if err != nil || sha256.Sum256(prepared) != claims.PreparationDigest {
		return c, nil, errors.Join(err, errors.New("HA Metadata preparation receipt differs from independent carrier"))
	}
	if c.Preparation.Head.Baseline != claims.New.Baseline || c.Preparation.Head.Hash != claims.HeaderHash || c.Preparation.Head.FileRevision != claims.New.FileRevision || c.Preparation.Head.Sequence != 0 || c.Preparation.Evidence.BackupID != claims.BackupID || c.Preparation.Evidence.CiphertextDigest != claims.CiphertextDigest || c.Preparation.Evidence.ManifestDigest != claims.ManifestDigest {
		return c, nil, errors.New("HA Metadata carrier and preparation disagree")
	}
	return c, authority, ctx.Err()
}
