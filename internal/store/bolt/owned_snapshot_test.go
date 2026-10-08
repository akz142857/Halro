package bolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
)

func TestMetadataOwnedSnapshotPreservesForeignAndUnknownClose(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	path := filepath.Join(t.TempDir(), "stage.db")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if info, owner, err := s.SnapshotCreateOwnedContext(context.Background(), path); err == nil || owner != nil || info != (MetadataInfo{}) {
		t.Fatal("foreign destination overwritten", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "foreign" {
		t.Fatal("foreign output changed", err)
	}
	path = filepath.Join(t.TempDir(), "unknown.db")
	info, owner, err := s.snapshotCreateOwnedContext(context.Background(), path, func(name string) (*os.File, *ownedfile.Owner, error) {
		f, o, err := ownedfile.Create(name, os.O_WRONLY, 0600)
		if err != nil {
			return nil, o, err
		}
		if err := f.Close(); err != nil {
			return nil, o, err
		}
		return f, o, nil
	})
	if !errors.Is(err, ErrMetadataFileRetirement) || !errors.Is(err, ownedfile.ErrRetirement) || owner == nil || info != (MetadataInfo{}) {
		t.Fatal("uncertain actual Close returned success or lost creation receipt", err)
	}
	if cleanupErr := retireFailedMetadataCopy(path, owner, err); !errors.Is(cleanupErr, ownedfile.ErrRetirement) {
		t.Fatal("uncertain snapshot cleanup lost actual descriptor debt", cleanupErr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown output removed", err)
	}
	// Caller can retain the creation receipt, but the failed result does not
	// grant it permission to retire an uncertain physical writer.
}

func TestMetadataOwnedSnapshotKnownCloseReturnsCompleteReadableFile(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	path := filepath.Join(t.TempDir(), "complete.db")
	var actual *os.File
	info, owner, err := s.snapshotCreateOwnedContext(context.Background(), path, func(name string) (*os.File, *ownedfile.Owner, error) {
		file, owner, err := ownedfile.Create(name, os.O_WRONLY, 0600)
		actual = file
		return file, owner, err
	})
	if err != nil || info.SchemaVersion != CurrentSchemaVersion() || owner == nil || actual == nil {
		t.Fatal(info, err)
	}
	if _, err := actual.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("actual snapshot FD did not retire", err)
	}
	copy, err := Open(path)
	if err != nil {
		t.Fatal("known-closed snapshot cannot reopen", err)
	}
	if copied, err := copy.Info(); err != nil || copied.SchemaVersion != info.SchemaVersion {
		t.Fatal(copied, err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
}
func TestMetadataOwnedCopyCancellationAndCeilingsPrecedeOutput(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "must-not-exist.db")
	if _, _, err := s.SnapshotCreateOwnedContext(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.CompactCreateOwnedContext(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled copy performed output IO", err)
	}
	s.maxFileBytes = 1
	if _, _, err := s.SnapshotCreateOwnedContext(context.Background(), path); !errors.Is(err, ErrMetadataStoreDiskLimit) {
		t.Fatal("source ceiling ignored", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source ceiling refused after creation", err)
	}
}
func TestMetadataOwnedCompactionPreservesCompleteBucketsAndSequences(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 130)
	defer s.Close()
	if err := s.migrate(func(tx *Tx) error {
		root, err := tx.tx.CreateBucket([]byte("owned-copy-test"))
		if err != nil {
			return err
		}
		if err := root.SetSequence(29); err != nil {
			return err
		}
		child, err := root.CreateBucket([]byte{0, 255})
		if err != nil {
			return err
		}
		if err := child.SetSequence(41); err != nil {
			return err
		}
		if err := child.Put([]byte("binary\x00key"), []byte{0, 7, 255}); err != nil {
			return err
		}
		_, err = root.CreateBucket([]byte("empty"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "compact.db")
	owner, err := s.CompactCreateOwnedContext(context.Background(), destination)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(destination, 0600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bbolt.Tx) error {
		root := tx.Bucket([]byte("owned-copy-test"))
		if root == nil || root.Sequence() != 29 {
			return errors.New("root sequence lost")
		}
		child := root.Bucket([]byte{0, 255})
		if child == nil || child.Sequence() != 41 || !bytes.Equal(child.Get([]byte("binary\x00key")), []byte{0, 7, 255}) || root.Bucket([]byte("empty")) == nil {
			return errors.New("complete binary/empty bucket lost")
		}
		return nil
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
}
func TestMetadataOwnedCompactionRefusesOverdeepAndForeignOutput(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "source.db"), 1)
	defer s.Close()
	if err := s.migrate(func(tx *Tx) error {
		root, err := tx.tx.CreateBucket([]byte("depth"))
		if err != nil {
			return err
		}
		for i := 0; i < 32; i++ {
			root, err = root.CreateBucket([]byte("child"))
			if err != nil {
				return err
			}
		}
		return root.Put([]byte("value"), []byte("one"))
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compact.db")
	owner, err := s.CompactCreateOwnedContext(context.Background(), path)
	if !errors.Is(err, ErrPricingRecoveryLimit) || owner == nil {
		t.Fatal("overdeep copy succeeded or lost receipt", err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if owner, err := s.CompactCreateOwnedContext(context.Background(), path); err == nil || owner != nil {
		t.Fatal("borrowed output overwritten", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "foreign" {
		t.Fatal("foreign output removed", err)
	}
}
