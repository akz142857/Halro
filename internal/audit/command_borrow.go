package audit

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// A command scope owns and joins this small handle before retiring its selected
// actual native source. Close cancels/joins this borrow, preserving that source
// for the scope's common checkpoint. Replay uses a separate bounded, cancellable reader. It grants no seal or deletion API.
type commandBorrow struct {
	mu     sync.Mutex
	source *Log
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	final  Summary
	budget *resourcebudget.Manager
	fee    resourcebudget.Permit
	err    error
}

func (l *Log) BorrowSelectedCommand(ctx context.Context, guard *lock.Lock, budget *resourcebudget.Manager, key []byte) (*Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || guard == nil || budget == nil {
		return nil, ErrStartupCache
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(key) != len(l.key) || subtle.ConstantTimeCompare(key, l.key) != 1 || l.file == nil || l.closing || l.startupProvenance == nil || l.startupProvenance.guard != guard || l.terminalErr != nil || l.startupAudit == nil || !l.startupAudit.Snapshot().Complete || l.startupAudit.Snapshot().Err != nil || l.cacheBudget != budget || !guard.CoversNativePath(l.path) {
		return nil, ErrStartupCache
	}
	fee, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return nil, err
	}
	borrowCtx, cancel := context.WithCancel(context.Background())
	return &Log{commandBorrow: &commandBorrow{source: l, ctx: borrowCtx, cancel: cancel, fee: fee, budget: budget}}, nil
}
func (b *commandBorrow) appendBatch(ctx context.Context, events []Event) ([]Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("selected command Audit borrow is closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	return b.source.AppendBatch(ctx, events)
}
func (b *commandBorrow) summary() Summary {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return b.final
	}
	return b.source.Summary()
}
func (b *commandBorrow) hasEvent(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closed && b.source.HasRetainedEventID(id)
}
func (b *commandBorrow) close() error {
	b.cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return b.err
	}
	b.closed = true
	b.final = b.source.Summary()
	b.source = nil
	b.err = errors.Join(b.err, b.fee.Release())
	return b.err
}

func (b *commandBorrow) replay(ctx context.Context, visit func(Record) error) (result Summary, resultErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Summary{}, errors.New("selected command Audit borrow is closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	fee, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * maxPayloadSize})
	if err != nil {
		return Summary{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, fee.Release())
		if resultErr != nil {
			result = Summary{}
		}
	}()
	return b.source.ReplayContext(ctx, visit)
}
