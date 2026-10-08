package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
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
	finish, err := l.beginMutation(nil)
	if err != nil {
		return SealResult{}, err
	}
	defer finish()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.options.Replica {
		return SealResult{}, errors.New("ledger is replica-owned and refuses local roll")
	}
	if l.options.BeforeRoll != nil {
		if err := l.options.BeforeRoll(l.generation, l.sequence); err != nil {
			return SealResult{}, fmt.Errorf("confirm ledger generation before roll: %w", err)
		}
	}
	result, err := l.rollLocked(time.Now().UTC(), nil)
	if err != nil {
		return result, err
	}
	if result.Rolled && l.options.AfterRoll != nil {
		if err := l.options.AfterRoll(result.Sealed); err != nil {
			l.status.MarkUnavailable()
			return result, fmt.Errorf("record durable ledger roll for replication: %w", err)
		}
	}
	return result, nil
}

// ApplyReplicatedRoll verifies the Primary's complete authoritative Segment
// description before publishing the local rename and manifest. Compression
// fields and filenames remain node-local.
func (l *Log) ApplyReplicatedRoll(expected Segment) (SealResult, error) {
	finish, err := l.beginMutation(nil)
	if err != nil {
		return SealResult{}, err
	}
	defer finish()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.options.Replica {
		return SealResult{}, errors.New("replicated ledger roll requires replica mode")
	}
	return l.rollLocked(expected.SealedAt.UTC(), &expected)
}

func (l *Log) rollLocked(sealedAt time.Time, expected *Segment) (result SealResult, resultErr error) {
	if physical := l.physical.Load(); physical != nil && (physical.debt != 0 || physical.active != 0) {
		return SealResult{}, resourcebudget.ErrPhysicalAllocation
	}
	if !l.selectedStartupStructuralAllowedLocked() {
		return SealResult{}, ErrStartupAuditPending
	}
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
		if expected != nil {
			if expected.Generation+1 != l.generation {
				return SealResult{}, errors.New("replicated ledger Roll does not name the previously sealed generation")
			}
			sealed, found, err := l.lookupSegmentLocked(expected.Generation)
			if err != nil {
				return SealResult{}, err
			}
			if !found || !sameAuthoritativeSegment(sealed, *expected) {
				return SealResult{}, errors.New("replicated ledger Roll does not match sealed history")
			}
			return SealResult{
				Sealed: sealed, Active: l.generation, Bytes: sealed.Length,
				Records: sealed.LastSequence - sealed.FirstSequence + 1,
			}, nil
		}
		return SealResult{Active: l.generation}, nil
	}
	if !l.chainSawFrames {
		return SealResult{}, errors.New("ledger cannot seal a generation with no authenticated frames")
	}
	if err := l.durability.Sync(); err != nil {
		l.status.MarkUnavailable()
		return SealResult{}, fmt.Errorf("sync ledger before sealing: %w", err)
	}

	// The committed prefix is what gets sealed. A file longer than the log's
	// own offset means bytes this process did not commit, which recovery — not
	// sealing — is allowed to touch. Stat rather than a full read: the
	// checksum below comes from the digest the writer has been maintaining as
	// it appended, so the only thing left to establish here is that nothing
	// else has grown the file behind us.
	info, err := l.file.Stat()
	if err != nil {
		return SealResult{}, fmt.Errorf("stat ledger before sealing: %w", err)
	}
	named, nameErr := os.Lstat(l.path)
	parent, parentErr := os.Lstat(l.directory)
	if nameErr != nil || parentErr != nil || !info.Mode().IsRegular() || !parent.IsDir() || !named.Mode().IsRegular() || !os.SameFile(info, named) || l.directoryInfo == nil || !os.SameFile(parent, l.directoryInfo) {
		l.status.RequireRecovery()
		return SealResult{}, errors.Join(ErrSegmentRetirement, ownedfile.ErrReplaced, nameErr, parentErr)
	}
	if err := l.segmentPhase("roll-native-synced"); err != nil {
		return SealResult{}, err
	}
	if info.Size() != l.offset {
		return SealResult{}, fmt.Errorf("ledger is %d bytes but %d are committed; sealing needs a clean tail",
			info.Size(), l.offset)
	}
	if l.plainDigest == nil {
		// Selected startup authenticates only the suffix. Its first Roll pays
		// this bounded-memory full active checksum pass; subsequent rolls use
		// the ordinary rolling digest again.
		credit, acquireErr := l.startupBudget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10})
		if acquireErr != nil {
			return SealResult{}, acquireErr
		}
		digest := sha256.New()
		copied, readErr := io.CopyBuffer(digest, io.NewSectionReader(l.file, 0, l.offset), make([]byte, 64<<10))
		current, statErr := l.file.Stat()
		named, namedErr := os.Lstat(l.path)
		parent, parentErr := os.Lstat(l.directory)
		err = errors.Join(readErr, statErr, namedErr, parentErr)
		if err == nil && (copied != l.offset || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) || !os.SameFile(current, named) || !named.Mode().IsRegular() || !os.SameFile(parent, l.directoryInfo)) {
			err = ErrProjectionSource
		}
		releaseErr := credit.Release()
		if releaseErr != nil {
			retainSelectedStartupCleanup(&l, nil, nil, credit, releaseErr)
		}
		if err = errors.Join(err, releaseErr); err != nil {
			l.status.RequireRecovery()
			return SealResult{}, err
		}
		l.plainDigest = digest
	}
	checksum := hex.EncodeToString(l.plainDigest.Sum(nil))

	pending := Segment{
		Generation:     l.generation,
		File:           fmt.Sprintf("ledger-%d.wal", l.generation),
		FirstSequence:  l.startSequence + 1,
		LastSequence:   l.sequence,
		Length:         l.offset,
		StoredLength:   l.offset,
		StartHash:      encodeChainHash(l.chainAnchor),
		EndHash:        encodeChainHash(l.chainHash),
		EndEpoch:       uint8(l.chainEpoch),
		PlainChecksum:  checksum,
		StoredChecksum: checksum,
		SealedAt:       sealedAt,
	}
	if expected != nil && !sameAuthoritativeSegment(pending, *expected) {
		return SealResult{}, errors.New("replicated ledger Roll does not match the active generation")
	}
	nextSummary, err := appendSegmentSummary(l.segmentSummary, pending)
	if err != nil {
		return SealResult{}, err
	}
	if err := checkSegmentDirectoryIndexLimit(l.segmentDirectory.parsed.count+1, l.options.MaxSegmentDirectoryIndexBytes); err != nil {
		return SealResult{}, err
	}
	rolled := filepath.Join(l.directory, pending.File)
	if _, err := os.Lstat(rolled); err == nil {
		return SealResult{}, fmt.Errorf("ledger generation %d is already on disk", pending.Generation)
	} else if !errors.Is(err, os.ErrNotExist) {
		return SealResult{}, err
	}
	if err := l.preparePhysicalDerivedLocked(context.Background(), PhysicalDerivedRoll, 1); err != nil {
		return SealResult{}, err
	}
	if err := l.retirePhysicalRollTailLocked(); err != nil {
		return SealResult{}, err
	}
	if l.physical.Load() != nil {
		// Punching only our unused tail changes mtime. Capture that actual
		// operation's resulting metadata without replacing its inode/EOF grant.
		refreshed, statErr := l.file.Stat()
		if statErr != nil || !os.SameFile(info, refreshed) || refreshed.Size() != info.Size() {
			l.status.RequireRecovery()
			return SealResult{}, errors.Join(ErrProjectionSource, statErr)
		}
		if err := l.checkPhysicalNativeLocked(); err != nil {
			return SealResult{}, err
		}
		info = refreshed
	}
	defer func() {
		if physical := l.physical.Load(); physical != nil && resultErr != nil {
			physical.failure = errors.Join(authenticatedstate.ErrRecovery, resultErr)
			l.status.RequireRecovery()
		}
	}()

	// Intent first. From here a crash is recoverable in both directions: the
	// rename below either happened or it did not, and repairSegments decides
	// which by looking for the rolled file.
	if renamed, err := saveSegmentManifestSourceWithDisk(l.directory, l.segmentDirectory.source(nil, nil, &pending), durable.SyncDirectory, l.options.ReserveNativeFileDisk); err != nil {
		if errors.Is(err, ErrSegmentRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			l.closeErr = errors.Join(l.closeErr, err)
		}
		if renamed || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrTampered) || errors.Is(err, ownedfile.ErrReplaced) {
			l.status.RequireRecovery()
		}
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-intent-published"); err != nil {
		return SealResult{}, err
	}
	if err := l.refreshSegmentDirectoryLocked(); err != nil {
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-before-rename"); err != nil {
		return SealResult{}, err
	}
	currentParent, parentErr := os.Lstat(l.directory)
	current, nameErr := os.Lstat(l.path)
	if parentErr != nil || nameErr != nil || !os.SameFile(parent, currentParent) || !os.SameFile(info, current) || !current.Mode().IsRegular() || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		l.status.RequireRecovery()
		return SealResult{}, errors.Join(ownedfile.ErrReplaced, parentErr, nameErr)
	}
	if _, err := os.Lstat(rolled); !errors.Is(err, os.ErrNotExist) {
		l.status.RequireRecovery()
		return SealResult{}, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := os.Rename(l.path, rolled); err != nil {
		return SealResult{}, fmt.Errorf("seal ledger generation: %w", err)
	}
	if err := l.segmentPhase("roll-native-renamed"); err != nil {
		return SealResult{}, err
	}
	// Past the rename the roll is committed; a failure from here leaves the
	// pending intent for the next open to finish, so the status is marked
	// unavailable rather than pretending the log is still writable. That
	// includes this directory sync: l.file still holds the renamed inode, so a
	// log left healthy here would append committed accounting frames into a
	// generation the manifest has already recorded a fixed length for, and the
	// next open would refuse to start with those frames stranded inside it.
	if err := durable.SyncDirectory(l.directory); err != nil {
		l.status.MarkUnavailable()
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-rename-synced"); err != nil {
		return SealResult{}, err
	}
	var successorReservation NativeFileDiskReservation
	if l.options.ReserveNativeFileDisk != nil {
		successorReservation, err = l.options.ReserveNativeFileDisk(64 << 10)
		if err != nil {
			l.status.RequireRecovery()
			return SealResult{}, err
		}
	}
	defer func() {
		resultErr = keepNativeFileDiskDebt(successorReservation, resultErr)
		if errors.Is(resultErr, ErrSegmentRetirement) {
			l.closeErr = errors.Join(l.closeErr, resultErr)
		}
	}()
	successor, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		l.status.RequireRecovery()
		return SealResult{}, fmt.Errorf("open successor ledger generation: %w", err)
	}
	transferred := false
	defer func() {
		if !transferred {
			if err := ownedfile.CloseFile(successor); err != nil {
				l.closeErr = errors.Join(l.closeErr, ErrSegmentRetirement, err)
				resultErr = errors.Join(resultErr, l.closeErr)
			}
		}
		if resultErr != nil {
			result = SealResult{}
		}
	}()
	successorInfo, err := successor.Stat()
	named, nameErr = os.Lstat(l.path)
	currentParent, parentErr = os.Lstat(l.directory)
	if err != nil || nameErr != nil || parentErr != nil || !successorInfo.Mode().IsRegular() || !os.SameFile(successorInfo, named) || !os.SameFile(parent, currentParent) {
		l.status.RequireRecovery()
		return SealResult{}, errors.Join(ownedfile.ErrReplaced, err, nameErr, parentErr)
	}
	if err := l.segmentPhase("roll-successor-created"); err != nil {
		return SealResult{}, err
	}
	if err := successor.Sync(); err != nil {
		l.status.RequireRecovery()
		return SealResult{}, fmt.Errorf("sync successor ledger generation: %w", err)
	}
	if err := durable.SyncDirectory(l.directory); err != nil {
		l.status.RequireRecovery()
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-successor-synced"); err != nil {
		return SealResult{}, err
	}
	current, nameErr = os.Lstat(l.path)
	currentParent, parentErr = os.Lstat(l.directory)
	finalSuccessor, statErr := successor.Stat()
	if nameErr != nil || parentErr != nil || statErr != nil || !current.Mode().IsRegular() || !os.SameFile(successorInfo, current) || !os.SameFile(successorInfo, finalSuccessor) || !os.SameFile(parent, currentParent) || current.Size() != 0 || finalSuccessor.Size() != 0 {
		l.status.RequireRecovery()
		return SealResult{}, errors.Join(ownedfile.ErrReplaced, nameErr, parentErr, statErr)
	}
	if _, err := saveSegmentManifestSourceWithDisk(l.directory, l.segmentDirectory.source(&pending, nil, nil), durable.SyncDirectory, l.options.ReserveNativeFileDisk); err != nil {
		if errors.Is(err, ErrSegmentRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			l.closeErr = errors.Join(l.closeErr, err)
		}
		l.status.RequireRecovery()
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-manifest-published"); err != nil {
		return SealResult{}, err
	}
	if err := l.refreshSegmentDirectoryLocked(); err != nil {
		return SealResult{}, err
	}
	if err := l.segmentPhase("roll-index-published"); err != nil {
		return SealResult{}, err
	}
	writer := DurabilityWriter(successor)
	if l.options.WrapDurability != nil {
		writer = l.options.WrapDurability(successor)
		if writer == nil {
			l.status.RequireRecovery()
			return SealResult{}, errors.New("ledger successor durability wrapper returned nil")
		}
	}
	previous := l.file
	l.file, l.durability = successor, writer
	transferred = true
	if err := l.segmentPhase("roll-before-previous-close"); err != nil {
		// The successor is already selected; Close retains both physical
		// obligations even when a boundary observer fails.
		closeErr := ownedfile.CloseFile(previous)
		if closeErr != nil {
			l.closeErr = errors.Join(l.closeErr, ErrSegmentRetirement, closeErr)
		}
		return SealResult{}, errors.Join(err, l.closeErr)
	}
	if err := ownedfile.CloseFile(previous); err != nil {
		l.closeErr = errors.Join(l.closeErr, ErrSegmentRetirement, err)
		l.status.RequireRecovery()
		return SealResult{}, l.closeErr
	}
	if err := l.segmentPhase("roll-previous-closed"); err != nil {
		return SealResult{}, err
	}
	l.segmentSummary = nextSummary
	l.generation = pending.Generation + 1
	l.startSequence = pending.LastSequence
	l.chainAnchor = l.chainHash
	l.offset = 0
	l.chainOffset = 0
	// The successor starts empty, so its running checksum does too.
	l.plainDigest = sha256.New()
	if err := l.transferPhysicalRollOwnerLocked(pending.Length); err != nil {
		return SealResult{}, err
	}
	return SealResult{
		Sealed: pending, Rolled: true, Active: l.generation,
		Bytes: pending.Length, Records: pending.LastSequence - pending.FirstSequence + 1,
	}, nil
}

func sameAuthoritativeSegment(left, right Segment) bool {
	return left.Generation == right.Generation && left.FirstSequence == right.FirstSequence &&
		left.LastSequence == right.LastSequence && left.Length == right.Length &&
		left.StartHash == right.StartHash && left.EndHash == right.EndHash &&
		left.EndEpoch == right.EndEpoch && left.PlainChecksum == right.PlainChecksum &&
		left.SealedAt.Equal(right.SealedAt)
}

// Compact replaces a sealed generation's plain bytes with a compressed copy.
//
// It is separate from Roll on purpose. A roll has to happen while the writer is
// held, so it must be fast; compressing gigabytes is not. Splitting them also
// makes compaction restartable: it reads a file nothing will ever append to,
// and every step can be repeated after a crash without consulting anything but
// the files themselves.
func (l *Log) Compact(generation uint64) (Segment, error) {
	return l.CompactContext(context.Background(), generation)
}

func (l *Log) CompactContext(ctx context.Context, generation uint64) (Segment, error) {
	return l.compactWithIOContext(ctx, generation, compactIO{
		compress: func(source, destination, checksum string, length int64) (*compressedArtifact, error) {
			return compressSegmentFileOwnedWithDisk(ctx, source, destination, checksum, length, l.options.ReserveNativeFileDisk)
		},
		publish: func(directory string, source segmentManifestSource) (bool, error) {
			return saveSegmentManifestSourceWithDisk(directory, source, durable.SyncDirectory, l.options.ReserveNativeFileDisk)
		},
	})
}

// Dependencies are per operation so tests can stop at real filesystem
// boundaries without replacing process-wide IO or publishing synthetic bytes.
type compactIO struct {
	compress func(source, destination, checksum string, length int64) (*compressedArtifact, error)
	publish  func(directory string, source segmentManifestSource) (renamed bool, err error)
}

// ErrSegmentCompactionBusy rejects overlapping compression work without an
// unbounded waiter queue. It does not change accounting health.
var ErrSegmentCompactionBusy = errors.New("another Ledger segment compaction is in progress")

func (l *Log) compactWithIO(generation uint64, operations compactIO) (Segment, error) {
	return l.compactWithIOContext(context.Background(), generation, operations)
}

func (l *Log) compactWithIOContext(ctx context.Context, generation uint64, operations compactIO) (result Segment, resultErr error) {
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			l.mu.Lock()
			l.closeErr = errors.Join(l.closeErr, resultErr)
			l.status.RequireRecovery()
			l.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return Segment{}, err
	}
	finish, err := l.beginMutation(nil)
	if err != nil {
		return Segment{}, err
	}
	defer finish()
	l.mu.Lock()
	if !l.selectedStartupStructuralAllowedLocked() {
		l.mu.Unlock()
		return Segment{}, ErrStartupAuditPending
	}
	if l.file == nil || l.status.Load() != AccountingHealthy {
		l.mu.Unlock()
		return Segment{}, errors.New("Ledger compaction requires a healthy open log")
	}
	segment, found, err := l.lookupSegmentLocked(generation)
	if err != nil {
		l.mu.Unlock()
		return Segment{}, err
	}
	directory := l.directory
	if !found {
		l.mu.Unlock()
		return Segment{}, fmt.Errorf("ledger generation %d is not sealed", generation)
	}
	if segment.Compressed {
		return l.retireSelectedCompactedDuplicate(ctx, segment)
	}
	if l.compacting {
		l.mu.Unlock()
		return Segment{}, ErrSegmentCompactionBusy
	}
	l.compacting = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.compacting = false
		l.mu.Unlock()
	}()

	source := filepath.Join(directory, segment.File)
	compressedName := fmt.Sprintf("ledger-%d.seg.gz", generation)
	destination := filepath.Join(directory, compressedName)
	// Compressing happens with the writer released. It reads a file nothing
	// will ever append to, so nothing about it needs the lock — and holding the
	// lock across a read, a gzip and a decompress-verify of up to
	// ledger.seal.max_active_bytes stalled every Append for the duration, which
	// is every budget reservation and therefore the gateway itself.
	artifact, err := operations.compress(source, destination, segment.PlainChecksum, segment.Length)
	if err != nil {
		return Segment{}, fmt.Errorf("compress ledger generation %d: %w", generation, err)
	}
	if err := l.segmentPhase("compact-artifact-published"); err != nil {
		return Segment{}, err
	}
	if err := errors.Join(ctx.Err(), artifact.check()); err != nil {
		return Segment{}, errors.Join(err, artifact.discard())
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-read the history rather than trusting what was captured above: a roll
	// may have sealed another generation while this ran. The generation being
	// compacted is immutable either way, so only its position can have moved.
	current, found, err := l.lookupSegmentLocked(generation)
	if err != nil {
		return Segment{}, errors.Join(err, artifact.discard())
	}
	switch {
	case l.file == nil:
		return Segment{}, errors.Join(errors.New("ledger is closed"), artifact.discard())
	case !found:
		return Segment{}, errors.Join(fmt.Errorf("ledger generation %d is not sealed", generation), artifact.discard())
	case current.Compressed:
		// Never remove a path named by the current manifest. In-process
		// compression admission excludes overlapping publishers.
		return current, nil
	case l.status.Load() != AccountingHealthy:
		return Segment{}, errors.Join(errors.New("Ledger became unhealthy during compaction"), artifact.discard())
	}
	if err := errors.Join(ctx.Err(), artifact.check()); err != nil {
		return Segment{}, errors.Join(err, artifact.discard())
	}
	stored, storedLength := artifact.checksum, artifact.length

	updated := current
	updated.File = compressedName
	updated.Compressed = true
	updated.StoredChecksum = stored
	updated.StoredLength = storedLength
	if current.StoredLength < 0 || l.segmentSummary.StoredBytes < current.StoredLength || storedLength < 0 || storedLength > math.MaxInt64-(l.segmentSummary.StoredBytes-current.StoredLength) {
		return Segment{}, errors.Join(fmt.Errorf("%w: compacted segment byte total is invalid", ErrCorrupt), artifact.discard())
	}
	nextSummary := l.segmentSummary
	nextSummary.StoredBytes = l.segmentSummary.StoredBytes - current.StoredLength + storedLength
	// The manifest switch is the commit point. Before it the plain file is
	// still what the log reads and the compressed copy is an orphan; after it
	// the reverse, and removing the plain file is cleanup that may fail without
	// costing anything but disk.
	if renamed, err := operations.publish(directory, l.segmentDirectory.source(nil, &updated, nil)); err != nil {
		if renamed {
			// The new manifest is visible but its crash durability is unknown.
			// Retain both representations and latch recovery before any further
			// writer/maintenance operation can publish a conflicting directory.
			l.status.RequireRecovery()
		} else {
			err = errors.Join(err, artifact.discard())
		}
		return Segment{}, err
	}
	if err := l.segmentPhase("compact-manifest-published"); err != nil {
		return Segment{}, err
	}
	if err := l.refreshSegmentDirectoryLocked(); err != nil {
		return Segment{}, err
	}
	if err := l.segmentPhase("compact-index-published"); err != nil {
		return Segment{}, err
	}
	l.segmentSummary = nextSummary
	if _, err := retireCompactedNative(source, artifact.sourceInfo, artifact.directoryInfo); err != nil {
		// The compressed manifest is already committed. Preserve that fact and
		// require recovery before further native maintenance or business writes.
		l.status.RequireRecovery()
		return updated, fmt.Errorf("retire compacted ledger generation %d: %w", generation, err)
	}
	if err := l.segmentPhase("compact-plain-retired"); err != nil {
		return Segment{}, err
	}
	return updated, nil
}

func (l *Log) segmentPhase(stage string) error {
	if l.maintenancePhase != nil {
		if err := l.maintenancePhase(stage); err != nil {
			l.status.RequireRecovery()
			return err
		}
	}
	return nil
}

// SegmentVerification is a fixed-size result of complete sealed verification.
// Counters are published only after the complete pinned manifest is rechecked.
type SegmentVerification struct {
	Generations   uint64
	Authenticated uint64
	ChecksumOnly  uint64
}

// VerifySegmentsSummary verifies the entire sealed chain without collecting a
// report or descriptor for every historical generation.
func VerifySegmentsSummary(directory string, key []byte, since uint64) (SegmentVerification, error) {
	var result SegmentVerification
	err := verifySegments(directory, key, since, func(report ChainReport) error {
		if result.Generations == math.MaxUint64 || report.Authenticated > math.MaxUint64-result.Authenticated || report.ChecksumOnly > math.MaxUint64-result.ChecksumOnly {
			return fmt.Errorf("%w: sealed verification counter overflow", ErrCorrupt)
		}
		result.Generations++
		result.Authenticated += report.Authenticated
		result.ChecksumOnly += report.ChecksumOnly
		return nil
	})
	if err != nil {
		return SegmentVerification{}, err
	}
	return result, nil
}

// verifySegments authenticates sealed generations end to end, from `since`
// onward (zero for all of them).
//
// This is what makes `halro ledger verify` mean the same thing after sealing as
// before it. Verifying only the active file would answer a smaller question
// each time a generation rolled off, and would eventually answer nothing at
// all — the chain would be intact and unexamined.
// consume observes tentative results: only a nil final error certifies the
// complete directory. Callers must discard derived results on failure.
func verifySegments(directory string, key []byte, since uint64, consume func(ChainReport) error) (resultErr error) {
	manifest, err := openInspectionManifest(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, manifest.close()) }()
	layout, digest, err := manifest.walk(nil)
	if err != nil {
		return err
	}
	var previousEnd [32]byte
	var previousSequence uint64
	var previousEpoch byte
	secondLayout, secondDigest, err := manifest.walk(func(segment Segment) error {
		// Skipped generations still hand their chain head and sequence to the
		// next one, so a partial verification checks the same links a full one
		// would — it just does not re-read bytes it has already accepted.
		if segment.Generation < since {
			previousEnd, err = decodeChainHash(segment.EndHash)
			if err != nil {
				return err
			}
			previousSequence = segment.LastSequence
			previousEpoch = segmentEndEpoch(segment)
			return nil
		}
		start, err := decodeChainHash(segment.StartHash)
		if err != nil {
			return err
		}
		if start != previousEnd {
			return fmt.Errorf("%w: generation %d does not continue the chain of its predecessor",
				ErrTampered, segment.Generation)
		}
		stored, length, err := hashFile(filepath.Join(directory, segment.File))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: generation %d (%s)", ErrSegmentMissing, segment.Generation, segment.File)
		}
		if err != nil {
			return err
		}
		if stored != segment.StoredChecksum {
			return fmt.Errorf("%w: generation %d does not match its recorded checksum (%d bytes)",
				ErrCorrupt, segment.Generation, length)
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return err
		}
		verifier := &chainVerifier{key: key, hash: start, sequence: previousSequence, epoch: previousEpoch}
		report := ChainReport{}
		head, partial, scanErr := scan(reader, segment.Generation, 0, previousSequence, func(record Record) error {
			if authenticatedFrameEpoch(record.Epoch) && verifier.verify() {
				report.Authenticated++
			} else {
				report.ChecksumOnly++
			}
			return nil
		}, verifier)
		closeErr := reader.Close()
		if err := errors.Join(scanErr, closeErr); err != nil {
			return err
		}
		if partial {
			return fmt.Errorf("%w: generation %d ends mid-frame", ErrCorrupt, segment.Generation)
		}
		if head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return fmt.Errorf("%w: generation %d ends at %d/%d, manifest says %d/%d",
				ErrCorrupt, segment.Generation, head.Offset, head.Sequence, segment.Length, segment.LastSequence)
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return err
		}
		if verifier.verify() && verifier.hash != end {
			return fmt.Errorf("%w: generation %d does not end at its recorded chain head",
				ErrTampered, segment.Generation)
		}
		if verifier.epoch == frameVersionRunAttribution && segment.EndEpoch != uint8(frameVersionRunAttribution) {
			return fmt.Errorf("%w: generation %d omits or misstates its run-attribution epoch",
				ErrCorrupt, segment.Generation)
		}
		if segment.EndEpoch != 0 && byte(segment.EndEpoch) != verifier.epoch {
			return fmt.Errorf("%w: generation %d ends at epoch %d, manifest says %d",
				ErrCorrupt, segment.Generation, verifier.epoch, segment.EndEpoch)
		}
		report.Head = head
		report.ChainSequence, report.ChainOffset, report.ChainHash, report.ChainVerified =
			verifier.sequence, verifier.offset, verifier.hash, verifier.sawFrames
		if err := consume(report); err != nil {
			return err
		}
		previousEnd, previousSequence, previousEpoch = end, segment.LastSequence, verifier.epoch
		return nil
	})
	if err != nil {
		return err
	}
	if layout != secondLayout || digest != secondDigest {
		return fmt.Errorf("%w: sealed verification manifest changed between passes", ErrCorrupt)
	}
	return manifest.check()
}

// VerifySealedSummary returns fixed counters while retaining full verification.
func (l *Log) VerifySealedSummary(since uint64) (SegmentVerification, error) {
	l.mu.Lock()
	if l.file == nil {
		l.mu.Unlock()
		return SegmentVerification{}, errors.New("ledger is closed")
	}
	directory, key := l.directory, append([]byte(nil), l.chainKey...)
	l.mu.Unlock()
	defer clear(key)
	return VerifySegmentsSummary(directory, key, since)
}

// StageSegmentsEach copies one historical file at a time without collecting
// names. Callbacks are tentative until final success. They must not reenter Log:
// the source directory and physical files stay stable under Log.mu for the walk.
// The caller owns staging cleanup on failure. Cancellation waits for current IO.
func (l *Log) StageSegmentsEach(ctx context.Context, directory string, consume func(string) error) error {
	return l.StageSegmentsEachWithDisk(ctx, directory, nil, consume)
}

// StageSegmentsEachWithDisk admits actual payloads and the staged manifest
// against an independent aggregate staging owner, one transient receipt at a
// time. Success/consumer failure leaves already published output fees held.
func (l *Log) StageSegmentsEachWithDisk(ctx context.Context, directory string, reserve func(int64) (NativeFileDiskReservation, error), consume func(string) error) (resultErr error) {
	if consume == nil {
		return errors.New("ledger staging consumer is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			l.closeErr = errors.Join(l.closeErr, resultErr)
		}
	}()
	if l.file == nil {
		return errors.New("ledger is closed")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := l.segmentDirectory.source(nil, nil, nil).each(func(segment Segment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := copySealedSegmentWithDisk(ctx, filepath.Join(l.directory, segment.File), filepath.Join(directory, segment.File), segment, reserve); err != nil {
			return fmt.Errorf("stage ledger generation %d: %w", segment.Generation, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return consume(segment.File)
	}); err != nil {
		if errors.Is(err, ErrCorrupt) || errors.Is(err, ErrTampered) {
			l.status.RequireRecovery()
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.segmentDirectory.parsed.count == 0 {
		return nil
	}
	if _, err := saveSegmentManifestSourceWithDisk(directory, l.segmentDirectory.source(nil, nil, nil), durable.SyncDirectory, reserve); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return consume(segmentManifestName)
}
