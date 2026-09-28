package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventArchiveSurvivesRestartAndMarksUnprovableIntervals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	archive, err := openEventArchive(path, "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := now.Add(-time.Minute)
	status := memberStatus{NodeID: "node-1", Incarnation: "inc-1", LiveTransitions: &liveTransitionHistory{PublisherStartedAt: start,
		Events: []liveTransition{{Sequence: 1, At: start.Add(time.Second), Kind: "promise"}}}}
	capture := func(status memberStatus, at time.Time) {
		archive.capture([]memberStatus{status, {NodeID: "node-2"}}, at)
	}
	capture(status, now)
	if got := archive.view(); got.Status != "ok" || len(got.Records) != 2 || got.Records[0].Gap != "initial_observation" || got.Records[1].Live.Sequence != 1 {
		t.Fatalf("first capture: %+v", got)
	}
	if _, err := openEventArchive(path, "test", "cluster", []string{"node-2", "node-1"}); err == nil {
		t.Fatal("concurrent journal writer accepted")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err = openEventArchive(path, "test", "cluster", []string{"node-2", "node-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	capture(status, now.Add(time.Second))
	if got := archive.view(); len(got.Records) != 2 {
		t.Fatalf("restart duplicated event: %+v", got)
	}
	capture(memberStatus{NodeID: "node-1", Error: "transport"}, now.Add(2*time.Second))
	capture(memberStatus{NodeID: "node-1", Error: "transport"}, now.Add(3*time.Second))
	if got := archive.view(); len(got.Records) != 3 || got.Records[2].Gap != "collection_failed" || len(got.CollectionErrors) != 1 || got.CollectionErrors[0] != "node-1" {
		t.Fatalf("collection gap not bounded: %+v", got)
	}
	status.LiveTransitions.Dropped = 2
	status.LiveTransitions.Events = []liveTransition{{Sequence: 3, At: start.Add(3 * time.Second), Kind: "promote"}}
	capture(status, now.Add(4*time.Second))
	got := archive.view()
	if len(got.Records) != 6 || got.Records[3].Gap != "collection_resumed" || got.Records[4].Gap != "ring_history_missing" || got.Records[5].Live.Sequence != 3 || len(got.CollectionErrors) != 0 {
		t.Fatalf("ring gap not marked: %+v", got)
	}
	status.LiveTransitions.PublisherStartedAt = now.Add(5 * time.Second)
	status.LiveTransitions.Dropped = 0
	status.LiveTransitions.Events = nil
	capture(status, now.Add(6*time.Second))
	got = archive.view()
	if len(got.Records) != 7 || got.Records[6].Gap != "source_changed" {
		t.Fatalf("process gap not marked: %+v", got)
	}
	status.Incarnation = "inc-2"
	capture(status, now.Add(7*time.Second))
	got = archive.view()
	if len(got.Records) != 8 || got.Records[7].Gap != "incarnation_changed" {
		t.Fatalf("incarnation gap not marked: %+v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal not private: info=%v err=%v", info, err)
	}
}

func TestEventArchiveFailureAndRetentionAreVisible(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	archive, err := openEventArchive(filepath.Join(directory, "events.json"), "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	for range archiveRecordLimit + 1 {
		appendArchiveRecord(&archive.doc, eventArchiveRecord{NodeID: "node-1"})
	}
	if len(archive.doc.Records) != archiveRecordLimit || archive.doc.RetentionDropped != 1 {
		t.Fatalf("retention=%d dropped=%d", len(archive.doc.Records), archive.doc.RetentionDropped)
	}
	defer archive.Close()
	if err := os.Remove(archive.path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		// The archive has no file yet; a failed test here means the setup changed.
		t.Fatal(err)
	}
	archive.capture(nil, time.Now().UTC())
	s := &server{archive: archive}
	response := httptest.NewRecorder()
	s.eventArchiveResponse(response, httptest.NewRequest(http.MethodGet, "/api/event-archive", nil))
	var view eventArchiveView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || view.Status != "journal_write_failed" || view.RetentionDropped != 1 || len(view.Records) != archiveRecordLimit {
		t.Fatalf("failed journal hidden: status=%d view=%+v", response.Code, view)
	}
}

func TestEventArchiveRetentionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	archive, err := openEventArchive(path, "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-34 * time.Second)
	for first := uint64(1); first <= archiveRecordLimit+1; first += 32 {
		last := min(first+31, archiveRecordLimit+1)
		events := make([]liveTransition, 0, last-first+1)
		for sequence := first; sequence <= last; sequence++ {
			events = append(events, liveTransition{Sequence: sequence, At: base.Add(time.Duration(sequence) * time.Millisecond), Kind: "promise"})
		}
		archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", LiveTransitions: &liveTransitionHistory{
			PublisherStartedAt: base, Dropped: first - 1, Events: events,
		}}, {NodeID: "node-2"}}, base.Add(time.Duration(first/32)*time.Second))
		if got := archive.view(); got.Status != "ok" {
			t.Fatalf("batch starting at %d did not persist: %+v", first, got)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err = openEventArchive(path, "test", "cluster", []string{"node-2", "node-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	view := archive.view()
	if view.Status != "ok" || view.RetentionDropped != 2 || len(view.Records) != archiveRecordLimit {
		t.Fatalf("retention boundary changed after restart: status=%s dropped=%d records=%d",
			view.Status, view.RetentionDropped, len(view.Records))
	}
	if view.Records[0].Live == nil || view.Records[0].Live.Sequence != 2 ||
		view.Records[len(view.Records)-1].Live == nil || view.Records[len(view.Records)-1].Live.Sequence != archiveRecordLimit+1 {
		t.Fatalf("retained sequence changed after restart: first=%+v last=%+v",
			view.Records[0], view.Records[len(view.Records)-1])
	}
}

func TestEventArchiveRejectsIncompleteOrAmbiguousCollectorInventory(t *testing.T) {
	archive, err := openEventArchive(filepath.Join(t.TempDir(), "events.json"), "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	now := time.Now().UTC()
	start := now.Add(-time.Minute)
	first := memberStatus{NodeID: "node-1", Incarnation: "inc-1", LiveTransitions: &liveTransitionHistory{
		PublisherStartedAt: start, Events: []liveTransition{{Sequence: 1, At: start.Add(time.Second), Kind: "promise"}},
	}}
	archive.capture([]memberStatus{first, {NodeID: "node-2"}}, now)
	if got := archive.view(); got.Status != "ok" || len(got.CollectionErrors) != 0 {
		t.Fatalf("complete inventory rejected: %+v", got)
	}

	archive.capture([]memberStatus{first}, now.Add(time.Second))
	if got := archive.view(); got.Status != "partial" || len(got.CollectionErrors) != 1 || got.CollectionErrors[0] != "node-2" {
		t.Fatalf("missing member hidden: %+v", got)
	}

	archive.capture([]memberStatus{first, first, {NodeID: "node-2"}, {NodeID: "node-3"}}, now.Add(2*time.Second))
	got := archive.view()
	if got.Status != "partial" || len(got.CollectionErrors) != 2 || got.CollectionErrors[0] != "unexpected:node-3" || got.CollectionErrors[1] != "node-1" ||
		len(got.Records) != 4 || got.Records[2].Gap != "collection_failed" || got.Records[3].Gap != "collection_failed" {
		t.Fatalf("duplicate or unexpected member hidden: %+v", got)
	}

	archive.capture([]memberStatus{first, {NodeID: "node-2"}}, now.Add(3*time.Second))
	if got := archive.view(); got.Status != "ok" || len(got.CollectionErrors) != 0 || len(got.Records) != 5 ||
		got.Records[4].Gap != "collection_resumed" {
		t.Fatalf("recovered inventory not visible: %+v", got)
	}
}

func TestEventArchiveRecoveryBoundaryPersistsWithoutNewMemberEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	archive, err := openEventArchive(path, "test", "cluster", []string{"node-1"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	status := memberStatus{NodeID: "node-1", Incarnation: "inc-1", LiveTransitions: &liveTransitionHistory{PublisherStartedAt: now.Add(-time.Minute)}}
	archive.capture([]memberStatus{status}, now)
	archive.capture([]memberStatus{{NodeID: "node-1", Error: "transport"}}, now.Add(time.Second))
	archive.capture([]memberStatus{status}, now.Add(2*time.Second))
	archive.capture([]memberStatus{status}, now.Add(3*time.Second))
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err = openEventArchive(path, "test", "cluster", []string{"node-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	got := archive.view()
	if got.Status != "ok" || len(got.Records) != 3 || got.Records[1].Gap != "collection_failed" ||
		got.Records[1].ObservedAt != now.Add(time.Second) || got.Records[2].Gap != "collection_resumed" ||
		got.Records[2].ObservedAt != now.Add(2*time.Second) || got.Records[2].Live != nil {
		t.Fatalf("event-free recovery boundary lost or duplicated: %+v", got)
	}
}

func TestEventArchiveMarksRecoveryWhenReplicaSourceDisappears(t *testing.T) {
	archive, err := openEventArchive(filepath.Join(t.TempDir(), "events.json"), "test", "cluster", []string{"node-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	now := time.Now().UTC()
	live := &liveTransitionHistory{PublisherStartedAt: now.Add(-time.Minute)}
	archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", LiveTransitions: live, ReplicaStageTransitions: &replicaStageHistory{
		ReceiverStartedAt: now.Add(-time.Minute), ReceiveState: "ready", ApplyState: "ready",
	}}}, now)
	archive.capture([]memberStatus{{NodeID: "node-1", Error: "transport"}}, now.Add(time.Second))
	archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", Role: "primary", LiveTransitions: live}}, now.Add(2*time.Second))
	archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", Role: "primary", LiveTransitions: live}}, now.Add(3*time.Second))
	got := archive.view()
	if got.Status != "ok" || len(got.Records) != 6 || got.Records[3].Gap != "collection_failed" ||
		got.Records[5].Gap != "collection_resumed" || got.Records[5].Source != "replica_stage" {
		t.Fatalf("disappearing source recovery boundary lost or duplicated: %+v", got)
	}
}

func TestEventArchiveRejectsIdentityAndUnsafeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	archive, err := openEventArchive(path, "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	archive.capture(nil, time.Now().UTC())
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openEventArchive(path, "other", "cluster", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("cross-environment archive reuse accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openEventArchive(path, "test", "cluster", []string{"node-1", "node-2"}); err == nil {
		t.Fatal("group/world-readable archive accepted")
	}
}

func TestEventArchiveCapturesReplicaStageBoundaries(t *testing.T) {
	archive, err := openEventArchive(filepath.Join(t.TempDir(), "events.json"), "test", "cluster", []string{"node-1", "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	now := time.Now().UTC()
	archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", ReplicaStageTransitions: &replicaStageHistory{
		ReceiverStartedAt: now.Add(-time.Minute), ReceiveState: "ready", ApplyState: "blocked",
		Events: []replicaStageTransition{{Sequence: 1, At: now.Add(-time.Second), Stage: "apply", From: "ready", To: "blocked", Reason: "projection_failed", TargetIndex: 7}},
	}}, {NodeID: "node-2"}}, now)
	view := archive.view()
	if view.Status != "ok" || len(view.Records) != 2 || view.Records[0].Gap != "initial_observation" ||
		view.Records[1].ReplicaStage == nil || view.Records[1].ReplicaStage.TargetIndex != 7 {
		t.Fatalf("Replica stage not archived: %+v", view)
	}
	archive.capture([]memberStatus{{NodeID: "node-1", Incarnation: "inc-1", Role: "primary"}, {NodeID: "node-2"}}, now.Add(time.Second))
	archive.capture([]memberStatus{{NodeID: "node-1", Error: "transport"}, {NodeID: "node-2"}}, now.Add(2*time.Second))
	view = archive.view()
	for _, record := range view.Records[2:] {
		if record.Source == "replica_stage" && record.Gap == "collection_failed" {
			t.Fatal("inactive Replica source counted as failed after role change")
		}
	}
}
