package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
	_ "time/tzdata"
)

type timeContextEnvelope struct {
	TimeContext struct {
		AccountingTimezone string    `json:"accounting_timezone"`
		TimezoneVersion    uint64    `json:"timezone_version"`
		PeriodID           string    `json:"period_id"`
		PeriodStart        time.Time `json:"period_start"`
		PeriodEnd          time.Time `json:"period_end"`
		GeneratedAt        time.Time `json:"generated_at"`
	} `json:"time_context"`
}

// Every response carrying a per-day figure must say which day it means. The
// console has no other way to know: the accounting timezone lives on the
// server, and a browser guessing from its own locale produced axes that
// disagreed with the totals printed beside them.
func TestUsageResponsesCarryTimeContext(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Timezone = "Asia/Shanghai"
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
	cookie, _ := loginAdminForTest(t, runtime)

	for _, path := range []string{
		"/admin/api/v1/dashboard",
		"/admin/api/v1/usage",
		"/admin/api/v1/usage/summary",
		"/admin/api/v1/usage/failures",
		"/admin/api/v1/system/status",
	} {
		t.Run(path, func(t *testing.T) {
			response := authenticatedAdminGet(t, runtime, cookie, path)
			var envelope timeContextEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			timing := envelope.TimeContext
			if timing.AccountingTimezone != "Asia/Shanghai" {
				t.Fatalf("accounting_timezone = %q, want Asia/Shanghai", timing.AccountingTimezone)
			}
			if timing.PeriodID == "" {
				t.Fatal("period_id is empty")
			}
			if !timing.PeriodEnd.After(timing.PeriodStart) {
				t.Fatalf("period [%s,%s) is empty", timing.PeriodStart, timing.PeriodEnd)
			}
			if timing.GeneratedAt.Before(timing.PeriodStart) || !timing.GeneratedAt.Before(timing.PeriodEnd) {
				t.Fatalf("generated_at %s falls outside the period it describes [%s,%s)",
					timing.GeneratedAt, timing.PeriodStart, timing.PeriodEnd)
			}
			// The version is what keeps periods either side of a timezone
			// change in separate balances, so it must reach the client.
			if timing.TimezoneVersion == 0 {
				t.Fatal("timezone_version is 0; the governed setting was not consulted")
			}
		})
	}
}

// The bounds are only useful if the aggregate sums "today" over exactly the
// same window; a mismatch would put the totals and the chart back out of step.
func TestTimeContextMatchesDashboardToday(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Timezone = "America/Santiago"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	// A local midnight is where the two definitions are most likely to part
	// company, and Santiago starts summer time exactly there.
	instants := []time.Time{
		time.Date(2024, time.September, 8, 4, 0, 0, 0, time.UTC),
		time.Date(2024, time.September, 8, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.August, 6, 9, 12, 33, 0, time.UTC),
	}
	for _, instant := range instants {
		timing, err := runtime.timeContextAt(instant)
		if err != nil {
			t.Fatalf("timeContextAt(%s): %v", instant, err)
		}
		location, err := runtime.periods.LocationAt(instant)
		if err != nil {
			t.Fatalf("LocationAt(%s): %v", instant, err)
		}
		local := instant.In(location)
		if timing.PeriodID != local.Format("2006-01-02") {
			t.Fatalf("period_id = %s, but the aggregate groups %s under %s",
				timing.PeriodID, instant, local.Format("2006-01-02"))
		}
		// The aggregate decides membership by local calendar date, so both
		// edges of the reported window must map to that same date.
		if got := timing.PeriodStart.In(location).Format("2006-01-02"); got != timing.PeriodID {
			t.Fatalf("period_start %s falls on %s, not on %s", timing.PeriodStart, got, timing.PeriodID)
		}
		if got := timing.PeriodEnd.Add(-time.Nanosecond).In(location).Format("2006-01-02"); got != timing.PeriodID {
			t.Fatalf("period_end %s closes on %s, not on %s", timing.PeriodEnd, got, timing.PeriodID)
		}
	}
}
