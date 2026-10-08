package dirscan

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var ErrDepth = errors.New("directory tree exceeds fixed depth or path limit")

// Tree keeps <=BatchEntries names per level and <=maxDepth+1 actual FDs.
// maxDepth is an explicit structural limit, never a truncation. Callbacks borrow
// the actual regular-file FD or postorder directory FD until they return;
// callback effects are tentative until the final identity/Close checks succeed.
// It refuses links/special files and opens each name through a pinned os.Root.
func Tree(ctx context.Context, path string, maxDepth int, consume func(relative string, file *os.File, info os.FileInfo) error) (resultErr error) {
	if ctx == nil || consume == nil || maxDepth < 0 || maxDepth > 64 {
		return ErrDepth
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !identity.IsDir() {
		return ErrIdentity
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return errors.Join(ErrIdentity, err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	defer func() {
		named, err := os.Lstat(path)
		if err != nil || !os.SameFile(identity, named) {
			resultErr = errors.Join(resultErr, ErrIdentity, err)
		}
	}()
	var walk func(string, int) error
	walk = func(relative string, depth int) (walkErr error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > maxDepth || len(relative) > 4096 {
			return ErrDepth
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return errors.Join(ErrIdentity, err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.Join(ErrIdentity, errors.New("directory tree contains symlink or special file"))
		}
		file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return errors.Join(ErrIdentity, err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				walkErr = errors.Join(walkErr, err)
			}
		}()
		actual, err := file.Stat()
		if err != nil || !os.SameFile(info, actual) || relative == "." && !os.SameFile(identity, actual) {
			return errors.Join(ErrIdentity, err)
		}
		if info.IsDir() {
			for {
				entries, readErr := file.ReadDir(BatchEntries)
				for _, entry := range entries {
					if err := walk(filepath.Join(relative, entry.Name()), depth+1); err != nil {
						return err
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return readErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consume(relative, file, info); err != nil {
			return err
		}
		named, err := root.Lstat(relative)
		if err != nil || !os.SameFile(info, named) {
			return errors.Join(ErrIdentity, err)
		}
		return ctx.Err()
	}
	err = walk(".", 0)
	if err == Stop {
		return nil
	}
	return err
}
