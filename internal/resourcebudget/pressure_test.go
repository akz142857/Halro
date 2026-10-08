package resourcebudget

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func testPolicy() PressurePolicy {
	return PressurePolicy{RuntimeEnter: 100, RuntimeExit: 80, CgroupEnter: 200, CgroupExit: 160, HeadroomEnter: 20, HeadroomExit: 40, DiskEnter: 100, DiskExit: 200, InodesEnter: 10, InodesExit: 20, MaxAge: 10 * time.Second, Cooldown: 2 * time.Second}
}
func lowSample(at time.Time) Sample {
	return Sample{At: at, RuntimeUsed: 50, DiskAvailable: 500, InodesAvailable: 50, GroupCount: 1, CgroupID: [32]byte{1}, Groups: [MaxCgroupDepth]GroupMemory{{Current: 100, Max: 500, MaxLimited: true}}}
}
func testController(t *testing.T) *Controller {
	t.Helper()
	c, err := NewController(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func healthyDecision(t *testing.T, c *Controller, at time.Time) Decision {
	t.Helper()
	if d := c.Observe(lowSample(at), at); d.State() != Unknown {
		t.Fatal(d)
	}
	d := c.Observe(lowSample(at.Add(2*time.Second)), at.Add(2*time.Second))
	if d.State() != Healthy || d.Issues() != 0 {
		t.Fatal(d)
	}
	return d
}

func TestPressurePolicyRejectsUncalibratedThresholds(t *testing.T) {
	for _, change := range []func(*PressurePolicy){func(p *PressurePolicy) { p.RuntimeExit = 0 }, func(p *PressurePolicy) { p.RuntimeEnter = p.RuntimeExit }, func(p *PressurePolicy) { p.CgroupEnter = p.CgroupExit }, func(p *PressurePolicy) { p.HeadroomExit = p.HeadroomEnter }, func(p *PressurePolicy) { p.DiskExit = p.DiskEnter }, func(p *PressurePolicy) { p.InodesExit = p.InodesEnter }, func(p *PressurePolicy) { p.MaxAge = 0 }, func(p *PressurePolicy) { p.Cooldown = 0 }} {
		p := testPolicy()
		change(&p)
		if _, err := NewController(p); !errors.Is(err, ErrInvalid) {
			t.Fatal(p, err)
		}
	}
}
func TestPressureHysteresisRequiresContinuousLowSamples(t *testing.T) {
	c := testController(t)
	at := time.Unix(1000, 0)
	healthyDecision(t, c, at)
	s := lowSample(at.Add(3 * time.Second))
	s.RuntimeUsed = 90
	if d := c.Observe(s, s.At); d.State() != Healthy {
		t.Fatal("healthy must tolerate middle band", d)
	}
	s.At = at.Add(4 * time.Second)
	s.RuntimeUsed = 100
	if d := c.Observe(s, s.At); d.State() != Pressured || d.Issues()&RuntimeHigh == 0 {
		t.Fatal(d)
	}
	for _, tc := range []struct {
		second int
		ram    int64
		want   Pressure
	}{{5, 50, Pressured}, {6, 90, Pressured}, {7, 50, Pressured}, {8, 50, Pressured}, {9, 50, Healthy}} {
		s = lowSample(at.Add(time.Duration(tc.second) * time.Second))
		s.RuntimeUsed = tc.ram
		if d := c.Observe(s, s.At); d.State() != tc.want {
			t.Fatal(tc, d)
		}
	}
}
func TestPressureSignalsAreIndependentAndAncestorsConstrainHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Sample)
		issue  SignalIssue
	}{
		{"runtime", func(s *Sample) { s.RuntimeUsed = 100 }, RuntimeHigh},
		{"leaf", func(s *Sample) { s.Groups[0].Current = 200 }, CgroupHigh},
		{"disk", func(s *Sample) { s.DiskAvailable = 100 }, DiskLow},
		{"inode", func(s *Sample) { s.InodesAvailable = 10 }, InodesLow},
		{"parent-max", func(s *Sample) {
			s.GroupCount = 2
			s.Groups[1] = GroupMemory{Current: 990, Max: 1000, MaxLimited: true}
		}, CgroupHeadroomLow},
		{"parent-high", func(s *Sample) {
			s.GroupCount = 2
			s.Groups[1] = GroupMemory{Current: 1010, High: 1000, HighLimited: true}
		}, CgroupHeadroomLow},
		{"zero-hard-limit", func(s *Sample) { s.Groups[0].Max = 0 }, CgroupHeadroomLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testController(t)
			s := lowSample(time.Unix(1000, 0))
			tc.change(&s)
			d := c.Observe(s, s.At)
			if d.State() != Pressured || d.Issues()&tc.issue == 0 {
				t.Fatal(d)
			}
		})
	}
	c := testController(t)
	at := time.Unix(1000, 0)
	for second := 0; second <= 2; second++ {
		s := lowSample(at.Add(time.Duration(second) * time.Second))
		s.RuntimeUsed = 80
		s.Groups[0].Current = 160
		s.GroupCount = 2
		s.Groups[1] = GroupMemory{Current: 10000}
		d := c.Observe(s, s.At)
		if second == 2 && d.State() != Healthy {
			t.Fatal("do not add runtime, leaf, or sibling-inclusive parent usage", d)
		}
	}
}
func TestMissingStaleChangedAndDiscontinuousSignalsResetCooling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Sample, *time.Time)
		issue  SignalIssue
	}{
		{"missing", func(s *Sample, _ *time.Time) { s.Issues = CgroupUnavailable }, CgroupUnavailable},
		{"stale", func(s *Sample, n *time.Time) { *n = s.At.Add(10 * time.Second) }, SampleStale},
		{"future", func(s *Sample, n *time.Time) { *n = s.At.Add(-time.Second) }, ClockInvalid},
		{"repeated", func(s *Sample, _ *time.Time) { s.At = s.At.Add(-time.Second) }, SampleRepeated},
		{"source", func(s *Sample, _ *time.Time) { s.CgroupID[0] = 2 }, SourceChanged},
		{"gap", func(s *Sample, n *time.Time) { s.At = s.At.Add(10 * time.Second); *n = s.At }, SampleGap},
		{"negative", func(s *Sample, _ *time.Time) { s.Groups[0].High = -1 }, SignalInvalid},
		{"depth", func(s *Sample, _ *time.Time) { s.GroupCount = MaxCgroupDepth + 1 }, SignalInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testController(t)
			at := time.Unix(1000, 0)
			healthyDecision(t, c, at)
			s := lowSample(at.Add(3 * time.Second))
			now := s.At
			tc.change(&s, &now)
			d := c.Observe(s, now)
			if d.State() != Unknown || d.Issues()&tc.issue == 0 {
				t.Fatal(d)
			}
			resume := now.Add(time.Second)
			s.At = resume
			s.Issues = 0
			s.GroupCount = 1
			s.Groups[0].High = 0
			d = c.Observe(s, resume)
			if d.State() == Healthy {
				t.Fatal("no immediate reopening", d)
			}
		})
	}
}
func TestPressureClockRegressionDoesNotResetClockHighWater(t *testing.T) {
	c := testController(t)
	at := time.Unix(1000, 0)
	healthyDecision(t, c, at)
	for _, second := range []int{1, 1, 2} {
		s := lowSample(at.Add(time.Duration(second) * time.Second))
		d := c.Observe(s, s.At)
		if d.State() != Unknown {
			t.Fatal(d)
		}
	}
}
func boundManager(t *testing.T, c *Controller, now *time.Time) *Manager {
	t.Helper()
	m, err := New(fixtureLimits())
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return *now }
	if err = m.BindController(c); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestOlderHealthyDecisionCannotReopenNewerPressureOrUnknown(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		c := testController(t)
		at := time.Unix(1000, 0)
		old := healthyDecision(t, c, at)
		now := at.Add(3 * time.Second)
		m := boundManager(t, c, &now)
		s := lowSample(now)
		if unknown {
			s.Issues = DiskUnavailable
		} else {
			s.RuntimeUsed = 100
		}
		newer := c.Observe(s, now)
		if err := m.ApplyPressure(newer); err != nil {
			t.Fatal(err)
		}
		if err := m.ApplyPressure(old); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if m.Snapshot().Pressure != newer.State() {
			t.Fatal("stale decision reopened admission")
		}
		if err := m.SetPressure(Healthy); !errors.Is(err, ErrInvalid) {
			t.Fatal("manual bypass", err)
		}
		if err := m.BindController(c); !errors.Is(err, ErrInvalid) {
			t.Fatal("rebind", err)
		}
	}
}
func TestHealthyLeaseUsesOriginalSampleAndRejectsReplayAndOtherAuthority(t *testing.T) {
	c := testController(t)
	at := time.Unix(1000, 0)
	d := healthyDecision(t, c, at)
	now := at.Add(9 * time.Second)
	m := boundManager(t, c, &now)
	if err := m.ApplyPressure(d); err != nil {
		t.Fatal(err)
	}
	other := testController(t)
	if err := m.ApplyPressure(healthyDecision(t, other, at)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if m.Snapshot().Pressure != Healthy {
		t.Fatal("foreign decisions should not change state")
	}
	if err := m.ApplyPressure(d); !errors.Is(err, ErrInvalid) {
		t.Fatal("replay", err)
	}
	now = at.Add(12 * time.Second)
	if m.Snapshot().Pressure != Unknown {
		t.Fatal("delayed apply extended original TTL")
	}
	c = testController(t)
	d = healthyDecision(t, c, at)
	m = boundManager(t, c, &now)
	if err := m.ApplyPressure(d); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired at first apply", err)
	}
}
func TestOutOfOrderConcurrentApplicationPreservesNewestDecision(t *testing.T) {
	c := testController(t)
	at := time.Unix(1000, 0)
	old := healthyDecision(t, c, at)
	now := at.Add(3 * time.Second)
	m := boundManager(t, c, &now)
	s := lowSample(now)
	s.RuntimeUsed = 100
	latest := c.Observe(s, now)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.ApplyPressure(old) }()
	}
	if err := m.ApplyPressure(latest); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if m.Snapshot().Pressure != Pressured {
		t.Fatal("old healthy decision won")
	}
}
func TestPressureDecisionSequenceExhaustionFailsClosed(t *testing.T) {
	c := testController(t)
	at := time.Unix(1000, 0)
	healthyDecision(t, c, at)
	c.sequence = math.MaxUint64 - 1
	now := at.Add(3 * time.Second)
	m := boundManager(t, c, &now)
	d := c.Observe(lowSample(now), now)
	if d.State() != Unknown || d.Issues()&SignalInvalid == 0 {
		t.Fatal(d)
	}
	if err := m.ApplyPressure(d); err != nil {
		t.Fatal(err)
	}
	d = c.Observe(lowSample(now.Add(time.Second)), now.Add(time.Second))
	if d.State() != Unknown {
		t.Fatal(d)
	}
	if err := m.ApplyPressure(d); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
