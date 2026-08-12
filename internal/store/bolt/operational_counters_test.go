package bolt

import (
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestShutdownTruncatedAttemptsPersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if total, err := store.ShutdownTruncatedAttempts(); err != nil || total != 0 {
		t.Fatalf("initial total=%d err=%v", total, err)
	}
	if total, err := store.AddShutdownTruncatedAttempts(2); err != nil || total != 2 {
		t.Fatalf("first total=%d err=%v", total, err)
	}
	if total, err := store.AddShutdownTruncatedAttempts(3); err != nil || total != 5 {
		t.Fatalf("second total=%d err=%v", total, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if total, err := store.ShutdownTruncatedAttempts(); err != nil || total != 5 {
		t.Fatalf("reopened total=%d err=%v", total, err)
	}
}

// A corrupt counter has to surface as an error rather than as a plausible
// number. The metrics exposition treats that error as "omit this series", and
// omission is only the right answer if the read actually reports failure —
// a silent zero would be indistinguishable from a clean shutdown history.
func TestShutdownTruncatedAttemptsReportsACorruptCounterRatherThanZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyShutdownTruncatedAttempts, []byte{1, 2, 3})
	}); err != nil {
		t.Fatal(err)
	}
	total, err := store.ShutdownTruncatedAttempts()
	if err == nil {
		t.Fatalf("a three-byte counter was accepted as total=%d", total)
	}
	if total != 0 {
		t.Fatalf("a failed read returned total=%d, which a caller could publish", total)
	}
}
