package dirscan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryScanFixedChunksCancellationAndIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "entries")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3*BatchEntries+1; n++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(n)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	if err := Walk(context.Background(), root, func(os.DirEntry) error { count++; return nil }); err != nil || count != 3*BatchEntries+1 {
		t.Fatal(count, err)
	}
	if err := Empty(context.Background(), root); !errors.Is(err, ErrNotEmpty) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count = 0
	if err := Walk(ctx, root, func(os.DirEntry) error { count++; cancel(); return nil }); !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatal("cancelled callback continued enumeration", count, err)
	}
	if err := Walk(context.Background(), root, func(os.DirEntry) error { return Stop }); err != nil {
		t.Fatal(err)
	}
	err := Walk(context.Background(), root, func(os.DirEntry) error {
		if err := os.Rename(root, root+".moved"); err != nil {
			return err
		}
		return Stop
	})
	if !errors.Is(err, ErrIdentity) || Missing(err) {
		t.Fatal("renamed directory was mistaken for absent input", err)
	}
	if err := os.Symlink(root+".moved", root); err != nil {
		t.Fatal(err)
	}
	if err := Empty(context.Background(), root); !errors.Is(err, ErrIdentity) {
		t.Fatal("symlink directory accepted", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+".moved", root); err != nil {
		t.Fatal(err)
	}
	if err := Empty(context.Background(), filepath.Join(root, "absent")); !Missing(err) {
		t.Fatal(err)
	}
}
