package metadatastate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	bbolt "go.etcd.io/bbolt"
)

func TestBucketRangeBinaryNamesAndDescendantsMatchActualBboltDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := bbolt.Open(filepath.Join(dir, "source.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	name := []byte{'p', 0, '/'}
	if err := db.Update(func(tx *bbolt.Tx) error {
		root, err := tx.CreateBucket([]byte("deployment_price_timeline"))
		if err != nil {
			return err
		}
		for _, key := range [][]byte{[]byte("p"), name, {'p', 0, '/', 0}, []byte("q")} {
			child, err := root.CreateBucket(key)
			if err != nil {
				return err
			}
			if err := child.SetSequence(17); err != nil {
				return err
			}
			if err := child.Put([]byte{0, 'k'}, []byte("unchanged-neighbor")); err != nil {
				return err
			}
		}
		child := root.Bucket(name)
		for i := 0; i < 3000; i++ {
			if err := child.Put([]byte(fmt.Sprintf("k_%05d", i)), []byte("target-value")); err != nil {
				return err
			}
		}
		grandchild, err := child.CreateBucket([]byte{0, '/', 255})
		if err != nil {
			return err
		}
		if _, err := grandchild.CreateBucket([]byte("empty")); err != nil {
			return err
		}
		return grandchild.Put([]byte("deep"), []byte("nested-value"))
	}); err != nil {
		t.Fatal(err)
	}
	options := authenticatedstate.StoreOptions{
		Scope: sha256.Sum256([]byte("private-test-scope")), ExpectedCut: sha256.Sum256([]byte("private-test-cut")),
		Key: bytes.Repeat([]byte{0x71}, 32), MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20, Budget: baselineBudget(t),
	}
	var store *authenticatedstate.Store
	if err := db.View(func(tx *bbolt.Tx) error {
		iterator := newBaselineIterator(ctx, tx)
		var err error
		// Only this explicit private test treats its own complete fixture as a
		// trusted import stream; no live DB production factory is introduced.
		store, err = authenticatedstate.CreateStore(ctx, filepath.Join(dir, "root"), options, iterator.next)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Discard(); err != nil {
			t.Error(err)
		}
	}()
	old, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	lo, hi, err := BucketRange([][]byte{[]byte("deployment_price_timeline"), name})
	if err != nil {
		t.Fatal(err)
	}
	var expected uint64
	if err := db.View(func(tx *bbolt.Tx) error {
		iterator := newBaselineIterator(ctx, tx)
		for {
			row, found, err := iterator.next()
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			if bytes.Compare(row.Key, lo) >= 0 && bytes.Compare(row.Key, hi) < 0 {
				expected++
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if expected != 3005 {
		t.Fatal("unexpected native subtree size", expected)
	}
	cut := sha256.Sum256([]byte("native-delete-bucket-durable-cut"))
	receipt, err := store.Update(ctx, cut, func(tx *authenticatedstate.Transaction) error {
		removed, err := tx.DeleteRange(lo, hi)
		if err != nil || removed != expected {
			return fmt.Errorf("removed %d expected %d: %w", removed, expected, err)
		}
		return nil
	})
	if err != nil || receipt.ReadPages > 256 || receipt.WrittenNodes > 192 {
		t.Fatal(receipt, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("deployment_price_timeline")).DeleteBucket(name) }); err != nil {
		t.Fatal(err)
	}
	var want []authenticatedstate.Row
	if err := db.View(func(tx *bbolt.Tx) error {
		iterator := newBaselineIterator(ctx, tx)
		for {
			row, found, err := iterator.next()
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			want = append(want, authenticatedstate.Row{Key: bytes.Clone(row.Key), Value: bytes.Clone(row.Value)})
		}
	}); err != nil {
		t.Fatal(err)
	}
	now, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer now.Close()
	var got []authenticatedstate.Row
	cursor := ""
	for {
		page, err := now.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Rows...)
		if page.Done {
			break
		}
		cursor = page.Cursor
	}
	if !reflect.DeepEqual(got, want) || receipt.Rows != uint64(len(want)) {
		t.Fatal("native delete and authenticated survivors differ", got, want)
	}
	if value, found, err := old.Get(ctx, lo); err != nil || !found || len(value) != 8 {
		t.Fatal("old bucket marker changed", value, found, err)
	}
	if _, found, err := now.Get(ctx, lo); err != nil || found {
		t.Fatal("removed marker survived", found, err)
	}
}

func TestBucketRangeRejectsInvalidPathsAndOwnsBinaryBounds(t *testing.T) {
	for _, path := range [][][]byte{nil, {nil}, {bytes.Repeat([]byte{0}, authenticatedstate.MaxKeyBytes)}, make([][]byte, maxBaselineDepth+1)} {
		if lo, hi, err := BucketRange(path); !errors.Is(err, authenticatedstate.ErrLimit) || lo != nil || hi != nil {
			t.Fatal(lo, hi, err)
		}
	}
	path := [][]byte{{0, 255, '/'}}
	lo, hi, err := BucketRange(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeLo, beforeHi := bytes.Clone(lo), bytes.Clone(hi)
	path[0][0] = 77
	if !bytes.Equal(lo, beforeLo) || !bytes.Equal(hi, beforeHi) {
		t.Fatal("bounds alias input")
	}
	lo[0] = 0
	if !bytes.Equal(hi, beforeHi) {
		t.Fatal("bounds alias each other")
	}
}
