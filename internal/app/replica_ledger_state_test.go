package app

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

// A Replica's Ledger can end in frames that are durable but unconfirmed. Its
// state holds the applied prefix only, and what it writes about that state —
// snapshots, archive positions — binds to the prefix, never to the tail.

// reopenBounded closes the Ledger and opens the state the way a Replica start
// does, through sequence through.
func (f *accountingFixture) reopenBounded(snapshotPath string, through uint64) (openedLedgerState, *appliedLedgerHead) {
	f.t.Helper()
	if err := f.log.Close(); err != nil {
		f.t.Fatal(err)
	}
	log, err := ledger.OpenWithOptions(f.path, nil, ledger.Options{MaxBatch: 1, ChainKey: ledgerStateTestKey})
	if err != nil {
		f.t.Fatal(err)
	}
	f.log = log
	head := &appliedLedgerHead{}
	opened, err := openBoundedLedgerState(log, snapshotPath, ledgerStateTestKey, f.archive,
		slog.New(slog.NewTextHandler(io.Discard, nil)), &ledgerReplayBound{through: through, head: head})
	if err != nil {
		f.t.Fatal(err)
	}
	return opened, head
}

// replayedThrough is the state a replay of the Ledger through sequence gives.
func (f *accountingFixture) replayedThrough(sequence uint64) []byte {
	f.t.Helper()
	state := ledger.NewState()
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Sequence > sequence {
			return nil
		}
		return state.Apply(record)
	}); err != nil {
		f.t.Fatal(err)
	}
	return encodedLedgerState(f.t, state)
}

// chainHashAt is the chain head after the record at sequence.
func (f *accountingFixture) chainHashAt(sequence uint64) [32]byte {
	f.t.Helper()
	var hash [32]byte
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Sequence == sequence {
			hash = record.Hash
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	if hash == ([32]byte{}) {
		f.t.Fatalf("no chain head at sequence %d", sequence)
	}
	return hash
}

func TestReplicaLedgerStateStopsAtTheAppliedPrefix(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(3)
	applied := f.state.Watermark().Sequence
	f.bill(4)
	opened, head := f.reopenBounded(snapshotPath, applied)
	defer f.log.Close()
	if opened.fromSnapshot {
		t.Fatal("there was no snapshot to resume from")
	}
	if got := opened.state.Watermark().Sequence; got != applied || opened.head.Sequence != applied {
		t.Fatalf("state at %d, head %d; want both at the applied prefix %d", got, opened.head.Sequence, applied)
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayedThrough(applied)) {
		t.Fatal("the state differs from a replay of the applied prefix")
	}
	at, hash, ok := head.ChainHead()
	if !ok || at.Sequence != applied || hash != f.chainHashAt(applied) {
		t.Fatalf("applied head = %d ok=%t, want the chain head at %d", at.Sequence, ok, applied)
	}
}

// The snapshot a Replica writes names the chain head of its applied prefix,
// not of the tail, and the next start resumes from it.
func TestReplicaLedgerStateSnapshotBindsToTheAppliedPrefix(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(3)
	applied := f.state.Watermark().Sequence
	f.bill(4)
	opened, head := f.reopenBounded(snapshotPath, applied)
	if _, _, err := ledger.WriteStateSnapshot(snapshotPath, ledgerStateTestKey, opened.state, head); err != nil {
		t.Fatal(err)
	}
	_, chain, err := ledger.ReadStateSnapshot(snapshotPath, ledgerStateTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if logHead, _, _ := f.log.ChainHead(); logHead.Sequence <= applied {
		t.Fatalf("the fixture has no tail past %d", applied)
	}
	if chain.Sequence != applied || chain.Hash != f.chainHashAt(applied) {
		t.Fatalf("snapshot chain at %d, want the applied prefix %d", chain.Sequence, applied)
	}
	resumed, resumedHead := f.reopenBounded(snapshotPath, applied)
	defer f.log.Close()
	if !resumed.fromSnapshot {
		t.Fatal("the start did not resume from the Replica's snapshot")
	}
	if !bytes.Equal(encodedLedgerState(t, resumed.state), f.replayedThrough(applied)) {
		t.Fatal("the resumed state differs from a replay of the applied prefix")
	}
	// Nothing was applied after the snapshot; the head comes from it, so the
	// next snapshot can still be written.
	if at, _, ok := resumedHead.ChainHead(); !ok || at.Sequence != applied {
		t.Fatalf("resumed applied head = %d ok=%t, want %d", at.Sequence, ok, applied)
	}
}

// A snapshot past the applied prefix — written before a crash that lost the
// applied index, or carried from a node further along — is set aside.
func TestReplicaLedgerStateSnapshotPastTheAppliedPrefixIsSetAside(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(3)
	applied := f.state.Watermark().Sequence
	f.bill(2)
	f.snapshot(snapshotPath)
	opened, _ := f.reopenBounded(snapshotPath, applied)
	defer f.log.Close()
	if opened.fromSnapshot {
		t.Fatal("a snapshot past the applied prefix was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayedThrough(applied)) {
		t.Fatal("the rebuilt state differs from a replay of the applied prefix")
	}
}

// A seed carries the source's ledger directory, state.snap with it (port
// design §4.5). One that was altered on the way fails its MAC and the Replica
// rebuilds rather than trusting it.
func TestReplicaLedgerStateSeededSnapshotThatWasAlteredIsRebuilt(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(4)
	applied := f.state.Watermark().Sequence
	f.snapshot(snapshotPath)

	carried, _ := f.reopenBounded(snapshotPath, applied)
	if !carried.fromSnapshot {
		t.Fatal("an intact seeded snapshot was not used")
	}

	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 1
	if err := os.WriteFile(snapshotPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, _ := f.reopenBounded(snapshotPath, applied)
	defer f.log.Close()
	if opened.fromSnapshot {
		t.Fatal("an altered seeded snapshot was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayedThrough(applied)) {
		t.Fatal("the rebuilt state differs from a replay of the applied prefix")
	}
}

// A Replica exports no Parquet, so its compaction bar is its usage history
// and its applied prefix, whichever is lower.
func TestReplicaCompactionGateIsTheHistoryAndTheAppliedPrefix(t *testing.T) {
	f := newAccountingFixture(t)
	f.bill(2)
	indexed := f.state.Watermark().Sequence
	f.bill(2)
	defer f.log.Close()
	history, _, err := usage.OpenHistory(filepath.Join(t.TempDir(), "history.db"), bytes.Repeat([]byte{0x34}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	aggregate := usage.NewAggregate(history)
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Sequence > indexed {
			return nil
		}
		return aggregate.Apply(record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, applied := range []uint64{indexed - 1, indexed + 3} {
		head := &appliedLedgerHead{}
		head.observe(ledger.Record{Sequence: applied, Hash: [32]byte{1}})
		runtime := &Runtime{
			usage:       aggregate,
			replication: &replicationRuntime{role: replication.RoleReplica, ledgerApplied: head},
		}
		through, ok := runtime.ledgerArchivedThrough()
		if want := min(applied, indexed); !ok || through != want {
			t.Fatalf("applied %d, indexed %d: gate=(%d,%t), want (%d,true)", applied, indexed, through, ok, want)
		}
	}
}

// A Replica's runtime never rolls its Ledger: the Primary decides where a
// generation ends. The Ledger here is not in Replica mode, so a roll the
// runtime asked for would happen; the Primary beside it shows the threshold
// was met.
func TestReplicaRuntimeNeverRollsItsLedger(t *testing.T) {
	for _, role := range []replication.Role{replication.RoleReplica, replication.RolePrimary} {
		f := newAccountingFixture(t)
		f.bill(3)
		runtime := &Runtime{
			ledger: f.log, status: ledger.NewStatus(),
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			replication: &replicationRuntime{role: role},
		}
		runtime.config.Ledger.Seal.Enabled = true
		runtime.config.Ledger.Seal.MaxActiveBytes = 1
		runtime.sealLedgerGeneration()
		rolled := f.log.Generation() != 1
		f.log.Close()
		if rolled != (role == replication.RolePrimary) {
			t.Fatalf("%s: rolled=%t", role, rolled)
		}
	}
}

// A Replica admits nothing, but its state is what it takes over on promotion,
// so its scrape carries the size against its own limit — and no rejection
// counter, which only an admitting member can move.
func TestReplicaMetricsReportTheHotStateItWouldTakeOver(t *testing.T) {
	f := newAccountingFixture(t)
	f.bill(3)
	defer f.log.Close()
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_01", NodeID: "halro-1",
		Role: replication.RoleReplica, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "halro-0", Address: "private.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	publisher, err := replication.NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	runtime := &Runtime{state: f.state, replication: &replicationRuntime{role: replication.RoleReplica, publisher: publisher}}
	runtime.config.Ledger.MaxHotStateBytes = 64 << 20
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	estimated := f.state.HotStateBytes(0)
	if !strings.Contains(body, fmt.Sprintf("halro_ledger_hot_state_estimated_bytes %d\n", estimated)) ||
		!strings.Contains(body, fmt.Sprintf("halro_ledger_hot_state_limit_bytes %d\n", 64<<20)) {
		t.Fatalf("Replica scrape lacks its hot state (estimate %d):\n%s", estimated, body)
	}
	if strings.Contains(body, "halro_ledger_hot_state_rejections_total") {
		t.Fatalf("Replica scrape carries a rejection counter it cannot move:\n%s", body)
	}
}
