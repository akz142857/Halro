package ledger

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

var ErrSegmentRetirement = errors.New("ledger segment physical retirement is uncertain")

type segmentContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r segmentContextReader) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(raw)
}

type compressedArtifact struct {
	checksum         string
	length           int64
	path, source     string
	info, sourceInfo os.FileInfo
	directoryInfo    os.FileInfo
	owner, temporary *ownedfile.Owner
	retirementErr    error
	diskReservation  NativeFileDiskReservation
}

func unchangedSegment(path string, expected os.FileInfo) error {
	actual, err := os.Lstat(path)
	if err != nil || expected == nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) || expected.Size() != actual.Size() || !expected.ModTime().Equal(actual.ModTime()) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}

func (a *compressedArtifact) check() error {
	if a == nil {
		return ownedfile.ErrReplaced
	}
	return errors.Join(a.retirementErr, a.checkDirectory(), unchangedSegment(a.path, a.info), unchangedSegment(a.source, a.sourceInfo))
}

func (a *compressedArtifact) checkDirectory() error {
	actual, err := os.Lstat(filepath.Dir(a.path))
	if err != nil || a.directoryInfo == nil || !actual.IsDir() || !os.SameFile(a.directoryInfo, actual) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}

func unchangedSegmentDescriptor(file *os.File, expected os.FileInfo) error {
	actual, err := file.Stat()
	if err != nil || expected == nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) || expected.Size() != actual.Size() || !expected.ModTime().Equal(actual.ModTime()) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}

// Existing verified orphans are borrowed representations, never cleanup
// authority. Only original creation receipts may remove this job's outputs.
func (a *compressedArtifact) discard() (resultErr error) {
	if a == nil {
		return nil
	}
	defer func() { resultErr = keepNativeFileDiskDebt(a.diskReservation, resultErr) }()
	if a.retirementErr != nil {
		return a.retirementErr
	}
	if err := a.checkDirectory(); err != nil {
		return err
	}
	var err error
	if a.temporary != nil {
		err = a.temporary.Retire()
	}
	if err == nil && a.owner != nil {
		err = a.owner.Retire()
	}
	if err == nil && (a.owner != nil || a.temporary != nil) {
		err = errors.Join(durable.SyncDirectory(filepath.Dir(a.path)), a.checkDirectory())
		if err == nil && a.diskReservation != nil {
			err = a.diskReservation.RetireKnown()
			if err == nil {
				a.diskReservation = nil
			}
		}
	}
	return err
}

func verifyCompressedReader(file *os.File, size int64, expectedChecksum string, expectedLength int64) (string, error) {
	return verifyCompressedReaderContext(context.Background(), file, size, expectedChecksum, expectedLength)
}

func verifyCompressedReaderContext(ctx context.Context, file *os.File, size int64, expectedChecksum string, expectedLength int64) (string, error) {
	reader, err := gzip.NewReader(segmentContextReader{ctx, boundedfilecache.NewReader(file, size)})
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	// A hostile gzip is not allowed to inflate beyond the independently
	// authenticated plaintext length merely to discover a checksum mismatch.
	length, copyErr := io.Copy(digest, io.LimitReader(reader, expectedLength+1))
	if err := errors.Join(copyErr, reader.Close()); err != nil {
		return "", err
	}
	if length != expectedLength || hex.EncodeToString(digest.Sum(nil)) != expectedChecksum {
		return "", ErrCorrupt
	}
	stored := sha256.New()
	if count, err := io.Copy(stored, segmentContextReader{ctx, boundedfilecache.NewReader(file, size)}); err != nil || count != size {
		return "", errors.Join(ErrCorrupt, err)
	}
	return hex.EncodeToString(stored.Sum(nil)), nil
}

func compressSegmentFileOwned(source, destination string, expectedChecksum string, expectedLength int64) (result *compressedArtifact, resultErr error) {
	return compressSegmentFileOwnedContext(context.Background(), source, destination, expectedChecksum, expectedLength)
}

func compressSegmentFileOwnedContext(ctx context.Context, source, destination string, expectedChecksum string, expectedLength int64) (result *compressedArtifact, resultErr error) {
	return compressSegmentFileOwnedWithDisk(ctx, source, destination, expectedChecksum, expectedLength, nil)
}

func compressSegmentFileOwnedWithDisk(ctx context.Context, source, destination string, expectedChecksum string, expectedLength int64, reserve reserveNativeFileDisk) (result *compressedArtifact, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expectedLength < 0 || expectedLength == int64(^uint64(0)>>1) || filepath.Dir(source) != filepath.Dir(destination) {
		return nil, ErrCorrupt
	}
	directoryInfo, err := os.Lstat(filepath.Dir(source))
	if err != nil || !directoryInfo.IsDir() {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != expectedLength {
		return nil, errors.Join(ErrCorrupt, err)
	}
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	inputClosed := false
	var output *os.File
	outputClosed := false
	artifact := &compressedArtifact{path: destination, source: source, sourceInfo: sourceInfo, directoryInfo: directoryInfo}
	defer func() {
		if !inputClosed {
			if err := ownedfile.CloseFile(input); err != nil {
				artifact.retirementErr = errors.Join(artifact.retirementErr, ErrSegmentRetirement, err)
				resultErr = errors.Join(resultErr, artifact.retirementErr)
			}
		}
		if output != nil && !outputClosed {
			if err := ownedfile.CloseFile(output); err != nil {
				artifact.retirementErr = errors.Join(artifact.retirementErr, ErrSegmentRetirement, err)
				resultErr = errors.Join(resultErr, artifact.retirementErr)
			}
		}
		if resultErr != nil {
			cleanupErr := artifact.discard()
			resultErr = errors.Join(resultErr, cleanupErr)
			if cleanupErr != nil {
				result = artifact
			} else {
				result = nil
			}
			resultErr = keepNativeFileDiskDebt(artifact.diskReservation, resultErr)
		}
	}()
	actualSource, err := input.Stat()
	if err != nil || !os.SameFile(sourceInfo, actualSource) || actualSource.Size() != expectedLength {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}

	// Reuse a prior complete representation only after authenticating all its
	// plaintext. It does not become owned just because this job can read it.
	if existing, err := os.Lstat(destination); err == nil {
		if !existing.Mode().IsRegular() {
			return nil, ownedfile.ErrReplaced
		}
		output, err = os.OpenFile(destination, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		actual, err := output.Stat()
		if err != nil || !os.SameFile(existing, actual) {
			return nil, errors.Join(ownedfile.ErrReplaced, err)
		}
		artifact.checksum, err = verifyCompressedReaderContext(ctx, output, actual.Size(), expectedChecksum, expectedLength)
		if err != nil {
			return nil, err
		}
		artifact.info, artifact.length = actual, actual.Size()
		if err := errors.Join(artifact.check(), unchangedSegmentDescriptor(input, sourceInfo), unchangedSegmentDescriptor(output, actual)); err != nil {
			return nil, err
		}
		inputErr, outputErr := ownedfile.CloseFile(input), ownedfile.CloseFile(output)
		inputClosed, outputClosed = true, true
		if err := errors.Join(inputErr, outputErr); err != nil {
			artifact.retirementErr = errors.Join(ErrSegmentRetirement, err)
			return artifact, artifact.retirementErr
		}
		return artifact, durable.SyncDirectory(filepath.Dir(destination))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	var envelope int64
	if reserve != nil {
		// BestSpeed may emit incompressible stored blocks. Allow more than
		// their overhead and bound every physical Write before source IO.
		if expectedLength > math.MaxInt64-(1<<20)-expectedLength/1024 {
			return nil, ErrNativeFileDiskLimit
		}
		envelope, err = nativeFileEnvelope(expectedLength + expectedLength/1024 + (1 << 20))
		if err != nil {
			return nil, err
		}
		artifact.diskReservation, err = reserve(envelope)
		if err != nil {
			return nil, err
		}
	}
	output, artifact.temporary, err = ownedfile.Create(destination+segmentTempSuffix, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var compressedOutput io.Writer = output
	if artifact.diskReservation != nil {
		compressedOutput = &nativeDiskWriter{output: output, limit: envelope}
	}
	writer, err := gzip.NewWriterLevel(compressedOutput, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	length, copyErr := io.Copy(writer, io.TeeReader(io.LimitReader(segmentContextReader{ctx, input}, expectedLength+1), digest))
	if err := errors.Join(copyErr, writer.Close()); err != nil {
		return nil, err
	}
	if length != expectedLength || hex.EncodeToString(digest.Sum(nil)) != expectedChecksum {
		return nil, ErrCorrupt
	}
	if err := errors.Join(artifact.checkDirectory(), unchangedSegment(source, sourceInfo), unchangedSegmentDescriptor(input, sourceInfo)); err != nil {
		return nil, err
	}
	if err := errors.Join(output.Sync(), artifact.temporary.VerifyFile(output)); err != nil {
		return nil, err
	}
	artifact.info, err = output.Stat()
	if err != nil {
		return nil, err
	}
	artifact.length = artifact.info.Size()
	if artifact.diskReservation != nil {
		if err := verifyNativeFileEnvelope(output, envelope); err != nil {
			return nil, err
		}
	}
	artifact.checksum, err = verifyCompressedReaderContext(ctx, output, artifact.length, expectedChecksum, expectedLength)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(artifact.checkDirectory(), artifact.temporary.VerifyFile(output), unchangedSegmentDescriptor(input, sourceInfo), unchangedSegmentDescriptor(output, artifact.info)); err != nil {
		return nil, err
	}
	inputErr, outputErr := ownedfile.CloseFile(input), ownedfile.CloseFile(output)
	inputClosed, outputClosed = true, true
	if err := errors.Join(inputErr, outputErr); err != nil {
		artifact.retirementErr = errors.Join(ErrSegmentRetirement, err)
		return artifact, artifact.retirementErr
	}
	artifact.owner, err = artifact.temporary.LinkCaptured(destination)
	if err != nil {
		return artifact, err
	}
	if err := artifact.temporary.Retire(); err != nil {
		return artifact, err
	}
	if err := artifact.check(); err != nil {
		return artifact, err
	}
	if err := durable.SyncDirectory(filepath.Dir(destination)); err != nil {
		return artifact, err
	}
	return artifact, nil
}
