package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func projectRegistryEntry(m *Manager, id string) (*projectAdmission, int, int) {
	m.projectLocks.mu.Lock()
	defer m.projectLocks.mu.Unlock()
	a := m.projectLocks.entries[id]
	users := 0
	if a != nil {
		users = a.users
	}
	return a, users, len(m.projectLocks.entries)
}
func projectRegistryEmpty(t *testing.T, m *Manager) {
	t.Helper()
	m.projectLocks.mu.Lock()
	defer m.projectLocks.mu.Unlock()
	if len(m.projectLocks.entries) != 0 || m.projectLocks.entries != nil {
		t.Fatal("idle registry retained identities/backing map", len(m.projectLocks.entries))
	}
}
func waitProjectRegistry(t *testing.T, m *Manager, id string, wantUsers int, closing string) *projectAdmission {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, users, _ := projectRegistryEntry(m, id)
		if a != nil && users == wantUsers {
			if closing == "" {
				return a
			}
			a.mu.Lock()
			marked := a.closingRuns[closing]
			a.mu.Unlock()
			if marked {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("project registry phase did not arrive", id, users, closing)
		}
		time.Sleep(time.Millisecond)
	}
}
func projectRegistrySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("project lock phase did not arrive")
	}
}

func TestProjectRegistryColdChurnStateAndDiskKeepDurableBalances(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint("disk=", disk), func(t *testing.T) {
			var m *Manager
			if disk {
				m, _, _ = newProjectedManagerFixture(t, ledger.Options{MaxBatch: 1})
			} else {
				var closeLog func()
				m, _, closeLog = newTestManager(t)
				defer closeLog()
			}
			ctx := context.Background()
			// Cold unknown control-plane reads used to allocate permanent locks.
			for i := 0; i < 1000; i++ {
				if _, err := m.ActiveRun(ctx, fmt.Sprint("cold_", i), "absent"); !errors.Is(err, ErrRunNotFound) {
					t.Fatal(err)
				}
				projectRegistryEmpty(t, m)
			}
			for i := 0; i < 100; i++ {
				project := fmt.Sprint("native_", i)
				request, err := m.BeginRequest(ctx, project, fmt.Sprint("req_", i))
				if err != nil {
					t.Fatal(err)
				}
				attempt, err := m.ReserveAttempt(ctx, request, 100, 7)
				if err != nil {
					t.Fatal(err)
				}
				projectRegistryEmpty(t, m) // durable reservation belongs to selected owner
				if err := m.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
					t.Fatal(err)
				}
				if err := m.Finalize(ctx, request, "success"); err != nil {
					t.Fatal(err)
				}
				key := ledger.BalanceKey{ProjectID: project, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
				balance, err := m.ReadBalance(ctx, key)
				if err != nil || balance.CommittedMicrosUSD != 3 || balance.ReservedMicrosUSD != 0 {
					t.Fatal("eviction changed persistent money", balance, err)
				}
				projectRegistryEmpty(t, m)
			}
			if m.log.Stats().Records != 400 {
				t.Fatal("native lifecycle omitted facts", m.log.Stats())
			}
		})
	}
}

func TestProjectRegistryRetainsMoneyAndLifecycleWaitersWithoutSplitLocks(t *testing.T) {
	for _, money := range []bool{true, false} {
		t.Run(fmt.Sprint("money=", money), func(t *testing.T) {
			m := &Manager{}
			var first *projectAdmission
			var unlockFirst func()
			if money {
				first, unlockFirst = m.lockProject("shared")
			} else {
				unlockFirst = m.governanceReadLock("shared")
				first, _, _ = projectRegistryEntry(m, "shared")
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var firstOnce sync.Once
			releaseFirst := func() { firstOnce.Do(unlockFirst) }
			t.Cleanup(func() {
				releaseFirst()
				releaseOnce.Do(func() { close(release) })
				projectRegistrySignal(t, done)
			})
			go func() {
				var unlock func()
				if money {
					var a *projectAdmission
					a, unlock = m.lockProject("shared")
					if a != first {
						t.Error("money waiter changed identity")
					}
				} else {
					unlock = m.governanceLock("shared")
				}
				close(entered)
				<-release
				unlock()
				close(done)
			}()
			waitProjectRegistry(t, m, "shared", 2, "") // waiter owns identity BEFORE taking lock
			releaseFirst()
			projectRegistrySignal(t, entered)
			if a, users, _ := projectRegistryEntry(m, "shared"); a != first || users != 1 {
				t.Fatal("last old holder discarded admitted waiter", a, users)
			}
			// A third kind of caller must retain the same identity as the waiter.
			if money {
				unlock := m.governanceReadLock("shared")
				a, users, _ := projectRegistryEntry(m, "shared")
				if a != first || users != 2 {
					t.Error("lifecycle split from money lock")
				}
				unlock()
			} else {
				a, unlock := m.lockProject("shared")
				if a != first {
					t.Error("money split from lifecycle lock")
				}
				unlock()
			}
			releaseOnce.Do(func() { close(release) })
			projectRegistrySignal(t, done)
			projectRegistryEmpty(t, m)
		})
	}
}

func TestProjectRegistryPendingRetainedUntilNativeConfirmationActuallyExits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	state := ledger.NewState()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), ledger.NewStatus(), ledger.Options{
		ChainKey: testChainKey, MaxBatch: 1, AfterDurable: func(b ledger.DurableBatch) (uint64, error) { return b.LastSequence, nil },
		WaitConfirmed: func(ctx context.Context, index uint64) error {
			if index != 2 {
				return nil
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	m, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := m.BeginRequest(context.Background(), "pending", "req_pending")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.ReserveAttempt(ctx, request, 100, 7); done <- err }()
	projectRegistrySignal(t, entered)
	original, users, count := projectRegistryEntry(m, "pending")
	if original == nil || users != 0 || count != 1 {
		t.Fatal("in-flight obligation was treated as idle", users, count)
	}
	key := ledger.BalanceKey{ProjectID: "pending", PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
	a, unlock := m.lockProject("pending")
	if a != original || a.pending[key] != 7 {
		t.Error("admitted money lost its identity")
	}
	unlock()
	for i := 0; i < 1000; i++ {
		if _, err := m.ActiveRun(context.Background(), fmt.Sprint("unrelated_", i), "absent"); !errors.Is(err, ErrRunNotFound) {
			t.Fatal(err)
		}
	}
	if a, users, count := projectRegistryEntry(m, "pending"); a != original || users != 0 || count != 1 {
		t.Fatal("cold churn displaced a pending project", users, count)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	projectRegistryEmpty(t, m)
	balance := state.Balance(key.ProjectID, key.PeriodID, key.TimezoneVersion)
	if balance.ReservedMicrosUSD != 7 || state.Watermark().Sequence != 2 {
		t.Fatal("idle cleanup expired authoritative reservation", balance)
	}
}

func TestProjectRegistryRunCloseMarkerSurvivesBetweenMoneyCalls(t *testing.T) {
	m, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	unit, _, err := m.CreateWorkUnit(ctx, "run_project", "key", 10, testGovernanceIntent("unit"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := m.CreateRun(ctx, "run_project", "key", unit.ID, 100, time.Hour, 10, testGovernanceIntent("run"))
	if err != nil {
		t.Fatal(err)
	}
	period, err := m.periods.PeriodAt(m.localNow())
	if err != nil {
		t.Fatal(err)
	}
	key := ledger.BalanceKey{ProjectID: "run_project", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}
	if err := m.admitForRun(ctx, key, 100, 7, true, unit.ID, run.ID, m.localNow()); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releasePending := func() { releaseOnce.Do(func() { m.releaseAdmittedForRun(key, 7, run.ID) }) }
	defer releasePending()
	original, _, _ := projectRegistryEntry(m, "run_project")
	closed := make(chan error, 1)
	go func() {
		_, _, err := m.CloseRun(ctx, "run_project", "key", unit.ID, run.ID, "completed", testGovernanceIntent("close"))
		closed <- err
	}()
	waitProjectRegistry(t, m, "run_project", 2, run.ID) // lifecycle writer + Cond waiter
	if err := m.admitForRun(ctx, key, 100, 1, true, unit.ID, run.ID, m.localNow()); !errors.Is(err, ErrRunNotActive) {
		t.Fatal("close lost the decision order", err)
	}
	if a, _, _ := projectRegistryEntry(m, "run_project"); a != original {
		t.Fatal("closing marker got a new lock identity")
	}
	releasePending()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run close did not leave its wait")
	}
	projectRegistryEmpty(t, m)
	got, found, err := m.ReadRun(ctx, "run_project", run.ID)
	if err != nil || !found || got.Status != domain.RunClosed {
		t.Fatal("Run terminal fact changed", got, found, err)
	}
	projectRegistryEmpty(t, m)
}
