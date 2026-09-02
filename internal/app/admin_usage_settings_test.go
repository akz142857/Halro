package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

// The console window as a setting rather than a file.
//
// What is being asserted is not that a number round-trips. It is that the one
// destructive setting in the console cannot be shortened by accident, that
// config.yaml stops deciding it once it has been decided, and that the trim
// actually follows the stored value rather than the file it was seeded from.

func readUsageSettings(t *testing.T, runtime *Runtime, cookie *http.Cookie) (usageSettingsResponse, string) {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/settings/usage")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body usageSettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body, response.Header().Get("ETag")
}

func TestShorteningTheConsoleWindowMustBeAcknowledged(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.ConsoleWindowDays = 90
	cfg.Usage.RetentionDays = 90
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
	cookie, csrf := loginAdminForTest(t, runtime)

	settings, etag := readUsageSettings(t, runtime, cookie)
	if settings.ConsoleWindowDays != 90 || !settings.ConfigFileInEffect || settings.MaxDays != 90 {
		t.Fatalf("seeded settings = %#v", settings)
	}

	// Shorter, unacknowledged: refused with a code the console can turn into a
	// confirmation rather than a generic failure.
	refused := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 30})
	if refused.Code != http.StatusBadRequest ||
		!jsonBodyContains(t, refused, `"console_window_trim_unacknowledged"`) {
		t.Fatalf("unacknowledged shortening status=%d body=%s", refused.Code, refused.Body.String())
	}
	if current, _ := readUsageSettings(t, runtime, cookie); current.ConsoleWindowDays != 90 {
		t.Fatalf("a refused change moved the window to %d", current.ConsoleWindowDays)
	}

	// Longer needs no acknowledgement, because nothing is discarded by it.
	// Refused here only because it would outrun the archive, which is the other
	// bound and a different answer.
	tooLong := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 180})
	if tooLong.Code != http.StatusBadRequest ||
		!jsonBodyContains(t, tooLong, `"console_window_exceeds_retention"`) {
		t.Fatalf("over-retention window status=%d body=%s", tooLong.Code, tooLong.Body.String())
	}

	// Below the floor the overview reads.
	tooShort := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 6, "acknowledge_trim": true})
	if tooShort.Code != http.StatusBadRequest || !jsonBodyContains(t, tooShort, `"invalid_console_window"`) {
		t.Fatalf("six-day window status=%d body=%s", tooShort.Code, tooShort.Body.String())
	}

	accepted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag,
		map[string]any{"console_window_days": 30, "acknowledge_trim": true})
	if accepted.Code != http.StatusOK || accepted.Header().Get("ETag") != `"2"` {
		t.Fatalf("acknowledged shortening status=%d etag=%q body=%s",
			accepted.Code, accepted.Header().Get("ETag"), accepted.Body.String())
	}
	// A second write at the old revision is a stale editor, not a repeat.
	stale := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag,
		map[string]any{"console_window_days": 60, "acknowledge_trim": true})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale write status=%d body=%s", stale.Code, stale.Body.String())
	}

	after, _ := readUsageSettings(t, runtime, cookie)
	if after.ConsoleWindowDays != 30 || after.ConfigFileInEffect || after.ConfigFileDays != 90 {
		t.Fatalf("after the change = %#v", after)
	}
	// And the change is on the audit trail, because it destroyed history.
	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit?limit=20")
	if audit.Code != http.StatusOK || !jsonBodyContains(t, audit, `"settings.usage.update"`) {
		t.Fatalf("audit status=%d body=%s", audit.Code, audit.Body.String())
	}
}

// The stored setting is what the trim uses, and config.yaml is only a seed.
//
// Reading the config at trim time would make an operator's deliberate,
// audited change revert on the next restart — silently, since nothing about a
// restart looks like a settings change.
func TestTheTrimFollowsTheStoredWindowRatherThanTheFile(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.ConsoleWindowDays = 90
	cfg.Usage.RetentionDays = 90
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

	// Sixty days old: inside the file's ninety-day window, outside the
	// thirty-day one about to be stored.
	seedRetentionUsage(t, runtime, 8, time.Now().UTC().AddDate(0, 0, -60))
	runtime.exportUsageParquet()
	runtime.pruneUsageWindow()
	page, err := runtime.usage.QueryAttempts(usage.AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 8 {
		t.Fatalf("%d attempts survived a trim inside the window, want all 8", len(page.Attempts))
	}

	cookie, csrf := loginAdminForTest(t, runtime)
	_, etag := readUsageSettings(t, runtime, cookie)
	accepted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag,
		map[string]any{"console_window_days": 30, "acknowledge_trim": true})
	if accepted.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	runtime.pruneUsageWindow()
	page, err = runtime.usage.QueryAttempts(usage.AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 0 {
		t.Fatalf("%d attempts survived the shortened window, want none", len(page.Attempts))
	}

	// A restart re-reads config.yaml, which still says ninety. The stored
	// setting is what must survive.
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if days := reopened.consoleWindowDays(); days != 30 {
		t.Fatalf("the window reverted to %d days after a restart", days)
	}
}
