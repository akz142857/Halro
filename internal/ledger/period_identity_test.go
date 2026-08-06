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
	if record.Epoch != frameVersionPeriod {
		t.Fatalf("epoch = %d, want %d", record.Epoch, frameVersionPeriod)
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

// An event with no period identity stays on its old epoch, so a log written
// before this change still replays.
func TestFrameVersionTracksPeriodIdentity(t *testing.T) {
	withoutPeriod := Event{Kind: EventAttemptSettled, LeaseMode: LeaseModeMetered}
	if got := eventFrameVersion(withoutPeriod); got != frameVersionCurrent {
		t.Fatalf("lease event without a period = epoch %d, want %d", got, frameVersionCurrent)
	}
	legacy := Event{Kind: EventRequestAccepted}
	if got := eventFrameVersion(legacy); got != frameVersionLegacy {
		t.Fatalf("plain event = epoch %d, want %d", got, frameVersionLegacy)
	}
	withPeriod := Event{Kind: EventRequestAccepted, PeriodTimezone: "UTC"}
	if got := eventFrameVersion(withPeriod); got != frameVersionPeriod {
		t.Fatalf("event with a period = epoch %d, want %d", got, frameVersionPeriod)
	}
}

// The reader was taught to accept epoch 3 and to reject a v3 frame missing its
// period identity. Neither branch means anything until a v3 frame has actually
// been through a file: the in-memory checks above never touch encoding.
func TestPeriodIdentitySurvivesTheWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "period.wal")
	log, err := Open(path, NewStatus())
	if err != nil {
		t.Fatal(err)
	}
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
	// A legacy event in the same file, so the reader is exercised on a mixed log
	// rather than a uniformly v3 one.
	if _, err := log.Append(context.Background(), Event{
		EventID: "evt_legacy", Kind: EventRequestAccepted, RequestID: "req_2", ProjectID: "prj_1",
		PeriodID: "2026-08-06", OccurredAt: start,
	}); err != nil {
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
	if len(records) != 2 {
		t.Fatalf("replayed %d records, want 2", len(records))
	}
	if records[0].Epoch != frameVersionPeriod {
		t.Fatalf("period event replayed at epoch %d, want %d", records[0].Epoch, frameVersionPeriod)
	}
	if records[1].Epoch != frameVersionLegacy {
		t.Fatalf("legacy event replayed at epoch %d, want %d", records[1].Epoch, frameVersionLegacy)
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
