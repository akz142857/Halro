package metadatajournal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{0x2a}, KeySize) }

func newJournal(t *testing.T) (string, *Log) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.journal")
	log, err := StartEpoch(path, testKey(), EpochHeader{Epoch: 1, Reason: "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return path, log
}

func put(bucket, key, value string) Op {
	return Op{Kind: OpPut, Path: []string{bucket}, Key: []byte(key), Value: []byte(value)}
}

// TestAppendedFramesAuthenticateAndChain is the baseline the rest rests on.
func TestAppendedFramesAuthenticateAndChain(t *testing.T) {
	path, log := newJournal(t)
	for index := range 5 {
		if _, err := log.Append([]Op{put("projects", "p", string(rune('a'+index)))}); err != nil {
			t.Fatal(err)
		}
	}
	head, err := Verify(path, testKey())
	if err != nil {
		t.Fatal(err)
	}
	if head.Epoch != 1 || head.Sequence != 5 {
		t.Fatalf("head is epoch %d sequence %d, want 1/5", head.Epoch, head.Sequence)
	}
	if head != log.Head() {
		t.Fatalf("the writer's head %+v does not match the file's %+v", log.Head(), head)
	}
}

// TestReplayReturnsTheOperationsThatWereRecorded. The journal is only useful
// if what comes back out is what a bbolt transaction did.
func TestReplayReturnsTheOperationsThatWereRecorded(t *testing.T) {
	path, log := newJournal(t)
	written := [][]Op{
		{put("projects", "p1", "one"), {Kind: OpDelete, Path: []string{"routes"}, Key: []byte("r1")}},
		{{Kind: OpCreateBucket, Path: []string{"deployment_price_timeline", "dep_1"}}},
		{put("deployment_price_timeline/dep_1", "v1", "price")},
	}
	for _, ops := range written {
		if _, err := log.Append(ops); err != nil {
			t.Fatal(err)
		}
	}
	var replayed [][]Op
	if _, err := Replay(path, testKey(), func(record Record) error {
		if record.Kind == KindOperations {
			replayed = append(replayed, record.Ops)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(written) {
		t.Fatalf("replayed %d transactions, wrote %d", len(replayed), len(written))
	}
	for index := range written {
		if len(replayed[index]) != len(written[index]) {
			t.Fatalf("transaction %d replayed %d operations, wrote %d", index, len(replayed[index]), len(written[index]))
		}
		for op := range written[index] {
			want, got := written[index][op], replayed[index][op]
			if want.Kind != got.Kind || len(want.Path) != len(got.Path) ||
				!bytes.Equal(want.Key, got.Key) || !bytes.Equal(want.Value, got.Value) {
				t.Fatalf("transaction %d operation %d came back as %+v, wrote %+v", index, op, got, want)
			}
		}
	}
}

// TestATornTailIsRepairedRatherThanRefused.
//
// A frame is fsynced before the bbolt transaction it belongs to commits, so a
// crash mid-append leaves a partial frame describing work that was never
// acknowledged to anyone and never reached the projection either. Refusing to
// open would turn a clean crash into an instance that will not start.
func TestATornTailIsRepairedRatherThanRefused(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "p1", "one")}); err != nil {
		t.Fatal(err)
	}
	intact := log.Head()
	if _, err := log.Append([]Op{put("projects", "p2", "two")}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	// Cut the final frame in half.
	size := fileSize(t, path)
	if err := os.Truncate(path, (intact.Offset+size)/2); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testKey())
	if err != nil {
		t.Fatalf("a torn tail refused to open: %v", err)
	}
	defer reopened.Close()
	if reopened.Head().Sequence != intact.Sequence {
		t.Fatalf("repaired head is sequence %d, want %d", reopened.Head().Sequence, intact.Sequence)
	}
	// And the repaired file is appendable again, chaining from the intact head.
	if _, err := reopened.Append([]Op{put("projects", "p3", "three")}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, testKey()); err != nil {
		t.Fatalf("the repaired journal does not verify: %v", err)
	}
}

// TestAFlippedByteIsCorruptionNotACrash. The distinction matters: a torn tail
// is repaired, and anything else must stop the instance rather than be trimmed
// away, because trimming it would discard a description of writes the
// projection may already hold.
func TestAFlippedByteIsCorruptionNotACrash(t *testing.T) {
	path, log := newJournal(t)
	for range 3 {
		if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Somewhere inside the second frame's payload.
	contents[len(contents)/2] ^= 0xff
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, testKey()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a flipped byte produced %v, want ErrCorrupt", err)
	}
}

// TestAnotherKeyCannotAppend is the point of the MAC domain: frames are not a
// thing anyone holding the data directory can write.
func TestAnotherKeyCannotAppend(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	other := bytes.Repeat([]byte{0x55}, KeySize)
	if _, err := Verify(path, other); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a foreign key verified the chain: %v", err)
	}
}

// TestAnEpochStartsAFreshFileAndRecordsWhatItReplaced.
//
// A whole-file publish of halro.db — a restore, a key rotation bridge, a
// compaction — makes the frames before it stop describing the database. The
// new file is the only place the old chain head survives, and that record is
// what lets a reader tell a deliberate publish from a deleted journal.
func TestAnEpochStartsAFreshFileAndRecordsWhatItReplaced(t *testing.T) {
	path, log := newJournal(t)
	for range 4 {
		if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	previous := log.Head()
	log.Close()

	next, err := StartEpoch(path, testKey(), EpochHeader{
		Epoch: previous.Epoch + 1, PreviousEpoch: previous.Epoch,
		PreviousChainHead: previous.Hash[:], Reason: "backup restore",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Head().Epoch != 2 || next.Head().Sequence != 0 {
		t.Fatalf("the new epoch opens at %+v", next.Head())
	}
	var header EpochHeader
	if _, err := Replay(path, testKey(), func(record Record) error {
		if record.Kind == KindEpoch {
			header = record.Header
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if header.PreviousEpoch != 1 || !bytes.Equal(header.PreviousChainHead, previous.Hash[:]) {
		t.Fatalf("the epoch header does not record what it replaced: %+v", header)
	}
	if header.Reason != "backup restore" {
		t.Fatalf("the epoch does not say why it started: %q", header.Reason)
	}
	// And the old frames are gone, which is the half of the rule that closes
	// the old-Master-Key-plus-journal path.
	if _, err := Replay(path, testKey(), func(record Record) error {
		if record.Kind == KindOperations && record.Epoch == 1 {
			return errors.New("an operations frame from the replaced epoch survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAnEpochMustMoveForward. Reusing or lowering an epoch would make two
// different projections answer to one number.
func TestAnEpochMustMoveForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.journal")
	for _, header := range []EpochHeader{
		{Epoch: 0, Reason: "initialize"},
		{Epoch: 3, PreviousEpoch: 3, Reason: "restore"},
		{Epoch: 2, PreviousEpoch: 5, Reason: "restore"},
		{Epoch: 1},
	} {
		if _, err := StartEpoch(path, testKey(), header); err == nil {
			t.Fatalf("StartEpoch accepted %+v", header)
		}
	}
}

// TestAMidFileEpochHeaderIsRefused. Two chains in one file means nothing can
// say which of them the projection follows.
func TestAMidFileEpochHeaderIsRefused(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodePayload(KindEpoch, nil, EpochHeader{Epoch: 2, PreviousEpoch: 1, Reason: "forged"}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := encodeFrame(testKey(), KindEpoch, 2, 0, [32]byte{}, payload)
	if err := os.WriteFile(path, append(contents, frame...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, testKey()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a second epoch header inside one file verified: %v", err)
	}
}

// TestTrimKeepsTheTailAndSignsWhereItCut.
//
// Standalone writes two price-pin frames per Attempt with a Deployment, so a
// journal that never shrinks grows with traffic rather than with
// administration. What a trim must not do is become indistinguishable from
// somebody deleting the head of the file.
func TestTrimKeepsTheTailAndSignsWhereItCut(t *testing.T) {
	path, log := newJournal(t)
	for index := range 10 {
		if _, err := log.Append([]Op{put("projects", "p", string(rune('a'+index)))}); err != nil {
			t.Fatal(err)
		}
	}
	before := log.Head()
	log.Close()

	head, err := Trim(path, testKey(), 6)
	if err != nil {
		t.Fatal(err)
	}
	if head.Sequence != before.Sequence || head.Hash != before.Hash {
		t.Fatalf("trimming moved the head: %+v, was %+v", head, before)
	}
	if head.TrimmedThrough != 6 {
		t.Fatalf("the head does not report the cut: %+v", head)
	}
	var anchor TrimAnchor
	var survivors []uint64
	if _, err := Replay(path, testKey(), func(record Record) error {
		switch record.Kind {
		case KindTrim:
			anchor = record.Trim
		case KindOperations:
			survivors = append(survivors, record.Sequence)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if anchor.TrimmedThrough != 6 || anchor.TrimmedFrameCount != 6 {
		t.Fatalf("the anchor does not describe the cut: %+v", anchor)
	}
	if len(survivors) != 4 || survivors[0] != 7 || survivors[3] != 10 {
		t.Fatalf("survivors are %v, want 7..10", survivors)
	}
	// A trimmed journal is still appendable, and still one chain.
	reopened, err := Open(path, testKey())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Append([]Op{put("projects", "p", "after")}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, testKey()); err != nil {
		t.Fatalf("an appended trimmed journal does not verify: %v", err)
	}
}

// TestTrimRefusesToGoBackwards. A second trim that claims a lower cut than the
// anchor already records would be rewriting history rather than shortening it.
func TestTrimRefusesToGoBackwards(t *testing.T) {
	path, log := newJournal(t)
	for range 8 {
		if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()
	if _, err := Trim(path, testKey(), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := Trim(path, testKey(), 3); err == nil {
		t.Fatal("a backwards trim was accepted")
	}
	if _, err := Trim(path, testKey(), 99); err == nil {
		t.Fatal("a trim past the head was accepted")
	}
	// And an idempotent re-trim at the same point is a no-op rather than a
	// rewrite of a durable artefact.
	head, err := Trim(path, testKey(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if head.TrimmedThrough != 5 {
		t.Fatalf("re-trimming at the same point moved the anchor: %+v", head)
	}
}

// TestEmptyAndHeaderlessFilesAreRefused. A journal that opens with operations,
// or with nothing, is not a journal this writer produced — and treating it as
// an empty one would silently accept a file whose head somebody removed.
func TestEmptyAndHeaderlessFilesAreRefused(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.journal")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(empty, testKey()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an empty file verified: %v", err)
	}
	headerless := filepath.Join(directory, "headerless.journal")
	payload, err := encodePayload(KindOperations, []Op{put("projects", "p", "v")}, EpochHeader{}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := encodeFrame(testKey(), KindOperations, 1, 1, [32]byte{}, payload)
	if err := os.WriteFile(headerless, frame, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(headerless, testKey()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a file with no epoch header verified: %v", err)
	}
}

// TestAnOversizedTransactionIsRefusedRatherThanWritten. The limit exists so a
// length field nobody checks cannot turn a corrupt header into an allocation;
// refusing at write time is what keeps the reader's limit reachable.
func TestAnOversizedTransactionIsRefusedRatherThanWritten(t *testing.T) {
	path, log := newJournal(t)
	huge := make([]byte, MaxPayloadSize)
	if _, err := log.Append([]Op{{Kind: OpPut, Path: []string{"credentials"}, Key: []byte("k"), Value: huge}}); err == nil {
		t.Fatal("an oversized transaction was written")
	}
	if _, err := Verify(path, testKey()); err != nil {
		t.Fatalf("the refused append damaged the file: %v", err)
	}
}

// TestAnEmptyTransactionIsRefused. A frame recording nothing costs an fsync and
// says nothing; a caller reaching this has a bug in its recorder.
func TestAnEmptyTransactionIsRefused(t *testing.T) {
	_, log := newJournal(t)
	if _, err := log.Append(nil); err == nil {
		t.Fatal("an empty transaction was recorded")
	}
	if _, err := log.Append([]Op{{Kind: "nonsense", Path: []string{"projects"}}}); err == nil {
		t.Fatal("an unknown operation was recorded")
	}
	if _, err := log.Append([]Op{{Kind: OpPut, Path: nil, Key: []byte("k")}}); err == nil {
		t.Fatal("an operation with no bucket path was recorded")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
