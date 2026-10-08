package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func recoveryResourceFixture(t *testing.T) (config.Config, *resourcePressureRuntime) {
	t.Helper()
	b := runtimeRAMBudgetFixture()
	m, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return config.Config{ResourceBudget: &b}, &resourcePressureRuntime{budget: m}
}

func TestAccountingRecoveryResourcesRetainThroughCancelledNativeIOAndRetry(t *testing.T) {
	var io *projectedWriterBlockedDurability
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{WrapDurability: func(f *os.File) ledger.DurabilityWriter {
		io = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		return io
	}})
	var once sync.Once
	unblock := func() { once.Do(func() { close(io.release) }) }
	defer unblock()
	lease, p := projectedWriterCandidate(t, l, path, key)
	cfg, pressure := recoveryResourceFixture(t)
	r := &Runtime{config: cfg, ledger: l, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: runtimeAccountingState{projection: p, recovery: &atomic.Bool{}}}
	r.draining.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	io.block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := r.recoverProjectedAccounting(ctx, lease, p, budget.RecoveryStandalone)
		done <- err
	}()
	select {
	case <-io.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery native fsync not reached")
	}
	manager := pressure.logicalBudget()
	checkHeld := func() {
		t.Helper()
		s := manager.Snapshot()
		if s.Pressure != resourcebudget.Unknown || s.RecoveryClaims != 1 || s.Classes[resourcebudget.Recovery].RAM != cfg.ResourceBudget.AccountingRecoveryRAMBytes {
			t.Fatal("recovery did not retain its independent workspace", s)
		}
	}
	checkHeld()
	alias := &Runtime{config: cfg, ledger: l, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: r.accountingState}
	alias.draining.Store(true)
	if _, err := alias.recoverProjectedAccounting(context.Background(), lease, p, budget.RecoveryStandalone); !errors.Is(err, errAccountingRecoveryBusy) {
		t.Fatal("second recovery queued or acquired another workspace", err)
	}
	checkHeld()
	cancel()
	select {
	case err := <-done:
		t.Fatal("cancellation released accepted native IO early", err)
	case <-time.After(10 * time.Millisecond):
	}
	checkHeld()
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("business")); !errors.Is(err, ledger.ErrLedgerMaintenance) {
		t.Fatal("cancelled recovery released native exclusivity early", err)
	}
	unblock()
	if err := <-done; err == nil {
		t.Fatal("cancelled durable recovery succeeded")
	}
	if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 || r.accountingState.recovery.Load() {
		t.Fatal("joined recovery leaked resources or job slot", s)
	}
	if l.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("durable cancelled recovery did not latch recovery-required")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	newLease, fresh := projectedWriterCandidate(t, reopened, path, key)
	r2 := &Runtime{config: cfg, ledger: reopened, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: runtimeAccountingState{projection: fresh, recovery: &atomic.Bool{}}}
	r2.draining.Store(true)
	result, err := r2.recoverProjectedAccounting(context.Background(), newLease, fresh, budget.RecoveryStandalone)
	if err != nil || result.RequestsFinalized != 0 || result.ConfirmedHead.Sequence != 2 {
		t.Fatal("authenticated retry duplicated or lost durable finalization", result, err)
	}
	if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Acquired[resourcebudget.Recovery] != 2 {
		t.Fatal("successful retry retained a workspace", s)
	}
}

func TestAccountingRecoveryResourcesRefuseBeforeFactsAndReleaseConstructorFailure(t *testing.T) {
	for _, shape := range []string{"missing_manager", "missing_config", "exhausted", "wrong_native_cut"} {
		t.Run(shape, func(t *testing.T) {
			l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
			lease, p := projectedWriterCandidate(t, l, path, key)
			cfg, pressure := recoveryResourceFixture(t)
			manager := pressure.logicalBudget()
			r := &Runtime{config: cfg, ledger: l, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: runtimeAccountingState{projection: p, recovery: &atomic.Bool{}}}
			r.draining.Store(true)
			var held resourcebudget.Permit
			switch shape {
			case "missing_manager":
				r.resourcePressure = nil
			case "missing_config":
				r.config.ResourceBudget = nil
			case "exhausted":
				var err error
				held, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes})
				if err != nil {
					t.Fatal(err)
				}
				defer held.Release()
			case "wrong_native_cut":
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := l.Stats().Records
			used := manager.Snapshot().Used
			_, err := r.recoverProjectedAccounting(context.Background(), lease, p, budget.RecoveryStandalone)
			if err == nil || l.Stats().Records != before || manager.Snapshot().Used != used || r.accountingState.recovery.Load() {
				t.Fatal("refused recovery appended facts or leaked a workspace", err, manager.Snapshot())
			}
			if shape == "exhausted" && !errors.Is(err, resourcebudget.ErrExhausted) {
				t.Fatal(err)
			}
		})
	}
}
