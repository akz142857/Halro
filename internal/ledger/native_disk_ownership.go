package ledger

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// NativeFileDiskReservation covers one exclusively created auxiliary inode.
// RetireKnown is called only after all actual descriptors have closed, every
// creation-receipt name has retired, and the directory durability barrier has
// completed. Publication, replacement and unknown Close NEVER call it.
// The provider must retain published/uncertain charges in a bounded aggregate
// owner after this transient token is dropped, including Runtime Close.
type NativeFileDiskReservation interface {
	RetireKnown() error
}

type reserveNativeFileDisk func(int64) (NativeFileDiskReservation, error)

var ErrNativeFileDiskLimit = errors.New("native Ledger auxiliary file exceeds admitted capacity")

// Allocation rounding is conservative accounting, not physical preallocation
// or a filesystem block-size guarantee. The actual blocks are checked too.
func nativeFileEnvelope(bytes int64) (int64, error) {
	const quantum int64 = 64 << 10
	if bytes < 0 || bytes > math.MaxInt64-quantum {
		return 0, ErrNativeFileDiskLimit
	}
	return (bytes/quantum + 1) * quantum, nil
}

type nativeDiskWriter struct {
	output         io.Writer
	limit, written int64
}

func (w *nativeDiskWriter) Write(raw []byte) (int, error) {
	if int64(len(raw)) > w.limit-w.written {
		return 0, ErrNativeFileDiskLimit
	}
	n, err := w.output.Write(raw)
	w.written += int64(n)
	return n, err
}

type nativeCountWriter struct{ bytes int64 }

func (w *nativeCountWriter) Write(raw []byte) (int, error) {
	if int64(len(raw)) > math.MaxInt64-w.bytes {
		return 0, ErrNativeFileDiskLimit
	}
	w.bytes += int64(len(raw))
	return len(raw), nil
}

func verifyNativeFileEnvelope(file *os.File, limit int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 || stat.Blocks > math.MaxInt64/512 || info.Size() < 0 || info.Size() > limit || stat.Blocks*512 > limit {
		return ErrNativeFileDiskLimit
	}
	return nil
}

// Errors retain the transient token and actual descriptor/receipt errors.
// The producer latches the debt before permitting another structural job.
type nativeFileDiskDebt struct {
	reservation NativeFileDiskReservation
	cause       error
}

func (e *nativeFileDiskDebt) Error() string   { return e.cause.Error() }
func (e *nativeFileDiskDebt) Unwrap() []error { return []error{e.cause, ErrSegmentRetirement} }

func keepNativeFileDiskDebt(reservation NativeFileDiskReservation, err error) error {
	if err == nil || reservation == nil {
		return err
	}
	return &nativeFileDiskDebt{reservation: reservation, cause: err}
}

func openNativeActiveWithDisk(path string, flags int, reserve reserveNativeFileDisk) (*os.File, error) {
	if reserve == nil || flags&os.O_CREATE == 0 {
		return os.OpenFile(path, flags, 0600)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		return os.OpenFile(path, flags&^os.O_CREATE, 0600)
	}
	reservation, err := reserve(64 << 10)
	if err != nil {
		return nil, err
	}
	file, _, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, keepNativeFileDiskDebt(reservation, err)
	}
	if err := errors.Join(file.Sync(), durable.SyncDirectory(filepath.Dir(path))); err != nil {
		return nil, keepNativeFileDiskDebt(reservation, errors.Join(err, ownedfile.CloseFile(file)))
	}
	// This native authority remains named even when later scanning fails.
	// Never recycle its reservation on a source Open/Close error.
	return file, nil
}
