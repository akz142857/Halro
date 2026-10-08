package audit

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"syscall"
)

var ErrInspectionRetirement = errors.New("audit inspection descriptor retirement is unproven")

// VerifyWithVisitorContext is read-only: a partial tail is corruption, never
// repaired. Its complete source ceiling is checked before frame decoding. A
// callback's observations remain tentative until identity and actual Close.
func VerifyWithVisitorContext(ctx context.Context, path string, key []byte, maximumBytes int64, visit func(Record) error) (result Summary, resultErr error) {
	if ctx == nil || len(key) != auditHMACKeySize || maximumBytes < 0 {
		return Summary{}, errors.New("invalid audit inspection configuration")
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Summary{}, err
	}
	if !info.Mode().IsRegular() {
		return Summary{}, fmt.Errorf("%w: audit source is not a regular file", ErrCorrupt)
	}
	if maximumBytes > 0 && info.Size() > maximumBytes {
		return Summary{}, ErrJournalDiskLimit
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Summary{}, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrInspectionRetirement, ownedfile.ErrRetirement, err)
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = Summary{}
		}
	}()
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) || info.Size() != actual.Size() || !info.ModTime().Equal(actual.ModTime()) {
		return Summary{}, errors.Join(ErrCorrupt, err)
	}
	if maximumBytes > 0 && actual.Size() > maximumBytes {
		return Summary{}, ErrJournalDiskLimit
	}
	// Freeze the exact bytes observed; a later append also fails the final
	// source check rather than entering the requested complete-chain report.
	summary, partial, err := scanFromContext(ctx, io.NewSectionReader(file, 0, actual.Size()), key, Summary{}, visit)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	named, nameErr := os.Lstat(path)
	end, statErr := file.Stat()
	if nameErr != nil || statErr != nil || !os.SameFile(actual, named) || !os.SameFile(actual, end) || end.Size() != actual.Size() || !end.ModTime().Equal(actual.ModTime()) || named.Size() != actual.Size() || !named.ModTime().Equal(actual.ModTime()) {
		return Summary{}, errors.Join(ErrCorrupt, nameErr, statErr)
	}
	return summary, nil
}
