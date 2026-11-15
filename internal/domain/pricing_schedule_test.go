package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// deepSeekShapedPrice is the case that made time-of-day pricing worth building:
// peak from 09:00 to 12:00 and 14:00 to 18:00 Beijing time, off-peak elsewhere
// at half the rate. Off-peak is the version's own rates, so the windows carry
// the more expensive rung.
func deepSeekShapedPrice(t *testing.T) DeploymentPriceVersion {
	t.Helper()
	price := validPriceVersion(t, "price_schedule", 1, "2026-08-01T00:00:00Z")
	price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion = 200_000, 20_000
	price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD = 800_000, 0
	price.Schedule = &PriceSchedule{
		Timezone: "Asia/Shanghai",
		Windows: []PriceWindow{
			{StartMinute: 9 * 60, EndMinute: 12 * 60, InputMicrosPerMillion: 400_000,
				CachedInputMicrosPerMillion: 40_000, OutputMicrosPerMillion: 1_600_000},
			{StartMinute: 14 * 60, EndMinute: 18 * 60, InputMicrosPerMillion: 400_000,
				CachedInputMicrosPerMillion: 40_000, OutputMicrosPerMillion: 1_600_000},
		},
	}
	if err := price.Validate(); err != nil {
		t.Fatal(err)
	}
	return price
}

func TestPriceScheduleSelectsTierInTheProviderZone(t *testing.T) {
	price := deepSeekShapedPrice(t)
	// 02:00Z is 10:00 in Shanghai and inside the morning peak; 05:00Z is 13:00
	// and in the lunch gap no window covers, which is the base rate.
	peak := price.TierAt(time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC))
	if peak.InputMicrosPerMillion != 400_000 || peak.Provenance == nil || peak.Provenance.Source != PriceTierWindow {
		t.Fatalf("peak tier = %+v", peak)
	}
	if peak.Provenance.StartMinute == nil || *peak.Provenance.StartMinute != 9*60 {
		t.Fatalf("peak window bounds = %+v", peak.Provenance)
	}
	gap := price.TierAt(time.Date(2026, 8, 18, 5, 0, 0, 0, time.UTC))
	if gap.InputMicrosPerMillion != 200_000 || gap.Provenance == nil || gap.Provenance.Source != PriceTierBase {
		t.Fatalf("gap tier = %+v", gap)
	}
	// The boundary is half-open: 12:00 local belongs to the gap, not the peak.
	boundary := price.TierAt(time.Date(2026, 8, 18, 4, 0, 0, 0, time.UTC))
	if boundary.InputMicrosPerMillion != 200_000 {
		t.Fatalf("12:00 local should leave the peak window, got %d", boundary.InputMicrosPerMillion)
	}
}

// The accounting timezone decides where a billing period starts and has no say
// in which rate applies. Nothing in tier selection reads it — this pins that,
// so an instance keeping its books in UTC still bills Beijing peak hours.
func TestPriceScheduleIgnoresTheAccountingTimezone(t *testing.T) {
	price := deepSeekShapedPrice(t)
	instant := time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)
	expected := price.TierAt(instant)
	for _, accounting := range []string{"UTC", "America/New_York", "Asia/Shanghai"} {
		if err := ValidateAccountingTimezone(accounting); err != nil {
			t.Fatal(err)
		}
		if got := price.TierAt(instant.In(time.UTC)); got.InputMicrosPerMillion != expected.InputMicrosPerMillion {
			t.Fatalf("accounting zone %q changed the tier", accounting)
		}
	}
}

// A settlement's snapshot must equal its reservation's byte for byte, so the
// rung is frozen when the reservation is taken and never re-read from a clock.
func TestPriceScheduleSnapshotPinsTheReservationTier(t *testing.T) {
	price := deepSeekShapedPrice(t)
	reservedAt := time.Date(2026, 8, 18, 3, 59, 0, 0, time.UTC) // 11:59 Shanghai, peak
	snapshot, err := NewVersionedPriceSnapshot(price, reservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if *snapshot.InputMicrosPerMillion != 400_000 {
		t.Fatalf("snapshot took the wrong rung: %d", *snapshot.InputMicrosPerMillion)
	}
	// The attempt finishes two minutes later, after the boundary. Settlement
	// prices from the snapshot, which still carries the peak rung.
	settled, err := snapshot.Calculate(BilledTokens{Input: 1_000_000, CachedInput: 0, Output: 0})
	if err != nil {
		t.Fatal(err)
	}
	if settled.TotalCostMicrosUSD != 400_000 {
		t.Fatalf("settled cost = %d, want the reserved peak rate", settled.TotalCostMicrosUSD)
	}
	// Re-deriving from the version and the pinned instant reproduces it exactly,
	// which is what the backup validator and replay both depend on.
	rederived, err := NewVersionedPriceSnapshot(price, reservedAt)
	if err != nil {
		t.Fatal(err)
	}
	left, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	right, err := rederived.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("re-derived snapshot digest differs: %s vs %s", left, right)
	}
}

// A price with no schedule has to serialise exactly as it did before schedules
// existed, or every snapshot written by the previous build would stop matching
// its re-derivation.
func TestFixedPriceSnapshotCarriesNoScheduleTier(t *testing.T) {
	price := validPriceVersion(t, "price_fixed", 1, "2026-08-01T00:00:00Z")
	price.CachedInputMicrosPerMillion = 40_000
	snapshot, err := NewVersionedPriceSnapshot(price, time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ScheduleTier != nil {
		t.Fatalf("fixed price produced a schedule tier: %+v", snapshot.ScheduleTier)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "schedule_tier") {
		t.Fatalf("fixed price snapshot gained a schedule_tier key: %s", encoded)
	}
}

// An unresolvable zone must not under-bill and must not take the deployment
// down. The rung it falls back to is at least as expensive as any rung the
// attempt could otherwise have landed on, for any mix of tokens.
func TestPriceScheduleFallsBackToTheDearestTierWhenTheZoneIsUnknown(t *testing.T) {
	price := deepSeekShapedPrice(t)
	// Constructed directly rather than through Validate: a zone this broken
	// cannot be stored, and the case being covered is a rule table that was
	// valid when written and whose zone the running binary can no longer load.
	price.Schedule.Timezone = "Mars/Olympus_Mons"
	tier := price.TierAt(time.Date(2026, 8, 18, 5, 0, 0, 0, time.UTC))
	if tier.Provenance == nil || tier.Provenance.Source != PriceTierZoneUnavailable {
		t.Fatalf("tier = %+v, want the zone-unavailable rung", tier)
	}
	if tier.Provenance.LocalMinute != nil {
		t.Fatal("the zone-unavailable rung must not claim a local time")
	}
	for _, tokens := range []BilledTokens{
		{Input: 1_000_000}, {Input: 1_000_000, CachedInput: 900_000}, {Output: 1_000_000}, {Input: 7, CachedInput: 3, Output: 11},
		{Input: 1_000_000, CacheWrite: 600_000, CacheWriteLong: 200_000}, {Input: 9, CachedInput: 2, CacheWrite: 5, CacheWriteLong: 5, Output: 3},
	} {
		fallback, err := tier.Calculate(tokens)
		if err != nil {
			t.Fatal(err)
		}
		candidates := []PriceTier{price.BaseTier()}
		for _, window := range price.Schedule.Windows {
			candidates = append(candidates, window.Tier())
		}
		for index, candidate := range candidates {
			cost, err := candidate.Calculate(tokens)
			if err != nil {
				t.Fatal(err)
			}
			if fallback.TotalCostMicrosUSD < cost.TotalCostMicrosUSD {
				t.Fatalf("fallback %d under-bills tier %d (%d) for %v", fallback.TotalCostMicrosUSD, index, cost.TotalCostMicrosUSD, tokens)
			}
		}
	}
}

func TestPriceScheduleValidationRejectsMalformedTables(t *testing.T) {
	cases := map[string]func(*PriceSchedule){
		"empty table":       func(s *PriceSchedule) { s.Windows = nil },
		"unknown zone":      func(s *PriceSchedule) { s.Timezone = "Mars/Olympus_Mons" },
		"fixed offset zone": func(s *PriceSchedule) { s.Timezone = "UTC+08:00" },
		"host local zone":   func(s *PriceSchedule) { s.Timezone = "Local" },
		"overlap":           func(s *PriceSchedule) { s.Windows[1].StartMinute = 11 * 60 },
		"out of order":      func(s *PriceSchedule) { s.Windows[0], s.Windows[1] = s.Windows[1], s.Windows[0] },
		"crosses midnight":  func(s *PriceSchedule) { s.Windows[1].EndMinute = 26 * 60 },
		"inverted window":   func(s *PriceSchedule) { s.Windows[0].EndMinute = s.Windows[0].StartMinute },
		"negative rate":     func(s *PriceSchedule) { s.Windows[0].OutputMicrosPerMillion = -1 },
		"metered zero rung": func(s *PriceSchedule) { s.Windows[0] = PriceWindow{StartMinute: 9 * 60, EndMinute: 12 * 60} },
		"too many windows": func(s *PriceSchedule) {
			s.Windows = make([]PriceWindow, maxPriceWindows+1)
			for index := range s.Windows {
				s.Windows[index] = PriceWindow{StartMinute: index, EndMinute: index + 1, InputMicrosPerMillion: 1}
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			price := deepSeekShapedPrice(t)
			mutate(price.Schedule)
			if err := price.Validate(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestFreePriceCannotCarryASchedule(t *testing.T) {
	price := deepSeekShapedPrice(t)
	price.BillingMode = BillingModeFree
	price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion = 0, 0
	price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD = 0, 0
	if err := price.Validate(); err == nil {
		t.Fatal("a free price accepted a time-of-day schedule")
	}
}

// The window bounds and the local minute in a snapshot have to agree with each
// other, so a hand-edited or truncated record cannot claim a rung it does not
// describe.
func TestPriceScheduleTierValidationRejectsIncoherentProvenance(t *testing.T) {
	minute := func(value int) *int { return &value }
	cases := map[string]PriceScheduleTier{
		"window without bounds":     {Timezone: "Asia/Shanghai", Source: PriceTierWindow, LocalMinute: minute(600)},
		"local minute outside":      {Timezone: "Asia/Shanghai", Source: PriceTierWindow, StartMinute: minute(540), EndMinute: minute(720), LocalMinute: minute(800)},
		"base claiming bounds":      {Timezone: "Asia/Shanghai", Source: PriceTierBase, StartMinute: minute(540), EndMinute: minute(720), LocalMinute: minute(600)},
		"base claiming start only":  {Timezone: "Asia/Shanghai", Source: PriceTierBase, StartMinute: minute(540), LocalMinute: minute(600)},
		"base claiming end only":    {Timezone: "Asia/Shanghai", Source: PriceTierBase, EndMinute: minute(720), LocalMinute: minute(600)},
		"base without local minute": {Timezone: "Asia/Shanghai", Source: PriceTierBase},
		"fallback claiming a time":  {Timezone: "Asia/Shanghai", Source: PriceTierZoneUnavailable, LocalMinute: minute(600)},
		"fallback claiming start":   {Timezone: "Asia/Shanghai", Source: PriceTierZoneUnavailable, StartMinute: minute(540)},
		"fallback claiming end":     {Timezone: "Asia/Shanghai", Source: PriceTierZoneUnavailable, EndMinute: minute(720)},
		"unknown source":            {Timezone: "Asia/Shanghai", Source: "whenever"},
		"missing zone":              {Source: PriceTierBase, LocalMinute: minute(600)},
	}
	for name, tier := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tier.Validate(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestPriceScheduleAuditSummaryNamesTheWindows(t *testing.T) {
	price := deepSeekShapedPrice(t)
	summary := price.Schedule.AuditSummary()
	if !strings.Contains(summary, "Asia/Shanghai") || !strings.Contains(summary, "540-720") || !strings.Contains(summary, "840-1080") {
		t.Fatalf("audit summary omits the rule: %q", summary)
	}
	var absent *PriceSchedule
	if absent.AuditSummary() != "" {
		t.Fatal("a price with no schedule should add nothing to its audit record")
	}
}

// Records written before schedules existed have to decode unchanged and, more
// importantly, hash unchanged: a price pin persisted by the previous build
// stores its snapshot digest, and settlement compares that stored digest with
// one recomputed by this build. A field that widened the encoding — one not
// omitted when absent, say — would invalidate every pin in an existing data
// directory rather than merely look untidy.
func TestRecordsWrittenBeforeSchedulesDecodeAndHashUnchanged(t *testing.T) {
	priceJSON := `{"id":"price_legacy","deployment_id":"dep_one","version":3,"billing_mode":"metered",` +
		`"currency":"USD","formula_version":"usd_token_v1","input_micros_per_million":400000,` +
		`"cached_input_micros_per_million":40000,"output_micros_per_million":1600000,"fixed_request_micros_usd":0,` +
		`"effective_from":"2026-08-01T00:00:00Z","source":{"type":"manual","assurance":"asserted",` +
		`"received_at":"2026-08-01T00:00:00Z","reference":"standard","asserted_without_archive":true},` +
		`"created_by":"admin_one","created_at":"2026-08-01T00:00:00Z","revision":1}`
	var price DeploymentPriceVersion
	if err := json.Unmarshal([]byte(priceJSON), &price); err != nil {
		t.Fatal(err)
	}
	if price.Schedule != nil {
		t.Fatalf("a price with no schedule key decoded one: %#v", price.Schedule)
	}
	if err := price.Validate(); err != nil {
		t.Fatalf("an existing price stopped validating: %v", err)
	}
	// A snapshot a previous build wrote — the one a stored price pin's digest
	// was taken over — must re-encode to exactly the same bytes, or every pin in
	// an existing data directory would stop matching its Ledger lease. That
	// holds for snapshots written before schedules and before cache-write
	// rates alike, because both are omitted when absent.
	legacy := `{"pricing_selected_at":"2026-08-18T02:00:00Z","price_evidence_status":"versioned",` +
		`"cost_value_status":"known","price_version_id":"price_legacy","price_version":3,` +
		`"billing_mode":"metered","currency":"USD","formula_version":"usd_token_v1",` +
		`"input_micros_per_million":400000,"cached_input_micros_per_million":40000,` +
		`"output_micros_per_million":1600000,"fixed_request_micros_usd":0,` +
		`"effective_from":"2026-08-01T00:00:00Z","source_type":"manual","source_assurance":"asserted",` +
		`"source_reference":"standard","source_without_archive":true}`
	var restored PriceSnapshot
	if err := json.Unmarshal([]byte(legacy), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ScheduleTier != nil || restored.CacheWriteMicrosPerMillion != nil || restored.CacheWriteLongMicrosPerMillion != nil {
		t.Fatalf("a legacy snapshot decoded terms it does not carry: %#v", restored)
	}
	reencoded, err := json.Marshal(restored.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != legacy {
		t.Fatalf("legacy snapshot encoding changed:\n got %s\nwant %s", reencoded, legacy)
	}
	// A snapshot taken now from the same price carries both write terms: a
	// price stored before they existed decodes them as zero until the schema
	// 43 migration fills them with the input rate.
	selectedAt := time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)
	snapshot, err := NewVersionedPriceSnapshot(price, selectedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CacheWriteMicrosPerMillion == nil || snapshot.CacheWriteLongMicrosPerMillion == nil {
		t.Fatal("a new snapshot omitted the cache-write terms")
	}
	// And the legacy snapshot is still priceable, at the same rung it was
	// written with, its absent write terms reading as zero.
	cost, err := restored.Calculate(BilledTokens{Input: 1_000_000, CachedInput: 0, Output: 0})
	if err != nil || cost.TotalCostMicrosUSD != 400_000 {
		t.Fatalf("cost=%d err=%v", cost.TotalCostMicrosUSD, err)
	}
}

func TestUnavailableZoneBoundsIndependentlyRoundedWriteTiers(t *testing.T) {
	price := deepSeekShapedPrice(t)
	price.InputMicrosPerMillion = 400_000
	price.CacheWriteMicrosPerMillion = 400_000
	price.CacheWriteLongMicrosPerMillion = 400_000
	price.Schedule = &PriceSchedule{Timezone: "Mars/Olympus_Mons", Windows: []PriceWindow{{
		StartMinute: 60, EndMinute: 120,
		InputMicrosPerMillion: 100_000, CacheWriteMicrosPerMillion: 300_000, CacheWriteLongMicrosPerMillion: 350_000,
	}}}
	tokens := BilledTokens{Input: 3, CacheWrite: 2, CacheWriteLong: 1}
	fallback := price.TierAt(priceTestInstant)
	cost, err := fallback.Calculate(tokens)
	if err != nil || cost.InputCostMicrosUSD != 3 || !fallback.Provenance.SeparateCacheWriteRounding {
		t.Fatalf("fallback=%+v cost=%+v err=%v", fallback, cost, err)
	}
	want, err := price.Schedule.Windows[0].Tier().Calculate(tokens)
	if err != nil || cost.InputCostMicrosUSD < want.InputCostMicrosUSD {
		t.Fatalf("fallback=%+v window=%+v err=%v", cost, want, err)
	}
}
