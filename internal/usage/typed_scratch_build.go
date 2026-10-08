package usage

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
)

type FixedUsageBuildRetirement struct {
	Cause              error
	Scratch, Candidate *DiskProjection
	Workspace, Live    resourcebudget.Permit
	uncertainSource    bool
}

func (e *FixedUsageBuildRetirement) Error() string { return e.Cause.Error() }
func (e *FixedUsageBuildRetirement) Unwrap() error { return e.Cause }

func createFixedUsageScratch(ctx context.Context, o AuthenticatedUsageOptions, source FinalizationReader) (*DiskProjection, error) {
	fresh, err := scratchstate.Create(ctx, o.ScratchPath, scratchstate.Options{Budget: o.Budget, MaxDiskBytes: o.ScratchMaxDiskBytes, Reservation: o.ScratchReservation})
	p := &DiskProjection{reader: source, readers: make(chan struct{}, 2), summaries: make(chan struct{}, 1), engine: &usageEngine{scratch: fresh, ctx: ctx}}
	if err != nil {
		return p, err
	}
	p.scratchWork, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: scratchstate.RowBufferRAMBytes})
	if err != nil {
		return p, err
	}
	p.engine.hotScratch = new(scratchstate.RowBuffer)
	err = p.engine.Update(func(tx *usageTx) error {
		if err := diskUsagePut(tx, "meta", []byte("rollup_rows"), uint64(0)); err != nil {
			return err
		}
		return diskUsagePut(tx, "meta", []byte("state"), diskUsageState{Version: diskUsageVersion})
	})
	return p, err
}

func buildFixedPackedTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, haReadOnly bool) (result *DiskProjection, reference TypedUsageReference, resultErr error) {
	return buildFixedPackedTypedUsageProjectionMode(ctx, o, lease, haReadOnly, false)
}

func buildFixedPackedTypedUsageProjectionMode(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, haReadOnly, selectedReference bool) (result *DiskProjection, reference TypedUsageReference, resultErr error) {
	if o.Native == nil || o.Owner == nil || o.Identity == ([32]byte{}) || len(o.UsageKey) != 32 || o.Budget == nil || o.RootDirectory == "" || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 || o.ScratchPath == "" || o.ScratchMaxDiskBytes <= 0 {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	if err := o.Owner.ValidateNativeSource(ctx, o.Native, lease); err != nil {
		return nil, reference, err
	}
	if err := o.Owner.ValidateResourceOwner(ctx, o.Budget); err != nil {
		return nil, reference, err
	}
	debt := &FixedUsageBuildRetirement{}
	defer func() {
		debt.uncertainSource = errors.Is(resultErr, ownedfile.ErrRetirement)
		if err := debt.Scratch.Discard(); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
		}
		if resultErr != nil && debt.Candidate != nil {
			if err := debt.Candidate.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
			result = nil
			reference = TypedUsageReference{}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			debt.Cause = resultErr
			resultErr = debt
			return
		}
		if err := releaseFixedUsageFees(debt); err != nil {
			debt.Cause = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			resultErr = debt
			result = nil
			reference = TypedUsageReference{}
		}
	}()
	var err error
	debt.Workspace, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes + rankScratchBatchRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	debt.Live, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	initial, err := o.Owner.SealReference(ctx)
	if err != nil {
		return nil, reference, err
	}
	withSource := o.Owner.WithOfflineMaintenanceUsageSource
	if haReadOnly {
		withSource = o.Owner.WithOfflineHAMaintenanceUsageSource
	}
	if selectedReference {
		withSource = o.Owner.WithSelectedCompleteUsageSource
	}
	resultErr = withSource(ctx, o.Native, lease, o.Budget, initial.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		ref := source.Reference()
		if ref.Anchor.Head.After(source.NativeHead().Head) {
			return authenticatedstate.ErrRecovery
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		debt.Scratch, err = createFixedUsageScratch(ctx, o, packedUsageSource{source})
		if err != nil {
			return err
		}
		if err := replayPackedUsageScratch(ctx, source, debt.Scratch, ref.Anchor); err != nil {
			return err
		}
		if err := alignPackedUsageBoundary(ctx, debt.Scratch, ref.Anchor); err != nil {
			return err
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		if err := source.Check(ctx); err != nil || source.Reference() != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		scope := typedUsageScope(o.Identity, ref.Scope)
		key, err := hkdf.Key(sha256.New, o.UsageKey, scope[:], "halro:usage:typed-content:v2", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		var store *authenticatedstate.Store
		err = debt.Scratch.engine.scratch.View(ctx, func(tx *scratchstate.Transaction) error {
			it, err := tx.Iterator()
			if err != nil {
				return err
			}
			marker := true
			store, err = authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedUsageCut(ref.Anchor), Key: key, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
				if marker {
					marker = false
					return authenticatedstate.Row{Key: []byte{0}, Value: []byte{byte(TypedUsageSealCodec)}}, true, nil
				}
				row, found, err := it.Next()
				return authenticatedstate.Row{Key: row.Key, Value: row.Value}, found, err
			})
			return err
		})
		if store != nil {
			var ownerErr error
			debt.Candidate, ownerErr = newTypedUsageOwner(store, o.Native, lease, o.Owner, o.Budget, o.Identity)
			err = errors.Join(err, ownerErr)
			if debt.Candidate != nil {
				debt.Candidate.liveRAM = debt.Live
				debt.Live = resourcebudget.Permit{}
			}
		}
		if err != nil {
			return err
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		if err := source.Check(ctx); err != nil || source.Reference() != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		actual, err := debt.Candidate.ReadReplayAnchor(ctx)
		if err != nil || actual != ref.Anchor {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		root, err := store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		reference = TypedUsageReference{Ledger: ref, Projection: AuthenticatedUsageSealReference{Schema: typedUsageSchema(), Root: root, Scope: scope, Position: authenticatedUsagePosition(ref)}}
		result = debt.Candidate
		return ctx.Err()
	})
	return result, reference, resultErr
}
func releaseFixedUsageFees(e *FixedUsageBuildRetirement) error {
	for _, p := range []*resourcebudget.Permit{&e.Workspace, &e.Live} {
		if *p != (resourcebudget.Permit{}) {
			if err := p.Release(); err != nil {
				return err
			}
			*p = resourcebudget.Permit{}
		}
	}
	return nil
}
func (e *FixedUsageBuildRetirement) Retire() error {
	if e == nil {
		return nil
	}
	if e.uncertainSource {
		return e.Cause
	}
	if err := e.Scratch.Discard(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := e.Candidate.Discard(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	return releaseFixedUsageFees(e)
}
