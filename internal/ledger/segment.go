package ledger

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// Sealing: how a write-ahead log that is the accounting authority stops growing
// without ceasing to be the accounting authority.
//
// The WAL is replayed from byte zero on every start to rebuild balances and
// reservations, so nothing in it can simply be deleted. What can happen is that
// the file stops being one file. A generation is rolled off whole — renamed
// aside, recorded in a manifest, and replaced by an empty successor whose first
// frame continues the ADR 0016 hash chain from where the rolled generation
// ended. Replay then walks the sealed generations in order and finishes in the
// active file, and reads exactly the records it would have read before.
//
// Rolling whole generations rather than splitting one at a byte boundary is the
// decision that keeps this tractable. A split would rebase the offsets of every
// frame after the cut, which would silently redirect any watermark already
// stored against them — and the usage checkpoint stores exactly such a
// watermark. A roll moves no live byte: a generation's offsets mean the same
// thing forever, whichever file holds them.

const (
	segmentManifestName    = "segments.json"
	segmentManifestVersion = 1
	segmentTempSuffix      = ".partial"
)

// ErrSegmentMissing says the manifest names a generation whose bytes are not on
// disk. It is fail-closed: a log that cannot produce its own history must not
// open and pretend the history was never there.
var ErrSegmentMissing = errors.New("ledger segment is missing")

// Segment is one sealed generation.
//
// PlainChecksum and Length describe the frames, StoredChecksum describes the
// file. They differ once a segment is compressed, and keeping both is what lets
// compaction be verified rather than trusted: the compressed file is read back
// and measured against the plaintext figures the roll recorded.
type Segment struct {
	Generation    uint64 `json:"generation"`
	File          string `json:"file"`
	Compressed    bool   `json:"compressed"`
	FirstSequence uint64 `json:"first_sequence"`
	LastSequence  uint64 `json:"last_sequence"`
	Length        int64  `json:"length"`
	StoredLength  int64  `json:"stored_length"`
	StartHash     string `json:"start_hash"`
	EndHash       string `json:"end_hash"`
	// EndEpoch records the highest authenticated frame epoch observed when the
	// generation was sealed. It is optional for manifests written before epoch
	// 5 existed; those generations can only have ended at epoch 4.
	EndEpoch       uint8     `json:"end_epoch,omitempty"`
	PlainChecksum  string    `json:"plain_checksum"`
	StoredChecksum string    `json:"stored_checksum"`
	SealedAt       time.Time `json:"sealed_at"`
}

func segmentManifestPath(directory string) string {
	return filepath.Join(directory, segmentManifestName)
}

// renamed distinguishes a failed preparation from an uncertain directory
// durability barrier. After rename, either manifest may survive a power loss;
// callers must retain files required by both instead of deleting the new one.
func saveSegmentManifestSourceWithSync(directory string, source segmentManifestSource, syncDirectory func(string) error) (renamed bool, err error) {
	return saveSegmentManifestSourceWithDisk(directory, source, syncDirectory, nil)
}

func saveSegmentManifestSourceWithDisk(directory string, source segmentManifestSource, syncDirectory func(string) error, reserve reserveNativeFileDisk) (renamed bool, err error) {
	var reservation NativeFileDiskReservation
	var envelope int64
	knownRetired := false
	if reserve != nil {
		counter := &nativeCountWriter{}
		if err := writeSegmentManifestSource(counter, source); err != nil {
			return false, err
		}
		var limitErr error
		envelope, limitErr = nativeFileEnvelope(counter.bytes)
		if limitErr != nil {
			return false, limitErr
		}
		reservation, err = reserve(envelope)
		if err != nil {
			return false, err
		}
	}
	// Registered first: every actual Close/retirement defer exits before this
	// refund decision. A parent Close failure cannot disappear behind refund.
	defer func() {
		if reservation != nil && !renamed && knownRetired && !errors.Is(err, ownedfile.ErrRetirement) && !errors.Is(err, ErrSegmentRetirement) {
			err = errors.Join(err, reservation.RetireKnown())
		} else if err != nil {
			err = keepNativeFileDiskDebt(reservation, err)
		}
	}()
	parentInfo, err := os.Lstat(directory)
	if err != nil || !parentInfo.IsDir() {
		return false, errors.Join(ownedfile.ErrReplaced, err)
	}
	parent, err := os.Open(directory)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(parent); closeErr != nil {
			err = errors.Join(err, ErrSegmentRetirement, closeErr)
		}
	}()
	actualParent, err := parent.Stat()
	if err != nil || !os.SameFile(parentInfo, actualParent) {
		return false, errors.Join(ownedfile.ErrReplaced, err)
	}
	checkParent := func() error {
		named, err := os.Lstat(directory)
		if err != nil || !os.SameFile(parentInfo, named) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		return nil
	}
	path := segmentManifestPath(directory)
	temporary := path + segmentTempSuffix
	previous, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if statErr == nil && !previous.Mode().IsRegular() {
		return false, ownedfile.ErrReplaced
	}
	file, owner, err := ownedfile.Create(temporary, os.O_WRONLY, 0600)
	if err != nil {
		return false, fmt.Errorf("write ledger segment manifest: %w", err)
	}
	closed := false
	closeUncertain := false
	defer func() {
		if !closed {
			closeErr := ownedfile.CloseFile(file)
			err = errors.Join(err, closeErr)
			if closeErr != nil {
				err = errors.Join(err, ErrSegmentRetirement)
				return
			} // Actual retirement unproven.
		}
		if closeUncertain {
			return
		}
		if !renamed {
			retireErr := owner.Retire()
			if retireErr == nil {
				retireErr = errors.Join(syncDirectory(directory), checkParent())
			}
			knownRetired = retireErr == nil
			err = errors.Join(err, retireErr)
		}
	}()
	var output io.Writer = file
	if reservation != nil {
		output = &nativeDiskWriter{output: file, limit: envelope}
	}
	if err := writeSegmentManifestSource(output, source); err != nil {
		return false, fmt.Errorf("write ledger segment manifest: %w", err)
	}
	if err := checkParent(); err != nil {
		return false, err
	}
	if err := owner.VerifyFile(file); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync ledger segment manifest: %w", err)
	}
	if reservation != nil {
		if err := verifyNativeFileEnvelope(file, envelope); err != nil {
			return false, err
		}
	}
	closeErr := ownedfile.CloseFile(file)
	closed = true // Close exactly once, including an uncertain Close failure.
	if closeErr != nil {
		// Do not unlink while the actual descriptor's retirement is uncertain.
		closeUncertain = true
		return false, errors.Join(ErrSegmentRetirement, fmt.Errorf("close ledger segment manifest: %w", closeErr))
	}
	if previous != nil {
		if err := owner.ReplaceCaptured(path, previous); err != nil {
			return false, err
		}
		renamed = true
	} else {
		alias, linkErr := owner.LinkCaptured(path)
		renamed = alias != nil
		if linkErr != nil {
			return renamed, linkErr
		}
		if err := owner.Retire(); err != nil {
			return true, err
		}
	}
	return true, errors.Join(syncDirectory(directory), checkParent())
}

type segmentManifestSource struct {
	each        func(func(Segment) error) error
	nilSegments bool
	pending     *Segment
}

func writeSegmentManifestSource(output io.Writer, source segmentManifestSource) error {
	buffer := bufio.NewWriterSize(output, replicationManifestValueLimit)
	if _, err := fmt.Fprintf(buffer, `{"version":%d,"segments":`, segmentManifestVersion); err != nil {
		return err
	}
	if source.nilSegments {
		if _, err := buffer.WriteString("null"); err != nil {
			return err
		}
	} else {
		if err := buffer.WriteByte('['); err != nil {
			return err
		}
		first := true
		if err := source.each(func(segment Segment) error {
			if !first {
				if err := buffer.WriteByte(','); err != nil {
					return err
				}
			}
			first = false
			return writeManifestSegment(buffer, segment)
		}); err != nil {
			return err
		}
		if err := buffer.WriteByte(']'); err != nil {
			return err
		}
	}
	if source.pending != nil {
		if _, err := buffer.WriteString(`,"pending":`); err != nil {
			return err
		}
		if err := writeManifestSegment(buffer, *source.pending); err != nil {
			return err
		}
	}
	if err := buffer.WriteByte('}'); err != nil {
		return err
	}
	return buffer.Flush()
}

func writeManifestSegment(output io.Writer, segment Segment) error {
	// Bound input strings before json.Marshal's escaping allocation. The
	// encoded descriptor is then checked against the exact reader limit.
	remaining := replicationManifestValueLimit
	for _, field := range []string{segment.File, segment.StartHash, segment.EndHash, segment.PlainChecksum, segment.StoredChecksum} {
		if len(field) > remaining {
			return fmt.Errorf("%w: segment descriptor exceeds manifest limit", ErrCorrupt)
		}
		remaining -= len(field)
	}
	encoded, err := json.Marshal(segment)
	if err != nil {
		return err
	}
	if len(encoded) > replicationManifestValueLimit {
		return fmt.Errorf("%w: encoded segment descriptor exceeds manifest limit", ErrCorrupt)
	}
	return writeFull(output, encoded)
}

// digestFilePrefix hashes the first length bytes of a file and returns the
// digest still open, so appends can continue into it. A file shorter than the
// prefix is a file the caller's own record disagrees with, which is corruption
// rather than something to hash around.
func digestFilePrefix(path string, length int64) (hash.Hash, error) {
	digest := sha256.New()
	if length == 0 {
		return digest, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	copied, err := io.Copy(digest, io.LimitReader(file, length))
	if err != nil {
		return nil, err
	}
	if copied != length {
		return nil, fmt.Errorf("%w: ledger is %d bytes but %d are committed", ErrCorrupt, copied, length)
	}
	return digest, nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	length, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), length, nil
}

// segmentReader gives scan a ReadSeeker over a segment whatever its storage.
//
// A compressed segment cannot seek, so the wrapper reopens the stream and
// discards forward. scan seeks exactly once, before the first frame, so the
// cost is paid at most once per replayed segment — and only for a replay that
// resumes inside a sealed generation, which happens after a crash, not on the
// hot path.
type segmentReader struct {
	open     func() (io.ReadCloser, error)
	reader   io.ReadCloser
	offset   int64
	closed   bool
	closeErr error
}

func openSegment(directory string, segment Segment) (*segmentReader, error) {
	return openSegmentWithStream(directory, segment, func(path string) (io.ReadCloser, error) { return os.Open(path) })
}

// A failed opener returns a stream only when its actual retirement is unknown.
// The private opener seam belongs to this reader, never to a global IO hook.
func openSegmentWithStream(directory string, segment Segment, openFile func(string) (io.ReadCloser, error)) (*segmentReader, error) {
	path := filepath.Join(directory, segment.File)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: generation %d (%s)", ErrSegmentMissing, segment.Generation, segment.File)
		}
		return nil, err
	}
	compressed := segment.Compressed
	open := func() (io.ReadCloser, error) {
		file, err := openFile(path)
		if err != nil {
			return file, err
		}
		var input io.Reader = file
		if actual, ok := file.(*os.File); ok {
			info, err := actual.Stat()
			if err != nil {
				closeErr := actual.Close()
				if closeErr != nil {
					return actual, errors.Join(err, closeErr, ownedfile.ErrRetirement)
				}
				return nil, err
			}
			input = boundedfilecache.NewReader(actual, info.Size())
		}
		if !compressed {
			return capturedSegmentReadCloser{Reader: input, Closer: file}, nil
		}
		decompressed, err := gzip.NewReader(input)
		if err != nil {
			openErr := fmt.Errorf("%w: generation %d is not readable: %v", ErrCorrupt, segment.Generation, err)
			closeErr := file.Close()
			if closeErr != nil {
				return file, errors.Join(openErr, ErrSegmentRetirement, ownedfile.ErrRetirement, closeErr)
			}
			return nil, openErr
		}
		return gzipReadCloser{Reader: decompressed, under: file}, nil
	}
	return &segmentReader{open: open}, nil
}

type capturedSegmentReadCloser struct {
	io.Reader
	io.Closer
}

type gzipReadCloser struct {
	*gzip.Reader
	under io.Closer
}

func (r gzipReadCloser) Close() error {
	err := errors.Join(r.Reader.Close(), r.under.Close())
	if err != nil {
		return errors.Join(ErrSegmentRetirement, err)
	}
	return nil
}

func (r *segmentReader) Read(buffer []byte) (int, error) {
	if r.closed {
		return 0, errors.Join(os.ErrClosed, r.closeErr)
	}
	if r.reader == nil {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}
	read, err := r.reader.Read(buffer)
	r.offset += int64(read)
	return read, err
}

func (r *segmentReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, errors.Join(os.ErrClosed, r.closeErr)
	}
	if whence != io.SeekStart {
		return 0, errors.New("ledger segments seek only from the start")
	}
	if offset < 0 {
		return 0, errors.New("ledger offset cannot be negative")
	}
	if r.reader != nil {
		if offset == r.offset {
			return offset, nil
		}
		if err := r.reader.Close(); err != nil {
			r.closed = true
			r.closeErr = errors.Join(ErrSegmentRetirement, err)
			return 0, r.closeErr
		}
		r.reader = nil
	}
	stream, err := r.open()
	if err != nil {
		if stream != nil || errors.Is(err, ErrSegmentRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			r.reader, r.closed = stream, true
			r.closeErr = errors.Join(ErrSegmentRetirement, err)
			return 0, r.closeErr
		}
		return 0, err
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, stream, offset); err != nil {
			if closeErr := stream.Close(); closeErr != nil {
				r.reader, r.closed = stream, true
				r.closeErr = errors.Join(ErrSegmentRetirement, closeErr)
			}
			return 0, errors.Join(err, r.closeErr)
		}
	}
	r.reader = stream
	r.offset = offset
	return offset, nil
}

func (r *segmentReader) Close() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	if r.reader == nil {
		return nil
	}
	if err := r.reader.Close(); err != nil {
		r.closeErr = errors.Join(ErrSegmentRetirement, err)
		return r.closeErr
	}
	r.reader = nil
	return nil
}

func verifyCompressedSegment(path, expectedChecksum string, expectedLength int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("compressed ledger segment is unreadable: %w", err)
	}
	defer reader.Close()
	digest := sha256.New()
	length, err := io.Copy(digest, reader)
	if err != nil {
		return fmt.Errorf("compressed ledger segment is unreadable: %w", err)
	}
	if length != expectedLength || hex.EncodeToString(digest.Sum(nil)) != expectedChecksum {
		return fmt.Errorf("compressed ledger segment does not reproduce its plaintext: %d bytes, want %d",
			length, expectedLength)
	}
	return nil
}

func encodeChainHash(hash [32]byte) string {
	if hash == [32]byte{} {
		return ""
	}
	return hex.EncodeToString(hash[:])
}

func decodeChainHash(value string) ([32]byte, error) {
	var hash [32]byte
	if value == "" {
		return hash, nil
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != len(hash) {
		return hash, fmt.Errorf("%w: ledger segment chain hash is malformed", ErrCorrupt)
	}
	copy(hash[:], raw)
	return hash, nil
}

// checkSegmentPresent refuses a manifest entry whose file is missing or has a
// different stored length. Full checksums are verified by the inspection path.
func checkSegmentPresent(directory string, segment Segment) error {
	info, err := os.Stat(filepath.Join(directory, segment.File))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: generation %d (%s)", ErrSegmentMissing, segment.Generation, segment.File)
	}
	if err != nil {
		return err
	}
	// StoredLength is zero on nothing this build writes; a manifest without
	// it is one this build did not produce.
	if segment.StoredLength <= 0 {
		return fmt.Errorf("%w: generation %d does not record its stored size",
			ErrCorrupt, segment.Generation)
	}
	if info.Size() != segment.StoredLength {
		return fmt.Errorf("%w: generation %d is %d bytes, manifest says %d",
			ErrCorrupt, segment.Generation, info.Size(), segment.StoredLength)
	}
	return nil
}

func segmentEndEpoch(segment Segment) byte {
	if segment.EndEpoch != 0 {
		return byte(segment.EndEpoch)
	}
	// EndEpoch was added with epoch 5. A non-empty historical segment without
	// it was therefore written by an epoch-4 binary.
	if segment.LastSequence != 0 {
		return frameVersionLedgerIntegrity
	}
	return 0
}
