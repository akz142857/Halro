package app

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

func TestDeletingUsageCheckpointRebuildsIdenticalAggregateFromLedger(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_1", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveAttemptDetailed(
		context.Background(), request, 1_000, 100,
		budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1",
			AttemptNumber: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(context.Background(), attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3,
		Outcome: "success", LatencyMillis: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	runtime.saveUsageCheckpoint()
	expected := runtime.usage.Snapshot()
	if expected.Watermark.Sequence == 0 {
		t.Fatal("checkpoint did not advance")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUsageCheckpoint(); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	actual := reopened.usage.Snapshot()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("rebuilt aggregate differs:\nactual=%#v\nexpected=%#v", actual, expected)
	}
}

func TestCheckpointWatermarkRejectsAlreadyAggregatedLedgerPrefix(t *testing.T) {
	aggregate := usage.NewAggregate()
	records := []ledger.Record{
		checkpointRecord(1, 100, ledger.EventRequestAccepted),
		checkpointRecord(2, 200, ledger.EventRequestFinalized),
	}
	for _, record := range records {
		if err := aggregate.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
	watermark, payload, err := aggregate.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := usage.RestoreCheckpoint(payload)
	if err != nil {
		t.Fatal(err)
	}
	if watermark != (ledger.Watermark{
		Generation: 1, Offset: records[1].Offset, Sequence: records[1].Sequence,
	}) {
		t.Fatalf("watermark=%#v", watermark)
	}
	// Skipped rather than refused: the checkpoint now carries the dedup window,
	// so a record already folded in is recognised by its event ID and ignored,
	// which is how Apply has always treated a duplicate. The invariant worth
	// asserting is that it changed nothing, not which of the two guards caught
	// it — the monotonic-sequence check still refuses anything older than the
	// window.
	if err := restored.Apply(records[0]); err != nil {
		t.Fatalf("re-applying a checkpointed record: %v", err)
	}
	if !reflect.DeepEqual(restored.Snapshot(), aggregate.Snapshot()) {
		t.Fatal("replaying a checkpointed prefix changed the aggregate")
	}
}

func checkpointRecord(sequence uint64, offset int64, kind ledger.EventKind) ledger.Record {
	event := ledger.Event{
		EventID: "evt_" + string(rune('0'+sequence)), Kind: kind,
		RequestID: "request_1", ProjectID: "project_1", PeriodID: "2026-07-31",
		OccurredAt: time.Date(2026, 7, 31, 12, 0, int(sequence), 0, time.UTC),
	}
	if kind == ledger.EventRequestFinalized {
		event.Outcome = "success"
	}
	return ledger.Record{Sequence: sequence, Offset: offset, Event: event}
}
