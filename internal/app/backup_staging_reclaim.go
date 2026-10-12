package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
)

// A failed offline backup keeps its staging directory when a bbolt Open
// failed inside it, because a closed descriptor does not prove the mapping
// retired (ownedfile.OpenExclusiveBolt). The process exit does, so the next
// backup of the same data directory reclaims it (board §40.15, §40.16).
const backupStagingOwnerName = "owner.json"

var backupStagingName = regexp.MustCompile(`^\.halro-backup-stage-[0-9]+$`)

type backupStagingOwner struct {
	DataDir string `json:"data_dir"`
	Process string `json:"process"`
}

var backupProcessToken = sync.OnceValues(func() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
})

// writeBackupStagingOwner records which data directory and process own a new
// staging directory, before anything else is written into it.
func writeBackupStagingOwner(staging, dataDir string) error {
	token, err := backupProcessToken()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(backupStagingOwner{DataDir: dataDir, Process: token})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(staging, backupStagingOwnerName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return durable.SyncDirectory(staging)
}

// reclaimBackupStaging removes staging directories under outputDir that a
// previous process left for the same data directory. The caller holds that
// data directory's exclusive lock, which every backup holds while its staging
// exists, so the owner is no longer running a backup; a different process
// token rules out this process's own retained debt. A missing, unreadable or
// foreign owner record keeps the directory.
func reclaimBackupStaging(outputDir, dataDir string) (removed int, err error) {
	token, err := backupProcessToken()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if !backupStagingName.MatchString(entry.Name()) {
			continue
		}
		staging := filepath.Join(outputDir, entry.Name())
		info, err := os.Lstat(staging)
		if err != nil || !info.IsDir() {
			continue
		}
		marker := filepath.Join(staging, backupStagingOwnerName)
		markerInfo, err := os.Lstat(marker)
		if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Size() > 4<<10 {
			continue
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		var owner backupStagingOwner
		if json.Unmarshal(raw, &owner) != nil || owner.DataDir != dataDir || owner.Process == "" || owner.Process == token {
			continue
		}
		if err := os.RemoveAll(staging); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		return removed, durable.SyncDirectory(outputDir)
	}
	return 0, nil
}
