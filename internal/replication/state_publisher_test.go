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

func TestStatePublisherWritesPrimaryAndReplicaProgress(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Run("primary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cluster", "state.json")
		publisher, err := NewStatePublisher(path, key, publisherTestState(RolePrimary))
		if err != nil {
			t.Fatal(err)
		}
		head := [32]byte{1}
		if err := publisher.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: head}); err != nil {
			t.Fatal(err)
		}
		state, err := ReadState(path, key)
		if err != nil {
			t.Fatal(err)
		}
		if state.DurableIndex != 2 || state.ConfirmedIndex != 1 || state.OrderingHeadMAC != head {
			t.Fatalf("state=%#v", state)
		}
		if err := publisher.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: head}); err == nil {
			t.Fatal("regressing Primary progress was accepted")
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
