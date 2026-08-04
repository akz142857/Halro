package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
