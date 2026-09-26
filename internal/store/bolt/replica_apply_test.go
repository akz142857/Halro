package bolt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

func TestReplicaAppliesOnlyRequestedConfirmedMetadataPrefix(t *testing.T) {
	primaryDir := t.TempDir()
	replicaDir := t.TempDir()
	primaryPath := filepath.Join(primaryDir, "halro.db")
	primary, err := Open(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.AttachMetadataJournal(testJournalKey(), "test"); err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	// Capture the seeded projection and epoch-zero position before later
	// operations exist, as a seed transfer would.
	copyTestFile(t, primaryPath, filepath.Join(replicaDir, "halro.db"))
	copyTestFile(t, filepath.Join(primaryDir, MetadataJournalFileName), filepath.Join(replicaDir, MetadataJournalFileName))
	primary, err = Open(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if _, err := primary.AttachMetadataJournal(testJournalKey(), "test"); err != nil {
		t.Fatal(err)
	}
	if err := primary.update(func(tx *Tx) error {
		return tx.Bucket(bucketProjects).Put([]byte("p1"), []byte(`{"enabled":true}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := primary.update(func(tx *Tx) error {
		return tx.Bucket(bucketProjects).Put([]byte("p2"), []byte(`{"enabled":true}`))
	}); err != nil {
		t.Fatal(err)
	}
	copyTestFile(t, primary.MetadataJournalPath(), filepath.Join(replicaDir, MetadataJournalFileName))

	replica, err := OpenReplica(filepath.Join(replicaDir, "halro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	journal, err := metadatajournal.OpenReplica(filepath.Join(replicaDir, MetadataJournalFileName), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.AttachReplicaMetadataJournal(journal, testJournalKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.TrimMetadataJournal(0); err == nil {
		t.Fatal("replica accepted local metadata journal maintenance")
	}
	if err := replica.update(func(*Tx) error { return nil }); err == nil {
		t.Fatal("replica accepted a local metadata update")
	}
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), 2, 1); err == nil {
		t.Fatal("replica accepted a confirmed sequence from another metadata epoch")
	}
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := replica.view(func(tx *Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte("p1")) == nil {
			t.Fatal("confirmed metadata frame was not applied")
		}
		if tx.Bucket(bucketProjects).Get([]byte("p2")) != nil {
			t.Fatal("unconfirmed metadata frame was applied")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := replica.view(func(tx *Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte("p2")) == nil {
			t.Fatal("second confirmed metadata frame was not applied")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
