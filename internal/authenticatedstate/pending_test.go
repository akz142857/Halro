package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// reachableNodes collects the offsets of every node reachable from a pinned
// root, verifying each read like an ordinary query.
func reachableNodes(t testing.TB, s *Store) map[uint64]bool {
	t.Helper()
	pin, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	out := map[uint64]bool{}
	if pin.current.count == 0 {
		return out
	}
	q := query{ctx: context.Background(), snapshot: pin, maxReads: 1 << 30}
	var walk func(descriptor)
	walk = func(d descriptor) {
		n, err := q.load(d)
		if err != nil {
			t.Fatal(err)
		}
		out[d.ref.offset] = true
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(descriptor{count: pin.current.count, height: pin.current.height, ref: pin.current.root})
	return out
}

func pendingTestBatch(i int) (key, value []byte) {
	return []byte(fmt.Sprintf("key_%06d_new", (i*7919)%2000)), bytes.Repeat([]byte{byte(i)}, 40+i%17)
}

// Board §57 criterion 1: a batch writes exactly the nodes its new root reaches
// that the old root does not; nodes replaced within the batch never reach the pack.
func TestTransactionWritesOnlyNodesTheNewRootReaches(t *testing.T) {
	s, _ := testStore(t, 500)
	before := reachableNodes(t, s)
	receipt, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		for i := range 64 {
			key, value := pendingTestBatch(i)
			if err := tx.Put(key, value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := reachableNodes(t, s)
	fresh := 0
	for offset := range after {
		if !before[offset] {
			fresh++
		}
	}
	if receipt.WrittenNodes != fresh {
		t.Fatal("batch wrote nodes its new root does not reach", receipt.WrittenNodes, fresh)
	}
}

// Board §57 criterion 3: partial write-outs under a small RAM bound give the
// same rows as a batch written once at commit.
func TestTransactionPartialWriteOutsKeepTheSameRows(t *testing.T) {
	run := func(bound int) []Row {
		saved := pendingFlushBytes
		pendingFlushBytes = bound
		defer func() { pendingFlushBytes = saved }()
		s, _ := testStore(t, 300)
		if _, err := s.UpdateNativeFrame(context.Background(), nextCut(1), func(tx *Transaction) error {
			for i := range 600 {
				key, value := pendingTestBatch(i)
				if err := tx.Put(key, value); err != nil {
					return err
				}
				if i%50 == 0 {
					if err := tx.Delete([]byte(fmt.Sprintf("key_%06d", i/2))); err != nil {
						return err
					}
				}
			}
			return nil
		}, func() error { return nil }); err != nil {
			t.Fatal(bound, err)
		}
		return storeRows(t, s)
	}
	whole, partial := run(1<<30), run(4<<10)
	if len(whole) != len(partial) {
		t.Fatal("partial write-outs changed the row set", len(whole), len(partial))
	}
	for i := range whole {
		if !bytes.Equal(whole[i].Key, partial[i].Key) || !bytes.Equal(whole[i].Value, partial[i].Value) {
			t.Fatal("partial write-outs changed a row", i)
		}
	}
}

// Board §57 criterion 4: a batch whose callback fails before any write-out
// leaves the pack untouched and the store healthy.
func TestTransactionCallbackFailureBeforeWriteOutKeepsStoreHealthy(t *testing.T) {
	s, _ := testStore(t, 100)
	before, extent := s.current, s.physicalExtent
	failure := errors.New("caller refused the batch")
	if _, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		for i := range 32 {
			key, value := pendingTestBatch(i)
			if err := tx.Put(key, value); err != nil {
				return err
			}
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if s.current != before || s.physicalExtent != extent {
		t.Fatal("failed batch changed the store", s.physicalExtent, extent)
	}
	if _, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		return tx.Put([]byte("key_after_failure"), []byte("v"))
	}); err != nil {
		t.Fatal("healthy store refused the next batch", err)
	}
}
