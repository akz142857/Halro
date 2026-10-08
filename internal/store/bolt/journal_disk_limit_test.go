package bolt

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestMetadataJournalCeilingRefusesUpdateBeforeProjectionCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "halro.db")
	const ceiling = int64(4 << 10)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachMetadataJournal(testJournalKey(), "disk ceiling test", ceiling); err != nil {
		t.Fatal(err)
	}
	before := store.journal.Head()
	err = store.update(func(tx *Tx) error {
		return tx.Bucket(bucketProjects).Put([]byte("disk-limit"), bytes.Repeat([]byte("x"), 8<<10))
	})
	if err == nil {
		t.Fatal("projection update committed beyond the configured Metadata journal ceiling")
	}
	if after := store.journal.Head(); after != before {
		t.Fatalf("refused transaction changed the journal head: before=%+v after=%+v", before, after)
	}
	if err := store.view(func(tx *Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte("disk-limit")) != nil {
			t.Fatal("projection transaction committed despite journal capacity refusal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.AttachMetadataJournal(testJournalKey(), "disk ceiling reopen", ceiling)
	if err != nil {
		t.Fatal("refused append poisoned the journal or prevented restart:", err)
	}
	if state.Sequence != before.Sequence {
		t.Fatalf("reopened position changed: got %d, want %d", state.Sequence, before.Sequence)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
