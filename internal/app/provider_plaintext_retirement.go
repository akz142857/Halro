package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/vault"
	"golang.org/x/sys/unix"
)

var errProviderObjectRetirement = errors.New("provider object physical reader retirement is uncertain")

func (r *Runtime) withProviderObjectMutation(ctx context.Context, work func() error) error {
	if r.gatewayService != nil {
		return r.gatewayService.WithProviderObjectMutation(ctx, work)
	}
	// A directly constructed diagnostic/test Runtime has no Gateway writers.
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

// The caller holds the conditional Metadata writer before taking the object
// gate. It authorizes only this observed plaintext inode, never its basename.
func (r *Runtime) retireUnsealedProviderObject(ctx context.Context, directory, name string, observed os.FileInfo) (resultErr error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || observed == nil {
		return dirscan.ErrIdentity
	}
	return r.withProviderObjectMutation(ctx, func() (resultErr error) {
		parentInfo, err := os.Lstat(directory)
		if err != nil || !parentInfo.IsDir() {
			return errors.Join(dirscan.ErrIdentity, err)
		}
		parent, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := parent.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, errProviderObjectRetirement, closeErr)
			}
		}()
		actual, err := parent.Stat()
		if err != nil || !os.SameFile(parentInfo, actual) {
			return errors.Join(dirscan.ErrIdentity, err)
		}
		path := filepath.Join(directory, name)
		header, current, err := inspectObjectHeader(path)
		if err != nil {
			return err
		}
		if vault.SealedEnvelope(header) || !os.SameFile(observed, current) || observed.Size() != current.Size() || !observed.ModTime().Equal(current.ModTime()) {
			return dirscan.ErrIdentity
		}
		namedParent, err := os.Lstat(directory)
		if err != nil || !os.SameFile(parentInfo, namedParent) {
			return errors.Join(dirscan.ErrIdentity, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
			return err
		}
		return parent.Sync()
	})
}
