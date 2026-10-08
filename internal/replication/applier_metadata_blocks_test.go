package replication

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestReplicaApplierMetadataPartialNativeCommitNeverAdvancesMemberApplied(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x42}, 32)
	path := filepath.Join(t.TempDir(), "metadata.db")
	seed, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.AttachMetadataJournal(key, "test seed"); err != nil {
		t.Fatal(err)
	}
	journalPath := seed.MetadataJournalPath()
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.OpenReplica(journalPath, key)
	if err != nil {
		t.Fatal(err)
	}
	// Authentic complete native transactions: the second cannot be materialized.
	for _, ops := range [][]metadatajournal.Op{
		{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("confirmed_one"), Value: []byte(`{"id":"confirmed_one","name":"one"}`)}},
		{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("partial_bad_frame"), Value: []byte(`{"id":"partial_bad_frame"}`)},
			{Kind: metadatajournal.OpPut, Path: []string{"missing_bucket"}, Key: []byte("x"), Value: []byte("x")}},
	} {
		// OpenReplica forbids local Append. Encode on the primary descriptor, then
		// reopen as the Replica's received, already-durable native prefix.
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		log, err = metadatajournal.Open(journalPath, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(ops); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = metadatajournal.OpenReplica(journalPath, key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.OpenReplica(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AttachReplicaMetadataJournal(log, key); err != nil {
		t.Fatal(err)
	}
	projection, _, _, _, _, _ := diskReplicaFixture(t)
	projection.metadata = store
	receiver, ordering := newTestReceiver(t, &recordingFrameSink{})
	defer ordering.Close()
	for i := uint64(1); i <= 3; i++ {
		frame := testConfirmationFrame(i, KindLeadershipEstablished)
		if i > 1 {
			frame.Kind = KindData
			frame.Store = StoreMetadata
			frame.StoreGeneration = log.Head().Epoch
			frame.StoreSequenceFirst = i - 1
			frame.StoreSequenceLast = i - 1
			frame.Metadata = nil
			frame.Payload, err = metadatajournal.ReadReplicationFrames(journalPath, key, log.Head().Epoch, i-1, i-1)
			if err != nil {
				t.Fatal(err)
			}
		}
		encoded, err := frame.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receiver.Receive(encoded); err != nil {
			t.Fatal(err)
		}
	}
	applier, err := NewReplicaApplier(receiver, ordering, projection)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		applied, err := applier.ApplyConfirmed(ctx)
		if err == nil || applied != 0 {
			t.Fatal(applied, err)
		}
		_, confirmed, memberApplied := receiver.Progress()
		if confirmed != 3 || memberApplied != 0 || receiver.ProjectionProgress().Index != 0 {
			t.Fatal("failed prefix published", receiver.ProjectionProgress())
		}
		native, err := store.MetadataJournalState()
		if err != nil || native.Applied != 1 {
			t.Fatal(native, err)
		}
		if _, err := store.GetProject(ctx, "confirmed_one"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetProject(ctx, "partial_bad_frame"); !errors.Is(err, boltstore.ErrNotFound) {
			t.Fatal("partial frame visible", err)
		}
	}
}
