package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	bolt "go.etcd.io/bbolt"
)

func observationFact(name string) File {
	digest := sha256.Sum256([]byte("native fact"))
	return File{Path: name, Size: 11, SHA256: hex.EncodeToString(digest[:])}
}

func TestObservationIndexBindsPathSizeAndDigestAndRetiresOwner(t *testing.T) {
	for _, damage := range []string{"size", "copy another path", "truncate value"} {
		t.Run(damage, func(t *testing.T) {
			directory := t.TempDir()
			index, err := newObservationIndex(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer index.Close()
			first, second := observationFact("data/first"), observationFact("data/second")
			if err := index.Add(first); err != nil {
				t.Fatal(err)
			}
			if err := index.Add(second); err != nil {
				t.Fatal(err)
			}
			actual, found, err := index.Get(first.Path)
			if err != nil || !found || actual != first {
				t.Fatalf("actual=%+v found=%v err=%v", actual, found, err)
			}
			if err := index.Add(first); err == nil || index.count != 2 {
				t.Fatalf("duplicate accepted, count=%d err=%v", index.count, err)
			}
			firstKey, secondKey := sha256.Sum256([]byte(first.Path)), sha256.Sum256([]byte(second.Path))
			if err := index.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket(observationBucket)
				value := bytes.Clone(bucket.Get(firstKey[:]))
				switch damage {
				case "size":
					value[8] ^= 1
				case "copy another path":
					value = bytes.Clone(bucket.Get(secondKey[:]))
				case "truncate value":
					value = value[:10]
				}
				return bucket.Put(firstKey[:], value)
			}); err != nil {
				t.Fatal(err)
			}
			actual, found, err = index.Get(first.Path)
			if !errors.Is(err, errObservationIndex) || found || actual != (File{}) {
				t.Fatalf("tamper actual=%+v found=%v err=%v", actual, found, err)
			}
			filename := index.db.Path()
			if err := index.Close(); err != nil {
				t.Fatal(err)
			}
			if index.key != ([32]byte{}) {
				t.Fatal("key survived owner retirement")
			}
			if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned file not retired: %v", err)
			}
			if _, _, err := index.Get(first.Path); err == nil {
				t.Fatal("closed index served facts")
			}
		})
	}
}

func TestObservationIndexPreservesReplacedPath(t *testing.T) {
	index, err := newObservationIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	filename := index.db.Path()
	if err := index.Add(observationFact("data/fact")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filename, filename+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := index.Get("data/fact"); !errors.Is(err, errObservationIndex) || found {
		t.Fatalf("replacement found=%v err=%v", found, err)
	}
	if err := index.Close(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filename)
	if err != nil || string(raw) != "foreign" {
		t.Fatalf("removed replacement: %q %v", raw, err)
	}
}

func writeObservationArchive(t *testing.T, entries []File, manifest Manifest, afterManifest bool) (string, []byte) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "fixture.hmbk")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{91}, 32)
	encrypted, err := newEncryptWriter(file, key)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(encrypted)
	for _, entry := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: entry.Path, Size: 11, Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte("native fact")); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(payload)), Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(payload); err != nil {
		t.Fatal(err)
	}
	if afterManifest {
		if err := archive.WriteHeader(&tar.Header{Name: "data/late", Size: 0, Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(archive.Close(), encrypted.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	return filename, key
}
func observationManifest(files []File) Manifest {
	return Manifest{FormatVersion: 1, BackupID: "fixture", CreatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Encrypted: true, Metadata: boltstore.MetadataInfo{SchemaVersion: boltstore.CurrentSchemaVersion(), TxID: 1}, MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64), Files: files}
}
func requiredObservationFacts() []File {
	var files []File
	for _, name := range []string{"config/config.yaml", "data/metadata.db", "data/ledger/ledger.wal", "data/audit/audit.log"} {
		files = append(files, observationFact(name))
	}
	return files
}

func TestVerifyDiskObservationsCoverAllFilesAndPreserveManifestOrder(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	entries := requiredObservationFacts()
	for n := 0; n < 1500; n++ {
		entries = append(entries, observationFact(fmt.Sprintf("data/ledger/ledger-%04d.wal", n)))
	}
	// Manifest order is a compatibility output contract, independent of lookups.
	manifestFiles := append([]File(nil), entries...)
	for a, b := 0, len(manifestFiles)-1; a < b; a, b = a+1, b-1 {
		manifestFiles[a], manifestFiles[b] = manifestFiles[b], manifestFiles[a]
	}
	manifest := observationManifest(manifestFiles)
	filename, key := writeObservationArchive(t, entries, manifest, false)
	verified, err := Verify(filename, key)
	if err != nil || len(verified.Files) != len(entries) {
		t.Fatalf("verified files=%d err=%v", len(verified.Files), err)
	}
	for n := range manifestFiles {
		if verified.Files[n] != manifestFiles[n] {
			t.Fatal("manifest order changed", n)
		}
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatalf("index leaked: %v %v", files, err)
	}
}

func TestVerifyDiskObservationsRejectInvalidSetsAndAuthenticatedEnding(t *testing.T) {
	for _, damage := range []string{"duplicate archive", "duplicate manifest", "missing manifest path", "checksum", "extra actual", "after manifest", "bad final record", "bad manifest tail"} {
		t.Run(damage, func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			entries := requiredObservationFacts()
			manifest := observationManifest(append([]File(nil), entries...))
			switch damage {
			case "duplicate archive":
				entries[3] = entries[2]
			case "duplicate manifest":
				manifest.Files[3] = manifest.Files[2]
			case "missing manifest path":
				manifest.Files[3].Path = "data/missing"
			case "checksum":
				manifest.Files[3].SHA256 = strings.Repeat("a", 64)
			case "extra actual":
				entries = append(entries, observationFact("data/extra"))
			case "bad manifest tail":
				manifest.MasterKeyFingerprint = "invalid"
			}
			filename, key := writeObservationArchive(t, entries, manifest, damage == "after manifest")
			if damage == "bad final record" {
				raw, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(filename, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Verify(filename, key)
			if err == nil || result.BackupID != "" || len(result.Files) != 0 {
				t.Fatalf("failure exposed partial manifest=%+v err=%v", result, err)
			}
			files, readErr := os.ReadDir(scratch)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("failed index leaked: %v %v", files, readErr)
			}
		})
	}
}

func TestVerifyObservationScratchFailurePreservesSource(t *testing.T) {
	entries := requiredObservationFacts()
	filename, key := writeObservationArchive(t, entries, observationManifest(entries), false)
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing-directory")
	t.Setenv("TMPDIR", missing)
	result, err := Verify(filename, key)
	if err == nil || result.BackupID != "" || len(result.Files) != 0 {
		t.Fatalf("unavailable scratch returned manifest=%+v err=%v", result, err)
	}
	after, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed verification changed encrypted source: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed verification created scratch directory: %v", err)
	}
}
