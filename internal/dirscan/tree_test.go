package dirscan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTreePostorderCancellationDepthAndActualFDIdentity(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"a", "a/deeper", "b"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 97; i++ {
		if err := os.WriteFile(filepath.Join(root, "a", fmt.Sprintf("file-%03d", i)), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, directories := 0, 0
	if err := Tree(context.Background(), root, 4, func(relative string, file *os.File, info os.FileInfo) error {
		actual, err := file.Stat()
		if err != nil || !os.SameFile(actual, info) {
			t.Fatal(relative, err)
		}
		if info.IsDir() {
			directories++
			if relative == "a" && files != 97 {
				t.Fatal("directory consumed before children", files)
			}
		} else {
			files++
		}
		return nil
	}); err != nil || files != 97 || directories != 4 {
		t.Fatal(files, directories, err)
	}
	if err := Tree(context.Background(), root, 1, func(string, *os.File, os.FileInfo) error { return nil }); !errors.Is(err, ErrDepth) {
		t.Fatal("depth was truncated", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Tree(ctx, root, 4, func(string, *os.File, os.FileInfo) error { calls++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(calls, err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Tree(context.Background(), root, 4, func(string, *os.File, os.FileInfo) error { return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal("symlink accepted", err)
	}
}
