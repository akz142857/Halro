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

	"github.com/akz142857/Halro/internal/durable"
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
	mu   sync.Mutex
	file *os.File
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
	trimmedThrough uint64
	afterDurable   func(DurableBatch) (uint64, error)
	waitConfirmed  func(context.Context, uint64) error
	replica        bool
	terminalErr    error
}

type DurableBatch struct {
	Epoch         uint64
	FirstSequence uint64
	LastSequence  uint64
	Frames        []byte
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
	if l.file == nil {
		return errors.New("metadata journal is closed")
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
}

// Open reads an existing journal, repairs a torn tail, and positions for
// append. A file that does not exist yet is not created here: a journal starts
// at an epoch, and StartEpoch is the call that says which one.
func Open(path string, key []byte) (*Log, error) {
	return open(path, key, false)
}

// OpenReplica opens an existing epoch for physical frame landing and refuses
// local semantic appends.
func OpenReplica(path string, key []byte) (*Log, error) {
	return open(path, key, true)
}

func open(path string, key []byte, replica bool) (*Log, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	head, partial, err := scan(file, key, nil)
	if err != nil {
		file.Close()
		return nil, err
	}
	// A torn tail is a crash during append, and it is repaired rather than
	// refused for the same reason the Audit log repairs one: the frame was
	// never acknowledged to anybody, because the bbolt transaction that would
	// have committed alongside it had not committed either. Anything the frame
	// described is therefore absent from the projection too.
	if partial {
		if err := file.Truncate(head.Offset); err != nil {
			file.Close()
			return nil, fmt.Errorf("truncate partial metadata journal tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, fmt.Errorf("sync repaired metadata journal tail: %w", err)
		}
	}
	if _, err := file.Seek(head.Offset, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return &Log{
		file: file, durability: durabilityFor(file),
		path: path, key: append([]byte(nil), key...),
		epoch: head.Epoch, sequence: head.Sequence, offset: head.Offset, lastHash: head.Hash,
		trimmedThrough: head.TrimmedThrough,
		replica:        replica,
	}, nil
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
	if len(key) != KeySize {
		return nil, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
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
	frame, hash := encodeFrame(key, KindEpoch, header.Epoch, 0, [32]byte{}, payload)
	if err := publish(path, frame); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(int64(len(frame)), io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return &Log{
		file: file, durability: durabilityFor(file),
		path: path, key: append([]byte(nil), key...),
		epoch: header.Epoch, sequence: 0, offset: int64(len(frame)), lastHash: hash,
	}, nil
}

// publish writes a whole file through a temporary and renames it, then fsyncs
// the directory so the rename itself is durable. Replacing the journal is one
// of the atomic-rename sequences internal/durable exists to serve; doing it by
// hand is how a data directory ends up with a name that points at nothing.
func publish(path string, contents []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".metadata-journal-*")
	if err != nil {
		return fmt.Errorf("create metadata journal: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
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
	if l.file == nil {
		return AppendReceipt{}, errors.New("metadata journal is closed")
	}
	if l.replica {
		return AppendReceipt{}, errors.New("metadata journal is replica-owned and refuses local append")
	}
	if l.terminalErr != nil {
		return AppendReceipt{}, fmt.Errorf("metadata journal requires restart after an uncertain durable callback: %w", l.terminalErr)
	}
	sequence := l.sequence + 1
	frame, hash := encodeFrame(l.key, KindOperations, l.epoch, sequence, l.lastHash, payload)
	if err := writeFull(l.durability, frame); err != nil {
		l.terminalErr = err
		return AppendReceipt{}, fmt.Errorf("append metadata journal: %w", err)
	}
	if err := l.durability.Sync(); err != nil {
		l.terminalErr = err
		return AppendReceipt{}, fmt.Errorf("sync metadata journal: %w", err)
	}
	record := Record{
		Kind: KindOperations, Epoch: l.epoch, Sequence: sequence,
		Ops: ops, Hash: hash, Offset: l.offset,
	}
	l.sequence, l.offset, l.lastHash = sequence, l.offset+int64(len(frame)), hash
	replicationIndex := uint64(0)
	if l.afterDurable != nil {
		var err error
		replicationIndex, err = l.afterDurable(DurableBatch{
			Epoch: l.epoch, FirstSequence: sequence, LastSequence: sequence,
			Frames: append([]byte(nil), frame...),
		})
		if err != nil {
			l.terminalErr = err
			return AppendReceipt{}, fmt.Errorf("record durable metadata batch for replication: %w", err)
		}
		if replicationIndex == 0 {
			err = errors.New("metadata durable callback returned replication index zero")
			l.terminalErr = err
			return AppendReceipt{}, err
		}
	}
	return AppendReceipt{Record: record, ReplicationIndex: replicationIndex}, nil
}

// WaitConfirmed waits for the receipt's global index. It is deliberately a
// separate call so bbolt can commit before a network wait begins.
func (l *Log) WaitConfirmed(ctx context.Context, receipt AppendReceipt) error {
	if receipt.ReplicationIndex == 0 {
		return nil
	}
	if l.waitConfirmed == nil {
		return errors.New("metadata confirmation callback is not configured")
	}
	return l.waitConfirmed(ctx, receipt.ReplicationIndex)
}

// Head is where the file ends right now.
func (l *Log) Head() Head {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Head{
		Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash,
		TrimmedThrough: l.trimmedThrough,
	}
}

// ReplayRange visits a stable authenticated operations prefix while holding
// the append mutex. It is used by the Replica projection so a concurrent
// receive cannot expose a half-written frame to an independent descriptor.
func (l *Log) ReplayRange(after, through uint64, visit func(Record) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return errors.New("metadata journal is closed")
	}
	if through < after || through > l.sequence || after < l.trimmedThrough {
		return errors.New("metadata replay range is outside the durable journal")
	}
	head, partial, err := scan(l.file, l.key, func(record Record) error {
		if record.Kind == KindOperations && record.Sequence > after && record.Sequence <= through && visit != nil {
			return visit(record)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if partial || head.Epoch != l.epoch || head.Sequence != l.sequence || head.Hash != l.lastHash {
		return fmt.Errorf("%w: metadata replay snapshot does not match durable head", ErrCorrupt)
	}
	return nil
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
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
	file, err := os.Open(path)
	if err != nil {
		return Head{}, err
	}
	defer file.Close()
	head, partial, err := scan(file, key, visit)
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
	return Replay(path, key, nil)
}

// scan authenticates frames from the start of the file. The boolean result is
// a torn final frame, which is a crash rather than corruption; every other
// disagreement is ErrCorrupt.
func scan(file io.ReadSeeker, key []byte, visit func(Record) error) (Head, bool, error) {
	return scanFrom(file, key, Head{}, true, visit)
}

func scanFrom(file io.ReadSeeker, key []byte, head Head, first bool, visit func(Record) error) (Head, bool, error) {
	if len(key) != KeySize {
		return Head{}, false, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Head{}, false, err
	}
	for {
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(file, header)
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
		if string(header[:4]) != frameMagic || header[4] != frameVersion {
			return Head{}, false, fmt.Errorf("%w at offset %d: invalid header", ErrCorrupt, head.Offset)
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
		tail := make([]byte, int(payloadLength)+frameMACSize)
		if _, err := io.ReadFull(file, tail); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return head, true, nil
			}
			return Head{}, false, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(header)
		mac.Write(tail[:payloadLength])
		if !hmac.Equal(mac.Sum(nil), tail[payloadLength:]) {
			return Head{}, false, fmt.Errorf("%w at offset %d: invalid HMAC", ErrCorrupt, head.Offset)
		}
		record := Record{Kind: kind, Epoch: epoch, Sequence: sequence, Offset: head.Offset}
		if err := decodePayload(kind, tail[:payloadLength], &record); err != nil {
			return Head{}, false, fmt.Errorf("%w at offset %d: %v", ErrCorrupt, head.Offset, err)
		}
		if err := checkPlacement(&head, first, kind, epoch, sequence, header[28:60]); err != nil {
			return Head{}, false, err
		}
		frame := append(header, tail...)
		record.Hash = sha256.Sum256(frame)
		head.Epoch, head.Sequence, head.Hash = epoch, sequence, record.Hash
		head.Offset += int64(len(frame))
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
	case sequence != head.Sequence+1:
		return fmt.Errorf("%w at offset %d: sequence %d does not follow %d",
			ErrCorrupt, head.Offset, sequence, head.Sequence)
	case !hmac.Equal(previous, head.Hash[:]):
		return fmt.Errorf("%w at offset %d: broken chain", ErrCorrupt, head.Offset)
	}
	return nil
}
