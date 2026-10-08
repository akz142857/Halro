package scratchstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"unsafe"
)

func TestScratchBlockBankHasFixedPaidBoundAndNoCrossTransactionReads(t *testing.T) {
	// Exactly 32MiB payload plus fixed metadata and one 4KiB verification
	// buffer, paid inside the unchanged 64MiB scratch workspace.
	if size := unsafe.Sizeof(transactionBlockBank{}); size < 32<<20 || size > (33<<20) || int64(size)+int64(unsafe.Sizeof(nodeAuthenticationReceipts{}))+MaxValueBytes*4+MaxKeyBytes*maxHeight >= WorkspaceRAMBytes {
		t.Fatal("fixed scratch workspace changed", size)
	}
	s, m := testStore(t, 4<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("value")) }); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	if err := s.View(ctx, func(tx *Transaction) error {
		if _, _, err := tx.Get([]byte("row")); err != nil {
			return err
		}
		wrong := s.root
		wrong.hash[0] ^= 1
		if _, err := s.readPage(0, wrong); !errors.Is(err, ErrCorrupt) {
			t.Fatal("cache hit bypassed caller's expected page hash", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.blockBank.active || m.Snapshot().Used != before {
		t.Fatal("bank escaped the transaction or added a fee")
	}
	// Outside mutation after a completed transaction must be read afresh.
	if _, err := s.files[0].WriteAt([]byte{0}, int64(s.root.id)*pageBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(tx *Transaction) error { _, _, err := tx.Get([]byte("row")); return err }); !errors.Is(err, ErrCorrupt) {
		t.Fatal("old transaction masked actual FD mutation", err)
	}
}

func TestScratchBlockBankExitAndWriteThroughRejectOutsideMutation(t *testing.T) {
	for _, mode := range []string{"node-exit", "blob-exit", "before-overwrite"} {
		t.Run(mode, func(t *testing.T) {
			s, m := testStore(t, 4<<20)
			ctx := context.Background()
			value := bytes.Repeat([]byte("a"), 512)
			if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("row"), value) }); err != nil {
				t.Fatal(err)
			}
			before := m.Snapshot().Used
			kind, offset := 0, int64(s.root.id)*pageBytes
			if mode == "blob-exit" {
				kind, offset = 1, pageBytes
			}
			var original [1]byte
			if _, err := s.files[kind].ReadAt(original[:], offset); err != nil {
				t.Fatal(err)
			}
			foreign := []byte{original[0] ^ 1}
			err := s.Update(ctx, func(tx *Transaction) error {
				if _, _, err := tx.Get([]byte("row")); err != nil {
					return err
				}
				if _, err := s.files[kind].WriteAt(foreign, offset); err != nil {
					return err
				}
				if mode == "before-overwrite" {
					return tx.Put([]byte("row"), []byte("replacement"))
				}
				// A cached hit still checks its MAC/link. The actual-FD compare
				// must reject the entire transaction before it returns success.
				_, _, err := tx.Get([]byte("row"))
				return err
			})
			if !errors.Is(err, ErrCorrupt) || s.fault == nil || m.Snapshot().Used != before {
				t.Fatal("outside modification was hidden or refunded", err, m.Snapshot())
			}
			var after [1]byte
			if _, err := s.files[kind].ReadAt(after[:], offset); err != nil || after[0] != foreign[0] {
				t.Fatal("cached overwrite destroyed unknown modified bytes", after, err)
			}
			if err := s.View(ctx, func(*Transaction) error { t.Fatal("poisoned owner exported a view"); return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
		})
	}
}

func TestScratchBlockBankEvictionJoinsOriginalFD(t *testing.T) {
	s, m := testStore(t, 32<<20)
	// Five actual MAC pages collide in one finite four-way set. They need
	// not be reachable tree nodes to test eviction of an observed raw page.
	stride := uint64((blockBankSlots / blockBankWays / 2) * (blockBankBytes / pageBytes))
	s.next[0] = stride*blockBankWays + 2
	var refs [blockBankWays + 1]link
	for i := range refs {
		var err error
		refs[i], err = s.writePage(0, uint64(i)*stride+1, []byte("SCT1"))
		if err != nil {
			t.Fatal(err)
		}
	}
	before := m.Snapshot().Used
	err := s.View(context.Background(), func(*Transaction) error {
		for _, ref := range refs[:blockBankWays] {
			if _, err := s.readPage(0, ref); err != nil {
				return err
			}
		}
		if _, err := s.files[0].WriteAt([]byte("X"), pageBytes); err != nil {
			return err
		}
		_, err := s.readPage(0, refs[blockBankWays])
		return err
	})
	if !errors.Is(err, ErrCorrupt) || s.fault == nil || m.Snapshot().Used != before {
		t.Fatal("evicted observation was dropped without actual join", err)
	}
}

func TestScratchBlockBankRetiredOwnerReleasesBankOnlyAfterActualClose(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			s, manager := testStore(t, 4<<20)
			if s.blockBank == nil || unsafe.Sizeof(*s) >= 1<<20 {
				t.Fatal("retained Store embeds the large bank")
			}
			if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("value")) }); err != nil {
				t.Fatal(err)
			}
			if err := s.View(context.Background(), func(tx *Transaction) error { _, _, err := tx.Get([]byte("row")); return err }); err != nil {
				t.Fatal(err)
			}
			bank, fees := s.blockBank, manager.Snapshot().Used
			if unknown {
				if err := s.files[0].Close(); err != nil {
					t.Fatal(err)
				}
				if err := s.Discard(); err == nil || s.blockBank != bank || manager.Snapshot().Used != fees || s.key == ([32]byte{}) {
					t.Fatal("unknown actual Close lost its bank/key/fees", err)
				}
				return
			}
			if err := s.Discard(); err != nil || s.blockBank != nil {
				t.Fatal("retained closed owner kept the refunded 2MiB bank", err)
			}
			if err := s.Close(); err != nil || s.blockBank != nil {
				t.Fatal("repeat Close recreated a bank", err)
			}
		})
	}
}
