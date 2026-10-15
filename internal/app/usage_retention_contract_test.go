package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// Fixtures for the usage archive: traffic written straight into the usage
// history at a chosen time, so retention and partitioning can be exercised
// without waiting for real days to pass.

func seedRetentionUsage(t *testing.T, runtime *Runtime, requests int, at time.Time) {
	t.Helper()
	// The opening build replays the Ledger into the same aggregate; seeding
	// waits for it and continues above where it ended.
	syncUsageHistory(t, runtime)
	sequence := runtime.usage.Watermark().Sequence
	for index := range requests {
		requestID := fmt.Sprintf("req_retention_%04d", index)
		occurred := at.Add(time.Duration(index) * time.Second)
		for _, event := range []ledger.Event{
			{
				EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project_1", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred,
			},
			{
				EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
				RequestID: requestID, AttemptID: requestID + ":1", AttemptNumber: 1,
				ProjectID: "project_1", PeriodID: "2026-09-01", ProviderID: "provider_1",
				DeploymentID: "dep_1", ProviderModel: "gpt-4o", RequestedModel: "chat",
				OccurredAt: occurred, Outcome: "success",
				CommittedMicrosUSD:  ledger.MicrosUSD(100),
				ProviderInputTokens: 10, ProviderOutputTokens: 5,
			},
			{
				EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project_1", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred.Add(time.Second), Outcome: "success",
			},
		} {
			sequence++
			if err := runtime.usage.Apply(ledger.Record{
				Generation: 1,
				Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := runtime.usage.Flush(); err != nil {
		t.Fatal(err)
	}
}

// The archive's own retention, on the maintenance tick rather than in a command
// nobody runs. It used to require stopping the gateway — it takes the data
// directory lock — so the setting shipped since the beginning did nothing on an
// instance that stayed up.
func TestTheArchiveIsPrunedWithoutStoppingTheGateway(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.RetentionDays = 30
	cfg.Usage.ConsoleWindowDays = 30
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	// One partition well outside the archive's retention.
	seedRetentionUsage(t, runtime, 4, time.Now().UTC().AddDate(0, 0, -90))
	runtime.exportUsageParquet()
	before, err := runtime.usageExporter.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Files) == 0 {
		t.Fatal("nothing was exported, so this test proves nothing")
	}

	runtime.pruneUsageArchive()
	after, err := runtime.usageExporter.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Files) != 0 {
		t.Fatalf("%d partitions survived past the retention window", len(after.Files))
	}
	// And the archive is still internally consistent after the sweep, which is
	// what `halro usage verify` and doctor check.
	if err := runtime.usageExporter.Verify(nil); err != nil {
		t.Fatalf("the pruned archive does not verify: %v", err)
	}
}

// Partitions carry an explicit codec now. The assertion is on what a reader
// gets back rather than on the bytes: a partition written with one codec and
// read with another is the failure that matters, and the codec is recorded per
// column chunk inside the file.
func TestExportedPartitionsStillReadBackAfterTheCodecChange(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seedRetentionUsage(t, runtime, 20, time.Now().UTC().Add(-time.Hour))
	runtime.exportUsageParquet()
	if err := runtime.usageExporter.Verify(runtime.usage.History()); err != nil {
		t.Fatalf("a compressed partition does not verify against the history: %v", err)
	}
	report, err := runtime.usageExporter.Reconcile(runtime.usage.History())
	if err != nil {
		t.Fatalf("a compressed partition does not reconcile: %v", err)
	}
	if report.ParquetRecords != 20 || report.LedgerRecords != 20 {
		t.Fatalf("reconciliation = %#v", report)
	}
}
