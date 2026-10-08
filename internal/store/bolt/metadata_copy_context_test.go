package bolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestMetadataCompatibilityCopyRefusesExistingOutputsAndCancelledIO(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "compact"}[compact], func(t *testing.T) {
			copy := func(ctx context.Context, path string) error {
				if compact {
					return s.CompactSnapshotContext(ctx, path)
				}
				_, err := s.SnapshotContext(ctx, path)
				return err
			}
			path := filepath.Join(t.TempDir(), "borrowed.db")
			original := []byte("borrowed output must survive")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := copy(context.Background(), path); err == nil {
				t.Fatal("borrowed destination accepted")
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatal("borrowed output changed", err)
			}
			link := filepath.Join(filepath.Dir(path), "link.db")
			if err := os.Symlink(path, link); err != nil {
				t.Fatal(err)
			}
			if err := copy(context.Background(), link); err == nil {
				t.Fatal("symlink destination accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			path = filepath.Join(t.TempDir(), "absent", "must-not-exist.db")
			if err := copy(ctx, path); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancelled command created parent", err)
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "absent", "closed.db")
	if _, err := s.SnapshotContext(context.Background(), path); err == nil {
		t.Fatal("closed source accepted")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("closed source performed output IO", err)
	}
}

func TestMetadataCopyFailureRetiresOnlyKnownClosedCreation(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	for _, mode := range []string{"known", "unknown-close", "foreign", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "stage.db")
			escaped := path + ".escaped"
			info, owner, err := s.snapshotCreateOwnedContext(ctx, path, func(name string) (*os.File, *ownedfile.Owner, error) {
				f, o, err := ownedfile.Create(name, os.O_WRONLY, 0600)
				if err != nil {
					return f, o, err
				}
				switch mode {
				case "unknown-close":
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
				case "foreign", "missing":
					if err := os.Rename(name, escaped); err != nil {
						t.Fatal(err)
					}
					if mode == "foreign" {
						if err := os.WriteFile(name, []byte("foreign"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				cancel()
				return f, o, nil
			})
			if err == nil || owner == nil || info != (MetadataInfo{}) {
				t.Fatal("failed copy returned successful facts or lost receipt", err)
			}
			err = retireFailedMetadataCopy(path, owner, err)
			if mode == "known" {
				if errors.Is(err, ErrMetadataFileRetirement) {
					t.Fatal("known retirement reported debt", err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("known closed owned output retained", err)
				}
				return
			}
			if !errors.Is(err, ErrMetadataFileRetirement) {
				t.Fatal("unknown retirement erased", err)
			}
			if mode == "unknown-close" {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("unknown physical output removed", err)
				}
			} else if _, err := os.Stat(escaped); err != nil {
				t.Fatal("escaped original was retired by inference", err)
			}
			if mode == "foreign" {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "foreign" {
					t.Fatal("foreign output removed", err)
				}
			}
		})
	}
}
