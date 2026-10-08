package resourcebudget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorManagerRenewsUnchangedHealthyAndPreservesOwnedCreditsOnBlockedCancel(t *testing.T) {
	block, entered := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(block) })
	var blocking atomic.Bool
	m, err := NewMonitor(sampleFunc(func(context.Context) Sample {
		s := lowSample(time.Now())
		if blocking.Load() {
			close(entered)
			<-block
		}
		return s
	}), monitorPolicy(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureManager(t)
	if err = m.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	if _, err = b.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal("unstarted owner admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	waitPressure(t, m, Healthy)
	// These acquisitions extend over several controller TTLs with unchanged
	// state; using onChange alone to publish would expire the manager.
	for i := 0; i < 12; i++ {
		p, err := b.TryAcquire(IO, Cost{RAM: 1})
		if err != nil {
			t.Fatal("healthy lease not renewed", i, err, b.Snapshot())
		}
		_ = p.Release()
		time.Sleep(10 * time.Millisecond)
	}
	work, err := b.TryAcquireWork(Active, Cost{RAM: 100}, Cost{RAM: 100})
	if err != nil {
		t.Fatal(err)
	}
	blocking.Store(true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source did not block")
	}
	cancel()
	if s := b.Snapshot(); s.Pressure != Unknown || s.Closed {
		t.Fatal(s)
	}
	if _, err = b.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		t.Fatal("owner returned before synchronous source", err)
	default:
	}
	finish, err := work.Completion.ActivateCompletion()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := b.TryAcquireRecovery(Cost{RAM: 100})
	if err != nil {
		t.Fatal(err)
	}
	_ = finish.Release()
	_ = work.Active.Release()
	_ = recovery.Release()
	unblock.Do(func() { close(block) })
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not join")
	}
	zeroUsage(t, b)
	if s := b.Snapshot(); s.Pressure != Unknown {
		t.Fatal(s)
	}
}

func TestMonitorManagerExclusiveAttachAndLateHealthyCannotUndoCancellation(t *testing.T) {
	newMonitor := func() *Monitor {
		m, err := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), monitorPolicy(), time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m, other, b := newMonitor(), newMonitor(), fixtureManager(t)
	var wg sync.WaitGroup
	var winners atomic.Int64
	for _, monitor := range []*Monitor{m, other} {
		wg.Add(1)
		go func(m *Monitor) {
			defer wg.Done()
			if m.AttachManager(b) == nil {
				winners.Add(1)
			}
		}(monitor)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal(winners.Load())
	}
	if m.manager == nil {
		m = other
	}
	if err := m.AttachManager(fixtureManager(t)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	waitPressure(t, m, Healthy)
	if err := m.AttachManager(fixtureManager(t)); !errors.Is(err, ErrInvalid) {
		t.Fatal("late attach", err)
	}
	// Produce a valid newer decision, then delay it behind the manager lock.
	d := m.controller.Observe(lowSample(time.Now()), time.Now())
	b.mu.Lock()
	applied := make(chan error, 1)
	go func() { applied <- b.ApplyPressure(d) }()
	cancel()
	b.mu.Unlock()
	<-applied // May be superseded by the sampling owner; either case must close.
	if s := b.Snapshot(); s.Pressure != Unknown {
		t.Fatal("late healthy reopened cancelled owner", s)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owner did not join")
	}
	if _, err := b.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal(err)
	}
}

func TestMonitorManagerExpiredApplyStaysClosedAndRecoversUsingFreshCooldown(t *testing.T) {
	p := monitorPolicy()
	p.Cooldown = 10 * time.Millisecond
	m, err := NewMonitor(sampleFunc(func(context.Context) Sample { return lowSample(time.Now()) }), p, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureManager(t)
	var delayed atomic.Bool
	delayed.Store(true)
	b.now = func() time.Time {
		if delayed.Load() {
			return time.Now().Add(2 * p.MaxAge)
		}
		return time.Now()
	}
	if err = m.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && m.Snapshot().Issues&SampleStale == 0 {
		time.Sleep(time.Millisecond)
	}
	s := m.Snapshot()
	if !s.Running || s.State != Unknown || s.Issues&SampleStale == 0 {
		t.Fatal("expired Apply stopped owner or leaked healthy", s)
	}
	if b.Snapshot().Pressure != Unknown {
		t.Fatal(b.Snapshot())
	}
	delayed.Store(false)
	waitPressure(t, m, Healthy)
	permit, err := b.TryAcquire(IO, Cost{RAM: 1})
	if err != nil {
		t.Fatal("fresh samples did not recover", err)
	}
	_ = permit.Release()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owner did not join")
	}
	zeroUsage(t, b)
}
