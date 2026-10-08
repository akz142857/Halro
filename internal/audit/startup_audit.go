package audit

import (
	"context"
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

// StartSelectedStartupAudit owns an independent fixed historical prefix.
// Same-inode append can continue after authenticated progress, while the
// finite deadline grant remains valid. Close cancels and joins the actual
// reader before releasing native state. A failed or stalled audit withdraws
// native readiness and semantic append permission immediately.
func (l *Log) StartSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions) (*StartupAudit, error) {
	return l.startSelectedStartupAudit(ctx, budget, o, nil)
}

// phase is a test-only hook at an already authenticated frame; it can block
// actual reader retirement to verify that deadline withdrawal does not refund.
func (l *Log) startSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions, phase func(uint64, *os.File)) (*StartupAudit, error) {
	if l == nil || budget == nil || l.cacheBudget != budget {
		return nil, ErrStartupCache
	}
	if err := startupaudit.Validate(o); err != nil {
		return nil, err
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: startupCacheRAMBytes})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if !l.startupAuditPending || l.startupAudit != nil || l.closing || l.file == nil || l.terminalErr != nil {
		l.mu.Unlock()
		return nil, errors.Join(ErrStartupCache, work.Release())
	}
	saved, path := l.startupSaved, l.path
	key := append([]byte(nil), l.key...)
	original, statErr := l.file.Stat()
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if statErr != nil || parentErr != nil || !parent.IsDir() || original.Size() < saved.Bytes {
		l.mu.Unlock()
		clear(key)
		return nil, errors.Join(ErrStartupCache, statErr, parentErr, work.Release())
	}
	l.replayWG.Add(1)
	a := startupaudit.Start(ctx, o, func(err error) {
		l.mu.Lock()
		l.terminalErr = errors.Join(l.terminalErr, err)
		l.mu.Unlock()
	}, func(jobCtx context.Context, progress func(uint64, int64)) (resultErr error) {
		defer l.replayWG.Done()
		var file *os.File
		defer func() {
			if file != nil {
				resultErr = errors.Join(resultErr, ownedfile.CloseFile(file))
			}
			l.mu.Lock()
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				// Existing replay retirement storage pins actual FD/key/permit
				// references when native Close itself cannot establish exit.
				l.startupReaderDebt = &startupAuditReaderDebt{file: file, key: key, work: work}
				l.replayRetirementErr = errors.Join(l.replayRetirementErr, resultErr)
				l.terminalErr = errors.Join(l.terminalErr, resultErr)
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			clear(key)
			resultErr = errors.Join(resultErr, work.Release())
		}()
		var err error
		file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		check := func() error {
			actual, err := file.Stat()
			named, nameErr := os.Lstat(path)
			directory, dirErr := os.Lstat(filepath.Dir(path))
			if err != nil || nameErr != nil || dirErr != nil || !named.Mode().IsRegular() || !os.SameFile(original, actual) || !os.SameFile(actual, named) || actual.Size() < saved.Bytes || !directory.IsDir() || !os.SameFile(parent, directory) {
				return errors.Join(ErrStartupCache, err, nameErr, dirErr)
			}
			return jobCtx.Err()
		}
		if err := check(); err != nil {
			return err
		}
		actual, partial, err := scanFromContext(jobCtx, io.NewSectionReader(file, 0, saved.Bytes), key, Summary{}, func(record Record) error {
			progress(record.Sequence, record.Offset)
			if phase != nil {
				phase(record.Sequence, file)
			}
			return jobCtx.Err()
		})
		if err != nil || partial || actual != saved {
			return errors.Join(ErrStartupCache, err)
		}
		if err := check(); err != nil {
			return err
		}
		return nil
	})
	l.startupAudit = a
	l.mu.Unlock()
	return a, nil
}

type startupAuditReaderDebt struct {
	file *os.File
	key  []byte
	work resourcebudget.Permit
}

// SelectedStartupStructuralReady excludes structural work and publication
// until the historical reader has authenticated and retired successfully.
func (l *Log) SelectedStartupStructuralReady() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return (!l.startupAuditPending || l.startupAudit != nil && l.startupAudit.Snapshot().Complete && l.startupAudit.Snapshot().Err == nil) && !l.closing && l.file != nil && l.terminalErr == nil
}
