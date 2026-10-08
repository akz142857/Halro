package backup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractDiskFactsPreserveCompleteFilesAndRetireIndexes(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	entries := requiredObservationFacts()
	for n := 0; n < 100; n++ {
		entries = append(entries, observationFact(fmt.Sprintf("data/ledger/ledger-%04d.wal", n)))
	}
	original := observationManifest(entries)
	filename, key := writeObservationArchive(t, entries, original, false)
	destination := filepath.Join(t.TempDir(), "extracted")
	manifest, err := Extract(filename, key, destination)
	if err != nil || len(manifest.Files) != len(entries) || manifest.BackupID != original.BackupID {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(entry.Path)))
		if err != nil || string(raw) != "native fact" {
			t.Fatalf("entry=%s bytes=%q err=%v", entry.Path, raw, err)
		}
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatalf("indexes leaked: %v %v", files, err)
	}
}

func TestExtractRejectsSourceReplacementAndSameInodeRewrite(t *testing.T) {
	for _, damage := range []string{"replace path", "rewrite same inode", "damage final record"} {
		t.Run(damage, func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			entries := requiredObservationFacts()
			first := observationManifest(entries)
			first.BackupID = "original"
			second := first
			second.BackupID = "modified"
			filename, key := writeObservationArchive(t, entries, first, false)
			other, _ := writeObservationArchive(t, entries, second, false)
			identity, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := os.ReadFile(other)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "extracted")
			result, err := extractArchive(filename, key, destination, func() error {
				if damage == "replace path" {
					return os.Rename(other, filename)
				}
				if damage == "damage final record" {
					replacement, err = os.ReadFile(filename)
					if err != nil {
						return err
					}
					replacement[len(replacement)-1] ^= 1
				}
				if int64(len(replacement)) != identity.Size() {
					t.Fatal("fixture rewrite must preserve size")
				}
				if err := os.WriteFile(filename, replacement, 0600); err != nil {
					return err
				}
				return os.Chtimes(filename, identity.ModTime(), identity.ModTime())
			})
			if err == nil || result.BackupID != "" || len(result.Files) != 0 {
				t.Fatalf("changed source returned manifest=%+v err=%v", result, err)
			}
			if damage == "rewrite same inode" && !strings.Contains(err.Error(), "between verification and extraction") {
				t.Fatalf("did not reach ciphertext comparison: %v", err)
			}
			if damage == "replace path" {
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("replacement wrote staging: %v", err)
				}
			}
			files, readErr := os.ReadDir(scratch)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("failed indexes leaked: %v %v", files, readErr)
			}
		})
	}
}

func TestExtractKeepsPreexistingDestinationOnFailure(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	entries := requiredObservationFacts()
	filename, key := writeObservationArchive(t, entries, observationManifest(entries), false)
	destination := t.TempDir()
	existing := filepath.Join(destination, "unrelated")
	if err := os.WriteFile(existing, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(filename, key, destination); err == nil {
		t.Fatal("nonempty destination accepted")
	}
	raw, err := os.ReadFile(existing)
	if err != nil || !bytes.Equal(raw, []byte("preserved")) {
		t.Fatal("existing file changed", err)
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatal("index leaked", files, err)
	}
}
