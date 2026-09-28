package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurableArchivePersistsVerifiedCursorAndRequiresFreshPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	collector := durableTestCollector(t, func(*http.Request) (*http.Response, error) { return durableResponse(t, page), nil })
	_, cursor, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
	if failure != "" || !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatalf("initial capture failed: %s", failure)
	}
	if archive.view().Status != "partial" || archive.view().Members[0].Status != "caught_up" {
		t.Fatalf("unexpected coverage: %+v", archive.view())
	}
	viewJSON, err := json.Marshal(archive.view())
	if err != nil || len(archive.view().Members[0].RecentEvents) != 1 || strings.Contains(string(viewJSON), "previous_digest") || strings.Contains(string(viewJSON), "baseline_digest") {
		t.Fatalf("operator view leaked chain internals or lost event: %s err=%v", viewJSON, err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDurableArchive(path, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.view().Members[0].Status != "unavailable" || reopened.view().Members[0].Failure != "not_polled_since_restart" {
		t.Fatalf("restart claimed old poll as current: %+v", reopened.view())
	}
	got := reopened.cursor("node-1", "inc_1")
	if got == nil || *got != cursor {
		t.Fatalf("durable cursor was not restored: %+v", got)
	}
	page.Events = nil
	if !reopened.capture("node-1", page, cursor, time.Now().UTC()) || reopened.view().Members[0].Status != "caught_up" {
		t.Fatalf("fresh continuation failed: %+v", reopened.view())
	}
}

func TestDurableArchiveRefusesTamperedStoredChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	collector := durableTestCollector(t, func(*http.Request) (*http.Response, error) { return durableResponse(t, page), nil })
	_, cursor, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
	if failure != "" || !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatalf("initial capture failed: %s", failure)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc durableArchiveDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Chains["node-1"][0].Events[0].PreviousDigest = strings.Repeat("c", 64)
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("tampered chain restored")
	}
}

func TestDurableArchiveRejectsChangedJournalWithoutAdvancing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("initial capture failed")
	}
	page.NextCursor.JournalID = strings.Repeat("f", 32)
	cursor.JournalID = page.NextCursor.JournalID
	page.Events = nil
	if archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("changed journal accepted")
	}
	if got := archive.cursor("node-1", "inc_1"); got == nil || got.JournalID == cursor.JournalID {
		t.Fatalf("stored cursor advanced: %+v", got)
	}
}

func TestDurableArchiveIdentifiesChangedJournalWithoutAcceptingReseed(t *testing.T) {
	archive, err := openDurableArchive(filepath.Join(t.TempDir(), "durable.json"), "test", "ha", []string{"node-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	oldPage := durableTestPage()
	oldCursor := durableTransitionCursor{JournalID: oldPage.NextCursor.JournalID, Sequence: 1, Digest: oldPage.NextCursor.Digest,
		BaselineDigest: oldPage.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", oldPage, oldCursor, time.Now().UTC()) {
		t.Fatal("initial member chain was not stored")
	}
	newPage := durableTestPage()
	newPage.NextCursor.JournalID = strings.Repeat("f", 32)
	newPage.Events = nil
	newPage.CommittedSequence = 0
	newPage.HeadDigest = newPage.BaselineDigest
	newPage.NextCursor.Sequence = 0
	newPage.NextCursor.Digest = newPage.BaselineDigest
	collector := durableTestCollector(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/ha/status" {
			return jsonResponse(`{"mode":"ha","node_id":"node-1","cluster_id":"ha","incarnation":"inc_1","role":"replica","term":7,"promised_term":7}`), nil
		}
		if request.URL.Path != "/ha/transitions" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		response := durableResponse(t, newPage)
		if request.URL.Query().Get("journal_id") == oldCursor.JournalID {
			response.StatusCode = http.StatusConflict
		}
		return response, nil
	})
	s := &server{cluster: "ha", members: []string{"node-1"}, statusCollectors: map[string]statusCollector{"node-1": collector}, durableArchive: archive}
	s.pollDurableOnce(context.Background())
	view := archive.view()
	if view.Members[0].Failure != "journal_changed_requires_reconciliation" ||
		archive.cursor("node-1", "inc_1").JournalID != oldCursor.JournalID || len(archive.doc.Chains["node-1"]) != 1 {
		t.Fatalf("changed journal replaced the old chain or lost its diagnostic: %+v", view)
	}
}

func TestDurableArchivePollsAuthenticatedMemberPages(t *testing.T) {
	archive, err := openDurableArchive(filepath.Join(t.TempDir(), "durable.json"), "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	collectors := map[string]statusCollector{}
	for _, node := range []string{"node-1", "node-2"} {
		node := node
		page := durableTestPage()
		page.NodeID = node
		if node == "node-2" {
			page.NextCursor.JournalID = strings.Repeat("e", 32)
		}
		collector := durableTestCollector(t, func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case "/ha/status":
				return jsonResponse(`{"mode":"ha","node_id":"` + node + `","cluster_id":"ha","incarnation":"inc_1","role":"replica","term":7,"promised_term":7}`), nil
			case "/ha/transitions":
				return durableResponse(t, page), nil
			default:
				t.Fatalf("unexpected path: %s", request.URL.Path)
				return nil, nil
			}
		})
		collector.config.NodeID = node
		collectors[node] = collector
	}
	s := &server{cluster: "ha", members: []string{"node-1", "node-2"}, statusCollectors: collectors, durableArchive: archive}
	s.pollDurableOnce(context.Background())
	if got := archive.view(); got.Status != "caught_up" || got.Members[0].StoredSequence != 1 || got.Members[1].StoredSequence != 1 {
		t.Fatalf("poll did not persist both chains: %+v", got)
	}
}

func TestDurableArchiveRejectsIncarnationRollback(t *testing.T) {
	archive, err := openDurableArchive(filepath.Join(t.TempDir(), "durable.json"), "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("first incarnation capture failed")
	}
	page.Incarnation = "inc_2"
	page.NextCursor.JournalID = strings.Repeat("e", 32)
	cursor.JournalID = page.NextCursor.JournalID
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("second incarnation capture failed")
	}
	page.Incarnation = "inc_1"
	page.NextCursor.JournalID = strings.Repeat("f", 32)
	cursor.JournalID = page.NextCursor.JournalID
	if archive.capture("node-1", page, cursor, time.Now().UTC()) || archive.view().Members[0].Failure != "incarnation_reused" {
		t.Fatalf("rollback was accepted: %+v", archive.view())
	}
}

func durableSecondPage() (durableTransitionPage, durableTransitionCursor) {
	page := durableTestPage()
	second := page.Events[0]
	second.Sequence = 2
	second.At = second.At.Add(time.Second)
	second.FromPromisedTerm, second.ToPromisedTerm = 8, 9
	second.PreviousDigest = page.NextCursor.Digest
	second.Digest = strings.Repeat("c", 64)
	page.Events = []durableTransitionEvent{second}
	page.CommittedSequence, page.HeadDigest = 2, second.Digest
	page.NextCursor.Sequence, page.NextCursor.Digest = 2, second.Digest
	return page, durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 2, Digest: second.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 9}
}

func TestDurableArchiveRolloverRetainsAndVerifiesAllPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	first := durableTestPage()
	firstCursor := durableTransitionCursor{JournalID: first.NextCursor.JournalID, Sequence: 1, Digest: first.NextCursor.Digest,
		BaselineDigest: first.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", first, firstCursor, time.Now().UTC()) {
		t.Fatal("first page was not stored")
	}
	encoded, err := json.Marshal(archive.doc)
	if err != nil {
		t.Fatal(err)
	}
	archive.segmentTarget = len(encoded) + 1
	second, secondCursor := durableSecondPage()
	if !archive.capture("node-1", second, secondCursor, time.Now().UTC()) {
		t.Fatalf("second page did not rotate: %+v", archive.view())
	}
	if archive.doc.Generation != 1 || len(archive.doc.Chains["node-1"][0].Events) != 1 ||
		archive.view().Members[0].EventsRetained != 2 || len(archive.view().Members[0].RecentEvents) != 2 {
		t.Fatalf("rollover coverage=%+v document=%+v", archive.view(), archive.doc)
	}
	segmentPath := durableArchiveSegmentPath(path, 0)
	segmentInfo, err := os.Stat(segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if view := archive.view(); view.ClosedSegments != 1 || view.ManifestBytes != uint64(manifestInfo.Size()) ||
		view.StorageBytes != uint64(segmentInfo.Size()+manifestInfo.Size()) {
		t.Fatalf("rollover storage accounting=%+v", view)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.cursor("node-1", "inc_1"); got == nil || *got != secondCursor || len(reopened.view().Members[0].RecentEvents) != 2 ||
		reopened.view().StorageBytes != archive.view().StorageBytes {
		t.Fatalf("cross-segment restore=%+v view=%+v", got, reopened.view())
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), data...)
	var segment durableArchiveDocument
	if err := json.Unmarshal(data, &segment); err != nil {
		t.Fatal(err)
	}
	segment.Chains["node-1"][0].Events[0].PreviousDigest = strings.Repeat("d", 64)
	data, err = json.Marshal(segment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segmentPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("tampered closed segment was accepted")
	}
	if err := os.WriteFile(segmentPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(segmentPath); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("missing closed segment was accepted")
	}
}

func TestDurableArchiveCompletesOrphanedRolloverAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("page was not stored")
	}
	if err := persistArchive(durableArchiveSegmentPath(path, 0), archive.doc); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.doc.Generation != 1 || len(recovered.doc.Chains["node-1"][0].Events) != 0 ||
		recovered.cursor("node-1", "inc_1").Sequence != 1 || len(recovered.view().Members[0].RecentEvents) != 1 {
		t.Fatalf("orphaned rollover was not reconciled: %+v", recovered.doc)
	}
}

func TestDurableArchiveUpgradesLegacyUnsegmentedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("page was not stored")
	}
	legacy := archive.doc
	legacy.Version = 1
	for node, chains := range legacy.Chains {
		copyChains := append([]durableStoredChain(nil), chains...)
		for i := range copyChains {
			copyChains[i].StartCursor = durableTransitionCursor{}
			copyChains[i].StartEventAt = time.Time{}
			copyChains[i].LastEventAt = time.Time{}
		}
		legacy.Chains[node] = copyChains
	}
	if err := persistArchive(path, legacy); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if upgraded.doc.Version != 2 || upgraded.cursor("node-1", "inc_1").Sequence != 1 || len(upgraded.view().Members[0].RecentEvents) != 1 {
		t.Fatalf("legacy migration lost evidence: %+v", upgraded.doc)
	}
}

func TestDurableArchiveRejectsConflictingOrphanedRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	page := durableTestPage()
	cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: 1, Digest: page.NextCursor.Digest,
		BaselineDigest: page.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", page, cursor, time.Now().UTC()) {
		t.Fatal("page was not stored")
	}
	orphan := archive.doc
	orphan.Chains = map[string][]durableStoredChain{"node-1": append([]durableStoredChain(nil), orphan.Chains["node-1"]...)}
	orphan.Chains["node-1"][0].HeadDigest = strings.Repeat("f", 64)
	if err := persistArchive(durableArchiveSegmentPath(path, 0), orphan); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("conflicting orphaned segment was accepted")
	}
	manifest, err := readDurableArchiveDocument(path)
	if err != nil || manifest.Generation != 0 {
		t.Fatalf("manifest advanced despite conflicting segment: generation=%d err=%v", manifest.Generation, err)
	}
}

func TestDurableArchiveReplaysMoreThanOneClosedSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.json")
	archive, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	first := durableTestPage()
	firstCursor := durableTransitionCursor{JournalID: first.NextCursor.JournalID, Sequence: 1, Digest: first.NextCursor.Digest,
		BaselineDigest: first.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 8}
	if !archive.capture("node-1", first, firstCursor, time.Now().UTC()) {
		t.Fatal("first page was not stored")
	}
	encoded, err := json.Marshal(archive.doc)
	if err != nil {
		t.Fatal(err)
	}
	archive.segmentTarget = len(encoded) + 1
	second, secondCursor := durableSecondPage()
	if !archive.capture("node-1", second, secondCursor, time.Now().UTC()) {
		t.Fatal("second page was not stored")
	}
	third := second
	thirdEvent := second.Events[0]
	thirdEvent.Sequence, thirdEvent.At = 3, thirdEvent.At.Add(time.Second)
	thirdEvent.FromPromisedTerm, thirdEvent.ToPromisedTerm = 9, 10
	thirdEvent.PreviousDigest, thirdEvent.Digest = secondCursor.Digest, strings.Repeat("d", 64)
	third.Events = []durableTransitionEvent{thirdEvent}
	third.CommittedSequence, third.HeadDigest = 3, thirdEvent.Digest
	third.NextCursor.Sequence, third.NextCursor.Digest = 3, thirdEvent.Digest
	thirdCursor := durableTransitionCursor{JournalID: third.NextCursor.JournalID, Sequence: 3, Digest: thirdEvent.Digest,
		BaselineDigest: third.BaselineDigest, Role: "replica", Term: 7, PromisedTerm: 10}
	if !archive.capture("node-1", third, thirdCursor, time.Now().UTC()) {
		t.Fatal("third page was not stored")
	}
	if archive.doc.Generation != 2 {
		t.Fatalf("expected two closed segments, got generation %d", archive.doc.Generation)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.cursor("node-1", "inc_1").Sequence != 3 || reopened.view().Members[0].EventsRetained != 3 ||
		len(reopened.view().Members[0].RecentEvents) != 3 {
		t.Fatalf("multi-segment replay lost events: %+v", reopened.view())
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(durableArchiveSegmentPath(path, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableArchive(path, "test", "ha", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("missing middle segment was accepted")
	}
}
