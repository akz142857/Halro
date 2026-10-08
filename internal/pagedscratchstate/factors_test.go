package pagedscratchstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func factorStore(t *testing.T, compact, shared bool, ceiling int64, reuse ...bool) *Store {
	t.Helper()
	s, err := Create(context.Background(), filepath.Join(t.TempDir(), "private"), Options{Budget: testManager(t), MaxDiskBytes: ceiling, CompactBlobs: compact, SharedOriginals: shared, ReuseAuthentication: len(reuse) > 0 && reuse[0]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Discard(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func TestFactorsDifferential(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("compact=%v/shared=%v", compact, shared), func(t *testing.T) {
				s := factorStore(t, compact, shared, 64<<20)
				ctx := context.Background()
				want := map[string][]byte{}
				for round := 0; round < 4; round++ {
					var buffer RowBuffer
					err := s.Update(ctx, func(tx *Transaction) error {
						for i := 0; i < 900; i++ {
							k := fmt.Sprintf("k%04d", (i*317)%900)
							v := bytes.Repeat([]byte{byte(i + round)}, (i*73)%4097)
							if round == 2 && i%3 == 0 {
								if err := buffer.Delete(tx, []byte(k)); err != nil {
									return err
								}
								delete(want, k)
							} else {
								if err := buffer.Put(tx, []byte(k), v); err != nil {
									return err
								}
								want[k] = v
							}
						}
						return buffer.Flush(tx)
					})
					if err != nil {
						t.Fatal(round, err)
					}
					verifyRows(t, s, want)
				}
				// Wide values still cross many authenticated compact pages, and reuse frees.
				wide := bytes.Repeat([]byte("w"), MaxValueBytes)
				if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("wide"), wide) }); err != nil {
					t.Fatal(err)
				}
				if err := s.View(ctx, func(tx *Transaction) error {
					v, found, err := tx.Get([]byte("wide"))
					if err != nil {
						return err
					}
					if !found || !bytes.Equal(v, wide) {
						return ErrCorrupt
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := s.Update(ctx, func(tx *Transaction) error { return tx.Delete([]byte("wide")) }); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestSharedOriginalsRejectChangedCleanPresenceAndValue(t *testing.T) {
	for _, mode := range []string{"clean-value", "absent-present", "present-absent", "empty-present"} {
		t.Run(mode, func(t *testing.T) {
			s := factorStore(t, true, true, 1<<20)
			ctx := context.Background()
			key := []byte("key")
			if mode != "absent-present" {
				if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put(key, []byte("old")) }); err != nil {
					t.Fatal(err)
				}
			}
			err := s.Update(ctx, func(tx *Transaction) error {
				var b RowBuffer
				if _, _, err := b.Get(tx, key); err != nil {
					return err
				}
				if mode == "present-absent" {
					if err := tx.Delete(key); err != nil {
						return err
					}
				} else {
					value := []byte("new")
					if mode == "empty-present" {
						value = nil
					}
					if err := tx.Put(key, value); err != nil {
						return err
					}
				}
				return b.Flush(tx)
			})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatal("changed clean intent accepted", err)
			}
			if err := s.View(ctx, func(*Transaction) error { return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
		})
	}
}
func TestCompactGeometryBudgetAndActualPrefix(t *testing.T) {
	ctx := context.Background()
	t.Run("ceiling", func(t *testing.T) {
		s := factorStore(t, true, false, 4096+512+4096+512)
		if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("key"), bytes.Repeat([]byte("v"), 100)) }); err != nil {
			t.Fatal(err)
		}
		for kind, f := range s.files {
			info, err := f.Stat()
			if err != nil || info.Size() != int64(s.next[kind])*int64(s.pageSize(kind)) {
				t.Fatal(kind, info, err)
			}
		}
		if total, ok := s.allocatedBytes(); !ok || total != s.ceiling {
			t.Fatal(total, ok)
		}
		if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("other"), bytes.Repeat([]byte("v"), 100)) }); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	})
	t.Run("cached-neighbor-overwrite", func(t *testing.T) {
		s := factorStore(t, true, true, 1<<20)
		if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("key"), bytes.Repeat([]byte("v"), 900)) }); err != nil {
			t.Fatal(err)
		}
		err := s.View(ctx, func(tx *Transaction) error {
			if _, _, err := tx.Get([]byte("key")); err != nil {
				return err
			}
			// An unchanged adjacent page in the cached 4KiB block is still an actual FD receipt.
			_, err := s.files[1].WriteAt([]byte{1}, 0)
			return err
		})
		if !errors.Is(err, ErrCorrupt) {
			t.Fatal("actual block prefix overwrite accepted", err)
		}
	})
	t.Run("foreign-append", func(t *testing.T) {
		s := factorStore(t, true, true, 1<<20)
		_, err := s.files[1].WriteAt([]byte{1}, 512)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, func(*Transaction) error { return nil }); !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	})
}
