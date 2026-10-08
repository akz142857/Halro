package app

import (
	"context"
	"errors"
	"sync"
)

var errUsageSummaryUnavailable = errors.New("usage summary is busy or retired")

// One zero-queue operation through actual HTTP response completion. Retirement
// is permanent; a canceled drain continues to preserve the active ticket.
// This count bound is not the shared physical RAM/disk fee contract.
type usageSummaryLifecycle struct {
	mu      sync.Mutex
	retired bool
	done    chan struct{}
}

func (s *usageSummaryLifecycle) enter(ctx context.Context) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.retired || s.done != nil {
		return nil, errUsageSummaryUnavailable
	}
	done := make(chan struct{})
	s.done = done
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); defer s.mu.Unlock(); close(done); s.done = nil }) }, nil
}
func (s *usageSummaryLifecycle) check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.retired {
		return errUsageSummaryUnavailable
	}
	return nil
}
func (s *usageSummaryLifecycle) withdraw() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	return s.done
}
func (s *usageSummaryLifecycle) drain(ctx context.Context) error {
	done := s.withdraw()
	if done == nil {
		return ctx.Err()
	}
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
