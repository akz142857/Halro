package metadatajournal

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
)

type StartupAudit = startupaudit.Owner
type StartupAuditOptions = startupaudit.Options

type startupAuditReaderDebt struct {
	file *os.File
	key  []byte
	work resourcebudget.Permit
}

// StartSelectedStartupAudit owns one fixed historical prefix and finite
// progress grant. Only already authenticated native frames advance progress.
// Semantic same-inode append can continue; no structural/Seal/HA work is
// eligible until the actual reader and its fee have retired successfully.
func (l *Log) StartSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions) (*StartupAudit, error) {
	return l.startSelectedStartupAudit(ctx, budget, o, nil, nil)
}

// StartSelectedStartupAuditObserved exposes only authenticated scalar progress.
// An observer cannot advance/extend the grant or access FD/key/payload. A blocked
// observer retains the actual reader fee and remains subject to both deadlines.
func (l *Log) StartSelectedStartupAuditObserved(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions, observe func(context.Context, uint64, int64) error) (*StartupAudit, error) {
	return l.startSelectedStartupAudit(ctx, budget, o, nil, observe)
}

func (l *Log) startSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions, phase func(context.Context, Record, *os.File), observe func(context.Context, uint64, int64) error) (*StartupAudit, error) {
	if l == nil || budget == nil {
		return nil, ErrCorrupt
	}
	if err := startupaudit.Validate(o); err != nil {
		return nil, err
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.startupBudget != budget || !l.startupAuditPending || l.startupAudit != nil || l.startupSaved.Offset <= 0 || l.closing || l.closed || l.file == nil || l.terminalErr != nil || l.retirementErr != nil || l.freezeCancel != nil {
		l.mu.Unlock()
		return nil, errors.Join(ErrCorrupt, work.Release())
	}
	saved, path, key := l.startupSaved, l.path, append([]byte(nil), l.key...)
	original, statErr := l.file.Stat()
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if statErr != nil || parentErr != nil || !parent.IsDir() || original.Size() < saved.Offset {
		l.mu.Unlock()
		clear(key)
		return nil, errors.Join(ErrCorrupt, statErr, parentErr, work.Release())
	}
	l.freezeWG.Add(1)
	a := startupaudit.Start(ctx, o, func(err error) {
		l.mu.Lock()
		l.terminalErr = errors.Join(l.terminalErr, err)
		l.mu.Unlock()
	}, func(jobCtx context.Context, progress func(uint64, int64)) (resultErr error) {
		defer l.freezeWG.Done()
		var file *os.File
		defer func() {
			if file != nil {
				resultErr = errors.Join(resultErr, ownedfile.CloseFile(file))
			}
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				l.mu.Lock()
				l.startupReaderDebt = &startupAuditReaderDebt{file, key, work}
				l.retirementErr = errors.Join(l.retirementErr, resultErr)
				l.terminalErr = errors.Join(l.terminalErr, resultErr)
				l.mu.Unlock()
				return
			}
			if releaseErr := work.Release(); releaseErr != nil {
				resultErr = errors.Join(resultErr, releaseErr)
				l.mu.Lock()
				l.startupReaderDebt = &startupAuditReaderDebt{file, key, work}
				l.terminalErr = errors.Join(l.terminalErr, resultErr)
				l.mu.Unlock()
				return
			}
			clear(key)
		}()
		var err error
		file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		check := func() error {
			if l.startupGuard != nil && !l.startupGuard.CoversNativePath(path) {
				return ErrCorrupt
			}
			actual, statErr := file.Stat()
			named, nameErr := os.Lstat(path)
			directory, dirErr := os.Lstat(filepath.Dir(path))
			if statErr != nil || nameErr != nil || dirErr != nil || !named.Mode().IsRegular() || !os.SameFile(original, actual) || !os.SameFile(actual, named) || actual.Size() < saved.Offset || !directory.IsDir() || !os.SameFile(parent, directory) {
				return errors.Join(ErrCorrupt, statErr, nameErr, dirErr)
			}
			return jobCtx.Err()
		}
		if err := check(); err != nil {
			return err
		}
		var records uint64
		actual, partial, err := scanContext(jobCtx, io.NewSectionReader(file, 0, saved.Offset), key, func(record Record) error {
			var frame [frameHeaderSize]byte
			if _, err := file.ReadAt(frame[:], record.Offset); err != nil {
				return err
			}
			records++
			end := record.Offset + frameHeaderSize + int64(binary.BigEndian.Uint32(frame[24:28])) + frameMACSize
			progress(records, end)
			if observe != nil {
				if err := observe(jobCtx, records, end); err != nil {
					return err
				}
			}
			if phase != nil {
				phase(jobCtx, record, file)
			}
			return jobCtx.Err()
		})
		if err != nil || partial || actual != saved {
			return errors.Join(ErrCorrupt, err)
		}
		return check()
	})
	l.startupAudit = a
	l.mu.Unlock()
	return a, nil
}

func (l *Log) selectedStartupStructuralReadyLocked() bool {
	if !l.startupAuditPending {
		return true
	}
	if l.startupAudit == nil {
		return false
	}
	p := l.startupAudit.Snapshot()
	return p.Complete && p.Err == nil
}

func (l *Log) SelectedStartupStructuralReady() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.selectedStartupStructuralReadyLocked() && l.availabilityError() == nil && l.terminalErr == nil
}
