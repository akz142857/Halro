package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameMagic          = "HLDG"
	frameVersionLegacy  = 1
	frameVersionCurrent = 2
	// frameVersionPeriod marks events that carry the accounting period's own
	// identity — its zone, version and UTC bounds — so a reader can reconstruct
	// the boundary a charge was filed against without consulting any setting.
	frameVersionPeriod = 3
	// frameVersionLedgerIntegrity (ADR 0016) is epoch 4, not 3: the
	// accounting-timezone work already shipped epoch 3 for period identity
	// before this guarantee existed, so the MAC/chain guarantee starts one
	// epoch later than originally proposed. Every event this build writes is
	// promoted to this epoch — eventFrameVersion no longer branches by event
	// shape, so there is no window where some new frames are authenticated
	// and others are not.
	frameVersionLedgerIntegrity = 4
	frameHeaderSize             = 24
	// chainPreviousHashSize and chainMACSize are appended after the base
	// 24-byte header for epoch 4 only. The CRC32 field keeps its original
	// meaning and byte range (header[4:20]||payload) unchanged by their
	// presence — it is the cheap check that distinguishes a torn tail from a
	// corrupt frame, and it is evaluated before the MAC.
	chainPreviousHashSize = 32
	chainMACSize          = 32
	chainHeaderSize       = frameHeaderSize + chainPreviousHashSize + chainMACSize
	maxPayloadSize        = 1 << 20
)

var ErrCorrupt = errors.New("ledger is corrupt")
var ErrUnsupportedVersion = errors.New("ledger version is unsupported")

// ErrTampered is distinct from ErrCorrupt: a CRC failure in the tail
// position is a partial write (recovery repairs it by truncation); a frame
// that passes CRC and fails its MAC, or whose previous-hash does not match
// the running chain, means bytes that were once durably committed no longer
// match what the chain says they should be. That is not something an open
// can silently truncate past.
var ErrTampered = errors.New("ledger chain integrity check failed")

// visitError tags an error as the caller's, raised by the visit callback,
// rather than the log's own. scan cannot tell the two apart once it returns a
// bare error, and the distinction decides whether a failed replay condemns the
// ledger. Unwrap is preserved, so callers still match on their own sentinels
// (context.Canceled above all) exactly as before.
type visitError struct{ err error }

func (e visitError) Error() string { return e.err.Error() }
func (e visitError) Unwrap() error { return e.err }

func isCallerVisitError(err error) bool {
	var visit visitError
	return errors.As(err, &visit)
}

// chainVerifier carries the running chain state across a full scan from the
// start of the file. It is only meaningful when the scan begins at offset 0
// with sequence 0 — the only case where "no prior frame" (a zero previous
// hash) is a correct starting point. A verifier with no key present still
// walks epoch-4 frames structurally (to compute correct offsets) but does not
// check their MAC or chain link, matching the checksum-only treatment epochs
// 1-3 always get.
type chainVerifier struct {
	key       []byte
	hash      [32]byte
	sequence  uint64
	offset    int64
	sawFrames bool
}

func (v *chainVerifier) verify() bool { return v != nil && len(v.key) == 32 }

type Log struct {
	mu             sync.Mutex
	lifecycle      sync.RWMutex
	file           *os.File
	durability     DurabilityWriter
	path           string
	sequence       uint64
	offset         int64
	status         *Status
	options        Options
	appendQueue    chan appendRequest
	closeSignal    chan struct{}
	writerDone     chan struct{}
	closed         bool
	batches        atomic.Uint64
	writtenRecords atomic.Uint64
	appendErrors   atomic.Uint64
	// syncs and syncNanos accumulate the cost of the one durability barrier per
	// batch. Every throughput ceiling in this process is bounded by it, and its
	// cost differs by orders of magnitude between an APFS laptop and an NVMe
	// server, so a benchmark taken anywhere else is not transferable without it.
	syncs     atomic.Uint64
	syncNanos atomic.Int64

	// chainKey, chainHash and chainSequence carry the epoch-4 MAC/hash-chain
	// state forward from whatever OpenWithOptions established (the verified
	// tail of history, or the zero value for a brand new log) into every
	// subsequently appended frame. chainKey is empty when the log was opened
	// without Options.ChainKey; a nil-key log can still replay, but cannot
	// Append, since every new frame this build writes is epoch 4 and epoch 4
	// requires a MAC.
	chainKey       []byte
	chainHash      [32]byte
	chainSequence  uint64
	chainOffset    int64
	chainSawFrames bool
}

type Options struct {
	QueueCapacity int
	MaxBatch      int
	FlushInterval time.Duration
	// WrapDurability is an internal test/integration seam for deterministic
	// write and fsync fault injection. Production callers leave it nil.
	WrapDurability func(*os.File) DurabilityWriter
	// ChainKey is the 32-byte Ledger frame HMAC key (ADR 0016). It is
	// required to Append, since every event this build writes is epoch 4.
	// Read-only callers (offline inspection, best-effort tooling) may open
	// without it: epoch-4 frames are still decoded, just not authenticated —
	// the same checksum-only treatment epochs 1-3 always get.
	ChainKey []byte
}

// ChainHead reports the verified epoch-4 chain state as of the most recent
// Append (or, immediately after OpenWithOptions, as of the last frame seen
// during the initial tail scan). ok is false when no epoch-4 frame has ever
// been observed — a brand new log, or one that predates this build.
func (l *Log) ChainHead() (sequence uint64, offset int64, hash [32]byte, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.chainSequence, l.chainOffset, l.chainHash, l.chainSawFrames
}

type DurabilityWriter interface {
	io.Writer
	Sync() error
}

// AppendStats is serialized straight into the Admin API, so the tags are part
// of that payload's contract rather than cosmetic.
type AppendStats struct {
	Batches uint64 `json:"batches"`
	Records uint64 `json:"records"`
	Errors  uint64 `json:"errors"`
	// Syncs and SyncDuration describe the durability barrier. Records/Batches is
	// the mean group-commit size, which is what separates "this host's fsync is
	// the ceiling" from "not enough concurrent appenders to coalesce" — two
	// situations with the same symptom and opposite remedies.
	Syncs uint64 `json:"syncs"`
	// Carried as a duration in Go and as seconds on the wire: a time.Duration
	// marshals to a bare nanosecond integer, which reaches an operator as an
	// unreadable 11-digit number.
	SyncDuration  time.Duration `json:"-"`
	QueueDepth    int           `json:"queue_depth"`
	QueueCapacity int           `json:"queue_capacity"`
}

// SyncSeconds reports the cumulative durability barrier cost for the wire and
// for display.
func (s AppendStats) SyncSeconds() float64 { return s.SyncDuration.Seconds() }

// MarshalJSON adds the seconds form beside the counters.
func (s AppendStats) MarshalJSON() ([]byte, error) {
	type wire AppendStats
	return json.Marshal(struct {
		wire
		SyncSeconds float64 `json:"sync_seconds"`
	}{wire(s), s.SyncSeconds()})
}

type appendRequest struct {
	event   Event
	payload []byte
	result  chan appendResult
}

type appendResult struct {
	watermark Watermark
	err       error
}

func Open(path string, status *Status) (*Log, error) {
	return OpenWithOptions(path, status, Options{})
}

// Inspect verifies the existing committed WAL prefix without creating,
// truncating, syncing, or otherwise repairing the file.
func Inspect(path string) (Watermark, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return Watermark{}, false, err
	}
	defer file.Close()
	return scan(file, 0, 0, nil, nil)
}

// InspectReplay verifies and visits the committed WAL prefix without opening
// it for writes or repairing a partial tail.
func InspectReplay(path string, visit func(Record) error) (Watermark, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return Watermark{}, false, err
	}
	defer file.Close()
	return scan(file, 0, 0, visit, nil)
}

// ChainReport summarizes epoch-4 MAC/chain verification of the committed WAL
// prefix: three states, matching ADR 0016 — Authenticated frames (epoch 4,
// MAC and chain link verified), ChecksumOnly frames (epoch 1-3, or any frame
// read without a key: CRC32 checked, nothing cryptographic), and whether the
// scan reached the end cleanly.
type ChainReport struct {
	Authenticated uint64
	ChecksumOnly  uint64
	Head          Watermark
	ChainSequence uint64
	ChainOffset   int64
	ChainHash     [32]byte
	ChainVerified bool
}

// VerifyChain walks the entire committed WAL prefix and authenticates every
// epoch-4 frame against key. Unlike Inspect/InspectReplay (used on the hot
// startup path without redoing work OpenWithOptions already did), this is
// for offline tooling — the verify CLI/doctor path — that wants a dedicated,
// on-demand deep check.
func VerifyChain(path string, key []byte) (ChainReport, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return ChainReport{}, false, err
	}
	defer file.Close()
	verifier := &chainVerifier{key: key}
	var report ChainReport
	watermark, partial, err := scan(file, 0, 0, func(record Record) error {
		if record.Epoch == frameVersionLedgerIntegrity && verifier.verify() {
			report.Authenticated++
		} else {
			report.ChecksumOnly++
		}
		return nil
	}, verifier)
	if err != nil {
		return ChainReport{}, false, err
	}
	report.Head = watermark
	report.ChainSequence, report.ChainOffset, report.ChainHash, report.ChainVerified =
		verifier.sequence, verifier.offset, verifier.hash, verifier.sawFrames
	return report, partial, nil
}

func OpenWithOptions(path string, status *Status, options Options) (*Log, error) {
	if status == nil {
		status = NewStatus()
	}
	if options.QueueCapacity <= 0 {
		options.QueueCapacity = 1024
	}
	if options.MaxBatch <= 0 {
		options.MaxBatch = 64
	}
	if options.MaxBatch > options.QueueCapacity {
		options.MaxBatch = options.QueueCapacity
	}
	if options.FlushInterval < 0 {
		return nil, errors.New("ledger flush interval cannot be negative")
	}
	if len(options.ChainKey) != 0 && len(options.ChainKey) != 32 {
		return nil, errors.New("ledger chain key must be 32 bytes")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ledger directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		status.MarkUnavailable()
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	verifier := &chainVerifier{key: options.ChainKey}
	last, partial, err := scan(file, 0, 0, nil, verifier)
	if err != nil {
		file.Close()
		status.RequireRecovery()
		return nil, err
	}
	if partial {
		if err := file.Truncate(last.Offset); err != nil {
			file.Close()
			status.RequireRecovery()
			return nil, fmt.Errorf("truncate partial ledger tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			status.RequireRecovery()
			return nil, fmt.Errorf("sync repaired ledger tail: %w", err)
		}
	}
	if _, err := file.Seek(last.Offset, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("seek ledger end: %w", err)
	}
	log := &Log{
		file:           file,
		durability:     file,
		path:           path,
		sequence:       last.Sequence,
		offset:         last.Offset,
		status:         status,
		options:        options,
		appendQueue:    make(chan appendRequest, options.QueueCapacity),
		closeSignal:    make(chan struct{}),
		writerDone:     make(chan struct{}),
		chainKey:       append([]byte(nil), options.ChainKey...),
		chainHash:      verifier.hash,
		chainSequence:  verifier.sequence,
		chainOffset:    verifier.offset,
		chainSawFrames: verifier.sawFrames,
	}
	if options.WrapDurability != nil {
		log.durability = options.WrapDurability(file)
		if log.durability == nil {
			file.Close()
			return nil, errors.New("ledger durability wrapper returned nil")
		}
	}
	go log.writerLoop()
	return log, nil
}

func (l *Log) Append(ctx context.Context, event Event) (Watermark, error) {
	if err := event.Validate(); err != nil {
		return Watermark{}, err
	}
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Watermark{}, fmt.Errorf("encode ledger event: %w", err)
	}
	if len(payload) > maxPayloadSize {
		return Watermark{}, fmt.Errorf("ledger payload exceeds %d bytes", maxPayloadSize)
	}

	if l.status.Load() != AccountingHealthy {
		return Watermark{}, errors.New("accounting is not healthy")
	}

	request := appendRequest{
		event: event, payload: payload, result: make(chan appendResult, 1),
	}
	l.lifecycle.RLock()
	if l.closed {
		l.lifecycle.RUnlock()
		return Watermark{}, errors.New("ledger is closed")
	}
	select {
	case l.appendQueue <- request:
		l.lifecycle.RUnlock()
	case <-ctx.Done():
		l.lifecycle.RUnlock()
		return Watermark{}, ctx.Err()
	case <-l.closeSignal:
		l.lifecycle.RUnlock()
		return Watermark{}, errors.New("ledger is closed")
	}
	// Once accepted, wait for the durability result even if the caller is
	// canceled: returning early would make an eventually committed event look
	// uncommitted to its caller.
	result := <-request.result
	return result.watermark, result.err
}

func (l *Log) Replay(from Watermark, visit func(Record) error) (Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	if from.Generation != 0 && from.Generation != 1 {
		return Watermark{}, fmt.Errorf("unsupported ledger generation %d", from.Generation)
	}
	last, partial, err := scan(l.file, from.Offset, from.Sequence, visit, nil)
	if err != nil {
		// Only the log's own integrity condemns the log. visit is a caller
		// callback replaying into a derived read model, and its failures —
		// above all a canceled request context — say nothing about the WAL.
		// Letting those latch the accounting status would let a derivative
		// take down the authority it is derived from, which is exactly what
		// the ledger is not allowed to permit.
		if !isCallerVisitError(err) {
			l.status.RequireRecovery()
		}
		return Watermark{}, err
	}
	if partial {
		return Watermark{}, errors.New("ledger has a partial tail while open")
	}
	return last, nil
}

// Snapshot copies exactly the committed prefix while holding the writer lock.
// Appends may queue concurrently, but no frame after the returned watermark can
// enter the snapshot file.
func (l *Log) Snapshot(path string) (Watermark, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Watermark{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Watermark{}, fmt.Errorf("create ledger snapshot: %w", err)
	}
	complete := false
	defer func() {
		output.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := io.CopyN(output, io.NewSectionReader(l.file, 0, l.offset), l.offset); err != nil {
		return Watermark{}, fmt.Errorf("copy ledger snapshot: %w", err)
	}
	if err := output.Sync(); err != nil {
		return Watermark{}, fmt.Errorf("sync ledger snapshot: %w", err)
	}
	if err := output.Close(); err != nil {
		return Watermark{}, fmt.Errorf("close ledger snapshot: %w", err)
	}
	complete = true
	return Watermark{Generation: 1, Offset: l.offset, Sequence: l.sequence}, nil
}

func (l *Log) Close() error {
	l.lifecycle.Lock()
	if !l.closed {
		l.closed = true
		close(l.closeSignal)
	}
	l.lifecycle.Unlock()
	<-l.writerDone
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.chainKey)
	l.chainKey = nil
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Status exposes the accounting health this log already reports into. A caller
// whose own state machine has failed can put the shared signal into the same
// state instead of keeping a second, invisible one beside it.
func (l *Log) Status() *Status {
	return l.status
}

func (l *Log) Stats() AppendStats {
	return AppendStats{
		Batches: l.batches.Load(), Records: l.writtenRecords.Load(),
		Errors: l.appendErrors.Load(),
		Syncs:  l.syncs.Load(), SyncDuration: time.Duration(l.syncNanos.Load()),
		QueueDepth:    len(l.appendQueue),
		QueueCapacity: cap(l.appendQueue),
	}
}

func (l *Log) writerLoop() {
	defer close(l.writerDone)
	for {
		select {
		case first := <-l.appendQueue:
			batch := l.collectBatch(first)
			l.writeBatch(batch)
		case <-l.closeSignal:
			for {
				select {
				case request := <-l.appendQueue:
					l.writeBatch(l.collectBatch(request))
				default:
					return
				}
			}
		}
	}
}

func (l *Log) collectBatch(first appendRequest) []appendRequest {
	batch := make([]appendRequest, 1, l.options.MaxBatch)
	batch[0] = first
	if l.options.FlushInterval > 0 {
		timer := time.NewTimer(l.options.FlushInterval)
		defer timer.Stop()
		for len(batch) < l.options.MaxBatch {
			select {
			case request := <-l.appendQueue:
				batch = append(batch, request)
			case <-timer.C:
				return batch
			case <-l.closeSignal:
				return batch
			}
		}
		return batch
	}
	for len(batch) < l.options.MaxBatch {
		select {
		case request := <-l.appendQueue:
			batch = append(batch, request)
		default:
			return batch
		}
	}
	return batch
}

func (l *Log) writeBatch(batch []appendRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		respondBatch(batch, nil, errors.New("ledger is closed"))
		return
	}
	if l.status.Load() != AccountingHealthy {
		respondBatch(batch, nil, errors.New("accounting is not healthy"))
		return
	}
	if len(l.chainKey) != 32 {
		respondBatch(batch, nil, errors.New("ledger requires a chain key to append authenticated frames"))
		return
	}
	sequence := l.sequence
	offset := l.offset
	previousHash := l.chainHash
	watermarks := make([]Watermark, len(batch))
	var encoded bytes.Buffer
	for index, request := range batch {
		sequence++
		frame, nextHash := encodeChainFrame(l.chainKey, sequence, request.event.Kind, request.payload, previousHash)
		if _, err := encoded.Write(frame); err != nil {
			respondBatch(batch, nil, fmt.Errorf("encode ledger batch: %w", err))
			return
		}
		offset += int64(len(frame))
		previousHash = nextHash
		watermarks[index] = Watermark{Generation: 1, Offset: offset, Sequence: sequence}
	}
	if err := writeFull(l.durability, encoded.Bytes()); err != nil {
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("append ledger: %w", err))
		return
	}
	// Timed on both paths: a failing fsync is usually a slow one first, and a
	// sum that silently excluded failures would understate exactly the incident
	// an operator is trying to see.
	syncStarted := time.Now()
	syncErr := l.durability.Sync()
	l.syncNanos.Add(int64(time.Since(syncStarted)))
	l.syncs.Add(1)
	if syncErr != nil {
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("sync ledger: %w", syncErr))
		return
	}
	l.sequence = sequence
	l.offset = offset
	l.chainHash = previousHash
	l.chainSequence = sequence
	l.chainOffset = offset
	l.chainSawFrames = true
	l.batches.Add(1)
	l.writtenRecords.Add(uint64(len(batch)))
	respondBatch(batch, watermarks, nil)
}

func respondBatch(batch []appendRequest, watermarks []Watermark, err error) {
	for index, request := range batch {
		var watermark Watermark
		if index < len(watermarks) {
			watermark = watermarks[index]
		}
		request.result <- appendResult{watermark: watermark, err: err}
	}
}

func encodeFrame(sequence uint64, kind EventKind, payload []byte) []byte {
	return encodeFrameVersion(frameVersionLegacy, sequence, kind, payload)
}

// eventFrameVersion no longer branches by event shape (ADR 0016): every event
// this build writes is promoted to the frame-integrity epoch, so there is no
// window where some new frames are authenticated and others merely
// checksummed. The function stays, rather than being inlined at its one call
// site, because "what epoch does a fresh write use" is a fact worth a name.
func eventFrameVersion(Event) byte {
	return frameVersionLedgerIntegrity
}

func encodeFrameVersion(version byte, sequence uint64, kind EventKind, payload []byte) []byte {
	frame := make([]byte, frameHeaderSize+len(payload))
	copy(frame[:4], frameMagic)
	frame[4] = version
	frame[5] = byte(kind)
	binary.BigEndian.PutUint64(frame[8:16], sequence)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[frameHeaderSize:], payload)
	checksum := crc32.NewIEEE()
	checksum.Write(frame[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(frame[20:24], checksum.Sum32())
	return frame
}

// encodeChainFrame builds an epoch-4 frame: the original 24-byte header
// (magic/version/kind/sequence/payload-length/CRC32, CRC32 covering
// header[4:20]||payload exactly as every other epoch), followed by the
// previous frame's chain hash and this frame's MAC, followed by the payload.
// MAC = HMAC-SHA256(key, header[0:24] || previousHash || payload) — computed
// after the CRC field is filled, so the MAC also authenticates the CRC.
// nextHash = SHA256(MAC), becoming the next frame's previousHash: hashing the
// MAC alone (not the whole frame) is cheaper and does not require re-reading
// the payload to extend the chain.
func encodeChainFrame(key []byte, sequence uint64, kind EventKind, payload []byte, previousHash [32]byte) ([]byte, [32]byte) {
	frame := make([]byte, chainHeaderSize+len(payload))
	copy(frame[:4], frameMagic)
	frame[4] = frameVersionLedgerIntegrity
	frame[5] = byte(kind)
	binary.BigEndian.PutUint64(frame[8:16], sequence)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[frameHeaderSize+chainPreviousHashSize+chainMACSize:], payload)
	checksum := crc32.NewIEEE()
	checksum.Write(frame[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(frame[20:24], checksum.Sum32())
	copy(frame[frameHeaderSize:frameHeaderSize+chainPreviousHashSize], previousHash[:])
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[:frameHeaderSize])
	mac.Write(previousHash[:])
	mac.Write(payload)
	sum := mac.Sum(nil)
	copy(frame[frameHeaderSize+chainPreviousHashSize:frameHeaderSize+chainPreviousHashSize+chainMACSize], sum)
	return frame, sha256.Sum256(sum)
}

func scan(file io.ReadSeeker, fromOffset int64, initialSequence uint64, visit func(Record) error, verifier *chainVerifier) (Watermark, bool, error) {
	if fromOffset < 0 {
		return Watermark{}, false, errors.New("ledger offset cannot be negative")
	}
	if _, err := file.Seek(fromOffset, io.SeekStart); err != nil {
		return Watermark{}, false, err
	}
	offset := fromOffset
	lastSequence := initialSequence
	// Once a frame has been written at the authenticated epoch, no later frame
	// may fall below it. Epoch 4 is the first that carries a MAC, so
	// re-encoding one as epoch 3 — drop the chain tail, flip the version byte,
	// recompute the keyless CRC32 — would otherwise strip authentication off
	// history without needing the chain key at all, and appending a hand-built
	// legacy frame after the chain would forge an event the verifier walks
	// straight past. The check deliberately does not depend on the verifier:
	// it must hold for keyless readers too.
	//
	// It says nothing about epochs 1-3 relative to each other. Before ADR 0016
	// the epoch was chosen per event by its shape rather than by the build, so
	// a real pre-upgrade log interleaves them — an accounting event carrying a
	// lease mode wrote epoch 2 while the plain event beside it wrote epoch 1.
	// Requiring a non-decreasing epoch across the whole file would reject
	// exactly the histories that predate the guarantee.
	var seenAuthenticatedEpoch bool
	for {
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(file, header)
		if errors.Is(err, io.EOF) && n == 0 {
			return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, false, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, true, nil
		}
		if err != nil {
			return Watermark{}, false, err
		}
		if string(header[:4]) != frameMagic {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: invalid frame header", ErrCorrupt, offset)
		}
		epoch := header[4]
		if epoch != frameVersionLegacy && epoch != frameVersionCurrent && epoch != frameVersionPeriod && epoch != frameVersionLedgerIntegrity {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: frame epoch %d", ErrUnsupportedVersion, offset, epoch)
		}
		if seenAuthenticatedEpoch && epoch != frameVersionLedgerIntegrity {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: frame epoch %d follows an authenticated frame", ErrTampered, offset, epoch)
		}
		if epoch == frameVersionLedgerIntegrity {
			seenAuthenticatedEpoch = true
		}
		kind := EventKind(header[5])
		if !kind.Valid() || epoch == frameVersionLegacy && kind > EventRequestFinalized {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: event kind %d is not supported by epoch %d", ErrUnsupportedVersion, offset, kind, epoch)
		}
		sequence := binary.BigEndian.Uint64(header[8:16])
		if sequence <= lastSequence {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: non-monotonic sequence", ErrCorrupt, offset)
		}
		payloadLength := binary.BigEndian.Uint32(header[16:20])
		if payloadLength > maxPayloadSize {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, offset)
		}
		// Epoch 4 carries a previous-hash and a MAC between the base header
		// and the payload. They are read unconditionally (so offsets are
		// always correct), but only checked when a key is available —
		// callers without one get the same checksum-only treatment epochs
		// 1-3 always get.
		var previousHash, frameMAC [chainMACSize]byte
		if epoch == frameVersionLedgerIntegrity {
			chainTail := make([]byte, chainPreviousHashSize+chainMACSize)
			if _, err := io.ReadFull(file, chainTail); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, true, nil
				}
				return Watermark{}, false, err
			}
			copy(previousHash[:], chainTail[:chainPreviousHashSize])
			copy(frameMAC[:], chainTail[chainPreviousHashSize:])
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, true, nil
			}
			return Watermark{}, false, err
		}
		checksum := crc32.NewIEEE()
		checksum.Write(header[4:20])
		checksum.Write(payload)
		if checksum.Sum32() != binary.BigEndian.Uint32(header[20:24]) {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: checksum mismatch", ErrCorrupt, offset)
		}
		if epoch == frameVersionLedgerIntegrity && verifier.verify() {
			if previousHash != verifier.hash {
				return Watermark{}, false, fmt.Errorf("%w at offset %d: chain link does not match the running hash", ErrTampered, offset)
			}
			mac := hmac.New(sha256.New, verifier.key)
			mac.Write(header)
			mac.Write(previousHash[:])
			mac.Write(payload)
			expected := mac.Sum(nil)
			if !hmac.Equal(expected, frameMAC[:]) {
				return Watermark{}, false, fmt.Errorf("%w at offset %d: MAC does not authenticate", ErrTampered, offset)
			}
			verifier.hash = sha256.Sum256(expected)
			verifier.sequence = sequence
			verifier.sawFrames = true
		}
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, offset, err)
		}
		if event.Kind != kind {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: event kind mismatch", ErrCorrupt, offset)
		}
		// Each epoch asserts only what its own writer promised. A v2 frame
		// promised a lease mode on accounting events; v3 and v4 promise the
		// period's identity, and may carry a reservation written through the
		// older path that never had a lease mode to record.
		if epoch == frameVersionCurrent {
			if kind == EventReservationCreated && event.LeaseMode == "" || kind == EventAttemptSettled && event.LeaseMode == "" {
				return Watermark{}, false, fmt.Errorf("%w at offset %d: v2 accounting event is missing its payload epoch fields", ErrCorrupt, offset)
			}
		}
		if (epoch == frameVersionPeriod || epoch == frameVersionLedgerIntegrity) && event.PeriodTimezone == "" {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: v%d event is missing its period identity", ErrCorrupt, offset, epoch)
		}
		nextOffset := offset + int64(headerSizeForEpoch(epoch)) + int64(payloadLength)
		if epoch == frameVersionLedgerIntegrity && verifier.verify() {
			verifier.offset = nextOffset
		}
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Offset: nextOffset, Epoch: epoch, Event: event}); err != nil {
				return Watermark{}, false, visitError{err}
			}
		}
		offset = nextOffset
		lastSequence = sequence
	}
}

func headerSizeForEpoch(epoch byte) int {
	if epoch == frameVersionLedgerIntegrity {
		return chainHeaderSize
	}
	return frameHeaderSize
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
