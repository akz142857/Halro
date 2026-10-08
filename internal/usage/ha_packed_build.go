package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/ledger"
)

// BuildPackedTypedHAUsageProjection performs the same bounded fresh native
// reducer and single packed build as the ordinary offline API, using the actual
// Replica read-maintenance source. Saved scratch remains ineligible; no semantic
// Ledger append or quorum confirmation is granted. Independent HA preflight and
// later complete common publication are the caller's obligations.
func BuildPackedTypedHAUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease) (*DiskProjection, TypedUsageReference, error) {
	return buildFixedPackedTypedUsageProjection(ctx, o, lease, true)
}
