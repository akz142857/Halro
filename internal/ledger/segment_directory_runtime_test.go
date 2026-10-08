package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultLogDirectoryRefreshPublishesPersistentIndex(t *testing.T) {
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	defer log.Close()
	for generation := uint64(1); generation <= 5; generation++ {
		old := log.segmentDirectory
		oldInfo, err := old.index.Stat()
		if err != nil {
			t.Fatal(err)
		}
		appendReservations(t, log, int(generation)*10, 2)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
		if !old.closed.Load() {
			t.Fatal("Roll retained old index owner")
		}
		newInfo, err := log.segmentDirectory.index.Stat()
		if err != nil || os.SameFile(oldInfo, newInfo) {
			t.Fatal("Roll did not replace the persisted directory locator", err)
		}
		if !log.segmentDirectory.persistent || newInfo.Size() != int64(segmentDirectoryHeaderBytes)+int64(generation)*segmentDirectorySlotBytes {
			t.Fatal(newInfo, "Roll did not publish the expected persistent directory locator")
		}
		assertSegmentSummary(t, log)
	}
	current := log.segmentDirectory.index.Name()
	currentInfo, err := log.segmentDirectory.index.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatal("Close removed the rebuildable persistent locator", err)
	}
	reopened := openChained(t, filepath.Join(filepath.Dir(current), "ledger.wal"), nil)
	defer reopened.Close()
	reopenedInfo, err := reopened.segmentDirectory.index.Stat()
	if err != nil || !reopened.segmentDirectory.persistent || !os.SameFile(currentInfo, reopenedInfo) {
		t.Fatal("Open did not reuse the authenticated persistent locator", err)
	}
}

func TestPersistentDirectoryIndexCorruptionRebuildsFromManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 2)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	want := log.SealedSummary()
	indexPath := filepath.Join(filepath.Dir(path), segmentDirectoryIndexName)
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt, err := os.OpenFile(indexPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.WriteAt([]byte{0xff}, segmentDirectoryHeaderBytes); err != nil {
		_ = corrupt.Close()
		t.Fatal(err)
	}
	if err := corrupt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	after, err := os.Stat(indexPath)
	if err != nil || os.SameFile(before, after) || !reopened.segmentDirectory.persistent || reopened.SealedSummary() != want {
		t.Fatal("corrupt sidecar was not rebuilt from the authoritative manifest", err, reopened.SealedSummary())
	}
}

func TestPersistentDirectoryIndexDetectsMissingAuthoritativeManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 1)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(segmentManifestPath(filepath.Dir(path))); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey}); err == nil {
		t.Fatal("Open accepted a missing authoritative manifest while its persistent locator remained")
	}
}

func TestDefaultLogCorruptDirectoryNeverReturnsEmptyHistory(t *testing.T) {
	for _, operation := range []string{"replay", "streaming observer", "roll"} {
		t.Run(operation, func(t *testing.T) {
			log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
			defer log.Close()
			appendReservations(t, log, 1, 2)
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
			appendReservations(t, log, 3, 1)
			corrupt, err := os.OpenFile(log.segmentDirectory.index.Name(), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := corrupt.WriteAt([]byte{0xff}, segmentDirectoryHeaderBytes); err != nil {
				_ = corrupt.Close()
				t.Fatal(err)
			}
			if err := corrupt.Close(); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "replay":
				visits := 0
				_, err = log.ReplayAuthenticated(context.Background(), ReplayAnchor{}, func(Record) error { visits++; return nil })
				if visits != 0 {
					t.Fatal("corrupt directory replayed partial facts")
				}
			case "streaming observer":
				visits := 0
				err = log.WalkSegments(context.Background(), func(Segment) error { visits++; return nil })
				if visits != 0 {
					t.Fatal("corrupt directory returned partial stream")
				}
			case "roll":
				_, err = log.Roll()
			}
			if err == nil || log.Status().Load() != AccountingRecoveryRequired {
				t.Fatal("directory corruption was not fail closed", err, log.Status().Load())
			}
		})
	}
}

func TestDefaultLogFailedIndexRefreshAfterIntentRequiresRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 2)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 3, 1)
	before := log.SealedSummary()
	indexPath := filepath.Join(filepath.Dir(path), segmentDirectoryIndexName)
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(indexPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err == nil {
		t.Fatal("index refresh unexpectedly succeeded")
	}
	if log.Status().Load() != AccountingRecoveryRequired || log.Generation() != 2 || log.SealedSummary() != before {
		t.Fatal("failed refresh exposed writable/new generation")
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	if reopened.Generation() != 2 || reopened.SealedSummary() != before {
		t.Fatal("restart did not abandon the unrenamed intent")
	}
	appendReservations(t, reopened, 4, 1)
	if _, err := reopened.ReplayAuthenticated(context.Background(), ReplayAnchor{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentDirectoryIndexCeilingRejectsRollBeforePublishingIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	indexBudget := int64(segmentDirectoryHeaderBytes + segmentDirectorySlotBytes)
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxSegmentDirectoryIndexBytes: indexBudget})
	if err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 1, 1)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 2, 1)
	if _, err := log.Roll(); err == nil {
		t.Fatal("roll exceeded the configured segment locator ceiling")
	}
	if log.Generation() != 2 {
		t.Fatalf("failed over-ceiling roll changed active generation to %d", log.Generation())
	}
	manifest, err := findReplicationGeneration(filepath.Dir(path), 2)
	if err != nil || manifest.pending || manifest.sealed {
		t.Fatalf("failed over-ceiling roll published pending intent: %+v err=%v", manifest, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxSegmentDirectoryIndexBytes: indexBudget - 1}); err == nil {
		t.Fatal("open accepted an existing directory over the configured locator ceiling")
	}
}
