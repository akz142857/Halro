package scratchstate

import (
	"context"
	"errors"
	"testing"
	"unsafe"
)

func TestRowBufferActualTamperFailsAtFlush(t *testing.T) {
	s, _ := testStore(t, 8<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("hot"), []byte("before")) }); err != nil {
		t.Fatal(err)
	}
	b := new(RowBuffer)
	err := s.Update(ctx, func(tx *Transaction) error {
		defer b.Reset()
		if err := b.Put(tx, []byte("hot"), []byte("after")); err != nil {
			return err
		}
		if _, err := s.files[0].WriteAt([]byte{0xff}, int64(s.root.id)*pageBytes+30); err != nil {
			return err
		}
		// Even an ignored facade failure remains sticky on the actual transaction.
		_ = b.Flush(tx)
		return nil
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("tampered backing accepted", err)
	}
	if err := s.View(ctx, func(*Transaction) error { t.Fatal("poisoned exported"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}

func TestRowBufferTransactionBoundaryReadOnlyAndCancellation(t *testing.T) {
	if unsafe.Sizeof(RowBuffer{}) > RowBufferRAMBytes {
		t.Fatal("workspace exceeds declared credit")
	}
	for _, mode := range []string{"cross-transaction", "read-only", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testStore(t, 8<<20)
			b := new(RowBuffer)
			ctx := context.Background()
			if mode == "cross-transaction" {
				if err := s.Update(ctx, func(tx *Transaction) error {
					if err := b.Put(tx, []byte("key"), []byte("one")); err != nil {
						return err
					}
					return b.Flush(tx)
				}); err != nil {
					t.Fatal(err)
				}
				err := s.Update(ctx, func(tx *Transaction) error { _ = b.Put(tx, []byte("key"), []byte("two")); return nil })
				if !errors.Is(err, ErrPoisoned) {
					t.Fatal("cross-transaction buffer accepted", err)
				}
			} else if mode == "read-only" {
				err := s.View(ctx, func(tx *Transaction) error { _ = b.Put(tx, []byte("key"), []byte("one")); return nil })
				if !errors.Is(err, ErrPoisoned) {
					t.Fatal("read-only mutation accepted", err)
				}
			} else {
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				err := s.Update(canceled, func(tx *Transaction) error {
					if err := b.Put(tx, []byte("key"), []byte("one")); err != nil {
						return err
					}
					cancel()
					_, _, _ = b.Get(tx, []byte("key"))
					_ = b.Flush(tx)
					return nil
				})
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled intent accepted", err)
				}
			}
			b.Reset()
		})
	}
}
