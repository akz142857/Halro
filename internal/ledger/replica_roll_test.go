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

	emptyPath := filepath.Join(directory, "empty", "ledger.wal")
	if err := os.MkdirAll(filepath.Dir(emptyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty, err := OpenWithOptions(emptyPath, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if _, err := empty.ApplyReplicatedRoll(result.Sealed); err == nil {
		t.Fatal("fresh empty Replica accepted a Roll it never landed")
	}
}

func TestRepairReplicaRollRequiresOrderingCursorAndResolvesBothCrashSides(t *testing.T) {
	primaryPath := filepath.Join(t.TempDir(), "primary", "ledger.wal")
	primary, err := OpenWithOptions(primaryPath, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append(context.Background(), validReservation("evt_repair_roll", "att_repair_roll")); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := primary.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}

	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before rename", true: "after rename before successor"}[renamed], func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "ledger.wal")
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			pending := result.Sealed
			if err := saveSegmentManifest(directory, segmentManifest{Pending: &pending}); err != nil {
				t.Fatal(err)
			}
			if renamed {
				if err := os.Rename(path, filepath.Join(directory, pending.File)); err != nil {
					t.Fatal(err)
				}
			}
			if err := RepairReplicaRoll(path, testChainKey, 1, 2); err == nil {
				t.Fatal("pending Roll accepted a mismatched ordering cursor")
			}
			if err := RepairReplicaRoll(path, testChainKey, 1, 1); err != nil {
				t.Fatal(err)
			}
			manifest, err := loadSegmentManifest(directory)
			if err != nil || manifest.Pending != nil {
				t.Fatalf("resolved manifest=%#v err=%v", manifest, err)
			}
			replica, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
			if err != nil {
				t.Fatal(err)
			}
			defer replica.Close()
			wantGeneration := uint64(1)
			if renamed {
				wantGeneration = 2
			}
			if replica.Generation() != wantGeneration {
				t.Fatalf("generation=%d want=%d", replica.Generation(), wantGeneration)
			}
			if renamed {
				duplicate, err := replica.ApplyReplicatedRoll(result.Sealed)
				if err != nil || duplicate.Rolled {
					t.Fatalf("retransmitted resolved Roll result=%#v err=%v", duplicate, err)
				}
			}
		})
	}
}
