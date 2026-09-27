package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerDurableHookRunsInsideAppendBeforeSuccess(t *testing.T) {
	status := NewStatus()
	var captured DurableBatch
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), status, Options{
		MaxBatch: 1, ChainKey: testChainKey,
		AfterDurable: func(batch DurableBatch) (uint64, error) {
			captured = batch
			return 0, errors.New("ordering disk full")
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append(context.Background(), validReservation("evt_hook", "attempt_hook")); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.FirstSequence != 1 || captured.LastSequence != 1 || captured.Generation != 1 || !captured.RequiresConfirmation || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if status.Load() != AccountingUnavailable {
		t.Fatalf("status=%v, want unavailable", status.Load())
	}
}

func TestLedgerReceiptWaitsOnlyForRequiredEvents(t *testing.T) {
	var waited []uint64
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{
		MaxBatch: 1, ChainKey: testChainKey,
		AfterDurable: func(batch DurableBatch) (uint64, error) {
			return 40 + batch.LastSequence, nil
		},
		WaitConfirmed: func(_ context.Context, index uint64) error {
			waited = append(waited, index)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	required, err := log.AppendWithReceipt(context.Background(), validReservation("evt_required", "attempt_required"))
	if err != nil {
		t.Fatal(err)
	}
	if !required.RequiresConfirmation || required.ReplicationIndex != 41 {
		t.Fatalf("required receipt=%#v", required)
	}
	if err := log.WaitConfirmed(context.Background(), required); err != nil {
		t.Fatal(err)
	}
	ordinaryEvent := governanceEvent("evt_ordinary", EventRequestFinalized, time.Now().UTC())
	ordinaryEvent.RequestID = "req_ordinary"
	ordinaryEvent.Outcome = "succeeded"
	ordinary, err := log.AppendWithReceipt(context.Background(), ordinaryEvent)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.RequiresConfirmation || ordinary.ReplicationIndex != 42 {
		t.Fatalf("ordinary receipt=%#v", ordinary)
	}
	if err := log.WaitConfirmed(context.Background(), ordinary); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 41 {
		t.Fatalf("waited=%v", waited)
	}
}
