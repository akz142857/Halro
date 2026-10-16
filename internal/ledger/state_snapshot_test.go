package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// snapshotFixtureEvents is a history touching every part of the state: a Work
// Unit with a Run, requests attributed to it and not, attempts settled and one
// left in flight, a pin-commit failure that keeps its lease, and an exact
// duplicate of a settlement.
func snapshotFixtureEvents() []Event {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	var events []Event
	workUnit := governanceEvent("evt_wu", EventWorkUnitCreated, now)
	workUnit.WorkUnitID = "wku_1"
	run := governanceEvent("evt_run", EventRunCreated, now)
	run.WorkUnitID, run.RunID = "wku_1", "run_1"
	run.RunBudgetMicrosUSD, run.RunExpiresAt = 100_000, now.Add(time.Hour)
	events = append(events, workUnit, run)
	for index := 0; index < 6; index++ {
		requestID, attemptID := fmt.Sprintf("req_%d", index), fmt.Sprintf("att_%d", index)
		accepted := governanceEvent("evt_acc_"+requestID, EventRequestAccepted, now)
		accepted.RequestID = requestID
		if index%2 == 0 {
			accepted.WorkUnitID, accepted.RunID = "wku_1", "run_1"
		}
		reserved := governanceEvent("evt_res_"+attemptID, EventReservationCreated, now)
		reserved.RequestID, reserved.AttemptID = requestID, attemptID
		reserved.WorkUnitID, reserved.RunID = accepted.WorkUnitID, accepted.RunID
		reserved.ReservationMicrosUSD = MicrosUSD(100)
		events = append(events, accepted, reserved)
		if index == 5 {
			break // left in flight
		}
		settled := governanceEvent("evt_set_"+attemptID, EventAttemptSettled, now)
		settled.RequestID, settled.AttemptID = requestID, attemptID
		settled.WorkUnitID, settled.RunID = accepted.WorkUnitID, accepted.RunID
		settled.CommittedMicrosUSD = MicrosUSD(int64(10 * (index + 1)))
		if index == 3 {
			settled.Outcome = OutcomePinCommitFailed
		}
		finalized := governanceEvent("evt_fin_"+requestID, EventRequestFinalized, now)
		finalized.RequestID, finalized.WorkUnitID, finalized.RunID = requestID, accepted.WorkUnitID, accepted.RunID
		events = append(events, settled, finalized)
		if index == 1 {
			events = append(events, settled)
		}
	}
	return events
}

func applyAll(t *testing.T, state *State, events []Event, firstSequence uint64) {
	t.Helper()
	for index, event := range events {
		sequence := firstSequence + uint64(index)
		if err := state.Apply(Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Epoch: frameVersionRunAttribution, Event: event}); err != nil {
			t.Fatalf("apply %d: %v", sequence, err)
		}
	}
}

func encodedState(t *testing.T, state *State) []byte {
	t.Helper()
	encoded, _, err := state.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// A state restored from a snapshot and replayed on from it is the state that
// replaying the whole history gives, wherever the snapshot was cut.
func TestStateSnapshotRestoresTheReplayedState(t *testing.T) {
	events := snapshotFixtureEvents()
	full := NewState()
	applyAll(t, full, events, 1)
	want := encodedState(t, full)
	for cut := 0; cut <= len(events); cut++ {
		prefix := NewState()
		applyAll(t, prefix, events[:cut], 1)
		restored, err := UnmarshalStateSnapshot(encodedState(t, prefix))
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		applyAll(t, restored, events[cut:], uint64(cut)+1)
		if got := encodedState(t, restored); !bytes.Equal(got, want) {
			t.Fatalf("cut %d: restored state differs from the replayed one\n got %s\nwant %s", cut, got, want)
		}
	}
	if _, ok := full.AccountingLease("att_3"); !ok {
		t.Fatal("the pin-commit failure lost its lease")
	}
	if _, ok := full.AccountingLease("att_5"); !ok {
		t.Fatal("the attempt in flight lost its lease")
	}
}

// The digest window survives a snapshot in order: a restored state forgets
// the same event next.
func TestStateSnapshotKeepsTheDigestWindowOrder(t *testing.T) {
	state := NewState()
	for index := 0; index < recentEventDigests+3; index++ {
		state.rememberDigest(fmt.Sprintf("evt_%d", index), [32]byte{byte(index)})
	}
	restored, err := UnmarshalStateSnapshot(encodedState(t, state))
	if err != nil {
		t.Fatal(err)
	}
	state.rememberDigest("evt_next", [32]byte{1})
	restored.rememberDigest("evt_next", [32]byte{1})
	if !bytes.Equal(encodedState(t, state), encodedState(t, restored)) {
		t.Fatal("the restored digest window forgot a different event")
	}
	if _, ok := restored.eventDigests["evt_3"]; ok {
		t.Fatal("the oldest digest survived")
	}
}

// The snapshot file is authenticated: an edited byte, another key or another
// version is unusable, and a missing file says so.
func TestStateSnapshotFileIsAuthenticated(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	path := filepath.Join(t.TempDir(), "ledger-state.snap")
	if _, _, err := ReadStateSnapshot(path, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing snapshot err=%v", err)
	}
	state := NewState()
	applyAll(t, state, snapshotFixtureEvents(), 1)
	if _, _, err := WriteStateSnapshot(path, key, state, fixedChainHead{sequence: state.Watermark().Sequence - 1}); err == nil {
		t.Fatal("a snapshot was written with a chain head behind its state")
	}
	written := fixedChainHead{sequence: state.Watermark().Sequence + 2, hash: [32]byte{9}}
	watermark, _, err := WriteStateSnapshot(path, key, state, written)
	if err != nil {
		t.Fatal(err)
	}
	if watermark != state.Watermark() {
		t.Fatalf("watermark=%#v state=%#v", watermark, state.Watermark())
	}
	restored, chain, err := ReadStateSnapshot(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if chain != (StateSnapshotChain{Sequence: written.sequence, Hash: written.hash}) {
		t.Fatalf("chain=%#v, written %#v", chain, written)
	}
	if !bytes.Equal(encodedState(t, restored), encodedState(t, state)) {
		t.Fatal("the read snapshot differs from the written state")
	}
	if _, _, err := ReadStateSnapshot(path, bytes.Repeat([]byte{8}, 32)); !errors.Is(err, ErrStateSnapshotUnusable) {
		t.Fatalf("another key err=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[10] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStateSnapshot(path, key); !errors.Is(err, ErrStateSnapshotUnusable) {
		t.Fatalf("edited snapshot err=%v", err)
	}
	if _, err := UnmarshalStateSnapshot([]byte(`{"version":1}`)); !errors.Is(err, ErrStateSnapshotUnusable) {
		t.Fatalf("another version err=%v", err)
	}
}

// fixedChainHead is a Ledger chain head for snapshots written without a Ledger.
type fixedChainHead struct {
	sequence uint64
	hash     [32]byte
}

func (h fixedChainHead) ChainHead() (Watermark, [32]byte, bool) {
	return Watermark{Generation: 1, Sequence: h.sequence}, h.hash, true
}
