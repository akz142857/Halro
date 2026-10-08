package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// BuildTypedDiskLedgerDerivative binds direct authenticated rows to the same
// derivative-before-Ledger-Apply bridge as the ordinary disk owner. Live Apply
// is admitted only by a real maintenance AppendApply callback's opaque current
// native scope, and still authenticates/matches the complete native Record.
// This explicit maintenance component neither selects default Runtime nor
// supplies identity/role/common-cut or HA confirmation authority. Close the
// derivative before retiring the returned caller-owned projection.
func BuildTypedDiskLedgerDerivative(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease) (*LedgerDerivative, *DiskProjection, TypedUsageReference, error) {
	p, ref, err := BuildTypedUsageProjection(ctx, o, lease)
	if err != nil {
		return nil, p, ref, err
	}
	d := &LedgerDerivative{owner: o.Owner, disk: p}
	p.reader = d
	return d, p, ref, nil
}

func OpenTypedDiskLedgerDerivative(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, reservation resourcebudget.Permit) (*LedgerDerivative, *DiskProjection, error) {
	p, err := OpenTypedUsageProjection(ctx, o, lease, selected, reservation)
	if err != nil {
		return nil, p, err
	}
	d := &LedgerDerivative{owner: o.Owner, disk: p}
	p.reader = d
	return d, p, nil
}

func OpenSelectedTypedDiskLedgerDerivative(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, reservation resourcebudget.Permit, source ledger.SelectedLedgerProjectionOpenOptions) (*LedgerDerivative, *DiskProjection, error) {
	p, err := OpenSelectedTypedUsageProjection(ctx, o, lease, selected, reservation, source)
	if err != nil {
		return nil, p, err
	}
	d := &LedgerDerivative{owner: o.Owner, disk: p}
	p.reader = d
	return d, p, nil
}
