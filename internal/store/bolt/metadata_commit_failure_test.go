package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func TestMetadataPostNativeCommitFailurePermanentlyRevokesOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	s := openJournalledStore(t, path)
	retainedView, err := s.captureView()
	if err != nil {
		t.Fatal(err)
	}
	before := s.journal.Head()
	s.batches.commit = func(*bbolt.Tx) error { return syscall.ENOSPC }
	_, err = s.PutProject(ctx, domain.Project{ID: "durable", Name: "durable", Enabled: true}, 0, nil)
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, ErrMetadataRecoveryRequired) || s.journal.Head().Sequence != before.Sequence+1 {
		t.Fatal("failed commit did not preserve native failure evidence", err)
	}
	fault := s.RecoveryError()
	// Returning physical space cannot make the skipped native fact disappear.
	s.batches.commit = nil
	called := false
	if err := s.update(func(*Tx) error { called = true; return nil }); !errors.Is(err, ErrMetadataRecoveryRequired) || called {
		t.Fatal("retired owner admitted a later callback", err, called)
	}
	if err := retainedView(func(*Tx) error { called = true; return nil }); !errors.Is(err, ErrMetadataRecoveryRequired) || called {
		t.Fatal("retained view reopened a retired owner", err, called)
	}
	if _, err := s.GetProject(ctx, "durable"); !errors.Is(err, ErrMetadataRecoveryRequired) || errors.Is(err, ErrNotFound) {
		t.Fatal("retired projection served an apparently missing fact", err)
	}
	if _, err := s.AttachMetadataJournal(testJournalKey(), "in-process retry"); !errors.Is(err, ErrMetadataRecoveryRequired) || s.RecoveryError() != fault {
		t.Fatal("retry cleared failure authority", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	state, err := next.AttachMetadataJournal(testJournalKey(), "reopen commit failure")
	if err != nil || state.Replayed != 1 || state.Applied != before.Sequence+1 || next.RecoveryError() != nil {
		t.Fatal("new owner did not authenticate and replay missing native work", state, err)
	}
	if _, err := next.GetProject(ctx, "durable"); err != nil {
		t.Fatal("recovery lost durable fact", err)
	}
	if _, err := next.PutProject(ctx, domain.Project{ID: "later", Name: "later", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataAlreadyAdmittedWriterCannotCrossCommitFailure(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	started, finish := make(chan struct{}), make(chan struct{})
	s.batches.commit = func(*bbolt.Tx) error {
		close(started)
		<-finish
		return syscall.ENOSPC
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- s.update(func(tx *Tx) error { return tx.Bucket(bucketProjects).Put([]byte("first"), []byte(`{"id":"first"}`)) })
	}()
	<-started
	called := false
	go func() { second <- s.update(func(*Tx) error { called = true; return nil }) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.lifetime.mu.Lock()
		admitted := s.lifetime.active
		s.lifetime.mu.Unlock()
		if admitted >= 2 {
			break
		}
		if time.Now().After(deadline) {
			close(finish)
			t.Fatal("second writer did not obtain its lifetime ticket")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	if err := <-first; !errors.Is(err, ErrMetadataRecoveryRequired) {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrMetadataRecoveryRequired) || called {
		t.Fatal("writer waiting on native DB lock crossed retired boundary", err, called)
	}
}

func TestMetadataDefiniteCallbackRefusalDoesNotRetireStore(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	before := s.journal.Head()
	refusal := errors.New("definite callback refusal")
	if err := s.update(func(*Tx) error { return refusal }); !errors.Is(err, refusal) || s.RecoveryError() != nil || s.journal.Head() != before {
		t.Fatal("definite refusal retired or changed native authority", err)
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "later", Name: "later", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataUnrecordedProjectionCommitFailureRetiresOwner(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	s.batches.commit = func(*bbolt.Tx) error { return syscall.ENOSPC }
	if err := s.migrate(func(*Tx) error { return nil }); !errors.Is(err, ErrMetadataRecoveryRequired) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("unrecorded physical commit failure did not revoke owner", err)
	}
	if _, err := s.SchemaVersion(); !errors.Is(err, ErrMetadataRecoveryRequired) {
		t.Fatal("physical failure served stale schema", err)
	}
}

func TestMetadataPostDurableOrderingFailureRetiresOwner(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	before := s.journal.Head()
	orderingErr := errors.New("ordering fsync failed")
	if err := s.SetMetadataJournalAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) { return 0, orderingErr }, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := s.PutProject(context.Background(), domain.Project{ID: "durable", Name: "durable", Enabled: true}, 0, nil)
	if !errors.Is(err, orderingErr) || !errors.Is(err, ErrMetadataRecoveryRequired) || s.journal.Head().Sequence != before.Sequence+1 {
		t.Fatal("post-durable failure lost recovery requirement", err)
	}
	if _, err := s.GetProject(context.Background(), "durable"); !errors.Is(err, ErrMetadataRecoveryRequired) {
		t.Fatal("post-durable failure still served old projection", err)
	}
}
