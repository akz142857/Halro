package scratchstate

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func seedChildReceipt(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Update(context.Background(), func(tx *Transaction) error {
		for i := 0; i < 24; i++ {
			if err := tx.Put([]byte(fmt.Sprintf("key-%03d", i)), []byte("value")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func checkedParent(t *testing.T, s *Store) (node, childReceipt) {
	t.Helper()
	n, err := s.readNode(s.root)
	if err != nil {
		t.Fatal(err)
	}
	root := n.left
	if root.empty() {
		root = n.right
	}
	if root.empty() {
		t.Fatal("seed root has no child")
	}
	child, err := s.readChild(root)
	if err != nil {
		t.Fatal(err)
	}
	return n, child
}
func TestScratchChildReceiptExactEncodingAndSingleConsumption(t *testing.T) {
	s, _ := testStore(t, 4<<20)
	seedChildReceipt(t, s)
	err := s.Update(context.Background(), func(*Transaction) error {
		if err := s.startMutation(); err != nil {
			return err
		}
		n, checked := checkedParent(t, s)
		original, err := s.writeNode(n)
		if err != nil {
			return err
		}
		actual, err := s.writeNodeWithChild(n, &checked)
		if err != nil || actual != original {
			t.Fatal("same-operation child changed authenticated encoding", actual, original, err)
		}
		if _, err := s.writeNodeWithChild(n, &checked); !errors.Is(err, ErrCorrupt) {
			t.Fatal("consumed receipt reused", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.View(context.Background(), func(tx *Transaction) error {
		iterator, err := tx.Iterator()
		if err != nil {
			return err
		}
		rows := 0
		for {
			_, found, err := iterator.Next()
			if err != nil {
				return err
			}
			if !found {
				if rows != 24 {
					t.Fatal(rows)
				}
				return nil
			}
			rows++
		}
	}); err != nil {
		t.Fatal(err)
	}
}
func TestScratchChildReceiptRefusesForeignStaleAndInvalidRelationships(t *testing.T) {
	for _, mode := range []string{"foreign", "stale", "membership", "priority", "range", "count", "actual-page-MAC"} {
		t.Run(mode, func(t *testing.T) {
			s, manager := testStore(t, 4<<20)
			seedChildReceipt(t, s)
			foreign, _ := testStore(t, 4<<20)
			seedChildReceipt(t, foreign)
			before := manager.Snapshot().Used
			err := s.Update(context.Background(), func(tx *Transaction) error {
				if err := s.startMutation(); err != nil {
					return err
				}
				parent, checked := checkedParent(t, s)
				switch mode {
				case "foreign":
					_, checked = checkedParent(t, foreign)
				case "stale":
					if err := tx.Put([]byte("another actual operation"), []byte("next")); err != nil {
						return err
					}
				case "membership":
					parent.left = link{}
					parent.right = link{}
				case "priority":
					checked.child.priority = parent.priority - 1
					if parent.priority == 0 {
						t.Fatal("unexpected zero priority")
					}
				case "range":
					checked.child.key = parent.key
				case "count":
					checked.child.rows = s.next[0]
				case "actual-page-MAC":
					if _, err := s.files[0].WriteAt([]byte{0xff}, int64(checked.root.id)*pageBytes+30); err != nil {
						return err
					}
					_, err := s.readChild(checked.root)
					return err
				}
				_, err := s.writeNodeWithChild(parent, &checked)
				return err
			})
			if !errors.Is(err, ErrCorrupt) || manager.Snapshot().Used != before {
				t.Fatal("invalid child receipt admitted/refunded", err, manager.Snapshot())
			}
			if err := s.View(context.Background(), func(*Transaction) error { t.Fatal("poisoned child exported"); return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
		})
	}
}
