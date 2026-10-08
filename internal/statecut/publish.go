// Package statecut joins actual native/component freezes into a complete,
// durable common seal. Prepared owners are explicit; this package never makes
// an unauthenticated live database a baseline or enables Runtime restoration.
package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

// HardLimits is this caller's complete versioned namespace inventory, including
// both Governance roots and the Ledger directory. A caller cannot omit a root.
func HardLimits() authenticatedstate.SealHardLimits {
	return authenticatedstate.SealHardLimits{MaxPageBytes: 4 << 20, MaxValueBytes: authenticatedstate.MaxValueBytes, MaxTreeHeight: 32, RequiredRoots: []authenticatedstate.SealRootSchema{
		{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLedger, IndexID: ledger.AuthenticatedLedgerSealIndexID, Codec: ledger.AuthenticatedLedgerSealCodec},
		{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: metadatastate.AuthenticatedMetadataSealIndexID, Codec: metadatastate.AuthenticatedMetadataSealCodec},
		{Kind: authenticatedstate.SealRootNative, Component: authenticatedstate.SealComponentGovernance, IndexID: governance.AuthenticatedGovernanceSealIndexID, Codec: governance.AuthenticatedGovernanceSealCodec},
		{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentGovernance, IndexID: governance.AuthenticatedGovernanceProjectionSealIndexID, Codec: governance.TypedGovernanceSealCodec},
		{Kind: authenticatedstate.SealRootNative, Component: authenticatedstate.SealComponentAudit, IndexID: audit.AuthenticatedAuditSealIndexID, Codec: audit.AuthenticatedAuditSealCodec},
		{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentUsage, IndexID: usage.AuthenticatedUsageSealIndexID, Codec: usage.TypedUsageSealCodec},
		{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLogDirectory, IndexID: ledger.AuthenticatedDirectorySealIndexID, Codec: ledger.AuthenticatedDirectorySealCodec},
	}}
}

type StandaloneOwners struct {
	Budget           *resourcebudget.Manager
	Guard            *lock.Lock
	Publisher        *authenticatedstate.SealCandidatePublisher
	NativeLedger     *ledger.Log
	Lease            *ledger.MaintenanceLease
	Ledger           *ledger.AuthenticatedLedgerProjection
	Directory        *ledger.AuthenticatedDirectory
	Usage            *usage.DiskProjection
	NativeGovernance *governance.Log
	Governance       *governance.GovernanceProjection
	Audit            *audit.Log
	Metadata         *metadatastate.Baseline
	InstanceID       string
	KeyGeneration    uint64
}

// Publication retains the whole fixed job fee on unknown physical retirement.
// In that case Seal is zero, Receipt still reports any durable publication, and
// CleanupError remains terminal; no retry can turn this debt into a refund.
type Publication struct {
	Seal       authenticatedstate.CandidateSeal
	Receipt    authenticatedstate.SealPublicationReceipt
	debt       resourcebudget.Permit
	cleanupErr error
}

func (p Publication) CleanupError() error { return p.cleanupErr }

// PublishStandalone keeps all actual writers and complete roots excluded
// through Publish. authority must come from independently prepared identity,
// versions and exact allowed positions. Real native replication modes are
// checked inside their freezes; a caller cannot relabel HA owners standalone.
// It grants no business readiness, historical-audit or deletion permission.
func PublishStandalone(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility) (result Publication, resultErr error) {
	return publishStandalone(ctx, o, base, authority, nil)
}

func publishStandalone(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, beforePublish func(context.Context, authenticatedstate.CandidateSeal) error) (result Publication, resultErr error) {
	return publishStandaloneWithLoan(ctx, o, base, authority, beforePublish, nil)
}

func publishStandaloneWithLoan(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, beforePublish func(context.Context, authenticatedstate.CandidateSeal) error, loan func(context.Context, authenticatedstate.CandidateSeal, func(context.Context) error) error) (Publication, error) {
	return publishStandaloneFrozen(ctx, o, base, authority, false, beforePublish, loan)
}

func publishStandaloneFrozen(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, capturePositions bool, beforePublish func(context.Context, authenticatedstate.CandidateSeal) error, loan func(context.Context, authenticatedstate.CandidateSeal, func(context.Context) error) error) (result Publication, resultErr error) {
	if o.Budget == nil || o.Guard == nil || o.Publisher == nil || o.NativeLedger == nil || o.Lease == nil || o.Ledger == nil || o.Directory == nil || o.Usage == nil || o.NativeGovernance == nil || o.Governance == nil || o.Audit == nil || o.Metadata == nil || o.InstanceID == "" || o.KeyGeneration == 0 || authority.Instance == ([32]byte{}) || authority.Role != authenticatedstate.SealStandalone || authority.Coverage != authenticatedstate.SealCoverageAuthenticated || authority.Cluster != ([32]byte{}) || authority.Member != ([32]byte{}) || authority.Term != 0 || authority.OrderingIndex != 0 || authority.OrderingOffset != 0 || authority.OrderingMAC != ([32]byte{}) {
		return result, authenticatedstate.ErrSealIneligible
	}
	credit, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes + 128<<10})
	if err != nil {
		return result, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := credit.Release(); err == nil {
				if resultErr != nil {
					result.Seal = authenticatedstate.CandidateSeal{}
				}
				return
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
		result.Seal = authenticatedstate.CandidateSeal{}
		result.debt, result.cleanupErr = credit, resultErr
	}()
	if err := o.Publisher.ValidateOwner(o.Guard, o.Budget); err != nil {
		return result, err
	}
	if replicated, err := o.Lease.ValidateOwner(o.NativeLedger); err != nil || replicated {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err := o.Ledger.ValidateNativeSource(ctx, o.NativeLedger, o.Lease); err != nil {
		return result, err
	}
	if err := o.Usage.ValidateTypedSource(o.NativeLedger, o.Lease, o.Ledger, o.Budget); err != nil {
		return result, err
	}
	if err := o.Governance.ValidateTypedSource(o.NativeGovernance, o.Budget); err != nil {
		return result, err
	}
	resultErr = o.Lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		authenticateLedger := func(ctx context.Context) error {
			actual, err := o.NativeLedger.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
			if err != nil || actual != head {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			current, currentHash, _ := o.NativeLedger.ChainHead()
			if current != head || currentHash != hash {
				return authenticatedstate.ErrSealIneligible
			}
			return ctx.Err()
		}
		if err := authenticateLedger(ctx); err != nil {
			return err
		}
		return o.NativeGovernance.WithFrozenStandaloneHead(ctx, o.Budget, func(ctx context.Context, governanceHead governance.Summary) error {
			return o.Audit.WithFrozenStandaloneHead(ctx, o.Budget, func(ctx context.Context, auditHead audit.Summary) error {
				return o.Metadata.WithFrozenStandaloneSealReference(ctx, o.Budget, authority.Instance, o.InstanceID, o.KeyGeneration, func(ctx context.Context, metadataRef metadatastate.AuthenticatedMetadataSealReference, nativeMetadata metadatajournal.Head) error {
					// Usage reads its Ledger receipt before the inner exclusive Ledger
					// root job; final Usage validation happens after that job exits.
					return o.Usage.WithFrozenTypedSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, usageRef usage.TypedUsageReference) error {
						return o.Ledger.WithFrozenSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, ledgerRef ledger.AuthenticatedLedgerSealReference) error {
							return o.Governance.WithFrozenTypedSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, governanceRef governance.TypedGovernanceReference) error {
								return o.Directory.WithFrozenSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, directoryRef ledger.AuthenticatedDirectoryReference) error {
									if ledgerRef.Anchor.Head != head || ledgerRef.Anchor.Hash != hash || usageRef.Ledger != ledgerRef || governanceRef.Journal.Position.Sequence != governanceHead.Records || governanceRef.Journal.Position.Offset != uint64(governanceHead.Bytes) || governanceRef.Journal.Position.Hash != governanceHead.LastHash || metadataRef.Position.Sequence != nativeMetadata.Sequence || metadataRef.Position.Offset != uint64(nativeMetadata.Offset) || metadataRef.Position.Hash != nativeMetadata.Hash {
										return authenticatedstate.ErrSealIneligible
									}
									auditRef, err := o.Audit.SealReference(ctx, o.Budget, auditHead)
									if err != nil {
										return err
									}
									ledgerPosition := authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: head.Generation, Sequence: head.Sequence, Offset: uint64(head.Offset), Hash: hash, Epoch: uint64(ledgerRef.Anchor.Epoch)}
									refs := []authenticatedstate.SealComponentReference{{Root: ledgerRef.Root, Position: ledgerPosition}, {Root: metadataRef.Root, Position: metadataRef.Position}, {Root: governanceRef.Journal.Root, Position: governanceRef.Journal.Position}, {Root: governanceRef.Projection.Root, Position: governanceRef.Projection.Position}, {Root: auditRef.Root, Position: auditRef.Position}, {Root: usageRef.Projection.Root, Position: usageRef.Projection.Position}, {Root: directoryRef.Root, Position: directoryRef.Position}}
									if capturePositions {
										// Standalone positions come from all actual source/root freezes here.
										// Identity, incarnation and versions still come from independent selection.
										authority.Positions[0] = ledgerPosition
										authority.Positions[1] = metadataRef.Position
										authority.Positions[2] = governanceRef.Journal.Position
										authority.Positions[3] = auditRef.Position
										authority.Positions[4] = usageRef.Projection.Position
									}
									candidate, err := authenticatedstate.AssembleSealCandidate(base, authority, refs, HardLimits())
									if err != nil {
										return err
									}
									if beforePublish != nil {
										if err := beforePublish(ctx, candidate); err != nil {
											return err
										}
									}
									persist := func(ctx context.Context) error {
										if err := ctx.Err(); err != nil {
											return err
										}
										if err := authenticateLedger(ctx); err != nil {
											return err
										}
										var err error
										result.Receipt, err = o.Publisher.Publish(ctx, candidate)
										if err == nil {
											result.Seal = candidate
										}
										return err
									}
									if loan != nil {
										return loan(ctx, candidate, persist)
									}
									return persist(ctx)
								})
							})
						})
					})
				})
			})
		})
	})
	return result, resultErr
}
