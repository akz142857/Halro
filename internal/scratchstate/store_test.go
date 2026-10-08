package scratchstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func testManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 2 << 30}, MaxClaims: 32, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}
func testStore(t *testing.T, ceiling int64) (*Store, *resourcebudget.Manager) {
	t.Helper()
	m := testManager(t)
	s, err := Create(context.Background(), filepath.Join(t.TempDir(), "private"), Options{ReuseAuthentication: true, Budget: m, MaxDiskBytes: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Discard() })
	return s, m
}
func verifyRows(t *testing.T, s *Store, want map[string][]byte) {
	t.Helper()
	err := s.View(context.Background(), func(tx *Transaction) error {
		keys := make([]string, 0, len(want))
		for key := range want {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		it, err := tx.Iterator()
		if err != nil {
			return err
		}
		for _, key := range keys {
			row, found, err := it.Next()
			if err != nil || !found || string(row.Key) != key || !bytes.Equal(row.Value, want[key]) {
				return fmt.Errorf("sorted row differs key=%q got=%q found=%v err=%v", key, row.Key, found, err)
			}
			value, found, err := tx.Get([]byte(key))
			if err != nil || !found || !bytes.Equal(value, want[key]) {
				return fmt.Errorf("lookup differs: %q", key)
			}
			floor, found, err := tx.Floor([]byte(key))
			if err != nil || !found || string(floor.Key) != key {
				return fmt.Errorf("floor differs: %q", key)
			}
		}
		if _, found, err := it.Next(); err != nil || found {
			return errors.Join(ErrCorrupt, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestScratchMutableRowsDifferentialAndFreeListReuse(t *testing.T) {
	s, m := testStore(t, 64<<20)
	ctx := context.Background()
	want := map[string][]byte{}
	random := rand.New(rand.NewSource(81))
	for batch := 0; batch < 30; batch++ {
		err := s.Update(ctx, func(tx *Transaction) error {
			for range 64 {
				key := fmt.Sprintf("%02d/request-%04d", random.Intn(8), random.Intn(400))
				if random.Intn(4) == 0 {
					if err := tx.Delete([]byte(key)); err != nil {
						return err
					}
					delete(want, key)
				} else {
					value := bytes.Repeat([]byte{byte(random.Intn(256))}, random.Intn(1500))
					if err := tx.Put([]byte(key), value); err != nil {
						return err
					}
					want[key] = value
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(batch, err)
		}
		verifyRows(t, s, want)
	}
	for cycle := 0; cycle < 3; cycle++ {
		if err := s.Update(ctx, func(tx *Transaction) error {
			for key := range want {
				if err := tx.Delete([]byte(key)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		clear(want)
		verifyRows(t, s, want)
		old := s.next
		if err := s.Update(ctx, func(tx *Transaction) error {
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("reuse-%04d", i)
				value := bytes.Repeat([]byte{byte(i)}, 650)
				if err := tx.Put([]byte(key), value); err != nil {
					return err
				}
				want[key] = value
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		verifyRows(t, s, want)
		if s.next != old {
			t.Fatal("free pages not reused", old, s.next)
		}
	}
	if used := m.Snapshot().Used; used.RAM != WorkspaceRAMBytes+2*resourcebudget.ClaimRAMBytes {
		t.Fatal("nonfixed workspace", used)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if used := m.Snapshot().Used; used != (resourcebudget.Cost{}) {
		t.Fatal("fees survived actual retirement", used)
	}
}
func TestScratchWideKeyValueAndPoisonRefusal(t *testing.T) {
	s, _ := testStore(t, 64<<20)
	key := bytes.Repeat([]byte{'k'}, MaxKeyBytes)
	value := bytes.Repeat([]byte{'v'}, MaxValueBytes)
	if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Put(key, value) }); err != nil {
		t.Fatal(err)
	}
	verifyRows(t, s, map[string][]byte{string(key): value})
	injected := errors.New("private reducer failure")
	if err := s.Update(context.Background(), func(tx *Transaction) error {
		if err := tx.Put([]byte("new"), nil); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if err := s.View(context.Background(), func(*Transaction) error { t.Fatal("poisoned scratch admitted reader"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}
func TestScratchPageTamperAndForeignNameKeepFees(t *testing.T) {
	for _, mode := range []string{"node", "blob", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			s, m := testStore(t, 64<<20)
			if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]byte("key"), bytes.Repeat([]byte{'v'}, 2000)) }); err != nil {
				t.Fatal(err)
			}
			if mode == "foreign" {
				path := filepath.Join(s.dir, "nodes")
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal(err)
				}
				if used := m.Snapshot().Used; used.RAM < WorkspaceRAMBytes {
					t.Fatal("refunded replaced owner", used)
				}
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "foreign" {
					t.Fatal(err)
				}
				return
			}
			kind := 0
			id := s.root.id
			if mode == "blob" {
				kind = 1
				n, err := s.readNode(s.root)
				if err != nil {
					t.Fatal(err)
				}
				id = n.valueRef.link.id
			}
			if _, err := s.files[kind].WriteAt([]byte{0xEE}, int64(id)*pageBytes+30); err != nil {
				t.Fatal(err)
			}
			if err := s.View(context.Background(), func(tx *Transaction) error { _, _, err := tx.Get([]byte("key")); return err }); !errors.Is(err, ErrCorrupt) {
				t.Fatal("tampered private fact was accepted", err)
			}
		})
	}
}
