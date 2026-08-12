package usage

import (
	"fmt"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/akz142857/Halro/internal/ledger"
)

// settledAt records one successful attempt at the given instant.
func settledAt(t *testing.T, aggregate *Aggregate, sequence uint64, instant time.Time) {
	t.Helper()
	requestID := fmt.Sprintf("req_%d", sequence)
	attemptID := requestID + ":1"
	for offset, event := range []ledger.Event{
		{EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
			RequestID: requestID, ProjectID: "project", PeriodID: "period",
			OccurredAt: instant, RequestedModel: "chat"},
		{EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
			RequestID: requestID, AttemptID: attemptID, ProjectID: "project",
			ProviderID: "provider", PeriodID: "period", OccurredAt: instant,
			ProviderInputTokens: 1, CommittedMicrosUSD: ledger.MicrosUSD(1), Outcome: "success"},
	} {
		if err := aggregate.Apply(ledger.Record{
			Sequence: sequence*10 + uint64(offset), Offset: int64(sequence) * 100, Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// localDay is the accounting period for the local calendar date containing
// instant, computed the way the resolver computes it.
func localDay(t *testing.T, zone string, instant time.Time) Period {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	local := instant.In(location)
	year, month, day := local.Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, location)
	end := time.Date(year, month, day+1, 0, 0, 0, 0, location)
	return Period{Start: start.UTC(), End: end.UTC()}
}

// A daily budget covers a calendar day, so on a fall-back day "today" has to
// span 25 hours. Comparing local calendar dates happened to get this right;
// stating the interval makes it true by construction and lets the figures match
// the interval the response advertises.
func TestTodaySpansTheWholeDSTDay(t *testing.T) {
	cases := []struct {
		name  string
		zone  string
		noon  time.Time
		hours int
	}{
		{name: "fall back is 25 hours", zone: "Europe/Berlin",
			noon: time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC), hours: 25},
		{name: "spring forward is 23 hours", zone: "Europe/Berlin",
			noon: time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC), hours: 23},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			period := localDay(t, testCase.zone, testCase.noon)
			if got := int(period.End.Sub(period.Start).Hours()); got != testCase.hours {
				t.Fatalf("period is %d hours, want %d", got, testCase.hours)
			}
			aggregate := NewAggregate()
			// One attempt in every hour of the local day, plus one on each side
			// of the boundary that must be excluded.
			var sequence uint64
			for hour := 0; hour < testCase.hours; hour++ {
				sequence++
				settledAt(t, aggregate, sequence, period.Start.Add(time.Duration(hour)*time.Hour))
			}
			sequence++
			settledAt(t, aggregate, sequence, period.Start.Add(-time.Second))
			sequence++
			settledAt(t, aggregate, sequence, period.End)

			dashboard := aggregate.Dashboard(period.Start.Add(time.Hour), period)
			if dashboard.Today.Attempts != int64(testCase.hours) {
				t.Fatalf("today counted %d attempts over a %d hour day; the interval was not applied",
					dashboard.Today.Attempts, testCase.hours)
			}
		})
	}
}

// The boundary is half-open: the closing instant belongs to the next day, and
// the instant before it to this one. Getting this wrong double-counts an
// attempt or drops it.
func TestTodayBoundaryIsHalfOpen(t *testing.T) {
	period := localDay(t, "Asia/Shanghai", time.Date(2026, time.August, 6, 3, 0, 0, 0, time.UTC))
	aggregate := NewAggregate()
	settledAt(t, aggregate, 1, period.Start)
	settledAt(t, aggregate, 2, period.End.Add(-time.Nanosecond))
	settledAt(t, aggregate, 3, period.End)

	dashboard := aggregate.Dashboard(period.Start.Add(time.Hour), period)
	if dashboard.Today.Attempts != 2 {
		t.Fatalf("today counted %d attempts, want 2 (start inclusive, end exclusive)", dashboard.Today.Attempts)
	}
	if !period.Contains(period.Start) || period.Contains(period.End) {
		t.Fatal("Contains is not half-open")
	}
}

// Offset zones do not align their local midnight with a UTC hour. Summing whole
// UTC buckets leaks the first or last partial hour into another accounting day.
func TestTodayUsesExactBoundaryInHalfHourZone(t *testing.T) {
	period := localDay(t, "Asia/Kolkata", time.Date(2026, time.August, 6, 6, 0, 0, 0, time.UTC))
	aggregate := NewAggregate()
	settledAt(t, aggregate, 1, period.Start.Add(-time.Minute))
	settledAt(t, aggregate, 2, period.Start)
	settledAt(t, aggregate, 3, period.End.Add(-time.Nanosecond))
	settledAt(t, aggregate, 4, period.End)

	dashboard := aggregate.Dashboard(period.Start.Add(time.Hour), period)
	if dashboard.Today.Attempts != 2 {
		t.Fatalf("today counted %d attempts, want only the exact half-hour period", dashboard.Today.Attempts)
	}
}
