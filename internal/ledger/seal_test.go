package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The four questions sealing had to answer before it could be written, one
// section each. They are questions rather than features because each of them is
// a way the accounting authority could quietly stop being the accounting
// authority: a chain that no longer joins up, a verification that shrinks to
// nothing, a backup that restores half a history, a replay that returns
// something other than what it returned yesterday.

func appendReservations(t *testing.T, log *Log, from, count int) {
	t.Helper()
	for index := range count {
		event := validReservation(fmt.Sprintf("evt_%d", from+index), fmt.Sprintf("attempt_%d", from+index))
		if _, err := log.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
}

func replayedEventIDs(t *testing.T, log *Log, from Watermark) ([]string, Watermark) {
	t.Helper()
	var ids []string
	head, err := log.Replay(from, func(record Record) error {
		ids = append(ids, record.Event.EventID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids, head
}

// 1 · The chain continues across generations.
//
// The first frame of a new generation anchors to the last frame of the sealed
// one. If it did not, every sealed instance would report tampering at its own
// first append after a roll — and the only ways to avoid that would be to stop
// verifying, or to start a fresh chain per file, which is the same thing said
// politely.
func TestTheChainContinuesIntoTheNextGeneration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 4)

	result, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rolled || result.Sealed.Generation != 1 || result.Active != 2 {
		t.Fatalf("roll = %#v", result)
	}
	if result.Sealed.FirstSequence != 1 || result.Sealed.LastSequence != 4 {
		t.Fatalf("sealed generation covers %d..%d, want 1..4",
			result.Sealed.FirstSequence, result.Sealed.LastSequence)
	}
	appendReservations(t, log, 5, 3)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening is the check: the open path verifies the active file's chain,
	// and the active file's first frame links to bytes that are no longer in it.
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	if reopened.Generation() != 2 {
		t.Fatalf("generation = %d after reopening a sealed log, want 2", reopened.Generation())
	}
	if reopened.SealedThrough() != 4 {
		t.Fatalf("sealed through %d, want 4", reopened.SealedThrough())
	}
	head, _, ok := reopened.ChainHead()
	if !ok || head.Sequence != 7 || head.Generation != 2 {
		t.Fatalf("chain head = %#v ok=%t", head, ok)
	}

	// And the successor cannot be re-anchored to nothing: rewriting the active
	// file's first frame to claim a zero previous-hash is exactly the edit that
	// would detach the new generation from the sealed history.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := range chainPreviousHashSize {
		raw[frameHeaderSize+index] = 0
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey}); !errors.Is(err, ErrTampered) {
		t.Fatalf("detaching the successor from its sealed predecessor: err=%v, want ErrTampered", err)
	}
}

// 2 · Verification still covers the whole history.
//
// `halro ledger verify` used to read one file, and after a roll that file holds
// a fraction of the ledger — right after a roll, none of it. A verification
// that keeps passing while covering less is worse than none, because it is
// reported as a pass.
func TestVerificationCoversSealedGenerationsAndCatchesEditsInThem(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 3)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 4, 2)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 6, 1)

	reports, err := log.VerifySealed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0].Authenticated != 3 || reports[1].Authenticated != 2 {
		t.Fatalf("sealed verification = %#v", reports)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// One byte of a sealed generation's payload, changed. Its CRC no longer
	// matches, which is the cheap check; the point is that anything reaches it
	// at all now that the bytes are not in the active file.
	sealed := filepath.Join(directory, "ledger-1.wal")
	raw, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw[chainHeaderSize+4] ^= 0xFF
	if err := os.WriteFile(sealed, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySegments(directory, testChainKey, 0); err == nil {
		t.Fatal("an edited sealed generation verified clean")
	}

	// And a generation that is simply gone is refused rather than skipped. A
	// missing file is the most likely way a history silently shortens, because
	// it looks like housekeeping.
	if err := os.Remove(sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySegments(directory, testChainKey, 0); !errors.Is(err, ErrSegmentMissing) {
		t.Fatalf("removing a sealed generation: err=%v, want ErrSegmentMissing", err)
	}
}

// 3 · Replay returns the same thing it returned before the roll.
//
// This is the invariant everything else rests on: balances and reservations are
// rebuilt by replaying from byte zero on every start. A roll that changed what
// a replay yields would change the balances, and it would do it silently.
func TestReplayIsUnchangedByASeal(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 6)

	// A watermark taken before the roll, pointing into what is about to become
	// a sealed generation. The usage checkpoint stores exactly this shape, and
	// resuming from it after a roll is the case that would break if a seal
	// rebased any offset.
	var resume Watermark
	if _, err := log.Replay(Watermark{}, func(record Record) error {
		if record.Sequence == 3 {
			resume = Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeAll, beforeHead := replayedEventIDs(t, log, Watermark{})
	beforeSuffix, beforeSuffixHead := replayedEventIDs(t, log, resume)

	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 7, 2)

	afterAll, afterHead := replayedEventIDs(t, log, Watermark{})
	if !slices.Equal(afterAll[:len(beforeAll)], beforeAll) {
		t.Fatalf("the sealed prefix replayed differently:\nbefore %v\nafter  %v", beforeAll, afterAll)
	}
	if len(afterAll) != len(beforeAll)+2 {
		t.Fatalf("replay returned %d records, want %d", len(afterAll), len(beforeAll)+2)
	}
	if afterHead.Sequence != 8 || afterHead.Generation != 2 {
		t.Fatalf("replay head = %#v", afterHead)
	}
	if beforeHead.Sequence != 6 || beforeHead.Generation != 1 {
		t.Fatalf("pre-roll replay head = %#v", beforeHead)
	}

	afterSuffix, afterSuffixHead := replayedEventIDs(t, log, resume)
	if !slices.Equal(afterSuffix[:len(beforeSuffix)], beforeSuffix) {
		t.Fatalf("resuming from a pre-roll watermark replayed differently:\nbefore %v\nafter  %v",
			beforeSuffix, afterSuffix)
	}
	if beforeSuffixHead.Sequence != 6 || afterSuffixHead.Sequence != 8 {
		t.Fatalf("suffix heads = %#v / %#v", beforeSuffixHead, afterSuffixHead)
	}

	// The same records, out of a compressed generation. Compaction is only
	// allowed to change how the bytes are stored.
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "ledger-1.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the plain generation survived compaction: %v", err)
	}
	compactedAll, compactedHead := replayedEventIDs(t, log, Watermark{})
	if !slices.Equal(compactedAll, afterAll) || compactedHead != afterHead {
		t.Fatalf("compaction changed what replay returns:\nbefore %v\nafter  %v", afterAll, compactedAll)
	}
	compactedSuffix, _ := replayedEventIDs(t, log, resume)
	if !slices.Equal(compactedSuffix, afterSuffix) {
		t.Fatalf("compaction changed a resumed replay:\nbefore %v\nafter  %v", afterSuffix, compactedSuffix)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

// 4 · A backup carries every generation.
//
// Staging only the active file would produce an archive that verifies, restores
// without complaint, and starts an instance whose balances begin at the last
// roll. Nothing about the result looks wrong, which is what makes it the worst
// shape this feature could fail in.
func TestStagingABackupCarriesEverySealedGeneration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 3)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 4, 2)
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	original, originalHead := replayedEventIDs(t, log, Watermark{})

	staging := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(staging, "ledger.wal")
	snapshotHead, err := log.Snapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := log.StageSegments(staging)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(staged, "ledger-1.seg.gz") || !slices.Contains(staged, segmentManifestName) {
		t.Fatalf("staged = %v", staged)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := OpenWithOptions(snapshotPath, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredAll, restoredHead := replayedEventIDs(t, restored, Watermark{})
	if !slices.Equal(restoredAll, original) {
		t.Fatalf("the restored ledger replays differently:\nsource  %v\nrestored %v", original, restoredAll)
	}
	if restoredHead != originalHead || restoredHead != snapshotHead {
		t.Fatalf("heads disagree: source=%#v restored=%#v snapshot=%#v",
			originalHead, restoredHead, snapshotHead)
	}
	if _, err := restored.VerifySealed(0); err != nil {
		t.Fatalf("the restored ledger's sealed history does not verify: %v", err)
	}
}

// A crash mid-roll, on both sides of the rename that commits it.
//
// The rename is the commit point, so a directory found afterwards has to be
// finished and one found before it has to be abandoned — and the decision has
// to come from the files, because a flag written by the process that crashed is
// exactly the thing that cannot be trusted.
func TestAnInterruptedRollIsFinishedOrAbandonedByTheFilesAlone(t *testing.T) {
	t.Run("before the rename", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "ledger.wal")
		log := openChained(t, path, nil)
		appendReservations(t, log, 1, 3)
		expected, expectedHead := replayedEventIDs(t, log, Watermark{})
		checksum, length, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		head, hash, _ := log.ChainHead()
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		// The intent, written; the rename, never reached.
		if err := saveSegmentManifest(directory, segmentManifest{Pending: &Segment{
			Generation: 1, File: "ledger-1.wal", FirstSequence: 1, LastSequence: head.Sequence,
			Length: length, StoredLength: length, EndHash: encodeChainHash(hash),
			PlainChecksum: checksum, StoredChecksum: checksum,
		}}); err != nil {
			t.Fatal(err)
		}

		reopened := openChained(t, path, nil)
		defer reopened.Close()
		if reopened.Generation() != 1 || len(reopened.Segments()) != 0 {
			t.Fatalf("an abandoned roll left generation=%d segments=%d",
				reopened.Generation(), len(reopened.Segments()))
		}
		got, gotHead := replayedEventIDs(t, reopened, Watermark{})
		if !slices.Equal(got, expected) || gotHead != expectedHead {
			t.Fatalf("abandoning the roll changed the ledger: %v vs %v", got, expected)
		}
	})

	t.Run("after the rename", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "ledger.wal")
		log := openChained(t, path, nil)
		appendReservations(t, log, 1, 3)
		expected, _ := replayedEventIDs(t, log, Watermark{})
		checksum, length, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		head, hash, _ := log.ChainHead()
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		if err := saveSegmentManifest(directory, segmentManifest{Pending: &Segment{
			Generation: 1, File: "ledger-1.wal", FirstSequence: 1, LastSequence: head.Sequence,
			Length: length, StoredLength: length, EndHash: encodeChainHash(hash),
			PlainChecksum: checksum, StoredChecksum: checksum,
		}}); err != nil {
			t.Fatal(err)
		}
		// The rename happened and the successor was never created, which is the
		// widest window the roll has.
		if err := os.Rename(path, filepath.Join(directory, "ledger-1.wal")); err != nil {
			t.Fatal(err)
		}

		reopened := openChained(t, path, nil)
		defer reopened.Close()
		if reopened.Generation() != 2 || len(reopened.Segments()) != 1 {
			t.Fatalf("a finished roll left generation=%d segments=%d",
				reopened.Generation(), len(reopened.Segments()))
		}
		got, gotHead := replayedEventIDs(t, reopened, Watermark{})
		if !slices.Equal(got, expected) {
			t.Fatalf("finishing the roll changed the ledger: %v vs %v", got, expected)
		}
		if gotHead.Generation != 2 || gotHead.Offset != 0 || gotHead.Sequence != 3 {
			t.Fatalf("head after a finished roll = %#v", gotHead)
		}
		// And it is writable from there, which is the whole point of finishing.
		appendReservations(t, reopened, 4, 1)
		if _, err := reopened.VerifySealed(0); err != nil {
			t.Fatal(err)
		}
	})
}
