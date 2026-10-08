//go:build darwin || linux

package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

const MasterKeySize = 32

func InitMasterKey(path string) error { return InitMasterKeyContext(context.Background(), path) }
func InitMasterKeyContext(ctx context.Context, path string) error {
	return initMasterKeyWithPhase(ctx, path, nil)
}
func initMasterKeyWithPhase(ctx context.Context, path string, phase func(*os.File) error) (resultErr error) {
	if ctx == nil {
		return errors.New("master key initialization requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create master key directory: %w", err)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	var key [MasterKeySize]byte
	if _, err := rand.Read(key[:]); err != nil {
		return fmt.Errorf("generate master key: %w", err)
	}
	defer clear(key[:])
	f, owner, err := ownedfile.Create(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("master key already exists at %s: if this instance is already initialized there is nothing to do; if the data directory is empty this key is from an earlier attempt and must be kept with it — never delete a Master Key that any data directory or backup was written under", path)
		}
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("create master key at %s: %w (the directory must be writable by the user Halro runs as; in a container that is the --user uid, not the host user that created the mount)", path, err)
		}
		return fmt.Errorf("create master key: %w", err)
	}
	closed, published, uncertain := false, false, false
	defer func() {
		if !closed {
			closed = true
			if err := f.Close(); err != nil {
				uncertain = true
				resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
			}
		}
		if published || uncertain {
			return
		}
		current, err := os.Lstat(filepath.Dir(path))
		if err != nil || !os.SameFile(parent, current) {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, ownedfile.ErrReplaced, err)
			return
		}
		if err := owner.Retire(); err != nil {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
			return
		}
		if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
		}
	}()
	n, err := f.Write(key[:])
	if err != nil || n != len(key) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if phase != nil {
		if err := phase(f); err != nil {
			return err
		}
	}
	if err := owner.VerifyFile(f); err != nil {
		return err
	}
	closed = true
	if err := f.Close(); err != nil {
		uncertain = true
		return errors.Join(ErrMasterKeyRetirement, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parent, current) {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	// The key name already exists. A directory Sync failure must preserve this
	// only key, and can never be recast as an unpublished temporary to delete.
	published = true
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return errors.Join(ErrMasterKeyRetirement, err)
	}
	return nil
}

var ErrMasterKeyRetirement = errors.New("master key actual file retirement is uncertain")

func LoadMasterKey(path string) ([]byte, error) {
	key, _, err := LoadMasterKeyWithIdentity(context.Background(), path)
	return key, err
}

// This receipt comes from the exact descriptor which supplied the key, after
// its actual Close. It can constrain replacement, never authenticate a DB.
func LoadMasterKeyWithIdentity(ctx context.Context, path string) (key []byte, identity os.FileInfo, resultErr error) {
	return loadMasterKeyWithOpen(ctx, path, func(name string) (*os.File, error) {
		return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	})
}
func loadMasterKeyWithOpen(ctx context.Context, path string, open func(string) (*os.File, error)) (key []byte, identity os.FileInfo, resultErr error) {
	if ctx == nil {
		return nil, nil, errors.New("master key reader requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f, err := open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open master key: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
		}
		if resultErr != nil {
			clear(key)
			key = nil
			identity = nil
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != MasterKeySize {
		return nil, nil, errors.New("master key must be a private regular file of exactly 32 bytes")
	}
	key = make([]byte, MasterKeySize)
	if _, err := io.ReadFull(f, key); err != nil {
		return key, nil, err
	}
	current, err := os.Lstat(path)
	final, statErr := f.Stat()
	if err != nil || statErr != nil || !os.SameFile(info, current) || !os.SameFile(info, final) || final.Size() != MasterKeySize || current.Size() != MasterKeySize || !info.ModTime().Equal(final.ModTime()) || !info.ModTime().Equal(current.ModTime()) {
		return key, nil, errors.Join(err, statErr, ownedfile.ErrReplaced)
	}
	if err := ctx.Err(); err != nil {
		return key, nil, err
	}
	return key, info, nil
}

// ReplaceMasterKey atomically publishes a caller-supplied key beside the
// current key. The caller must first make all encrypted state readable by the
// new key and retain a crash-recovery bridge for the old key.
func ReplaceMasterKey(path string, key []byte) error {
	return ReplaceMasterKeyCapturedContext(context.Background(), path, key, nil)
}
func ReplaceMasterKeyCapturedContext(ctx context.Context, path string, key []byte, expected os.FileInfo) error {
	return replaceMasterKeyCapturedContext(ctx, path, key, expected, nil)
}
func replaceMasterKeyCapturedContext(ctx context.Context, path string, key []byte, expected os.FileInfo, phase func(*os.File) error) (resultErr error) {
	if ctx == nil {
		return errors.New("master key replacement requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(key) != MasterKeySize {
		return errors.New("master key must be exactly 32 bytes")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	if expected != nil && (!os.SameFile(expected, info) || expected.Size() != info.Size() || !expected.ModTime().Equal(info.ModTime())) {
		return ownedfile.ErrReplaced
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	f, owner, err := ownedfile.CreateTemporary(filepath.Dir(path), ".halro-master-key-*")
	if err != nil {
		return err
	}
	closed, published, uncertain := false, false, false
	defer func() {
		if !closed {
			closed = true
			if err := f.Close(); err != nil {
				uncertain = true
				resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
			}
		}
		if uncertain || published {
			return
		}
		current, err := os.Lstat(filepath.Dir(path))
		if err != nil || !os.SameFile(current, parent) {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, ownedfile.ErrReplaced, err)
			return
		}
		if err := owner.Retire(); err != nil {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
			return
		}
		if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
			resultErr = errors.Join(resultErr, ErrMasterKeyRetirement, err)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	n, err := f.Write(key)
	if err != nil || n != len(key) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if phase != nil {
		if err := phase(f); err != nil {
			return err
		}
	}
	if err := owner.VerifyFile(f); err != nil {
		return err
	}
	closed = true
	if err := f.Close(); err != nil {
		uncertain = true
		return errors.Join(ErrMasterKeyRetirement, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parent, current) {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	current, err = os.Lstat(path)
	if err != nil || !os.SameFile(info, current) || info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	if err := owner.ReplaceCaptured(path, info); err != nil {
		return err
	}
	published = true
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return errors.Join(ErrMasterKeyRetirement, err)
	}
	return nil
}
