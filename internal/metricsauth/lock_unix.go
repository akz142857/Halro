//go:build darwin || linux

package metricsauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func withCredentialLock(path string, operation func() error) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open metrics credential lock: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock metrics credentials: %w", err)
	}
	operationErr := operation()
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return errors.Join(operationErr, unlockErr)
}
