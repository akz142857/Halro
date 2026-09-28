package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVerifyDurableSnapshotInventoryAndTamper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	first := durableTestPage()
	firstCursor := durableTransitionCursor{JournalID: first.NextCursor.JournalID, Sequence: 1, Digest: first.NextCursor.Digest,
		BaselineDigest: first.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", first, firstCursor, time.Now().UTC()) {
		t.Fatal("first page was not stored")
	}
	if _, err := verifyDurableSnapshot(path, "test", "ha", members); err == nil {
		t.Fatal("active collector source passed frozen snapshot verification")
	}
	archive.segmentTarget = 1
	second, secondCursor := durableSecondPage()
	if !archive.capture("node-1", second, secondCursor, time.Now().UTC()) {
		t.Fatal("second page was not stored")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := verifyDurableSnapshot(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "local_files_verified" || report.ClosedSegments != 1 || len(report.Files) != 2 || len(report.Chains) != 1 ||
		report.Chains[0].StoredSequence != 2 || report.Chains[0].CommittedSequence != 2 ||
		report.Chains[0].HeadDigest != second.HeadDigest || report.Chains[0].ObservedAt.IsZero() ||
		report.Chains[0].Coverage != "caught_up_to_observed_head" || !reflect.DeepEqual(report.NotStarted, []string{"node-2"}) || report.InventorySHA256 == "" {
		t.Fatalf("incomplete snapshot inventory: %+v", report)
	}
	eventHash := sha256.New()
	for _, event := range []durableTransitionEvent{first.Events[0], second.Events[0]} {
		if err := json.NewEncoder(eventHash).Encode(memberEventProjection(event)); err != nil {
			t.Fatal(err)
		}
	}
	if report.Chains[0].EventsSHA256 != hex.EncodeToString(eventHash.Sum(nil)) {
		t.Fatalf("cross-segment event root omitted or reordered events: %s", report.Chains[0].EventsSHA256)
	}
	var fileBytes uint64
	for _, file := range report.Files {
		fileBytes += file.Bytes
	}
	if fileBytes != report.StorageBytes {
		t.Fatalf("inventory size mismatch: %+v", report)
	}
	again, err := verifyDurableSnapshot(path, "test", "ha", []string{"node-2", "node-1"})
	if err != nil || !reflect.DeepEqual(again, report) {
		t.Fatalf("snapshot report is not deterministic: first=%+v second=%+v err=%v", report, again, err)
	}
	for _, mismatch := range []struct {
		environment string
		cluster     string
		members     []string
	}{
		{environment: "other", cluster: "ha", members: members},
		{environment: "test", cluster: "other", members: members},
		{environment: "test", cluster: "ha", members: []string{"node-1", "node-3"}},
	} {
		if _, err := verifyDurableSnapshot(path, mismatch.environment, mismatch.cluster, mismatch.members); err == nil {
			t.Fatalf("mismatched snapshot identity was accepted: %+v", mismatch)
		}
	}
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte(nil), manifest...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := verifyDurableSnapshot(path, "test", "ha", members)
	if err != nil || changed.InventorySHA256 == report.InventorySHA256 {
		t.Fatalf("file-byte change did not change snapshot root: %+v err=%v", changed, err)
	}
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	segmentPath := durableArchiveSegmentPath(path, 0)
	segment, err := os.ReadFile(segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segmentPath, []byte(strings.Replace(string(segment), first.BaselineDigest, strings.Repeat("f", 64), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDurableSnapshot(path, "test", "ha", members); err == nil {
		t.Fatal("tampered segment passed snapshot verification")
	}
	if err := os.Remove(segmentPath); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDurableSnapshot(path, "test", "ha", members); err == nil {
		t.Fatal("missing segment passed snapshot verification")
	}
}

func TestVerifyDurableSnapshotRejectsUnfinishedOrLegacyWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("page was not stored")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistArchive(durableArchiveSegmentPath(path, 0), archive.doc); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDurableSnapshot(path, "test", "ha", members); err == nil {
		t.Fatal("unfinished rollover passed frozen snapshot verification")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatalf("read-only snapshot verification changed source: %v", err)
	}
	if err := os.Remove(durableArchiveSegmentPath(path, 0)); err != nil {
		t.Fatal(err)
	}
	legacy := archive.doc
	legacy.Version = 1
	if err := persistArchive(path, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDurableSnapshot(path, "test", "ha", members); err == nil {
		t.Fatal("legacy unsegmented document passed finalized snapshot verification")
	}
}
