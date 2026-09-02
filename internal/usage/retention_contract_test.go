package usage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// The facts a retention window is built on.
//
// Trimming the aggregate is only safe because of three properties of the code
// around it, and every one of them was read out of the source rather than
// asserted anywhere: the export selects strictly above its own watermark, the
// reconciliation compares only what Parquet still holds, and the dashboard
// needs seven days. A window built on an unpinned reading is a window that
// silently starts losing history the day one of them changes.
//
// These tests exist before the window does, for the same reason the failure
// taxonomy was pinned before anything was built on it.

func retentionEvents(t *testing.T, count int, at time.Time) *Aggregate {
	t.Helper()
	aggregate := NewAggregate()
	var sequence uint64
	for index := range count {
		requestID := fmt.Sprintf("req_%06d", index)
		occurred := at.Add(time.Duration(index) * time.Second)
		for _, event := range []ledger.Event{
			{
				EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred,
			},
			{
				EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
				RequestID: requestID, AttemptID: requestID + ":1", AttemptNumber: 1,
				ProjectID: "project", PeriodID: "2026-09-01", ProviderID: "provider_1",
				DeploymentID: "dep_1", ProviderModel: "gpt-4o", RequestedModel: "chat",
				OccurredAt: occurred, Outcome: "success",
				CommittedMicrosUSD: ledger.MicrosUSD(1), ProviderInputTokens: 10,
			},
			{
				EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred.Add(time.Second), Outcome: "success",
			},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{
				Sequence: sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return aggregate
}

// The bound on trimming. Export takes only attempts above the manifest's
// watermark, so anything trimmed below it is already archived and anything
// trimmed above it is gone for good. The window has to stop at this line, and
// this is the line.
func TestExportTakesOnlyWhatIsAboveItsWatermark(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 3, day)
	snapshot := aggregate.Snapshot()

	first, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.LastSequence == 0 {
		t.Fatalf("nothing was exported: %#v", first)
	}
	var exported int64
	for _, entry := range first.Files {
		exported += entry.Records
	}
	if exported != int64(len(snapshot.Attempts)) {
		t.Fatalf("exported %d of %d attempts", exported, len(snapshot.Attempts))
	}

	// Offering the same snapshot again writes nothing: every attempt is at or
	// below the watermark. This is what makes trimming below it safe.
	repeat, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.LastSequence != first.LastSequence || len(repeat.Files) != len(first.Files) {
		t.Fatalf("a second export of the same snapshot changed the manifest: %#v", repeat)
	}

	// And an attempt above the watermark is still pending. Trimming one of
	// these would remove it from the only place it had left to be archived
	// from — which is why the window is bounded by LastSequence and not by
	// time alone.
	for _, attempt := range snapshot.Attempts {
		if attempt.Sequence > first.LastSequence {
			t.Fatalf("attempt %d is above the watermark it was exported under", attempt.Sequence)
		}
	}
}

// Reconciliation compares the aggregate against Parquet only inside the range
// Parquet still holds. A shorter aggregate window therefore does not, by
// itself, make the two disagree about records outside it — but it does make
// them disagree inside it, which is why the aggregate's own floor has to be
// taught to Reconcile before any trimming ships.
func TestReconcileComparesOnlyWhatParquetStillHolds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 4, day)
	snapshot := aggregate.Snapshot()
	if _, err := exporter.Export(snapshot); err != nil {
		t.Fatal(err)
	}
	report, err := exporter.Reconcile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerRecords != report.ParquetRecords || report.ParquetRecords == 0 {
		t.Fatalf("a freshly exported snapshot does not reconcile: %#v", report)
	}

	// The shape a trimmed aggregate will present: the same Parquet, fewer
	// attempts in hand. Reconcile does not report this as a difference — it
	// refuses outright, and `halro doctor` and `halro usage verify` refuse with
	// it. So trimming cannot ship before Reconcile is taught the aggregate's
	// own floor; this is not a cosmetic mismatch to tidy up afterwards.
	trimmed := snapshot
	trimmed.Attempts = snapshot.Attempts[len(snapshot.Attempts)-1:]
	if _, err := exporter.Reconcile(trimmed); err == nil {
		t.Fatal("a trimmed aggregate reconciled against a full Parquet; the ordering constraint this plan rests on is gone")
	}
}

// Seven days is the floor for any console window, because the dashboard reads
// hourly buckets and request summaries over exactly that span. A window below
// it would leave the overview with holes rather than with a shorter history.
func TestTheDashboardNeedsSevenDaysOfHistory(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	aggregate := NewAggregate()
	var sequence uint64
	for _, age := range []time.Duration{1 * time.Hour, 3 * 24 * time.Hour, 6 * 24 * time.Hour, 8 * 24 * time.Hour} {
		at := now.Add(-age)
		requestID := fmt.Sprintf("req_%d", int(age.Hours()))
		for _, event := range []ledger.Event{
			{
				EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-08",
				RequestedModel: "chat", OccurredAt: at,
			},
			{
				EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-08",
				RequestedModel: "chat", OccurredAt: at, Outcome: "success",
			},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{
				Sequence: sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	dashboard := aggregate.Dashboard(now, Period{ID: "2026-09-08", TimezoneVersion: 1})
	var counted int64
	for _, bucket := range dashboard.Hourly {
		counted += bucket.Requests
	}
	// Three of the four are inside the window; the eight-day-old one is not.
	if counted != 3 {
		t.Fatalf("the dashboard counted %d requests over its seven-day window, want 3", counted)
	}
}

// The baseline this plan is measured against, and a ceiling on the record's
// size while we are here. The checkpoint carries every attempt, so its cost is
// this number times the window — a change that quietly doubles the record
// doubles the window's cost with it.
func TestTheCheckpointCostsAKnownAmountPerAttempt(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	const attempts = 500
	aggregate := retentionEvents(t, attempts, day)
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	perAttempt := len(snapshot.Payload) / attempts
	t.Logf("checkpoint: %d bytes for %d attempts (%d bytes each)",
		len(snapshot.Payload), attempts, perAttempt)
	// Wide enough that ordinary field changes do not trip it, narrow enough
	// that a structural bloat does. The measured figure for a settled attempt
	// with a price snapshot is about 1150 bytes; these fixtures carry no
	// snapshot, so they sit lower.
	if perAttempt < 200 || perAttempt > 2000 {
		t.Fatalf("a checkpointed attempt is %d bytes; the window's cost is this times its length", perAttempt)
	}
	// And it is linear in the number of attempts, which is the whole reason a
	// window bounds anything.
	half := retentionEvents(t, attempts/2, day)
	halfSnapshot, err := half.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(len(snapshot.Payload)) / float64(len(halfSnapshot.Payload))
	if ratio < 1.7 || ratio > 2.3 {
		t.Fatalf("checkpoint size is not linear in attempt count: %.2fx for twice the attempts", ratio)
	}
	var decoded map[string]any
	if err := json.Unmarshal(snapshot.Payload, &decoded); err != nil {
		t.Fatalf("the checkpoint payload is not the JSON this measurement assumes: %v", err)
	}
}
