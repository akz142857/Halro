package ledger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func governanceEvent(id string, kind EventKind, occurredAt time.Time) Event {
	return Event{
		EventID: id, Kind: kind, ProjectID: "prj_1", KeyID: "key_1",
		PeriodID: "2026-09-04", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
		PeriodStartMicros:  occurredAt.Truncate(24 * time.Hour).UnixMicro(),
		PeriodEndMicros:    occurredAt.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(),
		OccurredAt:         occurredAt,
		Operation:          "test." + id,
		IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}

func TestRunAttributionLifecycleReplaysAndSettlesOnce(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	sequence := uint64(0)
	apply := func(event Event) {
		t.Helper()
		sequence++
		if err := state.Apply(Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Epoch: frameVersionRunAttribution, Event: event}); err != nil {
			t.Fatalf("apply %d: %v", sequence, err)
		}
	}

	workUnit := governanceEvent("evt_wu", EventWorkUnitCreated, now)
	workUnit.WorkUnitID = "wku_1"
	apply(workUnit)
	runCreated := governanceEvent("evt_run", EventRunCreated, now.Add(time.Second))
	runCreated.WorkUnitID, runCreated.RunID = "wku_1", "run_1"
	runCreated.RunBudgetMicrosUSD, runCreated.RunExpiresAt = 1_000, now.Add(time.Hour)
	apply(runCreated)
	accepted := governanceEvent("evt_req", EventRequestAccepted, now.Add(2*time.Second))
	accepted.RequestID, accepted.WorkUnitID, accepted.RunID = "req_1", "wku_1", "run_1"
	apply(accepted)
	reserved := governanceEvent("evt_res", EventReservationCreated, now.Add(3*time.Second))
	reserved.RequestID, reserved.AttemptID = "req_1", "att_1"
	reserved.WorkUnitID, reserved.RunID = "wku_1", "run_1"
	reserved.ReservationMicrosUSD = MicrosUSD(100)
	apply(reserved)
	settled := governanceEvent("evt_settle", EventAttemptSettled, now.Add(4*time.Second))
	settled.RequestID, settled.AttemptID = "req_1", "att_1"
	settled.WorkUnitID, settled.RunID = "wku_1", "run_1"
	settled.CommittedMicrosUSD = MicrosUSD(75)
	apply(settled)

	run, ok := state.Run("run_1")
	if !ok || run.ReservedMicrosUSD != 0 || run.CommittedMicrosUSD != 75 || run.UnknownAttempts != 0 {
		t.Fatalf("run balance after settlement = %#v, found=%t", run, ok)
	}
	if err := state.Apply(Record{Generation: 1, Sequence: sequence + 1, Offset: 999, Epoch: frameVersionRunAttribution, Event: settled}); err != nil {
		t.Fatalf("same durable event must replay idempotently: %v", err)
	}
	run, _ = state.Run("run_1")
	if run.CommittedMicrosUSD != 75 {
		t.Fatalf("duplicate settlement committed %d, want 75", run.CommittedMicrosUSD)
	}
}

func TestRunAttributionRejectsCrossProjectAttachment(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	workUnit := governanceEvent("evt_wu", EventWorkUnitCreated, now)
	workUnit.WorkUnitID = "wku_1"
	if err := state.Apply(Record{Sequence: 1, Event: workUnit}); err != nil {
		t.Fatal(err)
	}
	run := governanceEvent("evt_run", EventRunCreated, now)
	run.WorkUnitID, run.RunID, run.RunBudgetMicrosUSD, run.RunExpiresAt = "wku_1", "run_1", 1, now.Add(time.Hour)
	if err := state.Apply(Record{Sequence: 2, Event: run}); err != nil {
		t.Fatal(err)
	}
	request := governanceEvent("evt_bad", EventRequestAccepted, now.Add(time.Second))
	request.ProjectID, request.RequestID, request.WorkUnitID, request.RunID = "prj_2", "req_1", "wku_1", "run_1"
	if err := state.Apply(Record{Sequence: 3, Event: request}); err == nil {
		t.Fatal("cross-project Run attachment was accepted")
	}
}

func TestAuthenticatedEpochCannotReturnFromV5ToV4(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first := governanceEvent("evt_v5", EventWorkUnitCreated, now)
	first.WorkUnitID = "wku_1"
	firstPayload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	second := governanceEvent("evt_v4", EventRequestAccepted, now)
	second.RequestID = "req_1"
	secondPayload, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	firstFrame, head := encodeChainFrameVersion(frameVersionRunAttribution, testChainKey, 1, first.Kind, firstPayload, [32]byte{})
	secondFrame, _ := encodeChainFrameVersion(frameVersionLedgerIntegrity, testChainKey, 2, second.Kind, secondPayload, head)
	path := filepath.Join(t.TempDir(), "downgrade.wal")
	if err := os.WriteFile(path, append(firstFrame, secondFrame...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("v5 to v4 transition err=%v, want ErrTampered", err)
	}
}

func TestV4HistoryMayAdvanceToV5(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	v4 := governanceEvent("evt_v4", EventRequestAccepted, now)
	v4.RequestID = "req_1"
	v4Payload, _ := json.Marshal(v4)
	v5 := governanceEvent("evt_v5", EventWorkUnitCreated, now)
	v5.WorkUnitID = "wku_1"
	v5Payload, _ := json.Marshal(v5)
	first, head := encodeChainFrameVersion(frameVersionLedgerIntegrity, testChainKey, 1, v4.Kind, v4Payload, [32]byte{})
	second, _ := encodeChainFrameVersion(frameVersionRunAttribution, testChainKey, 2, v5.Kind, v5Payload, head)
	path := filepath.Join(t.TempDir(), "upgrade.wal")
	if err := os.WriteFile(path, append(first, second...), 0o600); err != nil {
		t.Fatal(err)
	}
	var kinds []EventKind
	if _, partial, err := InspectReplayAuthenticated(path, testChainKey, func(record Record) error {
		kinds = append(kinds, record.Event.Kind)
		return nil
	}); err != nil || partial || len(kinds) != 2 {
		t.Fatalf("v4 to v5 replay kinds=%v partial=%t err=%v", kinds, partial, err)
	}
}

func TestRunAttributionEpochCannotDowngradeAcrossSealedBoundary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 1)
	sealed, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	event := validReservation("evt_v4_after_roll", "attempt_v4_after_roll")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := decodeChainHash(sealed.Sealed.EndHash)
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := encodeChainFrameVersion(frameVersionLedgerIntegrity, testChainKey, sealed.Sealed.LastSequence+1, event.Kind, payload, anchor)
	if err := os.WriteFile(path, frame, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("verify accepted cross-generation v5 to v4 transition: %v", err)
	}
	if _, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey}); !errors.Is(err, ErrTampered) {
		t.Fatalf("open accepted cross-generation v5 to v4 transition: %v", err)
	}
}
