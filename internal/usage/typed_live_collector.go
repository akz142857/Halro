package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// NewTypedLiveCollector retains the existing bounded notification worker while
// giving each catch-up one coherent native/root job. The replay adapter applies
// the whole job itself: per-record public Apply would reenter its source locks.
// The caller activates the actual projection under startup maintenance first,
// then releases that lease and owns Collector/Derivative/Projection retirement.
func NewTypedLiveCollector(d *LedgerDerivative, native *ledger.Log, manager *resourcebudget.Manager, capacity int) (*Collector, error) {
	if d == nil || native == nil || manager == nil {
		return nil, errors.New("typed live Collector requires actual derivative, native source and shared Manager")
	}
	d.mu.Lock()
	ok := !d.retired.Load() && d.disk != nil && d.disk.engine != nil && d.disk.engine.cow != nil && d.disk.engine.live && d.disk.engine.native == native && d.disk.engine.budget == manager
	d.mu.Unlock()
	if !ok {
		return nil, ErrLedgerDerivativeRetired
	}
	target := &typedPhysicalCollectorTarget{derivative: d, native: native, manager: manager}
	return NewProjectedCollector(target, func(ctx context.Context, from ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		receipt, err := d.CatchUpTypedLive(ctx, native, manager)
		if err == nil && receipt.To.Head.Sequence < from.Sequence {
			return ledger.Watermark{}, ErrCollectorHead
		}
		return receipt.To.Head, err
	}, capacity)
}
