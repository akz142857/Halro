package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	bolt "go.etcd.io/bbolt"
)

func TestSourceCatalogSortedFactsAndSharedReferences(t *testing.T) {
	c, err := NewSourceCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for n := 999; n >= 0; n-- {
		file := SourceFile{ArchivePath: fmt.Sprintf("data/%04d", n), LocalPath: fmt.Sprintf("/source/%04d", n)}
		if err := c.Add(file); err != nil {
			t.Fatal(err)
		}
		if err := c.AddShared(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Add(SourceFile{ArchivePath: "data/0000", LocalPath: "/source/0000"}); err == nil {
		t.Fatal("ordinary duplicate accepted")
	}
	if err := c.AddShared(SourceFile{ArchivePath: "data/0000", LocalPath: "/different"}); err == nil {
		t.Fatal("conflicting shared reference accepted")
	}
	count := 0
	if err := c.Walk(context.Background(), func(file SourceFile) error {
		if file.ArchivePath != fmt.Sprintf("data/%04d", count) || file.LocalPath != fmt.Sprintf("/source/%04d", count) {
			t.Fatal(file, count)
		}
		// A write transaction here proves no source reader spans consumer IO.
		if err := c.index.db.Update(func(*bolt.Tx) error { return nil }); err != nil {
			return err
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1000 || c.index.count != 1000 {
		t.Fatal(count, c.index.count)
	}
}

func TestSourceCatalogDamageDeletionAndOwnership(t *testing.T) {
	for _, kind := range []string{"mac", "deletion", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			c, err := NewSourceCatalog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for _, name := range []string{"data/a", "data/b"} {
				if err := c.Add(SourceFile{ArchivePath: name, LocalPath: "/source"}); err != nil {
					t.Fatal(err)
				}
			}
			name := c.index.db.Path()
			if kind == "replacement" {
				if err := os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(name + ".old")
				if err := os.WriteFile(name, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := c.index.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(sourceBucket)
				if kind == "deletion" {
					return b.Delete([]byte("data/b"))
				}
				v := append([]byte(nil), b.Get([]byte("data/b"))...)
				v[len(v)-1] ^= 1
				return b.Put([]byte("data/b"), v)
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.Walk(context.Background(), func(SourceFile) error { return nil }); !errors.Is(err, errObservationIndex) {
				t.Fatalf("damage accepted: %v", err)
			}
			closeErr := c.Close()
			if kind == "replacement" {
				if !errors.Is(closeErr, ownedfile.ErrReplaced) {
					t.Fatal(closeErr)
				}
				data, err := os.ReadFile(name)
				if err != nil || string(data) != "foreign" {
					t.Fatal(string(data), err)
				}
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}

func TestCreateConsumesSourceCatalogAndKeepsLegacySortedManifest(t *testing.T) {
	options := manifestLimitOptions(t)
	c, err := NewSourceCatalog(filepath.Dir(options.OutputPath))
	if err != nil {
		t.Fatal(err)
	}
	name := c.index.db.Path()
	for n := len(options.Files) - 1; n >= 0; n-- {
		if err := c.Add(options.Files[n]); err != nil {
			t.Fatal(err)
		}
	}
	options.Files = nil
	options.SourceCatalog = c
	created, err := Create(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("catalog survived publication", err)
	}
	verified, err := Verify(options.OutputPath, options.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	for n, file := range verified.Files {
		if file != created.Files[n] || n > 0 && file.Path <= verified.Files[n-1].Path {
			t.Fatal("manifest order changed")
		}
	}
	if err := c.Add(SourceFile{ArchivePath: "data/late", LocalPath: "/source"}); err == nil {
		t.Fatal("consumed catalog reopened")
	}
}

func TestCreateCatalogFailureDoesNotPublish(t *testing.T) {
	for _, kind := range []string{"cancel", "missing", "deleted", "mixed", "too-wide"} {
		t.Run(kind, func(t *testing.T) {
			options := manifestLimitOptions(t)
			c, err := NewSourceCatalog(filepath.Dir(options.OutputPath))
			if err != nil {
				t.Fatal(err)
			}
			name := c.index.db.Path()
			for _, file := range options.Files {
				if err := c.Add(file); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "mixed" {
				options.Files = nil
			}
			options.SourceCatalog = c
			switch kind {
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				options.Context = ctx
			case "missing":
				if err := os.Remove(filepath.Join(filepath.Dir(options.OutputPath), "source")); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := c.index.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(sourceBucket).Delete([]byte("data/metadata.db")) }); err != nil {
					t.Fatal(err)
				}
			case "too-wide":
				options.Build.Version = strings.Repeat("v", backupManifestSizeLimit)
			}
			result, err := Create(options)
			if err == nil || result.BackupID != "" {
				t.Fatal(result.BackupID, err)
			}
			for _, path := range []string{options.OutputPath, name} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failure leaked output/catalog", path, err)
				}
			}
			files, err := filepath.Glob(filepath.Join(filepath.Dir(options.OutputPath), ".halro-backup-*.tmp"))
			if err != nil || len(files) != 0 {
				t.Fatal(files, err)
			}
		})
	}
}

func TestSourceCatalogRejectsWideAndReservedNames(t *testing.T) {
	c, err := NewSourceCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, file := range []SourceFile{{ArchivePath: "manifest.json", LocalPath: "/source"}, {ArchivePath: "../escape", LocalPath: "/source"}, {ArchivePath: "data/a", LocalPath: strings.Repeat("x", sourceCatalogRecordLimit)}} {
		if err := c.Add(file); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
	if c.index.count != 0 {
		t.Fatal("failed additions changed native count")
	}
}

func TestSourceCatalogEnforcesConfiguredDataFileLimit(t *testing.T) {
	directory := t.TempDir()
	const limit = int64(128 << 10)
	c, err := NewSourceCatalogWithLimit(directory, limit)
	if err != nil {
		t.Fatal(err)
	}
	path := c.index.db.Path()
	var rejected error
	for n := 0; n < 10_000; n++ {
		rejected = c.Add(SourceFile{ArchivePath: fmt.Sprintf("data/%06d", n), LocalPath: "/source/" + strings.Repeat("x", 96)})
		if rejected != nil {
			break
		}
	}
	if rejected == nil {
		t.Fatal("catalog exceeded its configured bbolt limit without refusing an addition")
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Size() > limit {
		t.Fatalf("catalog file exceeded its cap: info=%v err=%v", info, statErr)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("limited catalog was not removed", err)
	}
	if _, err := NewSourceCatalogWithLimit(directory, -1); err == nil {
		t.Fatal("negative source catalog limit accepted")
	}
}

func TestCreateLegacyManifestExactSizeBoundary(t *testing.T) {
	options := manifestLimitOptions(t)
	first, err := Create(options)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	options.OutputPath = filepath.Join(filepath.Dir(options.OutputPath), "exact.hmbk")
	options.Build.Version = strings.Repeat("v", backupManifestSizeLimit-len(encoded)+len(options.Build.Version))
	exact, err := Create(options)
	if err != nil {
		t.Fatal("exact reader limit rejected", err)
	}
	encoded, err = json.Marshal(exact)
	if err != nil || len(encoded) != backupManifestSizeLimit {
		t.Fatal(len(encoded), err)
	}
	if _, err := Verify(options.OutputPath, options.BackupKey); err != nil {
		t.Fatal(err)
	}
	options.OutputPath = filepath.Join(filepath.Dir(options.OutputPath), "over.hmbk")
	options.Build.Version += "v"
	if result, err := Create(options); err == nil || result.BackupID != "" {
		t.Fatal("one byte over reader limit accepted", err)
	}
	if _, err := os.Stat(options.OutputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversize manifest published", err)
	}
}
