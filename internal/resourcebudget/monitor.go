package resourcebudget

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// SampleSource is a synchronous, bounded-work sampler. Run owns its calls;
// implementations must not create a goroutine per sample or hide a queue.
type SampleSource interface{ Sample(context.Context) Sample }

type PressureSnapshot struct {
	State   Pressure
	Issues  SignalIssue
	Sample  Sample
	Samples uint64
	Running bool
}

// Monitor owns a single sampling loop and fixed-size latest state. Run is single
// use; WithSampling may own sequential finite sessions. Snapshot never
// waits for IO. Logical resource claims remain the Manager's separate contract.
type Monitor struct {
	mu                sync.Mutex
	controller        *Controller
	manager           *Manager
	source            SampleSource
	interval          time.Duration
	now               func() time.Time
	started, running  bool
	runUsed, sampling bool
	longLived         bool
	done              chan struct{}
	runErr            error
	ctx               context.Context
	latest            PressureSnapshot
	decision          Decision
}

// AttachManager gives this monitor exclusive pressure authority.
// Binding is atomic and only allowed before its first sampling owner. No manager operation calls
// back into the monitor, and no additional cancellation goroutine is needed.
func (m *Monitor) AttachManager(b *Manager) error {
	if b == nil {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.manager != nil {
		return ErrInvalid
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.controller != nil {
		return ErrInvalid
	}
	b.controller, b.monitorOwned = m.controller, true
	b.stats.Pressure = Unknown
	b.healthyAt, b.healthyUntil = time.Time{}, time.Time{}
	m.manager = b
	return nil
}

func NewMonitor(source SampleSource, p PressurePolicy, interval time.Duration) (*Monitor, error) {
	if source == nil || interval <= 0 || interval >= p.MaxAge {
		return nil, ErrInvalid
	}
	c, err := NewController(p)
	if err != nil {
		return nil, err
	}
	return &Monitor{source: source, controller: c, interval: interval, now: time.Now, latest: PressureSnapshot{State: Unknown, Issues: MonitorInactive}}, nil
}

// Run is synchronous; the caller owns exactly one goroutine and joins it.
// onChange runs outside locks only when state/issues change, not per sample.
// The runtime callback only logs; it must not enqueue work or wait for storage.
func (m *Monitor) Run(ctx context.Context, onChange func(PressureSnapshot)) error {
	if ctx == nil {
		return ErrInvalid
	}
	m.mu.Lock()
	if m.runUsed || m.running || m.sampling {
		m.mu.Unlock()
		return ErrInvalid
	}
	m.runUsed = true
	owner, cancel := context.WithCancel(ctx)
	m.startLocked(owner, true)
	m.mu.Unlock()
	defer cancel()
	return m.sampleLoop(owner, onChange)
}

// startLocked retains the original controller's sequence and source identity;
// a newly owned lifetime must earn a complete fresh cooldown.
func (m *Monitor) startLocked(ctx context.Context, longLived bool) {
	m.controller.resetRecovery()
	m.started, m.running, m.ctx = true, true, ctx
	m.longLived, m.done, m.runErr = longLived, make(chan struct{}), nil
	m.decision = Decision{}
	m.latest.State, m.latest.Issues = Unknown, MonitorInactive
	if m.manager != nil {
		m.manager.mu.Lock()
		m.manager.monitorContext = ctx
		m.manager.stats.Pressure = Unknown
		m.manager.healthyAt, m.manager.healthyUntil = time.Time{}, time.Time{}
		m.manager.mu.Unlock()
	}
}

func (m *Monitor) sampleLoop(ctx context.Context, onChange func(PressureSnapshot)) (result error) {
	defer func() {
		m.mu.Lock()
		if m.manager != nil {
			m.manager.mu.Lock()
			m.manager.monitorContext = nil
			m.manager.refreshPressureLocked()
			m.manager.mu.Unlock()
		}
		m.running = false
		m.latest.State = Unknown
		m.latest.Issues |= MonitorInactive | SampleCancelled
		m.runErr = result
		close(m.done)
		m.mu.Unlock()
	}()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sample := m.source.Sample(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		d := m.controller.Observe(sample, m.now())
		if m.manager != nil {
			// Refresh on every sample, including unchanged Healthy decisions.
			// Manager also checks ctx.Err itself if Sample blocks on shutdown.
			if err := m.manager.ApplyPressure(d); err != nil {
				if !errors.Is(err, errPressureLease) {
					return err
				}
				// Scheduling or lock contention can outlive a fresh lease. Stay
				// closed, reset cooldown, and keep the same sampling owner alive.
				d.state = Unknown
				d.issues |= SampleStale
				m.controller.resetRecovery()
			}
		}
		m.mu.Lock()
		changed := m.latest.State != d.State() || m.latest.Issues != d.Issues()
		count := m.latest.Samples
		if count < math.MaxUint64 {
			count++
		}
		m.decision = d
		m.latest = PressureSnapshot{State: d.State(), Issues: d.Issues(), Sample: sample, Samples: count, Running: true}
		snapshot := m.latest
		m.mu.Unlock()
		if changed && onChange != nil {
			onChange(snapshot)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Monitor) Snapshot() PressureSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.latest
	out.Running = m.running
	if !m.running || m.ctx.Err() != nil {
		out.State = Unknown
		out.Issues |= MonitorInactive
		if m.running {
			out.Issues |= SampleCancelled
		}
		return out
	}
	if out.Samples == 0 {
		out.State = Unknown
		out.Issues |= MonitorInactive
		return out
	}
	now := m.now()
	if m.decision.ObservedAt().IsZero() || now.Before(m.decision.ObservedAt()) {
		out.State = Unknown
		out.Issues |= ClockInvalid
	}
	if !now.Before(m.decision.ValidUntil()) {
		out.State = Unknown
		out.Issues |= SampleStale
	}
	return out
}
