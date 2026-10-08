package resourcebudget

import (
	"context"
	"errors"
	"time"
)

// WithSampling runs one finite callback under this monitor's original sampling
// authority. It waits for real, fresh Healthy evidence; it never changes policy
// or refunds any callback-owned claims. The callback must join its work before
// returning. A cancelled synchronous sample is also joined before return.
//
// An active Run is borrowed without stopping it. Otherwise this call owns a
// finite sampling loop. Concurrent callbacks or sampling owners are rejected.
func (m *Monitor) WithSampling(ctx context.Context, visit func(context.Context) error) (result error) {
	if m == nil || ctx == nil || visit == nil {
		return ErrInvalid
	}
	m.mu.Lock()
	if m.sampling || (m.running && (!m.longLived || m.ctx.Err() != nil)) {
		m.mu.Unlock()
		return ErrInvalid
	}
	m.sampling = true
	owned := !m.running
	var cancelOwner context.CancelFunc
	if owned {
		var owner context.Context
		owner, cancelOwner = context.WithCancel(ctx)
		m.startLocked(owner, false)
	}
	owner, done := m.ctx, m.done
	m.mu.Unlock()
	if owned {
		go func() { _ = m.sampleLoop(owner, nil) }()
	}
	callback, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(owner, cancel)
	// Loop failure also withdraws a borrowed lifetime whose parent context may
	// still be live. This watcher belongs to the finite callback and is joined.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-done:
			cancel()
		case <-callback.Done():
		}
	}()
	defer func() {
		cancel()
		stop()
		<-watchDone
		if owned {
			cancelOwner()
			<-done
		}
		result = errors.Join(result, ctx.Err())
		m.mu.Lock()
		if owned && m.runErr != nil && !errors.Is(m.runErr, context.Canceled) {
			result = errors.Join(result, m.runErr)
		}
		// Success cannot survive withdrawal of the exact borrowed owner.
		if !owned && (!m.running || owner.Err() != nil) {
			result = errors.Join(result, m.runErr, owner.Err())
			if result == nil {
				result = context.Canceled
			}
		}
		m.sampling = false
		m.mu.Unlock()
	}()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		if err := callback.Err(); err != nil {
			return err
		}
		if m.Snapshot().State == Healthy {
			result = visit(callback)
			return errors.Join(result, callback.Err())
		}
		select {
		case <-callback.Done():
			return callback.Err()
		case <-ticker.C:
		}
	}
}
