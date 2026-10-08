package ledger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// A per-owner lookup index over one immutable manifest inode. The authenticated
// sidecar may be reused across opens, but it never authorizes a saved projection
// or a pending Roll; the JSON manifest remains the authoritative directory.
// Slot: value offset/length, SHA-256(value), last sequence, compressed flag,
// reserved bytes, HMAC(generation, slot prefix). This is a volatile lookup
// hint only; the manifest descriptor remains authoritative.
const segmentDirectorySlotBytes = 96
const segmentDirectorySlotMACOffset = 64
const segmentDirectoryHeaderBytes = 96

type segmentDirectoryIndex struct {
	mu             sync.RWMutex
	manifest       *inspectionManifest
	index          *os.File
	indexInfo      os.FileInfo
	key            [32]byte
	parsed         parsedSegmentManifest
	summary        SegmentSummary
	maxBytes       int64
	slotBase       int64
	persistent     bool
	closed         atomic.Bool
	readPermit     chan struct{}
	owner          *ownedfile.Owner
	temporaryAlias *ownedfile.Owner
	parent         string
	parentInfo     os.FileInfo
	targetInfo     os.FileInfo
	closeErr       error
	ioClosed       bool
	diskToken      NativeFileDiskReservation
	diskEnvelope   int64
}

// scratchDirectory is caller-owned temporary storage, separate from the native
// directory. Nothing is exposed on failure, including a valid early prefix.
func buildSegmentDirectoryIndex(directory, scratchDirectory string) (result *segmentDirectoryIndex, err error) {
	return buildSegmentDirectoryIndexWithLimit(directory, scratchDirectory, 0)
}

func buildSegmentDirectoryIndexWithLimit(directory, scratchDirectory string, maxBytes int64) (result *segmentDirectoryIndex, err error) {
	return buildSegmentDirectoryIndexWithKey(directory, scratchDirectory, maxBytes, nil)
}

func buildSegmentDirectoryIndexWithKey(directory, scratchDirectory string, maxBytes int64, key []byte) (result *segmentDirectoryIndex, err error) {
	return buildSegmentDirectoryIndexInternal(directory, scratchDirectory, maxBytes, key, false)
}

func buildSegmentDirectoryIndexInternal(directory, scratchDirectory string, maxBytes int64, key []byte, persistent bool) (result *segmentDirectoryIndex, err error) {
	return buildSegmentDirectoryIndexInternalWithDisk(directory, scratchDirectory, maxBytes, key, persistent, nil)
}

func buildSegmentDirectoryIndexInternalWithDisk(directory, scratchDirectory string, maxBytes int64, key []byte, persistent bool, reserve reserveNativeFileDisk) (result *segmentDirectoryIndex, err error) {
	if maxBytes < 0 {
		return nil, errors.New("Ledger segment directory index disk limit cannot be negative")
	}
	m, err := openInspectionManifest(directory)
	if err != nil {
		return nil, err
	}
	d := &segmentDirectoryIndex{manifest: m, readPermit: make(chan struct{}, 1), maxBytes: maxBytes}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
		}
	}()
	if len(key) != 0 {
		if len(key) != 32 {
			return nil, errors.New("Ledger segment directory index key must be 32 bytes")
		}
		d.key = segmentDirectoryIndexKey(key)
	} else if _, err = rand.Read(d.key[:]); err != nil {
		return nil, err
	}
	indexDirectory, pattern := scratchDirectory, "halro-segment-directory-*.idx"
	if persistent && m.file != nil {
		indexDirectory, pattern = directory, segmentDirectoryIndexName+"-*.partial"
		d.slotBase = segmentDirectoryHeaderBytes
	}
	d.parent, err = filepath.Abs(indexDirectory)
	if err != nil {
		return nil, err
	}
	d.parentInfo, err = os.Lstat(d.parent)
	if err != nil || !d.parentInfo.IsDir() {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if persistent && m.file != nil {
		d.targetInfo, err = os.Lstat(filepath.Join(d.parent, segmentDirectoryIndexName))
		if errors.Is(err, os.ErrNotExist) {
			d.targetInfo, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
		if d.targetInfo != nil && !d.targetInfo.Mode().IsRegular() {
			return nil, ErrCorrupt
		}
	}
	if err := checkSegmentDirectoryIndexLimit(0, maxBytes); err != nil {
		return nil, err
	}
	if reserve != nil && m.file != nil {
		// Count the pinned source without collecting a historical descriptor
		// slice. A cached, matching sidecar never enters this rebuild path.
		var count uint64
		if m.file != nil {
			if _, err := parseSegmentManifestLocations(io.NewSectionReader(m.file, 0, m.info.Size()), func(_ Segment, _ int64, _ []byte) error {
				count++
				if count > uint64(math.MaxInt64-segmentDirectoryHeaderBytes)/segmentDirectorySlotBytes {
					return ErrNativeFileDiskLimit
				}
				return checkSegmentDirectoryIndexLimit(count, maxBytes)
			}); err != nil {
				return nil, err
			}
		}
		if err := m.check(); err != nil {
			return nil, err
		}
		d.diskEnvelope, err = nativeFileEnvelope(segmentDirectoryHeaderBytes + int64(count)*segmentDirectorySlotBytes)
		if err != nil {
			return nil, err
		}
		d.diskToken, err = reserve(d.diskEnvelope)
		if err != nil {
			return nil, err
		}
	}
	d.index, d.owner, err = ownedfile.CreateTemporary(indexDirectory, pattern)
	if err != nil {
		return nil, err
	}
	var writer io.Writer = d.index
	if d.diskToken != nil {
		writer = &nativeDiskWriter{output: d.index, limit: d.diskEnvelope}
	}
	if d.slotBase != 0 {
		if err = writeFull(writer, make([]byte, segmentDirectoryHeaderBytes)); err != nil {
			return nil, err
		}
	}
	d.indexInfo, err = d.index.Stat()
	if err != nil {
		return nil, err
	}
	if m.file == nil {
		d.parsed = parsedSegmentManifest{version: segmentManifestVersion}
		if err = m.check(); err != nil {
			return nil, err
		}
		return d, nil
	}
	var previousLastSequence uint64
	d.parsed, err = parseSegmentManifestLocations(io.NewSectionReader(m.file, 0, m.info.Size()), func(segment Segment, offset int64, raw []byte) error {
		if offset < 0 || int64(len(raw)) > m.info.Size()-offset {
			return fmt.Errorf("%w: manifest descriptor exceeds pinned source", ErrCorrupt)
		}
		if segment.FirstSequence == 0 || segment.LastSequence < segment.FirstSequence || segment.LastSequence <= previousLastSequence {
			return fmt.Errorf("%w: sealed segment sequence ranges are not strictly ordered", ErrCorrupt)
		}
		if segment.Generation > uint64(math.MaxInt64/segmentDirectorySlotBytes) {
			return fmt.Errorf("%w: directory index offset overflow", ErrCorrupt)
		}
		next, err := appendSegmentSummary(d.summary, segment)
		if err != nil {
			return err
		}
		if err := checkSegmentDirectoryIndexLimit(next.Generations, d.maxBytes); err != nil {
			return err
		}
		var slot [segmentDirectorySlotBytes]byte
		binary.BigEndian.PutUint64(slot[:8], uint64(offset))
		binary.BigEndian.PutUint64(slot[8:16], uint64(len(raw)))
		digest := sha256.Sum256(raw)
		copy(slot[16:48], digest[:])
		binary.BigEndian.PutUint64(slot[48:56], segment.LastSequence)
		if segment.Compressed {
			slot[56] = 1
		}
		mac := d.slotMAC(segment.Generation, slot[:segmentDirectorySlotMACOffset])
		copy(slot[segmentDirectorySlotMACOffset:], mac[:])
		if err := writeFull(writer, slot[:]); err != nil {
			return err
		}
		d.summary = next
		previousLastSequence = segment.LastSequence
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = m.check(); err != nil {
		return nil, err
	}
	if d.diskToken != nil {
		if err := verifyNativeFileEnvelope(d.index, d.diskEnvelope); err != nil {
			return nil, err
		}
	}
	if persistent && d.parsed.pending == nil && m.file != nil {
		if err = publishPersistentSegmentDirectoryIndex(directory, d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func checkSegmentDirectoryIndexLimit(count uint64, maxBytes int64) error {
	if maxBytes < 0 {
		return errors.New("Ledger segment directory index disk limit cannot be negative")
	}
	if count > uint64(math.MaxInt64/segmentDirectorySlotBytes) {
		return fmt.Errorf("%w: directory index size overflow", ErrCorrupt)
	}
	needed := uint64(segmentDirectoryHeaderBytes) + count*segmentDirectorySlotBytes
	if maxBytes > 0 && needed > uint64(maxBytes) {
		return fmt.Errorf("Ledger segment directory index needs %d bytes, exceeding configured %d-byte ceiling", needed, maxBytes)
	}
	return nil
}

func (d *segmentDirectoryIndex) slotMAC(generation uint64, prefix []byte) (result [32]byte) {
	return segmentDirectorySlotMAC(d.key, generation, prefix)
}

func segmentDirectorySlotMAC(key [32]byte, generation uint64, prefix []byte) (result [32]byte) {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("halro:volatile-segment-locator:v1"))
	var generationBytes [8]byte
	binary.BigEndian.PutUint64(generationBytes[:], generation)
	_, _ = mac.Write(generationBytes[:])
	_, _ = mac.Write(prefix)
	copy(result[:], mac.Sum(nil))
	return result
}

func (d *segmentDirectoryIndex) Get(generation uint64) (Segment, bool, error) {
	return d.getWithRead(generation, nil)
}

// FindSequenceCandidate performs a lower-bound search over authenticated,
// strictly increasing LastSequence hints. The caller resolves the one returned
// descriptor and checks its FirstSequence before treating the range as covered.
func (d *segmentDirectoryIndex) FindSequenceCandidate(sequence uint64) (uint64, bool, error) {
	if d.closed.Load() {
		return 0, false, errSegmentDirectoryClosed
	}
	select {
	case d.readPermit <- struct{}{}:
	default:
		return 0, false, errSegmentDirectoryBusy
	}
	d.mu.RLock()
	defer func() { <-d.readPermit; d.mu.RUnlock() }()
	if d.closed.Load() || d.index == nil {
		return 0, false, errSegmentDirectoryClosed
	}
	if err := d.manifest.check(); err != nil {
		return 0, false, err
	}
	if sequence == 0 || d.parsed.count == 0 {
		return 0, false, nil
	}
	readLast := func(generation uint64) (uint64, error) {
		var slot [segmentDirectorySlotBytes]byte
		offset := d.slotBase + int64(generation-1)*segmentDirectorySlotBytes
		if _, err := d.index.ReadAt(slot[:], offset); err != nil {
			return 0, fmt.Errorf("%w: directory locator unreadable: %v", ErrCorrupt, err)
		}
		mac := d.slotMAC(generation, slot[:segmentDirectorySlotMACOffset])
		if !hmac.Equal(mac[:], slot[segmentDirectorySlotMACOffset:]) {
			return 0, fmt.Errorf("%w: directory locator changed", ErrTampered)
		}
		if slot[56] > 1 {
			return 0, fmt.Errorf("%w: directory locator summary invalid", ErrCorrupt)
		}
		return binary.BigEndian.Uint64(slot[48:56]), nil
	}
	low, high := uint64(1), d.parsed.count+1
	for low < high {
		middle := low + (high-low)/2
		last, err := readLast(middle)
		if err != nil {
			return 0, false, err
		}
		if last >= sequence {
			high = middle
		} else {
			low = middle + 1
		}
	}
	if low > d.parsed.count {
		if err := d.manifest.check(); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	if _, err := readLast(low); err != nil {
		return 0, false, err
	}
	if err := d.manifest.check(); err != nil {
		return 0, false, err
	}
	return low, true, nil
}

var errSegmentDirectoryBusy = errors.New("segment directory read workspace is busy")
var errSegmentDirectoryClosed = errors.New("segment directory index is closed")

func (d *segmentDirectoryIndex) getWithRead(generation uint64, readAt func([]byte, int64) (int, error)) (Segment, bool, error) {
	if d.closed.Load() {
		return Segment{}, false, errSegmentDirectoryClosed
	}
	select {
	case d.readPermit <- struct{}{}:
	default:
		return Segment{}, false, errSegmentDirectoryBusy
	}
	d.mu.RLock()
	defer func() { <-d.readPermit; d.mu.RUnlock() }()
	if d.closed.Load() || d.index == nil {
		return Segment{}, false, errSegmentDirectoryClosed
	}
	if err := d.manifest.check(); err != nil {
		return Segment{}, false, err
	}
	if generation == 0 || generation > d.parsed.count {
		return Segment{}, false, nil
	}
	var slot [segmentDirectorySlotBytes]byte
	if _, err := d.index.ReadAt(slot[:], d.slotBase+int64(generation-1)*segmentDirectorySlotBytes); err != nil {
		return Segment{}, false, fmt.Errorf("%w: directory locator unreadable: %v", ErrCorrupt, err)
	}
	mac := d.slotMAC(generation, slot[:segmentDirectorySlotMACOffset])
	if !hmac.Equal(mac[:], slot[segmentDirectorySlotMACOffset:]) {
		return Segment{}, false, fmt.Errorf("%w: directory locator changed", ErrTampered)
	}
	offset, length := binary.BigEndian.Uint64(slot[:8]), binary.BigEndian.Uint64(slot[8:16])
	if offset > uint64(d.manifest.info.Size()) || length == 0 || length > replicationManifestValueLimit || length > uint64(d.manifest.info.Size())-offset {
		return Segment{}, false, fmt.Errorf("%w: directory locator span invalid", ErrCorrupt)
	}
	raw := make([]byte, int(length))
	if readAt == nil {
		readAt = d.manifest.file.ReadAt
	}
	if _, err := readAt(raw, int64(offset)); err != nil {
		return Segment{}, false, fmt.Errorf("%w: indexed descriptor unreadable: %v", ErrCorrupt, err)
	}
	digest := sha256.Sum256(raw)
	if !hmac.Equal(digest[:], slot[16:48]) {
		return Segment{}, false, fmt.Errorf("%w: indexed descriptor changed", ErrTampered)
	}
	var segment Segment
	if err := json.Unmarshal(raw, &segment); err != nil || segment.Generation != generation {
		return Segment{}, false, fmt.Errorf("%w: indexed descriptor invalid", ErrCorrupt)
	}
	if segment.LastSequence != binary.BigEndian.Uint64(slot[48:56]) || segment.Compressed != (slot[56] == 1) || slot[56] > 1 {
		return Segment{}, false, fmt.Errorf("%w: indexed descriptor summary changed", ErrTampered)
	}
	if err := d.manifest.check(); err != nil {
		return Segment{}, false, err
	}
	return segment, true, nil
}

// FindFirstUncompressedThrough reads only fixed-size authenticated locator
// slots until it finds one eligible candidate. It avoids decoding every JSON
// descriptor on a maintenance selection pass. The caller reopens the returned
// descriptor with Get before using it, so this does not grant deletion or
// retention authority.
func (d *segmentDirectoryIndex) FindFirstUncompressedThrough(through uint64) (uint64, bool, error) {
	if d.closed.Load() {
		return 0, false, errSegmentDirectoryClosed
	}
	select {
	case d.readPermit <- struct{}{}:
	default:
		return 0, false, errSegmentDirectoryBusy
	}
	d.mu.RLock()
	defer func() { <-d.readPermit; d.mu.RUnlock() }()
	if d.closed.Load() || d.index == nil {
		return 0, false, errSegmentDirectoryClosed
	}
	if err := d.manifest.check(); err != nil {
		return 0, false, err
	}
	var slot [segmentDirectorySlotBytes]byte
	for generation := uint64(1); generation <= d.parsed.count; generation++ {
		offset := d.slotBase + int64(generation-1)*segmentDirectorySlotBytes
		if _, err := d.index.ReadAt(slot[:], offset); err != nil {
			return 0, false, fmt.Errorf("%w: directory locator unreadable: %v", ErrCorrupt, err)
		}
		mac := d.slotMAC(generation, slot[:segmentDirectorySlotMACOffset])
		if !hmac.Equal(mac[:], slot[segmentDirectorySlotMACOffset:]) {
			return 0, false, fmt.Errorf("%w: directory locator changed", ErrTampered)
		}
		if slot[56] > 1 {
			return 0, false, fmt.Errorf("%w: directory locator summary invalid", ErrCorrupt)
		}
		lastSequence := binary.BigEndian.Uint64(slot[48:56])
		if slot[56] == 0 && lastSequence <= through {
			if err := d.manifest.check(); err != nil {
				return 0, false, err
			}
			return generation, true, nil
		}
	}
	if err := d.manifest.check(); err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

// FindReplicationGeneration resolves the active/sealed layout from this pinned
// directory snapshot. It has the same interrupted-Roll interpretation as the
// streaming compatibility reader while reusing the owner's ordinal locator.
func (d *segmentDirectoryIndex) FindReplicationGeneration(directory string, generation uint64) (replicationManifest, error) {
	if d.closed.Load() {
		return replicationManifest{}, errSegmentDirectoryClosed
	}
	if err := d.manifest.check(); err != nil {
		return replicationManifest{}, err
	}
	result := replicationManifest{segment: d.parsed.last, count: d.parsed.count, active: d.parsed.count + 1}
	if result.active == 0 {
		return replicationManifest{}, fmt.Errorf("%w: generation overflow", ErrCorrupt)
	}
	var pending *Segment
	if d.parsed.pending != nil {
		intent := *d.parsed.pending
		checksum, length, err := hashFile(filepath.Join(directory, intent.File))
		if !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return replicationManifest{}, err
			}
			if length != intent.Length || checksum != intent.PlainChecksum {
				return replicationManifest{}, fmt.Errorf("%w: interrupted roll generation is unreadable", ErrCorrupt)
			}
			pending = &intent
			result.count++
			result.active++
			if result.active == 0 {
				return replicationManifest{}, fmt.Errorf("%w: generation overflow", ErrCorrupt)
			}
		}
	}
	if generation > 0 && generation <= d.parsed.count {
		segment, found, err := d.Get(generation)
		if err != nil {
			return replicationManifest{}, err
		}
		if !found {
			return replicationManifest{}, errors.New("ledger generation is not present")
		}
		result.segment, result.sealed = segment, true
	} else if pending != nil && generation == result.count {
		result.segment, result.sealed = *pending, true
	} else if generation != result.active {
		return replicationManifest{}, errors.New("ledger generation is not present")
	}
	if err := d.manifest.check(); err != nil {
		return replicationManifest{}, err
	}
	return result, nil
}

// Close joins all accepted Get calls before clearing key/closing descriptors.
// Ephemeral locators are removed; the authenticated persistent sidecar remains
// as a rebuildable cache for the next owner.
func (d *segmentDirectoryIndex) Close() error {
	if d == nil {
		return nil
	}
	d.closed.Store(true)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ioClosed {
		return d.closeErr
	}
	d.ioClosed = true
	var actualCloseErr error
	if d.manifest != nil {
		actualCloseErr = d.manifest.close()
		if actualCloseErr == nil {
			d.manifest = nil
		}
	}
	if d.index != nil {
		closeErr := ownedfile.CloseFile(d.index)
		actualCloseErr = errors.Join(actualCloseErr, closeErr)
		if closeErr == nil {
			d.index = nil
		}
	}
	if actualCloseErr != nil {
		d.closeErr = keepNativeFileDiskDebt(d.diskToken, errors.Join(d.closeErr, ErrSegmentRetirement, actualCloseErr))
		return d.closeErr
	}
	if errors.Is(d.closeErr, ErrSegmentRetirement) {
		return d.closeErr
	}
	receipt := d.owner
	if d.persistent {
		receipt = d.temporaryAlias
	}
	if receipt == nil {
		clear(d.key[:])
		d.parsed = parsedSegmentManifest{}
		d.summary = SegmentSummary{}
		return d.closeErr
	}
	if err := d.checkParent(); err != nil {
		d.closeErr = keepNativeFileDiskDebt(d.diskToken, errors.Join(d.closeErr, ErrSegmentRetirement, err))
		return d.closeErr
	}
	if err := receipt.Retire(); err != nil {
		d.closeErr = keepNativeFileDiskDebt(d.diskToken, errors.Join(d.closeErr, ErrSegmentRetirement, err))
		return d.closeErr
	}
	if err := durable.SyncDirectory(d.parent); err != nil {
		d.closeErr = keepNativeFileDiskDebt(d.diskToken, errors.Join(d.closeErr, ErrSegmentRetirement, err))
	}
	if d.closeErr == nil && !d.persistent && d.diskToken != nil {
		if err := d.checkParent(); err != nil {
			d.closeErr = keepNativeFileDiskDebt(d.diskToken, errors.Join(ErrSegmentRetirement, err))
		} else {
			d.closeErr = d.diskToken.RetireKnown()
			if d.closeErr == nil {
				d.diskToken = nil
			}
		}
	}
	if d.closeErr == nil {
		clear(d.key[:])
		d.parsed = parsedSegmentManifest{}
		d.summary = SegmentSummary{}
	}
	return d.closeErr
}

func (d *segmentDirectoryIndex) checkParent() error {
	current, err := os.Lstat(d.parent)
	if err != nil || d.parentInfo == nil || !current.IsDir() || !os.SameFile(d.parentInfo, current) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}
