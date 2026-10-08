package ledger

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
)

type StartupAudit = startupaudit.Owner
type StartupAuditOptions = startupaudit.Options
type startupAuditReaderDebt struct {
	file    *os.File
	segment *segmentReader
	key     []byte
	work    resourcebudget.Permit
	err     error
}

func (l *Log) selectedStartupAppendAllowedLocked() bool {
	return !l.startupAuditPending || l.startupAudit != nil && l.startupAudit.Snapshot().Ready
}
func (l *Log) selectedStartupStructuralAllowedLocked() bool {
	return !l.startupAuditPending || l.startupAudit != nil && l.startupAudit.Snapshot().Complete && l.startupAudit.Snapshot().Err == nil
}
func (l *Log) SelectedStartupStructuralReady() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.selectedStartupStructuralAllowedLocked() && l.file != nil && l.closeContext.Err() == nil && l.status.Load() == AccountingHealthy
}

func (l *Log) StartSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions) (*StartupAudit, error) {
	return l.startSelectedStartupAudit(ctx, budget, o, nil)
}

func (l *Log) startSelectedStartupAudit(ctx context.Context, budget *resourcebudget.Manager, o StartupAuditOptions, phase func(Record, *os.File)) (*StartupAudit, error) {
	if l == nil || budget == nil || l.startupBudget != budget {
		return nil, ErrProjectionSource
	}
	if err := startupaudit.Validate(o); err != nil {
		return nil, err
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if !l.startupAuditPending || l.startupAudit != nil || l.file == nil || l.closeContext.Err() != nil || l.status.Load() != AccountingHealthy {
		l.mu.Unlock()
		return nil, errors.Join(ErrProjectionSource, work.Release())
	}
	saved, path, directory, parent := l.startupSaved, l.path, l.directory, l.directoryInfo
	key := append([]byte(nil), l.chainKey...)
	original, err := l.file.Stat()
	compressedSaved := false
	if err == nil && saved.Head.Generation < l.generation {
		segment, found, readErr := l.lookupSegmentLocked(saved.Head.Generation)
		if readErr != nil || !found {
			err = errors.Join(ErrSegmentMissing, readErr)
		} else {
			path, compressedSaved = filepath.Join(directory, segment.File), segment.Compressed
			original, err = os.Lstat(path)
			if err == nil && !original.Mode().IsRegular() {
				err = ErrProjectionSource
			}
		}
	}
	if err != nil {
		l.mu.Unlock()
		clear(key)
		return nil, errors.Join(err, work.Release())
	}
	l.startupAuditWG.Add(1)
	l.startupAuditDone = make(chan struct{})
	done := l.startupAuditDone
	a := startupaudit.Start(ctx, o, func(error) { l.status.RequireRecovery() }, func(jobCtx context.Context, progress func(uint64, int64)) (resultErr error) {
		defer close(done)
		defer l.startupAuditWG.Done()
		linkedCtx, cancel := context.WithCancel(jobCtx)
		stop := context.AfterFunc(l.closeContext, cancel)
		defer func() { stop(); cancel() }()
		var active *os.File
		var retained *segmentReader
		defer func() {
			if active != nil {
				resultErr = errors.Join(resultErr, ownedfile.CloseFile(active))
			}
			if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
				l.mu.Lock()
				l.startupReaderDebt = &startupAuditReaderDebt{file: active, segment: retained, key: key, work: work, err: resultErr}
				l.mu.Unlock()
				l.status.RequireRecovery()
				return
			}
			clear(key)
			resultErr = errors.Join(resultErr, work.Release())
		}()
		checkParent := func() error {
			info, err := os.Lstat(directory)
			if err != nil || !info.IsDir() || !os.SameFile(parent, info) {
				return errors.Join(ErrProjectionSource, err)
			}
			return linkedCtx.Err()
		}
		if err := checkParent(); err != nil {
			return err
		}
		v := &chainVerifier{key: key}
		var bytesRead int64
		consume := func(file *os.File) func(Record) error {
			return func(r Record) error {
				if err := linkedCtx.Err(); err != nil {
					return err
				}
				if authenticatedFrameEpoch(v.epoch) {
					progress(r.Sequence, bytesRead+r.Offset)
				}
				if phase != nil {
					phase(r, file)
				}
				return linkedCtx.Err()
			}
		}
		for generation := uint64(1); generation < saved.Head.Generation; generation++ {
			if err := checkParent(); err != nil {
				return err
			}
			l.mu.Lock()
			segment, found, err := l.lookupSegmentLocked(generation)
			l.mu.Unlock()
			if err != nil || !found {
				return errors.Join(ErrSegmentMissing, err)
			}
			start, err := decodeChainHash(segment.StartHash)
			if err != nil || start != v.hash {
				return errors.Join(ErrTampered, err)
			}
			segmentPath := filepath.Join(directory, segment.File)
			file, err := os.OpenFile(segmentPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			info, statErr := file.Stat()
			var stream io.ReadCloser = file
			if statErr == nil && segment.Compressed {
				gz, gzErr := gzip.NewReader(file)
				statErr = gzErr
				if gzErr == nil {
					stream = gzipReadCloser{Reader: gz, under: file}
				}
			}
			reader := &segmentReader{open: func() (io.ReadCloser, error) { return stream, nil }}
			// Retain the opened stream even if gzip header validation failed.
			if statErr != nil {
				closeErr := ownedfile.CloseFile(file)
				if closeErr != nil {
					retained = &segmentReader{reader: file, closed: true, closeErr: closeErr}
				}
				return errors.Join(statErr, closeErr)
			}
			v.offset = 0
			head, partial, scanErr := scan(reader, generation, 0, v.sequence, consume(file), v)
			current, nameErr := os.Lstat(segmentPath)
			if nameErr != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
				scanErr = errors.Join(scanErr, ErrProjectionSource, nameErr)
			}
			closeErr := reader.Close()
			if closeErr != nil {
				retained = reader
			}
			if err := errors.Join(scanErr, closeErr); err != nil {
				return err
			}
			end, err := decodeChainHash(segment.EndHash)
			if err != nil || partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence || v.hash != end || segment.EndEpoch != 0 && byte(segment.EndEpoch) != v.epoch {
				return errors.Join(ErrTampered, err)
			}
			bytesRead += segment.Length
		}
		active, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		checkActive := func() error {
			actual, err := active.Stat()
			named, nameErr := os.Lstat(path)
			if err != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(original, actual) || !os.SameFile(actual, named) || !compressedSaved && actual.Size() < saved.Head.Offset {
				return errors.Join(ErrProjectionSource, err, nameErr)
			}
			return checkParent()
		}
		if err := checkActive(); err != nil {
			return err
		}
		v.offset = 0
		var prefix io.ReadSeeker = boundedfilecache.NewReader(active, saved.Head.Offset)
		if compressedSaved {
			gz, err := gzip.NewReader(active)
			if err != nil {
				return err
			}
			defer gz.Close()
			prefix = startupPrefixStream{Reader: io.LimitReader(gz, saved.Head.Offset)}
		}
		head, partial, err := scan(prefix, saved.Head.Generation, 0, v.sequence, consume(active), v)
		if err != nil || partial || head != saved.Head || v.hash != saved.Hash || v.epoch != saved.Epoch {
			return errors.Join(ErrTampered, err)
		}
		return checkActive()
	})
	l.startupAudit = a
	l.startupAuditPtr.Store(a)
	l.mu.Unlock()
	return a, nil
}

// A fresh compressed stream supports only the scanner's initial logical seek.
// LimitReader stops at the exact selected frame boundary, never its suffix.
type startupPrefixStream struct{ io.Reader }

func (s startupPrefixStream) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 || whence != io.SeekStart {
		return 0, ErrProjectionSource
	}
	return 0, nil
}
