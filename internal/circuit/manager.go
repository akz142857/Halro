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
	failures   int
	openUntil  time.Time
	halfOpen   int
	generation uint64
}

type Manager struct {
	mu      sync.Mutex
	config  Config
	targets map[string]*state
}

type Lease struct {
	once       sync.Once
	manager    *Manager
	targetID   string
	generation uint64
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
	current.generation++
	return &Lease{
		manager: m, targetID: targetID, generation: current.generation,
	}, nil
}

// Done reports whether this attempt should affect availability health. A nil
// failure means success; a non-nil failure counts toward opening the circuit.
// Policy/auth/client errors should call Done(nil).
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
