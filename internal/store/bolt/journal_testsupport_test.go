package bolt

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

// testJournalKey is a fixed key. The real one is derived from the Master Key
// through HKDF; what these tests need from it is only that it is the same key
// across an open and a reopen.
func testJournalKey() []byte { return bytes.Repeat([]byte{0x7f}, metadatajournal.KeySize) }

// openJournalledStore is what every test that writes authoritative metadata
// needs, because the entry refuses to write anything it cannot record.
func openJournalledStore(t testing.TB, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachMetadataJournal(testJournalKey(), "test"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	return openJournalledStore(b, filepath.Join(b.TempDir(), "metadata.db"))
}

// openForTest is Open plus the attach every write path now requires.
//
// The gate is fail-closed by design — a metadata write that nothing would
// record is refused — so a test that opened a bare store could not write. This
// keeps the tests reading the way they did while still going through the entry
// they are meant to exercise, rather than being given a way around it.
func openForTest(tb testing.TB, path string) (*Store, error) {
	tb.Helper()
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := store.AttachMetadataJournal(testJournalKey(), "test"); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}
