package ledger

import (
	"context"
	"errors"
)

// BuildFreshProjection builds a private full-history reference while freezing
// this actual native writer. It neither takes semantic maintenance admission nor
// retires the business Manager. Appends/roll/close wait for this physical scan;
// the caller must bound duration and join the job before closing this Log.
// This transitional O(history) construction is not a trusted saved DB or a
// permission to select a durable HA tail as an applied business projection.
func (l *Log) BuildFreshProjection(ctx context.Context, output string) (*LedgerProjection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.lifecycle.RLock()
	defer l.lifecycle.RUnlock()
	if l.closed || l.closeContext.Err() != nil {
		return nil, errors.New("native Ledger is closed")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.file == nil || len(l.chainKey) != 32 || l.status.Load() != AccountingHealthy || l.options.Replica || l.options.AfterDurable != nil || l.options.WaitConfirmed != nil {
		return nil, errors.New("live reference requires a healthy Standalone keyed native writer")
	}
	report, partial, err := InspectReplayAuthenticatedContext(ctx, l.file.Name(), l.chainKey, nil)
	expected := Watermark{Generation: l.generation, Sequence: l.chainSequence, Offset: l.chainOffset}
	if err != nil || partial || report.Head != expected || report.ChainHash != l.chainHash {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			l.status.RequireRecovery()
		}
		return nil, errors.Join(err, ErrProjectionSource)
	}
	p, err := BuildLedgerProjection(ctx, l.file.Name(), l.chainKey, output, report)
	if errors.Is(err, ErrProjectionSource) {
		l.status.RequireRecovery()
	}
	return p, err
}
