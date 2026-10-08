package gateway

import (
	"context"
	"errors"
	"sync"
)

var ErrDeferredPaused = errors.New("deferred response dispatch is paused for maintenance")

type deferredWorkPause struct {
	done   chan struct{}
	resume chan struct{}
	joined bool
}

// Count includes the admitted dispatcher and complete workers, including their
// terminal writes. Adding a worker is legal only under an admitted dispatch;
// therefore a pause cannot overtake a worker between launch and registration.
type deferredWorkLifetime struct {
	mu     sync.Mutex
	active uint64
	paused *deferredWorkPause
}

func (l *deferredWorkLifetime) leave() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.active == 0 && l.paused != nil && !l.paused.joined {
		close(l.paused.done)
		l.paused.joined = true
	}
}

func (l *deferredWorkLifetime) retainAccepted() func() {
	l.mu.Lock()
	l.active++
	l.mu.Unlock()
	return l.leave
}

func (l *deferredWorkLifetime) enter(ctx context.Context, wait bool) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.paused == nil {
			l.active++
			l.mu.Unlock()
			return l.leave, nil
		}
		resume := l.paused.resume
		l.mu.Unlock()
		if !wait {
			return nil, ErrDeferredPaused
		}
		select {
		case <-resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// PauseDeferredResponses lets accepted workers finish normally and prevents new
// dispatch/expiry/recovery writes. Queued submissions remain durable and resume
// on the same engine; this does not cancel upstream calls or mark them failed.
func (s *Service) PauseDeferredResponses(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.deferred == nil {
		return func() error { return nil }, nil
	}
	l := &s.deferred.lifetime
	l.mu.Lock()
	if l.paused != nil {
		l.mu.Unlock()
		return nil, ErrDeferredPaused
	}
	p := &deferredWorkPause{done: make(chan struct{}), resume: make(chan struct{})}
	l.paused = p
	if l.active == 0 {
		close(p.done)
		p.joined = true
	}
	l.mu.Unlock()
	resume := func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.paused != p {
			return ErrDeferredPaused
		}
		l.paused = nil
		close(p.resume)
		s.deferred.nudge()
		return nil
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), resume())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, resume())
	}
	return resume, nil
}
