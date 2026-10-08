package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func auditSealBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	var classes [resourcebudget.ClassCount]resourcebudget.Cost
	classes[resourcebudget.IO] = resourcebudget.Cost{RAM: AuditSealScanRAMBytes}
	classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(resourcebudget.Limits{
		Total:          resourcebudget.Cost{RAM: 1 << 20, Disk: 1 << 20},
		Classes:        classes,
		MaxClaims:      16,
		MaxOwners:      4,
		MaxPressureAge: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestAuditSealReferenceVerifiesExactPrefixAndFinalFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for index := range 3 {
		if _, err := log.Append(context.Background(), validEvent(index, "key.create")); err != nil {
			t.Fatal(err)
		}
	}
	expected := log.Summary()
	if _, err := log.Append(context.Background(), validEvent(4, "key.delete")); err != nil {
		t.Fatal(err)
	}
	got, err := log.SealReference(context.Background(), auditSealBudget(t), expected)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != (authenticatedstate.SealRootSchema{Component: authenticatedstate.SealComponentAudit, IndexID: AuthenticatedAuditSealIndexID, Codec: AuthenticatedAuditSealCodec}) || got.Position.Component != authenticatedstate.SealComponentAudit || got.Position.Sequence != expected.Records || got.Position.Offset != uint64(expected.Bytes) || got.Position.Hash != expected.LastHash {
		t.Fatalf("unexpected Audit cut: %+v", got)
	}
	if got.Root.Empty || got.Root.Rows != expected.Records || got.Root.Digest != expected.LastHash || got.Root.FileID == ([16]byte{}) || got.Root.Offset+uint64(got.Root.Length) != uint64(expected.Bytes) {
		t.Fatalf("unexpected Audit root reference: %+v", got.Root)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := contents[got.Root.Offset : got.Root.Offset+uint64(got.Root.Length)]
	if sha256.Sum256(frame) != got.Root.Digest {
		t.Fatal("root digest does not match the referenced final frame")
	}
}

func TestAuditSealReferenceExportsExplicitEmptyPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := Open(path, randomKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	got, err := log.SealReference(context.Background(), auditSealBudget(t), Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Root.Empty || got.Root.Rows != 0 || got.Root.Digest == ([32]byte{}) || got.Root.FileID != ([16]byte{}) || got.Position.Sequence != 0 || got.Position.Offset != 0 || got.Position.Hash != ([32]byte{}) {
		t.Fatalf("empty Audit prefix was not explicit and canonical: %+v", got)
	}
}

func TestAuditSealReferenceRejectsTamperedPrefixAndForeignFile(t *testing.T) {
	t.Run("tampered prefix", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		log, err := Open(path, randomKey(t))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		for index := range 2 {
			if _, err := log.Append(context.Background(), validEvent(index, "key.create")); err != nil {
				t.Fatal(err)
			}
		}
		cut := log.Summary()
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte{0xff}, frameHeaderSize+1); err != nil {
			t.Fatal(errors.Join(err, file.Close()))
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := log.SealReference(context.Background(), auditSealBudget(t), cut); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("tampered historical prefix was accepted: %v", err)
		}
	})
	t.Run("replaced path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		log, err := Open(path, randomKey(t))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		if _, err := log.Append(context.Background(), validEvent(0, "key.create")); err != nil {
			t.Fatal(err)
		}
		cut := log.Summary()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(filepath.Dir(path), "foreign")
		if err := os.WriteFile(foreign, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(foreign, path); err != nil {
			t.Fatal(err)
		}
		if _, err := log.SealReference(context.Background(), auditSealBudget(t), cut); !errors.Is(err, ErrSealSource) {
			t.Fatalf("replacement inode was accepted: %v", err)
		}
	})
}
