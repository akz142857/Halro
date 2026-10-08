// Package replicationrange holds a process-local, constant-space authenticated
// prefix cursor. Native stores supply their own frame decoder and validator.
// This cursor is neither a persistent checkpoint nor a whole-file audit.
package replicationrange

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"sync"
	"syscall"
)

// Position describes a prefix already authenticated by the native scanner.
type Position struct {
	Sequence uint64
	Offset   int64
	Hash     [32]byte
}

// Range brackets a contiguous run of authenticated native frames.
type Range struct {
	Before Position
	After  Position
}

// Scan authenticates from initial through last, returning the exact absolute
// byte range first..last. It must use the native decoder, stop at last, and
// refuse a missing or partial requested frame. The reader starts at initial's
// offset; its Seek(0) is relative to that suffix.
type Scan func(io.ReadSeeker, []byte, Position, uint64, uint64) (Range, error)

var ErrClosed = errors.New("replication range reader is closed")

// Reader serializes callers and keeps one position and file identity. Forward
// ranges authenticate each skipped/requested frame; backward ranges restart
// from the file's chain root. Immutable authenticated prefixes are trusted for
// this process lifetime, as they are by the native appenders. Reconciliation
// and whole-file audit remain separate responsibilities.
type Reader struct {
	mu          sync.Mutex
	path        string
	key         []byte
	maxBytes    int64
	scan        Scan
	position    Position
	identity    os.FileInfo
	closed      bool
	closeErr    error
	retiredFile *os.File
	closeFile   func(*os.File) error
}

func New(path string, key []byte, maxBytes int64, scan Scan) (*Reader, error) {
	if path == "" || len(key) != 32 || maxBytes <= 0 || int64(int(maxBytes)) != maxBytes || scan == nil {
		return nil, errors.New("replication range reader requires path, 32-byte key, byte limit and native scanner")
	}
	return &Reader{path: path, key: append([]byte(nil), key...), maxBytes: maxBytes, scan: scan}, nil
}

func (r *Reader) Read(first, last uint64) (frames []byte, readErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.Join(ErrClosed, r.closeErr)
	}
	if first == 0 || last < first {
		return nil, errors.New("replication range is invalid")
	}
	defer func() {
		if readErr != nil {
			r.reset()
		}
	}()
	file, err := os.OpenFile(r.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeFile := r.closeFile
		if closeFile == nil {
			closeFile = (*os.File).Close
		}
		if err := closeFile(file); err != nil {
			r.closeErr = errors.Join(ownedfile.ErrRetirement, err)
			r.retiredFile = file
			r.closed = true
			readErr = errors.Join(readErr, r.closeErr)
			frames = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("replication source must be a regular file")
	}
	initial := r.position
	if r.identity == nil || !os.SameFile(r.identity, info) || info.Size() < initial.Offset || first <= initial.Sequence {
		initial = Position{}
	}
	// Freeze the readable length, but never scan past the requested last frame.
	// A concurrent writer's later partial frame is outside this ordered range.
	suffix := io.NewSectionReader(file, initial.Offset, info.Size()-initial.Offset)
	span, err := r.scan(suffix, r.key, initial, first, last)
	if err != nil {
		return nil, err
	}
	start, end := span.Before.Offset, span.After.Offset
	if span.Before.Sequence != first-1 || span.After.Sequence != last || start < initial.Offset || end <= start || end > info.Size() {
		return nil, errors.New("native replication scanner returned an invalid range")
	}
	if end-start > r.maxBytes {
		return nil, fmt.Errorf("replication range exceeds %d bytes", r.maxBytes)
	}
	frames = make([]byte, int(end-start))
	if _, err := file.ReadAt(frames, start); err != nil {
		return nil, err
	}
	// Authenticate the actual returned copy as well. Disk bytes can change
	// between locating frame boundaries and ReadAt; do not cache the scanned
	// head or return bytes that have never passed the native decoder.
	verified, err := r.scan(bytes.NewReader(frames), r.key, span.Before, first, last)
	if err != nil {
		return nil, err
	}
	if verified != span {
		return nil, errors.New("replication source bytes changed after native authentication")
	}
	current, err := os.Stat(r.path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, current) || current.Size() < end {
		return nil, errors.New("replication source replaced or truncated during range read")
	}
	// Ordering reconstruction still verifies its frame digest before delivery;
	// the native cursor alone does not authorize these bytes for replication.
	r.position, r.identity = span.After, info
	return frames, nil
}

// Reset discards prefix authority after an explicit source-tail mutation.
// Source reconciliation must finish before concurrent range consumers start.
func (r *Reader) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reset()
}

func (r *Reader) reset() {
	r.position = Position{}
	r.identity = nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.closeErr != nil {
		return r.closeErr
	}
	clear(r.key)
	r.key = nil
	r.reset()
	return nil
}
