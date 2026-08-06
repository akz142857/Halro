package usage

import (
	"fmt"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/ledger"
)

func TestUsageCursorFilteringRequestDetailAndDashboard(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var sequence uint64
	for requestIndex := 1; requestIndex <= 3; requestIndex++ {
		requestID := fmt.Sprintf("req_%d", requestIndex)
		attemptID := requestID + ":1"
		for _, event := range []ledger.Event{
			{EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project", PeriodID: "period",
				OccurredAt:     now.Add(time.Duration(requestIndex) * time.Minute),
				RequestedModel: "chat"},
			{EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
				RequestID: requestID, AttemptID: attemptID, ProjectID: "project",
				ProviderID: fmt.Sprintf("provider_%d", requestIndex), PeriodID: "period",
				OccurredAt:          now.Add(time.Duration(requestIndex)*time.Minute + time.Second),
				ProviderInputTokens: int64(requestIndex), CommittedMicrosUSD: ledger.MicrosUSD(int64(requestIndex)),
				Outcome: "success"},
			{EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project", PeriodID: "period",
				OccurredAt: now.Add(time.Duration(requestIndex)*time.Minute + time.Second),
				Outcome:    "success"},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{
				Sequence: sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := aggregate.QueryAttempts(AttemptQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Attempts) != 2 || first.Attempts[0].RequestID != "req_3" ||
		first.NextCursor == "" {
		t.Fatalf("first page=%#v", first)
	}
	cursor, err := DecodeCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := aggregate.QueryAttempts(AttemptQuery{Limit: 2, BeforeSequence: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Attempts) != 1 || second.Attempts[0].RequestID != "req_1" ||
		second.NextCursor != "" {
		t.Fatalf("second page=%#v", second)
	}
	filtered, err := aggregate.QueryAttempts(AttemptQuery{Limit: 10, ProviderID: "provider_2"})
	if err != nil || len(filtered.Attempts) != 1 || filtered.Attempts[0].RequestID != "req_2" {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
	requestFiltered, err := aggregate.QueryAttempts(AttemptQuery{Limit: 10, RequestID: "req_1"})
	if err != nil || len(requestFiltered.Attempts) != 1 || requestFiltered.Attempts[0].RequestID != "req_1" {
		t.Fatalf("request filtered=%#v err=%v", requestFiltered, err)
	}
	detail, exists := aggregate.RequestDetail("req_2")
	if !exists || len(detail.Attempts) != 1 || detail.Summary.RequestID != "req_2" {
		t.Fatalf("detail=%#v exists=%v", detail, exists)
	}
	dashboard := aggregate.Dashboard(now.Add(time.Hour), utcDay(now.Add(time.Hour)))
	if dashboard.Today.Requests != 3 || dashboard.Today.Attempts != 3 ||
		dashboard.Today.InputTokens != 6 {
		t.Fatalf("dashboard=%#v", dashboard)
	}
}

func TestDashboardSeparatesConservativeTokenEstimates(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for index, event := range []ledger.Event{
		{EventID: "estimated", Kind: ledger.EventAttemptSettled, RequestID: "req_estimated",
			AttemptID: "attempt_estimated", ProjectID: "project", PeriodID: "period",
			OccurredAt: now, ProviderInputTokens: 9, ProviderOutputTokens: 16_384,
			CommittedMicrosUSD: ledger.MicrosUSD(0), TokenEstimated: true, Outcome: "provider_error"},
		{EventID: "reported", Kind: ledger.EventAttemptSettled, RequestID: "req_reported",
			AttemptID: "attempt_reported", ProjectID: "project", PeriodID: "period",
			OccurredAt: now.Add(time.Minute), ProviderInputTokens: 7, ProviderOutputTokens: 167, CommittedMicrosUSD: ledger.MicrosUSD(0),
			Outcome: "success"},
	} {
		if err := aggregate.Apply(ledger.Record{
			Sequence: uint64(index + 1), Offset: int64((index + 1) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}

	dashboard := aggregate.Dashboard(now.Add(time.Hour), utcDay(now.Add(time.Hour)))
	if dashboard.Today.InputTokens != 16 || dashboard.Today.OutputTokens != 16_551 {
		t.Fatalf("accounting totals changed: %#v", dashboard.Today)
	}
	if dashboard.Today.EstimatedInputTokens != 9 ||
		dashboard.Today.EstimatedOutputTokens != 16_384 {
		t.Fatalf("estimated totals not separated: %#v", dashboard.Today)
	}
	if len(dashboard.Hourly) != 1 || dashboard.Hourly[0].EstimatedOutputTokens != 16_384 {
		t.Fatalf("hourly estimated totals not separated: %#v", dashboard.Hourly)
	}
}

func TestDashboardBuildsTodayBreakdownsAndRecentAnomalies(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	events := []ledger.Event{
		{EventID: "failed", Kind: ledger.EventAttemptSettled, RequestID: "req_failed",
			AttemptID: "attempt_failed", ProjectID: "project_a", ProviderID: "provider_a",
			RequestedModel: "chat", ProviderModel: "model_a", PeriodID: "period",
			OccurredAt: now, ProviderInputTokens: 10, ProviderOutputTokens: 4,
			CommittedMicrosUSD: ledger.MicrosUSD(2_000), CostEstimated: true, Outcome: "provider_error",
			ErrorClass: "rate_limited", HTTPStatus: 429, RetryCount: 1},
		{EventID: "success", Kind: ledger.EventAttemptSettled, RequestID: "req_success",
			AttemptID: "attempt_success", ProjectID: "project_a", ProviderID: "provider_b",
			RequestedModel: "chat", ProviderModel: "model_b", PeriodID: "period",
			OccurredAt: now.Add(time.Minute), ProviderInputTokens: 8, ProviderOutputTokens: 3,
			CommittedMicrosUSD: ledger.MicrosUSD(1_000), Outcome: "success", FallbackCount: 1},
	}
	for index, event := range events {
		if err := aggregate.Apply(ledger.Record{Sequence: uint64(index + 1), Offset: int64(index + 1), Event: event}); err != nil {
			t.Fatal(err)
		}
	}

	dashboard := aggregate.Dashboard(now.Add(time.Hour), utcDay(now.Add(time.Hour)))
	projects := dashboard.Breakdowns["project"]
	if len(projects) != 1 || projects[0].Key != "project_a" || projects[0].Calls != 2 ||
		projects[0].Errors != 1 || projects[0].CostMicrosUSD != 3_000 || projects[0].EstimatedCostMicros != 2_000 {
		t.Fatalf("project breakdown=%#v", projects)
	}
	providers := dashboard.Breakdowns["provider"]
	if len(providers) != 2 || providers[0].Key != "provider_a" {
		t.Fatalf("provider breakdown=%#v", providers)
	}
	if dashboard.Today.EstimatedCostMicrosUSD != 2_000 {
		t.Fatalf("estimated cost=%d", dashboard.Today.EstimatedCostMicrosUSD)
	}
	if len(dashboard.RecentAnomalies) != 2 || dashboard.RecentAnomalies[0].FallbackCount != 1 ||
		dashboard.RecentAnomalies[1].HTTPStatus != 429 {
		t.Fatalf("recent anomalies=%#v", dashboard.RecentAnomalies)
	}
}

func TestEmptyDashboardUsesEmptyCollections(t *testing.T) {
	dashboard := NewAggregate().Dashboard(time.Now(), utcDay(time.Now()))
	if dashboard.Hourly == nil || dashboard.RecentAnomalies == nil {
		t.Fatalf("empty dashboard collections must encode as arrays: %#v", dashboard)
	}
	for dimension, items := range dashboard.Breakdowns {
		if items == nil {
			t.Fatalf("breakdown %q must encode as an array", dimension)
		}
	}
}

// utcDay is the UTC calendar day containing instant — the period these tests
// previously got implicitly by passing time.UTC.
func utcDay(instant time.Time) Period {
	start := instant.UTC().Truncate(24 * time.Hour)
	return Period{Start: start, End: start.Add(24 * time.Hour)}
}
