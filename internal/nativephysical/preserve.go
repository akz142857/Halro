package nativephysical

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func hasAuthenticatedReceipt(path string, key []byte) (result bool, resultErr error) {
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(fmt.Sprintf("%s.physical-window-%d", path, i), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return false, errors.Join(ErrOwnership, err, ownedfile.CloseFile(f))
		}
		var raw [4096]byte
		if info.Size() > 0 {
			_, err = io.ReadFull(io.NewSectionReader(f, 0, info.Size()), raw[:info.Size()])
		}
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			return false, errors.Join(err, closeErr)
		}
		if err != nil {
			return false, err
		}
		var r receipt
		if json.Unmarshal(bytes.TrimRight(raw[:], "\x00"), &r) == nil {
			m, e := mac(r, key)
			if e == nil && hmac.Equal(m, r.MAC) {
				result = true
			}
		}
	}
	return result, nil
}

func preserveUnqualifiedControls(ctx context.Context, path string) error {
	var names [2]string
	var infos [2]os.FileInfo
	any := false
	for i := range names {
		names[i] = fmt.Sprintf("%s.physical-window-%d", path, i)
		info, err := os.Lstat(names[i])
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		infos[i] = info
		any = true
	}
	if !any {
		return nil
	}
	dir := ""
	for slot := 0; slot < 4; slot++ {
		candidate := fmt.Sprintf("%s.physical-unqualified-%d", path, slot)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			dir = candidate
			break
		} else if err != nil {
			return err
		}
	}
	if dir == "" {
		return resourcebudget.ErrExhausted
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	parent, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	for i, name := range names {
		if infos[i] == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := os.Lstat(name)
		if err != nil || !os.SameFile(actual, infos[i]) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		named, err := os.Lstat(dir)
		if err != nil || !os.SameFile(named, parent) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := renameOpaque(name, filepath.Join(dir, fmt.Sprintf("bank-%d", i))); err != nil {
			return err
		}
		if err := errors.Join(durable.SyncDirectory(dir), durable.SyncDirectory(filepath.Dir(path))); err != nil {
			return err
		}
	}
	return nil
}
