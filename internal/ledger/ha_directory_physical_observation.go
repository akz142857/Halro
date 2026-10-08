package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// HAReplicaDirectoryPhysicalObservation reports actual unused Directory roots.
// A final common scalar always owns one root. The current live roll protocol
// writes a descriptor page and scalar, so an additional roll needs two others.
type HAReplicaDirectoryPhysicalObservation struct {
	Window         authenticatedstate.PhysicalTerminalWindowObservation
	Native         authenticatedstate.PhysicalNativeReference
	AvailableRolls uint32
}

func (d *AuthenticatedDirectory) ObserveHAReplicaPhysicalCatchup(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection) (out HAReplicaDirectoryPhysicalObservation, resultErr error) {
	if d == nil || native == nil || owner == nil || !native.options.Replica {
		return out, ErrProjectionSource
	}
	resultErr = d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return owner.WithHASelectedPhysicalFrozenUsageSource(ctx, native, d.budget, d.ledgerScope, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
			if !d.mu.TryLock() {
				return authenticatedstate.ErrBusy
			}
			defer d.mu.Unlock()
			p := d.physical
			if p == nil || !p.ha || p.native != native || p.ledger != owner || p.window == nil || d.selectedNative != native || d.closeCtx.Err() != nil || d.sealErr != nil || p.remaining < 1 {
				return authenticatedstate.ErrRecovery
			}
			var err error
			out.Window, err = p.window.PreparedObservation(ctx)
			if err != nil {
				return err
			}
			out.Native, err = source.HAPhysicalNativeReference(ctx)
			if err != nil {
				return err
			}
			end, err := source.HAPhysicalNativePreparedEnd(ctx)
			if err != nil {
				return err
			}
			if out.Window.Selection != p.selection || out.Window.NativeEnd != end || out.Window.UnusedRoots != uint32(p.remaining) {
				return authenticatedstate.ErrRecovery
			}
			if err := source.VerifyHAPhysicalNativePrefix(ctx, out.Window.Native, end); err != nil {
				return err
			}
			if out.Native.Generation < d.anchor.Head.Generation {
				return ErrProjectionSource
			}
			owed := out.Native.Generation - d.anchor.Head.Generation
			if owed > uint64((p.remaining-1)/2) {
				return authenticatedstate.ErrRecovery
			}
			out.AvailableRolls = uint32(uint64((p.remaining-1)/2) - owed)
			return errors.Join(source.VerifyAnchors(ctx, [2]ReplayAnchor{d.anchor, source.Reference().Anchor}), source.Check(ctx), ctx.Err())
		})
	})
	if resultErr != nil {
		out = HAReplicaDirectoryPhysicalObservation{}
	}
	return out, resultErr
}
