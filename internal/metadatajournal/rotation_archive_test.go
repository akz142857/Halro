package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestRotationNativeArchiveUsesActualReadOnlyDescriptorAndLiveExclusion(t *testing.T) {
	for _, mode := range []string{"archive", "lost-guard", "replaced", "foreign-destination", "partial"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path, original, _ := newBoundJournal(t)
			if _, err := original.Append([]Op{put("projects", "latest", "preserve native fact")}); err != nil {
				t.Fatal(err)
			}
			head := original.Head()
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "partial" {
				partial := append(bytes.Clone(raw), []byte("HM")...)
				if err := os.WriteFile(path, partial, 0600); err != nil {
					t.Fatal(err)
				}
				if reader, err := OpenRotationSourceReadOnly(ctx, path, testKey(), head, 64<<20); reader != nil || err == nil {
					t.Fatal(reader, err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(partial, after) {
					t.Fatal("read-only qualification repaired a tail", err)
				}
				return
			}
			reader, err := OpenRotationSourceReadOnly(ctx, path, testKey(), head, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if _, err := reader.Append([]Op{put("projects", "forbidden", "mutation")}); err == nil {
				t.Fatal("historical owner appended")
			}
			guard, err := lock.Acquire(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			destination := path + ".history"
			archive, err := reader.PrepareNativeArchive(head, destination, guard)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "lost-guard":
				if err := guard.Close(); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-destination":
				if err := os.WriteFile(destination, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = archive.Archive()
			if mode != "archive" {
				if !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal("foreign namespace or exclusion loss authorized move", err)
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("source disappeared on rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := archive.Archive(); err != nil {
				t.Fatal("known move not idempotent", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source remained", err)
			}
			after, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("native facts changed during archive", err)
			}
		})
	}
}
