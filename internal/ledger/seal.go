package ledger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SealResult describes one roll.
type SealResult struct {
	// Sealed is the generation that was rolled off. Zero means nothing was
	// eligible and the active file was left alone.
	Sealed  Segment
	Rolled  bool
	Active  uint64
	Bytes   int64
	Records uint64
}

// ActiveBytes is the size of the generation currently being written.
//
// Sealing triggers on bytes rather than on age because bytes are what runs the
// disk out. A day of history is a different number of bytes on every install.
func (l *Log) ActiveBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.offset
}

// Generation is the WAL generation being appended to.
func (l *Log) Generation() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generation
}

// Segments is the sealed history, oldest first.
func (l *Log) Segments() []Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Segment(nil), l.segments...)
}

// SealedThrough is the highest sequence that has already been rolled into a
// sealed generation. Nothing at or below it lives in the active file.
func (l *Log) SealedThrough() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startSequence
}

// Roll seals the active generation whole and starts an empty successor.
//
// It rolls the whole file rather than a prefix of it. A prefix seal would have
// to rebase the offsets of the frames left behind, and a watermark already
// stored against one of those frames — the usage checkpoint holds exactly such
// a watermark — would then name a different record than the one it was written
// for. Rolling whole generations leaves every offset meaning what it always
// meant, at the cost of sealing slightly less than the caller might like.
//
// The caller decides *when*: Roll enforces that the log is in a state where a
// roll is meaningful, not that the accounting derived from it has caught up.
// Those preconditions live where the derivatives are known.
func (l *Log) Roll() (SealResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return SealResult{}, errors.New("ledger is closed")
	}
	if l.status.Load() != AccountingHealthy {
		return SealResult{}, errors.New("accounting is not healthy")
	}
	// Without a key the in-memory chain head was never verified, so the end
	// hash a roll would record is not the chain's — it is zero. Sealing on that
	// basis would write a manifest that no later verification could satisfy.
	if len(l.chainKey) != 32 {
		return SealResult{}, errors.New("ledger requires a chain key to seal a generation")
	}
	if l.offset == 0 {
		return SealResult{Active: l.generation}, nil
	}
	if !l.chainSawFrames {
		return SealResult{}, errors.New("ledger cannot seal a generation with no authenticated frames")
	}
	if err := l.durability.Sync(); err != nil {
		l.status.MarkUnavailable()
		return SealResult{}, fmt.Errorf("sync ledger before sealing: %w", err)
	}

	checksum, length, err := hashFile(l.path)
	if err != nil {
		return SealResult{}, fmt.Errorf("checksum ledger before sealing: %w", err)
	}
	// The committed prefix is what gets sealed. A file longer than the log's
	// own offset means bytes this process did not commit, which recovery — not
	// sealing — is allowed to touch.
	if length != l.offset {
		return SealResult{}, fmt.Errorf("ledger is %d bytes but %d are committed; sealing needs a clean tail",
			length, l.offset)
	}

	pending := Segment{
		Generation:     l.generation,
		File:           fmt.Sprintf("ledger-%d.wal", l.generation),
		FirstSequence:  l.startSequence + 1,
		LastSequence:   l.sequence,
		Length:         l.offset,
		StoredLength:   l.offset,
		StartHash:      encodeChainHash(l.chainAnchor),
		EndHash:        encodeChainHash(l.chainHash),
		PlainChecksum:  checksum,
		StoredChecksum: checksum,
		SealedAt:       time.Now().UTC(),
	}
	rolled := filepath.Join(l.directory, pending.File)
	if _, err := os.Stat(rolled); err == nil {
		return SealResult{}, fmt.Errorf("ledger generation %d is already on disk", pending.Generation)
	} else if !errors.Is(err, os.ErrNotExist) {
		return SealResult{}, err
	}

	// Intent first. From here a crash is recoverable in both directions: the
	// rename below either happened or it did not, and repairSegments decides
	// which by looking for the rolled file.
	if err := saveSegmentManifest(l.directory, segmentManifest{Segments: l.segments, Pending: &pending}); err != nil {
		return SealResult{}, err
	}
	if err := os.Rename(l.path, rolled); err != nil {
		return SealResult{}, fmt.Errorf("seal ledger generation: %w", err)
	}
	if err := syncDirectory(l.directory); err != nil {
		return SealResult{}, err
	}
	// Past the rename the roll is committed; a failure from here leaves the
	// pending intent for the next open to finish, so the status is marked
	// unavailable rather than pretending the log is still writable.
	successor, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		l.status.MarkUnavailable()
		return SealResult{}, fmt.Errorf("open successor ledger generation: %w", err)
	}
	if err := successor.Sync(); err != nil {
		successor.Close()
		l.status.MarkUnavailable()
		return SealResult{}, fmt.Errorf("sync successor ledger generation: %w", err)
	}
	if err := syncDirectory(l.directory); err != nil {
		successor.Close()
		l.status.MarkUnavailable()
		return SealResult{}, err
	}
	segments := append(append([]Segment(nil), l.segments...), pending)
	if err := saveSegmentManifest(l.directory, segmentManifest{Segments: segments}); err != nil {
		successor.Close()
		l.status.MarkUnavailable()
		return SealResult{}, err
	}

	previous := l.file
	l.file = successor
	l.durability = successor
	if l.options.WrapDurability != nil {
		l.durability = l.options.WrapDurability(successor)
	}
	previous.Close()
	l.segments = segments
	l.generation = pending.Generation + 1
	l.startSequence = pending.LastSequence
	l.chainAnchor = l.chainHash
	l.offset = 0
	l.chainOffset = 0
	return SealResult{
		Sealed: pending, Rolled: true, Active: l.generation,
		Bytes: pending.Length, Records: pending.LastSequence - pending.FirstSequence + 1,
	}, nil
}

// Compact replaces a sealed generation's plain bytes with a compressed copy.
//
// It is separate from Roll on purpose. A roll has to happen while the writer is
// held, so it must be fast; compressing gigabytes is not. Splitting them also
// makes compaction restartable: it reads a file nothing will ever append to,
// and every step can be repeated after a crash without consulting anything but
// the files themselves.
func (l *Log) Compact(generation uint64) (Segment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	index := -1
	for position, segment := range l.segments {
		if segment.Generation == generation {
			index = position
			break
		}
	}
	if index < 0 {
		return Segment{}, fmt.Errorf("ledger generation %d is not sealed", generation)
	}
	segment := l.segments[index]
	if segment.Compressed {
		return segment, nil
	}
	source := filepath.Join(l.directory, segment.File)
	compressedName := fmt.Sprintf("ledger-%d.seg.gz", generation)
	destination := filepath.Join(l.directory, compressedName)
	stored, storedLength, err := compressSegmentFile(source, destination, segment.PlainChecksum, segment.Length)
	if err != nil {
		return Segment{}, fmt.Errorf("compress ledger generation %d: %w", generation, err)
	}

	updated := segment
	updated.File = compressedName
	updated.Compressed = true
	updated.StoredChecksum = stored
	updated.StoredLength = storedLength
	segments := append([]Segment(nil), l.segments...)
	segments[index] = updated
	// The manifest switch is the commit point. Before it the plain file is
	// still what the log reads and the compressed copy is an orphan; after it
	// the reverse, and removing the plain file is cleanup that may fail without
	// costing anything but disk.
	if err := saveSegmentManifest(l.directory, segmentManifest{Segments: segments}); err != nil {
		return Segment{}, err
	}
	l.segments = segments
	if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
		return updated, fmt.Errorf("remove compacted ledger generation %d: %w", generation, err)
	}
	if err := syncDirectory(l.directory); err != nil {
		return updated, err
	}
	return updated, nil
}

// VerifySegments authenticates sealed generations end to end, from `since`
// onward (zero for all of them).
//
// This is what makes `halro ledger verify` mean the same thing after sealing as
// before it. Verifying only the active file would answer a smaller question
// each time a generation rolled off, and would eventually answer nothing at
// all — the chain would be intact and unexamined.
func VerifySegments(directory string, key []byte, since uint64) ([]ChainReport, error) {
	segments, _, err := resolveSegments(directory)
	if err != nil {
		return nil, err
	}
	reports := make([]ChainReport, 0, len(segments))
	var previousEnd [32]byte
	var previousSequence uint64
	for _, segment := range segments {
		// Skipped generations still hand their chain head and sequence to the
		// next one, so a partial verification checks the same links a full one
		// would — it just does not re-read bytes it has already accepted.
		if segment.Generation < since {
			previousEnd, err = decodeChainHash(segment.EndHash)
			if err != nil {
				return nil, err
			}
			previousSequence = segment.LastSequence
			continue
		}
		start, err := decodeChainHash(segment.StartHash)
		if err != nil {
			return nil, err
		}
		if start != previousEnd {
			return nil, fmt.Errorf("%w: generation %d does not continue the chain of its predecessor",
				ErrTampered, segment.Generation)
		}
		stored, length, err := hashFile(filepath.Join(directory, segment.File))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: generation %d (%s)", ErrSegmentMissing, segment.Generation, segment.File)
		}
		if err != nil {
			return nil, err
		}
		if stored != segment.StoredChecksum {
			return nil, fmt.Errorf("%w: generation %d does not match its recorded checksum (%d bytes)",
				ErrCorrupt, segment.Generation, length)
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return nil, err
		}
		verifier := &chainVerifier{key: key, hash: start, sequence: previousSequence}
		report := ChainReport{}
		head, partial, scanErr := scan(reader, segment.Generation, 0, previousSequence, func(record Record) error {
			if record.Epoch == frameVersionLedgerIntegrity && verifier.verify() {
				report.Authenticated++
			} else {
				report.ChecksumOnly++
			}
			return nil
		}, verifier)
		closeErr := reader.Close()
		if err := errors.Join(scanErr, closeErr); err != nil {
			return nil, err
		}
		if partial {
			return nil, fmt.Errorf("%w: generation %d ends mid-frame", ErrCorrupt, segment.Generation)
		}
		if head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return nil, fmt.Errorf("%w: generation %d ends at %d/%d, manifest says %d/%d",
				ErrCorrupt, segment.Generation, head.Offset, head.Sequence, segment.Length, segment.LastSequence)
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return nil, err
		}
		if verifier.verify() && verifier.hash != end {
			return nil, fmt.Errorf("%w: generation %d does not end at its recorded chain head",
				ErrTampered, segment.Generation)
		}
		report.Head = head
		report.ChainSequence, report.ChainOffset, report.ChainHash, report.ChainVerified =
			verifier.sequence, verifier.offset, verifier.hash, verifier.sawFrames
		reports = append(reports, report)
		previousEnd, previousSequence = end, segment.LastSequence
	}
	return reports, nil
}

// VerifySealed authenticates this log's own sealed generations with the key it
// was opened with, so a caller never has to hold the chain key to check the
// archive. The key is loaded once at startup and wiped; keeping a second copy
// alive for the length of the process to re-verify a file would be a strictly
// worse trade than asking the log that already holds it.
func (l *Log) VerifySealed(since uint64) ([]ChainReport, error) {
	l.mu.Lock()
	directory, key := l.directory, append([]byte(nil), l.chainKey...)
	l.mu.Unlock()
	defer clear(key)
	return VerifySegments(directory, key, since)
}

// StageSegments copies the sealed history and its manifest beside a snapshot of
// the active file, so a backup archives a log that can still replay itself.
//
// A backup that took only the active file would restore an installation whose
// balances start at whatever the last roll left behind — silently, because the
// active file alone verifies perfectly well.
func (l *Log) StageSegments(directory string) ([]string, error) {
	l.mu.Lock()
	segments := append([]Segment(nil), l.segments...)
	source := l.directory
	l.mu.Unlock()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	staged := make([]string, 0, len(segments)+1)
	for _, segment := range segments {
		if err := copyFile(filepath.Join(source, segment.File), filepath.Join(directory, segment.File)); err != nil {
			return nil, fmt.Errorf("stage ledger generation %d: %w", segment.Generation, err)
		}
		staged = append(staged, segment.File)
	}
	if len(segments) == 0 {
		return staged, nil
	}
	if err := saveSegmentManifest(directory, segmentManifest{Segments: segments}); err != nil {
		return nil, err
	}
	return append(staged, segmentManifestName), nil
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		os.Remove(destination)
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		os.Remove(destination)
		return err
	}
	return output.Close()
}
