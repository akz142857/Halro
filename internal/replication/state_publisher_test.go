package replication

import (
	"path/filepath"
	"testing"
)

func publisherTestState(role Role) MemberState {
	return MemberState{
		Version: StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0",
		Role: role, Term: 7, PromisedTerm: 7,
		Peers: []StatePeer{{Name: "halro-1", Address: "halro-1.internal:9910", SPKISHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
	}
}

func TestStatePublisherLiveEventsFollowDurableRoleTransitions(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "state.json")
	publisher, err := NewStatePublisher(path, key, publisherTestState(RoleReplica))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 0, 8); err == nil || len(publisher.LiveTransitions().Events) != 0 {
		t.Fatal("failed promotion emitted a transition event")
	}
	if _, err := publisher.Promise(8); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 0, 8); err != nil {
		t.Fatal(err)
	}
	history := publisher.LiveTransitions()
	if history.PublisherStartedAt.IsZero() || history.Dropped != 0 || len(history.Events) != 2 ||
		history.Events[0].Kind != "promise" || history.Events[0].Sequence != 1 ||
		history.Events[1].Kind != "promote" || history.Events[1].Sequence != 2 ||
		history.Events[1].FromRole != RoleReplica || history.Events[1].ToRole != RolePrimary ||
		history.Events[1].FromTerm != 7 || history.Events[1].ToTerm != 8 || history.Events[1].At.IsZero() {
		t.Fatalf("wrong live transition evidence: %+v", history)
	}
	onDisk, err := ReadState(path, key)
	if err != nil || onDisk.Role != RolePrimary || onDisk.Term != 8 {
		t.Fatalf("event preceded durable state: %+v, %v", onDisk, err)
	}
	history.Events[0].Kind = "tampered"
	if publisher.LiveTransitions().Events[0].Kind != "promise" {
		t.Fatal("caller mutated publisher transition history")
	}
	publisher.Close()
	restarted, err := NewStatePublisher(path, key, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.LiveTransitions().Events) != 0 {
		t.Fatal("process-local transition history falsely survived restart")
	}
}

func TestStatePublisherLiveEventRingReportsRetentionLoss(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	publisher, err := NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), key, publisherTestState(RoleReplica))
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	for term := uint64(8); term < 8+maxLiveTransitionEvents+2; term++ {
		if _, err := publisher.Promise(term); err != nil {
			t.Fatal(err)
		}
	}
	history := publisher.LiveTransitions()
	if history.Dropped != 2 || len(history.Events) != maxLiveTransitionEvents ||
		history.Events[0].Sequence != 3 || history.Events[len(history.Events)-1].Sequence != maxLiveTransitionEvents+2 {
		t.Fatalf("retention loss was not explicit: dropped=%d count=%d first=%d last=%d",
			history.Dropped, len(history.Events), history.Events[0].Sequence, history.Events[len(history.Events)-1].Sequence)
	}
}

func TestStatePublisherWritesPrimaryAndReplicaProgress(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Run("primary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cluster", "state.json")
		publisher, err := NewStatePublisher(path, key, publisherTestState(RolePrimary))
		if err != nil {
			t.Fatal(err)
		}
		head := [32]byte{1}
		projection := ProjectionState{Index: 1, MetadataEpoch: 4, MetadataSequence: 9}
		if err := publisher.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: head, Projection: projection}); err != nil {
			t.Fatal(err)
		}
		state, err := ReadState(path, key)
		if err != nil {
			t.Fatal(err)
		}
		if state.DurableIndex != 2 || state.ConfirmedIndex != 1 || state.OrderingHeadMAC != head || state.Projection != projection {
			t.Fatalf("state=%#v", state)
		}
		if err := publisher.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: head, Projection: projection}); err == nil {
			t.Fatal("regressing Primary progress was accepted")
		}
		projection.Index = 0
		if err := publisher.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: head, Projection: projection}); err == nil {
			t.Fatal("Primary projection detached from confirmed prefix")
		}
	})

	t.Run("replica", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cluster", "state.json")
		publisher, err := NewStatePublisher(path, key, publisherTestState(RoleReplica))
		if err != nil {
			t.Fatal(err)
		}
		head := [32]byte{2}
		progress := ReplicaProgress{
			DurableIndex: 3, ConfirmedIndex: 2, AppliedIndex: 2, OrderingHeadMAC: head,
			Projection: ProjectionState{Index: 2, MetadataEpoch: 4, MetadataSequence: 9},
		}
		if err := publisher.PublishReplica(progress); err != nil {
			t.Fatal(err)
		}
		state, err := ReadState(path, key)
		if err != nil {
			t.Fatal(err)
		}
		if state.DurableIndex != 3 || state.ConfirmedIndex != 2 || state.AppliedIndex != 2 || state.Projection != progress.Projection {
			t.Fatalf("state=%#v", state)
		}
		progress.Projection.Index = 1
		if err := publisher.PublishReplica(progress); err == nil {
			t.Fatal("projection index detached from applied index")
		}
	})
}

func TestStatePublisherPersistsPromiseBeforePromotion(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	state := publisherTestState(RoleReplica)
	state.AppliedIndex = 4
	state.ConfirmedIndex = 4
	state.DurableIndex = 4
	state.OrderingHeadMAC = [32]byte{4}
	state.Projection.Index = 4
	publisher, err := NewStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promote(7, 4, 8); err == nil {
		t.Fatal("promotion without a durable promise was accepted")
	}
	promised, err := publisher.Promise(8)
	if err != nil {
		t.Fatal(err)
	}
	if promised.Role != RoleReplica || promised.Term != 7 || promised.PromisedTerm != 8 {
		t.Fatalf("promised state=%#v", promised)
	}
	if _, err := publisher.Promise(8); err == nil {
		t.Fatal("duplicate promised term was accepted")
	}
	if _, err := publisher.Promote(7, 3, 8); err == nil {
		t.Fatal("stale expected index was accepted")
	}
	promoted, err := publisher.Promote(7, 4, 8)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Role != RolePrimary || promoted.Term != 8 || promoted.PromisedTerm != 8 {
		t.Fatalf("promoted state=%#v", promoted)
	}
	onDisk, err := ReadState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Role != RolePrimary || onDisk.Term != 8 || onDisk.AppliedIndex != 4 {
		t.Fatalf("on-disk promoted state=%#v", onDisk)
	}
}

func TestPromiseDemotesPrimaryInTheSameDurablePublication(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	publisher, err := NewStatePublisher(path, key, publisherTestState(RolePrimary))
	if err != nil {
		t.Fatal(err)
	}
	state, err := publisher.Promise(9)
	if err != nil {
		t.Fatal(err)
	}
	if state.Role != RoleReplica || state.Term != 7 || state.PromisedTerm != 9 {
		t.Fatalf("demoted state=%#v", state)
	}
}

func TestStatePublisherAdoptsHigherTermWithoutMovingProgress(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	state := publisherTestState(RolePrimary)
	state.DurableIndex = 2
	state.ConfirmedIndex = 1
	state.OrderingHeadMAC = [32]byte{2}
	publisher, err := NewStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := publisher.AdoptHigherTerm(8, 9)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Role != RoleReplica || adopted.Term != 8 || adopted.PromisedTerm != 9 || adopted.DurableIndex != 2 || adopted.ConfirmedIndex != 1 {
		t.Fatalf("adopted state=%#v", adopted)
	}
	if _, err := publisher.AdoptHigherTerm(7, 9); err == nil {
		t.Fatal("term regression was accepted")
	}
}
