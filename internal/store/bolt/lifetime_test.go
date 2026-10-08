package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func lifetimeWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("lifetime transition did not occur")
	}
}

func lifetimeResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("lifetime operation did not finish")
		return nil
	}
}

func lifetimeEventually(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatal("lifetime state did not become ready")
		case <-tick.C:
		}
	}
}

func lifetimeRevoked(s *Store) <-chan struct{} {
	s.lifetime.mu.Lock()
	defer s.lifetime.mu.Unlock()
	return s.lifetime.ctx.Done()
}

func lifetimeStillOpen(t *testing.T, s *Store, closed <-chan error) {
	t.Helper()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while an operation still owned its handles: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// This deliberately bypasses admission to prove physical Close has not
	// happened yet, rather than merely checking a logical lifetime flag.
	if err := s.db.View(func(*bbolt.Tx) error { return nil }); err != nil {
		t.Fatalf("native DB closed before owner exit: %v", err)
	}
}

func TestMetadataCloseJoinsTransactionsAndRevokesCapturedReads(t *testing.T) {
	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			read, err := s.captureView()
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			operation := make(chan error, 1)
			go func() {
				fn := func(tx *Tx) error {
					if mode == "write" {
						if err := tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte("joined")); err != nil {
							return err
						}
					}
					close(entered)
					<-release
					return nil
				}
				if mode == "write" {
					operation <- s.update(fn)
				} else {
					operation <- read(fn)
				}
			}()
			lifetimeWait(t, entered)
			closed, closedAgain := make(chan error, 1), make(chan error, 1)
			go func() { closed <- s.Close() }()
			lifetimeWait(t, lifetimeRevoked(s))
			go func() { closedAgain <- s.Close() }()
			lifetimeStillOpen(t, s, closed)
			if err := read(func(*Tx) error { t.Error("revoked callback ran"); return nil }); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
				t.Fatalf("captured read = %v", err)
			}
			if err := s.update(func(*Tx) error { t.Error("revoked write ran"); return nil }); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
				t.Fatalf("late write = %v", err)
			}
			close(release)
			if err := lifetimeResult(t, operation); err != nil {
				t.Fatal(err)
			}
			if err := lifetimeResult(t, closed); err != nil {
				t.Fatal(err)
			}
			if err := lifetimeResult(t, closedAgain); err != nil {
				t.Fatal(err)
			}
			if err := s.db.View(func(*bbolt.Tx) error { return nil }); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
				t.Fatalf("physical close = %v", err)
			}
			if s.journal != nil || s.journalKey != nil {
				t.Fatal("closed Store retained journal ownership")
			}
		})
	}
}

func TestMetadataCloseCancelsAndActuallyJoinsConfirmation(t *testing.T) {
	for _, mode := range []string{"update", "batch"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.db")
			s := openJournalledStore(t, path)
			waiting, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			if err := s.SetMetadataJournalAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) { return 1, nil }, func(ctx context.Context, _ uint64) error {
				close(waiting)
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			}); err != nil {
				t.Fatal(err)
			}
			operation := make(chan error, 1)
			go func() {
				fn := func(tx *Tx) error {
					return tx.Bucket(bucketGatewayKeys).Put([]byte("withdrawn"), []byte(`{"enabled":false}`))
				}
				if mode == "batch" {
					operation <- s.batch(fn)
				} else {
					operation <- s.update(fn)
				}
			}()
			lifetimeWait(t, waiting)
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			lifetimeWait(t, canceled)
			lifetimeStillOpen(t, s, closed)
			close(release)
			if err := lifetimeResult(t, operation); !errors.Is(err, context.Canceled) {
				t.Fatalf("confirmation = %v", err)
			}
			if err := lifetimeResult(t, closed); err != nil {
				t.Fatal(err)
			}
			reopened := openJournalledStore(t, path)
			if err := reopened.view(func(tx *Tx) error {
				if string(tx.Bucket(bucketGatewayKeys).Get([]byte("withdrawn"))) != `{"enabled":false}` {
					t.Error("Close rolled back a committed authority withdrawal")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataCloseFlushesAcceptedBatchBeforeTimer(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	s.batches.delay = time.Hour
	operation := make(chan error, 1)
	go func() {
		operation <- s.batch(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte("queued")) })
	}()
	lifetimeEventually(t, func() bool { s.batches.mu.Lock(); defer s.batches.mu.Unlock(); return len(s.batches.pending) == 1 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lifetimeResult(t, operation); err != nil {
		t.Fatal(err)
	}
	if head := journalHead(t, s); head.Sequence != 1 {
		t.Fatalf("accepted batch head = %+v", head)
	}
}

func TestMetadataCloseRefusesBatchEnqueueAfterRevocation(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	s.batches.mu.Lock()
	operation := make(chan error, 1)
	go func() { operation <- s.batch(func(*Tx) error { t.Error("late queued callback ran"); return nil }) }()
	lifetimeEventually(t, func() bool { s.lifetime.mu.Lock(); defer s.lifetime.mu.Unlock(); return s.lifetime.active != 0 })
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	lifetimeWait(t, lifetimeRevoked(s))
	s.batches.mu.Unlock()
	if err := lifetimeResult(t, operation); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
		t.Fatalf("late batch = %v", err)
	}
	if err := lifetimeResult(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataJournalMaintenanceSerializesConcurrentCommits(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	var group sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for step := 0; step < 12; step++ {
				var err error
				switch worker {
				case 0:
					err = s.update(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte{byte(step)}) })
				case 1:
					_, err = s.TrimMetadataJournal(0)
				case 2:
					_, err = s.MetadataJournalState()
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(worker)
	}
	group.Wait()
	state, err := s.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Sequence != 12 || state.Applied != 12 {
		t.Fatalf("journal/projection diverged: %+v", state)
	}
}

func TestMetadataClosedNativeEntriesRefuseBeforeIO(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	path := s.MetadataJournalPath()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"migrate":        func() error { return s.migrate(func(*Tx) error { t.Error("closed migration ran"); return nil }) },
		"attach":         func() error { _, err := s.AttachMetadataJournal(testJournalKey(), "closed"); return err },
		"attach-replica": func() error { _, err := s.AttachReplicaMetadataJournal(nil, nil); return err },
		"apply-replica":  func() error { _, err := s.ApplyReplicaMetadataThrough(context.Background(), 0, 0); return err },
		"hook":           func() error { return s.SetMetadataJournalAfterDurable(nil, nil) },
		"state":          func() error { _, err := s.MetadataJournalState(); return err },
		"trim":           func() error { _, err := s.TrimMetadataJournal(0); return err },
		"compact":        func() error { return s.CompactSnapshot(filepath.Join(t.TempDir(), "must-not-exist.db")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
				t.Fatalf("closed entry = %v", err)
			}
		})
	}
	if s.MetadataJournalPath() != path {
		t.Fatal("native Close changed diagnostic path identity")
	}
	if stats := s.MetadataWriteStats(); stats != (MetadataWriteStats{}) {
		t.Fatalf("closed stats = %+v", stats)
	}
}

func TestMetadataWritePanicReleasesNativeLifetime(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	func() {
		defer func() {
			if recover() != "callback" {
				t.Error("callback panic not propagated")
			}
		}()
		_ = s.update(func(tx *Tx) error {
			if err := tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte("uncommitted")); err != nil {
				t.Fatal(err)
			}
			panic("callback")
		})
	}()
	if head := journalHead(t, s); head.Sequence != 0 {
		t.Fatalf("panicked callback was journalled: %+v", head)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := lifetimeResult(t, closed); err != nil {
		t.Fatal(err)
	}
}
