package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
)

const orderingRecordBytes = 4 + orderingRecordBodyBytes

type OrderingDurability interface {
	io.Writer
	Sync() error
}

type OrderingJournalOptions struct {
	// WrapDurability is a test/integration seam for deterministic short-write,
	// ENOSPC, EIO and slow-fsync injection. Production leaves it nil.
	WrapDurability func(*os.File) OrderingDurability
	// SyncDirectory is a test seam for the journal file's directory-entry
	// durability barrier. Production leaves it nil.
	SyncDirectory func(string) error
}

// OrderingJournal owns the durable global order. Append returns only after the
// record bytes have been written and fsynced.
type OrderingJournal struct {
	mu         sync.Mutex
	file       *os.File
	durability OrderingDurability
	key        [sha256.Size]byte
	header     OrderingHeader
	dataOffset int64
	nextOffset int64
	lastIndex  uint64
	lastTerm   uint64
	head       [sha256.Size]byte
	closed     bool
	poisoned   error
}

// OpenOrderingJournal creates or authenticates an ordering journal. The
// expected values come from state.json. A journal may be ahead of state.json,
// but it may not be missing the complete authenticated prefix state.json says
// was durable. A partial final record is truncated and fsynced.
func OpenOrderingJournal(path string, key []byte, header OrderingHeader, expectedIndex uint64, expectedHead [sha256.Size]byte) (*OrderingJournal, error) {
	return OpenOrderingJournalWithOptions(path, key, header, expectedIndex, expectedHead, OrderingJournalOptions{})
}

func OpenOrderingJournalWithOptions(path string, key []byte, header OrderingHeader, expectedIndex uint64, expectedHead [sha256.Size]byte, options OrderingJournalOptions) (*OrderingJournal, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("ordering journal key must be 32 bytes")
	}
	if expectedIndex == 0 && expectedHead != ([sha256.Size]byte{}) {
		return nil, errors.New("ordering journal expected head requires a positive index")
	}
	if expectedIndex > 0 && expectedHead == ([sha256.Size]byte{}) {
		return nil, errors.New("ordering journal expected index requires a head MAC")
	}
	headerBytes, err := header.MarshalBinary(key)
	if err != nil {
		return nil, err
	}
	if err := ensureDurableDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create ordering journal directory: %w", err)
	}
	var file *os.File
	created := false
	if expectedIndex > 0 {
		file, err = os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open ordering journal required by member state: %w", err)
		}
	} else {
		file, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		created = err == nil
		if err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("create ordering journal: %w", err)
			}
			file, err = os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return nil, fmt.Errorf("open ordering journal: %w", err)
			}
		}
	}
	closeOnError := func(openErr error) (*OrderingJournal, error) {
		_ = file.Close()
		return nil, openErr
	}
	var durability OrderingDurability = file
	if options.WrapDurability != nil {
		durability = options.WrapDurability(file)
		if durability == nil {
			return closeOnError(errors.New("ordering journal durability wrapper returned nil"))
		}
	}
	if created {
		if err := writeAll(durability, headerBytes); err != nil {
			return closeOnError(fmt.Errorf("write ordering journal header: %w", err))
		}
		if err := durability.Sync(); err != nil {
			return closeOnError(fmt.Errorf("sync ordering journal header: %w", err))
		}
	} else {
		actualHeader := make([]byte, len(headerBytes))
		if _, err := io.ReadFull(file, actualHeader); err != nil {
			return closeOnError(fmt.Errorf("read ordering journal header: %w", err))
		}
		decoded, err := UnmarshalOrderingHeader(actualHeader, key)
		if err != nil {
			return closeOnError(err)
		}
		if decoded != header {
			return closeOnError(errors.New("ordering journal identity does not match configured cluster"))
		}
		// The file may exist because an earlier create wrote the header and then
		// failed its file-content barrier. Reading it from page cache proves its
		// authenticity, not its durability, so a retry must repeat file Sync too.
		if err := durability.Sync(); err != nil {
			return closeOnError(fmt.Errorf("sync existing ordering journal: %w", err))
		}
	}
	// Repeat the directory barrier even when the file already exists. A prior
	// create may have reached disk before its directory fsync failed, and that
	// uncertain publication must not turn into success on retry.
	syncDirectory := options.SyncDirectory
	if syncDirectory == nil {
		syncDirectory = durable.SyncDirectory
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return closeOnError(fmt.Errorf("sync ordering journal directory: %w", err))
	}

	journal := &OrderingJournal{
		file: file, durability: durability, header: header, dataOffset: int64(len(headerBytes)), nextOffset: int64(len(headerBytes)),
	}
	copy(journal.key[:], key)
	if err := journal.recover(expectedIndex, expectedHead); err != nil {
		return closeOnError(err)
	}
	return journal, nil
}

func (j *OrderingJournal) recover(expectedIndex uint64, expectedHead [sha256.Size]byte) error {
	if _, err := j.file.Seek(j.nextOffset, io.SeekStart); err != nil {
		return err
	}
	var expectedAtState [sha256.Size]byte
	for {
		start := j.nextOffset
		encoded := make([]byte, orderingRecordBytes)
		n, err := io.ReadFull(j.file, encoded)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if err := j.file.Truncate(start); err != nil {
				return fmt.Errorf("truncate partial ordering record: %w", err)
			}
			if err := j.file.Sync(); err != nil {
				return fmt.Errorf("sync truncated ordering journal: %w", err)
			}
			j.nextOffset = start
			break
		}
		if err != nil {
			return fmt.Errorf("read ordering record: %w", err)
		}
		if n != len(encoded) {
			return errors.New("ordering journal read returned a non-canonical record")
		}
		record, err := UnmarshalOrderingRecord(encoded, j.key[:], j.header.ClusterID, j.header.Incarnation, j.head)
		if err != nil {
			return fmt.Errorf("authenticate ordering record at offset %d: %w", start, err)
		}
		if record.Index != j.lastIndex+1 {
			return fmt.Errorf("ordering journal index %d does not continue %d", record.Index, j.lastIndex)
		}
		if record.Term < j.lastTerm {
			return fmt.Errorf("ordering journal term %d regresses from %d", record.Term, j.lastTerm)
		}
		if record.Term > j.lastTerm && record.Kind != KindLeadershipEstablished {
			return errors.New("ordering journal term does not begin with leadership_established")
		}
		if record.Term == j.lastTerm && record.Kind == KindLeadershipEstablished {
			return errors.New("ordering journal term contains a second leadership anchor")
		}
		j.lastIndex, j.lastTerm, j.head = record.Index, record.Term, record.MAC
		j.nextOffset += int64(len(encoded))
		if record.Index == expectedIndex {
			expectedAtState = record.MAC
		}
	}
	if j.lastIndex < expectedIndex {
		return errors.New("ordering journal is missing an authenticated suffix required by member state")
	}
	if expectedIndex > 0 && expectedAtState != expectedHead {
		return errors.New("ordering journal prefix does not match the head MAC in member state")
	}
	_, err := j.file.Seek(j.nextOffset, io.SeekStart)
	return err
}

func (j *OrderingJournal) Append(record OrderingRecord) (OrderingRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return OrderingRecord{}, errors.New("ordering journal is closed")
	}
	if j.poisoned != nil {
		return OrderingRecord{}, fmt.Errorf("ordering journal requires restart after an uncertain append: %w", j.poisoned)
	}
	if record.Index != j.lastIndex+1 {
		return OrderingRecord{}, fmt.Errorf("ordering record index %d does not continue %d", record.Index, j.lastIndex)
	}
	if record.Term < j.lastTerm {
		return OrderingRecord{}, fmt.Errorf("ordering record term %d regresses from %d", record.Term, j.lastTerm)
	}
	if record.Term > j.lastTerm && record.Kind != KindLeadershipEstablished {
		return OrderingRecord{}, errors.New("ordering record term must begin with leadership_established")
	}
	if record.Term == j.lastTerm && record.Kind == KindLeadershipEstablished {
		return OrderingRecord{}, errors.New("ordering record term already has a leadership anchor")
	}
	record.PreviousMAC = j.head
	encoded, head, err := record.MarshalBinaryAndMAC(j.key[:], j.header.ClusterID, j.header.Incarnation)
	if err != nil {
		return OrderingRecord{}, err
	}
	if err := writeAll(j.durability, encoded); err != nil {
		j.poisoned = err
		return OrderingRecord{}, fmt.Errorf("append ordering record: %w", err)
	}
	if err := j.durability.Sync(); err != nil {
		j.poisoned = err
		return OrderingRecord{}, fmt.Errorf("sync ordering record: %w", err)
	}
	record.MAC = head
	j.lastIndex, j.lastTerm, j.head = record.Index, record.Term, head
	j.nextOffset += int64(len(encoded))
	return record, nil
}

func (j *OrderingJournal) Head() (index, term uint64, mac [sha256.Size]byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastIndex, j.lastTerm, j.head
}

// BaselineCursors returns the authenticated per-store positions that existed
// before replication index one.
func (j *OrderingJournal) BaselineCursors() [4]StoreCursor {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.header.StoreCursors
}

// BaselineHeads returns the authenticated native chain heads paired with the
// index-zero cursors.
func (j *OrderingJournal) BaselineHeads() [4][sha256.Size]byte {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.header.StoreHeads
}

// StoreCursors reconstructs the logical native positions described by the
// durable ordering prefix. It deliberately does not inspect the native files:
// after a crash they may be one exact frame or Roll ahead because source fsync
// precedes ordering fsync. A Replica must resume from this logical position and
// let an exact Primary retransmission complete the interrupted transaction.
func (j *OrderingJournal) StoreCursors() ([4]StoreCursor, error) {
	cursors := j.BaselineCursors()
	head, _, _ := j.Head()
	for index := uint64(1); index <= head; index++ {
		record, err := j.Record(index)
		if err != nil {
			return [4]StoreCursor{}, err
		}
		switch record.Kind {
		case KindData:
			cursor := cursors[record.Store-StoreLedger]
			if cursor.Generation == 0 {
				if record.StoreSequenceFirst != 1 {
					return [4]StoreCursor{}, fmt.Errorf("ordering store %d begins at sequence %d", record.Store, record.StoreSequenceFirst)
				}
			} else if cursor.Generation != record.StoreGeneration || cursor.Sequence+1 != record.StoreSequenceFirst {
				return [4]StoreCursor{}, fmt.Errorf("ordering store %d range does not continue its durable cursor", record.Store)
			}
			cursors[record.Store-StoreLedger] = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
		case KindLedgerRoll:
			roll, err := DecodeLedgerRollMetadata(record.Metadata)
			if err != nil {
				return [4]StoreCursor{}, err
			}
			cursor := cursors[StoreLedger-StoreLedger]
			if cursor.Generation != roll.Generation || cursor.Sequence != roll.LastSequence {
				return [4]StoreCursor{}, errors.New("ordering Ledger Roll does not continue its durable cursor")
			}
			cursors[StoreLedger-StoreLedger] = StoreCursor{Generation: roll.Generation + 1, Sequence: roll.LastSequence}
		}
	}
	return cursors, nil
}

// Record authenticates and returns one already-durable record. Fixed-size
// version-1 records make this a bounded two-read lookup for retransmission and
// fork detection rather than an in-memory index that grows forever.
func (j *OrderingJournal) Record(index uint64) (OrderingRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return OrderingRecord{}, errors.New("ordering journal is closed")
	}
	if index == 0 || index > j.lastIndex {
		return OrderingRecord{}, errors.New("ordering journal index is out of range")
	}
	var previous [sha256.Size]byte
	if index > 1 {
		prior := make([]byte, orderingRecordBytes)
		priorOffset := j.dataOffset + int64(index-2)*orderingRecordBytes
		if _, err := j.file.ReadAt(prior, priorOffset); err != nil {
			return OrderingRecord{}, fmt.Errorf("read prior ordering record: %w", err)
		}
		copy(previous[:], prior[4+orderingRecordMACOffset:4+orderingRecordMACOffset+sha256.Size])
	}
	encoded := make([]byte, orderingRecordBytes)
	offset := j.dataOffset + int64(index-1)*orderingRecordBytes
	if _, err := j.file.ReadAt(encoded, offset); err != nil {
		return OrderingRecord{}, fmt.Errorf("read ordering record: %w", err)
	}
	record, err := UnmarshalOrderingRecord(encoded, j.key[:], j.header.ClusterID, j.header.Incarnation, previous)
	if err != nil {
		return OrderingRecord{}, err
	}
	if record.Index != index {
		return OrderingRecord{}, errors.New("ordering journal offset contains a different index")
	}
	return record, nil
}

func (j *OrderingJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	clear(j.key[:])
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	j.durability = nil
	return err
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
