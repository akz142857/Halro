package statecut

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// PublishHA publishes all seven actual roots under independent Member,
// Ordering and four-source permission. It never manufactures Standalone
// permission for a replicated writer. All actual component freezes remain
// held until the durable common publication and authority rechecks finish.
func PublishHA(ctx context.Context, o HAOwners, base authenticatedstate.CandidateSeal) (Publication, error) {
	return publishHA(ctx, o, base, nil)
}

func publishHA(ctx context.Context, o HAOwners, base authenticatedstate.CandidateSeal, loan func(context.Context, authenticatedstate.CandidateSeal, func(context.Context) error) error) (result Publication, resultErr error) {
	resultErr = WithHASealAuthority(ctx, o, func(ctx context.Context, authority authenticatedstate.SealEligibility) error {
		var err error
		result, err = publishHAComponentsFrozen(ctx, o.Components, base, authority, loan)
		return err
	})
	if resultErr != nil {
		result.Seal = authenticatedstate.CandidateSeal{}
	}
	return
}

func publishHAComponentsFrozen(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, loan func(context.Context, authenticatedstate.CandidateSeal, func(context.Context) error) error) (result Publication, resultErr error) {
	if o.Budget == nil || o.Guard == nil || o.Publisher == nil || o.NativeLedger == nil || o.Lease == nil || o.Ledger == nil || o.Directory == nil || o.Usage == nil || o.NativeGovernance == nil || o.Governance == nil || o.Audit == nil || o.Metadata == nil || o.InstanceID == "" || o.KeyGeneration == 0 || authority.Instance == ([32]byte{}) || (authority.Role != authenticatedstate.SealPrimary && authority.Role != authenticatedstate.SealReplica) || authority.Coverage != authenticatedstate.SealCoverageAuthenticated || authority.Cluster == ([32]byte{}) || authority.Member == ([32]byte{}) || authority.Term == 0 || authority.OrderingIndex == 0 || authority.OrderingOffset == 0 || authority.OrderingMAC == ([32]byte{}) {
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
	if _, err := o.Lease.ValidateOwner(o.NativeLedger); err != nil {
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
		return o.NativeGovernance.WithFrozenHead(ctx, o.Budget, func(ctx context.Context, governanceHead governance.Summary) error {
			return o.Audit.WithFrozenHead(ctx, o.Budget, func(ctx context.Context, auditHead audit.Summary) error {
				return o.Metadata.WithFrozenSealReference(ctx, o.Budget, authority.Instance, o.InstanceID, o.KeyGeneration, func(ctx context.Context, metadataRef metadatastate.AuthenticatedMetadataSealReference, nativeMetadata metadatajournal.Head) error {
					// Usage reads its Ledger receipt before the inner exclusive Ledger
					// root job; final Usage validation happens after that job exits.
					return o.Usage.WithFrozenTypedSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, usageRef usage.TypedUsageReference) error {
						return o.Ledger.WithFrozenSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, ledgerRef ledger.AuthenticatedLedgerSealReference) error {
							return o.Governance.WithFrozenTypedSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, governanceRef governance.TypedGovernanceReference) error {
								return o.Directory.WithFrozenSealReference(ctx, o.Budget, authority.Instance, func(ctx context.Context, directoryRef ledger.AuthenticatedDirectoryReference) error {
									if ledgerRef.Anchor.Head != head || ledgerRef.Anchor.Hash != hash || usageRef.Ledger != ledgerRef || governanceRef.Journal.Position.Sequence != governanceHead.Records || governanceRef.Journal.Position.Offset != uint64(governanceHead.Bytes) || governanceRef.Journal.Position.Hash != governanceHead.LastHash || metadataRef.Position.Sequence != nativeMetadata.Sequence || metadataRef.Position.Offset != uint64(nativeMetadata.Offset) || metadataRef.Position.Hash != nativeMetadata.Hash {
										return fmt.Errorf("HA frozen component root/native equality: %w", authenticatedstate.ErrSealIneligible)
									}
									auditRef, err := o.Audit.SealReference(ctx, o.Budget, auditHead)
									if err != nil {
										return err
									}
									ledgerPosition := authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: head.Generation, Sequence: head.Sequence, Offset: uint64(head.Offset), Hash: hash, Epoch: uint64(ledgerRef.Anchor.Epoch)}
									refs := []authenticatedstate.SealComponentReference{{Root: ledgerRef.Root, Position: ledgerPosition}, {Root: metadataRef.Root, Position: metadataRef.Position}, {Root: governanceRef.Journal.Root, Position: governanceRef.Journal.Position}, {Root: governanceRef.Projection.Root, Position: governanceRef.Projection.Position}, {Root: auditRef.Root, Position: auditRef.Position}, {Root: usageRef.Projection.Root, Position: usageRef.Projection.Position}, {Root: directoryRef.Root, Position: directoryRef.Position}}
									candidate, err := authenticatedstate.AssembleSealCandidate(base, authority, refs, HardLimits())
									if err != nil {
										return fmt.Errorf("HA complete seven-root candidate assembly: %w", err)
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
