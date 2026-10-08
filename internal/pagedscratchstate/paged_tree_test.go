package pagedscratchstate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"unsafe"
)

func TestPagedMultipleLevelsDeleteCollapseAndReinsert(t *testing.T) {
	s, _ := testStore(t, 64<<20)
	ctx := context.Background()
	want := map[string][]byte{}
	order := rand.New(rand.NewSource(741)).Perm(3200)
	for start := 0; start < len(order); start += 80 {
		if err := s.Update(ctx, func(tx *Transaction) error {
			for _, i := range order[start : start+80] {
				key := fmt.Sprintf("key-%05d", i)
				value := bytes.Repeat([]byte{byte(i)}, i%230)
				if err := tx.Put([]byte(key), value); err != nil {
					return err
				}
				want[key] = value
			}
			return nil
		}); err != nil {
			t.Fatal(start, err)
		}
	}
	n, err := s.readNode(s.root)
	if err != nil || n.level < 2 {
		t.Fatal("multiple levels not exercised", n.level, err)
	}
	verifyRows(t, s, want)
	highWater := s.next
	for start := 0; start < len(order); start += 80 {
		if err := s.Update(ctx, func(tx *Transaction) error {
			for _, i := range order[start : start+80] {
				key := fmt.Sprintf("key-%05d", i)
				if err := tx.Delete([]byte(key)); err != nil {
					return err
				}
				delete(want, key)
			}
			return nil
		}); err != nil {
			t.Fatal(start, err)
		}
		if start%400 == 0 {
			verifyRows(t, s, want)
		}
	}
	verifyRows(t, s, want)
	if !s.root.empty() || s.rows != 0 {
		t.Fatal("empty root did not collapse")
	}
	if err := s.Update(ctx, func(tx *Transaction) error {
		for i := 0; i < 1000; i++ {
			if err := tx.Put([]byte(fmt.Sprintf("new-%05d", i)), bytes.Repeat([]byte{byte(i)}, 200)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.next != highWater {
		t.Fatal("deleted page/blob owners not reused", s.next, highWater)
	}
}

func TestPagedAuthenticatedWriterFaultsRefuseFullKeyset(t *testing.T) {
	for _, mode := range []string{"count", "child-count", "missing-child", "level", "ordering", "child-range"} {
		t.Run(mode, func(t *testing.T) {
			s, m := testStore(t, 64<<20)
			ctx := context.Background()
			if err := s.Update(ctx, func(tx *Transaction) error {
				for i := 0; i < 60; i++ {
					k := []byte(fmt.Sprintf("key-%05d", i))
					if err := tx.Put(k, k); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			fees := m.Snapshot().Used
			raw, err := s.readPage(0, s.root)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "count":
				binary.BigEndian.PutUint64(raw[24:32], s.rows-1)
			case "child-count":
				// Preserve total expected root count while falsifying two edge counts.
				binary.BigEndian.PutUint64(raw[72:80], binary.BigEndian.Uint64(raw[72:80])+1)
				binary.BigEndian.PutUint64(raw[120:128], binary.BigEndian.Uint64(raw[120:128])-1)
			case "missing-child":
				clear(raw[32:80])
			case "level":
				raw[16]++
			case "ordering":
				copy(raw[80+entryBytes+140:80+entryBytes+148], raw[80+140:80+148])
			case "child-range":
				root, err := s.readNode(s.root)
				if err != nil {
					t.Fatal(err)
				}
				c := root.children[0]
				page, err := s.readPage(0, c.link)
				if err != nil {
					t.Fatal(err)
				}
				count := int(page[17])
				key := root.entries[0].key
				pos := 80 + (count-1)*entryBytes
				binary.BigEndian.PutUint16(page[pos+136:pos+138], uint16(len(key)))
				clear(page[pos+140 : pos+288])
				copy(page[pos+140:pos+288], key)
				r, err := s.writePage(0, c.link.id, page[:pageBytes-32])
				if err != nil {
					t.Fatal(err)
				}
				putLink(raw[32:72], r)
			}
			// Re-signing both MAC and owner root simulates a faulty writer; the
			// complete structural proof and independent live count still reject it.
			s.root, err = s.writePage(0, s.root.id, raw[:pageBytes-32])
			if err != nil {
				t.Fatal(err)
			}
			err = s.View(ctx, func(tx *Transaction) error {
				it, err := tx.Iterator()
				if err != nil {
					return err
				}
				for {
					_, found, err := it.Next()
					if err != nil || !found {
						return err
					}
				}
			})
			if !errors.Is(err, ErrCorrupt) || m.Snapshot().Used != fees {
				t.Fatal("faulty page granted full keyset", err)
			}
		})
	}
}

func TestPagedBoundaryOracleAndIteratorMutationRefusal(t *testing.T) {
	s, _ := testStore(t, 64<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error {
		for i := 0; i < 240; i += 2 {
			k := []byte(fmt.Sprintf("%03d", i))
			if err := tx.Put(k, k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(tx *Transaction) error {
		for probe := -1; probe <= 240; probe++ {
			key := []byte(fmt.Sprintf("%03d", probe))
			for _, reverse := range []bool{false, true} {
				for _, strict := range []bool{false, true} {
					want := -1
					for k := 0; k < 240; k += 2 {
						cmp := bytes.Compare([]byte(fmt.Sprintf("%03d", k)), key)
						match := cmp > 0 || cmp == 0 && !strict
						if reverse {
							match = cmp < 0 || cmp == 0 && !strict
						}
						if match && (want < 0 || reverse) {
							want = k
						}
					}
					row, found, err := tx.boundary(key, reverse, strict)
					if err != nil || found != (want >= 0) || found && string(row.Key) != fmt.Sprintf("%03d", want) {
						return fmt.Errorf("boundary probe=%d reverse=%v strict=%v got=%s want=%d err=%v", probe, reverse, strict, row.Key, want, err)
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := s.Update(ctx, func(tx *Transaction) error {
		it, err := tx.Iterator()
		if err != nil {
			return err
		}
		if err = tx.Put([]byte("new"), nil); err != nil {
			return err
		}
		_, _, err = it.Next()
		return err
	})
	if !errors.Is(err, ErrPoisoned) {
		t.Fatal("stale iterator accepted after mutation", err)
	}
}

func TestPagedWorkspaceEnvelopeAndBufferOriginalValue(t *testing.T) {
	// Bank plus worst-case decoded keys along the bounded path, two wide
	// values and page/owner workspaces fit the original 64 MiB owner envelope.
	bound := unsafe.Sizeof(nodeAuthenticationReceipts{}) + unsafe.Sizeof(transactionBlockBank{}) + unsafe.Sizeof(Store{}) + unsafe.Sizeof(Iterator{}) + uintptr(maxHeight*maxEntries*MaxKeyBytes+2*MaxValueBytes+2*MaxKeyBytes+8*pageBytes)
	if bound > uintptr(WorkspaceRAMBytes) {
		t.Fatal("workspace exceeds fee", bound)
	}
	s, _ := testStore(t, 64<<20)
	ctx := context.Background()
	b := new(RowBuffer)
	err := s.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]byte("key"), []byte("original")); err != nil {
			return err
		}
		if _, _, err := b.Get(tx, []byte("key")); err != nil {
			return err
		}
		if err := tx.Put([]byte("key"), []byte("changed outside intents")); err != nil {
			return err
		}
		return b.Flush(tx)
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("unchanged read intent lost full original value guard", err)
	}
}
