package bolt

import (
	"context"
	"errors"
	"sync"

	bbolt "go.etcd.io/bbolt"
)

var ErrMetadataRecoveryRequired = errors.New("metadata native/projection boundary requires close and authenticated recovery")

// Store operations own the native handles until their actual exit. The db
// pointer stays immutable; Close revokes admission before closing that handle.
// Callbacks must return before Close can finish (they must not call Close).
type storeLifetime struct {
	mu             sync.Mutex
	changed        *sync.Cond
	active         uint64
	closing        bool
	withdrawn      bool
	paused         *facadePause
	withdrawDone   chan struct{}
	withdrawJoined bool
	done           chan struct{}
	err            error
	fault          error
	ctx            context.Context
	cancel         context.CancelFunc
}

func (s *Store) enter() (func(), error) {
	if s == nil || s.db == nil && s.facade == nil {
		return nil, bbolt.ErrDatabaseNotOpen
	}
	l := &s.lifetime
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.withdrawn || l.paused != nil {
		return nil, bbolt.ErrDatabaseNotOpen
	}
	if l.fault != nil {
		return nil, l.fault
	}
	if l.changed == nil {
		l.changed = sync.NewCond(&l.mu)
		l.ctx, l.cancel = context.WithCancel(context.Background())
	}
	l.active++
	return func() {
		l.mu.Lock()
		l.active--
		if l.active == 0 {
			if l.paused != nil && !l.paused.joined {
				close(l.paused.done)
				l.paused.joined = true
			}
			if l.withdrawDone != nil && !l.withdrawJoined {
				close(l.withdrawDone)
				l.withdrawJoined = true
			}
			l.changed.Broadcast()
		}
		l.mu.Unlock()
	}, nil
}

type facadePause struct {
	done   chan struct{}
	joined bool
}

// PauseFacade joins admitted transactions and batches without canceling their
// native confirmation or releasing the selected backend. It may be resumed by
// its exact owner only; Close/RetireFacade still permanently withdraw admission.
func (s *Store) PauseFacade(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.facade == nil || s.db != nil {
		return nil, ErrFacadeMaintenance
	}
	l := &s.lifetime
	l.mu.Lock()
	if l.closing || l.withdrawn || l.paused != nil || l.fault != nil {
		l.mu.Unlock()
		return nil, ErrFacadeMaintenance
	}
	pause := &facadePause{done: make(chan struct{})}
	l.paused = pause
	if l.active == 0 {
		close(pause.done)
		pause.joined = true
	}
	l.mu.Unlock()
	resume := func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.paused != pause {
			return ErrFacadeMaintenance
		}
		l.paused = nil
		if l.closing || l.withdrawn || l.fault != nil {
			return errors.Join(ErrFacadeMaintenance, l.fault)
		}
		return nil
	}
	s.flushBatches()
	select {
	case <-pause.done:
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), resume())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, resume())
	}
	return resume, nil
}

// RecoveryError returns this exact Store's permanently latched failure. A
// facade additionally reports its actual owner's busy/closing availability;
// those states never report healthy and ordinary busy is not permanently latched.
// A real native/projection failure cannot be cleared by returning space.
func (s *Store) RecoveryError() error {
	if s == nil {
		return nil
	}
	s.lifetime.mu.Lock()
	fault, closing := s.lifetime.fault, s.lifetime.closing || s.lifetime.withdrawn
	s.lifetime.mu.Unlock()
	if fault != nil {
		return fault
	}
	if s.facade != nil {
		if closing {
			return bbolt.ErrDatabaseNotOpen
		}
		return s.facade.HealthError()
	}
	return nil
}

// RetireFacade withdraws and joins the business DAO while keeping its selected
// physical Metadata owner open for a common seal. Only the authenticated facade
// supports this handoff. Close still owns the eventual actual backend retirement.
// A timeout leaves admission withdrawn; it never permits early root deletion.
func (s *Store) RetireFacade(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.facade == nil || s.db != nil {
		return ErrFacadeMaintenance
	}
	l := &s.lifetime
	l.mu.Lock()
	if l.withdrawDone == nil {
		l.withdrawn = true
		l.withdrawDone = make(chan struct{})
		if l.cancel != nil {
			l.cancel()
		}
		if l.active == 0 {
			close(l.withdrawDone)
			l.withdrawJoined = true
		}
	}
	done := l.withdrawDone
	l.mu.Unlock()
	s.flushBatches()
	select {
	case <-done:
		return s.facade.HealthError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) requireMetadataRecovery(cause error) error {
	s.lifetime.mu.Lock()
	defer s.lifetime.mu.Unlock()
	if s.lifetime.fault == nil {
		s.lifetime.fault = errors.Join(ErrMetadataRecoveryRequired, cause)
		if s.lifetime.cancel != nil {
			s.lifetime.cancel()
		}
	}
	return s.lifetime.fault
}

// Cancellation stops a network confirmation wait, but never rolls back a
// committed transaction or reports the resources retired before callback exit.
func (s *Store) confirmationContext(parent context.Context) (context.Context, func()) {
	l := &s.lifetime
	l.mu.Lock()
	closing := l.ctx
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(closing, cancel)
	if closing.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

// Private native operations use this only while holding a Store ticket.
func (s *Store) closingContext() context.Context {
	s.lifetime.mu.Lock()
	defer s.lifetime.mu.Unlock()
	return s.lifetime.ctx
}
