package pagedscratchstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestNodeAuthenticationReuseDifferential(t *testing.T) {
	s := factorStore(t, true, true, 64<<20, true)
	ctx := context.Background()
	want := map[string][]byte{}
	for round := 0; round < 4; round++ {
		var b RowBuffer
		if err := s.Update(ctx, func(tx *Transaction) error {
			for i := 0; i < 1100; i++ {
				k := fmt.Sprintf("node-%05d", (i*317)%1100)
				v := bytes.Repeat([]byte{byte(round + i)}, i%451)
				if round == 2 && i%2 == 0 {
					if err := b.Delete(tx, []byte(k)); err != nil {
						return err
					}
					delete(want, k)
				} else {
					if err := b.Put(tx, []byte(k), v); err != nil {
						return err
					}
					want[k] = v
				}
			}
			return b.Flush(tx)
		}); err != nil {
			t.Fatal(round, err)
		}
		verifyRows(t, s, want)
	}
	for i := range s.nodeReceipts {
		if s.nodeReceipts[i].valid {
			t.Fatal("receipt escaped callback")
		}
	}
}
func TestNodeAuthenticationFaults(t *testing.T) {
	for _, mode := range []string{"expected-hash", "bank-byte", "snapshot-byte", "actual-FD", "stale-after-write"} {
		t.Run(mode, func(t *testing.T) {
			s := factorStore(t, true, true, 1<<20, true)
			ctx := context.Background()
			if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("key"), []byte("old")) }); err != nil {
				t.Fatal(err)
			}
			err := s.Update(ctx, func(tx *Transaction) error {
				if _, _, err := tx.Get([]byte("key")); err != nil {
					return err
				}
				old := s.root
				switch mode {
				case "expected-hash":
					old.hash[0] ^= 1
					_, err := s.readPage(0, old)
					return err
				case "bank-byte":
					block, _ := s.cachedBlock(0, old.id)
					block.raw[200] ^= 1
				case "snapshot-byte":
					s.nodeReceipts[old.id%nodeAuthenticationSlots].raw[200] ^= 1
				case "actual-FD":
					if _, err := s.files[0].WriteAt([]byte{1}, int64(old.id)*pageBytes+200); err != nil {
						return err
					}
				case "stale-after-write":
					if err := tx.Put([]byte("key"), []byte("new")); err != nil {
						return err
					}
					_, err := s.readPage(0, old)
					return err
				}
				_, _, err := tx.Get([]byte("key"))
				return err
			})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatal("authenticated result bypassed fault", err)
			}
			if err := s.View(ctx, func(*Transaction) error { return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
		})
	}
}
func TestNodeAuthenticationSameIDAndTransactionReset(t *testing.T) {
	s := factorStore(t, true, true, 1<<20, true)
	ctx := context.Background()
	var first uint64
	if err := s.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]byte("key"), []byte("old")); err != nil {
			return err
		}
		first = s.root.id
		if _, _, err := tx.Get([]byte("key")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("key")); err != nil {
			return err
		}
		if s.nodeReceipts[first%nodeAuthenticationSlots].valid {
			return ErrCorrupt
		}
		if err := tx.Put([]byte("new"), []byte("replacement")); err != nil {
			return err
		}
		if s.root.id != first {
			return ErrCorrupt
		}
		v, found, err := tx.Get([]byte("new"))
		if err != nil {
			return err
		}
		if !found || string(v) != "replacement" {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(tx *Transaction) error {
		if s.nodeReceipts[first%nodeAuthenticationSlots].valid {
			return ErrCorrupt
		}
		_, _, err := tx.Get([]byte("new"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestNodeAuthenticationEvictionRequiresFreshMAC(t *testing.T) {
	s := factorStore(t, false, false, 64<<20, true)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error {
		// Five nodes in one four-way bank set force an actual eviction.
		links := make([]link, 5)
		for i := uint64(1); i <= 4097; i++ {
			id, err := s.allocate(0)
			if err != nil {
				return err
			}
			var raw [16]byte
			copy(raw[:], "SPT1")
			r, err := s.writePage(0, id, raw[:])
			if err != nil {
				return err
			}
			if (i-1)%1024 == 0 {
				links[(i-1)/1024] = r
			}
		}
		for _, r := range links {
			if _, err := s.readPage(0, r); err != nil {
				return err
			}
		}
		if s.nodeReceipts[1%nodeAuthenticationSlots].link.id == 1 && s.nodeReceipts[1%nodeAuthenticationSlots].valid {
			return ErrCorrupt
		}
		// Current bytes are obtained and independently MAC checked after a miss.
		raw, err := s.readPage(0, links[0])
		if err != nil {
			return err
		}
		raw[20] ^= 1
		if _, err := s.files[0].WriteAt(raw[:], int64(links[0].id)*pageBytes); err != nil {
			return err
		}
		return nil
	}); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}
