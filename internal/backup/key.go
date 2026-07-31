package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func LoadKeyFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("backup key path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("backup key file must not be accessible by group or others")
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	key, err := io.ReadAll(io.LimitReader(handle, 33))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		clear(key)
		return nil, fmt.Errorf("backup key file must contain exactly 32 bytes, got %d", len(key))
	}
	return key, nil
}
