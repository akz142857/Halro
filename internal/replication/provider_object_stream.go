package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Per operation, independent of object size. Keep CopyBuffer's WriterTo and
// ReaderFrom shortcuts hidden so neither endpoint can allocate a whole object.
const providerObjectCopyBytes = 64 << 10

type providerObjectReader struct{ io.Reader }
type providerObjectWriter struct{ io.Writer }

// copyProviderObject authenticates the exact bytes copied before publication.
// Its limit prevents oversized/growing inputs from consuming unbounded IO.
func copyProviderObject(dst io.Writer, src io.Reader, length uint64, digest [sha256.Size]byte) error {
	if length > math.MaxInt64 {
		return errors.New("provider-object length exceeds file offset range")
	}
	hash := sha256.New()
	input := io.LimitReader(src, int64(length))
	written, err := io.CopyBuffer(providerObjectWriter{io.MultiWriter(dst, hash)}, providerObjectReader{input}, make([]byte, providerObjectCopyBytes))
	if err != nil {
		return err
	}
	if uint64(written) != length {
		return errors.New("provider-object length does not match receipt")
	}
	// Refuse even one extra byte without reading an oversized tail.
	var extra [1]byte
	if _, err := io.ReadFull(src, extra[:]); err != io.EOF {
		if err == nil {
			return errors.New("provider-object length exceeds receipt")
		}
		return err
	}
	if !equalBytes(hash.Sum(nil), digest[:]) {
		return errors.New("provider-object digest does not match receipt")
	}
	return nil
}

// Verify the opened descriptor and its current name around streaming validation.
// This is a bounded full object audit, not a cached proof or atomic disk snapshot.
func verifyProviderObjectFile(file *os.File, length uint64, digest [sha256.Size]byte) error {
	before, err := file.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || uint64(before.Size()) != length {
		return errors.New("provider-object file size or type does not match receipt")
	}
	// SectionReader bounds every read to the descriptor length captured above.
	if err := copyProviderObject(io.Discard, io.NewSectionReader(file, 0, before.Size()), length, digest); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Stat(file.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || !os.SameFile(before, named) || after.Size() != before.Size() || named.Size() != before.Size() {
		return fmt.Errorf("provider-object file changed during authentication")
	}
	return nil
}
