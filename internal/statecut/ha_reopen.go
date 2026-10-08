package statecut

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

// HAReopenOptions borrows real native/member/ordering owners. Components only
// supplies paths, keys and reconciled persistent permits; its Authority and
// Sources are ignored. No candidate/header may replace those independent owners.
type HAReopenOptions struct {
	Sources          HAOwners
	Components       ReopenOptions
	PhysicalRecovery *ledger.PhysicalStartupRecoveryOptions
}

// ReopenedHA owns reopened derived roots, including a separate read-only actual
// selected Metadata descriptor. Native sources, lease and publisher stay borrowed.
// This first recovery stage requires full-cut equality and grants no business,
// pending suffix, promotion or replication admission.
type ReopenedHA struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	metadataSource    *metadatastate.Baseline
	metadataBorrowed  bool
	advanceErr        error
	metadataStore     *boltstore.Store
	closed            bool
	closeComplete     bool
	closeJoinedSource bool
	closeErr          error
	Owners            HAOwners
	Seal              authenticatedstate.CandidateSeal
	LedgerReceipt     ledger.AuthenticatedLedgerReopen
	MetadataReceipt   metadatastate.ReopenReceipt
	Derivative        *usage.LedgerDerivative
	debt              resourcebudget.Permit
	cleanupErr        error
}

func (r *ReopenedHA) Close() error {
	return r.close(false)
}

// CloseWithMetadataSource joins the foundation's original Metadata source
// after its derived facade, before withdrawing the generation's paid directory
// workspace. The source remains borrowed in the ordinary Close API.
func (r *ReopenedHA) CloseWithMetadataSource(source *metadatastate.Baseline) error {
	if r == nil || source == nil || source != r.metadataSource {
		return authenticatedstate.ErrSealIneligible
	}
	return r.close(true)
}

func (r *ReopenedHA) close(joinMetadataSource bool) (resultErr error) {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeComplete {
		if joinMetadataSource && !r.closeJoinedSource {
			// A prior borrower-only Close has already withdrawn its pool.
			// Still join the actual source; its own qualification must hold
			// unselected targets if no paid read can now be obtained.
			r.closeErr = errors.Join(r.closeErr, r.metadataSource.CloseContext(context.Background()))
			r.closeJoinedSource = true
		}
		return r.closeErr
	}
	defer func() {
		r.closeComplete, r.closeJoinedSource, r.closeErr = true, joinMetadataSource, resultErr
	}()
	r.closed = true
	c := r.Owners.Components
	var err error
	stage := func(name string, cause error) error {
		if cause == nil {
			return nil
		}
		return fmt.Errorf("close HA selected %s: %w", name, cause)
	}
	withDirectoryWorkspace := func(closeOwner func(context.Context) error) error {
		entered := false
		join := func(ctx context.Context) error {
			entered = true
			return closeOwner(ctx)
		}
		var cause error
		if c.Directory != nil {
			cause = c.Directory.WithPhysicalCatchupWorkspace(context.Background(), join)
		} else {
			cause = join(context.Background())
		}
		if !entered {
			// Refusal cannot skip the actual join or grant physical retirement.
			cause = errors.Join(cause, join(context.Background()))
		}
		return cause
	}
	if r.metadataStore != nil {
		if err := withDirectoryWorkspace(r.metadataStore.CloseContext); err != nil {
			return errors.Join(r.cleanupErr, stage("Metadata DAO", err))
		}
	}
	if r.Derivative != nil {
		r.Derivative.Close()
	}
	if c.Usage != nil {
		err = errors.Join(err, stage("Usage", c.Usage.Close()))
	}
	if c.Governance != nil {
		err = errors.Join(err, stage("Governance", c.Governance.Close()))
	}
	if c.Metadata != nil && !r.metadataBorrowed {
		err = errors.Join(err, stage("Metadata", c.Metadata.Close()))
	}
	if joinMetadataSource {
		err = errors.Join(err, stage("Metadata source", withDirectoryWorkspace(r.metadataSource.CloseContext)))
	}
	if c.Ledger != nil {
		// The final current-Selection MAC read belongs to the original
		// admitted generation. Keep its paid directory pool alive until the
		// projection has joined, including when pressure forbids new IO.
		err = errors.Join(err, stage("Ledger", withDirectoryWorkspace(c.Ledger.CloseContext)))
	}
	if c.Directory != nil {
		err = errors.Join(err, stage("Directory", c.Directory.Close()))
	}
	return errors.Join(err, r.cleanupErr)
}

// HAReopenFailure keeps actual unknown cleanup owners and whole job fees
// reachable. Owner is closed for semantic use and may only be retired again.
type HAReopenFailure struct {
	Owner *ReopenedHA
	Cause error
}

func (e *HAReopenFailure) Error() string { return e.Cause.Error() }
func (e *HAReopenFailure) Unwrap() error { return e.Cause }

func ReopenHA(ctx context.Context, o HAReopenOptions) (*ReopenedHA, error) {
	return reopenHAMode(ctx, o, true, false)
}

// ReopenHAPrefix restores only the actual saved common cut, even when Primary
// has durable unconfirmed facts or Replica has a received/unapplied suffix.
// Every complete native tail authenticates; no suffix is reduced or admitted
// to business. A separate joined advance/confirmation handoff is required.
func ReopenHAPrefix(ctx context.Context, o HAReopenOptions) (*ReopenedHA, error) {
	return reopenHAMode(ctx, o, false, false)
}

// ReopenHASelectedGeneration restores actual selected native owners and advances
// derived Ledger/Usage/Governance/Directory roots only to their independently
// permitted physical heads. Metadata stays at its saved root until joined handoff.
// No common publication or business readiness is granted.
func ReopenHASelectedGeneration(ctx context.Context, o HAReopenOptions) (*ReopenedHA, error) {
	return reopenHAMode(ctx, o, false, true)
}

func reopenHAMode(ctx context.Context, o HAReopenOptions, fullCut, selectedGeneration bool) (result *ReopenedHA, resultErr error) {
	if o.PhysicalRecovery != nil && !selectedGeneration {
		return nil, authenticatedstate.ErrSealIneligible
	}
	s, p := o.Sources, o.Components
	c := s.Components
	if c.Budget == nil || c.Guard == nil || c.Publisher == nil || c.NativeLedger == nil || c.Lease == nil || c.NativeGovernance == nil || c.Audit == nil || c.Metadata == nil || s.Baseline == nil || c.InstanceID == "" || c.KeyGeneration == 0 || c.Ledger != nil || c.Usage != nil || c.Governance != nil || c.Directory != nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if replicated, err := c.Lease.ValidateOwner(c.NativeLedger); err != nil || !replicated {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	identity := s.Baseline.Claims().New.Baseline.Identity
	if p.LedgerOptions.Identity != identity || p.LedgerOptions.Budget != c.Budget || p.DirectoryOptions.Identity != identity || p.DirectoryOptions.Budget != c.Budget || p.UsageOptions.Identity != identity || p.UsageOptions.Budget != c.Budget || p.GovernanceOptions.Identity != identity || p.GovernanceOptions.Budget != c.Budget || p.MetadataOptions.Identity != identity || p.MetadataOptions.Budget != c.Budget || p.MetadataOptions.InstanceID != c.InstanceID || p.MetadataOptions.KeyGeneration != c.KeyGeneration {
		return nil, authenticatedstate.ErrSealIneligible
	}
	credit, err := c.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes + 128<<10})
	if err != nil {
		return nil, err
	}
	ownerCtx, cancel := context.WithCancel(context.Background())
	r := &ReopenedHA{Owners: s, ctx: ownerCtx, cancel: cancel, metadataSource: c.Metadata}
	// Never adopt the maintenance baseline's possibly newer physical root.
	r.Owners.Components.Metadata = nil
	defer func() {
		if resultErr != nil {
			cleanup := r.Close()
			resultErr = errors.Join(resultErr, cleanup)
			if cleanup != nil {
				resultErr = errors.Join(ownedfile.ErrRetirement, resultErr)
			}
			result = nil
		}
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := credit.Release(); err == nil {
				return
			} else {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err, r.Close())
			}
		}
		r.cleanupErr, r.debt = resultErr, credit
		result = r
		resultErr = &HAReopenFailure{Owner: r, Cause: resultErr}
	}()
	stage := "Member/Ordering/carrier/four-source common selection"
	qualify := func(ctx context.Context, visit func(context.Context, HASelectedCut) error) error {
		if !selectedGeneration {
			return WithHASelectedCut(ctx, s, visit)
		}
		return c.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed ledger.HASelectedLedgerPosition) error {
			return withBorrowedHASelectedCut(ctx, s, func(ctx context.Context, cut HASelectedCut) error {
				if cut.Authority != a || cut.Allowed[0].Generation != allowed.Generation || cut.Allowed[0].Sequence != allowed.Sequence || cut.Heads[0] != allowed.Hash {
					return authenticatedstate.ErrSealIneligible
				}
				return visit(ctx, cut)
			})
		})
	}
	resultErr = qualify(ctx, func(ctx context.Context, cut HASelectedCut) error {
		stage = "selected role-permitted full cut and seven root schemas"
		selected, a := cut.Seal, cut.Authority
		// Full-cut activation recovery first. Incremental business recovery needs a
		// distinct joined suffix handoff before it can widen this equality.
		if fullCut && selected.OrderingIndex != cut.AllowedIndex {
			return authenticatedstate.ErrSealIneligible
		}
		hard := HardLimits()
		if selected.RootCount != uint8(len(hard.RequiredRoots)) {
			return authenticatedstate.ErrSealIneligible
		}
		for i, schema := range hard.RequiredRoots {
			root := selected.Roots[i]
			if (authenticatedstate.SealRootSchema{Kind: root.Kind, Component: root.Component, IndexID: root.IndexID, Codec: root.Codec}) != schema {
				return authenticatedstate.ErrSealIneligible
			}
		}
		lp, mp := selected.Positions[0], selected.Positions[1]
		if lp.Offset > math.MaxInt64 || lp.Epoch > math.MaxUint8 || lp.Generation == 0 || lp.Revision != 0 || mp.Offset > math.MaxInt64 || selected.Positions[2].Offset > math.MaxInt64 || selected.Positions[3].Offset > math.MaxInt64 {
			return authenticatedstate.ErrSealIneligible
		}
		r.Seal = selected
		anchor := ledger.ReplayAnchor{Head: ledger.Watermark{Generation: lp.Generation, Sequence: lp.Sequence, Offset: int64(lp.Offset)}, Hash: lp.Hash, Epoch: uint8(lp.Epoch)}
		open := func(ctx context.Context) error {
			return c.Audit.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, _ audit.Summary) error {
				stage = "actual selected Audit root"
				ar, err := c.Audit.SealReference(ctx, c.Budget, audit.Summary{Records: selected.Positions[3].Sequence, Bytes: int64(selected.Positions[3].Offset), LastHash: selected.Positions[3].Hash})
				if err != nil || ar.Root != selected.Roots[4] {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				stage = "actual selected Ledger root"
				ledgerRoot := selected.Roots[0]
				lo := ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: p.LedgerOptions, SavedAnchor: anchor, SelectedSealRoot: &ledgerRoot, DiskReservation: p.LedgerDisk}
				lo.Prefix = &ledger.ProjectionPrefix{Head: anchor.Head, Hash: anchor.Hash}
				sourceOptions := ledger.SelectedLedgerProjectionOpenOptions{Publisher: c.Publisher, Guard: c.Guard, Authority: a, PhysicalRecovery: o.PhysicalRecovery}
				if selectedGeneration {
					var actual ledger.ReplayAnchor
					err = c.NativeLedger.WithHASelectedStartupOwner(ctx, c.Budget, sourceOptions, identity, func(ctx context.Context, source ledger.SelectedStartupSource) error {
						actual = source.NativeHead()
						return source.Check(ctx)
					})
					if err != nil || actual.Head.Generation != cut.Allowed[0].Generation || actual.Head.Sequence != cut.Allowed[0].Sequence || actual.Hash != cut.Heads[0] {
						return errors.Join(authenticatedstate.ErrSealIneligible, err)
					}
					lo.Prefix = &ledger.ProjectionPrefix{Head: actual.Head, Hash: actual.Hash}
					stage = "HA Ledger native factory"
					r.Owners.Components.Ledger, r.LedgerReceipt, err = ledger.OpenHASelectedAuthenticatedLedgerProjection(ctx, c.NativeLedger, p.LedgerDirectory, lo, sourceOptions)
				} else {
					r.Owners.Components.Ledger, r.LedgerReceipt, err = ledger.OpenAuthenticatedLedgerProjection(ctx, c.NativeLedger, p.LedgerDirectory, lo)
				}
				if err != nil {
					return err
				}
				stage = "actual reopened Ledger reference"
				lr, err := r.Owners.Components.Ledger.SealReference(ctx)
				if err != nil || !selectedGeneration && (lr.Root != ledgerRoot || lr.Anchor != anchor) {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				stage = "actual selected Usage root"
				uo := p.UsageOptions
				uo.Native, uo.Owner = c.NativeLedger, r.Owners.Components.Ledger
				savedLedger := lr
				if selectedGeneration {
					savedLedger.Root, savedLedger.Anchor = ledgerRoot, anchor
				}
				ur := usage.TypedUsageReference{Ledger: savedLedger, Projection: usage.AuthenticatedUsageSealReference{Schema: hard.RequiredRoots[5], Root: selected.Roots[5], Scope: selected.Roots[5].State.Scope, Position: selected.Positions[4]}}
				if selectedGeneration {
					r.Derivative, r.Owners.Components.Usage, err = usage.OpenHASelectedTypedDiskLedgerDerivative(ctx, uo, c.Lease, ur, p.UsageDisk, sourceOptions)
				} else {
					r.Derivative, r.Owners.Components.Usage, err = usage.OpenTypedDiskLedgerDerivative(ctx, uo, c.Lease, ur, p.UsageDisk)
				}
				if err != nil {
					return err
				}
				stage = "actual selected Governance roots"
				goOpts := p.GovernanceOptions
				goOpts.Native = c.NativeGovernance
				gr := governance.TypedGovernanceReference{Journal: governance.GovernanceSealReference{Schema: hard.RequiredRoots[2], Root: selected.Roots[2], Position: selected.Positions[2]}, Projection: governance.AuthenticatedGovernanceProjectionReference{Schema: hard.RequiredRoots[3], Root: selected.Roots[3], Scope: selected.Roots[3].State.Scope, Position: selected.Positions[2]}}
				if selectedGeneration {
					r.Owners.Components.Governance, _, err = governance.OpenHASelectedTypedGovernanceProjection(ctx, goOpts, governance.SelectedTypedGovernanceOpenOptions{Publisher: c.Publisher, Guard: c.Guard, Authority: a, Selected: gr, DiskReservation: p.GovernanceDisk})
				} else {
					r.Owners.Components.Governance, err = governance.OpenTypedGovernanceProjection(ctx, goOpts, gr, p.GovernanceDisk)
				}
				if err != nil {
					return err
				}
				stage = "actual selected Metadata bound head"
				mo := p.MetadataOptions
				binding := s.Baseline.Claims().New
				// Complete header binding comes from signed carrier. The selected source
				// position was already checked against Ordering and the real native FD.
				head := mo.NativeHead
				if head.Baseline != binding.Baseline || head.FileRevision != binding.FileRevision || head.Epoch != mp.Epoch || head.Sequence < mp.Sequence || head.Offset < int64(mp.Offset) || fullCut && (head.Sequence != mp.Sequence || head.Offset != int64(mp.Offset) || head.Hash != mp.Hash) {
					return authenticatedstate.ErrSealIneligible
				}
				savedHead := head
				savedHead.Sequence, savedHead.Offset, savedHead.Hash = mp.Sequence, int64(mp.Offset), mp.Hash
				mo.SavedHead, mo.PermittedHead = savedHead, savedHead
				stage = "actual selected Metadata read-only content root"
				r.Owners.Components.Metadata, r.MetadataReceipt, err = metadatastate.OpenHASelectedReadRootFromSource(ctx, p.MetadataKey, p.MetadataDirectory, p.MetadataJournalPath, mo, binding, c.Publisher, c.Guard, a, c.Metadata)
				if err != nil {
					return err
				}
				mr, err := r.Owners.Components.Metadata.SealReference(ctx)
				if err != nil || mr.Root != selected.Roots[1] || mr.Position != mp {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				stage = "actual selected Ledger directory root"
				dr := ledger.AuthenticatedDirectoryReference{Schema: hard.RequiredRoots[6], Root: selected.Roots[6], Position: lp}
				if selectedGeneration {
					r.Owners.Components.Directory, err = ledger.OpenHASelectedAuthenticatedDirectory(ctx, c.NativeLedger, dr, p.DirectoryPath, p.DirectoryOptions, sourceOptions)
				} else {
					r.Owners.Components.Directory, err = ledger.OpenHAAuthenticatedDirectory(ctx, c.NativeLedger, c.Lease, dr, p.DirectoryPath, p.DirectoryOptions)
				}
				if err != nil {
					return err
				}
				stage = "complete actual Ledger source recheck"
				if !selectedGeneration {
					if err := c.Lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
						if fullCut && (head != anchor.Head || hash != anchor.Hash) {
							return authenticatedstate.ErrSealIneligible
						}
						actual, err := c.NativeLedger.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
						if err != nil || actual != head {
							return errors.Join(authenticatedstate.ErrSealIneligible, err)
						}
						return ctx.Err()
					}); err != nil {
						return err
					}
				}
				stage = "final common publication recheck"
				current, found, err := c.Publisher.SelectContext(ctx, a)
				if err != nil || !found || current != selected {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				return ctx.Err()
			})
		}
		if selectedGeneration {
			return open(ctx)
		}
		return c.NativeGovernance.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, _ governance.Summary) error { return open(ctx) })
	})
	if resultErr != nil {
		resultErr = fmt.Errorf("HA selected reopen %s: %w", stage, resultErr)
	}
	if resultErr == nil {
		result = r
	}
	return result, resultErr
}
