package app

import (
	"context"
	"sync"
)

// Metrics remain available during business drain for HA/pressure diagnosis.
// Terminal Close alone withdraws them and joins actual response writes. The
// configured scrape cap bounds tickets, including direct renderer callers.
type usageMetricsLifecycle struct {
	mu      sync.Mutex
	retired bool
	active  int
	done    chan struct{}
}

func (s *usageMetricsLifecycle) enter(limit int) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired || limit < 1 || limit > 32 || s.active >= limit {
		return nil, errUsageSummaryUnavailable
	}
	if s.active == 0 {
		s.done = make(chan struct{})
	}
	s.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			if s.active == 0 {
				close(s.done)
				s.done = nil
			}
		})
	}, nil
}

func (s *usageMetricsLifecycle) withdraw() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	return s.done
}

func (s *usageMetricsLifecycle) drain(ctx context.Context) error {
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

func (r *Runtime) withdrawUsageMetrics() {
	if r.usage != nil {
		r.usage.metrics.withdraw()
	}
}
func (r *Runtime) drainUsageMetrics(ctx context.Context) error {
	if r.usage == nil {
		return ctx.Err()
	}
	return r.usage.metrics.drain(ctx)
}
