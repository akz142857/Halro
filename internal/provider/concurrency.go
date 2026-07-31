package provider

import (
	"errors"
	"sync"
)

var ErrConcurrency = errors.New("provider concurrency limit exceeded")

type ConcurrencyManager struct {
	mu     sync.Mutex
	active map[string]int64
}

type ConcurrencyLease struct {
	once    sync.Once
	release func()
}

func NewConcurrencyManager() *ConcurrencyManager {
	return &ConcurrencyManager{active: make(map[string]int64)}
}

// Acquire applies a process-wide limit per Provider instance. Zero is
// unlimited. Unlimited Providers are counted too, so a hot-reloaded limit sees
// the actual number of calls already in flight.
func (m *ConcurrencyManager) Acquire(providerID string, limit int64) (*ConcurrencyLease, error) {
	if providerID == "" {
		return nil, errors.New("provider id is required")
	}
	if limit < 0 {
		return nil, errors.New("provider concurrency limit cannot be negative")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > 0 && m.active[providerID] >= limit {
		return nil, ErrConcurrency
	}
	m.active[providerID]++
	return &ConcurrencyLease{release: func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active[providerID] <= 1 {
			delete(m.active, providerID)
			return
		}
		m.active[providerID]--
	}}, nil
}

func (l *ConcurrencyLease) Release() {
	if l != nil {
		l.once.Do(l.release)
	}
}

func (m *ConcurrencyManager) Active() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]int64, len(m.active))
	for providerID, active := range m.active {
		result[providerID] = active
	}
	return result
}
