package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

// The fact that makes a console window possible at all.
//
// The summary page's figures come from the daily rollup, which is durable in
// bbolt and independent of the attempt list the console pages through. If that
// were not true, shortening the console's window would silently shrink the
// month's cost and error counts — and the two numbers would disagree with each
// other rather than with history.
//
// It is asserted here because it was read out of the source rather than pinned
// anywhere, and because it is the assumption the whole retention plan rests on.

func seedRetentionUsage(t *testing.T, runtime *Runtime, requests int, at time.Time) {
	t.Helper()
	var sequence uint64
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
				Sequence: sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Compared by value, not by struct identity: the summary's request count is a
// pointer on the wire — absent means "this dimension has no request identity"
// — so comparing the decoded structs would compare two addresses and pass on
// anything.
type retentionSummary struct {
	Requests      int64
	Attempts      int64
	CostMicrosUSD int64
	InputTokens   int64
}

func readRetentionSummary(t *testing.T, runtime *Runtime, cookie *http.Cookie) retentionSummary {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie,
		"/admin/api/v1/usage/summary?granularity=day&start=2026-09-01&end=2026-09-01")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Totals struct {
			Requests      *int64 `json:"requests"`
			Attempts      int64  `json:"attempts"`
			CostMicrosUSD int64  `json:"cost_micros_usd"`
			InputTokens   int64  `json:"input_tokens"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	summary := retentionSummary{
		Attempts: decoded.Totals.Attempts, CostMicrosUSD: decoded.Totals.CostMicrosUSD,
		InputTokens: decoded.Totals.InputTokens,
	}
	if decoded.Totals.Requests != nil {
		summary.Requests = *decoded.Totals.Requests
	}
	return summary
}

func TestTheSummarySurvivesAnEmptiedAttemptList(t *testing.T) {
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

	const requests = 25
	seedRetentionUsage(t, runtime, requests, time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	// Draining the rollup into bbolt is what the checkpoint tick does; from
	// here the figures are durable and no longer need the aggregate.
	runtime.saveUsageCheckpoint()
	cookie, _ := loginAdminForTest(t, runtime)

	before := readRetentionSummary(t, runtime, cookie)
	if before.Attempts != requests || before.CostMicrosUSD != int64(requests)*100 || before.Requests != requests {
		t.Fatalf("the seeded traffic did not reach the summary: %#v", before)
	}

	// What a console window will do, in its most extreme form: every attempt
	// and every request summary gone, the durable rollup untouched.
	runtime.usage = usage.NewAggregate()
	if page, err := runtime.usage.QueryAttempts(usage.AttemptQuery{Limit: 10}); err != nil {
		t.Fatal(err)
	} else if len(page.Attempts) != 0 {
		t.Fatal("the replacement aggregate is not empty")
	}

	after := readRetentionSummary(t, runtime, cookie)
	if after != before {
		t.Fatalf("the summary changed when the attempt list was emptied:\nbefore %#v\nafter  %#v",
			before, after)
	}
}

// The window, end to end, and the condition that bounds it.
func TestTheConsoleWindowTrimsOnlyWhatWasExported(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.ConsoleWindowDays = 7
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

	// Old enough to be outside any window this test uses.
	seedRetentionUsage(t, runtime, 12, time.Now().UTC().AddDate(0, 0, -60))

	// Nothing exported yet: the watermark is unknown, so nothing is trimmed
	// however old it is. This is the whole safety property.
	runtime.pruneUsageWindow()
	page, err := runtime.usage.QueryAttempts(usage.AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 12 {
		t.Fatalf("%d attempts survived an unexported prune, want all 12", len(page.Attempts))
	}
	if runtime.usage.Floor() != 0 {
		t.Fatalf("the window moved with nothing exported: floor=%d", runtime.usage.Floor())
	}

	// Export, then trim. Now the same records are archived and may go.
	runtime.exportUsageParquet()
	runtime.pruneUsageWindow()
	page, err = runtime.usage.QueryAttempts(usage.AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 0 {
		t.Fatalf("%d attempts survived an exported prune, want none", len(page.Attempts))
	}
	if runtime.usage.Floor() == 0 {
		t.Fatal("records were trimmed without recording where the window now starts")
	}

	// And the archive still reconciles against the windowed aggregate, which
	// is what keeps doctor and `usage verify` green.
	if _, err := runtime.usageExporter.Reconcile(runtime.usage.Snapshot()); err != nil {
		t.Fatalf("a trimmed aggregate no longer reconciles: %v", err)
	}

	// A window shorter than seven days is refused, because the overview reads
	// seven days out of this same aggregate.
	narrow := testConfig(t)
	narrow.Usage.ConsoleWindowDays = 6
	if err := narrow.Validate(config.LoadOptions{}); err == nil {
		t.Fatal("a six-day console window was accepted; the overview reads seven")
	}
	// And one longer than the archive, because the screen would be promising
	// history the archive no longer holds.
	wide := testConfig(t)
	wide.Usage.ConsoleWindowDays = wide.Usage.RetentionDays + 1
	if err := wide.Validate(config.LoadOptions{}); err == nil {
		t.Fatal("a console window longer than the archive was accepted")
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

	// One partition well outside the window, one inside it.
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
	snapshot := runtime.usage.Snapshot()
	if err := runtime.usageExporter.Verify(&snapshot); err != nil {
		t.Fatalf("a compressed partition does not verify against the aggregate: %v", err)
	}
	report, err := runtime.usageExporter.Reconcile(snapshot)
	if err != nil {
		t.Fatalf("a compressed partition does not reconcile: %v", err)
	}
	if report.ParquetRecords != 20 || report.LedgerRecords != 20 {
		t.Fatalf("reconciliation = %#v", report)
	}
}
