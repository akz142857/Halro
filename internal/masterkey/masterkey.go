package masterkey

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/vault"
)

var ErrModeUnavailable = errors.New("master key mode is not available in this build")

// Unlocker is the provider-neutral boundary used by application control-plane
// paths. Returned key bytes are owned by the caller and must be cleared as soon
// as all required derived material and Vault state have been created.
type Unlocker interface {
	Mode() string
	Unlock(context.Context) ([]byte, error)
}

// NewUnlocker constructs an unlocker without performing I/O. In particular,
// static configuration validation never initializes a cloud adapter or makes a
// network call.
func NewUnlocker(masterKey config.MasterKey) (Unlocker, error) {
	switch masterKey.Mode {
	case config.MasterKeyModeFile:
		return NewFileStore(masterKey.File)
	case config.MasterKeyModeKeySlots:
		return nil, fmt.Errorf("%w: %s", ErrModeUnavailable, masterKey.Mode)
	default:
		return nil, fmt.Errorf("unsupported master key mode %q", masterKey.Mode)
	}
}

// FileStore owns the File-mode persistence operations. These capabilities are
// intentionally not part of Unlocker because a future KMS-backed unlocker must
// not inherit file replacement semantics.
type FileStore struct {
	path string
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("master key file path is required")
	}
	return &FileStore{path: path}, nil
}

func (f *FileStore) Mode() string {
	return config.MasterKeyModeFile
}

func (f *FileStore) Path() string {
	return f.path
}

func (f *FileStore) Unlock(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return vault.LoadMasterKey(f.path)
}

func (f *FileStore) Exists(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Lstat(f.path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect master key file: %w", err)
}

func (f *FileStore) Initialize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return vault.InitMasterKey(f.path)
}

func (f *FileStore) Replace(ctx context.Context, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return vault.ReplaceMasterKey(f.path, key)
}

func FileStoreFromConfig(masterKey config.MasterKey) (*FileStore, error) {
	if masterKey.Mode != config.MasterKeyModeFile {
		return nil, fmt.Errorf("%w: %s", ErrModeUnavailable, masterKey.Mode)
	}
	return NewFileStore(masterKey.File)
}
