package ledger

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// AuthenticatedLedgerOpenOptions describes an isolated component reopen, not a
// production cross-component Seal. Identity, SavedAnchor and non-nil Prefix
// must come from the caller's independent maintenance/role evidence. The root
// authenticates all rows and its cut; complete native inspection additionally
// proves the saved and permitted boundaries before any component mutation.
// Ordinary Runtime startup still must not call this until D-03 is complete.
type AuthenticatedLedgerOpenOptions struct {
	AuthenticatedLedgerOptions
	SavedAnchor      ReplayAnchor
	SelectedSealRoot *authenticatedstate.SealRoot // optional root from an already selected cross-component Seal
	DiskReservation  resourcebudget.Permit        // independently reconciled persistent owner
}

// AuthenticatedLedgerReopen distinguishes full source decoding from semantic
// delta application. Native Open and this source audit still cost O(history).
type AuthenticatedLedgerReopen struct {
	From, To, NativeHead                          ReplayAnchor
	SourceVerified, DeltaVerified, AppliedRecords uint64
}

// OpenAuthenticatedLedgerProjection validates a saved authenticated component
// and reduces only its permitted native delta. It requires an already opened,
// keyed native Log under independent maintenance admission. Native/component
// writer locks freeze both sources; no existing State or Bolt view is imported.
// Failure closes borrowed resources and preserves the persistent namespace and
// publisher's disk charge. It does not open business admission or grant a role.
func OpenAuthenticatedLedgerProjection(ctx context.Context, native *Log, directory string, o AuthenticatedLedgerOpenOptions) (*AuthenticatedLedgerProjection, AuthenticatedLedgerReopen, error) {
	return openAuthenticatedLedgerProjection(ctx, native, directory, o, nil)
}

// phase is a private deterministic IO/lifecycle test seam.
func openAuthenticatedLedgerProjection(ctx context.Context, native *Log, directory string, o AuthenticatedLedgerOpenOptions, phase func(string)) (*AuthenticatedLedgerProjection, AuthenticatedLedgerReopen, error) {
	return openAuthenticatedLedgerProjectionQualified(ctx, native, directory, o, phase, nil)
}

func openAuthenticatedLedgerProjectionQualified(ctx context.Context, native *Log, directory string, o AuthenticatedLedgerOpenOptions, phase func(string), selected *SelectedLedgerProjectionOpenOptions) (result *AuthenticatedLedgerProjection, receipt AuthenticatedLedgerReopen, resultErr error) {
	defer func() {
		if resultErr != nil {
			if result != nil {
				resultErr = errors.Join(resultErr, result.Close())
				if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
					result = nil
				}
			}
			receipt = AuthenticatedLedgerReopen{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	if native == nil || o.Identity == ([32]byte{}) || o.Budget == nil || o.Prefix == nil || o.SavedAnchor.Head.Generation == 0 || o.SavedAnchor.Head.Offset < 0 || o.SavedAnchor.Epoch > frameVersionRunAttribution || (o.SavedAnchor.Hash != ([32]byte{})) != authenticatedFrameEpoch(o.SavedAnchor.Epoch) {
		return nil, receipt, ErrProjectionSource
	}
	var selectedSealRoot *authenticatedstate.SealRoot
	if o.SelectedSealRoot != nil {
		root := *o.SelectedSealRoot
		selectedSealRoot = &root
	}
	permitted := *o.Prefix
	if permitted.Head.Generation == 0 || permitted.Head.Offset < 0 || projectionPrefixBefore(permitted.Head, o.SavedAnchor.Head) || permitted.Head.Sequence < o.SavedAnchor.Head.Sequence {
		return nil, receipt, ErrProjectionCursor
	}
	// Retain native mutation ownership before IO; lease/log Close must join the
	// complete factory, including source fees and borrowed-store cleanup.
	native.admission.Lock()
	owner := native.maintenance
	replica := native.options.Replica
	native.admission.Unlock()
	if !replica && owner == nil {
		return nil, receipt, ErrLedgerMaintenance
	}
	if owner != nil && !owner.borrowsHASelectedMaintenance(ctx) {
		select {
		case owner.gate <- struct{}{}:
		default:
			return nil, receipt, authenticatedstate.ErrBusy
		}
		defer func() { <-owner.gate }()
	}
	finish, err := native.beginMutation(owner)
	if err != nil {
		return nil, receipt, err
	}
	defer func() {
		// Even a late credit-retirement failure must close a newly opened component
		// before native/lease Close is allowed to observe this factory as joined.
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.Close())
			if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
				result = nil
			}
		}
		finish()
	}()
	credit, err := o.DiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		return nil, receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	permit, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	defer func() { resultErr = finishAuthenticatedSourceCredit(permit, resultErr) }()
	var cleanupCandidate *AuthenticatedLedgerProjection
	// The actual native mutex must join before failed physical projection Close
	// can inspect/retain that same owner. Register this defer before the unlock
	// defer; a source failure must not deadlock while preserving its targets.
	defer func() {
		if cleanupCandidate != nil {
			resultErr = errors.Join(resultErr, cleanupCandidate.Close())
			if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) || errors.Is(resultErr, authenticatedstate.ErrRecovery) {
				result = cleanupCandidate
			}
		}
	}()
	if !native.mu.TryLock() {
		return nil, receipt, authenticatedstate.ErrBusy
	}
	defer native.mu.Unlock()
	if native.file == nil || len(native.chainKey) != 32 || native.status.Load() != AccountingHealthy || native.closeContext.Err() != nil {
		return nil, receipt, ErrProjectionUnavailable
	}
	nativeHead := ReplayAnchor{Head: Watermark{native.generation, native.offset, native.sequence}, Hash: native.chainHash, Epoch: native.chainEpoch}
	if projectionPrefixBefore(nativeHead.Head, permitted.Head) || permitted.Head.Sequence > nativeHead.Head.Sequence {
		return nil, receipt, ErrProjectionCursor
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(native.closeContext, cancel)
	defer stop()
	if owner != nil {
		stopOwner := context.AfterFunc(owner.ctx, cancel)
		defer stopOwner()
	}
	checkContext := func() error {
		if owner != nil {
			return errors.Join(opCtx.Err(), native.closeContext.Err(), owner.ctx.Err())
		}
		return errors.Join(opCtx.Err(), native.closeContext.Err())
	}
	ctx = opCtx
	var selectedSeal authenticatedstate.CandidateSeal
	var sourceVerified uint64
	var target ReplayAnchor
	if selected != nil {
		selectedSeal, err = native.validateSelectedOwnerLocked(ctx, o.Budget, o.Identity, *selected)
		if err != nil || o.SavedAnchor != native.startupSaved {
			return nil, receipt, errors.Join(ErrProjectionSource, err)
		}
		selectedSealRoot = nil
		for _, root := range selectedSeal.Roots[:selectedSeal.RootCount] {
			if root.Kind == authenticatedstate.SealRootState && root.Component == authenticatedstate.SealComponentLedger && root.IndexID == AuthenticatedLedgerSealIndexID && root.Codec == AuthenticatedLedgerSealCodec {
				r := root
				selectedSealRoot = &r
			}
		}
		if selectedSealRoot == nil {
			return nil, receipt, ErrProjectionSource
		}
		seen := false
		observe := func(anchor ReplayAnchor) error {
			if anchor.Head == permitted.Head && anchor.Hash == permitted.Hash {
				target, seen = anchor, true
			}
			return nil
		}
		_ = observe(o.SavedAnchor)
		_, err = native.replayAuthenticatedLocked(ctx, o.SavedAnchor, 0, func(r Record) error {
			sourceVerified++
			return observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch})
		}, observe)
		if err != nil || !seen {
			return nil, receipt, errors.Join(ErrProjectionCursor, err)
		}
	} else {
		var last ReplayAnchor
		var startEpoch uint8
		savedSeen, permittedSeen := false, false
		observe := func(anchor ReplayAnchor) {
			if anchor == o.SavedAnchor {
				savedSeen = true
			}
			if anchor.Head == permitted.Head && anchor.Hash == permitted.Hash {
				target, permittedSeen = anchor, true
			}
		}
		report, partial, err := inspectReplayAuthenticated(ctx, native.path, native.chainKey, func(r Record) error {
			if r.Generation != last.Head.Generation {
				startEpoch = last.Epoch
			}
			last = ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch}
			observe(last)
			return nil
		}, func(head Watermark, hash [32]byte) error {
			epoch := last.Epoch
			if head.Generation == last.Head.Generation {
				epoch = startEpoch
			}
			observe(ReplayAnchor{Head: head, Hash: hash, Epoch: epoch})
			return nil
		})
		if err != nil || partial || report.Head != nativeHead.Head || report.ChainHash != nativeHead.Hash || last.Epoch != nativeHead.Epoch {
			if err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				native.status.RequireRecovery()
			}
			return nil, receipt, errors.Join(ErrProjectionSource, err)
		}
		if report.ChecksumOnly != 0 {
			return nil, receipt, fmt.Errorf("%w: checksum-era history requires an independently accepted legacy baseline", ErrProjectionSource)
		}
		if !savedSeen || !permittedSeen {
			return nil, receipt, fmt.Errorf("%w: saved/permitted cut is not an authenticated native boundary", ErrProjectionCursor)
		}
		sourceVerified = report.Authenticated + report.SealedAuthenticated
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := checkContext(); err != nil {
		return nil, receipt, err
	}
	path, err := filepath.Abs(native.path)
	if err != nil {
		return nil, receipt, err
	}
	scope := authenticatedLedgerScope(o.Identity)
	key, err := hkdf.Key(sha256.New, native.chainKey, scope[:], "halro:ledger:authenticated-rows:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(key)
	var selectedSchema *authenticatedstate.SealRootSchema
	if selectedSealRoot != nil {
		schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState,
			Component: authenticatedstate.SealComponentLedger,
			IndexID:   AuthenticatedLedgerSealIndexID,
			Codec:     AuthenticatedLedgerSealCodec,
		}
		selectedSchema = &schema
	}
	store, err := authenticatedstate.OpenStore(ctx, directory, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: authenticatedLedgerCut(scope, o.SavedAnchor), Key: key,
		MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget, DiskReservation: credit,
		ExpectedSealRoot: selectedSealRoot, ExpectedSealSchema: selectedSchema,
		PhysicalRecovery: selected != nil && selected.PhysicalRecovery != nil && native.startupProvenance != nil,
	})
	if err != nil {
		return nil, receipt, err
	}
	p := &AuthenticatedLedgerProjection{store: store, ready: true, scope: scope, budget: o.Budget, nativePath: path, nativeKeyTag: sha256.Sum256(native.chainKey)}
	p.closeContext, p.closeCancel = context.WithCancel(context.Background())
	cleanupCandidate = p
	if phase != nil {
		phase("component-open")
	}
	actual, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		return nil, receipt, err
	}
	if actual != o.SavedAnchor {
		return nil, receipt, ErrProjectionCorrupt
	}
	if selected != nil && selected.PhysicalRecovery != nil {
		encoded, err := json.Marshal(selectedSeal)
		if err != nil {
			return nil, receipt, err
		}
		var restored bool
		if native.startupProvenance.ha != nil {
			restored, err = p.restoreHAPhysicalStartupLocked(ctx, native, sha256.Sum256(encoded), target, selected.PhysicalRecovery)
		} else {
			restored, err = p.restorePhysicalStartupLocked(ctx, native, sha256.Sum256(encoded), target, selected.PhysicalRecovery)
		}
		if err != nil {
			return nil, receipt, err
		}
		if restored {
			actual, err = p.ReadReplayAnchor(ctx)
			if err != nil || native.startupProvenance.ha == nil && actual != target || native.startupProvenance.ha != nil && (projectionPrefixBefore(actual.Head, o.SavedAnchor.Head) || projectionPrefixBefore(target.Head, actual.Head)) {
				return nil, receipt, errors.Join(ErrProjectionSource, err)
			}
		}
	}
	receipt = AuthenticatedLedgerReopen{From: actual, To: target, NativeHead: nativeHead, SourceVerified: sourceVerified}
	if target != actual {
		p.mu.Lock()
		receipt.AppliedRecords, receipt.DeltaVerified, err = p.applyAuthenticatedNativeDeltaLocked(ctx, native, actual, target, phase, checkContext)
		p.mu.Unlock()
		if err != nil {
			return nil, receipt, err
		}
	}
	if err := checkContext(); err != nil {
		return nil, receipt, err
	}
	if selected != nil {
		current, err := native.validateSelectedOwnerLocked(ctx, o.Budget, o.Identity, *selected)
		if err != nil || current != selectedSeal {
			return nil, receipt, errors.Join(ErrProjectionSource, err)
		}
	}
	if selected != nil {
		p.selectedNative.Store(native)
	}
	result, cleanupCandidate = p, nil
	return result, receipt, nil
}

// DiskReservation retains an already closed component publisher's existing
// obligation. It cannot reconstruct persistent ownership after process exit.
func (p *AuthenticatedLedgerProjection) DiskReservation() (resourcebudget.Permit, error) {
	if p == nil || !p.mu.TryRLock() {
		return resourcebudget.Permit{}, authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	return p.store.DiskReservation()
}

// AdoptClosedSuccessor advances only the original publisher's cleanup receipt;
// the borrowed component still cannot Discard the persistent namespace.
func (p *AuthenticatedLedgerProjection) AdoptClosedSuccessor(next *AuthenticatedLedgerProjection) error {
	if p == nil || next == nil || p == next || p.scope != next.scope || p.budget != next.budget || p.nativePath != next.nativePath || p.nativeKeyTag != next.nativeKeyTag {
		return ErrProjectionSource
	}
	if !p.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.Unlock()
	if !next.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer next.mu.Unlock()
	if p.ready || next.ready || p.closeContext.Err() == nil || next.closeContext.Err() == nil {
		return ErrProjectionUnavailable
	}
	return p.store.AdoptClosedSuccessor(next.store)
}
