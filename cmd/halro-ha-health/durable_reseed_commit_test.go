package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReseedHandoffReplayRetainsBothGenerationsAndRejectsChangedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	old := durableTestPage()
	oldCursor := durableTransitionCursor{JournalID: old.NextCursor.JournalID, Sequence: 1, Digest: old.NextCursor.Digest,
		BaselineDigest: old.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", old, oldCursor, time.Now().UTC()) {
		t.Fatal("old generation was not captured")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	doc, err := readDurableArchiveDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	eventHash := sha256.New()
	if err := json.NewEncoder(eventHash).Encode(memberEventProjection(old.Events[0])); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	newID := strings.Repeat("f", 32)
	newDigest := strings.Repeat("c", 64)
	doc.Version = 3
	doc.Handoffs = []durableReseedHandoff{{NodeID: "node-1", Incarnation: "inc_1",
		OldJournalID: oldCursor.JournalID, OldCommittedSequence: 1, OldCommittedDigest: oldCursor.Digest,
		OldEventsSHA256:          hex.EncodeToString(eventHash.Sum(nil)),
		OldMemberInventorySHA256: strings.Repeat("1", 64), NewJournalID: newID,
		NewBaselineDigest: newDigest, NewMemberInventorySHA256: strings.Repeat("2", 64),
		SourceNode: "node-2", SourceInventorySHA256: strings.Repeat("3", 64),
		SeedManifestSHA256: strings.Repeat("4", 64), SeedTerm: 7, SeedIndex: 9,
		FencingEvidenceSHA256: strings.Repeat("5", 64), RetiredFrozenAt: now.Add(-time.Hour),
		SourceFrozenAt: now.Add(-time.Hour), ReplacementFrozenAt: now.Add(-time.Minute), CommittedAt: now}}
	baseline := durableTransitionCursor{JournalID: newID, BaselineDigest: newDigest, Digest: newDigest,
		Role: "replica", Term: 7, PromisedTerm: 7}
	doc.Chains["node-1"] = append(doc.Chains["node-1"], durableStoredChain{NodeID: "node-1",
		Incarnation: "inc_1", BaselineKind: "legacy_baseline", BaselineRole: "replica",
		BaselineTerm: 7, BaselinePromised: 7, StartCursor: baseline, Cursor: baseline,
		CommittedSequence: 0, HeadDigest: newDigest, ObservedAt: now})
	if err := persistArchive(path, doc); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatalf("authenticated generation handoff did not replay: %v", err)
	}
	if got := reopened.cursor("node-1", "inc_1"); got == nil || got.JournalID != newID || len(reopened.doc.Chains["node-1"]) != 2 {
		t.Fatalf("replay lost old or new generation: %+v", reopened.doc.Chains["node-1"])
	}
	newPage := durableTestPage()
	newPage.NextCursor.JournalID, newPage.NextCursor.Sequence, newPage.NextCursor.Digest = newID, 0, newDigest
	newPage.BaselineDigest, newPage.HeadDigest, newPage.CommittedSequence = newDigest, newDigest, 0
	newPage.Events = nil
	if !reopened.capture("node-1", newPage, baseline, time.Now().UTC()) {
		t.Fatalf("collector could not continue the new generation: %+v", reopened.view())
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := verifyDurableSnapshot(path, "test", "ha", members)
	if err != nil || len(verified.Chains) != 2 || len(verified.Handoffs) != 1 {
		t.Fatalf("snapshot dropped a generation: %+v err=%v", verified, err)
	}
	doc, err = readDurableArchiveDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.Handoffs[0].OldEventsSHA256 = strings.Repeat("0", 64)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", members); err == nil {
		t.Fatal("replay accepted a handoff whose old event history changed")
	}
}
