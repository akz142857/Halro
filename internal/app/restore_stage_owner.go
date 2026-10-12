package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// A restore extracts into .halro-restore-stage-* beside the data directory
// before it takes the data directory lock, so a concurrent restore may still
// be writing its own stage. Liveness is therefore proved per stage: its owner
// holds an exclusive flock on owner.lock until the stage is removed, or until
// the process exits when the stage is kept as retirement debt (board §42.1).
const restoreStageOwnerName = "owner.lock"

var restoreStageName = regexp.MustCompile(`^\.halro-restore-stage-[0-9]+$`)

// retainedRestoreStages keeps the locks of stages kept as retirement debt
// reachable until the process exits; a collected *os.File would close its
// descriptor and release the lock early.
var retainedRestoreStages struct {
	sync.Mutex
	files []*os.File
}

// releaseRestoreStage closes the stage lock after the stage was removed, and
// otherwise retains it for the life of the process.
func releaseRestoreStage(f *os.File, removed bool) error {
	if f == nil {
		return nil
	}
	if removed {
		return ownedfile.CloseFile(f)
	}
	retainedRestoreStages.Lock()
	retainedRestoreStages.files = append(retainedRestoreStages.files, f)
	retainedRestoreStages.Unlock()
	return nil
}

// holdRestoreStage locks a new stage for this process and records the data
// directory it restores. Close the returned file only after the stage is gone.
func holdRestoreStage(stage, dataDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(stage, restoreStageOwnerName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_, err = io.WriteString(f, dataDir)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = durable.SyncDirectory(stage)
	}
	if err != nil {
		return nil, errors.Join(err, ownedfile.CloseFile(f))
	}
	return f, nil
}

// reclaimRestoreStages removes stages of dataDir whose owner no longer holds
// their lock: the owner wrote its record while locked and releases the lock
// only after removing the stage or by exiting. A stage without a complete
// record for dataDir, or still locked, is kept.
func reclaimRestoreStages(dataParent, dataDir string) (removed int, err error) {
	entries, err := os.ReadDir(dataParent)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if !restoreStageName.MatchString(entry.Name()) {
			continue
		}
		stage := filepath.Join(dataParent, entry.Name())
		info, err := os.Lstat(stage)
		if err != nil || !info.IsDir() {
			continue
		}
		dead, err := restoreStageOwnerExited(filepath.Join(stage, restoreStageOwnerName), dataDir)
		if err != nil {
			return removed, err
		}
		if !dead {
			continue
		}
		if err := os.RemoveAll(stage); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		return removed, durable.SyncDirectory(dataParent)
	}
	return 0, nil
}

func restoreStageOwnerExited(path, dataDir string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<10 {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return false, err
	}
	return string(raw) == dataDir, nil
}
