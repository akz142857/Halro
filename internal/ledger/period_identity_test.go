package ledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// chargeRecords produces the reservation and settlement a single charge is made
// of. A settlement is only valid against a reservation, and both must carry the
// same period for the balance to move as one.
func chargeRecords(sequence uint64, periodID, zone string, version uint64, committed int64) []Record {
	start := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	attemptID := "att_" + string(rune('0'+sequence))
	stamp := func(event *Event) {
		event.PeriodID = periodID
		if zone == "" {
			return
		}
		event.PeriodTimezone = zone
		event.PeriodTimezoneVersion = version
		event.PeriodStartMicros = start.UnixMicro()
		event.PeriodEndMicros = start.Add(24 * time.Hour).UnixMicro()
	}
	reservation := Event{
		EventID: "evt_res_" + attemptID, Kind: EventReservationCreated,
		RequestID: "req_" + attemptID, AttemptID: attemptID, ProjectID: "prj_1",
		OccurredAt: start, ReservationMicrosUSD: MicrosUSD(committed),
	}
	settlement := Event{
		EventID: "evt_set_" + attemptID, Kind: EventAttemptSettled,
		RequestID: "req_" + attemptID, AttemptID: attemptID, ProjectID: "prj_1",
		OccurredAt: start, CommittedMicrosUSD: MicrosUSD(committed), Outcome: "success",
	}
	stamp(&reservation)
	stamp(&settlement)
	base := sequence * 2
	return []Record{
		{Sequence: base, Offset: int64(base) * 100, Epoch: eventFrameVersion(reservation), Event: reservation},
		{Sequence: base + 1, Offset: int64(base+1) * 100, Epoch: eventFrameVersion(settlement), Event: settlement},
	}
}

func settledEvent(sequence uint64, periodID, zone string, version uint64, committed int64) Record {
	return chargeRecords(sequence, periodID, zone, version, committed)[1]
}

// The reason the version is in the key. "2026-08-06" is a different interval in
// every zone, so charges measured against the old boundary and the new one must
// not pool into one balance — that is how a budget gets overspent or wiped with
// nothing in the ledger to explain it.
func TestBalanceIsolatesPeriodsAcrossATimezoneChange(t *testing.T) {
	state := NewState()
	records := append(
		chargeRecords(1, "2026-08-06", "Asia/Shanghai", 1, 400),
		chargeRecords(2, "2026-08-06", "Europe/Berlin", 2, 700)...,
	)
	for _, record := range records {
		if err := state.Apply(record); err != nil {
			t.Fatalf("apply %d: %v", record.Sequence, err)
		}
	}
	if got := state.Balance("prj_1", "2026-08-06", 1).CommittedMicrosUSD; got != 400 {
		t.Fatalf("version 1 balance = %d, want 400", got)
	}
	if got := state.Balance("prj_1", "2026-08-06", 2).CommittedMicrosUSD; got != 700 {
		t.Fatalf("version 2 balance = %d, want 700; the two zones' days merged", got)
	}
}

// Events written before the timezone became a governed setting carry no version
// and must stay in their own balance rather than joining a governed one.
func TestBalanceKeepsUnversionedEventsApart(t *testing.T) {
	state := NewState()
	records := append(
		chargeRecords(1, "2026-08-06", "", 0, 250),
		chargeRecords(2, "2026-08-06", "UTC", 1, 900)...,
	)
	for _, record := range records {
		if err := state.Apply(record); err != nil {
			t.Fatalf("apply %d: %v", record.Sequence, err)
		}
	}
	if got := state.Balance("prj_1", "2026-08-06", 0).CommittedMicrosUSD; got != 250 {
		t.Fatalf("unversioned balance = %d, want 250", got)
	}
	if got := state.Balance("prj_1", "2026-08-06", 1).CommittedMicrosUSD; got != 900 {
		t.Fatalf("versioned balance = %d, want 900", got)
	}
}

// An auditor reading one record must be able to state the interval it was
// charged against, without access to any setting.
func TestPeriodIdentitySurvivesEncoding(t *testing.T) {
	record := settledEvent(1, "2026-08-06", "Asia/Shanghai", 3, 100)
	if record.Epoch != frameVersionRunAttribution {
		t.Fatalf("epoch = %d, want %d", record.Epoch, frameVersionRunAttribution)
	}
	event := record.Event
	if event.PeriodTimezone != "Asia/Shanghai" || event.PeriodTimezoneVersion != 3 {
		t.Fatalf("period identity = %#v", event)
	}
	start := time.UnixMicro(event.PeriodStartMicros).UTC()
	end := time.UnixMicro(event.PeriodEndMicros).UTC()
	if !end.After(start) {
		t.Fatalf("period [%s,%s) is empty", start, end)
	}
}

// eventFrameVersion no longer branches by event shape (ADR 0016): every
// event this build writes — with or without period identity, lease mode, a
// price snapshot — is promoted to the same epoch, so there is no event shape
// that produces a lesser-authenticated frame.
func TestFrameVersionIsUnconditionallyRunAttribution(t *testing.T) {
	for _, event := range []Event{
		{Kind: EventAttemptSettled, LeaseMode: LeaseModeMetered},
		{Kind: EventRequestAccepted},
		{Kind: EventRequestAccepted, PeriodTimezone: "UTC"},
	} {
		if got := eventFrameVersion(event); got != frameVersionRunAttribution {
			t.Fatalf("event %#v = epoch %d, want %d", event, got, frameVersionRunAttribution)
		}
	}
}

// The reader was taught to accept epoch 4 and to reject a frame missing its
// period identity. Neither branch means anything until a real frame has
// actually been through a file: the in-memory checks above never touch
// encoding.
func TestPeriodIdentitySurvivesTheWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "period.wal")
	log := openChained(t, path, NewStatus())
	start := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	written := Event{
		EventID: "evt_period", Kind: EventRequestAccepted, RequestID: "req_1", ProjectID: "prj_1",
		PeriodID: "2026-08-06", PeriodTimezone: "Asia/Shanghai", PeriodTimezoneVersion: 7,
		PeriodStartMicros: start.UnixMicro(), PeriodEndMicros: start.Add(24 * time.Hour).UnixMicro(),
		OccurredAt: start,
	}
	if _, err := log.Append(context.Background(), written); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	var records []Record
	if _, partial, err := InspectReplay(path, func(record Record) error {
		records = append(records, record)
		return nil
	}); err != nil || partial {
		t.Fatalf("replay partial=%t err=%v", partial, err)
	}
	if len(records) != 1 {
		t.Fatalf("replayed %d records, want 1", len(records))
	}
	if records[0].Epoch != frameVersionRunAttribution {
		t.Fatalf("period event replayed at epoch %d, want %d", records[0].Epoch, frameVersionRunAttribution)
	}
	replayed := records[0].Event
	if replayed.PeriodTimezone != written.PeriodTimezone ||
		replayed.PeriodTimezoneVersion != written.PeriodTimezoneVersion ||
		replayed.PeriodStartMicros != written.PeriodStartMicros ||
		replayed.PeriodEndMicros != written.PeriodEndMicros {
		t.Fatalf("period identity did not survive the round trip: %#v", replayed)
	}
}

// A v3 frame whose payload lost its period identity is corruption, not an
// older event: the epoch is the writer's promise that the field is there.
func TestTruncatedPeriodIdentityIsRejected(t *testing.T) {
	start := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(Event{
		EventID: "evt_no_zone", Kind: EventRequestAccepted, RequestID: "req_1",
		ProjectID: "prj_1", PeriodID: "2026-08-06", OccurredAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "corrupt.wal")
	frame := encodeFrameVersion(frameVersionPeriod, 1, EventRequestAccepted, payload)
	if err := os.WriteFile(path, frame, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect(path); err == nil {
		t.Fatal("a v3 frame without period identity was accepted")
	}
}
