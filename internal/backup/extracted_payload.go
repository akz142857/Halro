package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// The receipt belongs to the actual exclusive output descriptor. It checks
// the final parent and name before and after IO; it does not claim ownership
// of ancestors or of the complete extraction tree.
func writeExtractedPayload(ctx context.Context, filename, name string, source io.Reader, phase func(*os.File) error) (actual File, resultErr error) {
	output, receipt, err := ownedfile.Create(filename, os.O_WRONLY, 0600)
	if err != nil {
		return File{}, err
	}
	defer func() {
		if closeErr := output.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, closeErr)
		}
		if resultErr != nil {
			actual = File{}
		}
	}()
	verify := func() error {
		if err := receipt.VerifyFile(output); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
		return ctx.Err()
	}
	if err := verify(); err != nil {
		return File{}, err
	}
	if phase != nil {
		if err := phase(output); err != nil {
			return File{}, err
		}
	}
	hash := sha256.New()
	var buffer [32 << 10]byte
	size, copyErr := io.CopyBuffer(io.MultiWriter(output, hash), backupContextReader{ctx: ctx, reader: source}, buffer[:])
	if err := errors.Join(copyErr, output.Sync(), verify()); err != nil {
		return File{}, err
	}
	return File{Path: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
