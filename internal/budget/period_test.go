package budget

import (
	"testing"
	"time"
	// The host may not ship a zone database; period arithmetic is the whole
	// subject of these tests, so pin it to the one the binary embeds.
	_ "time/tzdata"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return location
}

func TestPeriodAtBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		zone     string
		instant  time.Time
		wantID   string
		wantLen  time.Duration
		wantHead string
	}{
		{
			name:     "utc has no transitions",
			zone:     "UTC",
			instant:  time.Date(2026, time.August, 6, 9, 12, 33, 0, time.UTC),
			wantID:   "2026-08-06",
			wantLen:  24 * time.Hour,
			wantHead: "2026-08-06T00:00:00Z",
		},
		{
			name:     "fixed offset day starts the previous UTC evening",
			zone:     "Asia/Shanghai",
			instant:  time.Date(2026, time.August, 6, 9, 12, 33, 0, time.UTC),
			wantID:   "2026-08-06",
			wantLen:  24 * time.Hour,
			wantHead: "2026-08-05T16:00:00Z",
		},
		{
			name:     "spring forward is 23 hours",
			zone:     "Europe/Berlin",
			instant:  time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC),
			wantID:   "2026-03-29",
			wantLen:  23 * time.Hour,
			wantHead: "2026-03-28T23:00:00Z",
		},
		{
			name:     "fall back is 25 hours",
			zone:     "Europe/Berlin",
			instant:  time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC),
			wantID:   "2026-10-25",
			wantLen:  25 * time.Hour,
			wantHead: "2026-10-24T22:00:00Z",
		},
		{
			name:     "southern hemisphere fall back is 25 hours",
			zone:     "America/Santiago",
			instant:  time.Date(2024, time.April, 6, 12, 0, 0, 0, time.UTC),
			wantID:   "2024-04-06",
			wantLen:  25 * time.Hour,
			wantHead: "2024-04-06T03:00:00Z",
		},
		{
			name:     "half hour transition is 23.5 hours",
			zone:     "Australia/Lord_Howe",
			instant:  time.Date(2024, time.October, 6, 3, 0, 0, 0, time.UTC),
			wantID:   "2024-10-06",
			wantLen:  23*time.Hour + 30*time.Minute,
			wantHead: "2024-10-05T13:30:00Z",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			period, err := PeriodAt(testCase.instant, mustLoad(t, testCase.zone))
			if err != nil {
				t.Fatalf("PeriodAt: %v", err)
			}
			if period.ID != testCase.wantID {
				t.Fatalf("ID = %s, want %s", period.ID, testCase.wantID)
			}
			if got := period.Start.Format(time.RFC3339); got != testCase.wantHead {
				t.Fatalf("Start = %s, want %s", got, testCase.wantHead)
			}
			if got := period.End.Sub(period.Start); got != testCase.wantLen {
				t.Fatalf("length = %s, want %s", got, testCase.wantLen)
			}
			if !period.Contains(testCase.instant) {
				t.Fatalf("period %s does not contain %s", period.ID, testCase.instant)
			}
			if period.Timezone != testCase.zone {
				t.Fatalf("Timezone = %s, want %s", period.Timezone, testCase.zone)
			}
		})
	}
}

// A daily budget covers a calendar day. Adding 24 hours to the start would
// leave an hour uncovered every spring and double-count one every autumn.
func TestPeriodAtIsNotFixedTwentyFourHours(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	period, err := PeriodAt(time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC), berlin)
	if err != nil {
		t.Fatalf("PeriodAt: %v", err)
	}
	if got := period.End.Sub(period.Start); got == 24*time.Hour {
		t.Fatal("fall back period is 24h; boundaries were derived by fixed offset rather than by calendar")
	}
}

// Chile and Cuba start summer time at midnight, so the first day of summer time
// has no 00:00. Asking time.Date for it normalizes backwards onto the previous
// date; taking that as the day's start would overlap two periods and let one
// instant be billed against both.
func TestPeriodAtMidnightGap(t *testing.T) {
	cases := []struct {
		zone      string
		instant   time.Time
		wantID    string
		wantStart string
		wantLen   time.Duration
	}{
		{
			zone:      "America/Santiago",
			instant:   time.Date(2024, time.September, 8, 12, 0, 0, 0, time.UTC),
			wantID:    "2024-09-08",
			wantStart: "2024-09-08T04:00:00Z",
			wantLen:   23 * time.Hour,
		},
		{
			zone:      "America/Havana",
			instant:   time.Date(2024, time.March, 10, 12, 0, 0, 0, time.UTC),
			wantID:    "2024-03-10",
			wantStart: "2024-03-10T05:00:00Z",
			wantLen:   23 * time.Hour,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.zone, func(t *testing.T) {
			location := mustLoad(t, testCase.zone)
			period, err := PeriodAt(testCase.instant, location)
			if err != nil {
				t.Fatalf("PeriodAt: %v", err)
			}
			if period.ID != testCase.wantID {
				t.Fatalf("ID = %s, want %s", period.ID, testCase.wantID)
			}
			if got := period.Start.Format(time.RFC3339); got != testCase.wantStart {
				t.Fatalf("Start = %s, want %s", got, testCase.wantStart)
			}
			if got := period.End.Sub(period.Start); got != testCase.wantLen {
				t.Fatalf("length = %s, want %s", got, testCase.wantLen)
			}
			// The instant one tick before the start must belong to the previous
			// period, never to this one.
			previous, err := PeriodAt(period.Start.Add(-time.Nanosecond), location)
			if err != nil {
				t.Fatalf("PeriodAt previous: %v", err)
			}
			if !previous.End.Equal(period.Start) {
				t.Fatalf("previous period %s ends at %s but %s starts at %s",
					previous.ID, previous.End, period.ID, period.Start)
			}
		})
	}
}

// Samoa had no 2011-12-30: the label was erased when it crossed the date line.
// The day it did have was a normal 24 hours and must be accounted as one period.
func TestPeriodAtErasedCalendarDate(t *testing.T) {
	apia := mustLoad(t, "Pacific/Apia")
	instant := time.Date(2011, time.December, 29, 22, 0, 0, 0, time.UTC)
	period, err := PeriodAt(instant, apia)
	if err != nil {
		t.Fatalf("PeriodAt: %v", err)
	}
	if period.ID != "2011-12-29" {
		t.Fatalf("ID = %s, want 2011-12-29", period.ID)
	}
	if got := period.End.Sub(period.Start); got != 24*time.Hour {
		t.Fatalf("length = %s, want 24h", got)
	}
	if !period.Contains(instant) {
		t.Fatal("period does not contain the instant that produced it")
	}
}

// Contiguity is the property that makes a sequence of periods an accounting
// system: every instant belongs to exactly one, so nothing can be billed twice
// or escape a budget by landing in a gap.
func TestPeriodsAreContiguous(t *testing.T) {
	for _, zone := range []string{"UTC", "Asia/Shanghai", "Europe/Berlin", "America/New_York", "America/Santiago", "America/Havana", "Australia/Lord_Howe", "Pacific/Apia"} {
		t.Run(zone, func(t *testing.T) {
			location := mustLoad(t, zone)
			cursor := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
			previous, err := PeriodAt(cursor, location)
			if err != nil {
				t.Fatalf("PeriodAt: %v", err)
			}
			for day := 0; day < 400; day++ {
				// Step from inside the period rather than by a fixed day, so a
				// 23-hour period cannot be skipped over.
				next, err := PeriodAt(previous.End, location)
				if err != nil {
					t.Fatalf("day %d: %v", day, err)
				}
				if !next.Start.Equal(previous.End) {
					t.Fatalf("day %d: %s starts at %s but %s ended at %s",
						day, next.ID, next.Start, previous.ID, previous.End)
				}
				if !next.End.After(next.Start) {
					t.Fatalf("day %d: period %s is empty", day, next.ID)
				}
				previous = next
			}
		})
	}
}

func TestPeriodAtRejectsUnusableInput(t *testing.T) {
	if _, err := PeriodAt(time.Now(), nil); err == nil {
		t.Fatal("nil location accepted; a missing zone must not silently mean UTC")
	}
	if _, err := PeriodAt(time.Time{}, time.UTC); err == nil {
		t.Fatal("zero instant accepted")
	}
}

// The guard against unusable time zone rules. No IANA zone reaches it — the two
// ways a real day changes length are both handled and both land inside the band
// — so it is tested at the boundary directly rather than by fabricating a zone.
func TestPeriodLengthGuard(t *testing.T) {
	accepted := []time.Duration{
		22 * time.Hour,
		23 * time.Hour,
		23*time.Hour + 30*time.Minute, // Australia/Lord_Howe
		24 * time.Hour,
		25 * time.Hour,
		26 * time.Hour,
	}
	for _, length := range accepted {
		if err := checkPeriodLength(length); err != nil {
			t.Fatalf("checkPeriodLength(%s) = %v, want nil", length, err)
		}
	}
	rejected := []time.Duration{
		0,
		-time.Hour,
		21*time.Hour + 59*time.Minute,
		26*time.Hour + time.Minute,
		48 * time.Hour,
	}
	for _, length := range rejected {
		if err := checkPeriodLength(length); err == nil {
			t.Fatalf("checkPeriodLength(%s) = nil, want an error", length)
		}
	}
}

// Every zone the resolver is expected to serve stays inside the guard across a
// full year, so the guard can never deny service for a legitimate jurisdiction.
func TestRealZonesNeverTripThePeriodLengthGuard(t *testing.T) {
	zones := []string{
		"UTC", "Asia/Shanghai", "Europe/Berlin", "America/New_York", "America/Santiago",
		"America/Havana", "Australia/Lord_Howe", "Pacific/Apia", "Pacific/Kiritimati",
		"Asia/Kathmandu", "Iran", "Pacific/Chatham",
	}
	for _, zone := range zones {
		t.Run(zone, func(t *testing.T) {
			location := mustLoad(t, zone)
			cursor := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
			for day := 0; day < 400; day++ {
				period, err := PeriodAt(cursor, location)
				if err != nil {
					t.Fatalf("day %d (%s): %v", day, cursor, err)
				}
				cursor = period.End
			}
		})
	}
}
