package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestExtractionActualDescriptorCloseAndParentReplacementDenyCompletion(t *testing.T) {
	for _, mode := range []string{"actual-close", "parent-replaced"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "payload")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "native.wal")
			actual, err := writeExtractedPayload(context.Background(), path, "data/native.wal", strings.NewReader("native fact"), func(file *os.File) error {
				if mode == "actual-close" {
					return file.Close()
				}
				if err := os.Rename(parent, parent+".original"); err != nil {
					return err
				}
				if err := os.Mkdir(parent, 0700); err != nil {
					return err
				}
				return os.WriteFile(path, []byte("foreign"), 0600)
			})
			if !errors.Is(err, ownedfile.ErrRetirement) || actual != (File{}) {
				t.Fatal(actual, err)
			}
			if mode == "parent-replaced" {
				foreign, err := os.ReadFile(path)
				if err != nil || string(foreign) != "foreign" {
					t.Fatal("foreign inode changed", string(foreign), err)
				}
				original, err := os.ReadFile(filepath.Join(parent+".original", "native.wal"))
				if err != nil || string(original) != "native fact" {
					t.Fatal("actual inode lost", string(original), err)
				}
			}
		})
	}
}

func TestExtractionCancellationAndExclusiveOutputPreserveForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeExtractedPayload(context.Background(), path, "data/payload", strings.NewReader("new"), nil); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "foreign" {
		t.Fatal(string(raw), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if actual, err := writeExtractedPayload(ctx, path+".cancelled", "data/cancelled", strings.NewReader("new"), nil); !errors.Is(err, context.Canceled) || errors.Is(err, ownedfile.ErrRetirement) || actual != (File{}) {
		t.Fatal(actual, err)
	}
}
