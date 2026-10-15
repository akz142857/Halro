package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// The console window as a setting rather than a file.
//
// What is being asserted is not that a number round-trips. It is that
// config.yaml stops deciding it once it has been decided, that the lists'
// default range follows the stored value rather than the file it was seeded
// from, and that changing it discards nothing.

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

func TestTheConsoleWindowIsAStoredSetting(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.ConsoleWindowDays = 90
	cfg.Usage.RetentionDays = 30
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

	// The archive's retention does not bound it: the lists read the history
	// index, not the archive.
	settings, etag := readUsageSettings(t, runtime, cookie)
	if settings.ConsoleWindowDays != 90 || !settings.ConfigFileInEffect {
		t.Fatalf("seeded settings = %#v", settings)
	}

	// Below the floor the overview reads.
	tooShort := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 6})
	if tooShort.Code != http.StatusBadRequest || !jsonBodyContains(t, tooShort, `"invalid_console_window"`) {
		t.Fatalf("six-day window status=%d body=%s", tooShort.Code, tooShort.Body.String())
	}

	// Shorter needs no acknowledgement: it discards nothing.
	accepted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 30})
	if accepted.Code != http.StatusOK || accepted.Header().Get("ETag") != `"2"` {
		t.Fatalf("shortening status=%d etag=%q body=%s",
			accepted.Code, accepted.Header().Get("ETag"), accepted.Body.String())
	}
	// A second write at the old revision is a stale editor, not a repeat.
	stale := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 60})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale write status=%d body=%s", stale.Code, stale.Body.String())
	}

	after, _ := readUsageSettings(t, runtime, cookie)
	if after.ConsoleWindowDays != 30 || after.ConfigFileInEffect || after.ConfigFileDays != 90 {
		t.Fatalf("after the change = %#v", after)
	}
	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit?limit=20")
	if audit.Code != http.StatusOK || !jsonBodyContains(t, audit, `"settings.usage.update"`) {
		t.Fatalf("audit status=%d body=%s", audit.Code, audit.Body.String())
	}
}

// listedAttempts reads the attempt list the console reads, with the query it
// is given.
func listedAttempts(t *testing.T, runtime *Runtime, cookie *http.Cookie, query string) int {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/usage?limit=100"+query)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return len(body.Items)
}

// The stored setting is what the default range uses, and config.yaml is only
// a seed. Reading the config instead would make an operator's deliberate,
// audited change revert on the next restart. And the range is only a default:
// what falls outside it is still there for a list that asks.
func TestTheDefaultRangeFollowsTheStoredWindowRatherThanTheFile(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.ConsoleWindowDays = 90
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
	seededAt := time.Now().UTC().AddDate(0, 0, -60)
	seedRetentionUsage(t, runtime, 8, seededAt)
	cookie, csrf := loginAdminForTest(t, runtime)
	if listed := listedAttempts(t, runtime, cookie, ""); listed != 8 {
		t.Fatalf("%d attempts listed inside the window, want all 8", listed)
	}

	_, etag := readUsageSettings(t, runtime, cookie)
	accepted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/usage", etag, map[string]any{"console_window_days": 30})
	if accepted.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if listed := listedAttempts(t, runtime, cookie, ""); listed != 0 {
		t.Fatalf("%d attempts listed outside the shortened default range, want none", listed)
	}
	explicit := "&start=" + seededAt.Add(-time.Hour).Format(time.RFC3339)
	if listed := listedAttempts(t, runtime, cookie, explicit); listed != 8 {
		t.Fatalf("%d attempts listed for an explicit older range, want all 8: shortening must discard nothing", listed)
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
