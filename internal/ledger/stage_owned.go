package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// Staging is a borrowed native read and one exclusive derivative creation.
// Source size and checksum come from the verified descriptor, not from the
// copied output. The caller owns the complete staging directory on success.
func copySealedSegmentContext(ctx context.Context, source, destination string, segment Segment) (resultErr error) {
	return copySealedSegmentWithDisk(ctx, source, destination, segment, nil)
}

func copySealedSegmentWithDisk(ctx context.Context, source, destination string, segment Segment, reserve reserveNativeFileDisk) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || segment.StoredLength <= 0 || info.Size() != segment.StoredLength {
		return errors.Join(ErrCorrupt, err)
	}
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	var output *os.File
	var owner *ownedfile.Owner
	var reservation NativeFileDiskReservation
	var envelope int64
	defer func() {
		cleanupErr := ownedfile.CloseFile(input)
		if output != nil {
			cleanupErr = errors.Join(cleanupErr, ownedfile.CloseFile(output))
		}
		if cleanupErr != nil {
			resultErr = keepNativeFileDiskDebt(reservation, errors.Join(resultErr, ErrSegmentRetirement, cleanupErr))
			return
		}
		if resultErr != nil && owner != nil {
			retireErr := owner.Retire()
			if retireErr == nil {
				retireErr = durable.SyncDirectory(filepath.Dir(destination))
			}
			if retireErr == nil && reservation != nil {
				retireErr = reservation.RetireKnown()
				if retireErr == nil {
					reservation = nil
				}
			}
			resultErr = errors.Join(resultErr, retireErr)
		} else if resultErr == nil && owner != nil {
			resultErr = errors.Join(durable.SyncDirectory(filepath.Dir(destination)), owner.VerifyName())
			if resultErr != nil {
				resultErr = errors.Join(ErrSegmentRetirement, resultErr)
			}
		}
		resultErr = keepNativeFileDiskDebt(reservation, resultErr)
	}()
	if err := unchangedSegmentDescriptor(input, info); err != nil {
		return err
	}
	if reserve != nil {
		envelope, err = nativeFileEnvelope(segment.StoredLength)
		if err != nil {
			return err
		}
		reservation, err = reserve(envelope)
		if err != nil {
			return err
		}
	}
	output, owner, err = ownedfile.Create(destination, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	digest := sha256.New()
	var writer io.Writer = output
	if reservation != nil {
		writer = &nativeDiskWriter{output: output, limit: envelope}
	}
	var buffer [32 << 10]byte
	count, copyErr := io.CopyBuffer(io.MultiWriter(writer, digest), io.LimitReader(segmentContextReader{ctx, input}, segment.StoredLength), buffer[:])
	if copyErr != nil {
		return copyErr
	}
	if count != segment.StoredLength || hex.EncodeToString(digest.Sum(nil)) != segment.StoredChecksum {
		return ErrTampered
	}
	if err := errors.Join(ctx.Err(), unchangedSegmentDescriptor(input, info), unchangedSegment(source, info), owner.VerifyFile(output)); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if reservation != nil {
		if err := verifyNativeFileEnvelope(output, envelope); err != nil {
			return err
		}
	}
	return errors.Join(ctx.Err(), owner.VerifyFile(output))
}
