package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const orderingRecordBytes = 4 + orderingRecordBodyBytes

var ErrOrderingJournalDiskLimit = errors.New("ordering journal disk limit reached")

type OrderingDurability interface {
	io.Writer
	Sync() error
}

type OrderingJournalOptions struct {
	// MaxDiskBytes caps the logical authenticated file size. Zero disables it.
	MaxDiskBytes int64
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
	received          *PortableReceivedPublication
	physical          *PhysicalPublication
	ledgerReadMu      sync.Mutex
	ledgerReadClosing bool
	ledgerRead        *orderingLedgerReadOwner
	closeMu           sync.Mutex
	freezeWG          sync.WaitGroup
	freezeCancel      context.CancelFunc
	freezeDebt        resourcebudget.Permit
	closing           bool
	mu                sync.Mutex
	file              *os.File
	durability        OrderingDurability
	key               [sha256.Size]byte
	header            OrderingHeader
	dataOffset        int64
	nextOffset        int64
	lastIndex         uint64
	lastTerm          uint64
	head              [sha256.Size]byte
	closed            bool
	closeErr          error
	poisoned          error
	tailCursors       [4]StoreCursor
	tailCursorErr     error
	maxDiskBytes      int64
	invalidity        chan struct{}
	invalidated       bool
}

// OpenOrderingJournal creates or authenticates an ordering journal. The
// expected values come from state.json. A journal may be ahead of state.json,
// but it may not be missing the complete authenticated prefix state.json says
// was durable. A partial final record is truncated and fsynced.
func OpenOrderingJournal(path string, key []byte, header OrderingHeader, expectedIndex uint64, expectedHead [sha256.Size]byte) (*OrderingJournal, error) {
	return OpenOrderingJournalWithOptions(path, key, header, expectedIndex, expectedHead, OrderingJournalOptions{})
}

// ReadOrderingHeader authenticates the index-zero store baselines from an
// existing journal without deriving them again from current native tails.
func ReadOrderingHeader(path string, key []byte) (result OrderingHeader, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return OrderingHeader{}, fmt.Errorf("open existing ordering journal header: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
		}
		if resultErr != nil {
			result = OrderingHeader{}
		}
	}()
	encoded, err := ReadLengthDelimited(file, orderingHeaderFixedBytes+2*MaxIdentityBytes)
	if err != nil {
		return OrderingHeader{}, fmt.Errorf("read existing ordering journal header: %w", err)
	}
	header, err := UnmarshalOrderingHeader(encoded, key)
	if err != nil {
		return OrderingHeader{}, fmt.Errorf("authenticate existing ordering journal header: %w", err)
	}
	return header, nil
}

// OpenExistingOrderingJournal is the restart path. It never creates a journal
// and reuses the authenticated on-disk baseline before the normal full scan
// verifies expectedIndex and expectedHead against the record MAC chain.
func OpenExistingOrderingJournal(path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte) (*OrderingJournal, error) {
	return OpenExistingOrderingJournalWithOptions(path, key, clusterID, incarnation, expectedIndex, expectedHead, OrderingJournalOptions{})
}

func OpenExistingOrderingJournalWithOptions(path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte, options OrderingJournalOptions) (*OrderingJournal, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("ordering journal key must be 32 bytes")
	}
	if options.MaxDiskBytes < 0 {
		return nil, errors.New("ordering journal disk limit cannot be negative")
	}
	if expectedIndex == 0 && expectedHead != ([sha256.Size]byte{}) {
		return nil, errors.New("ordering journal expected head requires a positive index")
	}
	if expectedIndex > 0 && expectedHead == ([sha256.Size]byte{}) {
		return nil, errors.New("ordering journal expected index requires a head MAC")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open existing ordering journal: %w", err)
	}
	closeOnError := func(openErr error) (*OrderingJournal, error) {
		if closeErr := file.Close(); closeErr != nil {
			openErr = errors.Join(openErr, ownedfile.ErrRetirement, closeErr)
		}
		return nil, openErr
	}
	if options.MaxDiskBytes > 0 {
		info, statErr := file.Stat()
		if statErr != nil || info.Size() > options.MaxDiskBytes {
			return closeOnError(errors.Join(ErrOrderingJournalDiskLimit, statErr))
		}
	}
	encoded, err := ReadLengthDelimited(file, orderingHeaderFixedBytes+2*MaxIdentityBytes)
	if err != nil {
		return closeOnError(fmt.Errorf("read existing ordering journal header: %w", err))
	}
	header, err := UnmarshalOrderingHeader(encoded, key)
	if err != nil {
		return closeOnError(fmt.Errorf("authenticate existing ordering journal header: %w", err))
	}
	if header.ClusterID != clusterID || header.Incarnation != incarnation {
		return closeOnError(errors.New("ordering journal identity does not match authenticated member state"))
	}
	// A restart path never creates or replaces the file. Keep this same file
	// descriptor through header authentication and the complete record scan so
	// an unlink/replace race cannot fall through to the creation path.
	var durability OrderingDurability = file
	if options.WrapDurability != nil {
		durability = options.WrapDurability(file)
		if durability == nil {
			return closeOnError(errors.New("ordering journal durability wrapper returned nil"))
		}
	}
	if err := durability.Sync(); err != nil {
		return closeOnError(fmt.Errorf("sync existing ordering journal: %w", err))
	}
	syncDirectory := options.SyncDirectory
	if syncDirectory == nil {
		syncDirectory = durable.SyncDirectory
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return closeOnError(fmt.Errorf("sync ordering journal directory: %w", err))
	}
	journal := &OrderingJournal{
		file: file, durability: durability, header: header, maxDiskBytes: options.MaxDiskBytes,
		dataOffset: int64(len(encoded)), nextOffset: int64(len(encoded)),
		invalidity: make(chan struct{}),
	}
	copy(journal.key[:], key)
	if err := journal.recover(expectedIndex, expectedHead); err != nil {
		return closeOnError(err)
	}
	return journal, nil
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
	if options.MaxDiskBytes < 0 {
		return nil, errors.New("ordering journal disk limit cannot be negative")
	}
	headerBytes, err := header.MarshalBinary(key)
	if err != nil {
		return nil, err
	}
	if options.MaxDiskBytes > 0 && int64(len(headerBytes)) > options.MaxDiskBytes {
		return nil, fmt.Errorf("%w: ordering journal header exceeds configured ceiling", ErrOrderingJournalDiskLimit)
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
		if closeErr := file.Close(); closeErr != nil {
			openErr = errors.Join(openErr, ownedfile.ErrRetirement, closeErr)
		}
		return nil, openErr
	}
	if options.MaxDiskBytes > 0 && !created {
		info, statErr := file.Stat()
		if statErr != nil || info.Size() > options.MaxDiskBytes {
			return closeOnError(errors.Join(ErrOrderingJournalDiskLimit, statErr))
		}
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
		file: file, durability: durability, header: header, dataOffset: int64(len(headerBytes)), nextOffset: int64(len(headerBytes)), maxDiskBytes: options.MaxDiskBytes,
		invalidity: make(chan struct{}),
	}
	copy(journal.key[:], key)
	if err := journal.recover(expectedIndex, expectedHead); err != nil {
		return closeOnError(err)
	}
	return journal, nil
}

func (j *OrderingJournal) recover(expectedIndex uint64, expectedHead [sha256.Size]byte) error {
	j.tailCursors = j.header.StoreCursors
	j.tailCursorErr = nil
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
		if j.tailCursorErr == nil {
			j.tailCursors, j.tailCursorErr = advanceOrderingCursors(j.tailCursors, record)
			if j.tailCursorErr != nil {
				j.invalidateLocked()
			}
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
	return j.AppendWithPhysicalReservation(record, nil)
}

// AppendWithPhysicalReservation consumes only a reservation created by this
// actual journal. It changes neither record authentication nor confirmation.
func (j *OrderingJournal) AppendWithPhysicalReservation(record OrderingRecord, reservation *OrderingPhysicalReservation) (OrderingRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.freezeCancel != nil {
		return OrderingRecord{}, ErrOrderingFrozen
	}
	if j.closed || j.closing || j.file == nil {
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
	if j.maxDiskBytes > 0 && (j.nextOffset > j.maxDiskBytes || int64(len(encoded)) > j.maxDiskBytes-j.nextOffset) {
		return OrderingRecord{}, ErrOrderingJournalDiskLimit
	}
	if err := j.checkPhysicalAppendLocked(record, int64(len(encoded)), reservation); err != nil {
		return OrderingRecord{}, err
	}
	if err := writeAll(j.durability, encoded); err != nil {
		j.poisoned = err
		j.invalidateLocked()
		return OrderingRecord{}, fmt.Errorf("append ordering record: %w", err)
	}
	if err := j.durability.Sync(); err != nil {
		j.poisoned = err
		j.invalidateLocked()
		return OrderingRecord{}, fmt.Errorf("sync ordering record: %w", err)
	}
	record.MAC = head
	if j.tailCursorErr == nil {
		j.tailCursors, j.tailCursorErr = advanceOrderingCursors(j.tailCursors, record)
		if j.tailCursorErr != nil {
			j.invalidateLocked()
		}
	}
	if record.Term > j.lastTerm && !j.invalidated {
		close(j.invalidity)
		j.invalidity = make(chan struct{})
	}
	j.lastIndex, j.lastTerm, j.head = record.Index, record.Term, head
	j.nextOffset += int64(len(encoded))
	if reservation != nil {
		reservation.used = true
		j.physical.reserved--
	}
	return record, nil
}

func (j *OrderingJournal) Head() (index, term uint64, mac [sha256.Size]byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastIndex, j.lastTerm, j.head
}

func (j *OrderingJournal) appendUncertainty() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.poisoned
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
	head, _, _ := j.Head()
	return j.StoreCursorsThrough(head)
}

// StoreCursorsThrough reconstructs the native positions at one authenticated
// global index. Replica startup uses its persisted applied index rather than
// the physical tail so a durable-but-unconfirmed suffix is never projected.
func (j *OrderingJournal) StoreCursorsThrough(through uint64) ([4]StoreCursor, error) {
	return j.StoreCursorsThroughContext(context.Background(), through)
}

// StoreCursorsThroughContext bounds cancellation across the complete ordering
// prefix, before native projection construction has started. It retains only
// four cursors and leaves the journal/member state untouched on cancellation.
func (j *OrderingJournal) StoreCursorsThroughContext(ctx context.Context, through uint64) ([4]StoreCursor, error) {
	if err := ctx.Err(); err != nil {
		return [4]StoreCursor{}, err
	}
	cursors := j.BaselineCursors()
	head, _, _ := j.Head()
	if through > head {
		return [4]StoreCursor{}, errors.New("ordering cursor index is beyond the journal head")
	}
	for index := uint64(1); index <= through; index++ {
		if err := ctx.Err(); err != nil {
			return [4]StoreCursor{}, err
		}
		record, err := j.Record(index)
		if err != nil {
			return [4]StoreCursor{}, err
		}
		cursors, err = advanceOrderingCursors(cursors, record)
		if err != nil {
			return [4]StoreCursor{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return [4]StoreCursor{}, err
	}
	return cursors, nil
}

// Record authenticates and returns one already-durable record. Fixed-size
// version-1 records make this a bounded two-read lookup for retransmission and
// fork detection rather than an in-memory index that grows forever.
func (j *OrderingJournal) Record(index uint64) (OrderingRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recordLocked(index)
}

func (j *OrderingJournal) recordLocked(index uint64) (OrderingRecord, error) {
	if j.closed || j.closing || j.file == nil {
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
	j.closeMu.Lock()
	defer j.closeMu.Unlock()
	j.ledgerReadMu.Lock()
	j.ledgerReadClosing = true
	reader := j.ledgerRead
	if reader != nil {
		reader.cancel()
	}
	j.ledgerReadMu.Unlock()
	if reader != nil {
		<-reader.done
	}
	j.mu.Lock()
	if j.closed {
		err := j.closeErr
		j.mu.Unlock()
		return err
	}
	j.closing = true
	j.invalidateLocked()
	if j.freezeCancel != nil {
		j.freezeCancel()
	}
	j.mu.Unlock()
	j.freezeWG.Wait()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	if j.file != nil {
		if err := ownedfile.CloseFile(j.file); err != nil {
			j.closeErr = err
			return err
		}
		j.file = nil
	}
	j.durability = nil
	if j.physical != nil {
		if err := j.physical.closeOrderingLocked(); err != nil {
			j.closeErr = err
			return err
		}
	}
	clear(j.key[:])
	return nil
}

// Terminal invalidity wakes prefix waiters without calling back into the
// coordinator while holding the journal lock. The channel only rotates after
// a durable higher-term anchor; terminal failures close it permanently.
func (j *OrderingJournal) invalidateLocked() {
	if !j.invalidated {
		j.invalidated = true
		if j.invalidity != nil {
			close(j.invalidity)
		}
	}
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
