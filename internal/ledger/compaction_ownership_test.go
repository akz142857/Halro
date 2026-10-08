package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/durable"
)

func productionCompactIO() compactIO {
	return compactIO{compress: compressSegmentFileOwned, publish: func(directory string, source segmentManifestSource) (bool, error) {
		return saveSegmentManifestSourceWithSync(directory, source, durable.SyncDirectory)
	}}
}

func TestConcurrentCompactionPreservesPublishedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	compressed := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	firstDone := make(chan error, 1)
	operations := productionCompactIO()
	operations.compress = func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
		artifact, err := compressSegmentFileOwned(source, destination, checksum, length)
		if err != nil {
			return artifact, err
		}
		close(compressed)
		<-resume
		return artifact, nil
	}
	go func() { _, err := log.compactWithIO(rolled.Sealed.Generation, operations); firstDone <- err }()
	select {
	case <-compressed:
	case <-time.After(10 * time.Second):
		t.Fatal("compression did not reach publication boundary")
	}
	// The first call has produced real verified gzip bytes, but no manifest
	// points at them. Previously a second call published that same path and
	// the first call's duplicate cleanup then removed the authoritative file.
	_, concurrentErr := log.Compact(rolled.Sealed.Generation)
	if concurrentErr != nil && !errors.Is(concurrentErr, ErrSegmentCompactionBusy) {
		t.Fatal(concurrentErr)
	}
	// Appends and a structural roll still proceed while compression IO is
	// paused, but a different generation cannot allocate a second workspace.
	appendReservations(t, log, 5, 4)
	second, err := log.Roll()
	if err != nil || !second.Rolled {
		t.Fatalf("concurrent roll=%+v err=%v", second, err)
	}
	if _, err := log.Compact(second.Sealed.Generation); !errors.Is(err, ErrSegmentCompactionBusy) {
		t.Fatalf("second generation bypassed compression admission: %v", err)
	}
	release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("compaction did not finish")
	}
	segment := segmentsForTest(t, log)[0]
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), segment.File)); err != nil {
		t.Fatalf("committed compressed file lost after concurrent compaction: %v", err)
	}
	if !errors.Is(concurrentErr, ErrSegmentCompactionBusy) {
		t.Fatal("overlapping compression was not rejected")
	}
	assertSegmentSummary(t, log)
	if _, err := log.VerifySealedSummary(0); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	assertSegmentSummary(t, reopened)
}

func TestCompactionReleasesWorkspaceAfterFailedCompression(t *testing.T) {
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("compression IO failed")
	operations := productionCompactIO()
	operations.compress = func(string, string, string, int64) (*compressedArtifact, error) { return nil, injected }
	before := log.SealedSummary()
	if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if log.SealedSummary() != before || log.Status().Load() != AccountingHealthy {
		t.Fatal("pre-publication IO failure changed committed state")
	}
	if _, err := log.Compact(rolled.Sealed.Generation); err != nil {
		t.Fatalf("compression workspace leaked: %v", err)
	}
	assertSegmentSummary(t, log)
}

func TestCompactionRechecksHealthBeforeManifestPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	operations := productionCompactIO()
	operations.compress = func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
		artifact, err := compressSegmentFileOwned(source, destination, checksum, length)
		// A writer or roll may fail while gzip runs outside the writer lock.
		// Compaction must not overwrite a recovery-required native directory.
		log.Status().RequireRecovery()
		return artifact, err
	}
	before := log.SealedSummary()
	if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); err == nil {
		t.Fatal("unhealthy log published manifest")
	}
	if got := log.SealedSummary(); got != before {
		t.Fatalf("unhealthy publish changed summary=%+v", got)
	}
	manifest, err := loadSegmentManifest(filepath.Dir(path))
	if err != nil || len(manifest.Segments) != 1 || manifest.Segments[0].Compressed {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), rolled.Sealed.File)); err != nil {
		t.Fatal(err)
	}
}

func TestCloseJoinsCompressionOwnerBeforeClosingNativeFiles(t *testing.T) {
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	operations := productionCompactIO()
	operations.compress = func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
		close(entered)
		<-resume
		return compressSegmentFileOwned(source, destination, checksum, length)
	}
	compacted := make(chan error, 1)
	go func() { _, err := log.compactWithIO(rolled.Sealed.Generation, operations); compacted <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("compression did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- log.Close() }()
	select {
	case <-log.closeSignal:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not withdraw admission")
	}
	log.mu.Lock()
	owned := log.compacting && log.file != nil
	log.mu.Unlock()
	if !owned {
		t.Fatal("Close released resources before compression owner exited")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned with live compression: %v", err)
	default:
	}
	release()
	select {
	case err := <-compacted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("compression did not exit")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not join compression")
	}
	log.mu.Lock()
	owned = log.compacting || log.file != nil
	log.mu.Unlock()
	if owned {
		t.Fatal("Close left compression or native owner live")
	}
}

func TestCompactionUncertainManifestSyncRetainsBothFilesAndRequiresRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	before := log.SealedSummary()
	injected := errors.New("directory sync failed after real manifest rename")
	operations := productionCompactIO()
	operations.publish = func(directory string, source segmentManifestSource) (bool, error) {
		return saveSegmentManifestSourceWithSync(directory, source, func(string) error { return injected })
	}
	if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); !errors.Is(err, injected) {
		t.Fatalf("err=%v", err)
	}
	manifest, err := loadSegmentManifest(filepath.Dir(path))
	if err != nil || len(manifest.Segments) != 1 || !manifest.Segments[0].Compressed {
		t.Fatalf("renamed manifest=%+v err=%v", manifest, err)
	}
	for _, name := range []string{rolled.Sealed.File, manifest.Segments[0].File} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
			t.Fatalf("uncertain publish lost required file %s: %v", name, err)
		}
	}
	if log.Status().Load() != AccountingRecoveryRequired {
		t.Fatal("uncertain publish left accounting healthy")
	}
	if got := log.SealedSummary(); got != before {
		t.Fatalf("unsynced summary=%+v want=%+v", got, before)
	}
	if _, err := log.Compact(rolled.Sealed.Generation); err == nil {
		t.Fatal("unhealthy log accepted compaction retry")
	}
	if _, err := log.Roll(); err == nil {
		t.Fatal("unhealthy log accepted roll")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	if !segmentsForTest(t, reopened)[0].Compressed {
		t.Fatal("restart did not follow renamed manifest")
	}
	assertSegmentSummary(t, reopened)
	if _, err := reopened.VerifySealedSummary(0); err != nil {
		t.Fatal(err)
	}
}
