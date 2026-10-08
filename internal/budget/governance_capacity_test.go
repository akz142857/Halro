package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestRunGovernanceCapacityAdmissionCountsHistoryAndExpiry(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	// Closed work units retain their history but release open capacity.
	for i := 0; i < 10; i++ {
		unit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent(fmt.Sprint("create-history-", i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := manager.CloseWorkUnit(ctx, "prj_1", "key_1", unit.ID, testGovernanceIntent(fmt.Sprint("close-history-", i))); err != nil {
			t.Fatal(err)
		}
	}
	unit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent("create-live"))
	if err != nil {
		t.Fatal(err)
	}
	if count, err := state.ReadOpenWorkUnitCount(ctx, "prj_1"); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// Other projects' active runs must not consume this project's capacity.
	other, _, err := manager.CreateWorkUnit(ctx, "prj_other", "key_other", 1, testGovernanceIntent("create-other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.CreateRun(ctx, "prj_other", "key_other", other.ID, 100, 72*time.Hour, 1, testGovernanceIntent("run-other")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < domain.MaxRunsPerWorkUnit; i++ {
		run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 1, testGovernanceIntent(fmt.Sprint("create-run-", i)))
		if err != nil {
			t.Fatal(i, err)
		}
		if _, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 1, testGovernanceIntent(fmt.Sprint("refused-active-", i))); !errors.Is(err, ErrResourceLimit) {
			t.Fatal("active limit bypassed", err)
		}
		manager.now = func() time.Time { return run.ExpiresAt }
	}
	before := manager.log.Stats().Records
	if _, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 1, testGovernanceIntent("refused-lifetime")); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("expired history did not retain lifetime limit", err)
	}
	if manager.log.Stats().Records != before {
		t.Fatal("refused lifetime capacity wrote a native fact")
	}
	capacity, err := state.ReadRunCapacity(ctx, "prj_1", unit.ID, manager.now())
	if err != nil || capacity.ActiveRuns != 0 || capacity.WorkUnitRuns != domain.MaxRunsPerWorkUnit {
		t.Fatal(capacity, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := manager.CreateWorkUnit(canceled, "prj_new", "key_1", 1, testGovernanceIntent("canceled-unit")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled capacity read became admission", err)
	}
	if _, _, err := manager.CreateRun(canceled, "prj_1", "key_1", unit.ID, 100, time.Hour, 1, testGovernanceIntent("canceled-run")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read became a limit or successful admission", err)
	}
	if manager.log.Stats().Records != before {
		t.Fatal("canceled capacity query wrote a native fact")
	}
}

func TestRunGovernanceCapacityConcurrentCreatorsKeepProjectLimits(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	var unit domain.WorkUnit
	var lock sync.Mutex
	concurrent := func(create func(int) error) {
		t.Helper()
		var wg sync.WaitGroup
		var accepted, refused atomic.Int64
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				switch err := create(i); {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, ErrResourceLimit):
					refused.Add(1)
				default:
					t.Error(err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if accepted.Load() != 1 || refused.Load() != 31 {
			t.Fatal("project limit changed", accepted.Load(), refused.Load())
		}
	}
	concurrent(func(i int) error {
		got, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent(fmt.Sprint("concurrent-unit-", i)))
		if err == nil {
			lock.Lock()
			unit = got
			lock.Unlock()
		}
		return err
	})
	concurrent(func(i int) error {
		_, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 1, testGovernanceIntent(fmt.Sprint("concurrent-run-", i)))
		return err
	})
}
