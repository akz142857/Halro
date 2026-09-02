package ledger

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Sealing has to stay off the write path, and "off" is a claim about the
// writer lock rather than about how long a step takes.
//
// A roll and a compaction both run on the maintenance tick of a process that is
// serving traffic, and both used to hold the same mutex Append needs: the roll
// across a full SHA-256 of the active file, the compaction across a read, a
// gzip and a decompress-verify of the sealed one — up to
// ledger.seal.max_active_bytes each. A budget reservation must be durable
// before any upstream call is made, so an Append that waits is a request that
// waits, and the whole gateway stops for the duration.
//
// These two tests are about that mutex. They deliberately do not assert a
// duration: they assert that appends make progress *while* the slow step is
// still running, which is false whenever the lock is held across it and true
// whenever it is not.

func appendReservationsConcurrently(t *testing.T, log *Log, count, writers int) {
	t.Helper()
	var group sync.WaitGroup
	failures := make(chan error, writers)
	for writer := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := writer; index < count; index += writers {
				event := validReservation(fmt.Sprintf("evt_%d", index), fmt.Sprintf("attempt_%d", index))
				if _, err := log.Append(context.Background(), event); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

func TestCompactionDoesNotHoldTheWriterLock(t *testing.T) {
	directory := t.TempDir()
	log := openChained(t, filepath.Join(directory, "ledger.wal"), nil)
	defer log.Close()

	// Big enough that compressing it is not instantaneous, so "an append got
	// through first" means the lock was free rather than that the test won a
	// race. Written concurrently only to keep the fixture cheap: the writer
	// batches them, so this is a few hundred fsyncs rather than six thousand.
	appendReservationsConcurrently(t, log, 6000, 8)
	result, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rolled {
		t.Fatal("nothing was sealed, so this test proves nothing")
	}

	appended := make(chan time.Time, 1)
	go func() {
		event := validReservation("evt_during_compaction", "attempt_during_compaction")
		if _, err := log.Append(context.Background(), event); err != nil {
			close(appended)
			return
		}
		appended <- time.Now()
	}()

	if _, err := log.Compact(result.Sealed.Generation); err != nil {
		t.Fatal(err)
	}
	compacted := time.Now()

	select {
	case at, ok := <-appended:
		if !ok {
			t.Fatal("the append made during compaction failed")
		}
		if at.After(compacted) {
			t.Fatal("the append only completed after compaction returned: the writer lock was held across it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an append issued during compaction never completed")
	}
}

// A roll still holds the writer — it has to, since the committed prefix is only
// well defined under the lock — so what this pins is that it no longer reads
// the file it is sealing. The checksum comes from the digest the writer keeps
// as it appends, and the test's evidence is that the sealed generation's
// recorded checksum matches the bytes on disk: an incremental digest that
// drifted from the file would seal a manifest nothing could ever verify.
func TestARollRecordsTheChecksumOfWhatItSealedWithoutReadingIt(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)

	appendReservations(t, log, 1, 200)
	first, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	// A second generation, so the digest is exercised after a reset as well as
	// after a fresh open.
	appendReservations(t, log, 201, 150)
	second, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	for _, sealed := range []Segment{first.Sealed, second.Sealed} {
		checksum, length, err := hashFile(filepath.Join(directory, sealed.File))
		if err != nil {
			t.Fatal(err)
		}
		if checksum != sealed.PlainChecksum {
			t.Fatalf("generation %d records checksum %s, the file hashes to %s",
				sealed.Generation, sealed.PlainChecksum, checksum)
		}
		if length != sealed.Length {
			t.Fatalf("generation %d records %d bytes, the file is %d",
				sealed.Generation, sealed.Length, length)
		}
	}

	// And the same again through a reopen, which seeds the digest from disk
	// rather than from a sequence of appends.
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	appendReservations(t, reopened, 401, 50)
	third, err := reopened.Roll()
	if err != nil {
		t.Fatal(err)
	}
	checksum, _, err := hashFile(filepath.Join(directory, third.Sealed.File))
	if err != nil {
		t.Fatal(err)
	}
	if checksum != third.Sealed.PlainChecksum {
		t.Fatalf("after a reopen, generation %d records checksum %s, the file hashes to %s",
			third.Sealed.Generation, third.Sealed.PlainChecksum, checksum)
	}
	if _, err := VerifySegments(directory, testChainKey, 0); err != nil {
		t.Fatalf("the sealed archive does not verify: %v", err)
	}
}
