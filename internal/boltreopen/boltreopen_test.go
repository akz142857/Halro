package boltreopen

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

var bucket = []byte("rows")

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path, bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// writeBatches writes rows the way history.db does: an ordered key and an
// index row under a random key, so leaves all over the tree are rewritten
// and pages come back off the freelist.
func writeBatches(t *testing.T, db *DB, batches, rows int, sequence *uint64) {
	t.Helper()
	value := make([]byte, 1200)
	for range batches {
		err := db.Update(func(tx *bbolt.Tx) error {
			rowsBucket, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return err
			}
			index, err := tx.CreateBucketIfNotExists([]byte("index"))
			if err != nil {
				return err
			}
			for range rows {
				*sequence++
				key := binary.BigEndian.AppendUint64(nil, *sequence)
				if err := rowsBucket.Put(key, value); err != nil {
					return err
				}
				random := make([]byte, 16)
				rand.Read(random)
				if err := index.Put(random, key); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// The defect this package exists for: bbolt's freelist keeps one map entry per
// page it hands out again until that page is freed a second time, so the heap
// grows with every write since the file was opened. Reopening gives it back.
func TestReopenGivesBackWhatTheFreelistAccumulated(t *testing.T) {
	db, _ := openTemp(t)
	var sequence uint64
	writeBatches(t, db, 1, 1000, &sequence)
	before := liveHeap()
	writeBatches(t, db, 150, 2000, &sequence)
	grown := liveHeap()
	if grown < before+(512<<10) {
		t.Fatalf("heap grew %d bytes over 300k rows; the fixture no longer reproduces the freelist growth", int64(grown)-int64(before))
	}
	reopened, err := db.Reopen()
	if err != nil || !reopened {
		t.Fatalf("Reopen = %v, %v; want true, nil", reopened, err)
	}
	after := liveHeap()
	runtime.KeepAlive(db)
	t.Logf("live heap: %d before the writes, %d after them, %d after the reopen", before, grown, after)
	if after > before+(grown-before)/4 {
		t.Fatalf("heap after reopen %d, before writes %d, grown to %d; the reopen gave back less than three quarters", after, before, grown)
	}
	var rows int
	if err := db.View(func(tx *bbolt.Tx) error {
		rows = tx.Bucket(bucket).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rows != int(sequence) {
		t.Fatalf("rows after reopen = %d, want %d", rows, sequence)
	}
}

func TestReopenSkipsWhileATransactionRuns(t *testing.T) {
	db, _ := openTemp(t)
	inside, release, done := make(chan struct{}), make(chan struct{}), make(chan error)
	go func() {
		done <- db.View(func(*bbolt.Tx) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	reopened, err := db.Reopen()
	if reopened || err != nil {
		t.Fatalf("Reopen during a read = %v, %v; want false, nil without waiting", reopened, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reopened, err := db.Reopen(); !reopened || err != nil {
		t.Fatalf("Reopen after the read = %v, %v; want true, nil", reopened, err)
	}
}

func TestAFailedReopenFailsClosedUntilOneSucceeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	db, path := openTemp(t)
	var sequence uint64
	writeBatches(t, db, 1, 10, &sequence)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Reopen()
	if !reopened || err == nil {
		t.Fatalf("Reopen of an unreadable file = %v, %v; want true and an error", reopened, err)
	}
	if err := db.View(func(*bbolt.Tx) error { return nil }); err == nil {
		t.Fatal("a read after a failed reopen succeeded; want the reopen's error")
	}
	if err := db.Update(func(*bbolt.Tx) error { return nil }); err == nil {
		t.Fatal("a write after a failed reopen succeeded; want the reopen's error")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := db.Reopen(); !reopened || err != nil {
		t.Fatalf("Reopen once readable = %v, %v; want true, nil", reopened, err)
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		if n := tx.Bucket(bucket).Stats().KeyN; n != 10 {
			t.Errorf("rows = %d, want 10", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReopenRefusesAFileRenamedIntoPlace(t *testing.T) {
	db, path := openTemp(t)
	other := filepath.Join(filepath.Dir(path), "other.db")
	stranger, err := bbolt.Open(other, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stranger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Reopen(); err == nil {
		t.Fatal("Reopen accepted a file renamed into place; want an error")
	}
	if err := db.View(func(*bbolt.Tx) error { return nil }); err == nil {
		t.Fatal("a read after the refused reopen succeeded")
	}
}

func pageWrites(db *DB) int64 {
	stats := db.Stats()
	return stats.TxStats.GetWrite()
}

func TestPageWriteCountersSurviveAReopen(t *testing.T) {
	db, _ := openTemp(t)
	var sequence uint64
	writeBatches(t, db, 5, 100, &sequence)
	first := pageWrites(db)
	if first == 0 {
		t.Fatal("no page writes counted")
	}
	if _, err := db.Reopen(); err != nil {
		t.Fatal(err)
	}
	if got := pageWrites(db); got != first {
		t.Fatalf("page writes after reopen = %d, want %d carried over", got, first)
	}
	writeBatches(t, db, 5, 100, &sequence)
	if got := pageWrites(db); got <= first {
		t.Fatalf("page writes = %d after more writes, want more than %d", got, first)
	}
}

func TestClosedStaysClosed(t *testing.T) {
	db, _ := openTemp(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Reopen(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reopen after Close = %v, want ErrClosed", err)
	}
	if err := db.View(func(*bbolt.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("View after Close = %v, want ErrClosed", err)
	}
}

// Run with -race: race reopen attempts against reads and writes, then reopen
// between batches and keep using the DB. Reopen may skip every attempt while
// transactions overlap, so success must be checked at a known idle point,
// not within a wall-clock window that depends on the scheduler.
func TestReopenUnderConcurrentReadsAndWrites(t *testing.T) {
	db, _ := openTemp(t)
	const rounds, workers, operations = 10, 4, 20
	for round := range rounds {
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for worker := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range operations {
					var err error
					if worker%2 == 0 {
						err = db.Update(func(tx *bbolt.Tx) error {
							b, err := tx.CreateBucketIfNotExists(bucket)
							if err != nil {
								return err
							}
							key := binary.BigEndian.AppendUint64([]byte{byte(worker)}, uint64(round*operations+n))
							return b.Put(key, []byte("v"))
						})
					} else {
						err = db.View(func(tx *bbolt.Tx) error {
							if b := tx.Bucket(bucket); b != nil {
								b.Stats()
							}
							return nil
						})
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		var reopenErr error
	running:
		for {
			select {
			case <-done:
				break running
			default:
			}
			if _, reopenErr = db.Reopen(); reopenErr != nil {
				break running
			}
			runtime.Gosched()
		}
		// Finish every worker before reporting errors or closing the DB.
		<-done
		close(errs)
		if reopenErr != nil {
			t.Fatal(reopenErr)
		}
		for err := range errs {
			t.Fatal(err)
		}
		if reopened, err := db.Reopen(); !reopened || err != nil {
			t.Fatalf("round %d: idle Reopen = %v, %v; want true, nil", round, reopened, err)
		}
		if err := db.View(func(tx *bbolt.Tx) error {
			want := (round + 1) * (workers / 2) * operations
			if b := tx.Bucket(bucket); b == nil {
				t.Errorf("round %d: rows bucket missing after reopen", round)
			} else if got := b.Stats().KeyN; got != want {
				t.Errorf("round %d: rows after reopen = %d, want %d", round, got, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
