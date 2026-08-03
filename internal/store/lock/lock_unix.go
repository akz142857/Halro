//go:build darwin || linux

package lock

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrAlreadyLocked = errors.New("data directory is already locked by another process")

type Lock struct {
	file *os.File
}

func Acquire(dataDir string) (*Lock, error) {
	publication, err := acquireFile(publicationLockPath(dataDir), "publication")
	if err != nil {
		return nil, err
	}
	defer publication.Close()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, ".heimdall.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open data lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("acquire data lock: %w", err)
	}
	if err := file.Truncate(0); err == nil {
		if _, err = fmt.Fprintf(file, "%d\n", os.Getpid()); err == nil {
			err = file.Sync()
		}
	}
	if err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("write data lock: %w", err)
	}
	return &Lock{file: file}, nil
}

// AcquireInitialization serializes creation and atomic publication of a new
// data directory without creating that directory first. Normal writers briefly
// take the same sibling guard before opening .heimdall.lock, so they cannot
// race the final directory rename.
func AcquireInitialization(dataDir string) (*Lock, error) {
	return acquireFile(publicationLockPath(dataDir), "initialization publication")
}

func publicationLockPath(dataDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return filepath.Join(filepath.Dir(dataDir), fmt.Sprintf(".heimdall-publication-%x.lock", digest[:8]))
}

func acquireFile(path, purpose string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s lock directory: %w", purpose, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("acquire %s lock: %w", purpose, err)
	}
	return &Lock{file: file}, nil
}

// AcquireExistingReadOnly coordinates with the normal writer lock without
// creating, truncating, or rewriting the lock file. Initialized data
// directories always contain this file.
func AcquireExistingReadOnly(dataDir string) (*Lock, error) {
	path := filepath.Join(dataDir, ".heimdall.lock")
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open existing data lock read-only: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("acquire existing data lock read-only: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
