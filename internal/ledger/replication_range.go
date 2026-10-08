package ledger

import (
	"compress/gzip"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/replicationrange"
)

// ReplicationRangeReader keeps one authenticated prefix and at most one open
// stream. Forward ranges trust previously verified, append-only native history;
// they are not a fresh audit of that prefix. Reset/backward reads start at the
// root. Existing full-history inspection APIs retain their audit contract.
type ReplicationRangeReader struct {
	mu               sync.Mutex
	path             string
	key              []byte
	maxBytes         int
	closed           bool
	closeErr         error
	retirementErr    error
	retiredDirectory *segmentDirectoryIndex
	state            ledgerRangePosition
	stream           io.ReadSeekCloser
	info             os.FileInfo
	streamPath       string
	layout           replicationManifest
	directory        *segmentDirectoryIndex
}

type ledgerRangePosition struct {
	generation, sequence uint64
	offset               int64
	verifier             chainVerifier
}

var ledgerRangeComplete = errors.New("ledger range complete")

func NewReplicationRangeReader(path string, key []byte, maxBytes int) (*ReplicationRangeReader, error) {
	return NewReplicationRangeReaderWithIndexLimit(path, key, maxBytes, 0)
}

// NewReplicationRangeReaderWithIndexLimit applies a per-reader hard ceiling to
// its fixed-slot ordinal locator. Zero preserves the caller's legacy unmetered
// scratch behavior; shared owners should reserve their aggregate ceiling before
// constructing readers.
func NewReplicationRangeReaderWithIndexLimit(path string, key []byte, maxBytes int, maxIndexBytes int64) (*ReplicationRangeReader, error) {
	if path == "" || len(key) != 32 || maxBytes <= 0 {
		return nil, errors.New("ledger range reader requires a path, 32-byte key and positive output limit")
	}
	if maxIndexBytes < 0 {
		return nil, errors.New("ledger range reader index disk limit cannot be negative")
	}
	r := &ReplicationRangeReader{path: path, key: append([]byte(nil), key...), maxBytes: maxBytes}
	index, err := buildReaderSegmentDirectoryIndex(filepath.Dir(path), maxIndexBytes, r.key)
	if err != nil {
		clear(r.key)
		return nil, err
	}
	r.directory = index
	r.reset()
	return r, nil
}

func (r *ReplicationRangeReader) closeStream() error {
	if r.retirementErr != nil {
		return r.retirementErr
	}
	if r.stream != nil {
		if err := r.stream.Close(); err != nil {
			r.retirementErr = errors.Join(ownedfile.ErrRetirement, ErrSegmentRetirement, err)
			return r.retirementErr
		}
	}
	r.stream, r.info, r.streamPath = nil, nil, ""
	return nil
}

func (r *ReplicationRangeReader) reset() error {
	if err := r.closeStream(); err != nil {
		return err
	}
	r.layout = replicationManifest{}
	r.state = ledgerRangePosition{generation: 1, verifier: chainVerifier{key: r.key}}
	return nil
}

func (r *ReplicationRangeReader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reset()
}

func (r *ReplicationRangeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.closeErr = r.reset()
	if r.directory != nil {
		err := r.directory.Close()
		r.closeErr = errors.Join(r.closeErr, err)
		if err == nil {
			r.directory = nil
		}
	}
	if r.retiredDirectory != nil {
		r.closeErr = errors.Join(r.closeErr, r.retiredDirectory.Close())
	}
	if r.closeErr != nil {
		r.closeErr = errors.Join(ownedfile.ErrRetirement, r.closeErr)
	} else {
		clear(r.key)
	}
	return r.closeErr
}

func (r *ReplicationRangeReader) findGeneration(generation uint64) (replicationManifest, error) {
	if r.directory == nil {
		return replicationManifest{}, errSegmentDirectoryClosed
	}
	layout, err := r.directory.FindReplicationGeneration(filepath.Dir(r.path), generation)
	if err == nil {
		return layout, nil
	}
	if !errors.Is(err, errInspectionManifestStale) && !errors.Is(err, ErrTampered) {
		return replicationManifest{}, err
	}
	// The index is a rebuildable locator. A replaced or rewritten manifest
	// invalidates its pinned inode; rebuild once from the current authoritative
	// manifest, and let that complete parse decide whether the new state is valid.
	maxIndexBytes := r.directory.maxBytes
	next, rebuildErr := buildReaderSegmentDirectoryIndex(filepath.Dir(r.path), maxIndexBytes, r.key)
	if rebuildErr != nil {
		return replicationManifest{}, errors.Join(err, rebuildErr)
	}
	previous := r.directory
	r.directory = next
	if closeErr := previous.Close(); closeErr != nil {
		r.retiredDirectory = previous
		r.retirementErr = errors.Join(ownedfile.ErrRetirement, ErrSegmentRetirement, closeErr)
		return replicationManifest{}, errors.Join(err, r.retirementErr)
	}
	return next.FindReplicationGeneration(filepath.Dir(r.path), generation)
}

func (r *ReplicationRangeReader) Read(generation, first, last uint64) (output []byte, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.retirementErr != nil {
		return nil, errors.Join(replicationrange.ErrClosed, r.closeErr, r.retirementErr)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.reset())
			output = nil
		}
	}()
	if generation == 0 || first == 0 || last < first {
		return nil, errors.New("ledger replication range is invalid")
	}
	if generation < r.state.generation || first <= r.state.sequence {
		if err := r.reset(); err != nil {
			return nil, err
		}
	}
	// Re-resolve the current descriptor even on a hot read: Roll and Compact
	// may change its physical representation without changing native sequence.
	if r.stream != nil {
		layout, lookupErr := r.findGeneration(r.state.generation)
		if lookupErr != nil {
			return nil, lookupErr
		}
		info, statErr := os.Stat(r.streamPath)
		if statErr != nil || !os.SameFile(r.info, info) ||
			(!layout.sealed && info.Size() < r.state.offset) ||
			layout.sealed != r.layout.sealed || layout.segment != r.layout.segment {
			if err := r.reset(); err != nil {
				return nil, err
			}
		}
	}
	working := r.state // scanner failures must never publish half-advanced authority
	foundFirst, foundLast := false, false
	for working.generation <= generation {
		if r.stream == nil {
			if err := r.open(&working); err != nil {
				return nil, err
			}
		}
		var input io.ReadSeeker = r.stream
		if !r.layout.sealed {
			// Bound active reads to the size captured from the owned descriptor.
			info, err := r.stream.(*os.File).Stat()
			if err != nil {
				return nil, err
			}
			input = io.NewSectionReader(r.stream.(*os.File), 0, info.Size())
		}
		head, partial, scanErr := scanFromFrames(input, working.generation, working.offset, working.offset, working.sequence, nil,
			func(record Record, frame nativeFrameParts) error {
				// Old Roll records predecessor+1, even when a checksum-only
				// generation's first actual frame has a legacy sequence gap.
				if working.offset == 0 && r.layout.sealed && (record.Sequence < r.layout.segment.FirstSequence || authenticatedFrameEpoch(record.Epoch) && record.Sequence != r.layout.segment.FirstSequence) {
					return fmt.Errorf("%w: sealed first sequence mismatch", ErrCorrupt)
				}
				working.offset, working.sequence = record.Offset, record.Sequence
				if working.generation != generation {
					return nil
				}
				if record.Sequence == first {
					foundFirst = true
				}
				if record.Sequence >= first && record.Sequence <= last {
					if !foundFirst {
						return errors.New("ledger range first sequence is absent")
					}
					if frame.size() > r.maxBytes-len(output) {
						return errors.New("ledger replication range exceeds output limit")
					}
					output = append(output, frame.header...)
					output = append(output, frame.chainTail...)
					output = append(output, frame.payload...)
				}
				if record.Sequence == last {
					foundLast = true
					// At a declared sealed boundary also read EOF/trailer and check
					// the entire structural description before returning authority.
					if !r.layout.sealed || last < r.layout.segment.LastSequence {
						return ledgerRangeComplete
					}
				}
				if record.Sequence > last {
					return errors.New("ledger range last sequence is absent")
				}
				return nil
			}, &working.verifier)
		if scanErr != nil && !errors.Is(scanErr, ledgerRangeComplete) {
			return nil, scanErr
		}
		if errors.Is(scanErr, ledgerRangeComplete) {
			if err := r.validatePhysical(working.offset); err != nil {
				return nil, err
			}
			r.state = working
			return output, nil
		}
		if partial {
			return nil, fmt.Errorf("%w: ledger range ends mid-frame", ErrCorrupt)
		}
		working.offset, working.sequence = head.Offset, head.Sequence
		if err := r.validatePhysical(working.offset); err != nil {
			return nil, err
		}
		if !r.layout.sealed {
			return nil, errors.New("ledger replication range is outside durable history")
		}
		segment := r.layout.segment
		if head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return nil, fmt.Errorf("%w: sealed ledger length or last sequence mismatch", ErrCorrupt)
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return nil, err
		}
		if working.verifier.hash != end {
			return nil, fmt.Errorf("%w: sealed ledger end hash mismatch", ErrTampered)
		}
		if working.verifier.epoch == frameVersionRunAttribution && segment.EndEpoch != uint8(frameVersionRunAttribution) ||
			segment.EndEpoch != 0 && byte(segment.EndEpoch) != working.verifier.epoch {
			return nil, fmt.Errorf("%w: sealed ledger end epoch mismatch", ErrCorrupt)
		}
		if working.generation == generation {
			if !foundFirst || !foundLast {
				return nil, errors.New("ledger replication range is outside sealed history")
			}
			r.state = working
			return output, nil
		}
		if err := r.closeStream(); err != nil {
			return nil, err
		}
		working.generation++
		if working.generation == 0 {
			return nil, errors.New("ledger generation overflow")
		}
		working.offset, working.verifier.offset = 0, 0
	}
	return nil, errors.New("ledger replication generation is absent")
}

func (r *ReplicationRangeReader) open(position *ledgerRangePosition) error {
	layout, err := r.findGeneration(position.generation)
	if err != nil {
		return err
	}
	path := r.path
	if layout.sealed {
		if position.sequence == ^uint64(0) || layout.segment.FirstSequence != position.sequence+1 {
			return fmt.Errorf("%w: sealed ledger first sequence does not continue predecessor", ErrCorrupt)
		}
		start, err := decodeChainHash(layout.segment.StartHash)
		if err != nil {
			return err
		}
		if start != position.verifier.hash {
			return fmt.Errorf("%w: sealed ledger start hash mismatch", ErrTampered)
		}
		path = filepath.Join(filepath.Dir(r.path), layout.segment.File)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return r.closeUnpublished(file, path, err)
	}
	if !info.Mode().IsRegular() || layout.sealed && (layout.segment.StoredLength <= 0 || info.Size() != layout.segment.StoredLength) {
		return r.closeUnpublished(file, path, fmt.Errorf("%w: ledger range file length or type mismatch", ErrCorrupt))
	}
	var stream io.ReadSeekCloser = file
	if layout.sealed && layout.segment.Compressed {
		decompressed, err := gzip.NewReader(file)
		if err != nil {
			return r.closeUnpublished(file, path, fmt.Errorf("%w: ledger gzip header: %v", ErrCorrupt, err))
		}
		// Its current offset is reused; a backward read resets and opens a new
		// stream rather than seeking and decompressing the prefix for each frame.
		stream = &segmentReader{reader: gzipReadCloser{Reader: decompressed, under: file}}
	}
	r.stream, r.info, r.streamPath, r.layout = stream, info, path, layout
	return nil
}

func (r *ReplicationRangeReader) validatePhysical(end int64) error {
	info, err := os.Stat(r.streamPath)
	if err != nil {
		return err
	}
	if !os.SameFile(r.info, info) || r.layout.sealed && info.Size() != r.layout.segment.StoredLength ||
		!r.layout.sealed && info.Size() < end {
		return fmt.Errorf("%w: ledger range file changed during read", ErrCorrupt)
	}
	return nil
}

func (r *ReplicationRangeReader) closeUnpublished(file *os.File, path string, cause error) error {
	if err := file.Close(); err != nil {
		r.stream, r.streamPath = file, path
		r.retirementErr = errors.Join(ownedfile.ErrRetirement, ErrSegmentRetirement, err)
		return errors.Join(cause, r.retirementErr)
	}
	return cause
}
