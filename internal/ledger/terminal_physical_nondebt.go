package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// VerifyPhysicalNoDebt uses the real Standalone maintenance lane and complete
// selected native authentication. A zero physical counter is not a NoDebt
// proof: both complete pinned original request/lease streams must be empty.
func (p *AuthenticatedLedgerProjection) VerifyPhysicalNoDebt(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if p == nil || native == nil || lease == nil || p.selectedNative.Load() != native {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	return lease.ReadFrozenHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
		if native.startupProvenance == nil || native.startupProvenance.ha != nil || !native.selectedStartupStructuralAllowedLocked() || native.options.Replica || native.status.Load() != AccountingHealthy {
			return ErrProjectionSource
		}
		anchor, err := p.ReadReplayAnchor(ctx)
		if err != nil || anchor.Head != head || anchor.Hash != hash || anchor.Epoch != native.chainEpoch {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		return p.provePhysicalRecoveryNoDebt(ctx)
	})
}
