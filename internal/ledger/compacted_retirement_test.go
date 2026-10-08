package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestCompactedNativeRetirementPreservesForeignParentAndActualCloseDebt(t *testing.T) {
	for _, mode := range []string{"source", "parent", "close", "success"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sealed.seg")
			if err := os.WriteFile(path, []byte("original native representation"), 0600); err != nil {
				t.Fatal(err)
			}
			expected, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			parentInfo, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			renamed := dir + ".original"
			t.Cleanup(func() { _ = os.RemoveAll(renamed) })
			retired, err := retireCompactedNativeWithPhase(path, expected, parentInfo, func(parent, file *os.File) error {
				switch mode {
				case "source":
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("foreign source"), 0600)
				case "parent":
					if err := os.Rename(dir, renamed); err != nil {
						return err
					}
					if err := os.Mkdir(dir, 0700); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("foreign directory"), 0600)
				case "close":
					return file.Close()
				}
				return nil
			})
			if mode == "success" {
				if !retired || err != nil {
					t.Fatal("owned terminal retirement failed", retired, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				return
			}
			if retired || err == nil {
				t.Fatal("foreign/unproven retirement succeeded", retired, err)
			}
			if mode == "close" {
				if !errors.Is(err, ErrSegmentRetirement) {
					t.Fatal("actual close debt lost", err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("unproven native source removed", err)
				}
			} else {
				if !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "foreign "+map[string]string{"source": "source", "parent": "directory"}[mode] {
					t.Fatal("foreign native file removed", err)
				}
				original := path + ".original"
				if mode == "parent" {
					original = filepath.Join(renamed, "sealed.seg")
				}
				if _, err := os.Stat(original); err != nil {
					t.Fatal("renamed original removed", err)
				}
			}
		})
	}
}
func TestDirectoryRefreshPreservesLateForeignLocatorAfterManifestCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 4)
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, segmentDirectoryIndexName)
	operations := productionCompactIO()
	publish := operations.publish
	operations.publish = func(directory string, source segmentManifestSource) (bool, error) {
		committed, err := publish(directory, source)
		if err != nil || !committed {
			return committed, err
		}
		if err := os.Rename(index, index+".original"); err != nil {
			return committed, err
		}
		return committed, os.WriteFile(index, []byte("foreign locator"), 0600)
	}
	if _, err := log.compactWithIO(rolled.Sealed.Generation, operations); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("late foreign locator replaced", err)
	}
	if log.Status().Load() == AccountingHealthy {
		t.Fatal("native writer stayed healthy after committed manifest/locator mismatch")
	}
	raw, err := os.ReadFile(index)
	if err != nil || string(raw) != "foreign locator" {
		t.Fatal("foreign locator removed", err)
	}
	for _, preserved := range []string{index + ".original", filepath.Join(dir, rolled.Sealed.File), filepath.Join(dir, "ledger-1.seg.gz")} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatal("committed/native representation lost", preserved, err)
		}
	}
}
