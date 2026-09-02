package ledger

import (
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

	"github.com/akz142857/Halro/internal/durable"
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
	Generation     uint64    `json:"generation"`
	File           string    `json:"file"`
	Compressed     bool      `json:"compressed"`
	FirstSequence  uint64    `json:"first_sequence"`
	LastSequence   uint64    `json:"last_sequence"`
	Length         int64     `json:"length"`
	StoredLength   int64     `json:"stored_length"`
	StartHash      string    `json:"start_hash"`
	EndHash        string    `json:"end_hash"`
	PlainChecksum  string    `json:"plain_checksum"`
	StoredChecksum string    `json:"stored_checksum"`
	SealedAt       time.Time `json:"sealed_at"`
}

// segmentManifest is the durable record of which generations exist.
//
// Pending is the crash window made explicit. A roll writes the intent here
// before renaming the active file aside, and clears it after the successor is
// in place; an open that finds one decides which side of the rename it is on by
// looking for the rolled file, and finishes or abandons the roll accordingly.
// Without it, a crash mid-roll leaves a manifest and a set of files that
// disagree about what the active file contains, and the chain check would
// report tampering on a log nothing had tampered with.
type segmentManifest struct {
	Version  int       `json:"version"`
	Segments []Segment `json:"segments"`
	Pending  *Segment  `json:"pending,omitempty"`
}

func segmentManifestPath(directory string) string {
	return filepath.Join(directory, segmentManifestName)
}

func loadSegmentManifest(directory string) (segmentManifest, error) {
	raw, err := os.ReadFile(segmentManifestPath(directory))
	if errors.Is(err, os.ErrNotExist) {
		return segmentManifest{Version: segmentManifestVersion}, nil
	}
	if err != nil {
		return segmentManifest{}, fmt.Errorf("read ledger segment manifest: %w", err)
	}
	var manifest segmentManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return segmentManifest{}, fmt.Errorf("%w: ledger segment manifest is unreadable: %v", ErrCorrupt, err)
	}
	// An exact-equality gate here would refuse a directory this build can read
	// perfectly well the moment the manifest gains a field. A range accepts
	// everything up to what this build understands and refuses only what was
	// written by something newer.
	if manifest.Version < 1 || manifest.Version > segmentManifestVersion {
		return segmentManifest{}, fmt.Errorf("%w: ledger segment manifest version %d", ErrUnsupportedVersion, manifest.Version)
	}
	// Generations are numbered from one and never skipped, so the manifest has
	// to read as an unbroken run. Ordering alone was not enough: a manifest
	// edited to drop its oldest generations is still ordered, and every later
	// check — the presence check, the chain anchor, `ledger verify` — is
	// defined relative to what the manifest claims exists. A full replay would
	// then rebuild balances from a suffix of the history and report nothing
	// wrong, which is the one failure the accounting authority must not have.
	//
	// This is why moving old generations off the box is not yet a supported
	// operation, only a described one; taking them away needs a record of what
	// was taken, and there is not one.
	for position, segment := range manifest.Segments {
		if segment.Generation != uint64(position)+1 {
			return segmentManifest{}, fmt.Errorf(
				"%w: ledger segment %d is generation %d; the sealed history must run from 1 without gaps",
				ErrCorrupt, position, segment.Generation)
		}
	}
	if manifest.Pending != nil && manifest.Pending.Generation != uint64(len(manifest.Segments))+1 {
		return segmentManifest{}, fmt.Errorf(
			"%w: ledger roll intent names generation %d after %d sealed generations",
			ErrCorrupt, manifest.Pending.Generation, len(manifest.Segments))
	}
	return manifest, nil
}

func saveSegmentManifest(directory string, manifest segmentManifest) error {
	manifest.Version = segmentManifestVersion
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode ledger segment manifest: %w", err)
	}
	path := segmentManifestPath(directory)
	temporary := path + segmentTempSuffix
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write ledger segment manifest: %w", err)
	}
	if err := writeFull(file, encoded); err != nil {
		file.Close()
		os.Remove(temporary)
		return fmt.Errorf("write ledger segment manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporary)
		return fmt.Errorf("sync ledger segment manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("close ledger segment manifest: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("commit ledger segment manifest: %w", err)
	}
	return durable.SyncDirectory(directory)
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
	open   func() (io.ReadCloser, error)
	reader io.ReadCloser
	offset int64
}

func openSegment(directory string, segment Segment) (*segmentReader, error) {
	path := filepath.Join(directory, segment.File)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: generation %d (%s)", ErrSegmentMissing, segment.Generation, segment.File)
		}
		return nil, err
	}
	compressed := segment.Compressed
	open := func() (io.ReadCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if !compressed {
			return file, nil
		}
		decompressed, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("%w: generation %d is not readable: %v", ErrCorrupt, segment.Generation, err)
		}
		return gzipReadCloser{Reader: decompressed, under: file}, nil
	}
	return &segmentReader{open: open}, nil
}

type gzipReadCloser struct {
	*gzip.Reader
	under io.Closer
}

func (r gzipReadCloser) Close() error {
	err := r.Reader.Close()
	if closeErr := r.under.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (r *segmentReader) Read(buffer []byte) (int, error) {
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
		r.reader.Close()
		r.reader = nil
	}
	stream, err := r.open()
	if err != nil {
		return 0, err
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, stream, offset); err != nil {
			stream.Close()
			return 0, err
		}
	}
	r.reader = stream
	r.offset = offset
	return offset, nil
}

func (r *segmentReader) Close() error {
	if r.reader == nil {
		return nil
	}
	err := r.reader.Close()
	r.reader = nil
	return err
}

// compressSegmentFile writes source through gzip to destination and reads the
// result back, so the compressed copy is proven to reproduce the plaintext
// before anything points at it.
func compressSegmentFile(source, destination string, expectedChecksum string, expectedLength int64) (string, int64, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer input.Close()
	temporary := destination + segmentTempSuffix
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	// Fastest rather than best: the measured difference on real ledger frames
	// is a few percent of size for several times the CPU, and this runs on the
	// maintenance tick of a process that is also serving traffic.
	writer, err := gzip.NewWriterLevel(output, gzip.BestSpeed)
	if err != nil {
		output.Close()
		os.Remove(temporary)
		return "", 0, err
	}
	if _, err := io.Copy(writer, input); err != nil {
		writer.Close()
		output.Close()
		os.Remove(temporary)
		return "", 0, err
	}
	if err := writer.Close(); err != nil {
		output.Close()
		os.Remove(temporary)
		return "", 0, err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		os.Remove(temporary)
		return "", 0, err
	}
	if err := output.Close(); err != nil {
		os.Remove(temporary)
		return "", 0, err
	}
	if err := verifyCompressedSegment(temporary, expectedChecksum, expectedLength); err != nil {
		os.Remove(temporary)
		return "", 0, err
	}
	if err := os.Rename(temporary, destination); err != nil {
		os.Remove(temporary)
		return "", 0, err
	}
	if err := durable.SyncDirectory(filepath.Dir(destination)); err != nil {
		return "", 0, err
	}
	checksum, storedLength, err := hashFile(destination)
	if err != nil {
		return "", 0, err
	}
	return checksum, storedLength, nil
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

// resolveSegments decides what the sealed history is, including which side of
// an interrupted roll the directory is on, without writing anything.
//
// The decision is made from the files, not from a flag: a roll's rename either
// happened or it did not, and the rolled file's presence and checksum say
// which. Reading it this way means the offline inspection paths reach the same
// verdict as the opening one, from the same evidence.
func resolveSegments(directory string) ([]Segment, bool, error) {
	manifest, err := loadSegmentManifest(directory)
	if err != nil {
		return nil, false, err
	}
	if manifest.Pending == nil {
		return manifest.Segments, false, nil
	}
	pending := *manifest.Pending
	rolled := filepath.Join(directory, pending.File)
	checksum, length, err := hashFile(rolled)
	if errors.Is(err, os.ErrNotExist) {
		// The rename never ran, so the active file still holds this generation
		// whole. Abandoning the intent restores exactly the state before the
		// roll started.
		return manifest.Segments, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if length != pending.Length || checksum != pending.PlainChecksum {
		return nil, false, fmt.Errorf("%w: interrupted ledger roll left generation %d unreadable",
			ErrCorrupt, pending.Generation)
	}
	return append(manifest.Segments, pending), true, nil
}

// repairSegments resolves an interrupted roll and makes the verdict durable.
func repairSegments(directory string) ([]Segment, error) {
	segments, hadPending, err := resolveSegments(directory)
	if err != nil {
		return nil, err
	}
	if !hadPending {
		return segments, nil
	}
	if err := saveSegmentManifest(directory, segmentManifest{Segments: segments}); err != nil {
		return nil, err
	}
	return segments, nil
}

// checkSegmentsPresent refuses a directory whose manifest names history the
// files no longer hold.
//
// Existence and size, not checksums: this runs on every open, and re-hashing
// the whole archive at each start would make sealing cost more than it saves.
// What it catches is the failure that actually happens — a generation deleted
// or truncated by housekeeping — and it catches it at the open rather than
// letting the instance start with a history that is quietly shorter than the
// one its balances were computed from.
func checkSegmentsPresent(directory string, segments []Segment) error {
	for _, segment := range segments {
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
	}
	return nil
}

// activeGeneration is the generation the active WAL is writing, given the
// sealed history. Generation numbering starts at 1 and is never reused.
func activeGeneration(segments []Segment) uint64 {
	if len(segments) == 0 {
		return 1
	}
	return segments[len(segments)-1].Generation + 1
}

// sealedTail is where the active WAL picks up: the last sealed generation's
// chain head and sequence, or the zero value when nothing has been sealed.
func sealedTail(segments []Segment) (hash [32]byte, sequence uint64, err error) {
	if len(segments) == 0 {
		return hash, 0, nil
	}
	last := segments[len(segments)-1]
	hash, err = decodeChainHash(last.EndHash)
	if err != nil {
		return hash, 0, err
	}
	return hash, last.LastSequence, nil
}
