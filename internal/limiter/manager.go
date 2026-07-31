package limiter

import (
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

var (
	ErrRPM         = errors.New("requests per minute limit exceeded")
	ErrTPM         = errors.New("tokens per minute limit exceeded")
	ErrConcurrency = errors.New("concurrency limit exceeded")
)

type Error struct {
	Kind       error
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }

type bucket struct {
	tokens float64
	last   time.Time
	limit  int64
}

type projectState struct {
	rpm         bucket
	tpm         bucket
	concurrency int64
}

type Manager struct {
	mu       sync.Mutex
	projects map[string]*projectState
}

type Lease struct {
	releaseOnce   sync.Once
	reconcileOnce sync.Once
	release       func()
	reconcile     func(int64, time.Time)
}

func New() *Manager {
	return &Manager{projects: make(map[string]*projectState)}
}

// Acquire atomically applies RPM, TPM, and concurrency admission. Zero limits
// are unlimited. Rate tokens are consumed on admission; concurrency is released
// exactly once through the returned lease.
func (m *Manager) Acquire(project domain.Project, estimatedTokens int64, now time.Time) (*Lease, error) {
	if estimatedTokens < 0 {
		return nil, errors.New("estimated tokens cannot be negative")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.projects[project.ID]
	if state == nil {
		state = &projectState{}
		m.projects[project.ID] = state
	}
	refill(&state.rpm, project.RPM, now)
	refill(&state.tpm, project.TPM, now)
	if project.MaxConcurrency > 0 && state.concurrency >= project.MaxConcurrency {
		return nil, &Error{Kind: ErrConcurrency, RetryAfter: time.Second}
	}
	if project.RPM > 0 && state.rpm.tokens < 1 {
		return nil, &Error{
			Kind: ErrRPM, RetryAfter: refillWait(1-state.rpm.tokens, project.RPM),
		}
	}
	if project.TPM > 0 && state.tpm.tokens < float64(estimatedTokens) {
		return nil, &Error{
			Kind: ErrTPM,
			RetryAfter: refillWait(
				float64(estimatedTokens)-state.tpm.tokens, project.TPM,
			),
		}
	}
	if project.RPM > 0 {
		state.rpm.tokens--
	}
	if project.TPM > 0 {
		state.tpm.tokens -= float64(estimatedTokens)
	}
	state.concurrency++
	return &Lease{release: func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.projects[project.ID]
		if current != nil && current.concurrency > 0 {
			current.concurrency--
		}
	}, reconcile: func(actualTokens int64, reconciledAt time.Time) {
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.projects[project.ID]
		if current == nil || current.tpm.limit <= 0 {
			return
		}
		limit := current.tpm.limit
		refill(&current.tpm, limit, reconciledAt)
		delta := estimatedTokens - actualTokens
		current.tpm.tokens += float64(delta)
		if current.tpm.tokens > float64(limit) {
			current.tpm.tokens = float64(limit)
		}
	}}, nil
}

func refillWait(missing float64, perMinute int64) time.Duration {
	if missing <= 0 || perMinute <= 0 {
		return 0
	}
	seconds := missing * 60 / float64(perMinute)
	wait := time.Duration(seconds * float64(time.Second))
	if wait < time.Millisecond {
		return time.Millisecond
	}
	return wait
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.releaseOnce.Do(l.release)
}

// Reconcile replaces the admission estimate with actual Provider usage. A
// lower actual value refunds only unused capacity and never exceeds the bucket
// cap; a higher value creates debt that must refill before future admission.
func (l *Lease) Reconcile(actualTokens int64, now time.Time) error {
	if l == nil {
		return nil
	}
	if actualTokens < 0 {
		return errors.New("actual tokens cannot be negative")
	}
	l.reconcileOnce.Do(func() { l.reconcile(actualTokens, now) })
	return nil
}

func refill(value *bucket, limit int64, now time.Time) {
	if limit <= 0 {
		value.limit = limit
		value.tokens = 0
		value.last = now
		return
	}
	if value.limit != limit || value.last.IsZero() {
		value.limit = limit
		value.tokens = float64(limit)
		value.last = now
		return
	}
	if now.Before(value.last) {
		value.last = now
		return
	}
	elapsed := now.Sub(value.last).Seconds()
	value.tokens += elapsed * float64(limit) / 60
	if value.tokens > float64(limit) {
		value.tokens = float64(limit)
	}
	value.last = now
}
