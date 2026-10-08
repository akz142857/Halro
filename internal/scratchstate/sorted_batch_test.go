package scratchstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func TestSortedBatchCompleteRowsDifferentialAcrossFlushes(t *testing.T) {
	s, _ := testStore(t, 32<<20)
	ctx := context.Background()
	b := new(RowBuffer)
	want := map[string][]byte{}
	rng := rand.New(rand.NewSource(976))
	for batch := 0; batch < 12; batch++ {
		err := s.Update(ctx, func(tx *Transaction) error {
			defer b.Reset()
			for step := 0; step < 730; step++ {
				key := fmt.Sprintf("%c\x00/id_%04d", byte(rng.Intn(4)), rng.Intn(1800))
				old, found, err := b.Get(tx, []byte(key))
				if err != nil {
					return err
				}
				expected, present := want[key]
				if found != present || !bytes.Equal(old, expected) {
					return fmt.Errorf("point/absence differs %q", key)
				}
				if rng.Intn(5) == 0 {
					if err := b.Delete(tx, []byte(key)); err != nil {
						return err
					}
					delete(want, key)
				} else {
					value := bytes.Repeat([]byte{byte(step)}, rng.Intn(2100))
					if err := b.Put(tx, []byte(key), value); err != nil {
						return err
					}
					want[key] = value
					got, ok, err := b.Get(tx, []byte(key))
					if err != nil || !ok || !bytes.Equal(got, value) {
						return ErrCorrupt
					}
					if len(got) > 0 {
						got[0] ^= 1
					}
				}
				if step%211 == 0 {
					if err := b.Flush(tx); err != nil {
						return err
					}
				}
			}
			return b.Flush(tx)
		})
		if err != nil {
			t.Fatal(batch, err)
		}
		verifyRows(t, s, want)
	}
	for cycle := 0; cycle < 2; cycle++ {
		if err := s.Update(ctx, func(tx *Transaction) error {
			defer b.Reset()
			for key := range want {
				if err := b.Delete(tx, []byte(key)); err != nil {
					return err
				}
			}
			return b.Flush(tx)
		}); err != nil {
			t.Fatal(err)
		}
		clear(want)
		verifyRows(t, s, want)
		if err := s.Update(ctx, func(tx *Transaction) error {
			defer b.Reset()
			for i := 0; i < 1200; i++ {
				key := fmt.Sprintf("reused_%04d", i)
				value := []byte(key)
				want[key] = value
				if err := b.Put(tx, []byte(key), value); err != nil {
					return err
				}
			}
			return b.Flush(tx)
		}); err != nil {
			t.Fatal(err)
		}
		verifyRows(t, s, want)
	}
}

func TestSortedBatchOriginalPresenceAndValueRemainBinding(t *testing.T) {
	for _, mode := range []string{"present-value", "present-delete", "absent-insert", "clean-absent", "clean-present"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testStore(t, 8<<20)
			ctx := context.Background()
			b := new(RowBuffer)
			present := mode == "present-value" || mode == "present-delete" || mode == "clean-present"
			if present {
				if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("target"), []byte("original")) }); err != nil {
					t.Fatal(err)
				}
			}
			err := s.Update(ctx, func(tx *Transaction) error {
				defer b.Reset()
				if _, _, err := b.Get(tx, []byte("target")); err != nil {
					return err
				}
				if mode == "present-delete" {
					if err := b.Delete(tx, []byte("target")); err != nil {
						return err
					}
				} else if mode == "present-value" || mode == "absent-insert" {
					if err := b.Put(tx, []byte("target"), []byte("intent")); err != nil {
						return err
					}
				}
				if err := tx.Put([]byte("target"), []byte("different actual state")); err != nil {
					return err
				}
				_ = b.Flush(tx)
				return nil
			})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatal("original mismatch accepted", err)
			}
			if err := s.View(ctx, func(*Transaction) error { t.Fatal("poisoned export"); return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
		})
	}
}

func TestSortedBatchReadOnlyFlushAndWideFallback(t *testing.T) {
	s, _ := testStore(t, 32<<20)
	ctx := context.Background()
	b := new(RowBuffer)
	long := bytes.Repeat([]byte{'k'}, hotScratchKeyBytes+1)
	wide := bytes.Repeat([]byte{'v'}, hotScratchValueBytes+1)
	want := map[string][]byte{"empty": {}, "wide": wide, string(long): []byte("long key")}
	if err := s.Update(ctx, func(tx *Transaction) error {
		defer b.Reset()
		for k, v := range want {
			if err := b.Put(tx, []byte(k), v); err != nil {
				return err
			}
		}
		return b.Flush(tx)
	}); err != nil {
		t.Fatal(err)
	}
	root, operation := s.root, s.operation
	if err := s.View(ctx, func(tx *Transaction) error {
		defer b.Reset()
		for _, k := range []string{"empty", "absent"} {
			if _, _, err := b.Get(tx, []byte(k)); err != nil {
				return err
			}
		}
		return b.Flush(tx)
	}); err != nil {
		t.Fatal(err)
	}
	if s.root != root || s.operation != operation {
		t.Fatal("read intents changed tree")
	}
	verifyRows(t, s, want)
}
