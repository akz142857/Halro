package replication

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

type fakeDurableSource struct {
	cursors     map[Store]StoreCursor
	heads       map[Store][32]byte
	payloads    map[string][]byte
	truncated   map[Store]StoreCursor
	truncateErr error
	pendingRoll []byte
}

func (s *fakeDurableSource) PendingLedgerRoll(uint64) ([]byte, error) {
	if len(s.pendingRoll) == 0 {
		return nil, errors.New("pending Ledger Roll is missing")
	}
	return append([]byte(nil), s.pendingRoll...), nil
}

func sourceKey(store Store, generation, first, last uint64) string {
	return fmt.Sprintf("%d/%d/%d/%d", store, generation, first, last)
}

func (s *fakeDurableSource) Cursor(store Store) (StoreCursor, error) {
	return s.cursors[store], nil
}

func (s *fakeDurableSource) HeadAt(store Store, _ StoreCursor) ([32]byte, error) {
	return s.heads[store], nil
}

func (s *fakeDurableSource) Read(store Store, generation, first, last uint64) ([]byte, error) {
	payload, ok := s.payloads[sourceKey(store, generation, first, last)]
	if !ok {
		return nil, errors.New("source range is missing")
	}
	return append([]byte(nil), payload...), nil
}

func (s *fakeDurableSource) Truncate(store Store, cursor StoreCursor) error {
	if s.truncateErr != nil {
		return s.truncateErr
	}
	s.cursors[store] = cursor
	s.truncated[store] = cursor
	return nil
}

func TestRecoverLocalCommitsTruncatesUnorderedTailAndRebuildsExactSuffix(t *testing.T) {
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	payload := []byte("ordered-ledger-frame")
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	source := &fakeDurableSource{
		cursors:   map[Store]StoreCursor{StoreLedger: {Generation: 1, Sequence: 2}},
		payloads:  map[string][]byte{sourceKey(StoreLedger, 1, 1, 1): payload},
		truncated: make(map[Store]StoreCursor),
	}
	commits, err := RecoverLocalCommits("production-a", "inc_01", 1, journal, source)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.truncated[StoreLedger]; got != (StoreCursor{Generation: 1, Sequence: 1}) {
		t.Fatalf("truncated ledger cursor=%#v", got)
	}
	if len(commits) != 1 || commits[0].Index != 2 {
		t.Fatalf("recovered commits=%#v", commits)
	}
	frame, digest, err := UnmarshalFrameAndDigest(commits[0].Encoded)
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.Record(2)
	if err != nil {
		t.Fatal(err)
	}
	if digest != record.FrameDigest || frame.ConfirmedIndex != record.ConfirmedIndex || string(frame.Payload) != string(payload) {
		t.Fatalf("recovered frame=%#v digest=%x record=%x", frame, digest, record.FrameDigest)
	}
}

func TestRecoverLocalCommitsFailsClosedOnMissingOrDifferentSourceBytes(t *testing.T) {
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("expected"),
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		source *fakeDurableSource
	}{
		{name: "behind", source: &fakeDurableSource{cursors: map[Store]StoreCursor{}, payloads: map[string][]byte{}, truncated: map[Store]StoreCursor{}}},
		{name: "different", source: &fakeDurableSource{
			cursors:   map[Store]StoreCursor{StoreLedger: {Generation: 1, Sequence: 1}},
			payloads:  map[string][]byte{sourceKey(StoreLedger, 1, 1, 1): []byte("different")},
			truncated: map[Store]StoreCursor{},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RecoverLocalCommits("production-a", "inc_01", 1, journal, test.source); err == nil {
				t.Fatal("source divergence was accepted")
			}
		})
	}
}

func TestRecoverLocalCommitsStartsFromAuthenticatedSeedBaseline(t *testing.T) {
	directory := t.TempDir()
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreLedger-StoreLedger] = StoreCursor{Generation: 2, Sequence: 40}
	header.StoreHeads[StoreLedger-StoreLedger] = [32]byte{9}
	journal, err := OpenOrderingJournal(
		filepath.Join(directory, "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"), header, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	payload := []byte("post-seed-frame")
	anchor := Frame{
		Kind: KindLeadershipEstablished, Index: 1, Term: 1,
		ClusterID: "production-a", Incarnation: "inc_01",
	}
	_, anchorDigest, err := anchor.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(OrderingRecord{
		Kind: KindLeadershipEstablished, Index: 1, Term: 1, FrameDigest: anchorDigest,
	}); err != nil {
		t.Fatal(err)
	}
	frame := Frame{
		Kind: KindData, Store: StoreLedger, Index: 2, Term: 1, ConfirmedIndex: 1,
		StoreGeneration: 2, StoreSequenceFirst: 41, StoreSequenceLast: 41,
		ClusterID: "production-a", Incarnation: "inc_01", Payload: payload,
	}
	_, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(OrderingRecord{
		Kind: KindData, Store: StoreLedger, Index: 2, Term: 1, ConfirmedIndex: 1,
		StoreGeneration: 2, StoreSequenceFirst: 41, StoreSequenceLast: 41,
		FrameDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	source := &fakeDurableSource{
		cursors:   map[Store]StoreCursor{StoreLedger: {Generation: 2, Sequence: 42}},
		heads:     map[Store][32]byte{StoreLedger: {9}},
		payloads:  map[string][]byte{sourceKey(StoreLedger, 2, 41, 41): payload},
		truncated: make(map[Store]StoreCursor),
	}
	commits, err := RecoverLocalCommits("production-a", "inc_01", 1, journal, source)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.truncated[StoreLedger]; got != (StoreCursor{Generation: 2, Sequence: 41}) {
		t.Fatalf("truncated to %#v", got)
	}
	if len(commits) != 1 || commits[0].Index != 2 {
		t.Fatalf("commits=%#v", commits)
	}
}

func TestRecoverLocalCommitsRejectsSameCursorDifferentBaselineHistory(t *testing.T) {
	directory := t.TempDir()
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreLedger-StoreLedger] = StoreCursor{Generation: 1, Sequence: 1}
	header.StoreHeads[StoreLedger-StoreLedger] = [32]byte{1}
	journal, err := OpenOrderingJournal(
		filepath.Join(directory, "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"), header, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	source := &fakeDurableSource{
		cursors:   map[Store]StoreCursor{StoreLedger: {Generation: 1, Sequence: 1}},
		heads:     map[Store][32]byte{StoreLedger: {2}},
		payloads:  map[string][]byte{},
		truncated: map[Store]StoreCursor{},
	}
	if _, err := RecoverLocalCommits("production-a", "inc_01", 0, journal, source); err == nil {
		t.Fatal("same cursor with a different authenticated Ledger history was accepted")
	}
}

func TestRecoverLocalCommitsRejectsDifferentMetadataEpochHeader(t *testing.T) {
	directory := t.TempDir()
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreMetadata-StoreLedger] = StoreCursor{Generation: 1}
	header.StoreHeads[StoreMetadata-StoreLedger] = [32]byte{1}
	journal, err := OpenOrderingJournal(
		filepath.Join(directory, "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"), header, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	source := &fakeDurableSource{
		cursors:   map[Store]StoreCursor{StoreMetadata: {Generation: 1}},
		heads:     map[Store][32]byte{StoreMetadata: {2}},
		payloads:  map[string][]byte{},
		truncated: map[Store]StoreCursor{},
	}
	if _, err := RecoverLocalCommits("production-a", "inc_01", 0, journal, source); err == nil {
		t.Fatal("same metadata epoch/sequence with a different epoch-header history was accepted")
	}
}

func TestRecoverLocalCommitsCompletesNativeLedgerRollBeforeRequeue(t *testing.T) {
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("ledger-frame"),
	}); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := primary.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	rollBytes, err := (LedgerRollMetadata{
		Generation: 1, FirstSequence: 1, LastSequence: 1, Length: 64, EndEpoch: 4,
		StartHash: [32]byte{1}, EndHash: [32]byte{2}, PlainChecksum: [32]byte{3}, SealedAtUnix: 123,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeDurableSource{
		cursors: map[Store]StoreCursor{
			StoreLedger: {Generation: 2, Sequence: 1},
		},
		payloads:    map[string][]byte{},
		truncated:   make(map[Store]StoreCursor),
		pendingRoll: rollBytes,
	}
	commits, err := RecoverLocalCommits("production-a", "inc_01", 2, journal, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Index != 3 {
		t.Fatalf("recovered commits=%#v", commits)
	}
	frame, err := UnmarshalFrame(commits[0].Encoded)
	if err != nil || frame.Kind != KindLedgerRoll || frame.ConfirmedIndex != 2 {
		t.Fatalf("recovered Roll=%#v err=%v", frame, err)
	}
	if head, _, _ := journal.Head(); head != 3 {
		t.Fatalf("ordering head=%d, want recovered Roll at 3", head)
	}
	// A second restart authenticates the completed transaction; it neither
	// appends another Roll nor tries to undo the sealed generation.
	commits, err = RecoverLocalCommits("production-a", "inc_01", 2, journal, source)
	if err != nil || len(commits) != 1 || commits[0].Index != 3 {
		t.Fatalf("second recovery commits=%#v err=%v", commits, err)
	}
}
