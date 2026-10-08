package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func TestMaterializedImageTransfersExactFDAndKeepsCreditUntilRemoval(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd"), []byte("ef")}, nil)
	credit, err := s.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	before := s.budget.Snapshot().Used
	image, err := s.MaterializeImage(context.Background(), path, schema, root, 4, 64, credit)
	if err != nil {
		t.Fatal(err)
	}
	original := image.file
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	if got := s.budget.Snapshot().Used.Disk; got != before.Disk {
		t.Fatalf("live image lost credit: %d != %d", got, before.Disk)
	}
	file, err := image.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil || file != original {
		t.Fatalf("descriptor changed: %v", err)
	}
	if _, err := image.OpenFile(path, os.O_RDWR, 0o600); err == nil {
		t.Fatal("descriptor transferred twice")
	}
	var b [6]byte
	if _, err := file.ReadAt(b[:], 0); err != nil || string(b[:]) != "abcdef" {
		t.Fatalf("restored bytes = %q: %v", b, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Refusing removal must preserve both the unrelated file and the credit.
	oldPath := path + ".owned"
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := image.Remove(); !errors.Is(err, ownedfile.ErrReplaced) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("renamed image was mistaken for retired inode", err)
	}
	if got := s.budget.Snapshot().Used.Disk; got != before.Disk {
		t.Fatal("missing image name refunded live inode", got)
	}
	if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := image.Remove(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if got := s.budget.Snapshot().Used.Disk; got != before.Disk {
		t.Fatal("failed removal refunded physical owner", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, path); err != nil {
		t.Fatal(err)
	}
	if err := image.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := image.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := s.budget.Snapshot().Used.Disk; got != before.Disk-64 {
		t.Fatal("removed owner kept credit", got)
	}
}

func TestMaterializedImageFailedActualEngineHandoffRetainsInodeAndCredit(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd"), []byte("ef")}, nil)
	credit, err := s.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "invalid-engine-image")
	image, err := s.MaterializeImage(context.Background(), path, schema, root, 4, 64, credit)
	if err != nil {
		t.Fatal(err)
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	before := s.budget.Snapshot().Used
	db, err := bbolt.Open(path, 0600, &bbolt.Options{OpenFile: image.OpenFile, MaxSize: 64})
	if err == nil || db != nil {
		t.Fatal("invalid actual image opened", db, err)
	}
	if err := image.EngineOpenFailed(err); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("opaque receiving-engine failure lost retirement debt", err)
	}
	for n := 0; n < 2; n++ {
		if err := image.Remove(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("opaque engine debt erased", err)
		}
		if s.budget.Snapshot().Used != before {
			t.Fatal("unknown mapping returned file credit", before, s.budget.Snapshot())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown engine inode removed", err)
	}
}

func TestMaterializedImageRejectsAdmissionAndCleansPartialOutput(t *testing.T) {
	for _, kind := range []string{"no-credit", "existing", "canceled", "wrong-root", "malformed-chunk"} {
		t.Run(kind, func(t *testing.T) {
			values := [][]byte{[]byte("abcd"), []byte("ef")}
			if kind == "malformed-chunk" {
				values[0] = []byte("abc")
			}
			s, schema, root := imageStore(t, values, nil)
			credit, err := s.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64})
			if err != nil {
				t.Fatal(err)
			}
			defer credit.Release()
			path := filepath.Join(t.TempDir(), "restored.db")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "existing":
				if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "wrong-root":
				root.Digest[0] ^= 1
			}
			given := credit
			if kind == "no-credit" {
				given = resourcebudget.Permit{}
			}
			before := s.budget.Snapshot().Used
			image, err := s.MaterializeImage(ctx, path, schema, root, 4, 64, given)
			if err == nil || image != nil {
				t.Fatalf("invalid image restored: %v", err)
			}
			if got := s.budget.Snapshot().Used; got != before {
				t.Fatalf("failed restore leaked credit: %+v -> %+v", before, got)
			}
			b, err := os.ReadFile(path)
			if kind == "existing" {
				if err != nil || string(b) != "unrelated" {
					t.Fatalf("existing destination altered: %q %v", b, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial output survived: %v", err)
			}
		})
	}
}
