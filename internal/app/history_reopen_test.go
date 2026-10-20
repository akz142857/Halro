package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
)

func settleOneRequest(t *testing.T, runtime *Runtime, requestID string) {
	t.Helper()
	ctx := context.Background()
	request, err := runtime.accounting.BeginRequestDetailed(ctx, "project_1", "key_1", requestID, "chat")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveAttemptDetailed(ctx, request, 1_000, 100, budget.AttemptMetadata{
		RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(ctx, attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3, Outcome: "success", LatencyMillis: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
}

// The hourly reopen of history.db and archive.db keeps what they hold, and a
// reopen that fails leaves the file closed — the history stops taking records
// rather than losing them — until the minute tick's retry succeeds and the
// gap is replayed from the Ledger.
func TestHistoryFilesReopenAndRecoverFromAFailedReopen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	settleOneRequest(t, runtime, "request_1")
	before := dumpUsage(t, runtime)
	runtime.reopenHistoryFiles(false)
	if failed := runtime.accountingState.reopenFailed; failed.history || failed.archive {
		t.Fatalf("reopen failed: %+v", failed)
	}
	if got := dumpUsage(t, runtime); len(got.Attempts) != len(before.Attempts) || got.Totals != before.Totals {
		t.Fatalf("history after reopen = %+v, want %+v", got, before)
	}

	for _, path := range []string{cfg.HistoryPath(), cfg.ArchivePath()} {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
	runtime.reopenHistoryFiles(false)
	if failed := runtime.accountingState.reopenFailed; !failed.history || !failed.archive {
		t.Fatalf("reopen of unreadable files reported %+v, want both failed", failed)
	}
	if _, err := runtime.usage.History().State(); err == nil {
		t.Fatal("history read after a failed reopen succeeded; want an error")
	}
	settleOneRequest(t, runtime, "request_2")
	if err := runtime.usageCollector.CatchUp(context.Background()); err == nil {
		t.Fatal("catch-up into a closed history succeeded; want an error")
	}

	for _, path := range []string{cfg.HistoryPath(), cfg.ArchivePath()} {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtime.reopenHistoryFiles(true)
	if failed := runtime.accountingState.reopenFailed; failed.history || failed.archive {
		t.Fatalf("retry left %+v, want both reopened", failed)
	}
	after := dumpUsage(t, runtime)
	if len(after.Attempts) != len(before.Attempts)+1 || len(after.Requests) != len(before.Requests)+1 {
		t.Fatalf("history after recovery holds %d attempts and %d requests, want %d and %d",
			len(after.Attempts), len(after.Requests), len(before.Attempts)+1, len(before.Requests)+1)
	}
}
