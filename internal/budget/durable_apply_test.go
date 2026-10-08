package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestManagerDurableRecordsMatchWALAndFeedFreshProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey, MaxBatch: 8, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err)
	}
	p, err := ledger.BuildLedgerProjection(context.Background(), path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	state := ledger.NewState()
	manager, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	// This observer is a verification mirror, not production selection of the
	// disk backend or an error-returning projection publication mechanism.
	observed := make(map[uint64]ledger.Record)
	manager.AddObserver(func(r ledger.Record) {
		if r.Epoch == 0 || r.Hash == ([32]byte{}) {
			t.Error("observer received synthesized record")
		}
		if err := p.Apply(context.Background(), r); err != nil {
			t.Error(err)
		}
		observed[r.Sequence] = r
	})
	for generation := 1; generation <= 2; generation++ {
		var wg sync.WaitGroup
		errs := make(chan error, 24)
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				project, requestID := fmt.Sprint("prj_", i), fmt.Sprintf("req_%d_%d", generation, i)
				request, err := manager.BeginRequest(context.Background(), project, requestID)
				if err != nil {
					errs <- err
					return
				}
				attempt, err := manager.ReserveAttempt(context.Background(), request, 1000, 7)
				if err != nil {
					errs <- err
					return
				}
				if err := manager.MarkStarted(context.Background(), attempt); err != nil {
					errs <- err
					return
				}
				if err := manager.Settle(context.Background(), attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
					errs <- err
					return
				}
				if err := manager.Finalize(context.Background(), request, "success"); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if generation == 1 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	actual := make(map[uint64]ledger.Record)
	report, partial, err = ledger.InspectReplayAuthenticated(path, testChainKey, func(r ledger.Record) error { actual[r.Sequence] = r; return nil })
	if err != nil || partial || len(actual) != 240 || !reflect.DeepEqual(actual, observed) {
		t.Fatal("normal accounting evidence differs from authenticated WAL", len(actual), len(observed), partial, err)
	}
	head, hash, err := p.ReadPosition(context.Background())
	if err != nil || head != report.Head || hash != report.ChainHash {
		t.Fatal("mirror did not reach native cut", head, report.Head, err)
	}
}

func TestManagerFreezesEventBeforeOrderingAndAppliesAfterAcceptedCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	state := ledger.NewState()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey, MaxBatch: 1,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			if batch.FirstSequence == 2 {
				close(entered)
				<-release
			}
			return batch.LastSequence, nil
		},
		WaitConfirmed: func(ctx context.Context, _ uint64) error { return ctx.Err() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { once.Do(func() { close(release) }); log.Close() }()
	manager, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequest(context.Background(), "prj_frozen", "req_frozen")
	if err != nil {
		t.Fatal(err)
	}
	value := int64(7)
	event := ledger.Event{EventID: "evt_frozen", Kind: ledger.EventReservationCreated, RequestID: request.RequestID, ProjectID: request.ProjectID, AttemptID: "att_frozen", ReservationMicrosUSD: &value, OccurredAt: time.Now().UTC()}
	request.Period.Stamp(&event)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := manager.appendApplyRecord(ctx, event); done <- err }()
	<-entered
	// Marshal and record snapshot completed before the blocked ordering hook.
	value = 99
	cancel()
	select {
	case err := <-done:
		t.Fatal("accepted event returned before durable ordering", err)
	case <-time.After(10 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("required ACK did not retain cancellation", err)
	}
	lease, found := state.AccountingLease("att_frozen")
	if !found || *lease.Event.ReservationMicrosUSD != 7 || lease.Epoch == 0 || lease.Hash == ([32]byte{}) {
		t.Fatal("State applied mutable or fabricated event", found, lease)
	}
	var actual ledger.Record
	if _, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, func(r ledger.Record) error {
		if r.Sequence == 2 {
			actual = r
		}
		return nil
	}); err != nil || partial || !reflect.DeepEqual(lease, actual) {
		t.Fatal("lease differs from WAL", partial, err)
	}
	if log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("canceled confirmation condemned applied history")
	}
}

func TestManagerMaintenanceAndCloseJoinDurableObserver(t *testing.T) {
	for _, mode := range []string{"maintenance", "close"} {
		t.Run(mode, func(t *testing.T) {
			manager, state, closeLog := newTestManager(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer func() { once.Do(func() { close(release) }); closeLog() }()
			manager.AddObserver(func(ledger.Record) { close(entered); <-release })
			done := make(chan error, 1)
			go func() { _, err := manager.BeginRequest(context.Background(), "prj_join", "req_join"); done <- err }()
			<-entered
			if state.Watermark().Sequence != 1 {
				t.Fatal("observer ran before Apply")
			}
			if mode == "maintenance" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				lease, err := manager.log.BeginMaintenance(ctx)
				cancel()
				if lease != nil {
					lease.Close()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("maintenance did not wait for normal observer", err)
				}
				once.Do(func() { close(release) })
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				lease, err = manager.log.BeginMaintenance(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				lease.Close()
			} else {
				closed := make(chan error, 1)
				go func() { closed <- manager.log.Close() }()
				select {
				case err := <-closed:
					t.Fatal("Close returned before synchronous observer", err)
				case <-time.After(30 * time.Millisecond):
				}
				once.Do(func() { close(release) })
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not join released observer")
				}
			}
		})
	}
}

func TestManagerCloseInterruptsAnUnappliedPredecessorWait(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	// A native fact written by another owner was never applied to this manager.
	// Its next callback must not outlive native Close waiting for that missing row.
	now := time.Now().UTC()
	if _, err := manager.log.Append(context.Background(), ledger.Event{Kind: ledger.EventRequestAccepted, EventID: "evt_unapplied", RequestID: "req_unapplied", ProjectID: "prj_gap", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := manager.BeginRequest(context.Background(), "prj_gap", "req_waiting"); done <- err }()
	deadline := time.Now().Add(time.Second)
	for manager.log.Stats().Records < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second fact not durable")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.log.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close stranded durable predecessor wait")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join failed callback")
	}
	if state.Watermark().Sequence != 0 || manager.log.Stats().Records != 2 || manager.log.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("gap was skipped or durable facts lost", state.Watermark(), manager.log.Stats(), manager.log.Status().Load())
	}
}
