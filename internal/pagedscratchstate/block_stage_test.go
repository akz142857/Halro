package pagedscratchstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestScratchStagedAncestorOverwritesFlushBeforeSuccessfulView(t *testing.T) {
	s, manager := testStore(t, 4<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("old")) }); err != nil {
		t.Fatal(err)
	}
	fees := manager.Snapshot().Used
	var before, actual [pageBytes]byte
	if _, err := s.files[0].ReadAt(before[:], int64(s.root.id)*pageBytes); err != nil {
		t.Fatal(err)
	}
	err := s.Update(ctx, func(tx *Transaction) error {
		for i := range 64 {
			value := []byte{byte(i)}
			if err := tx.Put([]byte("row"), value); err != nil {
				return err
			}
			got, found, err := tx.Get([]byte("row"))
			if err != nil || !found || !bytes.Equal(got, value) {
				return errors.Join(ErrCorrupt, err)
			}
			if _, err := s.files[0].ReadAt(actual[:], int64(s.root.id)*pageBytes); err != nil {
				return err
			}
			if actual != before {
				t.Fatal("same-transaction ancestor overwrites were not combined")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.files[0].ReadAt(actual[:], int64(s.root.id)*pageBytes); err != nil || actual == before || sha256.Sum256(actual[:]) != s.root.hash {
		t.Fatal("successful mutation did not finish real FD writes", err)
	}
	verifyRows(t, s, map[string][]byte{"row": {63}})
	if manager.Snapshot().Used != fees {
		t.Fatal("combining overwrites changed the paid workspace")
	}
}

func TestScratchStagedAncestorOutsideMutationRefusesFlushWithoutOverwrite(t *testing.T) {
	s, manager := testStore(t, 4<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("old")) }); err != nil {
		t.Fatal(err)
	}
	fees := manager.Snapshot().Used
	offset := int64(s.root.id) * pageBytes
	err := s.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]byte("row"), []byte("new")); err != nil {
			return err
		}
		block, _ := s.cachedBlock(0, s.root.id)
		if block.dirty == 0 || block.original == ([32]byte{}) {
			t.Fatal("no original FD evidence retained for pending overwrite")
		}
		_, err := s.files[0].WriteAt([]byte("X"), offset)
		return err
	})
	if !errors.Is(err, ErrCorrupt) || s.fault == nil || manager.Snapshot().Used != fees || s.key == ([32]byte{}) {
		t.Fatal("dirty overwrite hid external bytes or refunded", err)
	}
	var raw [1]byte
	if _, err := s.files[0].ReadAt(raw[:], offset); err != nil || raw[0] != 'X' {
		t.Fatal("flush overwrote unknown outside bytes", raw, err)
	}
	if err := s.View(ctx, func(*Transaction) error { t.Fatal("failed dirty candidate exported a view"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}

func TestScratchStagedAncestorUnknownFDExitKeepsBankKeyAndFees(t *testing.T) {
	s, manager := testStore(t, 4<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("old")) }); err != nil {
		t.Fatal(err)
	}
	fees, bank := manager.Snapshot().Used, s.blockBank
	err := s.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]byte("row"), []byte("new")); err != nil {
			return err
		}
		return s.files[0].Close()
	})
	if err == nil || s.fault == nil || s.blockBank != bank || s.key == ([32]byte{}) || manager.Snapshot().Used != fees {
		t.Fatal("unknown original FD exit lost its owner", err)
	}
	if err := s.Discard(); err == nil || s.blockBank != bank || s.key == ([32]byte{}) || manager.Snapshot().Used != fees {
		t.Fatal("unknown retirement refunded dirty work", err)
	}
}
