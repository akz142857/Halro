package resourcebudget

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorSamplingCancellationWhileSuccessfulCallbackJoins(t *testing.T) {
	entered, block, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample {
		s := lowSample(time.Now())
		if calls.Add(1) == 3 {
			close(entered)
			<-block
		}
		return s
	}), monitorPolicy(), time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.WithSampling(ctx, func(context.Context) error { <-entered; close(returned); return nil })
	}()
	<-returned
	select {
	case err := <-done:
		t.Fatal("successful callback dropped actual source", err)
	case <-time.After(5 * time.Millisecond):
	}
	cancel()
	close(block)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancel while joining lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed to join")
	}
}

func TestMonitorSamplingLoopFailureRetainedAndInvalidCallsDoNotOwn(t *testing.T) {
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), monitorPolicy(), time.Millisecond)
	var nilMonitor *Monitor
	for _, err := range []error{
		nilMonitor.WithSampling(context.Background(), func(context.Context) error { return nil }),
		m.WithSampling(nil, func(context.Context) error { return nil }),
		m.WithSampling(context.Background(), nil),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	b := fixtureManager(t)
	if err := m.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	// Exhaust the actual bound Manager's decision sequence. The sampling loop
	// must preserve its real ApplyPressure failure and join, rather than publish
	// a synthetic healthy decision or silently retry under another controller.
	b.mu.Lock()
	b.pressureSequence = math.MaxUint64
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := m.WithSampling(ctx, func(context.Context) error { t.Error("failed authority visited"); return nil })
	if !errors.Is(err, ErrInvalid) || m.Snapshot().Running || b.Snapshot().Pressure != Unknown {
		t.Fatal(err, m.Snapshot())
	}
}

func TestMonitorSamplingActualSamplerMissingFilesystemFailsClosed(t *testing.T) {
	dir := t.TempDir()
	source, err := NewSampler(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	m, err := NewMonitor(source, monitorPolicy(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureManager(t)
	if err = m.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = m.WithSampling(ctx, func(context.Context) error { t.Error("actual missing filesystem admitted"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || m.Snapshot().Sample.Issues&DiskUnavailable == 0 {
		t.Fatal(err, m.Snapshot())
	}
	if m.source != source || b.Snapshot().Pressure != Unknown || m.Snapshot().Running {
		t.Fatal("actual source replaced or still ready")
	}
}

func TestMonitorSamplingCallbackFailureAndPanicJoinOwner(t *testing.T) {
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), monitorPolicy(), time.Millisecond)
	want := errors.New("callback failed")
	if err := m.WithSampling(context.Background(), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != want {
				t.Error("panic changed")
			}
		}()
		_ = m.WithSampling(context.Background(), func(context.Context) error { panic(want) })
	}()
	if m.Snapshot().Running {
		t.Fatal("panic leaked owner")
	}
	if err := m.WithSampling(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal("failed to reuse joined owner", err)
	}
}

func TestMonitorSamplingSequentialFreshCooldownAndSingleRun(t *testing.T) {
	p := monitorPolicy()
	p.Cooldown = 12 * time.Millisecond
	var calls atomic.Int64
	m, err := NewMonitor(sampleFunc(func(context.Context) Sample { calls.Add(1); return lowSample(time.Now()) }), p, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureManager(t)
	if err = m.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	controller := m.controller
	for i := 0; i < 3; i++ {
		before := calls.Load()
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = m.WithSampling(ctx, func(ctx context.Context) error {
			if ctx.Err() != nil || m.controller != controller || m.manager != b {
				t.Fatal("authority changed")
			}
			permit, e := b.TryAcquire(IO, Cost{RAM: 1})
			if e != nil {
				return e
			}
			return permit.Release()
		})
		cancel()
		if err != nil {
			t.Fatal(i, err)
		}
		if time.Since(start) < p.Cooldown || calls.Load()-before < 2 {
			t.Fatal("prior healthy reused", i)
		}
		if m.Snapshot().Running || b.Snapshot().Pressure != Unknown {
			t.Fatal("session still ready")
		}
		if _, e := b.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(e, ErrPressure) {
			t.Fatal(e)
		}
	}
	if err = m.AttachManager(fixtureManager(t)); !errors.Is(err, ErrInvalid) {
		t.Fatal("late binding", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	waitPressure(t, m, Healthy)
	cancel()
	<-done
	if err = m.Run(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("Run restarted", err)
	}
	zeroUsage(t, b)
}

func TestMonitorSamplingRejectsConcurrentSessionAndRun(t *testing.T) {
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), monitorPolicy(), time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithSampling(context.Background(), func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	if e := m.WithSampling(context.Background(), func(context.Context) error { return nil }); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	if e := m.Run(context.Background(), nil); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

func TestMonitorSamplingUnknownAndPressuredNeverVisit(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown", false: "pressured"}[unknown], func(t *testing.T) {
			m, _ := NewMonitor(sampleFunc(func(context.Context) Sample {
				if unknown {
					return Sample{At: time.Now(), Issues: DiskUnavailable}
				}
				s := lowSample(time.Now())
				s.RuntimeUsed = 100
				return s
			}), monitorPolicy(), time.Millisecond)
			b := fixtureManager(t)
			_ = m.AttachManager(b)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			e := m.WithSampling(ctx, func(context.Context) error { t.Error("visited without real healthy"); return nil })
			if !errors.Is(e, context.DeadlineExceeded) || m.Snapshot().Running || b.Snapshot().Pressure != Unknown {
				t.Fatal(e, m.Snapshot())
			}
		})
	}
}

func TestMonitorSamplingCancellationJoinsBlockedSourceAndRetainsCompletion(t *testing.T) {
	entered, block := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var blocking atomic.Bool
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample {
		s := lowSample(time.Now())
		if blocking.Load() {
			once.Do(func() { close(entered) })
			<-block
		}
		return s
	}), monitorPolicy(), time.Millisecond)
	b := fixtureManager(t)
	_ = m.AttachManager(b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.WithSampling(ctx, func(ctx context.Context) error {
			work, e := b.TryAcquireWork(Active, Cost{RAM: 10}, Cost{RAM: 10})
			if e != nil {
				return e
			}
			blocking.Store(true)
			<-entered
			cancel()
			<-ctx.Done()
			finish, e := work.Completion.ActivateCompletion()
			if e != nil {
				return e
			}
			return errors.Join(finish.Release(), work.Active.Release())
		})
	}()
	<-entered
	cancel()
	if _, e := b.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(e, ErrPressure) {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		t.Fatal("returned before actual source joined", e)
	case <-time.After(10 * time.Millisecond):
	}
	if e := m.WithSampling(context.Background(), func(context.Context) error { return nil }); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	close(block)
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("failed to join")
	}
	zeroUsage(t, b)
	if m.Snapshot().Running || b.Snapshot().Pressure != Unknown {
		t.Fatal("late healthy published")
	}
}

func TestMonitorSamplingBorrowsRunAndCancelsOnWithdrawal(t *testing.T) {
	m, _ := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), monitorPolicy(), time.Millisecond)
	b := fixtureManager(t)
	_ = m.AttachManager(b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx, nil) }()
	waitPressure(t, m, Healthy)
	if e := m.WithSampling(context.Background(), func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if !m.Snapshot().Running || ctx.Err() != nil {
		t.Fatal("borrower stopped external owner")
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithSampling(context.Background(), func(ctx context.Context) error { close(entered); <-ctx.Done(); return nil })
	}()
	<-entered
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("borrow not cancelled")
	}
	<-runDone
	if b.Snapshot().Pressure != Unknown {
		t.Fatal("withdrawn authority ready")
	}
}
