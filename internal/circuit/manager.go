package circuit

import (
	"errors"
	"sync"
	"time"
)

var ErrOpen = errors.New("circuit breaker is open")

type Config struct {
	FailureThreshold    int
	OpenDuration        time.Duration
	HalfOpenMaxRequests int
}

type state struct {
	failures  int
	openUntil time.Time
	halfOpen  int
}

type Manager struct {
	mu      sync.Mutex
	config  Config
	targets map[string]*state
}

type Lease struct {
	once     sync.Once
	manager  *Manager
	targetID string
}

func New(config Config) (*Manager, error) {
	if config.FailureThreshold <= 0 {
		return nil, errors.New("failure threshold must be positive")
	}
	if config.OpenDuration <= 0 {
		return nil, errors.New("open duration must be positive")
	}
	if config.HalfOpenMaxRequests <= 0 {
		config.HalfOpenMaxRequests = 1
	}
	return &Manager{config: config, targets: make(map[string]*state)}, nil
}

func (m *Manager) Acquire(targetID string, now time.Time) (*Lease, error) {
	if targetID == "" {
		return nil, errors.New("target id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.targets[targetID]
	if current == nil {
		current = &state{}
		m.targets[targetID] = current
	}
	if !current.openUntil.IsZero() {
		if now.Before(current.openUntil) || current.halfOpen >= m.config.HalfOpenMaxRequests {
			return nil, ErrOpen
		}
		current.halfOpen++
	}
	return &Lease{manager: m, targetID: targetID}, nil
}

// Abandon releases the attempt without letting it speak for the target. Use it
// whenever the request never reached the provider — a local concurrency,
// budget, pricing, or policy rejection, or a caller that went away — so a
// half-open probe that tested nothing neither closes the circuit nor counts
// against it. Reporting these as success would let a purely local rejection
// clear an open circuit and send traffic back to a provider still down.
func (l *Lease) Abandon() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.mu.Lock()
		defer l.manager.mu.Unlock()
		current := l.manager.targets[l.targetID]
		if current == nil {
			return
		}
		if current.halfOpen > 0 {
			current.halfOpen--
		}
	})
}

// Done records an attempt that actually reached the provider. A nil failure
// means the provider answered and the circuit closes; a non-nil failure counts
// toward opening it. Attempts that never reached the provider must call
// Abandon instead — passing nil here would report them as provider health.
func (l *Lease) Done(failure error, now time.Time) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.mu.Lock()
		defer l.manager.mu.Unlock()
		current := l.manager.targets[l.targetID]
		if current == nil {
			return
		}
		if failure == nil {
			current.failures = 0
			current.openUntil = time.Time{}
			current.halfOpen = 0
			return
		}
		current.failures++
		if current.halfOpen > 0 {
			current.halfOpen--
		}
		if current.failures >= l.manager.config.FailureThreshold {
			current.openUntil = now.Add(l.manager.config.OpenDuration)
		}
	})
}

func (m *Manager) IsOpen(targetID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.targets[targetID]
	return current != nil && !current.openUntil.IsZero() &&
		(now.Before(current.openUntil) || current.halfOpen >= m.config.HalfOpenMaxRequests)
}
