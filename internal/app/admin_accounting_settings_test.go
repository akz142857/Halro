package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

type accountingSettingsBody struct {
	Timezone           string     `json:"timezone"`
	TimezoneVersion    uint64     `json:"timezone_version"`
	PendingTimezone    string     `json:"pending_timezone"`
	PendingEffectiveAt *time.Time `json:"pending_effective_at"`
	CurrentPeriod      struct {
		PeriodID    string    `json:"period_id"`
		PeriodStart time.Time `json:"period_start"`
		PeriodEnd   time.Time `json:"period_end"`
	} `json:"current_period"`
	ConfigFileTimezone string `json:"config_file_timezone"`
	ConfigFileInEffect bool   `json:"config_file_in_effect"`
	Tzdata             *struct {
		Source      string `json:"source"`
		Version     string `json:"version"`
		Fingerprint string `json:"fingerprint"`
	} `json:"tzdata"`
	Revision uint64 `json:"revision"`
}

func openAccountingRuntime(t *testing.T, zone string) (*Runtime, *http.Cookie, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Usage.Timezone = zone
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
	t.Cleanup(func() { runtime.Close() })
	cookie, csrf := loginAdminForTest(t, runtime)
	return runtime, cookie, csrf
}

func readAccountingSettings(t *testing.T, runtime *Runtime, cookie *http.Cookie) accountingSettingsBody {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/settings/accounting")
	var body accountingSettingsBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode accounting settings: %v", err)
	}
	return body
}

// A change must never move the boundary of a day that is already accumulating:
// budgets have been enforced against it, and redefining "today" halfway through
// would retroactively change what those enforcements meant.
func TestAccountingTimezoneChangeTakesEffectAtTheNextPeriod(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "Asia/Shanghai")

	before := readAccountingSettings(t, runtime, cookie)
	if before.Timezone != "Asia/Shanghai" || before.TimezoneVersion != 1 {
		t.Fatalf("seeded settings = %#v", before)
	}
	if !before.ConfigFileInEffect || before.ConfigFileTimezone != "Asia/Shanghai" {
		t.Fatalf("configuration provenance = %#v", before)
	}
	if before.Tzdata == nil || before.Tzdata.Fingerprint == "" {
		t.Fatal("tzdata provenance is missing")
	}

	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/accounting", revisionETag(before.Revision),
		map[string]string{"timezone": "Europe/Berlin"})
	if response.Code != http.StatusOK {
		t.Fatalf("schedule status=%d body=%s", response.Code, response.Body.String())
	}

	after := readAccountingSettings(t, runtime, cookie)
	if after.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone changed immediately to %s", after.Timezone)
	}
	if after.PendingTimezone != "Europe/Berlin" || after.PendingEffectiveAt == nil {
		t.Fatalf("change was not scheduled: %#v", after)
	}
	if !after.PendingEffectiveAt.Equal(before.CurrentPeriod.PeriodEnd) {
		t.Fatalf("effective at %s, want the end of the period in progress %s",
			after.PendingEffectiveAt, before.CurrentPeriod.PeriodEnd)
	}
	if after.CurrentPeriod.PeriodEnd != before.CurrentPeriod.PeriodEnd {
		t.Fatal("the period in progress was moved")
	}
	// The resolver answers by instant, so it must already report the new zone
	// for a time after the change is due, and the old one before it.
	newPeriod, err := runtime.periods.PeriodAt(after.PendingEffectiveAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if newPeriod.Timezone != "Europe/Berlin" || newPeriod.TimezoneVersion != before.TimezoneVersion+1 {
		t.Fatalf("period after the change = %#v", newPeriod)
	}
	oldPeriod, err := runtime.periods.PeriodAt(after.PendingEffectiveAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if oldPeriod.Timezone != "Asia/Shanghai" || oldPeriod.TimezoneVersion != before.TimezoneVersion {
		t.Fatalf("period before the change = %#v", oldPeriod)
	}
}

func TestAccountingTimezoneChangeCanBeCancelledBeforeItApplies(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "UTC")
	before := readAccountingSettings(t, runtime, cookie)

	scheduled := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/accounting", revisionETag(before.Revision),
		map[string]string{"timezone": "America/New_York"})
	if scheduled.Code != http.StatusOK {
		t.Fatalf("schedule status=%d body=%s", scheduled.Code, scheduled.Body.String())
	}
	pending := readAccountingSettings(t, runtime, cookie)

	cancelled := performAdminMutation(t, runtime, cookie, csrf, http.MethodDelete,
		"/admin/api/v1/settings/accounting/pending", revisionETag(pending.Revision), nil)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	after := readAccountingSettings(t, runtime, cookie)
	if after.PendingTimezone != "" || after.PendingEffectiveAt != nil {
		t.Fatalf("change is still scheduled: %#v", after)
	}
	if after.TimezoneVersion != before.TimezoneVersion {
		t.Fatalf("a cancelled change burned a version: %d", after.TimezoneVersion)
	}

	repeat := performAdminMutation(t, runtime, cookie, csrf, http.MethodDelete,
		"/admin/api/v1/settings/accounting/pending", revisionETag(after.Revision), nil)
	if repeat.Code != http.StatusBadRequest {
		t.Fatalf("cancel with nothing scheduled status=%d", repeat.Code)
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(repeat.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "timezone_pending_absent" {
		t.Fatalf("code = %q, want timezone_pending_absent", failure.Code)
	}
}

// A switch mints a fresh balance whose period can begin before the switch
// instant, so the daily allowance restarts a few hours later. An administrator
// cannot be expected to work that out; the server has to state it.
func TestAccountingTimezoneSwitchPreviewReportsTheResetWindow(t *testing.T) {
	runtime, cookie, _ := openAccountingRuntime(t, "Asia/Shanghai")

	response := authenticatedAdminGet(t, runtime, cookie,
		"/admin/api/v1/settings/accounting?preview_timezone=UTC")
	var body struct {
		CurrentPeriod struct {
			PeriodEnd time.Time `json:"period_end"`
		} `json:"current_period"`
		SwitchPreview *struct {
			Timezone    string    `json:"timezone"`
			EffectiveAt time.Time `json:"effective_at"`
			FirstPeriod struct {
				PeriodStart time.Time `json:"period_start"`
				PeriodEnd   time.Time `json:"period_end"`
			} `json:"first_period"`
			NextResetAt      time.Time `json:"next_reset_at"`
			NextResetInHours float64   `json:"next_reset_in_hours"`
		} `json:"switch_preview"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.SwitchPreview == nil {
		t.Fatal("no switch preview returned")
	}
	preview := body.SwitchPreview
	if !preview.EffectiveAt.Equal(body.CurrentPeriod.PeriodEnd) {
		t.Fatalf("preview effective at %s, want the end of the period in progress %s",
			preview.EffectiveAt, body.CurrentPeriod.PeriodEnd)
	}
	// Shanghai to UTC: the new UTC day began 16 hours before the switch, so the
	// budget resets again 8 hours after it.
	if preview.NextResetInHours <= 0 || preview.NextResetInHours > 24 {
		t.Fatalf("next reset in %v hours; must fall in (0, 24]", preview.NextResetInHours)
	}
	if !preview.FirstPeriod.PeriodStart.Before(preview.EffectiveAt) {
		t.Fatalf("first period starts at %s, expected it to begin before the switch at %s",
			preview.FirstPeriod.PeriodStart, preview.EffectiveAt)
	}
	if !preview.NextResetAt.Equal(preview.FirstPeriod.PeriodEnd) {
		t.Fatalf("next reset %s does not match the first period end %s",
			preview.NextResetAt, preview.FirstPeriod.PeriodEnd)
	}
}

// Asking to preview the zone already in force has nothing to warn about.
func TestAccountingTimezoneSwitchPreviewAbsentForCurrentZone(t *testing.T) {
	runtime, cookie, _ := openAccountingRuntime(t, "UTC")
	response := authenticatedAdminGet(t, runtime, cookie,
		"/admin/api/v1/settings/accounting?preview_timezone=UTC")
	if strings.Contains(response.Body.String(), "switch_preview") {
		t.Fatalf("preview returned for the current zone: %s", response.Body.String())
	}
}

// Once the switch has happened it is part of the ledger's history: the periods
// on either side already have separate balances, so "cancel" has nothing left
// to withdraw and must say so rather than silently doing nothing.
func TestCancellingAnElapsedTimezoneChangeIsRefused(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "UTC")
	current := readAccountingSettings(t, runtime, cookie)

	// Place the effective instant in the past directly, which is the state a
	// pending change reaches once its moment arrives.
	elapsed := time.Now().Add(-time.Hour).UTC()
	settings := runtime.periods.Settings()
	settings.PendingTimezone = "Europe/Berlin"
	settings.PendingEffectiveAt = &elapsed
	stored, err := runtime.store.PutInstanceAccountingSettings(settings, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.periods.Update(stored); err != nil {
		t.Fatal(err)
	}

	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodDelete,
		"/admin/api/v1/settings/accounting/pending", revisionETag(stored.Revision), nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "timezone_pending_elapsed" {
		t.Fatalf("code = %q, want timezone_pending_elapsed; body=%s", failure.Code, response.Body.String())
	}
}

func TestAccountingTimezoneRejectsUnusableZonesAndStaleRevisions(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "UTC")
	current := readAccountingSettings(t, runtime, cookie)

	for _, rejected := range []string{"", "Local", "UTC+08:00", "Mars/Olympus_Mons"} {
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
			"/admin/api/v1/settings/accounting", revisionETag(current.Revision),
			map[string]string{"timezone": rejected})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("timezone %q status=%d body=%s", rejected, response.Code, response.Body.String())
		}
		var failure struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
			t.Fatal(err)
		}
		// Fixed offsets and Local collapse into this one code on purpose; the
		// message distinguishes them.
		if failure.Code != "timezone_invalid" {
			t.Fatalf("timezone %q returned code %q, want timezone_invalid", rejected, failure.Code)
		}
	}
	stale := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/accounting", revisionETag(current.Revision+9),
		map[string]string{"timezone": "Europe/Berlin"})
	if stale.Code != http.StatusPreconditionFailed && stale.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d body=%s", stale.Code, stale.Body.String())
	}
}

// The version is a ledger balance key. Advancing it for a request that changes
// nothing would split a day's spending across two balances for no reason.
func TestAccountingTimezoneNoOpDoesNotBurnAVersion(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "Asia/Shanghai")
	before := readAccountingSettings(t, runtime, cookie)

	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/accounting", revisionETag(before.Revision),
		map[string]string{"timezone": "Asia/Shanghai"})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	after := readAccountingSettings(t, runtime, cookie)
	if after.TimezoneVersion != before.TimezoneVersion || after.Revision != before.Revision {
		t.Fatalf("a no-op advanced the setting: before=%#v after=%#v", before, after)
	}
	if after.PendingTimezone != "" {
		t.Fatalf("a no-op scheduled a change to %s", after.PendingTimezone)
	}
}

// Once due, the stored record should stop describing the change as pending and
// the audit trail should show it happened without an administrator present.
func TestDueAccountingTimezoneChangeIsAppliedAndAudited(t *testing.T) {
	runtime, cookie, csrf := openAccountingRuntime(t, "UTC")
	before := readAccountingSettings(t, runtime, cookie)

	if response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/accounting", revisionETag(before.Revision),
		map[string]string{"timezone": "Europe/Berlin"}); response.Code != http.StatusOK {
		t.Fatalf("schedule status=%d body=%s", response.Code, response.Body.String())
	}
	pending := readAccountingSettings(t, runtime, cookie)

	if err := runtime.applyDueAccountingTimezoneChange(pending.PendingEffectiveAt.Add(time.Minute)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := readAccountingSettings(t, runtime, cookie)
	if after.Timezone != "Europe/Berlin" {
		t.Fatalf("timezone = %s, want Europe/Berlin", after.Timezone)
	}
	if after.TimezoneVersion != before.TimezoneVersion+1 {
		t.Fatalf("version = %d, want %d", after.TimezoneVersion, before.TimezoneVersion+1)
	}
	if after.PendingTimezone != "" || after.PendingEffectiveAt != nil {
		t.Fatalf("change is still pending: %#v", after)
	}
	if after.ConfigFileInEffect {
		t.Fatal("the configuration file is still reported as in effect after a governed change")
	}

	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	body := audit.Body.String()
	if !strings.Contains(body, "accounting.timezone.change_scheduled") ||
		!strings.Contains(body, "accounting.timezone.change_applied") {
		t.Fatalf("audit trail is missing the timezone change: %s", body)
	}
	// The action name alone cannot tell an auditor which boundary applied to a
	// given charge; the payload has to name both zones and both versions.
	for _, detail := range []string{
		`"from_timezone":"UTC"`,
		`"to_timezone":"Europe/Berlin"`,
		`"from_version":1`,
		`"to_version":2`,
	} {
		if !strings.Contains(body, detail) {
			t.Fatalf("audit payload is missing %s: %s", detail, body)
		}
	}
	// Applying again must be a no-op rather than advancing the version a
	// second time on the next node or the next tick.
	if err := runtime.applyDueAccountingTimezoneChange(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	settled := readAccountingSettings(t, runtime, cookie)
	if settled.TimezoneVersion != after.TimezoneVersion {
		t.Fatalf("re-applying advanced the version to %d", settled.TimezoneVersion)
	}
}
