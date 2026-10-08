package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeFrameProfilePrepares1024MutationsBeforeOneDurableBarrier(t *testing.T) {
	s, _ := testStore(t, 0)
	old, err := os.ReadFile(filepath.Join(s.dir, "active.root"))
	if err != nil {
		t.Fatal(err)
	}
	barriers := 0
	receipt, err := s.UpdateNativeFrame(context.Background(), nextCut(1), func(tx *Transaction) error {
		for i := 0; i < 1024; i++ {
			if err := tx.Put([]byte(fmt.Sprintf("key_%06d", i)), []byte("value")); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		barriers++
		now, err := os.ReadFile(filepath.Join(s.dir, "active.root"))
		if err != nil {
			return err
		}
		if !bytes.Equal(old, now) {
			return errors.New("root published before native")
		}
		if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrBusy) {
			return errors.New("candidate visible before native")
		}
		return nil
	})
	if err != nil || barriers != 1 || receipt.Rows != 1024 || receipt.Revision != 2 {
		t.Fatal(receipt, barriers, err)
	}
	if len(storeRows(t, s)) != 1024 {
		t.Fatal("partial frame")
	}
	ordinary, _ := testStore(t, 0)
	barrier := false
	if _, err := ordinary.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		for i := 0; i <= maxUpdateRows; i++ {
			if err := tx.Put([]byte(fmt.Sprintf("k_%d", i)), nil); err != nil {
				return err
			}
		}
		barrier = true
		return nil
	}); !errors.Is(err, ErrLimit) || barrier {
		t.Fatal("ordinary profile expanded", err)
	}
}

func TestNativeFrameBarrierFailureAndPanicRetireBeforeRootPublish(t *testing.T) {
	for _, mode := range []string{"error", "panic", "no-candidate-change"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testStore(t, 3)
			before := s.current
			root, err := os.ReadFile(filepath.Join(s.dir, "active.root"))
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("native durable uncertainty")
			var receipt UpdateReceipt
			func() {
				defer func() {
					if mode == "panic" && recover() == nil {
						t.Error("panic lost")
					}
				}()
				receipt, err = s.UpdateNativeFrame(context.Background(), nextCut(1), func(tx *Transaction) error {
					if mode == "no-candidate-change" {
						return nil
					}
					return tx.Put([]byte("key_000001"), []byte("new"))
				}, func() error {
					if mode == "panic" {
						panic(failure)
					}
					return failure
				})
			}()
			if mode != "panic" && !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if receipt != (UpdateReceipt{}) || s.current != before {
				t.Fatal("partial root", receipt)
			}
			after, err := os.ReadFile(filepath.Join(s.dir, "active.root"))
			if err != nil || !bytes.Equal(root, after) {
				t.Fatal("root replaced", err)
			}
			if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrRecovery) {
				t.Fatal("barrier uncertainty remained queryable", err)
			}
		})
	}
}

func TestNativeFrameAppendLimitRefusesBeforeDurable(t *testing.T) {
	s, _ := testStore(t, 3)
	s.appendLimit = 1
	called := false
	_, err := s.UpdateNativeFrame(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), []byte("changed")) }, func() error { called = true; return nil })
	if !errors.Is(err, ErrLimit) || called {
		t.Fatal("capacity rejection occurred after native", called, err)
	}
}
