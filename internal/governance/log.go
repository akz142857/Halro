package governance

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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	frameMagic            = "HGOV"
	frameVersion          = 1
	frameHeaderSize       = 52
	frameMACSize          = sha256.Size
	maxPayloadSize        = 64 << 10
	governanceHMACKeySize = 32

	// The fixed retry window retains only fingerprints and native positions;
	// durable application idempotency belongs to the projected Manager.
	maxTrackedEventIDs = governancelimits.MaxCachedEventIDs
)

const MaxBatchEvents = 512
const MaxBatchPayloadBytes = 8 << 20

var ErrBatchLimit = errors.New("governance batch exceeds bounded working envelope")

var ErrCorrupt = errors.New("governance journal is corrupt")
var ErrJournalDiskLimit = errors.New("governance journal disk limit reached")
var ErrJournalDiskLimitInvalid = errors.New("governance journal disk limit is invalid")

type Event struct {
	EventID             string    `json:"event_id"`
	ProjectID           string    `json:"project_id"`
	WorkUnitID          string    `json:"work_unit_id"`
	DefinitionID        string    `json:"definition_id"`
	DefinitionVersion   uint64    `json:"definition_version"`
	OutcomeID           string    `json:"outcome_id"`
	Value               string    `json:"value"`
	ReporterKeyID       string    `json:"reporter_key_id"`
	EvidenceSHA256      string    `json:"evidence_sha256,omitempty"`
	EvidenceRef         string    `json:"evidence_ref,omitempty"`
	ObservedAt          time.Time `json:"observed_at"`
	IngestedAt          time.Time `json:"ingested_at"`
	SupersedesOutcomeID string    `json:"supersedes_outcome_id,omitempty"`
	Revision            uint64    `json:"revision"`
	IdempotencyKeyHash  string    `json:"idempotency_key_hash"`
	RequestFingerprint  string    `json:"request_fingerprint"`
}

func (e Event) Validate() error {
	outcome := domain.Outcome{
		ID: e.OutcomeID, ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID,
		DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion,
		Value: e.Value, ReporterKeyID: e.ReporterKeyID,
		EvidenceSHA256: e.EvidenceSHA256, EvidenceRef: e.EvidenceRef,
		ObservedAt: e.ObservedAt, IngestedAt: e.IngestedAt,
		SupersedesOutcomeID: e.SupersedesOutcomeID, Revision: e.Revision,
	}
	if strings.TrimSpace(e.EventID) == "" || !domain.ValidSHA256Label(e.IdempotencyKeyHash) ||
		!domain.ValidSHA256Label(e.RequestFingerprint) {
		return errors.New("governance event identity is invalid")
	}
	if e.IngestedAt.Location() != time.UTC {
		return errors.New("ingested_at must be UTC")
	}
	return outcome.Validate()
}

type Record struct {
	Sequence uint64   `json:"sequence"`
	Offset   int64    `json:"offset"`
	Event    Event    `json:"event"`
	Hash     [32]byte `json:"hash"`
}

type Summary struct {
	Records  uint64   `json:"records"`
	LastHash [32]byte `json:"last_hash"`
	Bytes    int64    `json:"bytes"`
}

const (
	journalAnchorVersion  = 1
	checkpointAuthVersion = 1
)

// JournalAnchorAuth authenticates a durable high-water mark kept outside the
// Journal. The external anchor is what makes deletion of an otherwise valid
// suffix observable; the per-frame chain alone can only authenticate the
// prefix that remains on disk.
func JournalAnchorAuth(key []byte, sequence uint64, offset int64, hash [32]byte) [32]byte {
	return governanceMAC(key, "halro:governance-journal-anchor:v1", journalAnchorVersion, sequence, offset, hash, [32]byte{})
}

func VerifyJournalAnchorAuth(key []byte, sequence uint64, offset int64, hash, authentication [32]byte) bool {
	expected := JournalAnchorAuth(key, sequence, offset, hash)
	return hmac.Equal(expected[:], authentication[:])
}

// CheckpointAuth binds a rebuildable snapshot to both its bytes and the exact
// authenticated Journal frame it projects through.
func CheckpointAuth(key []byte, sequence uint64, offset int64, journalHash, payloadHash [32]byte) [32]byte {
	return governanceMAC(key, "halro:governance-checkpoint:v1", checkpointAuthVersion, sequence, offset, journalHash, payloadHash)
}

func VerifyCheckpointAuth(key []byte, sequence uint64, offset int64, journalHash, payloadHash, authentication [32]byte) bool {
	expected := CheckpointAuth(key, sequence, offset, journalHash, payloadHash)
	return hmac.Equal(expected[:], authentication[:])
}

func governanceMAC(key []byte, domain string, version int, sequence uint64, offset int64, first, second [32]byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	var encoded [24]byte
	binary.BigEndian.PutUint64(encoded[0:8], uint64(version))
	binary.BigEndian.PutUint64(encoded[8:16], sequence)
	binary.BigEndian.PutUint64(encoded[16:24], uint64(offset))
	mac.Write(encoded[:])
	mac.Write(first[:])
	mac.Write(second[:])
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

type Log struct {
	physicalProjection  *haPhysicalGovernanceProjection
	physicalCancel      context.CancelFunc
	physical            *nativephysical.Window
	startupAuditPending bool
	startupSaved        Summary
	startupAudit        *StartupAudit
	startupAuditPtr     atomic.Pointer[StartupAudit]
	startupVerified     atomic.Bool
	startupFault        atomic.Bool
	startupClosed       atomic.Bool
	startupProvenance   *selectedGovernanceProvenance
	startupReaderDebt   *startupAuditReaderDebt
	startupDebt         resourcebudget.Permit
	cacheBudget         *resourcebudget.Manager

	freezeCancel        context.CancelFunc    // guarded by mu; one whole native read job
	freezeDebt          resourcebudget.Permit // retained after uncertain real retirement
	mu                  sync.Mutex
	closeMu             sync.Mutex
	readerWG            sync.WaitGroup
	closing             bool
	readerRetirementErr error
	closeReaderFile     func(*os.File) error
	closed              bool
	closeErr            error
	file                *os.File
	path                string
	durability          DurabilityWriter
	key                 []byte
	sequence            uint64
	offset              int64
	lastHash            [32]byte
	recordsByEventID    map[[sha256.Size]byte]cachedRecord
	trackedOrder        [][sha256.Size]byte
	trackedCount        int
	trackedNext         int
	cachePermit         resourcebudget.Permit
	cacheOwned          bool
	maxDiskBytes        int64
	afterDurable        func(DurableBatch) error
	beforeDurable       func(DurableBatch) (AppendAdmission, error)
	uncertainAdmissions []AppendAdmission
	replica             bool
	terminalErr         error
}

// No string or business payload escapes into the native retry cache.
// Fingerprints compare canonical durable JSON, including all Event fields.
type cachedRecord struct {
	PayloadHash [sha256.Size]byte
	Sequence    uint64
	Offset      int64
	Hash        [sha256.Size]byte
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

	// One lifetime reservation acquired before file IO/cache allocation, held
	// until native operations join, physical Close succeeds and references clear.
	CacheBudget   *resourcebudget.Manager
	CacheRAMBytes int64
	// MaxDiskBytes rejects complete native frames before writing when the
	// authenticated journal would exceed this size. Zero leaves the library
	// caller responsible for an external disk policy.
	MaxDiskBytes   int64
	WrapDurability func(*os.File) DurabilityWriter
	AfterDurable   func(DurableBatch) error
	BeforeDurable  func(DurableBatch) (AppendAdmission, error)
	Replica        bool
	// RequireExisting is the HA Primary mode: append and recover an existing
	// journal, but never manufacture a missing seed.
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
	if len(key) != governanceHMACKeySize {
		return nil, fmt.Errorf("audit HMAC key must be %d bytes", governanceHMACKeySize)
	}
	if options.MaxDiskBytes < 0 || options.MaxDiskBytes > int64(^uint(0)>>1) {
		return nil, ErrJournalDiskLimitInvalid
	}
	if options.BeforeDurable != nil && options.AfterDurable == nil {
		return nil, errors.New("governance append admission requires a durable replication owner")
	}
	var permit resourcebudget.Permit
	if options.CacheBudget != nil {
		if options.CacheRAMBytes < governancelimits.MinCacheRAMBytes {
			return nil, errors.New("Governance retry cache requires a calibrated RAM envelope")
		}
		var err error
		permit, err = options.CacheBudget.TryAcquireRecovery(resourcebudget.Cost{RAM: options.CacheRAMBytes})
		if err != nil {
			return nil, fmt.Errorf("Governance retry cache RAM admission: %w", err)
		}
	} else if options.CacheRAMBytes != 0 {
		return nil, errors.New("Governance retry cache RAM requires a shared manager")
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
			return nil, fmt.Errorf("create governance directory: %w", err)
		}
	}
	flags := os.O_RDWR
	if !options.Replica && !options.RequireExisting {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open governance journal: %w", err)
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
			return nil, errors.New("governance durability wrapper returned nil")
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
			return nil, fmt.Errorf("%w: replica governance journal has a partial tail that must be reconciled against ordering", ErrCorrupt)
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

func Verify(path string, key []byte) (result Summary, resultErr error) {
	if len(key) != governanceHMACKeySize {
		return Summary{}, fmt.Errorf("audit HMAC key must be %d bytes", governanceHMACKeySize)
	}
	file, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
		}
		if resultErr != nil {
			result = Summary{}
		}
	}()
	summary, partial, err := scan(file, key, nil)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	return summary, nil
}

func (l *Log) Append(ctx context.Context, event Event) (Record, error) {
	records, err := l.AppendBatch(ctx, []Event{event})
	if err != nil {
		return Record{}, err
	}
	return records[0], nil
}

// AppendBatch writes a consecutive hash-chain segment and makes it durable
// with one fsync. Either every returned record is durable or an error is
// returned and the log must be treated as unavailable by the caller.
func (l *Log) AppendBatch(ctx context.Context, events []Event) ([]Record, error) {
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
			return nil, errors.New("governance event exceeds maximum size")
		}
		payloadBytes += len(payload)
		if payloadBytes > MaxBatchPayloadBytes {
			return nil, ErrBatchLimit
		}
		identities[index] = sha256.Sum256([]byte(event.EventID))
		if first, seen := firstByID[identities[index]]; seen {
			if !bytes.Equal(payloads[first], payload) {
				return nil, errors.New("governance batch repeats event ID with different payload")
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
		return nil, errors.New("governance journal is closed")
	}
	if l.replica {
		return nil, errors.New("governance journal is replica-owned and refuses local append")
	}
	if l.startupAuditPending && (l.startupAudit == nil || !l.startupAudit.Snapshot().Ready) {
		return nil, ErrStartupAuditPending
	}
	if l.terminalErr != nil {
		return nil, fmt.Errorf("governance journal requires restart after uncertain persistence: %w", l.terminalErr)
	}
	initial := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	if err := l.verifyHeadLocked(ctx, initial); err != nil {
		if errors.Is(err, ErrCorrupt) {
			l.terminalErr = err
			l.startupFault.Store(true)
		}
		return nil, err
	}
	// Check every retry conflict before any standalone frame is written. The
	// per-frame append path below is safe only after the whole batch has passed
	// all caller-controlled validation that could otherwise reject later rows.
	for index, event := range events {
		if existing, ok := l.recordsByEventID[sha256.Sum256([]byte(event.EventID))]; ok && existing.PayloadHash != sha256.Sum256(payloads[index]) {
			return nil, errors.New("governance event ID conflicts with a different payload")
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
	collectReplication := l.afterDurable != nil || l.beforeDurable != nil
	for index, payload := range payloads {
		if first := firstByID[identities[index]]; first != index {
			records[index] = records[first]
			continue
		}
		if existing, ok := l.recordsByEventID[sha256.Sum256([]byte(events[index].EventID))]; ok {
			if existing.PayloadHash != sha256.Sum256(payload) {
				return nil, errors.New("governance event ID conflicts with a different payload")
			}
			records[index] = Record{Sequence: existing.Sequence, Offset: existing.Offset, Hash: existing.Hash, Event: events[index]}
			continue
		}
		sequence++
		frame, hash := encodeFrame(l.key, sequence, previous, payload)
		if firstSequence == 0 {
			firstSequence = sequence
		}
		// Native persistence is required in every role. HA callbacks need the
		// complete encoded batch, while standalone writes each bounded frame
		// directly instead of retaining a second history-sized batch buffer.
		if collectReplication {
			durableFrames = append(durableFrames, frame...)
		} else if err := writeFull(l.durability, frame); err != nil {
			l.terminalErr = err
			l.startupFault.Store(true)
			return nil, fmt.Errorf("append governance journal: %w", err)
		}
		offset += int64(len(frame))
		previous = hash
		records[index] = Record{Sequence: sequence, Offset: offset, Event: events[index], Hash: hash}
	}
	var durableBatch DurableBatch
	var appendAdmission AppendAdmission
	if len(durableFrames) > 0 {
		durableBatch = DurableBatch{Context: ctx, FirstSequence: firstSequence, LastSequence: sequence, Frames: append([]byte(nil), durableFrames...)}
		if l.beforeDurable != nil {
			var err error
			appendAdmission, err = l.beforeDurable(durableBatch)
			if err != nil {
				return nil, fmt.Errorf("admit governance batch before durable append: %w", err)
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
		physical, err := l.physicalBeforeWriteLocked(int64(len(durableFrames)), int(sequence-firstSequence+1))
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
			l.startupFault.Store(true)
			return nil, fmt.Errorf("append governance journal: %w", err)
		}
		if err := l.durability.Sync(); err != nil {
			if l.physical != nil {
				l.physical.Hold(err)
			}
			retainUncertain()
			l.terminalErr = err
			l.startupFault.Store(true)
			return nil, fmt.Errorf("sync governance journal: %w", err)
		}
		if physical != nil {
			if err := physical.Durable(); err != nil {
				l.terminalErr = err
				l.startupFault.Store(true)
				return nil, err
			}
		}
	}
	if len(durableFrames) == 0 {
		if err := l.durability.Sync(); err != nil {
			retainUncertain()
			l.terminalErr = err
			l.startupFault.Store(true)
			return nil, fmt.Errorf("sync governance journal: %w", err)
		}
	}
	l.sequence, l.offset, l.lastHash = sequence, offset, previous
	// Accepted durable work does not re-enter caller cancellation here.
	if err := l.verifyHeadLocked(context.Background(), Summary{Records: sequence, LastHash: previous, Bytes: offset}); err != nil {
		l.terminalErr = err
		l.startupFault.Store(true)
		return nil, err
	}
	for index := range records {
		if err := l.remember(records[index]); err != nil {
			l.terminalErr = err
			l.startupFault.Store(true)
			return nil, fmt.Errorf("cache durable Governance position: %w", err)
		}
	}
	if len(durableFrames) > 0 && l.afterDurable != nil {
		if err := l.afterDurable(durableBatch); err != nil {
			l.terminalErr = err
			l.startupFault.Store(true)
			return nil, fmt.Errorf("record durable governance batch for replication: %w", err)
		}
	} else if appendAdmission != nil {
		_ = appendAdmission.Release()
	}
	if err := l.verifyHeadLocked(context.Background(), Summary{Records: sequence, LastHash: previous, Bytes: offset}); err != nil {
		l.terminalErr = err
		l.startupFault.Store(true)
		return nil, err
	}
	return records, nil
}

func (l *Log) Replay(visit func(Record) error) (Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startupAudit != nil && !l.startupAudit.Snapshot().Ready {
		return Summary{}, ErrStartupAuditPending
	}
	if l.file == nil || l.closing {
		return Summary{}, errors.New("governance journal is closed")
	}
	summary, partial, err := scan(l.file, l.key, visit)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	return summary, nil
}

// VerifyHead checks a caller-frozen native head against this writer's actual
// descriptor and named file. Equal authenticated bytes in a replacement inode
// do not qualify: this writer would otherwise append to the detached old file.
// It grants no role/confirmation/resume permission and reads no history.
func (l *Log) VerifyHead(ctx context.Context, expected Summary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.verifyHeadLocked(ctx, expected)
}

func (l *Log) verifyHeadLocked(ctx context.Context, expected Summary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.file == nil || l.closing {
		return errors.New("governance journal is closed")
	}
	if (Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}) != expected {
		return fmt.Errorf("%w: governance native head changed", ErrCorrupt)
	}
	actual, err := l.file.Stat()
	if err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	named, err := os.Stat(l.path)
	if err != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != expected.Bytes || named.Size() != expected.Bytes {
		return errors.Join(ErrCorrupt, err)
	}
	return ctx.Err()
}

func (l *Log) Summary() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
}

func (l *Log) AuthenticatedHead() (Summary, [32]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	summary := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	return summary, JournalAnchorAuth(l.key, summary.Records, summary.Bytes, summary.LastHash)
}

func (l *Log) AuthenticatedCheckpoint(payload []byte) (Summary, [32]byte, [32]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	summary := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	payloadHash := sha256.Sum256(payload)
	authentication := CheckpointAuth(l.key, summary.Records, summary.Bytes, summary.LastHash, payloadHash)
	return summary, payloadHash, authentication
}

func (l *Log) Close() (resultErr error) {
	l.startupClosed.Store(true)
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	l.mu.Lock()
	if l.closed {
		err := l.closeErr
		l.mu.Unlock()
		return err
	}
	l.closed, l.closing = true, true
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
	l.readerWG.Wait()
	if auditJob != nil {
		_ = auditJob.Wait(context.Background())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() { l.closeErr = resultErr }()
	physicalErr := l.readerRetirementErr
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

func scan(file *os.File, key []byte, visit func(Record) error) (Summary, bool, error) {
	return scanFrom(file, key, Summary{}, visit)
}

func scanFrom(file io.ReadSeeker, key []byte, summary Summary, visit func(Record) error) (Summary, bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Summary{}, false, err
	}
	for {
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
		if err := json.Unmarshal(tail[:payloadLength], &event); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event", ErrCorrupt, summary.Bytes)
		}
		if err := event.Validate(); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, summary.Bytes, err)
		}
		frame := append(header, tail...)
		hash := sha256.Sum256(frame)
		summary.Records = sequence
		summary.Bytes += int64(len(frame))
		summary.LastHash = hash
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Offset: summary.Bytes, Event: event, Hash: hash}); err != nil {
				return Summary{}, false, err
			}
		}
	}
}

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
