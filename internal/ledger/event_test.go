package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

func TestReservationWireFormatDistinguishesKnownZeroFromUnknown(t *testing.T) {
	free, err := json.Marshal(Event{ReservationMicrosUSD: MicrosUSD(0)})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := json.Marshal(Event{ReservationMicrosUSD: nil})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(free), `"reservation_micros_usd":0`) || !strings.Contains(string(unknown), `"reservation_micros_usd":null`) {
		t.Fatalf("free=%s unknown=%s", free, unknown)
	}
}

func TestSettlementWireFormatDistinguishesKnownZeroFromUnknown(t *testing.T) {
	free, err := json.Marshal(Event{CommittedMicrosUSD: MicrosUSD(0)})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := json.Marshal(Event{CommittedMicrosUSD: nil})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(free), `"committed_micros_usd":0`) || !strings.Contains(string(unknown), `"committed_micros_usd":null`) {
		t.Fatalf("free=%s unknown=%s", free, unknown)
	}
}

func TestStateDuplicateEventIsIdempotentAndAdvancesWatermark(t *testing.T) {
	state := NewState()
	event := Event{
		EventID: "evt_stable", Kind: EventRequestAccepted, RequestID: "req_1",
		ProjectID: "prj_1", PeriodID: "2026-08-04", OccurredAt: time.Unix(1, 0).UTC(),
	}
	if err := state.Apply(Record{Sequence: 1, Offset: 100, Event: event}); err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(Record{Sequence: 2, Offset: 200, Event: event}); err != nil {
		t.Fatal(err)
	}
	if got := state.Watermark(); got.Sequence != 2 || got.Offset != 200 {
		t.Fatalf("watermark=%#v", got)
	}
	changed := event
	changed.Outcome = "different"
	if err := state.Apply(Record{Sequence: 3, Offset: 300, Event: changed}); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("expected conflicting duplicate rejection, got %v", err)
	}
}

// The bound is enforced at the durable boundary as well as at the gateway that
// already narrowed the value. The gateway is one caller; the ledger is the
// record, and an event assembled by a recovery path or by a caller added later
// must not be able to make an unbounded upstream string permanent.
//
// Rejected rather than truncated: a shortened identifier is a different
// identifier, and one that would be quoted to an upstream that never issued it.
func TestAnOversizedProviderIdentifierIsRefusedAtTheDurableBoundary(t *testing.T) {
	base := Event{
		EventID: "event_1", Kind: EventAttemptSettled, RequestID: "req_1",
		AttemptID: "att_1", ProjectID: "project_1", PeriodID: "2026-08-22",
		OccurredAt:         time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
		CommittedMicrosUSD: MicrosUSD(1),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("the baseline event was refused: %v", err)
	}

	atBound := strings.Repeat("a", provider.MaxProviderIdentifierLength)
	accepted := base
	accepted.ProviderCode, accepted.ProviderRequestID = atBound, atBound
	if err := accepted.Validate(); err != nil {
		t.Fatalf("an identifier at the bound was refused: %v", err)
	}

	for name, mutate := range map[string]func(*Event){
		"provider code":       func(e *Event) { e.ProviderCode = atBound + "a" },
		"provider request ID": func(e *Event) { e.ProviderRequestID = atBound + "a" },
	} {
		t.Run(name, func(t *testing.T) {
			event := base
			mutate(&event)
			if err := event.Validate(); err == nil {
				t.Fatal("an oversized identifier was accepted into the ledger")
			}
		})
	}
}
