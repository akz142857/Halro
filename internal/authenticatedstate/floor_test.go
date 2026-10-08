package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestFloorProvesBoundariesAndPrivateDeletes(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{0, 1, 5, 65, 1025} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _ := testStore(t, count)
			pin, err := s.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			check := func(target string, want int) {
				t.Helper()
				row, found, err := pin.Floor(ctx, []byte(target))
				if err != nil || found != (want >= 0) {
					t.Fatal(target, row, found, err)
				}
				if want >= 0 && (string(row.Key) != fmt.Sprintf("key_%06d", want) || string(row.Value) != fmt.Sprintf("value_%06d", want)) {
					t.Fatal(target, row, want)
				}
			}
			check("a", -1)
			check("z", count-1)
			for i := 0; i < count; i++ {
				check(fmt.Sprintf("key_%06d", i), i)
				check(fmt.Sprintf("key_%06d!", i), i)
			}
			if count == 0 {
				return
			}
			_, err = s.Update(ctx, nextCut(77), func(tx *Transaction) error {
				key := []byte(fmt.Sprintf("key_%06d", count-1))
				if err := tx.Delete(key); err != nil {
					return err
				}
				row, found, err := tx.Floor(key)
				if err != nil || found != (count > 1) || found && string(row.Key) != fmt.Sprintf("key_%06d", count-2) {
					return fmt.Errorf("private floor after delete: %v %v %v", row, found, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			check("z", count-1) // The retained old root stays stable.
		})
	}
}

func TestFloorCorruptionAndIgnoredLimitNeverBecomeAbsence(t *testing.T) {
	ctx := context.Background()
	t.Run("selected-value-corrupt", func(t *testing.T) {
		s, path := testBuild(t, testOptions(t), 20)
		key := []byte("key_000005")
		leaf := firstLeaf(t, s, key)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteAt([]byte{0xff}, int64(leaf.value.offset+1))
		if err := errors.Join(writeErr, f.Close()); err != nil {
			t.Fatal(err)
		}
		row, found, err := s.Floor(ctx, append(bytes.Clone(key), '!'))
		if !errors.Is(err, ErrCorrupt) || found || row.Key != nil || row.Value != nil {
			t.Fatal(row, found, err)
		}
		if _, _, err = s.Floor(ctx, []byte("z")); !errors.Is(err, ErrCorrupt) {
			t.Fatal("integrity failure was not sticky", err)
		}
	})
	t.Run("ignored-read-limit", func(t *testing.T) {
		s, _ := testStore(t, 1)
		_, err := s.Update(ctx, nextCut(78), func(tx *Transaction) error {
			for i := 0; i <= maxReadCalls; i++ {
				_, _, _ = tx.Floor([]byte("z"))
			}
			return nil
		})
		if !errors.Is(err, ErrLimit) {
			t.Fatal("ignored floor limit published", err)
		}
	})
}
