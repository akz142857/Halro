package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/ledger"
)

// BuildPackedSelectedUsageReference reconstructs an unpublished complete Usage
// reference using fixed workspace and the actual selected Ledger owner. Its
// source loan requires a completed native historical audit and a live exclusive
// Standalone maintenance lease. Saved scratch or candidate bytes are never
// imported. The caller must discard this reference after its comparison.
func BuildPackedSelectedUsageReference(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease) (*DiskProjection, TypedUsageReference, error) {
	return buildFixedPackedTypedUsageProjectionMode(ctx, o, lease, false, true)
}
