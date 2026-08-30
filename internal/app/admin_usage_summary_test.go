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

type summaryResponse struct {
	Granularity string `json:"granularity"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Buckets     []struct {
		Period                 string `json:"period"`
		Start                  string `json:"start"`
		End                    string `json:"end"`
		Requests               *int64 `json:"requests"`
		Attempts               int64  `json:"attempts"`
		CostMicrosUSD          int64  `json:"cost_micros_usd"`
		InputTokens            int64  `json:"input_tokens"`
		OutputTokens           int64  `json:"output_tokens"`
		UnknownAttempts        int64  `json:"unknown_attempts"`
		EstimatedCostMicrosUSD int64  `json:"estimated_cost_micros_usd"`
	} `json:"buckets"`
	Totals struct {
		Requests      *int64 `json:"requests"`
		RequestErrors *int64 `json:"request_errors"`
		Attempts      int64  `json:"attempts"`
		CostMicrosUSD int64  `json:"cost_micros_usd"`
		InputTokens   int64  `json:"input_tokens"`
		OutputTokens  int64  `json:"output_tokens"`
	} `json:"totals"`
	Groups []struct {
		Key           string `json:"key"`
		Requests      *int64 `json:"requests"`
		Attempts      int64  `json:"attempts"`
		CostMicrosUSD int64  `json:"cost_micros_usd"`
	} `json:"groups"`
	GroupsTruncated bool `json:"groups_truncated"`
	TimezoneChanges []struct {
		PeriodID string `json:"period_id"`
	} `json:"timezone_changes"`
}

type dashboardTodayResponse struct {
	Usage struct {
		Today struct {
			Requests      int64 `json:"requests"`
			Attempts      int64 `json:"attempts"`
			CostMicrosUSD int64 `json:"cost_micros_usd"`
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
		} `json:"today"`
	} `json:"usage"`
}

func summaryRuntime(t *testing.T, requests int) (*Runtime, *http.Cookie) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	for index := 0; index < requests; index++ {
		billOneRequest(t, runtime, "request_"+string(rune('a'+index)))
	}
	cookie, _ := loginAdminForTest(t, runtime)
	return runtime, cookie
}

func getSummary(t *testing.T, runtime *Runtime, cookie *http.Cookie, path string) summaryResponse {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, path)
	if response.Code != http.StatusOK {
		t.Fatalf("%s => %d %s", path, response.Code, response.Body.String())
	}
	var body summaryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// The summary and the dashboard read the same accounting day by two different
// paths — stored rollup versus live aggregate. An operator comparing the two
// screens has no way to tell which is right if they disagree, so they must not.
func TestUsageSummaryAgreesWithDashboardForToday(t *testing.T) {
	runtime, cookie := summaryRuntime(t, 2)
	runtime.saveUsageCheckpoint()

	summary := getSummary(t, runtime, cookie, "/admin/api/v1/usage/summary?granularity=day")
	dashboardResponse := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/dashboard")
	var dashboard dashboardTodayResponse
	if err := json.Unmarshal(dashboardResponse.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	today := dashboard.Usage.Today
	if summary.Totals.Requests == nil {
		t.Fatal("totals omitted the request count")
	}
	if *summary.Totals.Requests != today.Requests || summary.Totals.Attempts != today.Attempts ||
		summary.Totals.CostMicrosUSD != today.CostMicrosUSD ||
		summary.Totals.InputTokens != today.InputTokens ||
		summary.Totals.OutputTokens != today.OutputTokens {
		t.Fatalf("summary totals=%#v dashboard today=%#v", summary.Totals, today)
	}
	if today.Attempts != 2 {
		t.Fatalf("fixture billed %d attempts", today.Attempts)
	}
	if len(summary.Buckets) != 1 || summary.Buckets[0].Attempts != 2 {
		t.Fatalf("buckets=%#v", summary.Buckets)
	}
	// The console charts and drills down on these instants. A label alone
	// cannot be turned back into an interval, because the same label under two
	// generations of the accounting timezone covers two different ones.
	start, err := time.Parse(time.RFC3339, summary.Buckets[0].Start)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339, summary.Buckets[0].End)
	if err != nil {
		t.Fatal(err)
	}
	if !end.After(start) || end.Sub(start) != 24*time.Hour {
		t.Fatalf("day bucket spans %v (%s → %s)", end.Sub(start), start, end)
	}
}

// The rollup reaches disk on a checkpoint tick, up to a minute behind. A read
// that only looked at disk would report a smaller number than the dashboard
// beside it for that minute, with nothing to explain the gap.
func TestUsageSummaryIncludesTheUnpersistedIncrement(t *testing.T) {
	runtime, cookie := summaryRuntime(t, 1)
	// Deliberately no saveUsageCheckpoint: nothing has been written yet.
	if runtime.usage.PendingRollupRows() == 0 {
		t.Fatal("fixture wrote the increment before the assertion could test it")
	}
	summary := getSummary(t, runtime, cookie, "/admin/api/v1/usage/summary?granularity=day")
	if summary.Totals.Attempts != 1 {
		t.Fatalf("summary hid work that has not been checkpointed yet: %#v", summary.Totals)
	}
}

// The rollup stores one dimension per row and no cross terms, so this
// combination has no row that answers it. Returning the marginal total instead
// would answer a different question with the same shape of number.
func TestUsageSummaryRefusesGroupByCombinedWithAFilter(t *testing.T) {
	runtime, cookie := summaryRuntime(t, 1)
	response := authenticatedAdminGet(t, runtime, cookie,
		"/admin/api/v1/usage/summary?group_by=provider&project_id=project_1")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected the combination to be refused, got %d %s", response.Code, response.Body.String())
	}
	response = authenticatedAdminGet(t, runtime, cookie,
		"/admin/api/v1/usage/summary?project_id=project_1&provider_id=provider_1")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected two filters to be refused, got %d", response.Code)
	}
}

// A request can span providers through retry and fallback, so a provider row
// has no share of it to report. Absent, not zero: zero would read as "this
// provider served no requests".
func TestUsageSummaryOmitsRequestCountsOnProviderGroups(t *testing.T) {
	runtime, cookie := summaryRuntime(t, 1)
	runtime.saveUsageCheckpoint()

	byProject := getSummary(t, runtime, cookie, "/admin/api/v1/usage/summary?group_by=project")
	if len(byProject.Groups) != 1 || byProject.Groups[0].Requests == nil || *byProject.Groups[0].Requests != 1 {
		t.Fatalf("project groups=%#v", byProject.Groups)
	}
	byProvider := getSummary(t, runtime, cookie, "/admin/api/v1/usage/summary?group_by=provider")
	if len(byProvider.Groups) != 1 || byProvider.Groups[0].Attempts != 1 {
		t.Fatalf("provider groups=%#v", byProvider.Groups)
	}
	if byProvider.Groups[0].Requests != nil {
		t.Fatalf("provider group claimed a request count: %#v", byProvider.Groups[0])
	}
}

// A month is a whole number of accounting days, so both ends are inclusive.
// Half-open date labels would drop the 31st from August every time.
func TestUsageSummaryRangeIsInclusiveAndBounded(t *testing.T) {
	if start, end, err := summaryRange("2026-08-01", "2026-08-31", "day", "2026-08-30"); err != nil ||
		start != "2026-08-01" || end != "2026-08-31" {
		t.Fatalf("start=%s end=%s err=%v", start, end, err)
	}
	if _, _, err := summaryRange("2020-01-01", "2026-08-31", "month", "2026-08-30"); err == nil {
		t.Fatal("a range beyond the month ceiling must be refused, not truncated")
	}
	if _, _, err := summaryRange("2026-09-01", "2026-08-31", "day", "2026-08-30"); err == nil {
		t.Fatal("a backwards range must be refused")
	}
	start, _, err := summaryRange("", "2026-08-30", "month", "2026-08-30")
	if err != nil || start != "2025-09-30" {
		t.Fatalf("default month window start=%s err=%v", start, err)
	}
}

// The ceiling and the default window are two halves of one decision, and they
// were written apart: a limit expressed in months rejected the year view's own
// three-year default, so that view answered nothing but an error. Each default
// is checked against the ceiling it will actually be measured by.
func TestUsageSummaryDefaultRangeFitsItsOwnCeiling(t *testing.T) {
	for _, granularity := range []string{"day", "month", "year"} {
		start, end, err := summaryRange("", "", granularity, "2026-08-30")
		if err != nil {
			t.Fatalf("%s: the default range is not servable: %v", granularity, err)
		}
		startDate, parseErr := time.Parse(summaryDateLayout, start)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		endDate, parseErr := time.Parse(summaryDateLayout, end)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		buckets := summaryBucketCount(startDate, endDate, granularity)
		if buckets > summaryBucketCeilings[granularity] {
			t.Fatalf("%s: default window is %d buckets, ceiling is %d",
				granularity, buckets, summaryBucketCeilings[granularity])
		}
		if buckets < 2 {
			t.Fatalf("%s: default window is %d bucket, which charts nothing", granularity, buckets)
		}
	}
}

// A year view of three years is three points, and refusing it because the same
// span is 25 months at another resolution is the defect that put a permanent
// error on the page.
func TestUsageSummaryCeilingCountsBucketsNotMonths(t *testing.T) {
	if _, _, err := summaryRange("2024-08-30", "2026-08-30", "year", "2026-08-30"); err != nil {
		t.Fatalf("a three-year year view must be servable: %v", err)
	}
	if _, _, err := summaryRange("2024-08-30", "2026-08-30", "day", "2026-08-30"); err == nil {
		t.Fatal("the same span at day resolution is 731 points and must be refused")
	}
}
