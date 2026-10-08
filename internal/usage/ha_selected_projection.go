package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// OpenHASelectedTypedUsageProjection obtains a source loan from the actual HA
// native constructor. Its independent callback freezes Member, Ordering and all
// four source permissions; a supplied seal alone cannot admit this opener.
func OpenHASelectedTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, reservation resourcebudget.Permit, source ledger.SelectedLedgerProjectionOpenOptions) (*DiskProjection, error) {
	return openTypedUsageProjection(ctx, o, lease, selected, reservation, &source, true)
}

func OpenHASelectedTypedDiskLedgerDerivative(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, reservation resourcebudget.Permit, source ledger.SelectedLedgerProjectionOpenOptions) (*LedgerDerivative, *DiskProjection, error) {
	p, err := OpenHASelectedTypedUsageProjection(ctx, o, lease, selected, reservation, source)
	if err != nil {
		return nil, p, err
	}
	d := &LedgerDerivative{owner: o.Owner, disk: p}
	p.reader = d
	return d, p, nil
}
