package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreDeleteRangePrunesLargeInteriorPreservesPinsAndReopens(t *testing.T) {
	ctx := context.Background()
	s, options := testStore(t, 10000)
	old, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var count uint64
	receipt, err := s.Update(ctx, nextCut(1), func(tx *Transaction) error {
		var err error
		count, err = tx.DeleteRange([]byte("key_000013"), []byte("key_009987"))
		if err != nil {
			return err
		}
		if _, found, err := tx.Get([]byte("key_005000")); err != nil || found {
			return fmt.Errorf("private deleted row found=%v: %w", found, err)
		}
		return tx.Put([]byte("key_005000"), []byte("new-generation"))
	})
	if err != nil || count != 9974 || receipt.Rows != 27 || receipt.ReadPages > 8*maxHeight || receipt.WrittenNodes > 6*maxHeight || receipt.AppendedBytes > 256<<10 {
		t.Fatal(count, receipt, err)
	}
	if value, found, err := old.Get(ctx, []byte("key_005000")); err != nil || !found || string(value) != "value_005000" {
		t.Fatal("old pin changed", string(value), found, err)
	}
	actual := storeRows(t, s)
	want := make([]Row, 0, 27)
	for i := 0; i < 10000; i++ {
		if i >= 13 && i < 9987 && i != 5000 {
			continue
		}
		value := fmt.Sprintf("value_%06d", i)
		if i == 5000 {
			value = "new-generation"
		}
		want = append(want, Row{[]byte(fmt.Sprintf("key_%06d", i)), []byte(value)})
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatal("survivor set differs", actual)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	options.ExpectedCut = nextCut(1)
	reopened, err := OpenStore(ctx, s.dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(storeRows(t, reopened), want) {
		t.Fatal("reopen lost survivor set")
	}
}

func TestStoreDeleteRangeMixedChurnMatchesIndependentMap(t *testing.T) {
	s, _ := testStore(t, 0)
	ctx := context.Background()
	random := rand.New(rand.NewSource(71))
	want := map[string]string{}
	key := func(i int) string { return fmt.Sprintf("k_%03d", i) }
	for batch := 1; batch <= 40; batch++ {
		_, err := s.Update(ctx, nextCut(batch), func(tx *Transaction) error {
			for i := 0; i < 8; i++ {
				k, v := key(random.Intn(300)), fmt.Sprintf("v_%d_%d", batch, i)
				if err := tx.Put([]byte(k), []byte(v)); err != nil {
					return err
				}
				want[k] = v
			}
			lo, hi := random.Intn(200), 200+random.Intn(101)
			var expected uint64
			for k := range want {
				if k >= key(lo) && k < key(hi) {
					delete(want, k)
					expected++
				}
			}
			removed, err := tx.DeleteRange([]byte(key(lo)), []byte(key(hi)))
			if err != nil || removed != expected {
				return fmt.Errorf("removed %d expected %d: %w", removed, expected, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(batch, err)
		}
		got := map[string]string{}
		for _, row := range storeRows(t, s) {
			got[string(row.Key)] = string(row.Value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("mixed range/point mutations differ", batch, got, want)
		}
	}
}

func TestStoreDeleteRangeOpenEndpointsEmptyNoopAndSameFrameRecreation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lo, hi  []byte
		removed uint64
	}{
		{"all", nil, nil, 9},
		{"prefix", nil, []byte("key_000004"), 4},
		{"suffix", []byte("key_000004"), nil, 5},
		{"outside", []byte("zz"), nil, 0},
		{"gap", []byte("key_000002a"), []byte("key_000003"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testStore(t, 9)
			before := s.current.content
			receipt, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
				removed, err := tx.DeleteRange(tc.lo, tc.hi)
				if err != nil || removed != tc.removed {
					return fmt.Errorf("removed %d: %w", removed, err)
				}
				if tc.name == "all" {
					if tx.candidate.count != 0 || tx.candidate.height != 0 {
						return ErrCorrupt
					}
					return tx.Put([]byte("key_000004"), []byte("recreated"))
				}
				return nil
			})
			want := 9 - tc.removed
			if tc.name == "all" {
				want++
			}
			if err != nil || receipt.Rows != want {
				t.Fatal(receipt, err)
			}
			if tc.removed == 0 && (s.current.content != before || receipt.AppendedBytes != 0 || receipt.WrittenNodes != 0) {
				t.Fatal("no-op appended content", receipt)
			}
		})
	}
	s, _ := testStore(t, 0)
	if _, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		count, err := tx.DeleteRange(nil, nil)
		if count != 0 {
			return ErrCorrupt
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDeleteRangeIgnoredLimitCannotPublishAndEscapedTxIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lo, hi []byte
	}{
		{"equal", []byte("key"), []byte("key")},
		{"inverted", []byte("z"), []byte("a")},
		{"wide", bytes.Repeat([]byte{'a'}, MaxKeyBytes+1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testStore(t, 3)
			before := s.current
			var escaped *Transaction
			receipt, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
				escaped = tx
				_, _ = tx.DeleteRange(tc.lo, tc.hi)
				return nil
			})
			if !errors.Is(err, ErrLimit) || receipt != (UpdateReceipt{}) || s.current != before {
				t.Fatal("ignored invalid range published", receipt, err)
			}
			if count, err := escaped.DeleteRange(nil, nil); count != 0 || !errors.Is(err, ErrClosed) {
				t.Fatal("escaped mutation", count, err)
			}
			if len(storeRows(t, s)) != 3 {
				t.Fatal("preflight failure changed root")
			}
		})
	}
}

func TestStoreDeleteRangeUsesCumulativeMutationAndReadLimits(t *testing.T) {
	for _, mode := range []string{"writes", "reads", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testStore(t, 5)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := s.current
			expected := ErrLimit
			if mode == "cancel" {
				expected = context.Canceled
			}
			receipt, err := s.Update(ctx, nextCut(1), func(tx *Transaction) error {
				switch mode {
				case "writes":
					for i := 0; i < maxUpdateRows; i++ {
						if _, err := tx.DeleteRange([]byte("zz"), nil); err != nil {
							return err
						}
					}
				case "reads":
					tx.reads = maxUpdateReads
				case "cancel":
					cancel()
				}
				_, _ = tx.DeleteRange(nil, nil)
				return nil
			})
			if !errors.Is(err, expected) || receipt != (UpdateReceipt{}) || s.current != before {
				t.Fatal("range bypassed admission", receipt, err)
			}
		})
	}
}

func TestStoreDeleteRangeCorruptBoundaryCannotPublish(t *testing.T) {
	s, _ := testStore(t, 256)
	pin, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	leaf := firstLeaf(t, pin, []byte("key_000013"))
	// Locate a boundary branch independently, then corrupt its bytes. A wholly
	// discarded leaf need not be read, whereas this partial branch must be.
	q := query{ctx: context.Background(), snapshot: pin, maxReads: 4096}
	d := descriptor{count: pin.current.count, height: pin.current.height, ref: pin.current.root}
	for d.height > 2 {
		n, err := q.load(d)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range n.children {
			if bytes.Compare(c.first, leaf.key) <= 0 && bytes.Compare(c.last, leaf.key) >= 0 {
				d = c
				found = true
				break
			}
		}
		if !found {
			t.Fatal("missing boundary")
		}
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "pages"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteAt([]byte{0xff}, int64(d.ref.offset)); err != nil {
		t.Fatal(err)
	}
	before := s.current
	receipt, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		_, err := tx.DeleteRange([]byte("key_000013x"), []byte("key_000199"))
		return err
	})
	if !errors.Is(err, ErrCorrupt) || receipt != (UpdateReceipt{}) || s.current != before {
		t.Fatal(receipt, err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatal("fault not latched", err)
	}
}

func TestStoreDeleteRangeCallbackFailureKeepsPublishedRootAndRequiresReopen(t *testing.T) {
	s, options := testStore(t, 64)
	before := s.current
	failure := errors.New("native frame reduction failed")
	receipt, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		if _, err := tx.DeleteRange([]byte("key_000003"), []byte("key_000053")); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) || receipt != (UpdateReceipt{}) || s.current != before {
		t.Fatal(receipt, err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatal("unpublished append stayed queryable", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(context.Background(), s.dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(storeRows(t, reopened)) != 64 {
		t.Fatal("failed batch published prefix")
	}
}

func TestStoreDeleteRangeWholeTreeUsesCommittedCountAndOldPinStillAuthenticatesValues(t *testing.T) {
	s, _ := testStore(t, 4096)
	ctx := context.Background()
	old, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	leaf := firstLeaf(t, old, []byte("key_002000"))
	f, err := os.OpenFile(filepath.Join(s.dir, "pages"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, int64(leaf.value.offset+1)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The independently authorized whole-range operation makes no claim about
	// discarded value bytes. Its count comes from the actual authenticated root,
	// while old pins must still verify any old value they return.
	receipt, err := s.Update(ctx, nextCut(1), func(tx *Transaction) error {
		removed, err := tx.DeleteRange(nil, nil)
		if err != nil || removed != 4096 {
			return fmt.Errorf("removed %d: %w", removed, err)
		}
		return nil
	})
	if err != nil || receipt.Rows != 0 || receipt.ReadPages != 1 || receipt.WrittenNodes != 0 || receipt.AppendedBytes != 0 {
		t.Fatal(receipt, err)
	}
	if value, found, err := old.Get(ctx, leaf.key); !errors.Is(err, ErrCorrupt) || found || value != nil {
		t.Fatal("old pin returned damaged discarded content", value, found, err)
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatal("observed corruption did not revoke store", err)
	}
}
