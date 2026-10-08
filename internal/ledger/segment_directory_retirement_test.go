package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestSegmentDirectoryRetirementRequiresActualCloseAndOriginalName(t *testing.T) {
	for _, fault := range []string{"close", "missing name", "parent replacement"} {
		t.Run(fault, func(t *testing.T) {
			dir, scratch := writeDirectoryIndexFixture(t, 1)
			index, err := buildSegmentDirectoryIndex(dir, scratch)
			if err != nil {
				t.Fatal(err)
			}
			name, retained := index.index.Name(), index.index.Name()
			originalFile, originalKey := index.index, index.key
			switch fault {
			case "close":
				if err := index.index.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing name":
				retained = name + ".renamed"
				if err := os.Rename(name, retained); err != nil {
					t.Fatal(err)
				}
			case "parent replacement":
				retained = filepath.Join(scratch+".renamed", filepath.Base(name))
				if err := os.Rename(scratch, scratch+".renamed"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(scratch + ".renamed") })
				if err := os.Mkdir(scratch, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := index.Close(); !errors.Is(err, ErrSegmentRetirement) {
				t.Fatal("unproven retirement accepted", err)
			}
			if err := index.Close(); !errors.Is(err, ErrSegmentRetirement) {
				t.Fatal("repeated close erased debt", err)
			}
			if fault == "close" && (index.index != originalFile || index.key != originalKey) {
				t.Fatal("unknown actual exit discarded the original FD or verification key")
			}
			if _, err := os.Lstat(retained); err != nil {
				t.Fatal("uncertain owned inode removed", err)
			}
			if fault == "parent replacement" {
				if raw, err := os.ReadFile(name); err != nil || string(raw) != "foreign" {
					t.Fatal("foreign path removed", err)
				}
			}
		})
	}
}

func TestPersistentDirectoryPublishRetainsPublishedStateAfterSyncFailure(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 0)
	index, err := buildSegmentDirectoryIndexWithKey(dir, dir, 4096, testChainKey)
	if err != nil {
		t.Fatal(err)
	}
	index.slotBase = segmentDirectoryHeaderBytes
	syncFailure := errors.New("injected published directory Sync failure")
	if err := publishPersistentSegmentDirectoryIndexWithSync(dir, index, func(string) error { return syncFailure }); !errors.Is(err, syncFailure) {
		t.Fatal(err)
	}
	if !index.persistent {
		t.Fatal("published state lost")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, segmentDirectoryIndexName)); err != nil {
		t.Fatal("published locator removed", err)
	}
	reader, err := openPersistentSegmentDirectoryIndex(dir, 4096, testChainKey)
	if err != nil || reader == nil {
		t.Fatal("published locator unavailable", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentDirectoryPublishPreservesLateForeignTarget(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 0)
	path := filepath.Join(dir, segmentDirectoryIndexName)
	if err := os.WriteFile(path, []byte("captured cache"), 0600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	index, err := buildSegmentDirectoryIndexWithKey(dir, dir, 4096, testChainKey)
	if err != nil {
		t.Fatal(err)
	}
	index.targetInfo, index.slotBase = previous, segmentDirectoryHeaderBytes
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishPersistentSegmentDirectoryIndex(dir, index); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if index.persistent {
		t.Fatal("failed replacement reported published")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign target overwritten", err)
	}
}

func TestBorrowedInvalidDirectoryCacheDoesNotGrantRemovalAuthority(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 0)
	path := filepath.Join(dir, segmentDirectoryIndexName)
	if err := os.WriteFile(path, []byte("bad borrowed cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if index, err := openPersistentSegmentDirectoryIndex(dir, 4096, testChainKey); err != nil || index != nil {
		t.Fatal(index, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "bad borrowed cache" {
		t.Fatal("borrowed cache removed", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(segmentManifestPath(dir), path); err != nil {
		t.Fatal(err)
	}
	if index, err := openPersistentSegmentDirectoryIndex(dir, 4096, testChainKey); err == nil || index != nil {
		t.Fatal("symlink accepted", index, err)
	}
}

func TestInspectionManifestRejectsSymlinkParentReplacementAndUncertainClose(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 0)
	manifest, err := openInspectionManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	originalFile := manifest.file
	if err := manifest.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manifest.close(); !errors.Is(err, ErrSegmentRetirement) {
		t.Fatal(err)
	}
	if err := manifest.close(); !errors.Is(err, ErrSegmentRetirement) {
		t.Fatal("retirement debt lost", err)
	}
	if manifest.file != originalFile || !errors.Is(manifest.closeErr, ownedfile.ErrRetirement) {
		t.Fatal("manifest lost its actual descriptor retirement debt")
	}
	path := segmentManifestPath(dir)
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".retained", path); err != nil {
		t.Fatal(err)
	}
	if manifest, err := openInspectionManifest(dir); err == nil || manifest != nil {
		t.Fatal("manifest symlink accepted", manifest, err)
	}
}
