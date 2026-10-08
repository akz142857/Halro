package resourcebudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// PrepareCapturedPhysicalCandidate prepays an existing, independently verified
// inactive publication inode. expected must come from the caller's complete
// bounded MAC/source verification under its actual directory exclusion. This
// function grants only physical target permission, never content authority.
// It preserves content/EOF, and keeps any partial allocation and actual FD debt.
func PrepareCapturedPhysicalCandidate(ctx context.Context, path string, expected os.FileInfo, bytes int64, manager *Manager, reservation Permit) (candidate *PhysicalCandidate, resultErr error) {
	if manager == nil || expected == nil || !expected.Mode().IsRegular() || expected.Size() < 0 || expected.Size() > bytes || bytes <= 0 || bytes > 1<<30 {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	disk, err := reservation.RetainCovering(manager, Cost{Disk: bytes})
	if err != nil {
		return nil, err
	}
	work, err := manager.TryAcquireContext(ctx, Background, Cost{RAM: 4096})
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	c := &PhysicalCandidate{path: absolute, bytes: bytes, manager: manager, disk: disk, work: work}
	file, err := os.OpenFile(absolute, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.Join(err, work.Release(), disk.Release())
	}
	c.file = file
	candidate = c
	defer func() { c.preparationErr = resultErr; c.ready = resultErr == nil }()
	parent, err := os.Open(filepath.Dir(absolute))
	if err != nil {
		return c, err
	}
	info, err := parent.Stat()
	closeErr := ownedfile.CloseFile(parent)
	if err != nil || closeErr != nil {
		return c, errors.Join(err, closeErr)
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(actual, expected) || actual.Size() != expected.Size() || !actual.ModTime().Equal(expected.ModTime()) {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	c.owner, err = ownedfile.RestoreAuthenticated(absolute, file, expected, info)
	if err != nil {
		return c, err
	}
	if err := AllocatePhysicalRange(file, 0, bytes); err != nil {
		return c, err
	}
	if err := errors.Join(CheckPhysicalRange(file, 0, bytes), CheckPhysicalTail(file, actual.Size(), bytes)); err != nil {
		return c, err
	}
	if err := c.owner.VerifyFile(file); err != nil {
		return c, err
	}
	return c, file.Sync()
}
