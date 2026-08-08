//go:build darwin || linux

package vault

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const MasterKeySize = 32

func InitMasterKey(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create master key directory: %w", err)
	}
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate master key: %w", err)
	}
	defer clear(key)

	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("master key already exists")
		}
		return fmt.Errorf("create master key: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		syscall.Close(fd)
		return errors.New("create master key file handle")
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		return fmt.Errorf("write master key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync master key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close master key: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	ok = true
	return nil
}

func LoadMasterKey(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open master key: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		syscall.Close(fd)
		return nil, errors.New("open master key file handle")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat master key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("master key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("master key permissions must not grant group or other access")
	}
	if info.Size() != MasterKeySize {
		return nil, fmt.Errorf("master key must be exactly %d bytes", MasterKeySize)
	}
	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(file, key); err != nil {
		clear(key)
		return nil, fmt.Errorf("read master key: %w", err)
	}
	return key, nil
}

// ReplaceMasterKey atomically publishes a caller-supplied key beside the
// current key. The caller must first make all encrypted state readable by the
// new key and retain a crash-recovery bridge for the old key.
func ReplaceMasterKey(path string, key []byte) error {
	if len(key) != MasterKeySize {
		return fmt.Errorf("master key must be exactly %d bytes", MasterKeySize)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect current master key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("current master key must be a regular non-symlink file")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".halro-master-key-*")
	if err != nil {
		return fmt.Errorf("create replacement master key: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		temporary.Close()
		if !published {
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set replacement master key permissions: %w", err)
	}
	if _, err := temporary.Write(key); err != nil {
		return fmt.Errorf("write replacement master key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync replacement master key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close replacement master key: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish replacement master key: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	published = true
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
