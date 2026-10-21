package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyArchiveReadbackRejectsChangedMissingAndSharedFiles(t *testing.T) {
	root := t.TempDir()
	sourceDir, readbackDir := filepath.Join(root, "source"), filepath.Join(root, "readback")
	for _, dir := range []string{sourceDir, readbackDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(sourceDir, "durable.json")
	readback := filepath.Join(readbackDir, "durable.json")
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(source, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	first := durableTestPage()
	firstCursor := durableTransitionCursor{JournalID: first.NextCursor.JournalID, Sequence: 1, Digest: first.NextCursor.Digest,
		BaselineDigest: first.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", first, firstCursor, time.Now().UTC()) {
		t.Fatal("first page was not stored")
	}
	archive.segmentTarget = 1
	second, secondCursor := durableSecondPage()
	if !archive.capture("node-1", second, secondCursor, time.Now().UTC()) {
		t.Fatal("second page was not stored")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	sourceReport, err := verifyDurableSnapshot(source, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range sourceReport.Files {
		data, err := os.ReadFile(filepath.Join(sourceDir, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(readbackDir, file.Name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := verifyArchiveReadback(source, readback, "test", "ha", members)
	if err != nil || result.Status != "readback_bytes_match_local_only" || result.InventorySHA256 != sourceReport.InventorySHA256 ||
		result.SourceVerification.ClosedSegments != 1 || result.ReadbackVerification.ClosedSegments != 1 {
		t.Fatalf("valid readback rejected or misreported: %+v err=%v", result, err)
	}
	if _, err := verifyArchiveReadback(source, source, "test", "ha", members); err == nil {
		t.Fatal("source path accepted as an archive readback")
	}
	manifest, err := os.ReadFile(readback)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readback, append(append([]byte(nil), manifest...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyArchiveReadback(source, readback, "test", "ha", members); err == nil {
		t.Fatal("different but valid readback bytes passed inventory comparison")
	}
	if err := os.WriteFile(readback, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	segment := durableArchiveSegmentPath(readback, 0)
	if err := os.Remove(segment); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyArchiveReadback(source, readback, "test", "ha", members); err == nil {
		t.Fatal("missing readback segment passed verification")
	}
	if err := os.Link(durableArchiveSegmentPath(source, 0), segment); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyArchiveReadback(source, readback, "test", "ha", members); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("shared segment was not rejected: %v", err)
	}
}
