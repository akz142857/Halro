package replication

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func durablePublisherFixture(t testing.TB) (string, []byte, MemberState) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	state := publisherTestState(RoleReplica)
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	cursor, err := CreateTransitionJournal(TransitionJournalPath(path), key, state, "legacy_baseline")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStatePublisher(path, key, state); err == nil || !strings.Contains(err.Error(), "finish or recover") {
		t.Fatalf("version-2 publisher continued after migration intent: %v", err)
	}
	state.Version, state.Transition = TransitionStateVersion, cursor
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	return path, key, state
}

func durableEvents(t testing.TB, publisher *StatePublisher) []MemberTransitionEvent {
	t.Helper()
	events, err := publisher.DurableTransitions()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTransitionJournalStorageTracksAppendRotationAndRecovery(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := TransitionJournalPath(path)
	baseline, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := publisher.TransitionJournalStorage()
	if err != nil || stats.Segments != 1 || stats.Bytes != uint64(baseline.Size()) {
		t.Fatalf("baseline storage=%+v err=%v", stats, err)
	}
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err = publisher.TransitionJournalStorage()
	if err != nil || stats.Segments != 1 || stats.Bytes != uint64(first.Size()) || stats.Bytes <= uint64(baseline.Size()) {
		t.Fatalf("appended storage=%+v err=%v", stats, err)
	}
	publisher.journal.segmentLimit = first.Size()
	if _, err := publisher.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	committed := publisher.Snapshot()
	second, err := os.Stat(transitionSegmentPath(journalPath, 2))
	if err != nil {
		t.Fatal(err)
	}
	stats, err = publisher.TransitionJournalStorage()
	if err != nil || stats.Segments != 2 || stats.Bytes != uint64(first.Size()+second.Size()) {
		t.Fatalf("rotated storage=%+v err=%v", stats, err)
	}
	publisher.Close()
	j, err := OpenTransitionJournal(journalPath, key, committed)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMemberState(committed)
	next.Role = RoleReplica
	next.PromisedTerm++
	if _, err := j.appendIntent("promise", next); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableStatePublisher(path, key, committed)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	stats, err = recovered.TransitionJournalStorage()
	if err != nil || stats.Segments != 2 || stats.Bytes != uint64(first.Size()+second.Size()) {
		t.Fatalf("recovered storage=%+v err=%v", stats, err)
	}
	activePath := transitionSegmentPath(journalPath, 2)
	if err := os.Chmod(activePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.TransitionJournalStorage(); err == nil {
		t.Fatal("insecure active segment passed capacity accounting")
	}
	if err := os.Chmod(activePath, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(activePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.TransitionJournalStorage(); err == nil {
		t.Fatal("externally changed active segment passed capacity accounting")
	}
}

func TestDurableTransitionsSurvivePublisherRestart(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	before := publisher.Snapshot().Transition
	if before.Sequence != 2 || len(durableEvents(t, publisher)) != 2 {
		t.Fatalf("durable transitions not committed: cursor=%+v events=%+v", before, durableEvents(t, publisher))
	}
	if err := publisher.PublishPrimary(PrimaryProgress{Projection: ProjectionState{}}); err != nil {
		t.Fatal(err)
	}
	if got := publisher.Snapshot().Transition; got != before {
		t.Fatalf("progress changed transition cursor: %+v", got)
	}
	publisher.Close()
	onDisk, err := ReadState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDurableStatePublisher(path, key, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	events := durableEvents(t, restarted)
	if len(events) != 2 || events[0].Kind != "promise" || events[0].Sequence != 1 ||
		events[1].Kind != "promote" || events[1].Sequence != 2 || events[1].ToRole != RolePrimary ||
		restarted.Snapshot().Transition != before {
		t.Fatalf("restarted history=%+v state=%+v", events, restarted.Snapshot())
	}
	if len(restarted.LiveTransitions().Events) != 0 {
		t.Fatal("process-local live event ring was incorrectly restored")
	}
	first, err := restarted.DurableTransitionPage(0, "", 1)
	if err != nil || len(first.Events) != 1 || first.Events[0].Sequence != 1 || !first.HasMore ||
		first.NextCursor.Sequence != 1 || first.Events[0].PreviousDigest != first.BaselineDigest {
		t.Fatalf("first durable page=%+v err=%v", first, err)
	}
	second, err := restarted.DurableTransitionPage(first.NextCursor.Sequence, first.NextCursor.Digest, 1)
	if err != nil || len(second.Events) != 1 || second.Events[0].Sequence != 2 || second.HasMore ||
		second.Events[0].PreviousDigest != first.NextCursor.Digest || second.NextCursor.Digest != second.HeadDigest {
		t.Fatalf("second durable page=%+v err=%v", second, err)
	}
	if _, err := restarted.DurableTransitionPage(first.NextCursor.Sequence, "wrong-digest", 1); err == nil {
		t.Fatal("durable page accepted a mismatched cursor")
	}
	if _, err := restarted.DurableTransitionPage(2, "", 1); err == nil {
		t.Fatal("durable page accepted a sequence without its digest")
	}
}

func TestDurableTransitionSegmentsRetainAndAuthenticateFullChain(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := TransitionJournalPath(path)
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	// A tiny test threshold forces a new durable segment for each transition.
	publisher.journal.segmentLimit = info.Size()
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	committed := publisher.Snapshot()
	publisher.Close()
	segments, err := transitionSegments(journalPath)
	if err != nil || len(segments) != 3 || segments[1].start != 1 || segments[2].start != 2 {
		t.Fatalf("rotation did not create ordered segments: %+v err=%v", segments, err)
	}
	restarted, err := NewDurableStatePublisher(path, key, committed)
	if err != nil {
		t.Fatal(err)
	}
	page, err := restarted.DurableTransitionPage(0, "", 64)
	if err != nil || len(page.Events) != 2 || page.Events[0].Sequence != 1 || page.Events[1].Sequence != 2 || page.HasMore {
		t.Fatalf("cross-segment page=%+v err=%v", page, err)
	}
	restarted.Close()
	original, err := os.ReadFile(segments[1].path)
	if err != nil {
		t.Fatal(err)
	}
	var forged transitionJournalRecord
	if err := json.Unmarshal(original, &forged); err != nil {
		t.Fatal(err)
	}
	forged.To.PromisedTerm++
	modified, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segments[1].path, append(modified, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableStatePublisher(path, key, committed); err == nil {
		t.Fatal("tampered closed segment was accepted")
	}
	if err := os.WriteFile(segments[1].path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(segments[1].path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableStatePublisher(path, key, committed); err == nil {
		t.Fatal("missing closed segment was accepted")
	}
}

func TestDurableTransitionSparsePagesAcrossCheckpointsAndSegments(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journalPath := TransitionJournalPath(path)
	journal, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		t.Fatal(err)
	}
	journal.segmentLimit = 48 << 10
	for range 150 {
		next := cloneMemberState(state)
		next.PromisedTerm++
		cursor, err := journal.appendIntent("promise", next)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.commit(cursor); err != nil {
			t.Fatal(err)
		}
		next.Transition = cursor
		state = next
	}
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.segments) < 2 || len(restarted.segments[0].checkpoints) < 2 {
		t.Fatalf("fixture did not cross a sparse checkpoint and segment: %+v", restarted.segments)
	}
	var after uint64
	var digest string
	var count int
	for {
		page, err := restarted.Page(after, digest, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			count++
			if event.Sequence != uint64(count) || event.FromPromisedTerm+1 != event.ToPromisedTerm {
				t.Fatalf("sparse page lost ordering at %d: %+v", count, event)
			}
		}
		if !page.HasMore {
			if count != 150 || page.NextCursor.Sequence != 150 || page.NextCursor.Digest != page.HeadDigest {
				t.Fatalf("incomplete sparse replay: count=%d page=%+v", count, page)
			}
			break
		}
		after, digest = page.NextCursor.Sequence, page.NextCursor.Digest
	}
	if _, err := restarted.Page(64, "wrong", 64); !errors.Is(err, ErrInvalidTransitionCursor) {
		t.Fatalf("wrong page digest accepted: %v", err)
	}
	pendingState := cloneMemberState(state)
	pendingState.PromisedTerm++
	if _, err := restarted.appendIntent("promise", pendingState); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	lastPage, err := recovered.DurableTransitionPage(after, digest, 64)
	if err != nil || len(lastPage.Events) != 22 || lastPage.CommittedSequence != 150 || lastPage.NextCursor.Sequence != 150 {
		t.Fatalf("pending recovery changed sparse page: %+v err=%v", lastPage, err)
	}
	recovered.Close()
	restarted, err = OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		t.Fatal(err)
	}
	firstSegment := restarted.segments[0].path
	data, err := os.ReadFile(firstSegment)
	if err != nil {
		t.Fatal(err)
	}
	data[20] ^= 1
	if err := os.WriteFile(firstSegment, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Page(0, "", 64); err == nil || errors.Is(err, ErrInvalidTransitionCursor) {
		t.Fatalf("changed on-disk page segment did not fail as unavailable: %v", err)
	}
	_ = restarted.Close()
}

func TestDurableTransitionRecoveryRemovesOnlyUncommittedNewSegment(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journalPath := TransitionJournalPath(path)
	journal, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal.segmentLimit = info.Size()
	next := cloneMemberState(state)
	next.PromisedTerm = 8
	if _, err := journal.appendIntent("promise", next); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	newSegment := transitionSegmentPath(journalPath, 1)
	if _, err := os.Stat(newSegment); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newSegment); !os.IsNotExist(err) {
		t.Fatalf("uncommitted segment remained after recovery: %v", err)
	}
	if _, err := recovered.Promise(8); err != nil {
		t.Fatal(err)
	}
	recovered.Close()
	verified, err := ReadState(path, key)
	if err != nil || verified.Transition.Sequence != 1 {
		t.Fatalf("recovered publication=%+v err=%v", verified.Transition, err)
	}
}

func TestDurableTransitionRecoveryRewritesOnlyActiveSegment(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journalPath := TransitionJournalPath(path)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	publisher.journal.segmentLimit = info.Size()
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	committed := publisher.Snapshot()
	publisher.Close()
	journal, err := OpenTransitionJournal(journalPath, key, committed)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMemberState(committed)
	next.Role, next.Term = RolePrimary, 8
	if _, err := journal.appendIntent("promote", next); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableStatePublisher(path, key, committed)
	if err != nil {
		t.Fatal(err)
	}
	if len(durableEvents(t, recovered)) != 1 || recovered.Snapshot().Transition.Sequence != 1 {
		t.Fatal("uncommitted suffix became visible after active-segment recovery")
	}
	if _, err := recovered.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	recovered.Close()
	verified, err := ReadState(path, key)
	if err != nil || verified.Transition.Sequence != 2 {
		t.Fatalf("promotion after recovery=%+v err=%v", verified.Transition, err)
	}
}

func TestLegacyPublisherRefusesOrphanedTransitionSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	state := publisherTestState(RoleReplica)
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	orphan := transitionSegmentPath(TransitionJournalPath(path), 1)
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStatePublisher(path, key, state); err == nil {
		t.Fatal("version-2 publisher ignored orphaned transition segment")
	}
	if _, err := CreateTransitionJournal(TransitionJournalPath(path), key, state, "legacy_baseline"); err == nil {
		t.Fatal("new baseline hid orphaned transition segment")
	}
}

func TestDurableTransitionRecoveryDiscardsIntentWithoutStateCommit(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journal, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMemberState(state)
	next.PromisedTerm = 8
	if _, err := journal.appendIntent("promise", next); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	restarted, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(durableEvents(t, restarted)) != 0 || restarted.Snapshot().Transition.Sequence != 0 {
		t.Fatal("uncommitted intent appeared as durable transition")
	}
	if _, err := restarted.Promise(8); err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	onDisk, err := ReadState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	journal, err = OpenTransitionJournal(TransitionJournalPath(path), key, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if events, err := journal.CommittedEvents(); err != nil || len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("recovery duplicated or lost event: %+v", events)
	}
}

func TestDurableTransitionRecoveryFindsStateCommitBeforeProcessEvent(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journal, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMemberState(state)
	next.PromisedTerm = 8
	cursor, err := journal.appendIntent("promise", next)
	if err != nil {
		t.Fatal(err)
	}
	next.Transition = cursor
	if err := WriteState(path, next, key); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	restarted, err := NewDurableStatePublisher(path, key, next)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if events := durableEvents(t, restarted); len(events) != 1 || events[0].Kind != "promise" || events[0].Sequence != 1 {
		t.Fatalf("durable state transition disappeared after crash: %+v", events)
	}
}

func TestDurableTransitionJournalFailsClosedOnLossOrTampering(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	journalPath := TransitionJournalPath(path)
	if info, err := os.Stat(journalPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions: %+v, %v", info, err)
	}
	if err := os.Rename(journalPath, journalPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableStatePublisher(path, key, state); err == nil {
		t.Fatal("version-3 state started without its journal")
	}
	if err := os.Rename(journalPath+".saved", journalPath); err != nil {
		t.Fatal(err)
	}
	state.Transition.Digest[0] ^= 0xff
	if _, err := OpenTransitionJournal(journalPath, key, state); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("wrong authenticated cursor accepted: %v", err)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	data[20] ^= 1
	if err := os.WriteFile(journalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	state.Transition.Digest[0] ^= 0xff
	if _, err := OpenTransitionJournal(journalPath, key, state); err == nil {
		t.Fatal("tampered transition journal accepted")
	}
}

func TestDurablePublisherPoisonsAfterFailedStatePublication(t *testing.T) {
	path, key, state := durablePublisherFixture(t)
	publisher, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promise(8); err == nil {
		t.Fatal("state rename failure acknowledged a promise")
	}
	if _, err := publisher.Promise(9); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("publisher continued after ambiguous state write: %v", err)
	}
	publisher.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if len(durableEvents(t, recovered)) != 0 {
		t.Fatal("failed publication became a committed transition")
	}
	if _, err := recovered.Promise(8); err != nil {
		t.Fatalf("reconciled publisher could not make the fresh promise: %v", err)
	}
}

func TestDurableJournalRejectsSignedButInvalidSequenceAndTransition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*transitionJournalRecord)
	}{
		{"sequence gap", func(record *transitionJournalRecord) { record.Sequence++ }},
		{"wrong kind", func(record *transitionJournalRecord) { record.Kind = "promote" }},
		{"wrong predecessor", func(record *transitionJournalRecord) { record.From.Term++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, key, state := durablePublisherFixture(t)
			publisher, err := NewDurableStatePublisher(path, key, state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := publisher.Promise(8); err != nil {
				t.Fatal(err)
			}
			publisher.Close()
			onDisk, err := ReadState(path, key)
			if err != nil {
				t.Fatal(err)
			}
			journalPath := TransitionJournalPath(path)
			data, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.SplitAfter(data, []byte{'\n'})
			if len(lines) != 3 || len(lines[2]) != 0 {
				t.Fatalf("unexpected journal line count: %d", len(lines))
			}
			record, err := decodeTransitionRecord(lines[1], key)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&record)
			lines[1], err = encodeTransitionRecord(record, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journalPath, bytes.Join(lines, nil), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDurableStatePublisher(path, key, onDisk); err == nil {
				t.Fatal("signed but invalid journal chain was accepted")
			}
		})
	}
}

func TestOfflineTransitionMigrationResumesFromExistingBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	state := publisherTestState(RoleReplica)
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	cursor, err := CreateTransitionJournal(TransitionJournalPath(path), key, state, "legacy_baseline")
	if err != nil {
		t.Fatal(err)
	}
	// Crash here: the journal exists but state.json is still version 2.
	migrated, err := MigrateStateTransitionJournal(path, key)
	if err != nil || migrated.Version != TransitionStateVersion || migrated.Transition != cursor {
		t.Fatalf("migration did not resume the authenticated baseline: %+v, %v", migrated, err)
	}
	again, err := MigrateStateTransitionJournal(path, key)
	if err != nil || again.Transition != cursor {
		t.Fatalf("migration was not idempotent: %+v, %v", again, err)
	}
	publisher, err := OpenStatePublisher(path, key, again)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineTransitionMigrationRejectsChangedLegacyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	state := publisherTestState(RoleReplica)
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateTransitionJournal(TransitionJournalPath(path), key, state, "legacy_baseline"); err != nil {
		t.Fatal(err)
	}
	state.DurableIndex, state.ConfirmedIndex, state.AppliedIndex = 1, 1, 1
	state.OrderingHeadMAC[0] = 1
	state.Projection.Index = 1
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateStateTransitionJournal(path, key); err == nil || !strings.Contains(err.Error(), "matching baseline") {
		t.Fatalf("legacy progress changed after baseline but migration continued: %v", err)
	}
}
