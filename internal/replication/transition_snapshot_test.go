package replication

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTransitionEvidenceJSONLineContract(t *testing.T) {
	event := DurableTransitionEvidence{MemberTransitionEvent: MemberTransitionEvent{
		Sequence: 1, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Kind: "promise",
		FromRole: RolePrimary, ToRole: RoleReplica, FromTerm: 7, ToTerm: 7,
		FromPromisedTerm: 7, ToPromisedTerm: 8,
	}, PreviousDigest: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64)}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sequence":1,"at":"2026-01-02T03:04:05Z","kind":"promise","from_role":"primary","to_role":"replica","from_term":7,"to_term":7,"from_promised_term":7,"to_promised_term":8,"previous_digest":"` + strings.Repeat("a", 64) + `","digest":"` + strings.Repeat("b", 64) + `"}`
	if string(encoded) != want {
		t.Fatalf("transition evidence encoding changed: %s", encoded)
	}
}

func TestVerifyTransitionSnapshotAuthenticatesSegmentsAndInventory(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(TransitionJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	publisher.journal.segmentLimit = info.Size()
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	state = publisher.Snapshot()
	publisher.Close()
	report, err := VerifyTransitionSnapshot(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "member_mac_verified" || report.Segments != 3 || len(report.Files) != 4 || report.Committed != 2 ||
		report.BaselineKind != "legacy_baseline" || report.InventorySHA256 == "" || report.CommittedDigest == report.BaselineDigest ||
		report.EventsSHA256 == hex.EncodeToString(sha256.New().Sum(nil)) {
		t.Fatalf("incomplete member snapshot report: %+v", report)
	}
	baseline := DurableTransitionCursor{JournalID: report.JournalID, Digest: report.BaselineDigest}
	tailReport, page, err := ReadVerifiedTransitionSnapshotPage(path, key, state, baseline)
	if err != nil || !reflect.DeepEqual(tailReport, report) || len(page.Events) != 2 || page.NextCursor.Sequence != report.Committed ||
		page.NextCursor.Digest != report.CommittedDigest || page.HasMore {
		t.Fatalf("authenticated frozen tail mismatch: report=%+v page=%+v err=%v", tailReport, page, err)
	}
	first := DurableTransitionCursor{JournalID: report.JournalID, Sequence: 1, Digest: page.Events[0].Digest}
	_, missing, err := ReadVerifiedTransitionSnapshotPage(path, key, state, first)
	if err != nil || len(missing.Events) != 1 || missing.Events[0] != page.Events[1] {
		t.Fatalf("exact cursor did not recover committed suffix: page=%+v err=%v", missing, err)
	}
	for _, wrong := range []DurableTransitionCursor{
		{JournalID: strings.Repeat("0", 32), Sequence: 1, Digest: first.Digest},
		{JournalID: report.JournalID, Sequence: 1, Digest: strings.Repeat("0", 64)},
		{JournalID: report.JournalID, Sequence: report.Committed + 1, Digest: report.CommittedDigest},
	} {
		if _, _, err := ReadVerifiedTransitionSnapshotPage(path, key, state, wrong); err == nil {
			t.Fatalf("invalid frozen cursor accepted: %+v", wrong)
		}
	}
	var size uint64
	for _, file := range report.Files {
		size += file.Bytes
	}
	if size != report.StorageBytes {
		t.Fatalf("member snapshot byte accounting=%+v", report)
	}
	again, err := VerifyTransitionSnapshot(path, key, state)
	if err != nil || !reflect.DeepEqual(again, report) {
		t.Fatalf("member snapshot report changed without file change: %+v err=%v", again, err)
	}
	wrong := cloneMemberState(state)
	wrong.PromisedTerm++
	if _, err := VerifyTransitionSnapshot(path, key, wrong); err == nil {
		t.Fatal("wrong expected state passed snapshot verification")
	}
	segment := transitionSegmentPath(TransitionJournalPath(path), 1)
	data, err := os.ReadFile(segment)
	if err != nil {
		t.Fatal(err)
	}
	data[20] ^= 1
	if err := os.WriteFile(segment, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTransitionSnapshot(path, key, state); err == nil {
		t.Fatal("tampered member segment passed MAC verification")
	}
	if err := os.Remove(segment); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTransitionSnapshot(path, key, state); err == nil {
		t.Fatal("missing member segment passed verification")
	}
}

func TestVerifyTransitionSnapshotRejectsPendingIntentWithoutRepair(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journal, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMemberState(state)
	next.PromisedTerm++
	if _, err := journal.appendIntent("promise", next); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(TransitionJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTransitionSnapshot(path, key, state); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("pending member intent passed snapshot verification: %v", err)
	}
	after, err := os.ReadFile(TransitionJournalPath(path))
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("snapshot verification repaired pending intent: %v", err)
	}
}
