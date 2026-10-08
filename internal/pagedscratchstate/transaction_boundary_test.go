package pagedscratchstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestScratchTransactionExitJoinsActualFilesAndNamespace(t *testing.T) {
	for _, mode := range []string{"namespace", "parent", "nodes-inode", "blobs-inode", "nodes-truncate", "blobs-append", "nodes-close", "directory-close"} {
		t.Run(mode, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			manager := testManager(t)
			store, err := Create(context.Background(), filepath.Join(parent, "fresh"), Options{Budget: manager, MaxDiskBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]byte("first"), []byte("real authenticated row")) }); err != nil {
				t.Fatal(err)
			}
			before := manager.Snapshot().Used
			foreignPath := ""
			err = store.View(context.Background(), func(tx *Transaction) error {
				value, found, err := tx.Get([]byte("first"))
				if err != nil || !found || string(value) != "real authenticated row" {
					t.Fatal(value, found, err)
				}
				switch mode {
				case "namespace":
					if err := os.Rename(store.dir, store.dir+".owned"); err != nil {
						return err
					}
					if err := os.Mkdir(store.dir, 0700); err != nil {
						return err
					}
					foreignPath = filepath.Join(store.dir, "foreign")
				case "parent":
					if err := os.Rename(parent, parent+".owned"); err != nil {
						return err
					}
					if err := os.Mkdir(parent, 0700); err != nil {
						return err
					}
					foreignPath = filepath.Join(parent, "foreign")
				case "nodes-inode", "blobs-inode":
					kind := 0
					if mode == "blobs-inode" {
						kind = 1
					}
					foreignPath = filepath.Join(store.dir, []string{"nodes", "blobs"}[kind])
					if err := os.Rename(foreignPath, foreignPath+".owned"); err != nil {
						return err
					}
				case "nodes-truncate":
					return store.files[0].Truncate(pageBytes)
				case "blobs-append":
					return store.files[1].Truncate(int64(store.next[1]+1) * pageBytes)
				case "nodes-close":
					return store.files[0].Close()
				case "directory-close":
					return store.directory.Close()
				}
				return os.WriteFile(foreignPath, []byte("foreign must survive"), 0600)
			})
			if err == nil || store.fault == nil {
				t.Fatal("callback filesystem mutation returned success", mode)
			}
			if manager.Snapshot().Used != before {
				t.Fatal("failed transaction refunded owner", before, manager.Snapshot())
			}
			if err := store.Update(context.Background(), func(*Transaction) error { t.Fatal("poisoned owner admitted callback"); return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
			retirement := store.Discard()
			if foreignPath != "" {
				if retirement == nil {
					t.Fatal("replaced namespace was retired")
				}
				raw, err := os.ReadFile(foreignPath)
				if err != nil || string(raw) != "foreign must survive" {
					t.Fatal("foreign data changed", string(raw), err)
				}
				if manager.Snapshot().Used != before {
					t.Fatal("foreign namespace refunded owner")
				}
			} else if mode == "nodes-close" || mode == "directory-close" {
				var failure *Failure
				if !errors.Is(retirement, ownedfile.ErrRetirement) || !errors.As(retirement, &failure) || failure.Owner != store || store.key == ([32]byte{}) || manager.Snapshot().Used != before {
					t.Fatal("unknown actual Close lost owner", retirement)
				}
			} else {
				// The malformed bytes still belong to the captured creator. Only this
				// subsequent actual Close/unlink/fsync receipt refunds the failed scratch.
				if retirement != nil || manager.Snapshot().Used.Disk != 0 || manager.Snapshot().Used.RAM != 0 {
					t.Fatal("actual owned malformed scratch retirement", retirement, manager.Snapshot())
				}
			}
		})
	}
}

func TestScratchTransactionExitRejectsMutationAfterLastWrite(t *testing.T) {
	store, manager := testStore(t, 4<<20)
	before := manager.Snapshot().Used
	err := store.Update(context.Background(), func(tx *Transaction) error {
		if err := tx.Put([]byte("new"), []byte("fact")); err != nil {
			return err
		}
		return store.files[0].Truncate(int64(store.next[0]+1) * pageBytes)
	})
	if !errors.Is(err, ErrCorrupt) || manager.Snapshot().Used != before {
		t.Fatal("postwrite extension admitted/refunded", err)
	}
	if err := store.View(context.Background(), func(*Transaction) error { t.Fatal("failed candidate exported"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}
