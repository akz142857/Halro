package metadatajournal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Log is the append side of the journal. One process owns one data directory
// exclusively, so there is exactly one of these per directory and its mutex
// serializes appends the way the Ledger's and the Audit log's do.
// DurabilityWriter is the append side of the journal file, narrowed to what
// the log actually needs.
//
// It exists as an interface for one reason: fault injection. The HA design's
// release gate (§17) requires the crash matrix to be exercised at real failure
// points, and the Ledger already carries the same seam — a disk that returns
// ENOSPC or EIO is not something a test can produce on a filesystem it does not
// own. Production always passes the *os.File straight through.
type DurabilityWriter interface {
	io.Writer
	Sync() error
}

// WrapDurability is the injection seam. Nil means the file is used directly.
//
// It is a package variable rather than an option on Open because every caller
// of Open and StartEpoch is inside internal/store/bolt, and threading a test
// hook through a store's attach path would put a test seam in a production
// signature that nothing else needs.
var WrapDurability func(*os.File) DurabilityWriter

func durabilityFor(file *os.File) DurabilityWriter {
	if WrapDurability != nil {
		return WrapDurability(file)
	}
	return file
}

type Log struct {
	physical             *physicalNativeOwner
	readOnly             bool
	closeMu              sync.Mutex
	freezeWG             sync.WaitGroup
	freezeCancel         context.CancelFunc
	freezeDebt           resourcebudget.Permit
	closing              bool
	mu                   sync.Mutex
	file                 *os.File
	closed               bool
	closeErr             error
	retirementErr        error
	syncRetiredDirectory func(string) error
	// durability is what Append writes through; it is the file itself unless a
	// test has installed a seam.
	durability DurabilityWriter
	path       string
	key        []byte
	epoch      uint64
	sequence   uint64
	offset     int64
	lastHash   [32]byte
	// trimmedThrough is carried so Head answers the same question the file
	// does. A reader comparing a projection against this log needs it: a
	// projection below the cut cannot be caught up from here.
	trimmedThrough        uint64
	afterDurable          func(DurableBatch) (uint64, error)
	beforeDurable         func(DurableBatch) (AppendAdmission, error)
	uncertainAdmissions   []AppendAdmission
	waitConfirmed         func(context.Context, uint64) error
	replica               bool
	terminalErr           error
	formatVersion         uint8
	fileRevision          uint64
	baseline              BaselineBinding
	boundReplication      *BoundReplicationAuthority
	boundPhysicalRecovery bool
	boundReaderDebt       *ReplicationRangeReader
	maxBytes              int64
	created               *ownedfile.Owner
	startupAuditPending   bool
	startupBudget         *resourcebudget.Manager
	startupSaved          Head
	startupDebt           resourcebudget.Permit
	startupAudit          *StartupAudit
	startupReaderDebt     *startupAuditReaderDebt
	startupGuard          *lock.Lock
}

type DurableBatch struct {
	// Context is the original prepared append context. Legacy AppendWithReceipt
	// leaves it nil; it cannot manufacture a request's outstanding IO promise.
	Context       context.Context
	Epoch         uint64
	FirstSequence uint64
	LastSequence  uint64
	Frames        []byte
	Admission     AppendAdmission
}

type AppendAdmission interface{ Release() error }

// SetReplicationAdmissionAndAfterDurable installs the pre-write capacity
// owner and post-fsync ordering callback atomically before metadata writes are
// admitted.
func (l *Log) SetReplicationAdmissionAndAfterDurable(before func(DurableBatch) (AppendAdmission, error), after func(DurableBatch) (uint64, error), waitConfirmed func(context.Context, uint64) error) error {
	if before == nil || after == nil || waitConfirmed == nil {
		return errors.New("metadata admission, durability and confirmation callbacks are required together")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return ErrNativeFrozen
	}
	if err := l.availabilityError(); err != nil {
		return err
	}
	if l.formatVersion == boundFrameVersion || l.replica {
		return errors.New("metadata journal does not support this Primary admission hook")
	}
	if l.afterDurable != nil || l.beforeDurable != nil {
		return errors.New("metadata replication callback is already installed")
	}
	l.beforeDurable, l.afterDurable, l.waitConfirmed = before, after, waitConfirmed
	return nil
}

// SetAfterDurable installs the HA ordering callback after the journal is
// attached and before authoritative writes are admitted. It is one-shot so a
// running store cannot silently change which replication tenure owns commits.
func (l *Log) SetAfterDurable(callback func(DurableBatch) (uint64, error), waitConfirmed func(context.Context, uint64) error) error {
	if callback == nil || waitConfirmed == nil {
		return errors.New("metadata journal durability and confirmation callbacks are required together")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return ErrNativeFrozen
	}
	if err := l.availabilityError(); err != nil {
		return err
	}
	if l.formatVersion == boundFrameVersion {
		return errors.New("bound metadata HA requires an explicit qualified integration")
	}
	if l.replica {
		return errors.New("metadata journal is replica-owned")
	}
	if l.afterDurable != nil {
		return errors.New("metadata journal durable callback is already installed")
	}
	l.afterDurable = callback
	l.waitConfirmed = waitConfirmed
	return nil
}

// AppendReceipt binds a committed metadata-journal frame to the global
// replication index allocated after its source fsync. A zero index is the
// Standalone path.
type AppendReceipt struct {
	Record           Record
	ReplicationIndex uint64
}

// Head is where the file ends: the position a projection is compared against.
type Head struct {
	Epoch    uint64
	Sequence uint64
	Offset   int64
	Hash     [32]byte
	// TrimmedThrough is the sequence the file's head was cut at, or zero if it
	// still starts at its epoch header. A reader comparing against a projection
	// needs it: a projection at a sequence below this one cannot be caught up
	// from this file, and that is a fail-closed condition rather than a gap to
	// paper over.
	TrimmedThrough uint64
	FormatVersion  uint8
	FileRevision   uint64
	Baseline       BaselineBinding
}

// Open reads an existing journal, repairs a torn tail, and positions for
// append. A file that does not exist yet is not created here: a journal starts
// at an epoch, and StartEpoch is the call that says which one.
func Open(path string, key []byte) (*Log, error) {
	return OpenContext(context.Background(), path, key)
}

// OpenReplica opens an existing epoch for physical frame landing and refuses
// local semantic appends.
func OpenReplica(path string, key []byte) (*Log, error) {
	return OpenReplicaContext(context.Background(), path, key)
}

// OpenContext retains the full authenticated Open contract and checks cancel
// between bounded reads. A blocked filesystem Read still has to return.
func OpenContext(ctx context.Context, path string, key []byte) (*Log, error) {
	return open(ctx, path, key, false)
}

// OpenContextWithLimit authenticates a journal and rejects a physical file
// larger than maxBytes. A zero limit preserves the unmetered API behavior.
func OpenContextWithLimit(ctx context.Context, path string, key []byte, maxBytes int64) (*Log, error) {
	if maxBytes < 0 {
		return nil, errors.New("metadata journal disk limit cannot be negative")
	}
	return openExpected(ctx, path, key, false, nil, 0, maxBytes, nil)
}

func OpenReplicaContext(ctx context.Context, path string, key []byte) (*Log, error) {
	return open(ctx, path, key, true)
}

func OpenReplicaContextWithLimit(ctx context.Context, path string, key []byte, maxBytes int64) (*Log, error) {
	if maxBytes < 0 {
		return nil, errors.New("metadata journal disk limit cannot be negative")
	}
	return openExpected(ctx, path, key, true, nil, 0, maxBytes, nil)
}

func open(ctx context.Context, path string, key []byte, replica bool) (*Log, error) {
	return openExpected(ctx, path, key, replica, nil, 0, 0, nil)
}
func openExpected(ctx context.Context, path string, key []byte, replica bool, expected *BaselineBinding, revision uint64, maxBytes int64, exact *Head) (*Log, error) {
	return openExpectedWithFactory(ctx, path, key, replica, expected, revision, maxBytes, exact, durabilityFor)
}

func openExpectedWithFactory(ctx context.Context, path string, key []byte, replica bool, expected *BaselineBinding, revision uint64, maxBytes int64, exact *Head, factory func(*os.File) DurabilityWriter) (result *Log, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			resultErr = errors.Join(resultErr, closeDescriptor(file))
		}
	}()
	if expected != nil || maxBytes > 0 {
		info, err := file.Stat()
		if err != nil || maxBytes > 0 && info.Size() > maxBytes {
			return nil, errors.Join(errors.New("metadata journal exceeds configured disk limit"), err)
		}
	}
	var captured os.FileInfo
	if expected != nil {
		captured, err = os.Lstat(path)
		actual, statErr := file.Stat()
		if err != nil || statErr != nil || !captured.Mode().IsRegular() || !os.SameFile(captured, actual) {
			return nil, errors.Join(ErrCorrupt, err, statErr)
		}
	}
	head, partial, err := scanContext(ctx, file, key, nil)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 {
		info, statErr := file.Stat()
		if statErr != nil || info.Size() > maxBytes {
			return nil, errors.Join(errors.New("metadata journal exceeds configured disk limit"), statErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expected == nil && head.FormatVersion == boundFrameVersion || expected != nil && (head.FormatVersion != boundFrameVersion || head.Baseline != *expected || head.FileRevision != revision || head.Offset > maxBytes) || maxBytes > 0 && head.Offset > maxBytes {
		return nil, errors.Join(ErrIncompatibleFormat, errors.New("metadata journal requires matching baseline and file revision"))
	}
	if expected != nil {
		named, nameErr := os.Lstat(path)
		actual, statErr := file.Stat()
		if nameErr != nil || statErr != nil || !os.SameFile(captured, named) || !os.SameFile(captured, actual) || captured.Size() != actual.Size() {
			return nil, errors.Join(ErrCorrupt, nameErr, statErr)
		}
	}
	if exact != nil && (partial || head != *exact) {
		return nil, errors.Join(ErrCorrupt)
	}
	if partial {
		if head.Epoch == 0 || head.FormatVersion == 0 {
			return nil, errors.Join(fmt.Errorf("%w: no complete metadata control frame permits tail repair", ErrCorrupt))
		}
		if replica {
			return nil, fmt.Errorf("%w: replica metadata journal has a partial tail that must be reconciled against ordering", ErrCorrupt)
		}
		if err := file.Truncate(head.Offset); err != nil {
			return nil, fmt.Errorf("truncate partial metadata journal tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			return nil, fmt.Errorf("sync repaired metadata journal tail: %w", err)
		}
	}
	if _, err := file.Seek(head.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	writer := factory(file)
	if writer == nil {
		return nil, errors.New("metadata durability writer is absent")
	}
	return &Log{
		file: file, durability: writer,
		path: path, key: append([]byte(nil), key...),
		epoch: head.Epoch, sequence: head.Sequence, offset: head.Offset, lastHash: head.Hash,
		trimmedThrough: head.TrimmedThrough,
		replica:        replica,
		formatVersion:  head.FormatVersion, fileRevision: head.FileRevision, baseline: head.Baseline, maxBytes: maxBytes,
	}, nil
}

func (l *Log) MaxDiskBytes() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxBytes
}

// HealthError distinguishes definite pre-write refusal from a writer whose
// native append or post-durable publication failed and requires recovery.
func (l *Log) HealthError() error {
	if l == nil {
		return errors.New("metadata journal is absent")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.retirementErr != nil {
		return l.retirementErr
	}
	if l.terminalErr != nil {
		return l.terminalErr
	}
	if err := l.availabilityError(); err != nil {
		return err
	}
	return nil
}

// StartEpoch publishes a fresh journal for a halro.db that was replaced as a
// whole file — a restore, a Master Key rotation bridge, a compaction, a schema
// migration. The previous file is replaced, not appended to: after such a
// publish its frames no longer describe the database, and keeping them would
// keep alive the one path this rule exists to close (see the package comment).
//
// The caller supplies the epoch it is starting and what the previous one ended
// on, so the new file's first frame is the only surviving record of what it
// replaced.
func StartEpoch(path string, key []byte, header EpochHeader) (*Log, error) {
	return StartEpochWithLimit(path, key, header, 0)
}

func StartEpochWithLimit(path string, key []byte, header EpochHeader, maxBytes int64) (*Log, error) {
	if header.Baseline != nil || header.FileRevision != 0 {
		return nil, errors.New("bound metadata epoch requires explicit StartBoundEpoch")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	if maxBytes < 0 {
		return nil, errors.New("metadata journal disk limit cannot be negative")
	}
	if header.Epoch == 0 {
		return nil, errors.New("an epoch starts at 1")
	}
	if header.Epoch <= header.PreviousEpoch {
		return nil, errors.New("a new epoch must be greater than the one it replaces")
	}
	if header.Reason == "" {
		return nil, errors.New("an epoch records why it started")
	}
	payload, err := encodePayload(KindEpoch, nil, header, TrimAnchor{})
	if err != nil {
		return nil, err
	}
	frame, _ := encodeFrame(key, KindEpoch, header.Epoch, 0, [32]byte{}, payload)
	if maxBytes > 0 && int64(len(frame)) > maxBytes {
		return nil, errors.New("metadata journal epoch header exceeds configured disk limit")
	}
	if err := publish(path, frame); err != nil {
		return nil, err
	}
	return OpenContextWithLimit(context.Background(), path, key, maxBytes)
}

// publish owns the candidate's creation receipt. Caller exclusion is required
// for replacing a captured journal; a missing destination is published by an
// exclusive hard link, never an overwriting rename.
func publish(path string, contents []byte) (resultErr error) {
	previous, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if previous != nil && !previous.Mode().IsRegular() {
		return ownedfile.ErrReplaced
	}
	directory := filepath.Dir(path)
	temporary, owner, err := ownedfile.CreateTemporary(directory, ".metadata-journal-*")
	if err != nil {
		return err
	}
	closed, moved := false, false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, closeDescriptor(temporary))
		}
		if !moved && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, owner.Retire())
		}
	}()
	if err := writeFull(temporary, contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := owner.VerifyFile(temporary); err != nil {
		return err
	}
	closed = true
	if err := closeDescriptor(temporary); err != nil {
		return err
	}
	if previous != nil {
		if err := owner.ReplaceCaptured(path, previous); err != nil {
			return err
		}
		moved = true
	} else {
		alias, err := owner.LinkCaptured(path)
		if alias != nil {
			if err != nil {
				return errors.Join(ownedfile.ErrRetirement, err)
			}
		}
		if err != nil {
			return err
		}
	}
	return durable.SyncDirectory(directory)
}

// Append records one transaction's operations and makes the frame durable.
//
// It returns before the bbolt transaction commits, which is the whole ordering
// this design rests on: the journal is ahead of the projection or level with
// it, never behind. A crash between this fsync and the commit leaves a frame
// describing work the database has not done, and open-time reconciliation
// replays it.
func (l *Log) Append(ops []Op) (Record, error) {
	receipt, err := l.AppendWithReceipt(ops)
	return receipt.Record, err
}

// AppendWithReceipt writes one operations frame and returns the replication
// token without waiting on the network. The bbolt transaction must commit
// before its caller invokes WaitConfirmed.
func (l *Log) AppendWithReceipt(ops []Op) (AppendReceipt, error) {
	if l.formatVersion == boundFrameVersion {
		if err := validateBoundOps(ops); err != nil {
			return AppendReceipt{}, err
		}
	}
	if len(ops) == 0 {
		return AppendReceipt{}, errors.New("an operations frame with no operations would record nothing")
	}
	payload, err := encodePayload(KindOperations, ops, EpochHeader{}, TrimAnchor{})
	if err != nil {
		return AppendReceipt{}, err
	}
	if len(payload) > MaxPayloadSize {
		return AppendReceipt{}, fmt.Errorf("metadata transaction records %d bytes, over the %d byte frame limit",
			len(payload), MaxPayloadSize)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return AppendReceipt{}, ErrNativeFrozen
	}
	if err := l.availabilityError(); err != nil {
		return AppendReceipt{}, err
	}
	if l.replica || l.readOnly {
		return AppendReceipt{}, errors.New("metadata journal is replica-owned and refuses local append")
	}
	if l.boundPhysicalRecovery {
		return AppendReceipt{}, ErrIncompatibleFormat
	}
	if l.physical != nil && l.physical.enabled {
		return AppendReceipt{}, resourcebudget.ErrPhysicalAllocation
	}
	if l.terminalErr != nil {
		return AppendReceipt{}, fmt.Errorf("metadata journal requires restart after an uncertain durable callback: %w", l.terminalErr)
	}
	if l.sequence == ^uint64(0) {
		return AppendReceipt{}, errors.New("metadata sequence overflow")
	}
	if l.boundReplication != nil {
		if !l.boundReplication.matches(Head{Epoch: l.epoch, Offset: l.offset, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}) {
			return AppendReceipt{}, ErrIncompatibleFormat
		}
		if err := l.checkPreparedFile(); err != nil {
			return AppendReceipt{}, err
		}
	}
	sequence := l.sequence + 1
	version := l.formatVersion
	if version == 0 {
		version = frameVersion
	}
	frame, hash := encodeFrameVersion(version, l.key, KindOperations, l.epoch, sequence, l.lastHash, payload)
	if l.maxBytes > 0 && (l.offset > l.maxBytes || int64(len(frame)) > l.maxBytes-l.offset) {
		return AppendReceipt{}, errors.New("bound metadata journal disk limit reached")
	}
	durableBatch := DurableBatch{Epoch: l.epoch, FirstSequence: sequence, LastSequence: sequence, Frames: append([]byte(nil), frame...)}
	var appendAdmission AppendAdmission
	if l.beforeDurable != nil {
		appendAdmission, err = l.beforeDurable(durableBatch)
		if err != nil {
			return AppendReceipt{}, fmt.Errorf("admit metadata frame before durable append: %w", err)
		}
		durableBatch.Admission = appendAdmission
	}
	retainUncertain := func() {
		if appendAdmission != nil {
			l.uncertainAdmissions = append(l.uncertainAdmissions, appendAdmission)
			appendAdmission = nil
		}
	}
	if err := writeFull(l.durability, frame); err != nil {
		retainUncertain()
		l.terminalErr = err
		return AppendReceipt{}, fmt.Errorf("append metadata journal: %w", err)
	}
	if err := l.durability.Sync(); err != nil {
		retainUncertain()
		l.terminalErr = err
		return AppendReceipt{}, fmt.Errorf("sync metadata journal: %w", err)
	}
	record := Record{
		FormatVersion: version,
		Kind:          KindOperations, Epoch: l.epoch, Sequence: sequence,
		Ops: ops, Hash: hash, Offset: l.offset,
	}
	l.sequence, l.offset, l.lastHash = sequence, l.offset+int64(len(frame)), hash
	replicationIndex := uint64(0)
	if l.afterDurable != nil {
		var err error
		replicationIndex, err = l.afterDurable(durableBatch)
		if err != nil {
			l.terminalErr = err
			return AppendReceipt{}, fmt.Errorf("record durable metadata batch for replication: %w", err)
		}
		if replicationIndex == 0 {
			err = errors.New("metadata durable callback returned replication index zero")
			l.terminalErr = err
			return AppendReceipt{}, err
		}
	} else if appendAdmission != nil {
		_ = appendAdmission.Release()
	}
	return AppendReceipt{Record: record, ReplicationIndex: replicationIndex}, nil
}

// WaitConfirmed waits for the receipt's global index. It is deliberately a
// separate call so bbolt can commit before a network wait begins.
func (l *Log) WaitConfirmed(ctx context.Context, receipt AppendReceipt) error {
	if receipt.ReplicationIndex == 0 {
		return nil
	}
	l.mu.Lock()
	wait := l.waitConfirmed
	l.mu.Unlock()
	if wait == nil {
		return errors.New("metadata confirmation callback is not configured")
	}
	return wait(ctx, receipt.ReplicationIndex)
}

// Head is where the file ends right now.
func (l *Log) Head() Head {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Head{
		Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash,
		TrimmedThrough: l.trimmedThrough,
		FormatVersion:  l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline,
	}
}

// ReplayRange visits a stable authenticated operations prefix while holding
// the append mutex. It is used by the Replica projection so a concurrent
// receive cannot expose a half-written frame to an independent descriptor.
func (l *Log) ReplayRange(after, through uint64, visit func(Record) error) error {
	return l.replayRange(context.Background(), after, through, visit, false)
}

// ReplayRangeValidated authenticates the complete durable descriptor before
// invoking any callback, retaining the append lock through both passes. Use it
// when callbacks commit independently and cannot roll back an entire range.
// Validation and replay still scan history; this is not an incremental cursor.
func (l *Log) ReplayRangeValidated(after, through uint64, visit func(Record) error) error {
	return l.ReplayRangeValidatedContext(context.Background(), after, through, visit)
}

func (l *Log) ReplayRangeValidatedContext(ctx context.Context, after, through uint64, visit func(Record) error) error {
	return l.replayRange(ctx, after, through, visit, true)
}

func (l *Log) replayRange(ctx context.Context, after, through uint64, visit func(Record) error, validateFirst bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startupAudit != nil && !l.selectedStartupStructuralReadyLocked() {
		return ErrStartupAuditPending
	}
	if err := l.availabilityError(); err != nil {
		return err
	}
	if through < after || through > l.sequence || after < l.trimmedThrough {
		return errors.New("metadata replay range is outside the durable journal")
	}
	validate := func(head Head, partial bool, err error) error {
		if l.formatVersion == boundFrameVersion {
			if e := l.checkPreparedFile(); e != nil {
				return e
			}
		}
		if err != nil {
			return err
		}
		if partial || head.Epoch != l.epoch || head.Sequence != l.sequence || head.Hash != l.lastHash || head.Offset != l.offset || head.TrimmedThrough != l.trimmedThrough || head.FormatVersion != l.formatVersion || head.FileRevision != l.fileRevision || head.Baseline != l.baseline {
			return fmt.Errorf("%w: metadata replay snapshot does not match durable head", ErrCorrupt)
		}
		return nil
	}
	if l.formatVersion == boundFrameVersion {
		if e := l.checkPreparedFile(); e != nil {
			return e
		}
	}
	if validateFirst {
		head, partial, err := scanContext(ctx, l.file, l.key, nil)
		if err := validate(head, partial, err); err != nil {
			return err
		}
	}
	head, partial, err := scanContext(ctx, l.file, l.key, func(record Record) error {
		if record.Kind == KindOperations && record.Sequence > after && record.Sequence <= through && visit != nil {
			return visit(record)
		}
		return nil
	})
	return validate(head, partial, err)
}

func (l *Log) Close() error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	l.mu.Lock()
	if l.closed {
		defer l.mu.Unlock()
		if l.closeErr != nil {
			return l.closeErr
		}
		return l.retirementErr
	}
	l.closing = true
	l.cancelBoundPhysicalReceiveLocked()
	auditJob := l.startupAudit
	if auditJob != nil {
		auditJob.Cancel()
	}
	if l.freezeCancel != nil {
		l.freezeCancel()
	}
	l.mu.Unlock()
	if auditJob != nil {
		_ = auditJob.Wait(context.Background())
	}
	l.freezeWG.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if err := l.closePhysicalNativeLocked(); err != nil {
		l.closeErr = l.joinPhysicalFailureDescriptorsLocked(err)
		return l.closeErr
	}
	if err := l.closeCurrent(); err != nil {
		l.closeErr = err
		return err
	}
	if l.startupDebt != (resourcebudget.Permit{}) {
		return l.closeErr
	}
	if l.startupReaderDebt != nil {
		l.closeErr = errors.Join(l.terminalErr, l.retirementErr)
		return l.closeErr
	}
	for _, admission := range l.uncertainAdmissions {
		if admission != nil {
			if err := admission.Release(); err != nil {
				l.closeErr = err
				return err
			}
		}
	}
	l.uncertainAdmissions = nil
	clear(l.key)
	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for written := 0; written < len(data); {
		n, err := writer.Write(data[written:])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

// Replay visits every authenticated frame in the file, in order.
func Replay(path string, key []byte, visit func(Record) error) (Head, error) {
	return ReplayContext(context.Background(), path, key, visit)
}

func ReplayContext(ctx context.Context, path string, key []byte, visit func(Record) error) (result Head, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Head{}, err
	}
	defer func() {
		if err := closeDescriptor(file); err != nil {
			result = Head{}
			resultErr = errors.Join(resultErr, err)
		}
	}()
	head, partial, err := scanContext(ctx, file, key, visit)
	if err != nil {
		return Head{}, err
	}
	if partial {
		return head, fmt.Errorf("%w: partial final frame at offset %d", ErrCorrupt, head.Offset)
	}
	return head, nil
}

// Verify authenticates the whole chain without decoding anything a caller
// asked for. It is what `halro doctor` and `backup verify` call.
func Verify(path string, key []byte) (Head, error) {
	return VerifyContext(context.Background(), path, key)
}

func VerifyContext(ctx context.Context, path string, key []byte) (Head, error) {
	return ReplayContext(ctx, path, key, nil)
}

// scan authenticates frames from the start of the file. The boolean result is
// a torn final frame, which is a crash rather than corruption; every other
// disagreement is ErrCorrupt.
func scan(file io.ReadSeeker, key []byte, visit func(Record) error) (Head, bool, error) {
	return scanContext(context.Background(), file, key, visit)
}

func scanContext(ctx context.Context, file io.ReadSeeker, key []byte, visit func(Record) error) (Head, bool, error) {
	return scanFromContext(ctx, file, key, Head{}, true, visit)
}

func scanFrom(file io.ReadSeeker, key []byte, head Head, first bool, visit func(Record) error) (Head, bool, error) {
	return scanFromContext(context.Background(), file, key, head, first, visit)
}

func scanFromContext(ctx context.Context, file io.ReadSeeker, key []byte, head Head, first bool, visit func(Record) error) (Head, bool, error) {
	if err := ctx.Err(); err != nil {
		return Head{}, false, err
	}
	if len(key) != KeySize {
		return Head{}, false, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Head{}, false, err
	}
	var headerStorage [frameHeaderSize]byte
	header := headerStorage[:]
	var tail []byte
	mac := hmac.New(sha256.New, key)
	frameHash := sha256.New()
	var frameMAC [sha256.Size]byte
	for {
		if err := ctx.Err(); err != nil {
			return Head{}, false, err
		}
		n, err := readFullContext(ctx, file, header)
		if errors.Is(err, io.EOF) && n == 0 {
			if first {
				// An empty file is not an empty journal: every file starts
				// with an epoch header or a trim anchor, so nothing here means
				// the file was created by something that is not this writer.
				return Head{}, false, fmt.Errorf("%w: no epoch header", ErrCorrupt)
			}
			return head, false, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return head, true, nil
		}
		if err != nil {
			return Head{}, false, err
		}
		if string(header[:4]) != frameMagic || header[4] != frameVersion && header[4] != boundFrameVersion {
			return Head{}, false, fmt.Errorf("%w at offset %d: invalid header", ErrCorrupt, head.Offset)
		}
		version := header[4]
		if version == boundFrameVersion && (header[6] != 0 || header[7] != 0) {
			return Head{}, false, fmt.Errorf("%w: bound metadata reserved header bytes", ErrCorrupt)
		}
		if !first {
			expected := head.FormatVersion
			if expected == 0 {
				expected = frameVersion
			}
			if version != expected {
				return Head{}, false, fmt.Errorf("%w: metadata frame format changes within a file", ErrCorrupt)
			}
		}
		kind := Kind(header[5])
		if !kind.valid() {
			return Head{}, false, fmt.Errorf("%w at offset %d: unknown frame kind %d", ErrCorrupt, head.Offset, header[5])
		}
		epoch := binary.BigEndian.Uint64(header[8:16])
		sequence := binary.BigEndian.Uint64(header[16:24])
		payloadLength := binary.BigEndian.Uint32(header[24:28])
		if payloadLength > MaxPayloadSize {
			return Head{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, head.Offset)
		}
		length := int(payloadLength) + frameMACSize
		if cap(tail) < length {
			capacity := max(256, cap(tail))
			for capacity < length {
				capacity = min(capacity*2, MaxPayloadSize+frameMACSize)
			}
			tail = make([]byte, length, capacity)
		} else {
			tail = tail[:length]
		}
		if _, err := readFullContext(ctx, file, tail); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return head, true, nil
			}
			return Head{}, false, err
		}
		if err := ctx.Err(); err != nil {
			return Head{}, false, err
		}
		mac.Reset()
		mac.Write(header)
		mac.Write(tail[:payloadLength])
		if !hmac.Equal(mac.Sum(frameMAC[:0]), tail[payloadLength:]) {
			return Head{}, false, fmt.Errorf("%w at offset %d: invalid HMAC", ErrCorrupt, head.Offset)
		}
		if err := ctx.Err(); err != nil {
			return Head{}, false, err
		}
		record := Record{FormatVersion: version, Kind: kind, Epoch: epoch, Sequence: sequence, Offset: head.Offset}
		if err := decodePayload(kind, tail[:payloadLength], &record); err != nil {
			return Head{}, false, fmt.Errorf("%w at offset %d: %v", ErrCorrupt, head.Offset, err)
		}
		if err := validateBoundRecord(record, tail[:payloadLength]); err != nil {
			return Head{}, false, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if first {
			head.FormatVersion = version
			if record.Header.Baseline != nil {
				head.Baseline = *record.Header.Baseline
				head.FileRevision = record.Header.FileRevision
			}
			if record.Trim.Baseline != nil {
				head.Baseline = *record.Trim.Baseline
				head.FileRevision = record.Trim.FileRevision
			}
		}
		if err := checkPlacement(&head, first, kind, epoch, sequence, header[28:60]); err != nil {
			return Head{}, false, err
		}
		if err := ctx.Err(); err != nil {
			return Head{}, false, err
		}
		frameHash.Reset()
		frameHash.Write(header)
		frameHash.Write(tail)
		frameHash.Sum(record.Hash[:0])
		head.Epoch, head.Sequence, head.Hash = epoch, sequence, record.Hash
		head.Offset += int64(frameHeaderSize + len(tail))
		if visit != nil {
			if err := visit(record); err != nil {
				return Head{}, false, err
			}
		}
		first = false
		if kind == KindTrim {
			head.TrimmedThrough = record.Trim.TrimmedThrough
			head.Sequence = record.Trim.TrimmedThrough
			copy(head.Hash[:], record.Trim.ChainHeadAtTrim)
		}
	}
}

// Context checks bound cancellation latency to one actual Read of at most
// 64KiB plus one bounded frame's HMAC/JSON CPU work. No goroutine closes a reader
// in use, and cancellation never turns incomplete authentication into success.
const scanReadBytes = 64 << 10

func readFullContext(ctx context.Context, reader io.Reader, dst []byte) (int, error) {
	offset, emptyReads := 0, 0
	for offset < len(dst) {
		if err := ctx.Err(); err != nil {
			return offset, err
		}
		end := min(len(dst), offset+scanReadBytes)
		n, err := reader.Read(dst[offset:end])
		offset += n
		if ctxErr := ctx.Err(); ctxErr != nil {
			return offset, ctxErr
		}
		if offset == len(dst) {
			return offset, nil
		}
		if err != nil {
			if err == io.EOF && offset > 0 {
				err = io.ErrUnexpectedEOF
			}
			return offset, err
		}
		if n == 0 {
			emptyReads++
			if emptyReads == 100 {
				return offset, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
	return offset, ctx.Err()
}

// checkPlacement is where a file that authenticates frame by frame is still
// refused as a whole: a chain is only a chain if each frame follows the last,
// and only the first frame is allowed to open one.
func checkPlacement(head *Head, first bool, kind Kind, epoch, sequence uint64, previous []byte) error {
	switch {
	case first && kind == KindEpoch:
		if sequence != 0 {
			return fmt.Errorf("%w at offset %d: an epoch header is sequence 0", ErrCorrupt, head.Offset)
		}
		var zero [32]byte
		if !hmac.Equal(previous, zero[:]) {
			return fmt.Errorf("%w at offset %d: an epoch header opens a chain", ErrCorrupt, head.Offset)
		}
		return nil
	case first && kind == KindTrim:
		var zero [32]byte
		if !hmac.Equal(previous, zero[:]) {
			return fmt.Errorf("%w at offset %d: a trim anchor opens a chain", ErrCorrupt, head.Offset)
		}
		return nil
	case first:
		return fmt.Errorf("%w: the file opens with an operations frame and no epoch header", ErrCorrupt)
	case kind != KindOperations:
		// An epoch header or a trim anchor mid-file would mean two chains in
		// one file, and nothing can say which of them the projection follows.
		return fmt.Errorf("%w at offset %d: frame kind %d may only open a file", ErrCorrupt, head.Offset, kind)
	case epoch != head.Epoch:
		return fmt.Errorf("%w at offset %d: frame belongs to epoch %d, the file to %d",
			ErrCorrupt, head.Offset, epoch, head.Epoch)
	case head.Sequence == ^uint64(0):
		return fmt.Errorf("%w: metadata sequence overflow", ErrCorrupt)
	case sequence != head.Sequence+1:
		return fmt.Errorf("%w at offset %d: sequence %d does not follow %d",
			ErrCorrupt, head.Offset, sequence, head.Sequence)
	case !hmac.Equal(previous, head.Hash[:]):
		return fmt.Errorf("%w at offset %d: broken chain", ErrCorrupt, head.Offset)
	}
	return nil
}
