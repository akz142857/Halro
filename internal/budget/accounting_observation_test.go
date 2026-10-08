package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type blockedObservationReader struct {
	ledger.AccountingAdmissionReader
	entered, release chan struct{}
	err              error
}

func (r *blockedObservationReader) wait() {
	if r.entered != nil {
		close(r.entered)
		<-r.release
	}
}
func (r *blockedObservationReader) ReadBalance(ctx context.Context, key ledger.BalanceKey) (ledger.Balance, error) {
	r.wait()
	v, err := r.AccountingAdmissionReader.ReadBalance(ctx, key)
	return v, errors.Join(err, r.err)
}
func (r *blockedObservationReader) ReadPendingLeaseStats(ctx context.Context, now time.Time) (ledger.PendingLeaseStatistics, error) {
	r.wait()
	v, err := r.AccountingAdmissionReader.ReadPendingLeaseStats(ctx, now)
	return v, errors.Join(err, r.err)
}

func TestAccountingObservationRetireJoinsActualReadsAndSanitizesFailure(t *testing.T) {
	for _, name := range []string{"balance", "pending"} {
		t.Run(name, func(t *testing.T) {
			m, state, closeLog := newTestManager(t)
			defer closeLog()
			request, err := m.BeginRequest(context.Background(), "prj_1", "req_observation")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.ReserveAttempt(context.Background(), request, 100, 7); err != nil {
				t.Fatal(err)
			}
			r := &blockedObservationReader{AccountingAdmissionReader: state, entered: make(chan struct{}), release: make(chan struct{})}
			var unblock sync.Once
			defer unblock.Do(func() { close(r.release) })
			m.reads = r
			key := ledger.BalanceKey{ProjectID: request.ProjectID, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
			read := func() error {
				if name == "balance" {
					v, err := m.ReadBalance(context.Background(), key)
					if err == nil && v.ReservedMicrosUSD != 7 {
						return errors.New("live balance lost")
					}
					if err != nil && v != (ledger.Balance{}) {
						return errors.New("failed balance retained value")
					}
					return err
				}
				v, err := m.ReadPendingLeaseStats(context.Background(), time.Now())
				if err == nil && v.Count != 1 {
					return errors.New("live pending count lost")
				}
				if err != nil && v != (ledger.PendingLeaseStatistics{}) {
					return errors.New("failed pending read retained value")
				}
				return err
			}
			done := make(chan error, 1)
			go func() { done <- read() }()
			<-r.entered
			retireWithDeadline(t, m)
			if err := read(); !errors.Is(err, ErrAccountingRetired) {
				t.Fatal(err)
			}
			m.operationMu.Lock()
			if m.owner == nil || m.activeOperations != 1 {
				t.Error("in-flight observation lost its owner")
			}
			m.operationMu.Unlock()
			unblock.Do(func() { close(r.release) })
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertRetiredOwnerReleased(t, m)
			// A new independent fixture proves a useful value+error cannot
			// escape the public Manager API as a financial observation.
			fresh, freshState, closeFresh := newTestManager(t)
			defer closeFresh()
			freshRequest, err := fresh.BeginRequest(context.Background(), "prj_1", "req_failure")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.ReserveAttempt(context.Background(), freshRequest, 100, 7); err != nil {
				t.Fatal(err)
			}
			key = ledger.BalanceKey{ProjectID: freshRequest.ProjectID, PeriodID: freshRequest.Period.ID, TimezoneVersion: freshRequest.Period.TimezoneVersion}
			injected := errors.New("observation read failure")
			fresh.reads = &blockedObservationReader{AccountingAdmissionReader: freshState, err: injected}
			if name == "balance" {
				v, err := fresh.ReadBalance(context.Background(), key)
				if !errors.Is(err, injected) || v != (ledger.Balance{}) {
					t.Fatal(v, err)
				}
			} else {
				v, err := fresh.ReadPendingLeaseStats(context.Background(), time.Now())
				if !errors.Is(err, injected) || v != (ledger.PendingLeaseStatistics{}) {
					t.Fatal(v, err)
				}
			}
		})
	}
}
