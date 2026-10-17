package ledger

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// hotIdent is an identifier of production length.
func hotIdent(prefix string, index int) string { return fmt.Sprintf("%s_%026d", prefix, index) }

// hotReservation is a reservation carrying every field the gateway sets,
// at production lengths, metered or with unknown-price evidence.
func hotReservation(index int, at time.Time, unknown bool) []Event {
	project := hotIdent("prj", 0)
	accepted := governanceEvent(hotIdent("evtacc", index), EventRequestAccepted, at)
	accepted.ProjectID, accepted.KeyID, accepted.RequestID = project, hotIdent("key", 0), hotIdent("req", index)
	accepted.RequestedModel = "claude-sonnet-4-5-20250929-production-alias"
	reserved := accepted
	reserved.EventID, reserved.Kind = hotIdent("evtres", index), EventReservationCreated
	reserved.AttemptID = hotIdent("att", index)
	reserved.RouteID, reserved.DeploymentID, reserved.ProviderID = hotIdent("rte", 0), hotIdent("dep", 0), hotIdent("prv", 0)
	identity, _ := domain.IdentityForProfile(domain.ProfileAnthropicMessages)
	reserved.OfferingID, reserved.ProfileID = identity.Offering, domain.ProfileAnthropicMessages
	reserved.ProviderModel, reserved.ProviderPrimitive = "anthropic.claude-sonnet-4-5-20250929-v1:0", "messages"
	reserved.AttemptNumber, reserved.PreparedInputTokens, reserved.PreparedOutputTokens = 1, 12_000, 4_096
	reserved.RecoveryKey = fmt.Sprintf("sha256:%064x", index)
	reserved.TokenGuardPricingViewDigest = fmt.Sprintf("sha256:%064x", index+1)
	version, start, end, local := uint64(7), 480, 1080, 600
	rate := func(value int64) *int64 { return &value }
	price := domain.PriceSnapshot{
		PricingSelectedAt: at, PriceEvidenceStatus: domain.PriceEvidenceVersioned, CostValueStatus: domain.CostValueKnown,
		PriceVersionID: hotIdent("dpv", 0), PriceVersion: &version, BillingMode: domain.BillingModeMetered, Currency: "USD",
		FormulaVersion: domain.PriceFormulaUSDTokensV1, InputMicrosPerMillion: rate(3_000_000), CachedInputMicrosPerMillion: rate(300_000),
		OutputMicrosPerMillion: rate(15_000_000), FixedRequestMicrosUSD: rate(0),
		ScheduleTier:  &domain.PriceScheduleTier{Timezone: "America/Los_Angeles", Source: domain.PriceTierWindow, StartMinute: &start, EndMinute: &end, LocalMinute: &local},
		EffectiveFrom: &at, SourceType: domain.PriceSourceOfficialURL, SourceAssurance: domain.PriceAssuranceAsserted,
		SourceContentSHA256: fmt.Sprintf("sha256:%064x", 7),
		SourceReference:     "https://www.anthropic.com/pricing#api-claude-sonnet-4-5-production-reference",
	}
	reserved.LeaseMode, reserved.PriceSnapshot, reserved.ReservationMicrosUSD = LeaseModeMetered, &price, MicrosUSD(100)
	if unknown {
		price = domain.NewUnknownPriceSnapshot(at)
		reserved.LeaseMode, reserved.PriceSnapshot, reserved.ReservationMicrosUSD = LeaseModeUnknownAllowed, &price, nil
		reserved.UnknownPolicyEvidence = &domain.UnknownPricePolicyEvidence{
			PolicyVersion: "unknown-price-policy-v1", ProjectID: project, TokenGuardStatus: "disabled",
			ReasonCode: "cost_governance_disabled", InstanceExplicitOptIn: true, CostGovernanceDisabled: true,
		}
	}
	return []Event{accepted, reserved}
}

// hotHeap is the live heap after collection. What is measured must still be
// reachable when it is read — runtime.KeepAlive after the reading — or the
// reading is of the heap without it.
func hotHeap() int64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// The hot state's estimate is priced from what each kind of entry actually
// holds in the heap. Each kind is added many times to a state whose digest
// window is already full, and the estimate's growth must stay within 25% of
// the heap's. A price that drifted from the code fails here.
func TestHotStateEstimateTracksTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	const count = 10_000
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	event := func(kind EventKind, at time.Time, name string, index int) Event {
		e := governanceEvent(hotIdent("evt"+name, index), kind, at)
		e.ProjectID, e.KeyID, e.Operation = hotIdent("prj", 0), hotIdent("key", 0), name
		e.IdempotencyKeyHash = fmt.Sprintf("sha256:%064x", index)
		return e
	}
	workUnit := func(index int) Event {
		created := event(EventWorkUnitCreated, base, "wu.create", index)
		created.WorkUnitID = hotIdent("wku", index)
		return created
	}
	run := func(index, workUnitIndex int) Event {
		created := event(EventRunCreated, base, "run.create", index)
		created.IdempotencyKeyHash = fmt.Sprintf("sha256:%064x", 1<<40+index)
		created.WorkUnitID, created.RunID = hotIdent("wku", workUnitIndex), hotIdent("run", index)
		created.RunBudgetMicrosUSD, created.RunExpiresAt = 1_000, base.Add(time.Hour)
		return created
	}
	kinds := []struct {
		name   string
		setup  []Event
		events func(index int) []Event
	}{
		{name: "metered reservation", events: func(index int) []Event { return hotReservation(index, base, false) }},
		{name: "unknown-price reservation", events: func(index int) []Event { return hotReservation(index, base, true) }},
		{name: "lease kept after settlement", events: func(index int) []Event {
			events := hotReservation(index, base, false)
			settled := events[1]
			settled.EventID, settled.Kind, settled.Outcome = hotIdent("evtset", index), EventAttemptSettled, OutcomePinCommitFailed
			settled.ReservationMicrosUSD, settled.CommittedMicrosUSD = nil, MicrosUSD(75)
			return append(events, settled)
		}},
		{name: "attributed request", setup: []Event{workUnit(0), run(0, 0)}, events: func(index int) []Event {
			accepted := event(EventRequestAccepted, base, "acc", index)
			accepted.RequestID, accepted.WorkUnitID, accepted.RunID = hotIdent("req", index), hotIdent("wku", 0), hotIdent("run", 0)
			return []Event{accepted}
		}},
		{name: "open work unit", events: func(index int) []Event { return []Event{workUnit(index)} }},
		{name: "open work unit with a run", events: func(index int) []Event { return []Event{workUnit(index), run(index, index)} }},
		{name: "balance", events: func(index int) []Event {
			at := base.Add(time.Duration(index%365) * 24 * time.Hour)
			events := hotReservation(index, at, false)
			for position := range events {
				events[position].ProjectID = hotIdent("prj", index/365+1)
				events[position].PeriodID = at.Format("2006-01-02")
			}
			settled := events[1]
			settled.EventID, settled.Kind = hotIdent("evtset", index), EventAttemptSettled
			settled.ReservationMicrosUSD, settled.CommittedMicrosUSD = nil, MicrosUSD(75)
			finalized := events[0]
			finalized.EventID, finalized.Kind = hotIdent("evtfin", index), EventRequestFinalized
			return append(events, settled, finalized)
		}},
		{name: "archived work unit awaiting the archive", events: func(index int) []Event { return governedTask(index, base, false) }},
	}
	for _, kind := range kinds {
		state := NewState()
		state.SetArchive(newMemoryArchive())
		sequence := uint64(1)
		for index := 0; index < recentEventDigests; index++ {
			warm := event(EventRequestAccepted, base, "warm", index)
			warm.ProjectID, warm.RequestID = hotIdent("prw", 0), hotIdent("rqw", index)
			sequence = applyEvents(t, state, []Event{warm}, sequence)
		}
		sequence = applyEvents(t, state, kind.setup, sequence)
		heapBefore, estimateBefore := hotHeap(), state.HotStateBytes(0)
		for index := 1; index <= count; index++ {
			sequence = applyEvents(t, state, kind.events(index), sequence)
		}
		heapAfter, estimateAfter := hotHeap(), state.HotStateBytes(0)
		runtime.KeepAlive(state)
		heap, estimate := float64(heapAfter-heapBefore)/count, float64(estimateAfter-estimateBefore)/count
		t.Logf("%-40s heap %5.0f B  estimate %5.0f B  (%.2f)", kind.name, heap, estimate, estimate/heap)
		if estimate < 0.75*heap || estimate > 1.25*heap {
			t.Errorf("%s: estimate %.0f B per entry is not within 25%% of the heap's %.0f B", kind.name, estimate, heap)
		}
	}
}

// The digest window is priced once, as the heap it holds when full.
func TestHotStateEstimatePricesTheDigestWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	heapBefore := hotHeap()
	sequence := uint64(1)
	for index := 0; index < recentEventDigests; index++ {
		warm := governanceEvent(hotIdent("evtwarm", index), EventRequestAccepted, base)
		warm.RequestID = hotIdent("rqw", index)
		sequence = applyEvents(t, state, []Event{warm}, sequence)
	}
	heap, estimate := float64(hotHeap()-heapBefore), float64(state.HotStateBytes(0))
	runtime.KeepAlive(state)
	t.Logf("digest window heap %.1f MiB  estimate %.1f MiB", heap/(1<<20), estimate/(1<<20))
	if estimate < 0.75*heap || estimate > 1.25*heap {
		t.Errorf("digest window estimate %.0f B is not within 25%% of the heap's %.0f B", estimate, heap)
	}
}

// A burst of reservations that settles leaves no slots behind: the maps it
// filled are rebuilt once they empty out, so the heap falls back with the
// estimate instead of keeping the burst's size while the estimate says zero.
func TestASettledBurstGivesItsMemoryBack(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	sequence := uint64(1)
	for index := 0; index < recentEventDigests; index++ {
		warm := governanceEvent(hotIdent("evtwarm", index), EventRequestAccepted, base)
		warm.RequestID = hotIdent("rqw", index)
		sequence = applyEvents(t, state, []Event{warm}, sequence)
	}
	heapBefore := hotHeap()
	const burst = 40_000
	for index := 1; index <= burst; index++ {
		sequence = applyEvents(t, state, hotReservation(index, base, false), sequence)
	}
	heapPeak := hotHeap()
	for index := 1; index <= burst; index++ {
		events := hotReservation(index, base, false)
		settled := events[1]
		settled.EventID, settled.Kind = hotIdent("evtset", index), EventAttemptSettled
		settled.ReservationMicrosUSD, settled.CommittedMicrosUSD = nil, MicrosUSD(75)
		finalized := events[0]
		finalized.EventID, finalized.Kind = hotIdent("evtfin", index), EventRequestFinalized
		sequence = applyEvents(t, state, []Event{settled, finalized}, sequence)
	}
	retained := hotHeap() - heapBefore
	runtime.KeepAlive(state)
	t.Logf("burst peak +%.1f MiB, retained after settling %.2f MiB", float64(heapPeak-heapBefore)/(1<<20), float64(retained)/(1<<20))
	// The digest window holds the newest events whatever happens; what is
	// left beyond it is the balance row and slack, far under the 3.3 MiB the
	// emptied maps kept before they were rebuilt.
	if retained > 1<<20 {
		t.Fatalf("a settled burst left %d bytes behind", retained)
	}
}
