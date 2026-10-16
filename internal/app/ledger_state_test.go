package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/archive"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
)

var ledgerStateTestKey = bytes.Repeat([]byte{0x31}, 32)

// accountingFixture is a Ledger written through the accounting manager, as a
// running instance writes it.
type accountingFixture struct {
	t       *testing.T
	path    string
	log     *ledger.Log
	state   *ledger.State
	manager *budget.Manager
	billed  int
	// cost is what each billed attempt commits.
	cost int64
	// archive is where the state's finished Work Units go; every snapshot
	// records its position in it.
	archive *archive.Store
}

func newAccountingFixture(t *testing.T) *accountingFixture {
	t.Helper()
	f := &accountingFixture{t: t, path: filepath.Join(t.TempDir(), "ledger", "ledger.wal"), cost: 7}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, _, err := archive.Open(filepath.Join(filepath.Dir(f.path), "archive.db"), bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.archive = store
	f.open(ledger.NewState())
	return f
}

func (f *accountingFixture) open(state *ledger.State) {
	f.t.Helper()
	state.SetArchive(f.archive)
	log, err := ledger.OpenWithOptions(f.path, nil, ledger.Options{MaxBatch: 1, ChainKey: ledgerStateTestKey})
	if err != nil {
		f.t.Fatal(err)
	}
	f.log = log
	f.attach(state)
}

// attach writes through the open Ledger into state from here on.
func (f *accountingFixture) attach(state *ledger.State) {
	f.t.Helper()
	state.SetArchive(f.archive)
	resolver, err := budget.NewFixedPeriodResolver("UTC")
	if err != nil {
		f.t.Fatal(err)
	}
	// A fixed clock keeps every event the same length, so two fixtures that
	// bill alike lay out their frames alike.
	fixed := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	manager, err := budget.NewWithOptions(f.log, state, resolver, budget.Options{Now: func() time.Time { return fixed }})
	if err != nil {
		f.t.Fatal(err)
	}
	f.state, f.manager = state, manager
}

func (f *accountingFixture) bill(count int) {
	f.t.Helper()
	ctx := context.Background()
	for range count {
		f.billed++
		request, err := f.manager.BeginRequestDetailed(ctx, "project_1", "key_1", fmt.Sprintf("req_%d", f.billed), "chat")
		if err != nil {
			f.t.Fatal(err)
		}
		attempt, err := f.manager.ReserveAttemptDetailed(ctx, request, 1_000_000, 100, budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
		})
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.manager.MarkStarted(ctx, attempt); err != nil {
			f.t.Fatal(err)
		}
		if err := f.manager.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: f.cost, ProviderInputTokens: 3, ProviderOutputTokens: 2, Outcome: "success"}); err != nil {
			f.t.Fatal(err)
		}
		if err := f.manager.Finalize(ctx, request, "success"); err != nil {
			f.t.Fatal(err)
		}
	}
}

// reopen closes the Ledger and opens the state the way a start does.
func (f *accountingFixture) reopen(snapshotPath string) openedLedgerState {
	f.t.Helper()
	if err := f.log.Close(); err != nil {
		f.t.Fatal(err)
	}
	log, err := ledger.OpenWithOptions(f.path, nil, ledger.Options{MaxBatch: 1, ChainKey: ledgerStateTestKey})
	if err != nil {
		f.t.Fatal(err)
	}
	f.log = log
	opened, err := openLedgerState(log, snapshotPath, ledgerStateTestKey, f.archive, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	return opened
}

// replayed is the state a full replay of the Ledger gives.
func (f *accountingFixture) replayed() []byte {
	f.t.Helper()
	state := ledger.NewState()
	if _, err := f.log.Replay(ledger.Watermark{}, state.Apply); err != nil {
		f.t.Fatal(err)
	}
	return encodedLedgerState(f.t, state)
}

func encodedLedgerState(t *testing.T, state *ledger.State) []byte {
	t.Helper()
	encoded, _, err := state.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (f *accountingFixture) snapshot(path string) {
	f.t.Helper()
	if _, _, err := ledger.WriteStateSnapshot(path, ledgerStateTestKey, f.state, f.log); err != nil {
		f.t.Fatal(err)
	}
}

// A start resumes from the snapshot and replays only what follows it, and
// ends with the state a full replay gives.
func TestLedgerStateResumesFromASnapshotInTheActiveGeneration(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(5)
	f.snapshot(snapshotPath)
	f.bill(4)
	opened := f.reopen(snapshotPath)
	defer f.log.Close()
	if !opened.fromSnapshot {
		t.Fatal("the start did not resume from the snapshot")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayed()) {
		t.Fatal("the resumed state differs from a full replay")
	}
}

// A snapshot inside a sealed generation resumes from that generation's first
// frame, so every frame it applies was authenticated on the way.
func TestLedgerStateResumesFromASnapshotInASealedGeneration(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(5)
	f.snapshot(snapshotPath)
	f.bill(3)
	if result, err := f.log.Roll(); err != nil || !result.Rolled {
		t.Fatalf("roll rolled=%t err=%v", result.Rolled, err)
	}
	f.bill(3)
	opened := f.reopen(snapshotPath)
	defer f.log.Close()
	if !opened.fromSnapshot {
		t.Fatal("the start did not resume from the snapshot")
	}
	if opened.state.Watermark().Generation == 1 {
		t.Fatal("the fixture's head is still in the sealed generation")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayed()) {
		t.Fatal("the resumed state differs from a full replay")
	}
}

// A snapshot that describes more than this Ledger holds — another copy of it,
// or one cut back — is set aside, and the state is rebuilt from the Ledger.
func TestLedgerStateSnapshotAheadOfTheLedgerIsSetAside(t *testing.T) {
	longer := newAccountingFixture(t)
	snapshotPath := filepath.Join(t.TempDir(), "state.snap")
	longer.bill(6)
	longer.snapshot(snapshotPath)
	longer.log.Close()

	shorter := newAccountingFixture(t)
	shorter.bill(2)
	opened := shorter.reopen(snapshotPath)
	defer shorter.log.Close()
	if opened.fromSnapshot {
		t.Fatal("a snapshot ahead of the Ledger was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), shorter.replayed()) {
		t.Fatal("the rebuilt state differs from a full replay")
	}
}

// A snapshot from a fork of this Ledger — a copy written under the same key
// that went another way after a common prefix — is set aside even when the
// fork is the same length, so its offsets and sequences all line up. Only the
// chain says the histories differ.
func TestLedgerStateSnapshotFromAForkIsSetAside(t *testing.T) {
	ours := newAccountingFixture(t)
	ours.bill(2)
	if err := ours.log.Close(); err != nil {
		t.Fatal(err)
	}
	fork := newAccountingFixture(t)
	fork.log.Close()
	raw, err := os.ReadFile(ours.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fork.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ours.open(ours.state)
	fork.open(ledger.NewState())
	if _, err := fork.log.Replay(ledger.Watermark{}, fork.state.Apply); err != nil {
		t.Fatal(err)
	}
	fork.billed = ours.billed

	fork.cost = 8
	fork.bill(3)
	snapshotPath := filepath.Join(t.TempDir(), "state.snap")
	fork.snapshot(snapshotPath)
	forked := fork.state.Watermark()
	fork.log.Close()

	ours.bill(4)
	var at ledger.Watermark
	if _, err := ours.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Sequence == forked.Sequence {
			at = ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if at != forked {
		t.Fatalf("the fork does not line up with this Ledger: fork %#v, ours %#v", forked, at)
	}
	// This Ledger's own archive covers the fork's snapshot position, so the
	// only thing left to refuse it is the chain.
	ours.snapshot(filepath.Join(t.TempDir(), "ours.snap"))
	if through, _, err := ours.archive.ArchivedThrough(); err != nil || through < forked.Sequence {
		t.Fatalf("archive through=%d err=%v, fork at %d", through, err, forked.Sequence)
	}
	opened := ours.reopen(snapshotPath)
	defer ours.log.Close()
	if opened.fromSnapshot {
		t.Fatal("a snapshot from a fork of the Ledger was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), ours.replayed()) {
		t.Fatal("the rebuilt state differs from a full replay")
	}
}

// A snapshot past the archive's recorded position is set aside: the Work
// Units it no longer holds may be in no archive at all, as they are when the
// archive was emptied or lost.
func TestLedgerStateSnapshotPastTheArchiveIsSetAside(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(3)
	f.snapshot(snapshotPath)
	f.archive.Close()
	emptied, reset, err := archive.Open(filepath.Join(filepath.Dir(f.path), "archive.db"), bytes.Repeat([]byte{0x33}, 32))
	if err != nil || !reset {
		t.Fatalf("reset=%t err=%v", reset, err)
	}
	t.Cleanup(func() { emptied.Close() })
	f.archive = emptied
	opened := f.reopen(snapshotPath)
	defer f.log.Close()
	if opened.fromSnapshot {
		t.Fatal("a snapshot the archive does not cover was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayed()) {
		t.Fatal("the rebuilt state differs from a full replay")
	}
}

// A damaged snapshot is set aside the same way.
func TestLedgerStateDamagedSnapshotIsSetAside(t *testing.T) {
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	f.bill(3)
	f.snapshot(snapshotPath)
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 1
	if err := os.WriteFile(snapshotPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := f.reopen(snapshotPath)
	defer f.log.Close()
	if opened.fromSnapshot {
		t.Fatal("a damaged snapshot was used")
	}
	if !bytes.Equal(encodedLedgerState(t, opened.state), f.replayed()) {
		t.Fatal("the rebuilt state differs from a full replay")
	}
}

// A clean shutdown writes the state it ends with, and the next start resumes
// from it with the state a full replay gives.
func TestRuntimeShutdownWritesTheStateTheNextStartResumesFrom(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := range 3 {
		request, err := runtime.accounting.BeginRequestDetailed(ctx, "project_1", "key_1", fmt.Sprintf("req_%d", index), "chat")
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := runtime.accounting.ReserveAttemptDetailed(ctx, request, 1_000_000, 100, budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 5, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	closed := runtime.state.Watermark()
	stateKey := append([]byte(nil), runtime.accountingState.snapshots.key...)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	written, _, err := ledger.ReadStateSnapshot(cfg.LedgerStatePath(), stateKey)
	if err != nil {
		t.Fatal(err)
	}
	if written.Watermark() != closed {
		t.Fatalf("snapshot at %#v, state closed at %#v", written.Watermark(), closed)
	}

	reopened, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed := ledger.NewState()
	if _, err := reopened.ledger.Replay(ledger.Watermark{}, replayed.Apply); err != nil {
		t.Fatal(err)
	}
	// The reopened state may already hold the start's own events past the
	// snapshot; what it must match is a full replay of the same Ledger.
	if !bytes.Equal(encodedLedgerState(t, reopened.state), encodedLedgerState(t, replayed)) {
		t.Fatal("the reopened state differs from a full replay")
	}
}

// finishWorkUnits opens and closes count Work Units through the accounting
// manager; with no Run and nothing in flight each is archived as it closes.
func (f *accountingFixture) finishWorkUnits(count int) []string {
	f.t.Helper()
	ctx := context.Background()
	var ids []string
	for range count {
		f.billed++
		intent := func(operation string) budget.GovernanceIntent {
			return budget.GovernanceIntent{Operation: operation,
				IdempotencyKeyHash: fmt.Sprintf("sha256:%064x", f.billed), RequestFingerprint: fmt.Sprintf("sha256:%064x", f.billed)}
		}
		workUnit, _, err := f.manager.CreateWorkUnit(ctx, "project_1", "key_1", 1000, intent("work_unit.create"))
		if err != nil {
			f.t.Fatal(err)
		}
		if _, _, err := f.manager.CloseWorkUnit(ctx, "project_1", "key_1", workUnit.ID, intent("work_unit.close")); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, workUnit.ID)
	}
	return ids
}

// A replay that fills the archive queue writes it out as it goes — while the
// Ledger is being walked, both from the first frame and from a snapshot — and
// every finished Work Unit is found in the archive afterwards.
func TestLedgerStateReplayWritesTheArchiveAsItFills(t *testing.T) {
	previous := archiveFlushThreshold
	archiveFlushThreshold = 1
	defer func() { archiveFlushThreshold = previous }()
	f := newAccountingFixture(t)
	snapshotPath := filepath.Join(filepath.Dir(f.path), "state.snap")
	ids := f.finishWorkUnits(3)

	rebuilt := f.reopen(snapshotPath)
	if rebuilt.fromSnapshot || rebuilt.state.ArchiveQueueLen() != 0 {
		t.Fatalf("from snapshot=%t queue=%d", rebuilt.fromSnapshot, rebuilt.state.ArchiveQueueLen())
	}
	f.attach(rebuilt.state)
	f.snapshot(snapshotPath)
	ids = append(ids, f.finishWorkUnits(2)...)

	resumed := f.reopen(snapshotPath)
	defer f.log.Close()
	if !resumed.fromSnapshot || resumed.state.ArchiveQueueLen() != 0 {
		t.Fatalf("from snapshot=%t queue=%d", resumed.fromSnapshot, resumed.state.ArchiveQueueLen())
	}
	for _, id := range ids {
		item, found, err := f.archive.ArchivedWorkUnit(id)
		if err != nil || !found || item.WorkUnit.Status != "closed" {
			t.Fatalf("%s: archived %#v found=%t err=%v", id, item, found, err)
		}
	}
}
