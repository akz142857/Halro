package replication

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingOrderingDurability struct {
	file      *os.File
	syncCalls int
}

type headerSyncOrderingDurability struct {
	file      *os.File
	syncCalls *int
	fail      bool
}

func (d *headerSyncOrderingDurability) Write(value []byte) (int, error) { return d.file.Write(value) }
func (d *headerSyncOrderingDurability) Sync() error {
	*d.syncCalls++
	if d.fail {
		return errors.New("injected header fsync failure")
	}
	return d.file.Sync()
}

func (d *failingOrderingDurability) Write(value []byte) (int, error) { return d.file.Write(value) }

func (d *failingOrderingDurability) Sync() error {
	d.syncCalls++
	if d.syncCalls == 2 {
		return errors.New("injected ordering fsync failure")
	}
	return d.file.Sync()
}

func testOrderingRecord(index, term uint64, label string) OrderingRecord {
	return OrderingRecord{
		Kind: KindData, Store: StoreLedger, Index: index, Term: term,
		StoreGeneration:    1,
		StoreSequenceFirst: index, StoreSequenceLast: index,
		FrameDigest: sha256.Sum256([]byte(label)),
	}
}

func testLeadershipRecord(index, term uint64, label string) OrderingRecord {
	return OrderingRecord{
		Kind: KindLeadershipEstablished, Store: StoreNone, Index: index, Term: term,
		FrameDigest: sha256.Sum256([]byte(label)),
	}
}

func TestOrderingJournalCreatesAppendsAndRecoversAuthenticatedHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(testLeadershipRecord(1, 1, "one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Append(testOrderingRecord(2, 1, "two"))
	if err != nil {
		t.Fatal(err)
	}
	if second.PreviousMAC != first.MAC {
		t.Fatal("append did not chain the second record to the first")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenOrderingJournal(path, key, header, 2, second.MAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	index, term, head := reopened.Head()
	if index != 2 || term != 1 || head != second.MAC {
		t.Fatalf("recovered head=(%d,%d,%x)", index, term, head)
	}
	third, err := reopened.Append(testLeadershipRecord(3, 2, "three"))
	if err != nil {
		t.Fatal(err)
	}
	if third.PreviousMAC != second.MAC {
		t.Fatal("reopened journal did not continue the authenticated chain")
	}
}

func TestOrderingJournalRetriesDirectoryBarrierAfterCreatedFileSyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	if _, err := OpenOrderingJournalWithOptions(path, key, header, 0, [sha256.Size]byte{}, OrderingJournalOptions{
		SyncDirectory: func(string) error { return errors.New("injected directory sync failure") },
	}); err == nil || !strings.Contains(err.Error(), "directory sync failure") {
		t.Fatalf("first open error=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("created journal did not remain for retry: %v", err)
	}
	syncCalls := 0
	journal, err := OpenOrderingJournalWithOptions(path, key, header, 0, [sha256.Size]byte{}, OrderingJournalOptions{
		SyncDirectory: func(string) error { syncCalls++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if syncCalls != 1 {
		t.Fatalf("retry directory sync calls=%d, want 1", syncCalls)
	}
}

func TestOrderingJournalRetriesHeaderFileBarrierAfterCreatedFileSyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	firstSyncCalls := 0
	if _, err := OpenOrderingJournalWithOptions(path, key, header, 0, [sha256.Size]byte{}, OrderingJournalOptions{
		WrapDurability: func(file *os.File) OrderingDurability {
			return &headerSyncOrderingDurability{file: file, syncCalls: &firstSyncCalls, fail: true}
		},
	}); err == nil || !strings.Contains(err.Error(), "header fsync failure") {
		t.Fatalf("first open error=%v", err)
	}
	if firstSyncCalls != 1 {
		t.Fatalf("first header sync calls=%d, want 1", firstSyncCalls)
	}
	retrySyncCalls := 0
	journal, err := OpenOrderingJournalWithOptions(path, key, header, 0, [sha256.Size]byte{}, OrderingJournalOptions{
		WrapDurability: func(file *os.File) OrderingDurability {
			return &headerSyncOrderingDurability{file: file, syncCalls: &retrySyncCalls}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if retrySyncCalls != 1 {
		t.Fatalf("retry header sync calls=%d, want 1", retrySyncCalls)
	}
}

func TestOrderingJournalTruncatesOnlyAPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(testLeadershipRecord(1, 1, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	clean, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, 7)
	binary.BigEndian.PutUint32(partial[:4], orderingRecordBodyBytes)
	copy(partial[4:], []byte{1, 2, 3})
	if _, err := file.Write(partial); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOrderingJournal(path, key, header, 1, first.MAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != clean.Size() {
		t.Fatalf("partial tail size=%d, want %d", after.Size(), clean.Size())
	}
}

func TestOrderingJournalRefusesDeletedWholeSuffixAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(testLeadershipRecord(1, 1, "one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Append(testOrderingRecord(2, 1, "two"))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-orderingRecordBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOrderingJournal(path, key, header, 2, second.MAC); err == nil || !strings.Contains(err.Error(), "missing an authenticated suffix") {
		t.Fatalf("deleted suffix error=%v", err)
	}
	prefix, err := OpenOrderingJournal(path, key, header, 1, first.MAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := prefix.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents[len(contents)-1] ^= 1
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOrderingJournal(path, key, header, 1, first.MAC); err == nil || !strings.Contains(err.Error(), "MAC mismatch") {
		t.Fatalf("corruption error=%v", err)
	}
}

func TestOrderingJournalAllowsAuthenticatedJournalAheadOfState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(testLeadershipRecord(1, 1, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(testOrderingRecord(2, 1, "two")); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOrderingJournal(path, key, header, 1, first.MAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if index, _, _ := reopened.Head(); index != 2 {
		t.Fatalf("journal-ahead head=%d, want 2", index)
	}
}

func TestOrderingJournalRefusesGapsAndTermRegression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	journal, err := OpenOrderingJournal(path, key, OrderingHeader{ClusterID: "c", Incarnation: "i"}, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.Append(testOrderingRecord(2, 1, "gap")); err == nil || !strings.Contains(err.Error(), "does not continue") {
		t.Fatalf("gap error=%v", err)
	}
	if _, err := journal.Append(testLeadershipRecord(1, 2, "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(testOrderingRecord(2, 1, "regress")); err == nil || !strings.Contains(err.Error(), "regresses") {
		t.Fatalf("term error=%v", err)
	}
}

func TestOrderingJournalPoisonsAfterUncertainFsync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	journal, err := OpenOrderingJournalWithOptions(
		path, key, OrderingHeader{ClusterID: "c", Incarnation: "i"}, 0, [sha256.Size]byte{},
		OrderingJournalOptions{WrapDurability: func(file *os.File) OrderingDurability {
			return &failingOrderingDurability{file: file}
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.Append(testLeadershipRecord(1, 1, "one")); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("fsync error=%v", err)
	}
	if _, err := journal.Append(testLeadershipRecord(1, 1, "retry")); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}

func TestOrderingJournalDoesNotCreateMissingFileRequiredByState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	head := sha256.Sum256([]byte("head"))
	if _, err := OpenOrderingJournal(path, []byte("0123456789abcdef0123456789abcdef"), OrderingHeader{ClusterID: "c", Incarnation: "i"}, 1, head); err == nil || !strings.Contains(err.Error(), "required by member state") {
		t.Fatalf("missing journal error=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing required journal was created: %v", err)
	}
}
