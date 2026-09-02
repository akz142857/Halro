package usage

import (
	"fmt"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/akz142857/Halro/internal/ledger"
)

// settledAt records one successful attempt at the given instant, charged to the
// given accounting period. The instant and the period are separate arguments on
// purpose: which day a call belongs to is decided when its request is admitted,
// not by where its completion lands on a clock.
func settledAt(t *testing.T, aggregate *Aggregate, sequence uint64, instant time.Time, periodID string, timezoneVersion uint64) {
	t.Helper()
	requestID := fmt.Sprintf("req_%d", sequence)
	attemptID := requestID + ":1"
	for offset, event := range []ledger.Event{
		{EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
			RequestID: requestID, ProjectID: "project", PeriodID: periodID,
			PeriodTimezoneVersion: timezoneVersion,
			OccurredAt:            instant, RequestedModel: "chat"},
		{EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
			RequestID: requestID, AttemptID: attemptID, ProjectID: "project",
			ProviderID: "provider", PeriodID: periodID, PeriodTimezoneVersion: timezoneVersion,
			OccurredAt:          instant,
			ProviderInputTokens: 1, CommittedMicrosUSD: ledger.MicrosUSD(1), Outcome: "success"},
	} {
		if err := aggregate.Apply(ledger.Record{
			Generation: 1,
			Sequence:   sequence*10 + uint64(offset), Offset: int64(sequence) * 100, Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// localDay returns the UTC interval of the local calendar date containing
// instant. The tests use it to generate realistic instants, not to decide
// membership.
func localDay(t *testing.T, zone string, instant time.Time) (time.Time, time.Time) {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	local := instant.In(location)
	year, month, day := local.Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, location)
	end := time.Date(year, month, day+1, 0, 0, 0, 0, location)
	return start.UTC(), end.UTC()
}

// A daily budget covers a calendar day, so on a fall-back day "today" has to
// span 25 hours. Keying on the stamped period makes that true by construction
// rather than by interval arithmetic that has to be rediscovered for every
// zone: the attempts are charged to one day, so they are reported as one day.
func TestTodaySpansTheWholeDSTDay(t *testing.T) {
	cases := []struct {
		name  string
		zone  string
		noon  time.Time
		hours int
	}{
		{name: "fall back is 25 hours", zone: "Europe/Berlin",
			noon: time.Date(2026, time.October, 25, 10, 0, 0, 0, time.UTC), hours: 25},
		{name: "spring forward is 23 hours", zone: "Europe/Berlin",
			noon: time.Date(2026, time.March, 29, 10, 0, 0, 0, time.UTC), hours: 23},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			start, end := localDay(t, testCase.zone, testCase.noon)
			if hours := int(end.Sub(start).Hours()); hours != testCase.hours {
				t.Fatalf("fixture day is %d hours, want %d", hours, testCase.hours)
			}
			aggregate := NewAggregate()
			for hour := 0; hour < testCase.hours; hour++ {
				settledAt(t, aggregate, uint64(hour+1), start.Add(time.Duration(hour)*time.Hour), "2026-10-25", 1)
			}
			dashboard := aggregate.Dashboard(start.Add(time.Hour), Period{ID: "2026-10-25", TimezoneVersion: 1})
			if dashboard.Today.Attempts != int64(testCase.hours) {
				t.Fatalf("today counted %d attempts over a %d hour day", dashboard.Today.Attempts, testCase.hours)
			}
		})
	}
}

// The case the interval rule got wrong: a request admitted at 23:59 and settled
// at 00:02 is charged to the day it was admitted on, because that is the budget
// it reserved against. Reporting it on the day it happened to finish would put
// the same call in a different day from the one that paid for it.
func TestTodayFollowsTheStampNotTheCompletionInstant(t *testing.T) {
	start, end := localDay(t, "Asia/Shanghai", time.Date(2026, time.August, 6, 3, 0, 0, 0, time.UTC))
	aggregate := NewAggregate()
	// Admitted on the 6th, settled three minutes into the 7th.
	settledAt(t, aggregate, 1, end.Add(2*time.Minute), "2026-08-06", 1)
	// Admitted on the 7th, settled immediately — inside the 7th, not the 6th.
	settledAt(t, aggregate, 2, end.Add(3*time.Minute), "2026-08-07", 1)
	// An ordinary call in the middle of the 6th.
	settledAt(t, aggregate, 3, start.Add(12*time.Hour), "2026-08-06", 1)

	sixth := aggregate.Dashboard(end, Period{ID: "2026-08-06", TimezoneVersion: 1})
	if sixth.Today.Attempts != 2 {
		t.Fatalf("the 6th counted %d attempts, want the two it was charged for", sixth.Today.Attempts)
	}
	seventh := aggregate.Dashboard(end, Period{ID: "2026-08-07", TimezoneVersion: 1})
	if seventh.Today.Attempts != 1 {
		t.Fatalf("the 7th counted %d attempts, want 1", seventh.Today.Attempts)
	}
}

// The same date label under two generations of the accounting timezone denotes
// two different UTC intervals. Adding them together would be adding two
// different days, so the version is part of the identity.
func TestTodayDistinguishesTimezoneVersions(t *testing.T) {
	start, _ := localDay(t, "Asia/Shanghai", time.Date(2026, time.August, 6, 3, 0, 0, 0, time.UTC))
	aggregate := NewAggregate()
	settledAt(t, aggregate, 1, start.Add(time.Hour), "2026-08-06", 1)
	settledAt(t, aggregate, 2, start.Add(2*time.Hour), "2026-08-06", 2)

	first := aggregate.Dashboard(start.Add(3*time.Hour), Period{ID: "2026-08-06", TimezoneVersion: 1})
	second := aggregate.Dashboard(start.Add(3*time.Hour), Period{ID: "2026-08-06", TimezoneVersion: 2})
	if first.Today.Attempts != 1 || second.Today.Attempts != 1 {
		t.Fatalf("versions were merged: v1=%d v2=%d", first.Today.Attempts, second.Today.Attempts)
	}
}
