package ledger

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// A descriptor created after the common cut has no independent signed checksum.
// Authenticate its exact stored and plaintext bytes with fixed scratch before
// it may enter the selected directory. This reads the whole newly sealed file,
// including the old saved prefix, but never materializes historical records.
func (s *selectedNativeStartup) verifyRolledDescriptor(directory string, segment Segment) (resultErr error) {
	path := filepath.Join(directory, segment.File)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() {
		if err := ownedfile.CloseFile(file); err != nil {
			s.retainedFile = file
			s.retirementErr = errors.Join(s.retirementErr, err)
			resultErr = errors.Join(resultErr, err)
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != segment.StoredLength {
		return errors.Join(ErrProjectionSource, err)
	}
	check := func() error {
		actual, err := file.Stat()
		named, nameErr := os.Lstat(path)
		if err != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, actual) || !os.SameFile(actual, named) || actual.Size() != info.Size() || !actual.ModTime().Equal(info.ModTime()) {
			return errors.Join(ErrProjectionSource, err, nameErr)
		}
		return s.ctx.Err()
	}
	buffer := make([]byte, 32<<10)
	copyDigest := func(input io.Reader, ceiling int64) (string, int64, error) {
		digest := sha256.New()
		var total int64
		for {
			if err := check(); err != nil {
				return "", total, err
			}
			n, err := input.Read(buffer)
			if n > 0 {
				if int64(n) > ceiling-total {
					return "", total, ErrTampered
				}
				_, _ = digest.Write(buffer[:n])
				total += int64(n)
			}
			if errors.Is(err, io.EOF) {
				return hex.EncodeToString(digest.Sum(nil)), total, check()
			}
			if err != nil {
				return "", total, err
			}
			if n == 0 {
				return "", total, io.ErrNoProgress
			}
		}
	}
	stored, size, err := copyDigest(file, segment.StoredLength)
	if err != nil || size != segment.StoredLength || stored != segment.StoredChecksum {
		return errors.Join(ErrTampered, err)
	}
	plain, size := stored, size
	if segment.Compressed {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		gz, err := gzip.NewReader(file)
		if err != nil {
			return errors.Join(ErrTampered, err)
		}
		plain, size, err = copyDigest(gz, segment.Length)
		err = errors.Join(err, gz.Close())
		if err != nil {
			return errors.Join(ErrTampered, err)
		}
	}
	if size != segment.Length || plain != segment.PlainChecksum {
		return ErrTampered
	}
	return check()
}
