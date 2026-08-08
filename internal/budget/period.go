package budget

import (
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// Period is the accounting window a daily budget is enforced against.
//
// The identity is deliberately more than the date string: the same
// "2026-08-06" denotes different UTC intervals under different zones, so a
// period that only carries the date cannot be re-derived by anyone reading it
// later. Start and End make it self-describing.
type Period struct {
	// ID is the local calendar date, the form written to the ledger.
	ID string
	// Timezone is the IANA name the boundaries were derived from.
	Timezone string
	// TimezoneVersion identifies which generation of the accounting timezone
	// setting produced this period. It is 0 while the timezone is still read
	// from configuration rather than from a governed, versioned setting.
	TimezoneVersion uint64
	// Start and End are the UTC half-open interval [Start, End).
	Start time.Time
	End   time.Time
}

// Contains reports whether instant falls inside the period.
func (p Period) Contains(instant time.Time) bool {
	return !instant.Before(p.Start) && instant.Before(p.End)
}

// Stamp writes the period's identity onto an event.
//
// Downstream events inherit the period their request began in rather than
// re-deriving one, so a request that runs across a boundary reserves and
// settles against a single budget. Recomputing at settlement would charge a
// reservation to one day and its settlement to the next.
func (p Period) Stamp(event *ledger.Event) {
	event.PeriodID = p.ID
	event.PeriodTimezone = p.Timezone
	event.PeriodTimezoneVersion = p.TimezoneVersion
	event.PeriodStartMicros = p.Start.UnixMicro()
	event.PeriodEndMicros = p.End.UnixMicro()
}

// PeriodFromEvent reconstructs the period an event was filed under, using only
// the event itself. This is the property §6.3 exists for: an auditor reading a
// single record can state the exact UTC interval it was charged against.
func PeriodFromEvent(event ledger.Event) Period {
	period := Period{
		ID:              event.PeriodID,
		Timezone:        event.PeriodTimezone,
		TimezoneVersion: event.PeriodTimezoneVersion,
	}
	if event.PeriodStartMicros != 0 {
		period.Start = time.UnixMicro(event.PeriodStartMicros).UTC()
	}
	if event.PeriodEndMicros != 0 {
		period.End = time.UnixMicro(event.PeriodEndMicros).UTC()
	}
	return period
}

// minPeriodLength and maxPeriodLength bound a plausible calendar day. A DST
// transition moves a day to 23 or 25 hours, and half-hour zones such as
// Australia/Lord_Howe to 23.5 or 24.5. Anything outside the range means the
// rules being applied are not describing a calendar day, which is a corrupt or
// truncated tzdata rather than an unusual jurisdiction — refusing is safer than
// billing against a boundary nobody can reproduce.
const (
	minPeriodLength = 22 * time.Hour
	maxPeriodLength = 26 * time.Hour
)

// checkPeriodLength rejects a span that is not describing a calendar day.
//
// No IANA zone can reach this: the two ways a day changes length — a summer
// time transition and an erased date label — are both handled above and both
// land inside the band. It fires only on rules that are corrupt or truncated,
// where continuing would bill against a boundary nobody can reproduce. It is a
// separate function so that guarantee can be tested without fabricating a zone.
func checkPeriodLength(length time.Duration) error {
	if length < minPeriodLength || length > maxPeriodLength {
		return fmt.Errorf("period length %s is outside the plausible range %s–%s; time zone rules are unusable",
			length, minPeriodLength, maxPeriodLength)
	}
	return nil
}

// maxErasedDates bounds how many consecutive calendar labels a date-line move
// may erase. Every recorded move — Samoa 2011, Kiritimati 1994, Tokelau 2011 —
// erased exactly one. Two is slack; beyond that the rules are not describing a
// calendar and the period length guard rejects the result anyway.
const maxErasedDates = 2

// maxMidnightGapMinutes bounds how far into a local date the search for its
// first instant will walk. Chile and Cuba begin summer time at midnight, so
// their first day of summer time has no 00:00 at all; no jurisdiction has ever
// erased more than an hour there.
const maxMidnightGapMinutes = 240

// startOfLocalDate returns the first instant of a local calendar date, and
// whether that date exists in this zone at all.
//
// time.Date cannot answer this on its own. Asked for a local time a forward
// transition erased, it normalizes to a real instant — but normalizes
// *backwards*, onto the preceding date. Taking that as the start of the day
// would overlap it with the previous period and place instants in two periods
// at once, so the erased range has to be walked to the first minute that
// genuinely belongs to the requested date.
func startOfLocalDate(year int, month time.Month, day int, location *time.Location) (time.Time, bool) {
	// Normalize the calendar arithmetic (day+1 may run past the month end)
	// before comparing dates, so the comparison below is against a real date.
	year, month, day = time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Date()
	for minute := 0; minute <= maxMidnightGapMinutes; minute++ {
		candidate := time.Date(year, month, day, 0, minute, 0, 0, location)
		if candidateYear, candidateMonth, candidateDay := candidate.Date(); candidateYear == year && candidateMonth == month && candidateDay == day {
			return candidate, true
		}
	}
	return time.Time{}, false
}

// PeriodAt returns the accounting period containing instant.
//
// The end of the period is the next local midnight computed on the calendar,
// never instant plus 24 hours: a DST day is 23 or 25 hours long and a daily
// budget covers a calendar day, not a fixed duration. Fixed-offset arithmetic
// would leave a gap or an overlap at every transition.
func PeriodAt(instant time.Time, location *time.Location) (Period, error) {
	if location == nil {
		return Period{}, errors.New("accounting timezone is required")
	}
	if instant.IsZero() {
		return Period{}, errors.New("instant is required")
	}
	local := instant.In(location)
	year, month, day := local.Date()
	start, ok := startOfLocalDate(year, month, day, location)
	if !ok {
		return Period{}, fmt.Errorf("timezone %s has no instant on %04d-%02d-%02d", location.String(), year, month, day)
	}
	// A jurisdiction moving across the date line erases a calendar date
	// outright — Samoa had no 2011-12-30. The day it did have was a normal
	// length; only the label is missing, so skip to the next label that exists.
	var end time.Time
	for offset := 1; offset <= maxErasedDates+1; offset++ {
		candidate, found := startOfLocalDate(year, month, day+offset, location)
		if found && candidate.After(start) {
			end = candidate
			break
		}
	}
	if end.IsZero() {
		return Period{}, fmt.Errorf("timezone %s has no date following %04d-%02d-%02d", location.String(), year, month, day)
	}
	if err := checkPeriodLength(end.Sub(start)); err != nil {
		return Period{}, fmt.Errorf("timezone %s for %04d-%02d-%02d: %w", location.String(), year, month, day, err)
	}
	period := Period{
		ID:       local.Format("2006-01-02"),
		Timezone: location.String(),
		Start:    start.UTC(),
		End:      end.UTC(),
	}
	if !period.Contains(instant.UTC()) {
		return Period{}, fmt.Errorf("timezone %s placed %s outside period %s", location.String(), instant.UTC().Format(time.RFC3339), period.ID)
	}
	return period, nil
}
