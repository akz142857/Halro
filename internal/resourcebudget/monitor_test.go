package resourcebudget

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type sampleFunc func(context.Context) Sample

func (f sampleFunc) Sample(ctx context.Context) Sample { return f(ctx) }
func waitPressure(t *testing.T, m *Monitor, want Pressure) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("state timeout", m.Snapshot())
}
func monitorPolicy() PressurePolicy {
	p := testPolicy()
	p.Cooldown = time.Nanosecond
	p.MaxAge = 50 * time.Millisecond
	return p
}
func TestMonitorSingleOwnerCancellationJoinsAndCannotRestart(t *testing.T) {
	var high atomic.Bool
	source := sampleFunc(func(context.Context) Sample {
		s := lowSample(time.Now())
		if high.Load() {
			s.RuntimeUsed = 100
		}
		return s
	})
	m, err := NewMonitor(source, monitorPolicy(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); s.State != Unknown || s.Running || s.Samples != 0 || s.Issues&MonitorInactive == 0 {
		t.Fatal(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var callbacks atomic.Int64
	go func() { done <- m.Run(ctx, func(PressureSnapshot) { callbacks.Add(1) }) }()
	waitPressure(t, m, Healthy)
	if err = m.Run(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("second sampling owner", err)
	}
	high.Store(true)
	waitPressure(t, m, Pressured)
	high.Store(false)
	waitPressure(t, m, Healthy)
	before := m.Snapshot()
	before.Sample.Groups[0].Current = 999
	if m.Snapshot().Sample.Groups[0].Current == 999 {
		t.Fatal("snapshot array aliases monitor")
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not join")
	}
	if s := m.Snapshot(); s.State != Unknown || s.Running || s.Issues&MonitorInactive == 0 {
		t.Fatal(s)
	}
	if err = m.Run(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("restart", err)
	}
	if callbacks.Load() < 3 {
		t.Fatal("missing transitions", callbacks.Load())
	}
}
func TestMonitorSlowReadDoesNotHoldSnapshotOrExtendLease(t *testing.T) {
	block, entered := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	source := sampleFunc(func(context.Context) Sample {
		s := lowSample(time.Now())
		if calls.Add(1) == 3 {
			close(entered)
			<-block
		}
		return s
	})
	p := monitorPolicy()
	p.MaxAge = 15 * time.Millisecond
	m, err := NewMonitor(source, p, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read did not block")
	}
	// IO runs outside the state lock. An expired previously healthy sample must
	// close admission even while the next sample is stuck in its source.
	time.Sleep(20 * time.Millisecond)
	result := make(chan PressureSnapshot, 1)
	go func() { result <- m.Snapshot() }()
	select {
	case s := <-result:
		if s.State != Unknown || s.Issues&SampleStale == 0 {
			t.Fatal(s)
		}
	case <-time.After(time.Second):
		t.Fatal("Snapshot waited for source IO")
	}
	cancel()
	if s := m.Snapshot(); s.State != Unknown || s.Issues&SampleCancelled == 0 {
		t.Fatal(s)
	}
	select {
	case <-done:
		t.Fatal("Run claimed to join while OS-like source was blocked")
	default:
	}
	close(block)
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not join after IO released")
	}
	if s := m.Snapshot(); s.State != Unknown || s.Samples != 2 {
		t.Fatal("cancelled late healthy sample published", s)
	}
}
func TestMonitorRejectsInvalidCadenceAndPreservesMissingSignal(t *testing.T) {
	source := sampleFunc(func(context.Context) Sample { return Sample{At: time.Now(), Issues: DiskUnavailable} })
	p := monitorPolicy()
	for _, interval := range []time.Duration{0, -1, p.MaxAge, p.MaxAge + 1} {
		if _, err := NewMonitor(source, p, interval); !errors.Is(err, ErrInvalid) {
			t.Fatal(interval, err)
		}
	}
	if _, err := NewMonitor(nil, p, time.Millisecond); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	m, err := NewMonitor(source, p, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	deadline := time.Now().Add(time.Second)
	for m.Snapshot().Samples == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s := m.Snapshot()
	cancel()
	<-done
	if s.State != Unknown || s.Samples == 0 || s.Issues&DiskUnavailable == 0 {
		t.Fatal(s)
	}
}
