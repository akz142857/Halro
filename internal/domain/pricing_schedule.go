package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// minutesPerDay bounds every window edge. A window is a span of one ordinary
// day in the provider's own zone, so summer time is handled by resolving the
// local wall clock through the zone rather than by widening this bound.
const minutesPerDay = 24 * 60

// maxPriceWindows bounds the table an operator can type. Real published
// schedules have two or three rungs; the bound exists so a malformed import
// cannot make price selection walk an unbounded list on the request path.
const maxPriceWindows = 24

// PriceTierSource says which rung of a price version produced the rates an
// attempt was billed at.
type PriceTierSource string

const (
	// PriceTierBase is the version's own rates, used when no window covers the
	// pinned instant — including when the version carries no schedule at all.
	PriceTierBase PriceTierSource = "base"
	// PriceTierWindow is a recurring window's rates.
	PriceTierWindow PriceTierSource = "window"
	// PriceTierZoneUnavailable is the fail-closed rung: the schedule's zone
	// could not be resolved, so the highest rate the version can express was
	// used instead of guessing an hour. See DeploymentPriceVersion.TierAt.
	PriceTierZoneUnavailable PriceTierSource = "zone_unavailable"
)

// PriceWindow prices a recurring daily span at rates that replace the price
// version's base rates for an attempt pinned inside it.
//
// The rates are absolute, not a discount factor applied to the base. A factor
// would put a second, derived way to express a rate next to the literal one and
// make every billed amount depend on where that division rounds; these are the
// same exact micro-USD integers the base terms are, and they are compared and
// re-derived the same way.
type PriceWindow struct {
	// StartMinute is inclusive and EndMinute exclusive, both counted from
	// midnight in the schedule's zone. A span that crosses midnight is two
	// windows, so a window never has to be read as wrapping.
	StartMinute                 int   `json:"start_minute"`
	EndMinute                   int   `json:"end_minute"`
	InputMicrosPerMillion       int64 `json:"input_micros_per_million"`
	CachedInputMicrosPerMillion int64 `json:"cached_input_micros_per_million"`
	OutputMicrosPerMillion      int64 `json:"output_micros_per_million"`
	FixedRequestMicrosUSD       int64 `json:"fixed_request_micros_usd"`
}

// PriceSchedule is the recurring rule a price version carries when its provider
// bills different rates at different times of day.
//
// It lives inside the price version, and it has to: a settlement's price
// snapshot is re-derived from the version it names plus the instant it was
// pinned at, and any check that re-derivation feeds — the Ledger's byte
// equality between reservation and settlement, the backup validator, replay —
// only holds while the snapshot is a pure function of those two. A schedule
// held beside the version and referenced by it would be mutable state inside
// that function, and every past snapshot would stop being reproducible the
// moment it changed.
type PriceSchedule struct {
	// Timezone is the provider's authority for what "9am" means, and it is not
	// the instance's accounting timezone or the operator's display timezone.
	// An instance keeping its books in UTC still bills a Beijing-scheduled
	// discount on Beijing hours.
	Timezone string `json:"timezone"`
	// Windows are disjoint and sorted by StartMinute. They need not cover the
	// day: an instant no window covers is billed at the version's base rates.
	Windows []PriceWindow `json:"windows"`
}

// PriceTier is the rate table one attempt is billed at, together with the
// evidence for why that rung was chosen.
type PriceTier struct {
	InputMicrosPerMillion       int64
	CachedInputMicrosPerMillion int64
	OutputMicrosPerMillion      int64
	FixedRequestMicrosUSD       int64
	// Provenance is nil exactly when the version carries no schedule, which
	// keeps a fixed price's snapshot byte-identical to what it was before
	// schedules existed.
	Provenance *PriceScheduleTier
}

// PriceScheduleTier is the part of a tier that is written into the price
// snapshot, so a settled attempt can say which rung billed it without the
// reader needing the zone database or the version's rule table.
type PriceScheduleTier struct {
	Timezone string          `json:"timezone"`
	Source   PriceTierSource `json:"source"`
	// StartMinute and EndMinute are set only for PriceTierWindow.
	StartMinute *int `json:"start_minute,omitempty"`
	EndMinute   *int `json:"end_minute,omitempty"`
	// LocalMinute is the pinned instant's minute of day in the schedule's zone.
	// It is nil on the PriceTierZoneUnavailable path, where the whole point is
	// that no local hour could be established.
	LocalMinute *int `json:"local_minute,omitempty"`
}

func (t PriceScheduleTier) Validate() error {
	if err := validateIANAZoneName("price schedule timezone", t.Timezone); err != nil {
		return err
	}
	inWindow := t.StartMinute != nil && t.EndMinute != nil
	switch t.Source {
	case PriceTierWindow:
		if !inWindow {
			return errors.New("window price tier must carry its window bounds")
		}
		if !validWindowBounds(*t.StartMinute, *t.EndMinute) {
			return errors.New("window price tier bounds are out of range")
		}
		if t.LocalMinute == nil || *t.LocalMinute < *t.StartMinute || *t.LocalMinute >= *t.EndMinute {
			return errors.New("window price tier local minute falls outside its window")
		}
	case PriceTierBase:
		if inWindow || t.LocalMinute == nil {
			return errors.New("base price tier must carry a local minute and no window bounds")
		}
	case PriceTierZoneUnavailable:
		if inWindow || t.LocalMinute != nil {
			return errors.New("zone-unavailable price tier must not claim a local time")
		}
	default:
		return fmt.Errorf("unsupported price tier source %q", t.Source)
	}
	if t.LocalMinute != nil && (*t.LocalMinute < 0 || *t.LocalMinute >= minutesPerDay) {
		return errors.New("price tier local minute is out of range")
	}
	return nil
}

func (t PriceScheduleTier) Clone() PriceScheduleTier {
	clone := t
	clone.StartMinute = cloneMinute(t.StartMinute)
	clone.EndMinute = cloneMinute(t.EndMinute)
	clone.LocalMinute = cloneMinute(t.LocalMinute)
	return clone
}

func cloneMinute(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func validWindowBounds(start, end int) bool {
	return start >= 0 && start < end && end <= minutesPerDay
}

// Validate checks the rule table itself. mode decides whether a window is
// allowed to price everything at zero: on a metered version an all-zero window
// would settle to a snapshot with no positive component, which the snapshot
// contract already refuses, so it is refused here where the operator can still
// see why.
func (s PriceSchedule) Validate(mode BillingMode) error {
	if err := validateIANAZoneName("price schedule timezone", s.Timezone); err != nil {
		return err
	}
	if len(s.Windows) == 0 {
		return errors.New("price schedule requires at least one window; omit the schedule for a single all-day rate")
	}
	if len(s.Windows) > maxPriceWindows {
		return fmt.Errorf("price schedule supports at most %d windows", maxPriceWindows)
	}
	previousEnd := 0
	for index, window := range s.Windows {
		if !validWindowBounds(window.StartMinute, window.EndMinute) {
			return fmt.Errorf("price schedule window %d must satisfy 0 <= start < end <= %d; a span across midnight is two windows", index, minutesPerDay)
		}
		// Sorted and disjoint is a stored property, not something selection
		// sorts out at read time: the version is immutable and its JSON is
		// hashed into the price snapshot, so one canonical order keeps the
		// digest of a given rule table stable.
		if window.StartMinute < previousEnd {
			return fmt.Errorf("price schedule window %d overlaps the previous window or is out of order", index)
		}
		previousEnd = window.EndMinute
		if window.InputMicrosPerMillion < 0 || window.CachedInputMicrosPerMillion < 0 ||
			window.OutputMicrosPerMillion < 0 || window.FixedRequestMicrosUSD < 0 {
			return fmt.Errorf("price schedule window %d cannot carry a negative rate", index)
		}
		if mode == BillingModeMetered && window.InputMicrosPerMillion == 0 && window.CachedInputMicrosPerMillion == 0 &&
			window.OutputMicrosPerMillion == 0 && window.FixedRequestMicrosUSD == 0 {
			return fmt.Errorf("price schedule window %d prices a metered deployment at zero", index)
		}
	}
	return nil
}

// AuditSummary renders the rule table for a pricing audit record. A schedule
// that only the request path can see is a hidden rate change, so the audit
// trail carries the windows themselves and not just the fact that some rule
// exists. The nil receiver is the ordinary case — a price with no schedule —
// and renders as nothing.
func (s *PriceSchedule) AuditSummary() string {
	if s == nil {
		return ""
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, " schedule={tz:%s,windows:[", s.Timezone)
	for index, window := range s.Windows {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, "%d-%d:%d/%d/%d/%d", window.StartMinute, window.EndMinute,
			window.InputMicrosPerMillion, window.CachedInputMicrosPerMillion,
			window.OutputMicrosPerMillion, window.FixedRequestMicrosUSD)
	}
	builder.WriteString("]}")
	return builder.String()
}

func (s PriceSchedule) Clone() PriceSchedule {
	clone := s
	if s.Windows != nil {
		clone.Windows = make([]PriceWindow, len(s.Windows))
		copy(clone.Windows, s.Windows)
	}
	return clone
}

// TierAt resolves the rates that apply to an attempt pinned at selectedAt.
//
// It never fails. A schedule whose zone cannot be resolved does not stop the
// attempt from being priced — it is billed at the highest rate the version can
// express, term by term. Refusing would take the deployment down over a missing
// zone file, and guessing an hour would under-bill silently; an amount that is
// too high is visible to whoever reads the usage, and it is the only direction
// that cannot quietly lose money. The same rung answers the gap between
// windows, except that there the answer is decidable and is simply the base.
func (p DeploymentPriceVersion) TierAt(selectedAt time.Time) PriceTier {
	base := PriceTier{
		InputMicrosPerMillion:       p.InputMicrosPerMillion,
		CachedInputMicrosPerMillion: p.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion:      p.OutputMicrosPerMillion,
		FixedRequestMicrosUSD:       p.FixedRequestMicrosUSD,
	}
	if p.Schedule == nil {
		return base
	}
	location, err := time.LoadLocation(p.Schedule.Timezone)
	if err != nil {
		tier := p.highestTier()
		tier.Provenance = &PriceScheduleTier{Timezone: p.Schedule.Timezone, Source: PriceTierZoneUnavailable}
		return tier
	}
	local := selectedAt.In(location)
	minute := local.Hour()*60 + local.Minute()
	for _, window := range p.Schedule.Windows {
		if minute < window.StartMinute || minute >= window.EndMinute {
			continue
		}
		start, end := window.StartMinute, window.EndMinute
		return PriceTier{
			InputMicrosPerMillion:       window.InputMicrosPerMillion,
			CachedInputMicrosPerMillion: window.CachedInputMicrosPerMillion,
			OutputMicrosPerMillion:      window.OutputMicrosPerMillion,
			FixedRequestMicrosUSD:       window.FixedRequestMicrosUSD,
			Provenance: &PriceScheduleTier{
				Timezone: p.Schedule.Timezone, Source: PriceTierWindow,
				StartMinute: &start, EndMinute: &end, LocalMinute: &minute,
			},
		}
	}
	base.Provenance = &PriceScheduleTier{Timezone: p.Schedule.Timezone, Source: PriceTierBase, LocalMinute: &minute}
	return base
}

// highestTier is the per-term maximum across the base rates and every window.
// Taking the maximum of each term rather than of some tier's total is what
// makes the result at least as expensive as any rung the attempt could have
// landed on, whatever mix of prompt, cached prompt and completion tokens it
// turns out to have.
func (p DeploymentPriceVersion) highestTier() PriceTier {
	highest := PriceTier{
		InputMicrosPerMillion:       p.InputMicrosPerMillion,
		CachedInputMicrosPerMillion: p.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion:      p.OutputMicrosPerMillion,
		FixedRequestMicrosUSD:       p.FixedRequestMicrosUSD,
	}
	if p.Schedule == nil {
		return highest
	}
	for _, window := range p.Schedule.Windows {
		highest.InputMicrosPerMillion = max(highest.InputMicrosPerMillion, window.InputMicrosPerMillion)
		highest.CachedInputMicrosPerMillion = max(highest.CachedInputMicrosPerMillion, window.CachedInputMicrosPerMillion)
		highest.OutputMicrosPerMillion = max(highest.OutputMicrosPerMillion, window.OutputMicrosPerMillion)
		highest.FixedRequestMicrosUSD = max(highest.FixedRequestMicrosUSD, window.FixedRequestMicrosUSD)
	}
	return highest
}
