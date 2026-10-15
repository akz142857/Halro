package usage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// Three properties the archive and the dashboard rest on: the export selects
// strictly above its own watermark, reconciliation refuses an archive the
// history does not match, and the dashboard spans seven days of hours.

func retentionEvents(t *testing.T, count int, at time.Time) *Aggregate {
	t.Helper()
	aggregate := newTestAggregate(t)
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
				Generation: 1,
				Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return aggregate
}

// Export takes only attempts above the manifest's watermark, so offering the
// same history twice writes nothing the second time.
func TestExportTakesOnlyWhatIsAboveItsWatermark(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 3, day)
	snapshot := snapshotOf(t, aggregate)

	first, err := exporter.Export(aggregate.History())
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

	// Offering the same history again writes nothing: every attempt is at or
	// below the watermark.
	repeat, err := exporter.Export(aggregate.History())
	if err != nil {
		t.Fatal(err)
	}
	if repeat.LastSequence != first.LastSequence || len(repeat.Files) != len(first.Files) {
		t.Fatalf("a second export of the same snapshot changed the manifest: %#v", repeat)
	}

	for _, attempt := range snapshot.Attempts {
		if attempt.Sequence > first.LastSequence {
			t.Fatalf("attempt %d is above the watermark it was exported under", attempt.Sequence)
		}
	}
}

// Reconciliation compares the history against Parquet over the range Parquet
// still holds, and an attempt missing from the history inside that range is a
// refusal, not a difference to report.
func TestReconcileComparesOnlyWhatParquetStillHolds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 4, day)
	snapshot := snapshotOf(t, aggregate)
	if _, err := exporter.Export(aggregate.History()); err != nil {
		t.Fatal(err)
	}
	report, err := exporter.Reconcile(aggregate.History())
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerRecords != report.ParquetRecords || report.ParquetRecords == 0 {
		t.Fatalf("a freshly exported snapshot does not reconcile: %#v", report)
	}

	// The same Parquet against a history that lost attempts: refused, and
	// `halro doctor` and `halro usage verify` refuse with it.
	trimmed := attemptSlice(snapshot.Attempts[len(snapshot.Attempts)-1:])
	if _, err := exporter.Reconcile(trimmed); err == nil {
		t.Fatal("a history missing attempts reconciled against a full Parquet")
	}
}

// The dashboard's hourly series spans exactly seven days.
func TestTheDashboardNeedsSevenDaysOfHistory(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	aggregate := newTestAggregate(t)
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
				Generation: 1,
				Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	dashboard := dashboardOf(t, aggregate, now, Period{ID: "2026-09-08", TimezoneVersion: 1})
	var counted int64
	for _, bucket := range dashboard.Hourly {
		counted += bucket.Requests
	}
	// Three of the four are inside the window; the eight-day-old one is not.
	if counted != 3 {
		t.Fatalf("the dashboard counted %d requests over its seven-day window, want 3", counted)
	}
}
