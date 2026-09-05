package budget

// This file is an S0 design harness. It deliberately does not change the
// production Manager or any persisted event. Its job is to falsify the proposed
// locking shape before a Ledger epoch is committed to it.

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

type s0RunBalance struct {
	cap       int64
	committed int64
	reserved  int64
	active    bool
}

type s0DualAdmission struct {
	mu sync.Mutex

	projectCap       int64
	projectCommitted int64
	projectReserved  int64
	projectPending   int64
	runPending       map[string]int64
	runs             map[string]*s0RunBalance
}

func newS0DualAdmission(projectCap int64, runCaps map[string]int64) *s0DualAdmission {
	runs := make(map[string]*s0RunBalance, len(runCaps))
	for runID, cap := range runCaps {
		runs[runID] = &s0RunBalance{cap: cap, active: true}
	}
	return &s0DualAdmission{
		projectCap: projectCap,
		runPending: make(map[string]int64, len(runCaps)),
		runs:       runs,
	}
}

// admit mirrors the proposed critical section: project and run balances are
// checked and both pending counters move under one mutex. An empty runID is the
// unchanged project-only path.
func (a *s0DualAdmission) admit(runID string, amount int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if amount < 0 {
		return false
	}
	projectTotal, ok := s0Add(a.projectCommitted, a.projectReserved, a.projectPending, amount)
	if !ok || a.projectCap > 0 && projectTotal > a.projectCap {
		return false
	}
	if runID != "" {
		run, exists := a.runs[runID]
		if !exists || !run.active {
			return false
		}
		runTotal, valid := s0Add(run.committed, run.reserved, a.runPending[runID], amount)
		if !valid || run.cap > 0 && runTotal > run.cap {
			return false
		}
	}
	a.projectPending += amount
	if runID != "" {
		a.runPending[runID] += amount
	}
	return true
}

func (a *s0DualAdmission) persist(runID string, amount int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if amount < 0 || a.projectPending < amount {
		panic("invalid S0 persist")
	}
	a.projectPending -= amount
	a.projectReserved += amount
	if runID != "" {
		run := a.runs[runID]
		if run == nil || a.runPending[runID] < amount {
			panic("invalid S0 run persist")
		}
		a.runPending[runID] -= amount
		run.reserved += amount
	}
}

func (a *s0DualAdmission) settle(runID string, reservation, committed int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if reservation < 0 || committed < 0 || a.projectReserved < reservation {
		panic("invalid S0 settlement")
	}
	a.projectReserved -= reservation
	a.projectCommitted += committed
	if runID != "" {
		run := a.runs[runID]
		if run == nil || run.reserved < reservation {
			panic("invalid S0 run settlement")
		}
		run.reserved -= reservation
		run.committed += committed
	}
}

func (a *s0DualAdmission) releasePending(runID string, amount int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if amount < 0 || a.projectPending < amount {
		panic("invalid S0 pending release")
	}
	a.projectPending -= amount
	if runID != "" {
		if a.runPending[runID] < amount {
			panic("invalid S0 run pending release")
		}
		a.runPending[runID] -= amount
	}
}

func (a *s0DualAdmission) closeRun(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if run := a.runs[runID]; run != nil {
		run.active = false
	}
}

func s0Add(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value > 0 && total > math.MaxInt64-value || value < 0 && total < math.MinInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func TestS0DualAdmissionNeverOverAdmitsSameRun(t *testing.T) {
	const cap = int64(100)
	admission := newS0DualAdmission(cap, map[string]int64{"run-a": cap})
	start := make(chan struct{})
	var accepted atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 64; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if admission.admit("run-a", 10) {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("accepted=%d want=10", accepted.Load())
	}
	if admission.projectPending != cap || admission.runPending["run-a"] != cap {
		t.Fatalf("pending project=%d run=%d want=%d", admission.projectPending, admission.runPending["run-a"], cap)
	}
}

func TestS0DualAdmissionManyRunsShareProjectCap(t *testing.T) {
	admission := newS0DualAdmission(100, map[string]int64{"run-a": 100, "run-b": 100})
	if !admission.admit("run-a", 60) {
		t.Fatal("first run was refused")
	}
	if admission.admit("run-b", 60) {
		t.Fatal("second run bypassed the shared project cap")
	}
	if !admission.admit("run-b", 40) {
		t.Fatal("remaining project headroom was refused")
	}
}

func TestS0DualAdmissionSettlementCanRestoreHeadroom(t *testing.T) {
	admission := newS0DualAdmission(100, map[string]int64{"run-a": 100})
	if !admission.admit("run-a", 100) {
		t.Fatal("reservation was refused")
	}
	admission.persist("run-a", 100)
	if admission.admit("run-a", 1) {
		t.Fatal("fully reserved run admitted more spend")
	}
	admission.settle("run-a", 100, 60)
	if !admission.admit("run-a", 40) {
		t.Fatal("lower settlement did not restore run and project headroom")
	}
}

func TestS0RunCloseSerializesWithAdmission(t *testing.T) {
	admission := newS0DualAdmission(100, map[string]int64{"run-a": 100})
	if !admission.admit("run-a", 50) {
		t.Fatal("reservation before close was refused")
	}
	admission.closeRun("run-a")
	if admission.admit("run-a", 1) {
		t.Fatal("closed run admitted a new reservation")
	}
	admission.persist("run-a", 50)
	admission.settle("run-a", 50, 50)
}

// BenchmarkS0AdmissionPrototype compares the shipped project-only decision with
// the proposed one-lock Project+Run decision. It is not a release threshold;
// record the host and raw output before using the ratio as evidence.
func BenchmarkS0AdmissionPrototype(b *testing.B) {
	for _, workers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("current-project-only/workers=%d", workers), func(b *testing.B) {
			manager, _, closeLog := newTestManager(b)
			defer closeLog()
			key := ledger.BalanceKey{ProjectID: "project-s0", PeriodID: "2026-09-04", TimezoneVersion: 1}
			runS0Workers(b, workers, func() bool {
				if err := manager.admit(key, math.MaxInt64, 1, true); err != nil {
					return false
				}
				manager.releaseAdmitted(key, 1)
				return true
			})
		})
		b.Run(fmt.Sprintf("proposed-project-and-run/workers=%d", workers), func(b *testing.B) {
			admission := newS0DualAdmission(math.MaxInt64, map[string]int64{"run-s0": math.MaxInt64})
			runS0Workers(b, workers, func() bool {
				if !admission.admit("run-s0", 1) {
					return false
				}
				admission.releasePending("run-s0", 1)
				return true
			})
		})
	}
}

func runS0Workers(b *testing.B, workers int, operation func() bool) {
	b.Helper()
	var next atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	b.ResetTimer()
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= b.N {
					return
				}
				if !operation() {
					failures.Add(1)
				}
			}
		}()
	}
	group.Wait()
	b.StopTimer()
	if failures.Load() != 0 {
		b.Fatalf("failed operations=%d", failures.Load())
	}
}
