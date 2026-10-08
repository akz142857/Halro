package statecut

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

// ReopenOptions supplies independently established startup identity/cut,
// trusted keys and reconciled persistent fees. Metadata's backup/epoch/trim
// evidence remains explicit; a live database is never a source of trust.
type ReopenOptions struct {
	Sources                                StandaloneOwners
	Authority                              authenticatedstate.SealEligibility
	LedgerDirectory                        string
	LedgerOptions                          ledger.AuthenticatedLedgerOptions
	LedgerDisk                             resourcebudget.Permit
	DirectoryPath                          string
	DirectoryOptions                       ledger.AuthenticatedDirectoryOptions
	UsageOptions                           usage.AuthenticatedUsageOptions
	UsageDisk                              resourcebudget.Permit
	GovernanceOptions                      governance.AuthenticatedGovernanceOptions
	GovernanceDisk                         resourcebudget.Permit
	MetadataDirectory, MetadataJournalPath string
	MetadataKey                            []byte
	MetadataOptions                        metadatastate.OpenOptions
}

// ReopenedStandalone owns only the borrowed component handles, not the native
// logs, publisher or process directory lock supplied by the caller. Owners all
// remain at the exact selected common cut. Deltas and business readiness are a
// later, separate startup step; native verification is still full-history.
type ReopenedStandalone struct {
	mu                      sync.Mutex
	closed                  bool
	metadataStore           *boltstore.Store
	Owners                  StandaloneOwners
	Seal                    authenticatedstate.CandidateSeal
	LedgerReceipt           ledger.AuthenticatedLedgerReopen
	MetadataReceipt         metadatastate.ReopenReceipt
	Derivative              *usage.LedgerDerivative
	debt                    resourcebudget.Permit
	cleanupErr              error
	selectedStartup         bool
	physicalRecovered       bool
	physicalStartupRecovery budget.ProjectedRecoveryResult
	startupAudits           [4]*startupaudit.Owner
	services                *SelectedServices
	servicesMu              sync.Mutex
	servicesCancel          context.CancelFunc
	servicesClosing         bool
	checkpointLease         *ledger.MaintenanceLease
}

func (r *ReopenedStandalone) Close() error {
	return r.closeWithNative(nil)
}

// NativeGeneration closes its actual writer here, after every business/source
// borrower joins and while the original Directory completion pool is alive.
func (r *ReopenedStandalone) closeWithNative(native *ledger.Log) error {
	if r == nil {
		return nil
	}
	// Cancel a startup handoff before waiting for its whole operation to leave
	// mu. Holding mu while asking it to cancel would deadlock the join.
	r.servicesMu.Lock()
	r.servicesClosing = true
	if r.servicesCancel != nil {
		r.servicesCancel()
	}
	r.servicesMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	var err error
	if r.services != nil {
		// Business and observer operations borrow these roots. Never close a
		// root while an accepted operation can still reach it.
		if err := r.services.retire(context.Background()); err != nil {
			return err
		}
	}
	for _, audit := range r.startupAudits {
		if audit != nil {
			audit.Cancel()
		}
	}
	for _, audit := range r.startupAudits {
		if audit != nil {
			if e := audit.Wait(context.Background()); e != nil && !errors.Is(e, context.Canceled) {
				err = errors.Join(err, e)
			}
		}
	}

	if r.Derivative != nil {
		r.Derivative.Close()
	}
	if r.Owners.Usage != nil {
		err = errors.Join(err, selectedClosePhase("selected Usage projection", r.Owners.Usage.Close()))
	}
	if r.Owners.Governance != nil {
		err = errors.Join(err, selectedClosePhase("selected Governance projection", r.Owners.Governance.Close()))
	}

	closeEntered := false
	closeLedgerOwners := func(ctx context.Context) error {
		closeEntered = true
		var closeErr error
		// Business and source borrowers have joined. Qualify the actual
		// Metadata exit under the still-live original global work before the
		// DAO's ordinary Close reuses that same joined result.
		if r.Owners.Metadata != nil {
			closeErr = selectedClosePhase("selected Metadata baseline", r.Owners.Metadata.CloseContext(ctx))
		}
		if r.metadataStore != nil {
			closeErr = errors.Join(closeErr, selectedClosePhase("selected Metadata facade store", r.metadataStore.Close()))
		}
		if native != nil {
			closeErr = errors.Join(closeErr, selectedClosePhase("selected native Ledger", native.CloseContext(ctx)))
		}
		if r.Owners.Ledger != nil {
			closeErr = errors.Join(closeErr, selectedClosePhase("selected Ledger projection", r.Owners.Ledger.CloseContext(ctx)))
		}
		return closeErr
	}
	if r.Owners.Directory != nil {
		err = errors.Join(err, r.Owners.Directory.WithPhysicalCatchupWorkspace(context.Background(), closeLedgerOwners))
	} else {
		err = errors.Join(err, closeLedgerOwners(context.Background()))
	}
	if !closeEntered {
		// An unavailable workspace cannot skip a real descriptor join. Close
		// retains unqualified targets and fees for recovery on this path.
		err = errors.Join(err, closeLedgerOwners(context.Background()))
	}
	if r.Owners.Directory != nil {
		err = errors.Join(err, selectedClosePhase("selected Ledger Directory", r.Owners.Directory.Close()))
	}
	if r.checkpointLease != nil {
		err = errors.Join(err, selectedClosePhase("selected checkpoint lease", r.checkpointLease.Close()))
	}
	return errors.Join(err, r.cleanupErr)
}

func selectedClosePhase(phase string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", phase, err)
}

// ReopenStandalone authenticates/selects the complete common envelope before
// opening any component. Every opener receives its exact selected root, so
// mismatches cannot trigger orphan repair of an unselected root. It returns no
// reader on known failure; unknown retirement returns a closed cleanup-only
// owner and retains the whole job fee. This is not default Runtime startup.
func ReopenStandalone(ctx context.Context, o ReopenOptions) (result *ReopenedStandalone, resultErr error) {
	s, a := o.Sources, o.Authority
	if s.Budget == nil || s.Publisher == nil || s.Guard == nil || s.NativeLedger == nil || s.Lease == nil || s.NativeGovernance == nil || s.Audit == nil || s.InstanceID == "" || s.KeyGeneration == 0 || s.Ledger != nil || s.Usage != nil || s.Governance != nil || s.Metadata != nil || s.Directory != nil || a.Role != authenticatedstate.SealStandalone || a.Instance == ([32]byte{}) || a.Coverage != authenticatedstate.SealCoverageAuthenticated || o.LedgerOptions.Identity != a.Instance || o.LedgerOptions.Budget != s.Budget || o.DirectoryOptions.Identity != a.Instance || o.DirectoryOptions.Budget != s.Budget || o.UsageOptions.Identity != a.Instance || o.UsageOptions.Budget != s.Budget || o.GovernanceOptions.Identity != a.Instance || o.GovernanceOptions.Budget != s.Budget || o.MetadataOptions.Identity != a.Instance || o.MetadataOptions.Budget != s.Budget || o.MetadataOptions.InstanceID != s.InstanceID || o.MetadataOptions.KeyGeneration != s.KeyGeneration || o.MetadataOptions.SavedHead != o.MetadataOptions.PermittedHead {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := s.Publisher.ValidateOwner(s.Guard, s.Budget); err != nil {
		return nil, err
	}
	if replicated, err := s.Lease.ValidateOwner(s.NativeLedger); err != nil || replicated {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	selected, found, err := s.Publisher.Select(a)
	if err != nil || !found {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	hard := HardLimits()
	if selected.RootCount != uint8(len(hard.RequiredRoots)) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	for i, schema := range hard.RequiredRoots {
		r := selected.Roots[i]
		if (authenticatedstate.SealRootSchema{Kind: r.Kind, Component: r.Component, IndexID: r.IndexID, Codec: r.Codec}) != schema {
			return nil, authenticatedstate.ErrSealIneligible
		}
	}
	lp, mp := selected.Positions[0], selected.Positions[1]
	if lp.Offset > math.MaxInt64 || lp.Epoch > math.MaxUint8 || lp.Generation == 0 || lp.Revision != 0 || mp.Epoch != o.MetadataOptions.SavedHead.Epoch || mp.Revision != o.MetadataOptions.SavedHead.FileRevision || mp.Sequence != o.MetadataOptions.SavedHead.Sequence || mp.Offset != uint64(o.MetadataOptions.SavedHead.Offset) || mp.Hash != o.MetadataOptions.SavedHead.Hash || mp.MetadataBaseline != o.MetadataOptions.SavedHead.Baseline.Cut {
		return nil, authenticatedstate.ErrSealIneligible
	}
	credit, err := s.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes + 128<<10})
	if err != nil {
		return nil, err
	}
	r := &ReopenedStandalone{Owners: s, Seal: selected}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.Close())
			result = nil
		}
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := credit.Release(); err == nil {
				return
			} else {
				resultErr = errors.Join(resultErr, err, r.Close())
			}
		}
		r.cleanupErr, r.debt = resultErr, credit
		result = r
	}()
	anchor := ledger.ReplayAnchor{Head: ledger.Watermark{Generation: lp.Generation, Sequence: lp.Sequence, Offset: int64(lp.Offset)}, Hash: lp.Hash, Epoch: uint8(lp.Epoch)}
	resultErr = s.NativeGovernance.WithFrozenStandaloneHead(ctx, s.Budget, func(ctx context.Context, _ governance.Summary) error {
		return s.Audit.WithFrozenStandaloneHead(ctx, s.Budget, func(ctx context.Context, _ audit.Summary) error {
			auditRef, err := s.Audit.SealReference(ctx, s.Budget, audit.Summary{Records: selected.Positions[3].Sequence, Bytes: int64(selected.Positions[3].Offset), LastHash: selected.Positions[3].Hash})
			if err != nil || auditRef.Root != selected.Roots[4] {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			ledgerRoot := selected.Roots[0]
			lo := ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o.LedgerOptions, SavedAnchor: anchor, SelectedSealRoot: &ledgerRoot, DiskReservation: o.LedgerDisk}
			lo.Prefix = &ledger.ProjectionPrefix{Head: anchor.Head, Hash: anchor.Hash}
			r.Owners.Ledger, r.LedgerReceipt, err = ledger.OpenAuthenticatedLedgerProjection(ctx, s.NativeLedger, o.LedgerDirectory, lo)
			if err != nil {
				return err
			}
			lr, err := r.Owners.Ledger.SealReference(ctx)
			if err != nil || lr.Root != ledgerRoot || lr.Anchor != anchor {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			usageRoot := selected.Roots[5]
			uo := o.UsageOptions
			uo.Native, uo.Owner = s.NativeLedger, r.Owners.Ledger
			ur := usage.TypedUsageReference{Ledger: lr, Projection: usage.AuthenticatedUsageSealReference{Schema: hard.RequiredRoots[5], Root: usageRoot, Scope: usageRoot.State.Scope, Position: selected.Positions[4]}}
			r.Derivative, r.Owners.Usage, err = usage.OpenTypedDiskLedgerDerivative(ctx, uo, s.Lease, ur, o.UsageDisk)
			if err != nil {
				return err
			}
			governanceRoot := selected.Roots[3]
			governanceOptions := o.GovernanceOptions
			governanceOptions.Native = s.NativeGovernance
			gr := governance.TypedGovernanceReference{Journal: governance.GovernanceSealReference{Schema: hard.RequiredRoots[2], Root: selected.Roots[2], Position: selected.Positions[2]}, Projection: governance.AuthenticatedGovernanceProjectionReference{Schema: hard.RequiredRoots[3], Root: governanceRoot, Scope: governanceRoot.State.Scope, Position: selected.Positions[2]}}
			r.Owners.Governance, err = governance.OpenTypedGovernanceProjection(ctx, governanceOptions, gr, o.GovernanceDisk)
			if err != nil {
				return err
			}
			metadataRoot := selected.Roots[1]
			mo := o.MetadataOptions
			mo.SelectedSealRoot = &metadataRoot
			mo.SavedRoot = authenticatedstate.RootIdentity{Scope: metadataRoot.State.Scope, Cut: metadataRoot.State.Cut, Digest: metadataRoot.State.Digest, ID: metadataRoot.State.PackID, Revision: metadataRoot.State.Revision, Rows: metadataRoot.Rows}
			r.Owners.Metadata, r.MetadataReceipt, err = metadatastate.OpenFromRoot(ctx, o.MetadataKey, o.MetadataDirectory, o.MetadataJournalPath, mo)
			if err != nil {
				return err
			}
			dr := ledger.AuthenticatedDirectoryReference{Schema: hard.RequiredRoots[6], Root: selected.Roots[6], Position: lp}
			r.Owners.Directory, err = ledger.OpenAuthenticatedDirectory(ctx, s.NativeLedger, s.Lease, dr, o.DirectoryPath, o.DirectoryOptions)
			if err != nil {
				return err
			}
			if err := r.Owners.Metadata.WithFrozenStandaloneSealReference(ctx, s.Budget, a.Instance, s.InstanceID, s.KeyGeneration, func(ctx context.Context, ref metadatastate.AuthenticatedMetadataSealReference, head metadatajournal.Head) error {
				if ref.Root != metadataRoot || ref.Position != mp || head != mo.NativeHead {
					return authenticatedstate.ErrSealIneligible
				}
				return ctx.Err()
			}); err != nil {
				return err
			}
			if err := s.Lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, _ [32]byte) error {
				actual, err := s.NativeLedger.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
				if err != nil || actual != head {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				return ctx.Err()
			}); err != nil {
				return err
			}
			current, found, err := s.Publisher.Select(a)
			if err != nil || !found || current != selected {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			return ctx.Err()
		})
	})
	if resultErr == nil {
		result = r
	}
	return result, resultErr
}
