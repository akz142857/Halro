package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticatedSeekNearestAndStrictTraversalMatchesCompleteRows(t *testing.T) {
	ctx := context.Background()
	keys := [][]byte{[]byte{0}, []byte("a"), []byte("aa"), []byte("ab"), []byte("b"), []byte("ba"), []byte{0xff}, []byte{0xff, 0xff}}
	o := testOptions(t)
	i := 0
	s, err := Build(ctx, filepath.Join(t.TempDir(), "snapshot"), o, func() (Row, bool, error) {
		if i == len(keys) {
			return Row{}, false, nil
		}
		row := Row{Key: keys[i], Value: bytes.Clone(keys[i])}
		i++
		return row, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	for _, key := range append(keys, []byte("ac"), []byte("c"), []byte{0xff, 0xff, 0xff}) {
		for _, reverse := range []bool{false, true} {
			for _, exclusive := range []bool{false, true} {
				want := -1
				for i, k := range keys {
					n := bytes.Compare(k, key)
					eligible := n > 0 || n == 0 && !exclusive
					if reverse {
						eligible = n < 0 || n == 0 && !exclusive
					}
					if eligible && (want < 0 || reverse) {
						want = i
					}
				}
				row, found, err := s.Seek(ctx, SeekOptions{Key: key, Reverse: reverse, Exclusive: exclusive})
				if err != nil || found != (want >= 0) || found && (!bytes.Equal(row.Key, keys[want]) || !bytes.Equal(row.Value, keys[want])) {
					t.Fatal(key, reverse, exclusive, row, found, want, err)
				}
			}
		}
	}
	for _, reverse := range []bool{false, true} {
		key := []byte{0}
		if reverse {
			key = []byte{0xff, 0xff, 0xff}
		}
		seen := 0
		for {
			row, found, err := s.Seek(ctx, SeekOptions{Key: key, Reverse: reverse, Exclusive: seen > 0})
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				break
			}
			ordinal := seen
			if reverse {
				ordinal = len(keys) - 1 - seen
			}
			if seen >= len(keys) || !bytes.Equal(row.Key, keys[ordinal]) {
				t.Fatal("lost nearest-key completeness", seen, row)
			}
			seen++
			key = row.Key
		}
		if seen != len(keys) {
			t.Fatal(seen)
		}
	}
}

func TestAuthenticatedSeekCorruptContentReturnsNoRowAndPoisonsOwner(t *testing.T) {
	s, _ := testStore(t, 20)
	pin, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	leaf := firstLeaf(t, pin, []byte("key_000005"))
	f, err := os.OpenFile(filepath.Join(s.dir, "pages"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, int64(leaf.value.offset+1)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	row, found, err := pin.Seek(context.Background(), SeekOptions{Key: []byte("key_000005")})
	if !errors.Is(err, ErrCorrupt) || found || row.Key != nil || row.Value != nil {
		t.Fatal(row, found, err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
}

func TestTransactionSeekSeesPrivateInsertDeleteAndStrictNearest(t *testing.T) {
	s, _ := testStore(t, 3)
	_, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		if err := tx.Put([]byte("key_000001a"), []byte("inserted")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("key_000001")); err != nil {
			return err
		}
		for _, reverse := range []bool{false, true} {
			key := []byte("key_000001")
			if reverse {
				key = []byte("key_000002")
			}
			row, found, err := tx.Seek(SeekOptions{Key: key, Reverse: reverse, Exclusive: true})
			if err != nil || !found || string(row.Key) != "key_000001a" || string(row.Value) != "inserted" {
				t.Fatal(row, found, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransactionSeekIgnoredLimitAbortsPublication(t *testing.T) {
	s, _ := testStore(t, 1)
	before, err := s.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		for i := 0; i <= maxReadCalls; i++ {
			_, _, _ = tx.Seek(SeekOptions{Key: []byte("key_000000")})
		}
		return nil // the callback cannot erase the accumulated limit failure
	})
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	after, err := s.RootIdentity(context.Background())
	if err != nil || after != before {
		t.Fatal("ignored Seek error published a new root", before, after, err)
	}
}

func TestAuthenticatedPinnedCommittedExtentDoesNotFollowLaterPublication(t *testing.T) {
	s, _ := testStore(t, 1)
	pin, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	before, err := pin.CommittedBytes(context.Background())
	if err != nil || before <= headerBytes {
		t.Fatal(before, err)
	}
	if _, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		return tx.Put([]byte("later"), bytes.Repeat([]byte{'x'}, 1000))
	}); err != nil {
		t.Fatal(err)
	}
	old, err := pin.CommittedBytes(context.Background())
	if err != nil || old != before {
		t.Fatal("old pin observed publisher's later extent", before, old, err)
	}
	current, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	after, err := current.CommittedBytes(context.Background())
	if err != nil || after <= before {
		t.Fatal(before, after, err)
	}
}
