// Package logging owns Halro's process log: how it is built from configuration,
// and where it is written.
//
// A file sink exists because a single-binary gateway that owns its own data
// directory should not need a log shipper to answer "why did that request fail".
// Halro wrote to stderr only, which is fine under systemd or Docker and nothing
// at all when the operator runs the binary directly — and the answers that
// matter here (a provider refusal, a probe that never left the process) are
// exactly the ones nobody scrolls back far enough to find.
//
// What this deliberately is not: a log shipper, a network sink, or a second
// retention system. Rotation is size-based because that is the property that
// bounds a disk; anything richer belongs to the operator's own tooling, which is
// welcome to read these files.
package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Options describes one file sink. Zero values are refused rather than guessed:
// the caller is configuration, which has its own defaults, and a sink that
// invents a size limit is a sink that fills a disk on a typo.
type Options struct {
	// Path is the file written now. Rotated generations are Path.1, Path.2, and
	// so on, oldest highest.
	Path string
	// MaxSizeBytes rotates the current file once a write would carry it past
	// this size. A single record larger than the limit is still written whole:
	// splitting a log line is worse than briefly exceeding a soft bound.
	MaxSizeBytes int64
	// MaxFiles counts every generation kept, including the one being written.
	// 1 keeps no history — the file is rotated onto nothing and started again.
	MaxFiles int
	// Fallback receives records that could not be written to the file, plus one
	// notice explaining why. A full disk must not make the log silent as well.
	Fallback io.Writer
}

// Sink is an io.Writer for a slog handler. Writes are serialized: slog handlers
// may be called from any goroutine, and a rotation must not interleave with the
// record that triggered it.
type Sink struct {
	mu           sync.Mutex
	file         *os.File
	path         string
	size         int64
	maxSizeBytes int64
	maxFiles     int
	fallback     io.Writer
	// degraded is set after the first failed write, so the fallback is told once
	// what went wrong rather than once per record.
	degraded bool
	// absence says why there is no file to write to. "Closed" and "the reopen
	// failed" send records to the same fallback but mean opposite things to
	// whoever is reading it, and the fallback notice is the only place either
	// one is stated.
	absence error
}

// DirPerm and FilePerm keep the log as private as the data directory beside it.
// Log records are redacted before they arrive, but "redacted" is a property of
// the writer, not a licence for the file to be world-readable.
const (
	DirPerm  os.FileMode = 0o700
	FilePerm os.FileMode = 0o600
)

func OpenSink(options Options) (*Sink, error) {
	if options.Path == "" {
		return nil, errors.New("log file path is required")
	}
	if options.MaxSizeBytes <= 0 {
		return nil, errors.New("log file size limit is required")
	}
	if options.MaxFiles < 1 {
		return nil, errors.New("log file count must keep at least the current file")
	}
	if err := os.MkdirAll(filepath.Dir(options.Path), DirPerm); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(options.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FilePerm)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("measure log file: %w", err)
	}
	return &Sink{
		file: file, path: options.Path, size: info.Size(),
		maxSizeBytes: options.MaxSizeBytes, maxFiles: options.MaxFiles, fallback: options.Fallback,
	}, nil
}

func (s *Sink) Write(record []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		cause := s.absence
		if cause == nil {
			cause = errors.New("log sink is closed")
		}
		return s.writeFallback(record, cause)
	}
	// Rotate before the write rather than after it, so the limit bounds the file
	// that exists rather than the one that existed a record ago. An empty file is
	// never rotated: a record larger than the whole limit would otherwise rotate
	// on every write and destroy the history it was supposed to keep.
	if s.size > 0 && s.size+int64(len(record)) > s.maxSizeBytes {
		if err := s.rotate(); err != nil {
			return s.writeFallback(record, err)
		}
	}
	written, err := s.file.Write(record)
	s.size += int64(written)
	if err != nil {
		return s.writeFallback(record, err)
	}
	s.degraded = false
	return written, nil
}

// writeFallback keeps the record readable somewhere. The returned error is the
// original failure; slog discards handler errors, so the fallback write is what
// actually reaches the operator.
func (s *Sink) writeFallback(record []byte, cause error) (int, error) {
	if s.fallback == nil {
		return 0, cause
	}
	if !s.degraded {
		s.degraded = true
		fmt.Fprintf(s.fallback, "halro: log file %s is unavailable, writing to this stream instead: %v\n", s.path, cause)
	}
	written, err := s.fallback.Write(record)
	if err != nil {
		return written, cause
	}
	return written, cause
}

// rotate renames the current file down the generation ladder and starts a new
// one. It runs under the write lock.
func (s *Sink) rotate() error {
	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	// The oldest generation is removed first; every rename below it then has a
	// free slot to move into. Going the other way would overwrite the survivor
	// with its predecessor before it had been read.
	if s.maxFiles > 1 {
		if err := os.Remove(generationPath(s.path, s.maxFiles-1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for generation := s.maxFiles - 2; generation >= 1; generation-- {
			from, to := generationPath(s.path, generation), generationPath(s.path, generation+1)
			if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(s.path, generationPath(s.path, 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FilePerm)
	if err != nil {
		return err
	}
	s.file, s.size = file, 0
	return nil
}

// generationPath names a rotated file. Generation 0 is the live file.
func generationPath(path string, generation int) string {
	if generation <= 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, generation)
}

// Reopen closes the current file and opens Path again. It exists for the
// rotate-then-signal convention external tooling uses: logrotate renames the
// file out from under the process, which keeps writing to a descriptor pointing
// at a name nobody will read again until it is told to look at the path afresh.
//
// A failed reopen leaves the sink without a file rather than holding the stale
// descriptor: continuing to write into a renamed file is what the caller was
// trying to stop, and Write already falls back to stderr when there is no file.
// A failure to close the old descriptor is reported but does not abandon the
// reopen — the point of the call is to be writing to the path again, and the
// close is the part that has already stopped mattering.
func (s *Sink) Reopen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var closeErr error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			closeErr = fmt.Errorf("close log file: %w", err)
		}
		s.file = nil
	}
	fail := func(err error) error {
		s.absence = errors.Join(closeErr, err)
		return s.absence
	}
	if err := os.MkdirAll(filepath.Dir(s.path), DirPerm); err != nil {
		return fail(fmt.Errorf("create log directory: %w", err))
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FilePerm)
	if err != nil {
		return fail(fmt.Errorf("open log file: %w", err))
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fail(fmt.Errorf("measure log file: %w", err))
	}
	s.file, s.size, s.degraded, s.absence = file, info.Size(), false, nil
	return closeErr
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file, s.absence = nil, errors.New("log sink is closed")
	return err
}
