package usage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
)

// Private archive temporaries are created with os.CreateTemp/MkdirTemp, whose
// "*" becomes decimal digits. Fixed-name resumable state
// (.archive-date-*-v1.json) never matches.
var (
	archiveCrashTemporaryFile = regexp.MustCompile(`^\.archive-(entries|column|partition|legacy|date-obligation|catalog|root|date-state)-[0-9]+\.tmp$|^\.archive-date-reclaim-spool-[0-9]+$`)
	archiveCrashTemporaryDir  = regexp.MustCompile(`^\.archive-(verify|date-ref-index|date-index|date-orphan-page-index)-[0-9]+$`)
)

// liveArchiveRoots counts the Exporters of this process per root. A root with
// a live Exporter, or one whose Close failed, may still own its temporaries.
var liveArchiveRoots = struct {
	sync.Mutex
	count map[string]int
}{count: map[string]int{}}

func registerArchiveRoot(root string) (first bool) {
	liveArchiveRoots.Lock()
	defer liveArchiveRoots.Unlock()
	first = liveArchiveRoots.count[root] == 0
	liveArchiveRoots.count[root]++
	return first
}

func unregisterArchiveRoot(root string) {
	liveArchiveRoots.Lock()
	defer liveArchiveRoots.Unlock()
	if liveArchiveRoots.count[root]--; liveArchiveRoots.count[root] <= 0 {
		delete(liveArchiveRoots.count, root)
	}
}

// reclaimArchiveCrashTemporaries removes the private temporaries a killed
// process left under root (board §28, §40.16). The caller holds the data
// directory's exclusive lock, so no other process owns them, and no other
// Exporter of this process is live on root. Nothing is refunded: the fees of
// a killed process died with it.
func reclaimArchiveCrashTemporaries(ctx context.Context, root string) (removed int, err error) {
	var walk func(string, int) error
	walk = func(directory string, depth int) error {
		f, err := os.Open(directory)
		if err != nil {
			return err
		}
		var subdirectories []string
		changed := false
		for {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, f.Close())
			}
			entries, readErr := f.ReadDir(archiveInventoryBatch)
			for _, entry := range entries {
				path := filepath.Join(directory, entry.Name())
				info, err := os.Lstat(path)
				if err != nil {
					return errors.Join(err, f.Close())
				}
				switch {
				case info.Mode().IsRegular() && archiveCrashTemporaryFile.MatchString(entry.Name()):
					err = os.Remove(path)
				case info.IsDir() && archiveCrashTemporaryDir.MatchString(entry.Name()):
					err = os.RemoveAll(path)
				case info.IsDir() && depth < archiveInventoryMaxDepth:
					subdirectories = append(subdirectories, path)
					continue
				default:
					continue
				}
				if err != nil {
					return errors.Join(err, f.Close())
				}
				removed++
				changed = true
			}
			if errors.Is(readErr, io.EOF) || readErr == nil && len(entries) == 0 {
				break
			}
			if readErr != nil {
				return errors.Join(readErr, f.Close())
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		if changed {
			if err := durable.SyncDirectory(directory); err != nil {
				return err
			}
		}
		for _, subdirectory := range subdirectories {
			if err := walk(subdirectory, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return removed, walk(root, 0)
}
