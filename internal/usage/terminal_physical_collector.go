package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Only this actual typed Collector borrows the original physical continuation
// pool for its two watermark barriers. Ordinary queries keep normal admission;
// source qualification, selected root checks and actual pin retirement remain
// those of the same derivative. The Collector serializes these reads with its
// replay job, and the projection joins the early lifetime before closing IO.
type typedPhysicalCollectorTarget struct {
	derivative *LedgerDerivative
	native     *ledger.Log
	manager    *resourcebudget.Manager
}

func (t *typedPhysicalCollectorTarget) ApplyContext(ctx context.Context, record ledger.Record) error {
	return t.derivative.ApplyContext(ctx, record)
}

func (t *typedPhysicalCollectorTarget) ReadWatermark(ctx context.Context) (head ledger.Watermark, resultErr error) {
	d := t.derivative
	d.mu.Lock()
	p := d.disk
	var q *typedPhysicalUsageOwner
	if p != nil {
		p.mu.RLock()
		q = p.physical
		p.mu.RUnlock()
	}
	d.mu.Unlock()
	if q == nil {
		return d.ReadWatermark(ctx)
	}
	ctx, finish, err := p.beginPhysicalUsageJob(ctx)
	if err != nil {
		return head, err
	}
	defer finish()
	if q.native != t.native || q.manager != t.manager || q.work == nil {
		return head, authenticatedstate.ErrRecovery
	}
	resultErr = resourcebudget.WithCompletionWorkspace(q.work.Bind(ctx), func(ctx context.Context) error {
		var err error
		head, err = d.ReadWatermark(ctx)
		return err
	})
	if resultErr != nil {
		head = ledger.Watermark{}
	}
	return head, resultErr
}
