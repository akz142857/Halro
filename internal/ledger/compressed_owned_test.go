package ledger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestCompactionPreservesForeignPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(filepath.Dir(path), "ledger-1.seg.gz.partial")
	foreign := []byte("another creation owner")
	if err := os.WriteFile(partial, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	before := log.SealedSummary()
	if _, err := log.Compact(rolled.Sealed.Generation); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err=%v", err)
	}
	got, err := os.ReadFile(partial)
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign=%q err=%v", got, err)
	}
	if log.SealedSummary() != before {
		t.Fatal("failed compression changed summary")
	}
	assertSegmentSummary(t, log)
}

func TestCompactionRejectsLateNativeReplacementWithoutRemovingForeign(t *testing.T) {
	for _, replaceSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "output", true: "source"}[replaceSource], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log := openChained(t, path, nil)
			defer log.Close()
			appendReservations(t, log, 1, 4)
			rolled, err := log.Roll()
			if err != nil {
				t.Fatal(err)
			}
			before := log.SealedSummary()
			var replaced, moved string
			foreign := []byte("foreign inode must survive")
			operations := productionCompactIO()
			operations.compress = func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
				artifact, err := compressSegmentFileOwned(source, destination, checksum, length)
				if err != nil {
					return artifact, err
				}
				replaced = destination
				if replaceSource {
					replaced = source
				}
				moved = replaced + ".moved"
				if err := os.Rename(replaced, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(replaced, foreign, 0600); err != nil {
					t.Fatal(err)
				}
				return artifact, nil
			}
			if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); !errors.Is(err, ownedfile.ErrReplaced) {
				t.Fatalf("err=%v", err)
			}
			got, err := os.ReadFile(replaced)
			if err != nil || !bytes.Equal(got, foreign) {
				t.Fatalf("foreign=%q err=%v", got, err)
			}
			if _, err := os.Stat(moved); err != nil {
				t.Fatal("renamed original inode disappeared:", err)
			}
			if log.SealedSummary() != before {
				t.Fatal("replacement published new accounting summary")
			}
			manifest, err := loadSegmentManifest(filepath.Dir(path))
			if err != nil || len(manifest.Segments) != 1 || manifest.Segments[0].Compressed {
				t.Fatalf("manifest=%+v err=%v", manifest, err)
			}
		})
	}
}

func TestCompactionBorrowsVerifiedOrphanWithoutCleanupAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(path), rolled.Sealed.File)
	destination := filepath.Join(filepath.Dir(path), "ledger-1.seg.gz")
	created, err := compressSegmentFileOwned(source, destination, rolled.Sealed.PlainChecksum, rolled.Sealed.Length)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	operations := productionCompactIO()
	operations.compress = func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
		borrowed, err := compressSegmentFileOwned(source, destination, checksum, length)
		if err == nil && (borrowed.owner != nil || borrowed.temporary != nil) {
			t.Fatal("readability invented deletion ownership")
		}
		log.Status().RequireRecovery()
		return borrowed, err
	}
	if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); err == nil {
		t.Fatal("unhealthy publisher succeeded")
	}
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("borrowed orphan removed or changed: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if err := created.discard(); err != nil {
		t.Fatal("original creation receipt failed:", err)
	}
}

func TestCompressedReaderRejectsInflationBeyondAuthenticatedLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(path), rolled.Sealed.File)
	destination := filepath.Join(filepath.Dir(path), "ledger-1.seg.gz")
	artifact, err := compressSegmentFileOwned(source, destination, rolled.Sealed.PlainChecksum, rolled.Sealed.Length)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.discard()
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := verifyCompressedReader(file, artifact.length, rolled.Sealed.PlainChecksum, rolled.Sealed.Length-1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err=%v", err)
	}
}
