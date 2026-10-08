package scratchstate

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func TestScratchCompoundKeysUseAuthenticatedNodePadding(t *testing.T) {
	for _, size := range []int{33, 64, 65, 73, 97, 129, inlineKeyBytes, inlineKeyBytes + 1, MaxKeyBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, _ := testStore(t, 16<<20)
			key := bytes.Repeat([]byte{'k'}, size)
			value := bytes.Repeat([]byte{'v'}, inlineValueBytes)
			if err := s.Update(context.Background(), func(tx *Transaction) error {
				return tx.Put(key, value)
			}); err != nil {
				t.Fatal(err)
			}
			n, err := s.readNode(s.root)
			if err != nil || !bytes.Equal(n.key, key) || !bytes.Equal(n.value, value) {
				t.Fatal("actual node key/value differs", err)
			}
			if size <= inlineKeyBytes && (!n.keyRef.link.empty() || s.next[1] != 1) {
				t.Fatal("compound key allocated an avoidable blob", s.next[1])
			}
			if size > inlineKeyBytes && n.keyRef.link.empty() {
				t.Fatal("wide key bypassed authenticated blob")
			}
			verifyRows(t, s, map[string][]byte{string(key): value})
			if err := s.Update(context.Background(), func(tx *Transaction) error {
				return tx.Put(key, bytes.Repeat([]byte{'w'}, inlineValueBytes+1))
			}); err != nil {
				t.Fatal(err)
			}
			verifyRows(t, s, map[string][]byte{string(key): bytes.Repeat([]byte{'w'}, inlineValueBytes+1)})
			if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Delete(key) }); err != nil {
				t.Fatal(err)
			}
			verifyRows(t, s, map[string][]byte{})
		})
	}
}

func TestScratchInlineCompoundKeyTailRejectsActualTamper(t *testing.T) {
	s, _ := testStore(t, 4<<20)
	key := bytes.Repeat([]byte{'k'}, inlineKeyBytes)
	if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Put(key, []byte("value")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.files[0].WriteAt([]byte{'x'}, int64(s.root.id)*pageBytes+inlineKeyTailOffset); err != nil {
		t.Fatal(err)
	}
	if err := s.View(context.Background(), func(tx *Transaction) error {
		_, _, err := tx.Get(key)
		return err
	}); err == nil {
		t.Fatal("inline key tail escaped page authentication")
	}
}
