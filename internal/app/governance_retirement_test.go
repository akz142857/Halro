package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

func TestBusinessDrainJoinsGovernanceBeforeAccountingMaintenance(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	oldGov, oldAccounting := r.governance.manager, r.accounting
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	result := make(chan error, 1)
	go func() {
		result <- oldGov.WithOutcomeExport(context.Background(), func(view governance.OutcomeExportView) error {
			page, err := view.ReadPage(context.Background(), 1, governance.OutcomePageMaxBytes)
			if err != nil || !page.Done {
				return errors.Join(err, errors.New("empty source not completed"))
			}
			if err := view.Complete(context.Background()); err != nil {
				return err
			}
			// External publication still owns the Manager after source Complete.
			close(entered)
			<-release
			_, err = r.store.GetProject(context.Background(), bootstrap.ProjectID)
			if err != nil {
				return err
			}
			_, _, err = oldAccounting.ReadWorkUnit(context.Background(), bootstrap.ProjectID, "absent")
			return err
		})
	}()
	awaitDrainSignal(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type maintenanceResult struct {
		lease *ledger.MaintenanceLease
		err   error
	}
	maintenance := make(chan maintenanceResult, 1)
	go func() {
		lease, err := r.beginAccountingMaintenance(ctx, nil)
		maintenance <- maintenanceResult{lease, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready, err := oldGov.Ready()
		if !ready && errors.Is(err, governance.ErrRetired) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain never reached Governance retirement", ready, err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	got := <-maintenance
	if got.lease != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatal("maintenance bypassed Governance publication", got.lease, got.err)
	}
	if ready, err := oldGov.Ready(); ready || !errors.Is(err, governance.ErrRetired) {
		t.Fatal("old Governance reopened", ready, err)
	}
	if _, _, err := oldAccounting.ReadWorkUnit(context.Background(), bootstrap.ProjectID, "absent"); err != nil {
		t.Fatal("dependent accounting retired early", err)
	}
	once.Do(func() { close(release) })
	if err := <-result; err != nil {
		t.Fatal("accepted publication lost dependencies", err)
	}
	lease, err := r.beginAccountingMaintenance(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, _, err := oldAccounting.ReadWorkUnit(context.Background(), bootstrap.ProjectID, "absent"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal("old accounting still admitted", err)
	}
	if oldGov.State() != nil {
		t.Fatal("retired Governance retains State")
	}
}

func TestRuntimeCloseRetiresAccountingAndGovernanceAfterFailedOrDirectDrain(t *testing.T) {
	for _, failedDrain := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "failed-drain"}[failedDrain], func(t *testing.T) {
			r, _, _ := openBootstrappedRuntime(t)
			defer r.Close()
			oldGov, oldAccounting := r.governance.manager, r.accounting
			if failedDrain {
				failure := errors.New("controlled HTTP failure")
				if err := r.drainAccountingBusiness(context.Background(), []shutdownHTTPServer{&fakeShutdownServer{shutdownError: failure}}); !errors.Is(err, failure) {
					t.Fatal(err)
				}
				if _, err := oldAccounting.ReadBalance(context.Background(), ledger.BalanceKey{}); err != nil {
					t.Fatal("failed drain prematurely retired accounting", err)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := oldAccounting.ReadBalance(context.Background(), ledger.BalanceKey{}); !errors.Is(err, budget.ErrAccountingRetired) {
				t.Fatal("Close left accounting read admission", err)
			}
			if _, _, err := oldAccounting.ReadWorkUnit(context.Background(), "", "absent"); !errors.Is(err, budget.ErrAccountingRetired) {
				t.Fatal("Close left State observation", err)
			}
			if ready, err := oldGov.Ready(); ready || !errors.Is(err, governance.ErrRetired) {
				t.Fatal("Close left Governance admission", ready, err)
			}
			if r.accountingState.reader != nil || oldGov.State() != nil {
				t.Fatal("closed Runtime retains historical State alias")
			}
		})
	}
}

func TestRuntimeCloseWithdrawsThenWakesUnappliedAccountingPredecessor(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	oldAccounting := r.accounting
	native := r.ledger
	if _, err := native.Append(context.Background(), ledger.Event{Kind: ledger.EventRequestAccepted, EventID: "evt_gap", RequestID: "req_gap", ProjectID: bootstrap.ProjectID, PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := oldAccounting.BeginRequest(ctx, bootstrap.ProjectID, "req_waiting"); result <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for native.Stats().Records < 2 {
		if time.Now().After(deadline) {
			t.Fatal("accepted operation never reached predecessor wait")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("terminal retirement waited before native Close could wake predecessor")
	}
	if err := <-result; err == nil {
		t.Fatal("missing predecessor credited success")
	}
	if _, err := oldAccounting.ReadBalance(context.Background(), ledger.BalanceKey{}); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal("terminal native close left accounting admission", err)
	}
	if native.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("missing Apply gap did not require recovery")
	}
}

func TestRuntimeTerminalAccountingCloseWakesAppliedUnconfirmedACK(t *testing.T) {
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	defer coordinator.Stop()
	if _, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	log, _, _ := projectedWriterLog(t, nil, ledger.Options{
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			commit, err := coordinator.RecordLedgerBatch(batch)
			return commit.Index, err
		},
		WaitConfirmed: func(ctx context.Context, index uint64) error {
			close(entered)
			return coordinator.WaitConfirmed(ctx, index)
		},
	})
	state := ledger.NewState()
	periods, err := budget.NewFixedPeriodResolver("UTC")
	if err != nil {
		t.Fatal(err)
	}
	m, err := budget.New(log, state, periods)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{ledger: log, accountingState: runtimeAccountingState{reader: state}, accounting: m, replication: &replicationRuntime{coordinator: coordinator}}
	request, err := m.BeginRequest(context.Background(), "prj_ack", "req_ack")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := m.ReserveAttempt(context.Background(), request, 100, 7); result <- err }()
	awaitDrainSignal(t, entered)
	if pending, _ := state.PendingLeaseStats(time.Now()); pending != 1 || state.Watermark().Sequence != 2 {
		t.Fatal("ACK waiter did not retain applied native fact", pending, state.Watermark())
	}
	if _, available, err := coordinator.Status(); !available || err != nil {
		t.Fatal("fixture ACK source already stopped", available, err)
	}
	m.WithdrawAdmission()
	closed := make(chan error, 1)
	go func() { closed <- r.closeAccountingOwner() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		coordinator.Stop()
		t.Fatal("terminal join could not reach confirmation Stop")
	}
	if err := <-result; !errors.Is(err, replication.ErrReplicationUnavailable) {
		t.Fatal("unconfirmed accepted call credited success", err)
	}
	if _, err := m.ReadBalance(context.Background(), ledger.BalanceKey{}); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal("old accounting admission remained", err)
	}
	if state.Watermark().Sequence != 2 || log.Stats().Records != 2 || r.accountingState.reader != nil {
		t.Fatal("terminal join lost durable/applied fact or retained Runtime alias")
	}
}
