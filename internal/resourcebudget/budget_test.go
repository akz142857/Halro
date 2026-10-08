package resourcebudget

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func fixtureLimits() Limits {
	l := Limits{Total: Cost{65536, 1000, 1000}, RecoveryReserve: Cost{8192, 200, 200}, MaxClaims: 8, RecoveryClaims: 2, MaxOwners: 4, MaxPressureAge: time.Minute}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[Recovery] = l.RecoveryReserve
	l.Classes[Control] = Cost{RAM: 8 * ClaimRAMBytes}
	return l
}
func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(fixtureLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}
func zeroUsage(t *testing.T, m *Manager) {
	t.Helper()
	s := m.Snapshot()
	if s.Used != (Cost{}) || s.Normal != (Cost{}) || s.Recovery != (Cost{}) || s.CompletionReserved != (Cost{}) || s.NormalClaims != 0 || s.RecoveryClaims != 0 || s.Owners != 0 {
		t.Fatalf("leaked resources %+v", s)
	}
	for _, c := range s.Classes {
		if c != (Cost{}) {
			t.Fatal("class leak", s)
		}
	}
}

func TestLimitsRejectInvalidDimensionsAndControlReserve(t *testing.T) {
	for _, mode := range []string{"zero-total", "negative", "class-limit", "control", "recovery-control", "recovery-class", "claims", "recovery-slots", "owners", "unused-reserve", "pressure-age"} {
		t.Run(mode, func(t *testing.T) {
			l := fixtureLimits()
			switch mode {
			case "zero-total":
				l.Total.RAM = 0
			case "negative":
				l.Total.Disk = -1
			case "class-limit":
				l.Classes[IO].Unconfirmed = l.Total.Unconfirmed + 1
			case "control":
				l.Classes[Control].RAM--
			case "recovery-control":
				l.RecoveryReserve.RAM = ClaimRAMBytes
			case "recovery-class":
				l.Classes[Recovery].Disk = l.RecoveryReserve.Disk + 1
			case "claims":
				l.MaxClaims = 0
			case "recovery-slots":
				l.RecoveryClaims = l.MaxClaims
			case "owners":
				l.MaxOwners = maxOwners + 1
			case "unused-reserve":
				l.RecoveryClaims = 0
			case "pressure-age":
				l.MaxPressureAge = 0
			}
			if _, err := New(l); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
}

func TestExpiredPressureStopsNewWorkWithoutStrandingOwnedCompletion(t *testing.T) {
	m, err := New(fixtureLimits())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	m.SetPressure(Healthy)
	w, err := m.TryAcquireWork(IO, Cost{RAM: 100}, Cost{RAM: 50})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(m.limits.MaxPressureAge)
	if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal("expired pressure admitted", err)
	}
	if m.Snapshot().Pressure != Unknown {
		t.Fatal("stale pressure reported healthy")
	}
	recovery, err := m.TryAcquireRecovery(Cost{RAM: 100})
	if err != nil {
		t.Fatal(err)
	}
	completion, err := w.Completion.ActivateCompletion()
	if err != nil {
		t.Fatal("completion stranded", err)
	}
	w.Active.Release()
	completion.Release()
	recovery.Release()
	zeroUsage(t, m)
	m.SetPressure(Healthy)
	now = now.Add(-time.Second)
	if m.Snapshot().Pressure != Unknown {
		t.Fatal("clock regression retained healthy")
	}
	if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal(err)
	}
}

func TestRecoveryDiskClaimCanBecomePersistentBackgroundOwnerWithoutAdmissionGap(t *testing.T) {
	m := fixtureManager(t)
	startup, err := m.TryAcquireRecovery(Cost{Disk: 200})
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	if err := m.SetPressure(Unknown); err != nil {
		t.Fatal(err)
	}
	background, err := startup.Reclassify(Background)
	if err != nil {
		t.Fatal("same already-charged persistent owner could not leave recovery reserve", err)
	}
	after := m.Snapshot()
	if before.Used != after.Used || after.Recovery != (Cost{}) || after.RecoveryClaims != 0 || after.NormalClaims != 1 || after.Classes[Background].Disk != 200 {
		t.Fatal("reclassification changed total charge or lost class accounting", before, after)
	}
	if _, err := m.TryAcquire(Background, Cost{Disk: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal("reclassification accidentally admitted new work while pressure is unknown", err)
	}
	if err := background.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestRecoveryDiskReclassificationRefusesOverfullBackgroundClass(t *testing.T) {
	limits := fixtureLimits()
	limits.Classes[Background].Disk = 750
	m, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(Healthy); err != nil {
		t.Fatal(err)
	}
	existing, err := m.TryAcquire(Background, Cost{Disk: 700})
	if err != nil {
		t.Fatal(err)
	}
	startup, err := m.TryAcquireRecovery(Cost{Disk: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startup.Reclassify(Background); !errors.Is(err, ErrExhausted) {
		t.Fatal("overfull class accepted persistent owner", err)
	}
	if s := m.Snapshot(); s.RecoveryClaims != 1 || s.Classes[Recovery].Disk != 100 {
		t.Fatal("failed reclassification lost original recovery claim", s)
	}
	_ = startup.Release()
	_ = existing.Release()
	zeroUsage(t, m)
}

func TestAtomicWorkAdmissionChecksAllDimensions(t *testing.T) {
	for _, mode := range []string{"ram", "disk", "unconfirmed", "class", "claims"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			active, finish := Cost{2048, 100, 100}, Cost{1024, 100, 100}
			switch mode {
			case "ram":
				active.RAM = 55000
			case "disk":
				active.Disk = 701
			case "unconfirmed":
				finish.Unconfirmed = 701
			case "class":
				m.limits.Classes[Completion].RAM = 1023
			case "claims":
				for i := 0; i < 5; i++ {
					if _, err := m.TryAcquire(IO, Cost{RAM: 1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := m.Snapshot()
			if _, err := m.TryAcquireWork(Active, active, finish); !errors.Is(err, ErrExhausted) {
				t.Fatalf("admission %v", err)
			}
			after := m.Snapshot()
			if after.Used != before.Used || after.Normal != before.Normal || after.NormalClaims != before.NormalClaims || after.CompletionReserved != before.CompletionReserved || after.Classes != before.Classes || after.Acquired != before.Acquired {
				t.Fatal("partial reservation", before, after)
			}
			if after.Rejected[Active] != before.Rejected[Active]+1 {
				t.Fatal("rejection missing")
			}
		})
	}
}

func TestRecoveryHasProtectedBytesAndClaimSlots(t *testing.T) {
	m := fixtureManager(t)
	var normal []Permit
	for i := 0; i < 6; i++ {
		p, err := m.TryAcquire(IO, Cost{1000, 100, 100})
		if err != nil {
			t.Fatal(err)
		}
		normal = append(normal, p)
	}
	if _, err := m.TryAcquire(Background, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	m.SetPressure(Unknown)
	if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
		t.Fatal("unknown opened admission", err)
	}
	var recovery []Permit
	for i := 0; i < 2; i++ {
		p, err := m.TryAcquireRecovery(Cost{100, 100, 100})
		if err != nil {
			t.Fatal("recovery blocked by normal work", err)
		}
		recovery = append(recovery, p)
	}
	if _, err := m.TryAcquireRecovery(Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
		t.Fatal("unbounded recovery", err)
	}
	if got := m.Snapshot(); got.NormalClaims != 6 || got.RecoveryClaims != 2 || got.Used.Disk != 800 || got.Recovery.Disk != 200 || got.Classes[Control].RAM != 8*ClaimRAMBytes {
		t.Fatal(got)
	}
	for _, p := range append(normal, recovery...) {
		if err := p.Release(); err != nil {
			t.Fatal(err)
		}
	}
	zeroUsage(t, m)
}

func TestCompletionCreditSurvivesPressureAndAdmissionClose(t *testing.T) {
	m := fixtureManager(t)
	w, err := m.TryAcquireWork(Active, Cost{4096, 10, 20}, Cost{1024, 30, 40})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != (Cost{4096 + 1024 + 2*ClaimRAMBytes, 40, 60}) || got.CompletionReserved != (Cost{1024, 30, 40}) {
		t.Fatal(got)
	}
	if _, err := w.Completion.Cost(); !errors.Is(err, ErrReserved) {
		t.Fatal("reserved used early", err)
	}
	if _, err := w.Completion.Retain(); !errors.Is(err, ErrReserved) {
		t.Fatal("reserved shared", err)
	}
	if err := w.Completion.ReleasePart(Cost{RAM: 1}); !errors.Is(err, ErrReserved) {
		t.Fatal(err)
	}
	credit, err := w.Completion.Transfer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Completion.ActivateCompletion(); !errors.Is(err, ErrInvalid) {
		t.Fatal("old owner activated", err)
	}
	m.SetPressure(Pressured)
	m.CloseAdmission()
	if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := m.TryAcquireRecovery(Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := w.Active.Release(); err != nil {
		t.Fatal(err)
	}
	completion, err := credit.ActivateCompletion()
	if err != nil {
		t.Fatal("owned completion stranded", err)
	}
	if _, err := credit.ActivateCompletion(); !errors.Is(err, ErrInvalid) {
		t.Fatal("credit activated twice", err)
	}
	if err := credit.Release(); !errors.Is(err, ErrInvalid) {
		t.Fatal("old credit released live work", err)
	}
	if got, err := completion.Cost(); err != nil || got != (Cost{1024, 30, 40}) {
		t.Fatal(got, err)
	}
	if m.Snapshot().CompletionReserved != (Cost{}) {
		t.Fatal("reserved after activation")
	}
	if err := completion.ReleasePart(Cost{RAM: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := completion.ReleasePart(Cost{Unconfirmed: 40}); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != (Cost{ClaimRAMBytes, 30, 0}) || got.NormalClaims != 1 {
		t.Fatal("disk retired too early", got)
	}
	if err := completion.ReleasePart(Cost{Disk: 30}); err != nil {
		t.Fatal(err)
	}
	if err := completion.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestOwnershipMoveSharedLastReleaseAndPartialRetirement(t *testing.T) {
	m := fixtureManager(t)
	p, err := m.TryAcquire(IO, Cost{4096, 300, 400})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := p.Retain()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ReleasePart(Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("shared resource partially released", err)
	}
	moved, err := p.Transfer()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{p.Release, func() error { _, e := p.Transfer(); return e }, func() error { _, e := p.Retain(); return e }} {
		if !errors.Is(op(), ErrInvalid) {
			t.Fatal("old handle usable")
		}
	}
	if err := shared.Release(); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != (Cost{4096 + ClaimRAMBytes, 300, 400}) || got.Owners != 1 {
		t.Fatal("premature release", got)
	}
	before := m.Snapshot()
	for _, cost := range []Cost{{-1, 0, 0}, {0, 301, 0}, {}, {0, 0, 401}} {
		if !errors.Is(moved.ReleasePart(cost), ErrInvalid) {
			t.Fatal("invalid part accepted")
		}
	}
	if !reflect.DeepEqual(before, m.Snapshot()) {
		t.Fatal("invalid part changed accounting")
	}
	if err := moved.ReleasePart(Cost{RAM: 4096}); err != nil {
		t.Fatal(err)
	}
	persisted, err := moved.Transfer()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := persisted.Cost(); err != nil || got != (Cost{0, 300, 400}) {
		t.Fatal(got, err)
	}
	if err := persisted.ReleasePart(Cost{Unconfirmed: 400}); err != nil {
		t.Fatal(err)
	}
	if err := persisted.ReleasePart(Cost{Disk: 300}); err != nil {
		t.Fatal(err)
	}
	if err := persisted.Release(); err != nil {
		t.Fatal(err)
	}
	if err := persisted.Release(); !errors.Is(err, ErrInvalid) {
		t.Fatal("double release", err)
	}
	zeroUsage(t, m)
}

func TestOwnerSlotLimitReuseDoesNotReviveOldHandle(t *testing.T) {
	m := fixtureManager(t)
	p, err := m.TryAcquire(Cache, Cost{RAM: 1024})
	if err != nil {
		t.Fatal(err)
	}
	owners := []Permit{p}
	for i := 1; i < 4; i++ {
		q, err := p.Retain()
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, q)
	}
	if _, err := p.Retain(); !errors.Is(err, ErrOwners) {
		t.Fatal(err)
	}
	old := owners[1]
	if err := old.Release(); err != nil {
		t.Fatal(err)
	}
	replacement, err := p.Retain()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Release(); !errors.Is(err, ErrInvalid) {
		t.Fatal("old slot revived", err)
	}
	owners[1] = replacement
	for _, q := range owners {
		if err := q.Release(); err != nil {
			t.Fatal(err)
		}
	}
	zeroUsage(t, m)
}

func TestPersistentOwnerMergeRevokesBothHandlesAndPreservesCharges(t *testing.T) {
	m := fixtureManager(t)
	a, err := m.TryAcquire(IO, Cost{0, 100, 200})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TryAcquire(IO, Cost{0, 50, 75})
	if err != nil {
		t.Fatal(err)
	}
	m.SetPressure(Unknown)
	merged, err := a.Merge(b)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := merged.Cost(); err != nil || got != (Cost{0, 150, 275}) {
		t.Fatal(got, err)
	}
	if got := m.Snapshot(); got.Used != (Cost{ClaimRAMBytes, 150, 275}) || got.NormalClaims != 1 || got.Owners != 1 {
		t.Fatal("persistent aggregate lost data charge", got)
	}
	if !errors.Is(a.Release(), ErrInvalid) || !errors.Is(b.Release(), ErrInvalid) {
		t.Fatal("merged old handle usable")
	}
	if err := merged.ReleasePart(Cost{Unconfirmed: 275}); err != nil {
		t.Fatal(err)
	}
	if err := merged.ReleasePart(Cost{Disk: 150}); err != nil {
		t.Fatal(err)
	}
	if err := merged.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
	for _, mode := range []string{"same", "manager", "class", "shared", "reserved"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			a, _ := m.TryAcquire(IO, Cost{RAM: 100})
			b, _ := m.TryAcquire(IO, Cost{RAM: 100})
			want := ErrInvalid
			switch mode {
			case "same":
				b = a
			case "manager":
				other := fixtureManager(t)
				b, _ = other.TryAcquire(IO, Cost{RAM: 100})
			case "class":
				b, _ = m.TryAcquire(Cache, Cost{RAM: 100})
			case "shared":
				a.Retain()
			case "reserved":
				w, _ := m.TryAcquireWork(IO, Cost{RAM: 100}, Cost{RAM: 100})
				x, _ := m.TryAcquireWork(IO, Cost{RAM: 100}, Cost{RAM: 100})
				a, b = w.Completion, x.Completion
				want = ErrReserved
			}
			before := m.Snapshot()
			if _, err := a.Merge(b); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, m.Snapshot()) {
				t.Fatal("failed merge changed budget")
			}
		})
	}
}

func TestRecoveryMergeKeepsReserveAccountingAndAllowsNextBoundedClaim(t *testing.T) {
	m := fixtureManager(t)
	m.SetPressure(Unknown)
	a, err := m.TryAcquireRecovery(Cost{100, 80, 80})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TryAcquireRecovery(Cost{100, 80, 80})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := a.Merge(b)
	if err != nil {
		t.Fatal(err)
	}
	next, err := m.TryAcquireRecovery(Cost{100, 40, 40})
	if err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); s.RecoveryClaims != 2 || s.NormalClaims != 0 || s.Recovery != (Cost{300 + 2*ClaimRAMBytes, 200, 200}) || s.Classes[Recovery] != (Cost{300, 200, 200}) {
		t.Fatal("reserve merge accounting", s)
	}
	merged.Release()
	next.Release()
	zeroUsage(t, m)
}

func BenchmarkAcquireRelease(b *testing.B) {
	m, err := New(fixtureLimits())
	if err != nil {
		b.Fatal(err)
	}
	m.SetPressure(Healthy)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := m.TryAcquire(IO, Cost{RAM: 256})
		if err != nil {
			b.Fatal(err)
		}
		if err := p.Release(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOwnershipTransfer(b *testing.B) {
	m, err := New(fixtureLimits())
	if err != nil {
		b.Fatal(err)
	}
	m.SetPressure(Healthy)
	p, err := m.TryAcquire(IO, Cost{RAM: 256})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err = p.Transfer()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	p.Release()
}

func BenchmarkRetainRelease(b *testing.B) {
	m, err := New(fixtureLimits())
	if err != nil {
		b.Fatal(err)
	}
	m.SetPressure(Healthy)
	p, err := m.TryAcquire(IO, Cost{RAM: 256})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		shared, err := p.Retain()
		if err != nil {
			b.Fatal(err)
		}
		if err := shared.Release(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	p.Release()
}

func TestInvalidOverflowZeroHandlesAndSnapshotIsolation(t *testing.T) {
	m := fixtureManager(t)
	for _, class := range []Class{Completion, Recovery, Control, ClassCount, 255} {
		if _, err := m.TryAcquire(class, Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	for _, cost := range []Cost{{}, {RAM: -1}, {Disk: -1}, {Unconfirmed: -1}} {
		if _, err := m.TryAcquire(IO, cost); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if err := (Permit{}).Release(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := (Permit{}).Cost(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := (Permit{}).Retain(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := (Permit{}).Transfer(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := (Permit{}).ActivateCompletion(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	s := m.Snapshot()
	s.Classes[IO] = Cost{RAM: 1}
	if m.Snapshot().Classes[IO] != (Cost{}) {
		t.Fatal("snapshot alias")
	}
	l := fixtureLimits()
	l.Total = Cost{math.MaxInt64, math.MaxInt64, math.MaxInt64}
	l.RecoveryReserve = Cost{}
	l.RecoveryClaims = 0
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[Recovery] = Cost{}
	l.Classes[Control] = Cost{RAM: 8 * ClaimRAMBytes}
	huge, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	huge.SetPressure(Healthy)
	if _, err := huge.TryAcquireWork(IO, Cost{RAM: math.MaxInt64 - ClaimRAMBytes}, Cost{RAM: ClaimRAMBytes + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("overflow admitted", err)
	}
	zeroUsage(t, huge)
	if err := m.SetPressure(Pressure(255)); !errors.Is(err, ErrInvalid) || m.Snapshot().Pressure != Healthy {
		t.Fatal("invalid pressure applied")
	}
	m.stats.Acquired[IO] = math.MaxUint64
	m.stats.Rejected[IO] = math.MaxUint64
	p, err := m.TryAcquire(IO, Cost{RAM: 1})
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	m.SetPressure(Unknown)
	m.TryAcquire(IO, Cost{RAM: 1})
	if got := m.Snapshot(); got.Acquired[IO] != math.MaxUint64 || got.Rejected[IO] != math.MaxUint64 {
		t.Fatal("counter overflow", got)
	}
}

func TestConcurrentAdmissionsOwnershipAndFinalRelease(t *testing.T) {
	m := fixtureManager(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w, err := m.TryAcquireWork(IO, Cost{RAM: 512}, Cost{RAM: 256})
				if errors.Is(err, ErrExhausted) {
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				shared, err := w.Active.Retain()
				if err != nil {
					errs <- err
					return
				}
				moved, err := w.Active.Transfer()
				if err != nil {
					errs <- err
					return
				}
				if err := w.Active.Release(); !errors.Is(err, ErrInvalid) {
					errs <- errors.New("old handle released")
					return
				}
				if err := shared.Release(); err != nil {
					errs <- err
					return
				}
				if err := moved.Release(); err != nil {
					errs <- err
					return
				}
				completion, err := w.Completion.ActivateCompletion()
				if err != nil {
					errs <- err
					return
				}
				if err := completion.Release(); err != nil {
					errs <- err
					return
				}
				s := m.Snapshot()
				if !fits(Cost{}, s.Used, m.limits.Total) || s.NormalClaims > 6 || s.Owners < 0 {
					errs <- errors.New("budget exceeded during churn")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if m.Snapshot().Acquired[Completion] == 0 {
		t.Fatal("admission unexercised")
	}
	zeroUsage(t, m)
}

func TestConcurrentTransferVersusReleaseHasOneOwner(t *testing.T) {
	m := fixtureManager(t)
	for i := 0; i < 100; i++ {
		p, err := m.TryAcquire(IO, Cost{RAM: 1})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			moved, err := p.Transfer()
			if errors.Is(err, ErrInvalid) {
				return
			}
			if err != nil {
				errs <- err
				return
			}
			if err := moved.Release(); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if err := p.Release(); err != nil && !errors.Is(err, ErrInvalid) {
				errs <- err
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		zeroUsage(t, m)
	}
}
