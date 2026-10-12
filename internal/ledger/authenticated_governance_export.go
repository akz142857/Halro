package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

var _ AccountingExportReader = (*AuthenticatedLedgerProjection)(nil)

// WithGovernanceExport serves the selected (memory-state) owner the same fixed
// cut the bbolt projection serves: the shared reducer already maintains the
// as-of lifecycle positions and counts in the authenticated rows. Each page is
// its own point job, so no pin or fee is held between pages or through
// publication; a Roll or a different hash at the cut's sequence revokes it.
func (p *AuthenticatedLedgerProjection) WithGovernanceExport(ctx context.Context, visit func(GovernanceExportView) error) error {
	return withGovernanceExport(ctx, authenticatedGovernanceExportSource{p}, visit)
}

type authenticatedGovernanceExportSource struct {
	p *AuthenticatedLedgerProjection
}

// governanceExportBusyWait bounds how long one export read waits out a lock or
// pin slot held for a moment. An export reads many pages; ordinary appends
// holding the owner lock briefly must not poison it.
const governanceExportBusyWait = 30 * time.Second

func (s authenticatedGovernanceExportSource) exportRead(ctx context.Context, fn func(governanceExportRead) error) error {
	deadline := time.Now().Add(governanceExportBusyWait)
	for delay := time.Millisecond; ; delay = min(2*delay, 50*time.Millisecond) {
		entered := false
		err := authenticatedPointJob(ctx, s.p, func(c context.Context, pin *authenticatedstate.Snapshot, anchor ReplayAnchor) error {
			entered = true
			return fn(governanceExportRead{
				rows:     authenticatedReadRows{c, pin},
				position: projectionPosition{Watermark: anchor.Head, Hash: anchor.Hash, Epoch: anchor.Epoch},
				walk: func(index string, start []byte, visit func(key, value []byte) (bool, error)) error {
					return authenticatedIndexWalk(c, pin, index, nil, start, visit)
				},
			})
		})
		// Only a refusal before any row was read is retried: the callback may
		// already have accumulated part of a page.
		if entered || !onlyBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}
