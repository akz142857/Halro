package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	frameMagic       = "HAUD"
	frameVersion     = 1
	frameHeaderSize  = 52
	frameMACSize     = sha256.Size
	maxPayloadSize   = 64 << 10
	auditHMACKeySize = 32

	// maxTrackedEventIDs bounds the in-memory dedup index. An event ID is only
	// ever re-appended within a short window of its first append — a retry
	// inside one request, or the startup drain re-delivering an intent that was
	// in flight when the process died — so a tail window answers every real
	// duplicate. Retaining the whole log instead would let an append-only file
	// pin every historical event, and its payload, in memory for the life of
	// the process.
	maxTrackedEventIDs = auditlimits.MaxCachedEventIDs
)

// Batch limits bound payload, result and replication encoding temporaries.
const MaxBatchEvents = 512
const MaxBatchPayloadBytes = 8 << 20

var ErrBatchLimit = errors.New("audit batch exceeds bounded working envelope")

var ErrCorrupt = errors.New("audit log is corrupt")
var ErrJournalDiskLimit = errors.New("audit journal disk limit reached")
var ErrJournalDiskLimitInvalid = errors.New("audit journal disk limit is invalid")

type Event struct {
	EventID       string         `json:"event_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	ActorType     string         `json:"actor_type"`
	ActorID       string         `json:"actor_id,omitempty"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type,omitempty"`
	TargetID      string         `json:"target_id,omitempty"`
	Outcome       string         `json:"outcome"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

func (e Event) Validate() error {
	if err := e.Preflight(maxPayloadSize); err != nil {
		return err
	}
	var problems []error
	for name, value := range map[string]string{
		"event ID": e.EventID, "actor type": e.ActorType,
		"action": e.Action, "outcome": e.Outcome,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	if e.OccurredAt.IsZero() {
		problems = append(problems, errors.New("occurred at is required"))
	}
	if e.OccurredAt.Location() != time.UTC {
		problems = append(problems, errors.New("occurred at must be UTC"))
	}
	if len(e.CorrelationID) > 256 {
		problems = append(problems, errors.New("correlation ID exceeds maximum size"))
	}
	if e.Metadata != nil {
		encoded, err := json.Marshal(e.Metadata)
		if err != nil || len(encoded) > 8<<10 {
			problems = append(problems, errors.New("audit metadata is invalid or too large"))
		}
	}
	return errors.Join(problems...)
}

type Record struct {
	Sequence uint64   `json:"sequence"`
	Event    Event    `json:"event"`
	Hash     [32]byte `json:"hash"`
	Offset   int64    `json:"-"`
}

type Summary struct {
	Records  uint64   `json:"records"`
	LastHash [32]byte `json:"last_hash"`
	Bytes    int64    `json:"bytes"`
}

type cachedRecord struct {
	PayloadHash [sha256.Size]byte
	Sequence    uint64
	Offset      int64
	Hash        [sha256.Size]byte
}

type Log struct {
	physicalCancel      context.CancelFunc
	physical            *nativephysical.Window
	commandBorrow       *commandBorrow
	startupAuditPending bool
	startupSaved        Summary
	startupProvenance   *selectedAuditProvenance
	startupAudit        *StartupAudit
	startupReaderDebt   *startupAuditReaderDebt
	startupDebt         resourcebudget.Permit
	cacheBudget         *resourcebudget.Manager

	freezeCancel              context.CancelFunc    // guarded by mu; one whole native read job
	freezeDebt                resourcebudget.Permit // retained after uncertain real retirement
	closeMu                   sync.Mutex
	replayWG                  sync.WaitGroup
	closing                   bool
	closed                    bool
	closeErr                  error
	replayRetirementErr       error
	closeReplayFile           func(*os.File) error
	mu                        sync.Mutex
	file                      *os.File
	path                      string
	durability                DurabilityWriter
	key                       []byte
	sequence                  uint64
	offset                    int64
	lastHash                  [32]byte
	recordsByEventID          map[[sha256.Size]byte]cachedRecord
	trackedOrder              [][sha256.Size]byte
	trackedCount, trackedNext int
	cachePermit               resourcebudget.Permit
	cacheOwned                bool
	afterDurable              func(DurableBatch) error
	beforeDurable             func(DurableBatch) (AppendAdmission, error)
	uncertainAdmissions       []AppendAdmission
	replica                   bool
	terminalErr               error
	maxDiskBytes              int64
}

type DurabilityWriter interface {
	io.Writer
	Sync() error
}

type DurableBatch struct {
	Context       context.Context
	FirstSequence uint64
	LastSequence  uint64
	Frames        []byte
	Admission     AppendAdmission
}

type AppendAdmission interface{ Release() error }

type Options struct {
	selectedStartup *selectedNativeStartup

	// Cache RAM is retained through native file close and removal of every
	// cache reference. Zero leaves library admission to the caller.
	CacheBudget    *resourcebudget.Manager
	CacheRAMBytes  int64
	WrapDurability func(*os.File) DurabilityWriter
	AfterDurable   func(DurableBatch) error
	BeforeDurable  func(DurableBatch) (AppendAdmission, error)
	Replica        bool
	// MaxDiskBytes rejects complete native frames before writing when the
	// authenticated journal would exceed this size. Zero leaves the library
	// caller responsible for an external disk policy.
	MaxDiskBytes int64
	// RequireExisting is the HA Primary mode: allow normal append and torn-tail
	// repair, but never turn a missing seed into a new log.
	RequireExisting bool
}

// Caller holds l.mu, or has the only unpublished reference during Open.
// Duplicate reads do not move their original insertion slot. The fixed ring
// never retains old strings or append-grown backing arrays after eviction.
func (l *Log) remember(record Record) error {
	payload, err := json.Marshal(record.Event)
	if err != nil {
		return err
	}
	identity := sha256.Sum256([]byte(record.Event.EventID))
	if _, tracked := l.recordsByEventID[identity]; !tracked {
		if l.trackedCount == maxTrackedEventIDs {
			delete(l.recordsByEventID, l.trackedOrder[l.trackedNext])
		} else {
			l.trackedCount++
		}
		l.trackedOrder[l.trackedNext] = identity
		l.trackedNext = (l.trackedNext + 1) % maxTrackedEventIDs
	}
	l.recordsByEventID[identity] = cachedRecord{PayloadHash: sha256.Sum256(payload), Sequence: record.Sequence, Offset: record.Offset, Hash: record.Hash}
	return nil
}

func Open(path string, key []byte) (*Log, error) {
	return OpenWithOptions(path, key, Options{})
}

func OpenWithOptions(path string, key []byte, options Options) (result *Log, resultErr error) {
	if len(key) != auditHMACKeySize {
		return nil, fmt.Errorf("audit HMAC key must be %d bytes", auditHMACKeySize)
	}
	if options.MaxDiskBytes < 0 || options.MaxDiskBytes > int64(^uint(0)>>1) {
		return nil, ErrJournalDiskLimitInvalid
	}
	if options.BeforeDurable != nil && options.AfterDurable == nil {
		return nil, errors.New("audit append admission requires a durable replication owner")
	}
	var permit resourcebudget.Permit
	if options.CacheBudget != nil {
		if options.CacheRAMBytes < auditlimits.MinCacheRAMBytes {
			return nil, errors.New("Audit retry cache requires a calibrated RAM envelope")
		}
		var err error
		permit, err = options.CacheBudget.TryAcquireRecovery(resourcebudget.Cost{RAM: options.CacheRAMBytes})
		if err != nil {
			return nil, fmt.Errorf("Audit retry cache RAM admission: %w", err)
		}
	} else if options.CacheRAMBytes != 0 {
		return nil, errors.New("Audit retry cache RAM requires a shared manager")
	}
	var file *os.File
	var log *Log
	transferred := false
	defer func() {
		if transferred {
			return
		}
		if log != nil {
			resultErr = errors.Join(resultErr, log.Close())
			if options.selectedStartup != nil && errors.Is(resultErr, ownedfile.ErrRetirement) {
				options.selectedStartup.retainedLog = log
			}
			return
		}
		if file != nil {
			if err := file.Close(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
				if options.selectedStartup != nil {
					options.selectedStartup.retainedLog = &Log{file: file, path: path, key: append([]byte(nil), key...), closed: true, closing: true, closeErr: resultErr, terminalErr: resultErr, startupAuditPending: true, cachePermit: permit, cacheOwned: options.CacheBudget != nil, cacheBudget: options.CacheBudget}
				}
				return // physical retirement is unproven; keep the charge
			}
		}
		if options.CacheBudget != nil {
			resultErr = errors.Join(resultErr, permit.Release())
		}
	}()
	if !options.Replica && !options.RequireExisting {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create audit directory: %w", err)
		}
	}
	flags := os.O_RDWR
	if !options.Replica && !options.RequireExisting {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit journal: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if options.MaxDiskBytes > 0 && info.Size() > options.MaxDiskBytes {
		return nil, ErrJournalDiskLimit
	}
	var durability DurabilityWriter = file
	if options.WrapDurability != nil {
		durability = options.WrapDurability(file)
		if durability == nil {
			return nil, errors.New("audit durability wrapper returned nil")
		}
	}
	log = &Log{file: file, path: path, durability: durability, recordsByEventID: make(map[[sha256.Size]byte]cachedRecord, maxTrackedEventIDs), trackedOrder: make([][sha256.Size]byte, maxTrackedEventIDs), afterDurable: options.AfterDurable, beforeDurable: options.BeforeDurable, replica: options.Replica, cachePermit: permit, cacheOwned: options.CacheBudget != nil, maxDiskBytes: options.MaxDiskBytes, cacheBudget: options.CacheBudget, startupAuditPending: options.selectedStartup != nil}
	var summary Summary
	var partial bool
	if start := options.selectedStartup; start != nil {
		if info.Size() < start.cache.head.Bytes {
			return nil, ErrStartupCache
		}
		if err := start.cache.restore(log); err != nil {
			return nil, err
		}
		summary, partial, err = scanFrom(startupContextSeeker{ctx: start.ctx, reader: io.NewSectionReader(file, start.cache.head.Bytes, info.Size()-start.cache.head.Bytes)}, key, start.cache.head, func(record Record) error { start.decoded++; return log.remember(record) })
		current, statErr := file.Stat()
		named, namedErr := os.Lstat(path)
		if statErr != nil || namedErr != nil || !named.Mode().IsRegular() || !os.SameFile(current, named) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return nil, errors.Join(ErrStartupCache, statErr, namedErr)
		}
	} else {
		summary, partial, err = scan(file, key, log.remember)
	}
	if err != nil {
		return nil, err
	}
	if log.maxDiskBytes > 0 && summary.Bytes > log.maxDiskBytes {
		return nil, ErrJournalDiskLimit
	}
	if partial {
		if options.Replica {
			return nil, fmt.Errorf("%w: replica audit journal has a partial tail that must be reconciled against ordering", ErrCorrupt)
		}
		if err := file.Truncate(summary.Bytes); err != nil {
			return nil, fmt.Errorf("truncate partial audit tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			return nil, fmt.Errorf("sync repaired audit tail: %w", err)
		}
	}
	if _, err := file.Seek(summary.Bytes, io.SeekStart); err != nil {
		return nil, err
	}
	log.key = append([]byte(nil), key...)
	log.sequence = summary.Records
	log.offset = summary.Bytes
	log.lastHash = summary.LastHash
	transferred = true
	return log, nil
}

func Verify(path string, key []byte) (Summary, error) {
	return VerifyWithVisitor(path, key, nil)
}

// VerifyWithVisitor authenticates the complete chain and visits each verified
// record. It is used when a caller must prove that a trusted checkpoint also
// contains a particular durable intent, rather than merely proving that the
// file is internally well formed.
func VerifyWithVisitor(path string, key []byte, visit func(Record) error) (Summary, error) {
	return VerifyWithVisitorContext(context.Background(), path, key, 0, visit)
}

func (l *Log) Append(ctx context.Context, event Event) (Record, error) {
	records, err := l.AppendBatch(ctx, []Event{event})
	if err != nil {
		return Record{}, err
	}
	return records[0], nil
}

// HasRetainedEventID is a startup scheduling hint over the fixed retry window.
// It is not historical membership or payload authentication: Append must still
// verify the full event. A startup caller uses this before any new appends so
// previously durable, undelivered receipts cannot be evicted by its own drain.
func (l *Log) HasRetainedEventID(eventID string) bool {
	if l.commandBorrow != nil {
		return l.commandBorrow.hasEvent(eventID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.recordsByEventID[sha256.Sum256([]byte(eventID))]
	return l.file != nil && !l.closing && ok
}

// AppendBatch writes a consecutive hash-chain segment and makes it durable
// with one fsync. Either every returned record is durable or an error is
// returned and the log must be treated as unavailable by the caller.
func (l *Log) AppendBatch(ctx context.Context, events []Event) ([]Record, error) {
	if l.commandBorrow != nil {
		return l.commandBorrow.appendBatch(ctx, events)
	}
	var records []Record
	err := l.withPhysicalEncoder(ctx, func(ctx context.Context) error {
		var err error
		records, err = l.appendBatch(ctx, events)
		return err
	})
	return records, err
}
func (l *Log) appendBatch(ctx context.Context, events []Event) ([]Record, error) {
	if len(events) == 0 {
		return nil, nil
	}
	if len(events) > MaxBatchEvents {
		return nil, ErrBatchLimit
	}
	payloads := make([][]byte, len(events))
	identities := make([][sha256.Size]byte, len(events))
	firstByID := make(map[[sha256.Size]byte]int, len(events))
	payloadBytes := 0
	for index, event := range events {
		if err := event.Validate(); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if len(payload) > maxPayloadSize {
			return nil, errors.New("audit event exceeds maximum size")
		}
		payloadBytes += len(payload)
		if payloadBytes > MaxBatchPayloadBytes {
			return nil, ErrBatchLimit
		}
		identities[index] = sha256.Sum256([]byte(event.EventID))
		if first, seen := firstByID[identities[index]]; seen {
			if !bytes.Equal(payloads[first], payload) {
				return nil, errors.New("audit batch repeats event ID with different payload")
			}
		} else {
			firstByID[identities[index]] = index
		}
		payloads[index] = payload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return nil, ErrNativeFrozen
	}
	if l.file == nil || l.closing {
		return nil, errors.New("audit log is closed")
	}
	if l.optionsReplica() {
		return nil, errors.New("audit log is replica-owned and refuses local append")
	}
	if l.startupAuditPending && (l.startupAudit == nil || !l.startupAudit.Snapshot().Ready) {
		return nil, ErrStartupAuditPending
	}
	if l.terminalErr != nil {
		return nil, fmt.Errorf("audit log requires restart after an uncertain durable callback: %w", l.terminalErr)
	}
	// Reject every caller-controlled conflict before any native frame write.
	for index, identity := range identities {
		if existing, ok := l.recordsByEventID[identity]; ok && existing.PayloadHash != sha256.Sum256(payloads[index]) {
			return nil, errors.New("audit event ID conflicts with a different payload")
		}
	}
	if l.maxDiskBytes > 0 {
		remaining := l.maxDiskBytes - l.offset
		var required int64
		for index, payload := range payloads {
			if firstByID[identities[index]] != index {
				continue
			}
			if _, exists := l.recordsByEventID[sha256.Sum256([]byte(events[index].EventID))]; exists {
				continue
			}
			frameBytes := int64(frameHeaderSize + len(payload) + frameMACSize)
			if frameBytes > remaining-required {
				return nil, ErrJournalDiskLimit
			}
			required += frameBytes
		}
	}
	records := make([]Record, len(events))
	sequence, offset, previous := l.sequence, l.offset, l.lastHash
	firstSequence := uint64(0)
	var durableFrames []byte
	for index, payload := range payloads {
		if first := firstByID[identities[index]]; first != index {
			records[index] = records[first]
			continue
		}
		if existing, ok := l.recordsByEventID[sha256.Sum256([]byte(events[index].EventID))]; ok {
			if existing.PayloadHash != sha256.Sum256(payload) {
				return nil, errors.New("audit event ID conflicts with a different payload")
			}
			var nativeEvent Event
			if err := decodeAuditEvent(payload, &nativeEvent); err != nil {
				return nil, err
			}
			records[index] = Record{Sequence: existing.Sequence, Event: nativeEvent, Hash: existing.Hash, Offset: existing.Offset}
			continue
		}
		sequence++
		frame, hash := encodeFrame(l.key, sequence, previous, payload)
		if l.afterDurable != nil || l.beforeDurable != nil {
			if firstSequence == 0 {
				firstSequence = sequence
			}
			durableFrames = append(durableFrames, frame...)
		} else if err := writeFull(l.durability, frame); err != nil {
			l.terminalErr = err
			return nil, fmt.Errorf("append audit log: %w", err)
		}
		offset += int64(len(frame))
		records[index] = Record{Sequence: sequence, Event: events[index], Hash: hash, Offset: offset}
		previous = hash
	}
	var durableBatch DurableBatch
	var appendAdmission AppendAdmission
	if len(durableFrames) > 0 {
		durableBatch = DurableBatch{Context: ctx, FirstSequence: firstSequence, LastSequence: sequence, Frames: append([]byte(nil), durableFrames...)}
		if l.beforeDurable != nil {
			var err error
			appendAdmission, err = l.beforeDurable(durableBatch)
			if err != nil {
				return nil, fmt.Errorf("admit audit batch before durable append: %w", err)
			}
			durableBatch.Admission = appendAdmission
		}
	}
	retainUncertain := func() {
		if appendAdmission != nil {
			l.uncertainAdmissions = append(l.uncertainAdmissions, appendAdmission)
			appendAdmission = nil
		}
	}
	if len(durableFrames) > 0 {
		physical, err := l.physicalBeforeWriteLocked(int64(len(durableFrames)))
		if err != nil {
			if appendAdmission != nil {
				err = errors.Join(err, appendAdmission.Release())
			}
			return nil, err
		}
		if err := writeFull(l.durability, durableFrames); err != nil {
			if l.physical != nil {
				l.physical.Hold(err)
			}
			retainUncertain()
			l.terminalErr = err
			return nil, fmt.Errorf("append audit log: %w", err)
		}
		if err := l.durability.Sync(); err != nil {
			if l.physical != nil {
				l.physical.Hold(err)
			}
			retainUncertain()
			l.terminalErr = err
			return nil, fmt.Errorf("sync audit log: %w", err)
		}
		if physical != nil {
			if err := physical.Durable(); err != nil {
				l.terminalErr = err
				return nil, err
			}
		}
	}
	if len(durableFrames) == 0 {
		if err := l.durability.Sync(); err != nil {
			retainUncertain()
			l.terminalErr = err
			return nil, fmt.Errorf("sync audit log: %w", err)
		}
	}
	l.sequence, l.offset, l.lastHash = sequence, offset, previous
	for index := range records {
		if err := l.remember(records[index]); err != nil {
			l.terminalErr = err
			return nil, fmt.Errorf("index durable audit event: %w", err)
		}
	}
	if len(durableFrames) > 0 && l.afterDurable != nil {
		if err := l.afterDurable(durableBatch); err != nil {
			l.terminalErr = err
			return nil, fmt.Errorf("record durable audit batch for replication: %w", err)
		}
	} else if appendAdmission != nil {
		_ = appendAdmission.Release()
	}
	return records, nil
}

func (l *Log) Replay(visit func(Record) error) (Summary, error) {
	return l.ReplayContext(context.Background(), visit)
}

// ReplayContext authenticates a captured durable prefix while joining cancellation
// and reader retirement; the visitor must not retain unbounded history.
func (l *Log) ReplayContext(ctx context.Context, visit func(Record) error) (result Summary, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if l.commandBorrow != nil {
		return l.commandBorrow.replay(ctx, visit)
	}
	l.mu.Lock()
	if l.startupAudit != nil && !l.startupAudit.Snapshot().Ready {
		l.mu.Unlock()
		return Summary{}, ErrStartupAuditPending
	}
	if l.file == nil || l.closing {
		l.mu.Unlock()
		return Summary{}, errors.New("audit log is closed")
	}
	// Appends are serialized on l.file, but replay does not need to hold that
	// lock while it authenticates and decodes historical records. Capture the
	// last durable byte and read it through a separate descriptor: a concurrent
	// append starts after that boundary and therefore cannot change this view.
	// Opening the descriptor while locked also makes Close harmless to the
	// snapshot after the lock is released.
	sourceIdentity, err := l.file.Stat()
	if err != nil {
		l.mu.Unlock()
		return Summary{}, err
	}
	file, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		l.mu.Unlock()
		return Summary{}, err
	}
	closeFile := l.closeReplayFile
	if closeFile == nil {
		closeFile = (*os.File).Close
	}

	boundary := l.offset
	expected := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	key := append([]byte(nil), l.key...)
	l.replayWG.Add(1)
	l.mu.Unlock()
	defer l.replayWG.Done()
	defer func() {
		resultErr = errors.Join(resultErr, l.retireReader(file, closeFile))
		if resultErr != nil {
			result = Summary{}
		}
	}()
	defer clear(key)
	identity, err := file.Stat()
	if err != nil || !identity.Mode().IsRegular() || !os.SameFile(sourceIdentity, identity) || identity.Size() < boundary {
		return Summary{}, errors.Join(ErrCorrupt, err)
	}

	summary, partial, err := scanFromContext(ctx, io.NewSectionReader(file, 0, boundary), key, Summary{}, visit)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	if summary != expected {
		return Summary{}, fmt.Errorf("%w: replay snapshot does not match durable boundary", ErrCorrupt)
	}
	named, nameErr := os.Lstat(l.path)
	if nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(identity, named) {
		return Summary{}, errors.Join(ErrCorrupt, nameErr)
	}
	return summary, nil
}

func (l *Log) Summary() Summary {
	if l.commandBorrow != nil {
		return l.commandBorrow.summary()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
}

func (l *Log) Close() (resultErr error) {
	if l.commandBorrow != nil {
		return l.commandBorrow.close()
	}
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	l.mu.Lock()
	if l.closed {
		err := l.closeErr
		l.mu.Unlock()
		return err
	}
	l.closed = true
	l.closing = true
	if l.freezeCancel != nil {
		l.freezeCancel()
	}
	if l.physicalCancel != nil {
		l.physicalCancel()
	}
	if l.physical != nil {
		l.physical.Withdraw()
	}
	if l.startupAudit != nil {
		l.startupAudit.Cancel()
	}
	auditJob := l.startupAudit
	l.mu.Unlock()
	// Replay owns an independent descriptor. Withdraw first, then join it
	// without holding the append mutex; refund only after all native IO stops.
	l.replayWG.Wait()
	if auditJob != nil {
		_ = auditJob.Wait(context.Background())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() { l.closeErr = resultErr }()
	physicalErr := l.replayRetirementErr
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			physicalErr = errors.Join(physicalErr, ownedfile.ErrRetirement, err)
		} else {
			l.file = nil
		}
	}
	if l.physical != nil {
		physicalErr = errors.Join(physicalErr, l.physical.CloseAfterNative(physicalErr))
	}
	if physicalErr != nil {
		return physicalErr
	}

	l.durability = nil
	l.afterDurable = nil
	l.beforeDurable = nil
	for _, admission := range l.uncertainAdmissions {
		if admission != nil {
			if err := admission.Release(); err != nil {
				return err
			}
		}
	}
	l.uncertainAdmissions = nil
	clear(l.key)
	l.key = nil
	clear(l.recordsByEventID)
	l.recordsByEventID = nil
	clear(l.trackedOrder)
	l.trackedOrder = nil
	l.trackedCount, l.trackedNext = 0, 0
	if l.cacheOwned {
		if err := l.cachePermit.Release(); err != nil {
			return err
		}
		l.cacheOwned = false
		l.cachePermit = resourcebudget.Permit{}
	}
	return nil
}

func encodeFrame(key []byte, sequence uint64, previous [32]byte, payload []byte) ([]byte, [32]byte) {
	frame := make([]byte, frameHeaderSize+len(payload)+frameMACSize)
	copy(frame[:4], frameMagic)
	frame[4] = frameVersion
	binary.BigEndian.PutUint64(frame[8:16], sequence)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[20:52], previous[:])
	copy(frame[frameHeaderSize:], payload)
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[:frameHeaderSize+len(payload)])
	copy(frame[frameHeaderSize+len(payload):], mac.Sum(nil))
	return frame, sha256.Sum256(frame)
}

func scan(file io.ReadSeeker, key []byte, visit func(Record) error) (Summary, bool, error) {
	return scanFrom(file, key, Summary{}, visit)
}

func scanFrom(file io.ReadSeeker, key []byte, summary Summary, visit func(Record) error) (Summary, bool, error) {
	return scanFromContext(context.Background(), file, key, summary, visit)
}

func scanFromContext(ctx context.Context, file io.ReadSeeker, key []byte, summary Summary, visit func(Record) error) (Summary, bool, error) {
	return scanFromContextWithFrame(ctx, file, key, summary, func(record Record, _ int64, _ uint32) error {
		if visit == nil {
			return nil
		}
		return visit(record)
	})
}

func scanFromContextWithFrame(ctx context.Context, file io.ReadSeeker, key []byte, summary Summary, visit func(Record, int64, uint32) error) (Summary, bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Summary{}, false, err
	}
	frameHasher := sha256.New()
	var frameDigest [sha256.Size]byte
	for {
		if err := ctx.Err(); err != nil {
			return Summary{}, false, err
		}
		frameStart := summary.Bytes
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(file, header)
		if errors.Is(err, io.EOF) && n == 0 {
			return summary, false, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return summary, true, nil
		}
		if err != nil {
			return Summary{}, false, err
		}
		if string(header[:4]) != frameMagic || header[4] != frameVersion {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid header", ErrCorrupt, summary.Bytes)
		}
		sequence := binary.BigEndian.Uint64(header[8:16])
		if sequence != summary.Records+1 {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid sequence", ErrCorrupt, summary.Bytes)
		}
		if !hmac.Equal(header[20:52], summary.LastHash[:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: broken hash chain", ErrCorrupt, summary.Bytes)
		}
		payloadLength := binary.BigEndian.Uint32(header[16:20])
		if payloadLength > maxPayloadSize {
			return Summary{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, summary.Bytes)
		}
		tail := make([]byte, int(payloadLength)+frameMACSize)
		if _, err := io.ReadFull(file, tail); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return summary, true, nil
			}
			return Summary{}, false, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(header)
		mac.Write(tail[:payloadLength])
		if !hmac.Equal(mac.Sum(nil), tail[payloadLength:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid HMAC", ErrCorrupt, summary.Bytes)
		}
		var event Event
		if err := decodeAuditEvent(tail[:payloadLength], &event); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event", ErrCorrupt, summary.Bytes)
		}
		if err := event.Validate(); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, summary.Bytes, err)
		}
		frameHasher.Reset()
		_, _ = frameHasher.Write(header)
		_, _ = frameHasher.Write(tail)
		copy(frameDigest[:], frameHasher.Sum(frameDigest[:0]))
		hash := frameDigest
		summary.Records = sequence
		frameLength := frameHeaderSize + len(tail)
		summary.Bytes += int64(frameLength)
		summary.LastHash = hash
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Event: event, Hash: hash, Offset: summary.Bytes}, frameStart, uint32(frameLength)); err != nil {
				return Summary{}, false, err
			}
		}
	}
}

func (l *Log) optionsReplica() bool { return l != nil && l.replica }

func writeFull(writer io.Writer, value []byte) error {
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

// UseNumber preserves native metadata integers above float64's exact range.
// Validate the whole JSON value because Decoder.Decode alone accepts suffixes.
func decodeAuditEvent(payload []byte, event *Event) error {
	if !json.Valid(payload) {
		return errors.New("invalid audit event JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	return decoder.Decode(event)
}
