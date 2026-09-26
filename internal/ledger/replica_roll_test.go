package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReplicatedRollVerifiesWholeSegmentBeforePublish(t *testing.T) {
	directory := t.TempDir()
	primaryPath := filepath.Join(directory, "primary", "ledger.wal")
	replicaPath := filepath.Join(directory, "replica", "ledger.wal")
	var beforeCalled, afterCalled bool
	primary, err := OpenWithOptions(primaryPath, NewStatus(), Options{
		ChainKey: testChainKey,
		BeforeRoll: func(generation, sequence uint64) error {
			beforeCalled = generation == 1 && sequence == 1
			return nil
		},
		AfterRoll: func(segment Segment) error {
			afterCalled = segment.Generation == 1 && segment.LastSequence == 1
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if _, err := primary.Append(context.Background(), validReservation("evt_roll", "att_roll")); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(replicaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replicaPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := primary.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if !beforeCalled || !afterCalled || !result.Rolled {
		t.Fatalf("before=%t after=%t result=%#v", beforeCalled, afterCalled, result)
	}
	replica, err := OpenWithOptions(replicaPath, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	wrong := result.Sealed
	wrong.Length++
	if _, err := replica.ApplyReplicatedRoll(wrong); err == nil {
		t.Fatal("mismatched Segment was published")
	}
	if replica.Generation() != 1 {
		t.Fatalf("mismatched roll advanced generation to %d", replica.Generation())
	}
	replicaResult, err := replica.ApplyReplicatedRoll(result.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !replicaResult.Rolled || replica.Generation() != 2 {
		t.Fatalf("replica result=%#v generation=%d", replicaResult, replica.Generation())
	}
	duplicate, err := replica.ApplyReplicatedRoll(result.Sealed)
	if err != nil {
		t.Fatalf("idempotent Roll replay failed: %v", err)
	}
	if duplicate.Rolled || duplicate.Sealed.Generation != result.Sealed.Generation || replica.Generation() != 2 {
		t.Fatalf("idempotent replay changed the Ledger: result=%#v generation=%d", duplicate, replica.Generation())
	}
	if _, err := replica.ApplyReplicatedRoll(wrong); err == nil {
		t.Fatal("completed Roll accepted different Segment metadata")
	}

	empty, err := OpenWithOptions(filepath.Join(directory, "empty", "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if _, err := empty.ApplyReplicatedRoll(result.Sealed); err == nil {
		t.Fatal("fresh empty Replica accepted a Roll it never landed")
	}
}
