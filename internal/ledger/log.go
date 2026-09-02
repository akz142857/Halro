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
	"hash"
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
	// batchNanos is the writer's own wall clock: from the moment an append is
	// taken off the queue to the moment its batch is durable, which is the
	// interval one batch actually occupies. It is longer than the barrier —
	// collectBatch lingers for FlushInterval, and the frames still have to be
	// encoded and written — and that difference is the whole reason it is
	// measured rather than derived. Deriving a throughput ceiling from the
	// barrier alone reported this filesystem 55% faster than it is at low
	// concurrency, because the linger is idle time the ceiling still has to
	// spend.
	batchNanos atomic.Int64

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

	// directory, generation, segments and the two anchors describe sealing.
	// generation is what the active file is writing; segments is the sealed
	// history in ascending order; chainAnchor and startSequence are where this
	// generation began, which a roll needs in order to record what it sealed
	// and which the next generation inherits.
	directory     string
	generation    uint64
	segments      []Segment
	chainAnchor   [32]byte
	startSequence uint64
	// plainDigest is the SHA-256 of the active generation's committed prefix,
	// carried forward as frames are appended rather than recomputed when a roll
	// needs it.
	//
	// A roll has to record the checksum of what it seals, and it has to do that
	// while holding the writer — the committed prefix is only well defined
	// under the lock. Reading the whole file there meant every append in the
	// process waited out a full pass over up to ledger.seal.max_active_bytes,
	// and since a budget reservation must be durable before any upstream call,
	// that wait is the gateway's. Hashing each batch as it commits costs the
	// same bytes spread across the writes that produced them, and leaves the
	// roll with nothing to read.
	plainDigest hash.Hash
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
func (l *Log) ChainHead() (head Watermark, hash [32]byte, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Watermark{Generation: l.generation, Offset: l.chainOffset, Sequence: l.chainSequence},
		l.chainHash, l.chainSawFrames
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
	SyncDuration time.Duration `json:"-"`
	// BatchDuration over Records is the sustained record rate this writer
	// achieved, and unlike Records/Batches over SyncDuration it needs no
	// assumption about what else a batch costs.
	BatchDuration time.Duration `json:"-"`
	// MaxBatch is the coalescing ceiling this log was opened with. Reported
	// alongside the observed mean because the two together answer a question
	// neither answers alone: a mean of 1.0 against a ceiling of 128 says the
	// barrier cost is being paid per record and does not have to be.
	MaxBatch      int `json:"max_batch"`
	QueueDepth    int `json:"queue_depth"`
	QueueCapacity int `json:"queue_capacity"`
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
	return InspectReplay(path, nil)
}

// InspectReplay verifies and visits the committed WAL prefix without opening
// it for writes or repairing a partial tail.
//
// "The WAL" is every generation, not the file currently being appended to. The
// offline commands built on this — doctor's state rebuild, `usage export`'s
// aggregate — reconstruct accounting from what they read, so reading only the
// active generation would not report an error; it would report a smaller
// ledger, which is the shape of failure that gets believed.
func InspectReplay(path string, visit func(Record) error) (Watermark, bool, error) {
	directory := filepath.Dir(path)
	segments, _, err := resolveSegments(directory)
	if err != nil {
		return Watermark{}, false, err
	}
	if err := checkSegmentsPresent(directory, segments); err != nil {
		return Watermark{}, false, err
	}
	var sequence uint64
	for _, segment := range segments {
		reader, err := openSegment(directory, segment)
		if err != nil {
			return Watermark{}, false, err
		}
		head, partial, err := scan(reader, segment.Generation, 0, sequence, visit, nil)
		closeErr := reader.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return Watermark{}, false, err
		}
		// A sealed generation is immutable, so a short read is damage rather
		// than a torn tail an open would repair.
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return Watermark{}, false, fmt.Errorf("%w: sealed generation %d ends at %d/%d, manifest says %d/%d",
				ErrCorrupt, segment.Generation, head.Offset, head.Sequence,
				segment.Length, segment.LastSequence)
		}
		sequence = head.Sequence
	}
	file, err := os.Open(path)
	if err != nil {
		return Watermark{}, false, err
	}
	defer file.Close()
	return scan(file, activeGeneration(segments), 0, sequence, visit, nil)
}

// ChainReport summarizes epoch-4 MAC/chain verification of the committed WAL
// prefix: three states, matching ADR 0016 — Authenticated frames (epoch 4,
// MAC and chain link verified), ChecksumOnly frames (epoch 1-3, or any frame
// read without a key: CRC32 checked, nothing cryptographic), and whether the
// scan reached the end cleanly.
type ChainReport struct {
	Authenticated uint64
	ChecksumOnly  uint64
	// SealedGenerations and SealedAuthenticated describe the history that no
	// longer lives in the active file. Reporting only the active file would let
	// this command answer a smaller question after every roll while still
	// printing a pass.
	SealedGenerations   uint64 `json:",omitempty"`
	SealedAuthenticated uint64 `json:",omitempty"`
	Head                Watermark
	ChainSequence       uint64
	ChainOffset         int64
	ChainHash           [32]byte
	ChainVerified       bool
}

// VerifyChain walks the active generation and authenticates every epoch-4 frame
// in it against key. Unlike Inspect/InspectReplay (used on the hot startup path
// without redoing work OpenWithOptions already did), this is for offline
// tooling — the verify CLI/doctor path — that wants a dedicated, on-demand deep
// check.
//
// The active generation, not the whole history: once a log has sealed anything,
// the frames before the last roll live in files this does not open, and it
// seeds itself from the manifest's record of where they left the chain.
// VerifySegments is the other half, and a caller that means "verify the ledger"
// needs both — which is why the report has somewhere to put the sealed counts.
func VerifyChain(path string, key []byte) (ChainReport, bool, error) {
	segments, _, err := resolveSegments(filepath.Dir(path))
	if err != nil {
		return ChainReport{}, false, err
	}
	anchor, sealedSequence, err := sealedTail(segments)
	if err != nil {
		return ChainReport{}, false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ChainReport{}, false, err
	}
	defer file.Close()
	// The active file's first frame links to the last sealed generation's chain
	// head, not to the zero hash. Verifying it against zero would report
	// tampering on every sealed instance, which is the failure mode a chain
	// check must not have.
	verifier := &chainVerifier{
		key: key, hash: anchor, sequence: sealedSequence, sawFrames: len(segments) > 0,
	}
	var report ChainReport
	watermark, partial, err := scan(file, activeGeneration(segments), 0, sealedSequence, func(record Record) error {
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
	directory := filepath.Dir(path)
	// Before the file is read: finish or abandon a roll that a crash caught
	// mid-flight, so the chain the scan below verifies is the one the manifest
	// claims. Doing it after would verify the active file against the wrong
	// anchor and report tampering.
	segments, err := repairSegments(directory)
	if err != nil {
		status.RequireRecovery()
		return nil, err
	}
	// Fail-closed on a history that is no longer all there. Starting with a
	// shorter archive than the manifest claims would rebuild balances from part
	// of the ledger and report nothing wrong.
	if err := checkSegmentsPresent(directory, segments); err != nil {
		status.RequireRecovery()
		return nil, err
	}
	anchor, sealedSequence, err := sealedTail(segments)
	if err != nil {
		status.RequireRecovery()
		return nil, err
	}
	generation := activeGeneration(segments)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		status.MarkUnavailable()
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	// The active file's first frame links to the chain head of the last sealed
	// generation, so the verifier is seeded there rather than at zero. A
	// directory that has never been sealed seeds at zero, which is what every
	// log did before sealing existed.
	verifier := &chainVerifier{key: options.ChainKey, hash: anchor, sequence: sealedSequence}
	last, partial, err := scan(file, generation, 0, sealedSequence, nil, verifier)
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
	// Seed the rolling checksum from what is already on disk. This is the one
	// full pass over the active generation, and it happens on a path that has
	// just read every one of those bytes anyway; every roll afterwards reads
	// none of them.
	plainDigest, err := digestFilePrefix(path, last.Offset)
	if err != nil {
		file.Close()
		status.RequireRecovery()
		return nil, err
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
		chainSawFrames: verifier.sawFrames || len(segments) > 0,
		directory:      directory,
		generation:     generation,
		segments:       segments,
		chainAnchor:    anchor,
		startSequence:  sealedSequence,
		plainDigest:    plainDigest,
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
	if from.Generation > l.generation {
		return Watermark{}, fmt.Errorf("unsupported ledger generation %d", from.Generation)
	}
	// A replay is one walk over several files. Sealed generations are read in
	// order and then the active one, and because a roll never moves a live byte
	// each generation's offsets still mean what they meant when the watermark
	// that names them was written.
	sequence := from.Sequence
	started := from.Generation == 0
	fail := func(err error) (Watermark, error) {
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
	for _, segment := range l.segments {
		if !started {
			if segment.Generation != from.Generation {
				continue
			}
			started = true
		}
		offset := int64(0)
		if segment.Generation == from.Generation {
			offset = from.Offset
		}
		if offset >= segment.Length {
			sequence = max(sequence, segment.LastSequence)
			continue
		}
		reader, err := openSegment(l.directory, segment)
		if err != nil {
			return fail(err)
		}
		// Authenticate what is being replayed, not just its CRCs. Open checks
		// sealed generations for presence and size only — deliberately, since
		// re-reading them every start is the cost sealing exists to remove —
		// so this is the moment those bytes are examined, and it is also the
		// only moment they can affect a balance. A frame edited in a sealed
		// file, with its CRC recomputed, would otherwise replay into
		// accounting with nothing to object.
		//
		// It costs the MAC over bytes already being read. The verifier is
		// seeded from the manifest's recorded chain head for the generation,
		// and the scan's own end state is checked against it below, so a
		// manifest edited to match a tampered file fails on the frames.
		verifier := l.sealedVerifier(segment, offset)
		head, partial, err := scan(reader, segment.Generation, offset, sequence, visit, verifier)
		closeErr := reader.Close()
		if err != nil {
			return fail(err)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		// A sealed generation is immutable, so a short read is damage rather
		// than the torn tail of an in-flight append.
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return fail(fmt.Errorf("%w: sealed generation %d ends at %d/%d, manifest says %d/%d",
				ErrCorrupt, segment.Generation, head.Offset, head.Sequence,
				segment.Length, segment.LastSequence))
		}
		if verifier.verify() {
			end, err := decodeChainHash(segment.EndHash)
			if err != nil {
				return fail(err)
			}
			if verifier.hash != end {
				return fail(fmt.Errorf("%w: sealed generation %d does not end at its recorded chain head",
					ErrTampered, segment.Generation))
			}
		}
		sequence = head.Sequence
	}
	if !started && from.Generation != l.generation {
		return Watermark{}, fmt.Errorf("unknown ledger generation %d", from.Generation)
	}
	offset := int64(0)
	if from.Generation == l.generation {
		offset = from.Offset
	}
	head, partial, err := scan(l.file, l.generation, offset, sequence, visit, nil)
	if err != nil {
		return fail(err)
	}
	if partial {
		return Watermark{}, errors.New("ledger has a partial tail while open")
	}
	return head, nil
}

// sealedVerifier is the chain verifier for one sealed generation, or nil when
// the frames cannot be authenticated from where the scan starts.
//
// Two cases return nil. Without a chain key the log is a read-only reader —
// offline tooling — and has nothing to verify with. Resuming inside a
// generation means the chain state at that offset is not recoverable from the
// manifest, which records the generation's ends and not its middle; the frames
// before the resume point were authenticated when whatever wrote the watermark
// replayed them, and every later generation is entered at zero and verified in
// full.
func (l *Log) sealedVerifier(segment Segment, offset int64) *chainVerifier {
	if len(l.chainKey) != 32 || offset != 0 {
		return nil
	}
	start, err := decodeChainHash(segment.StartHash)
	if err != nil {
		return nil
	}
	return &chainVerifier{key: l.chainKey, hash: start, sequence: segment.FirstSequence - 1}
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
	return Watermark{Generation: l.generation, Offset: l.offset, Sequence: l.sequence}, nil
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
		BatchDuration: time.Duration(l.batchNanos.Load()),
		MaxBatch:      l.options.MaxBatch,
		QueueDepth:    len(l.appendQueue),
		QueueCapacity: cap(l.appendQueue),
	}
}

func (l *Log) writerLoop() {
	defer close(l.writerDone)
	for {
		select {
		case first := <-l.appendQueue:
			// Timed from here, not from inside writeBatch: collectBatch's linger
			// is part of what a batch costs, and leaving it out is what made the
			// derived ceiling optimistic.
			started := time.Now()
			batch := l.collectBatch(first)
			l.writeBatch(batch)
			l.batchNanos.Add(int64(time.Since(started)))
		case <-l.closeSignal:
			for {
				select {
				case request := <-l.appendQueue:
					started := time.Now()
					l.writeBatch(l.collectBatch(request))
					l.batchNanos.Add(int64(time.Since(started)))
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
		watermarks[index] = Watermark{Generation: l.generation, Offset: offset, Sequence: sequence}
	}
	// Position the write at the committed tail the log tracks, never at
	// wherever the shared descriptor's cursor happens to sit. Replay scans
	// this same descriptor, and a scan that stopped early — a canceled visit,
	// a corrupt frame — leaves the cursor mid-file; a write that trusted it
	// would land inside already-durable frames and silently overwrite them.
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("seek ledger tail: %w", err))
		return
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
	// Past the barrier: these bytes are committed, so they join the generation's
	// running checksum here and nowhere else. Hashing before the sync would
	// fold in a write that may not have survived.
	l.plainDigest.Write(encoded.Bytes())
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

func scan(file io.ReadSeeker, generation uint64, fromOffset int64, initialSequence uint64, visit func(Record) error, verifier *chainVerifier) (Watermark, bool, error) {
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
			return Watermark{Generation: generation, Offset: offset, Sequence: lastSequence}, false, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Watermark{Generation: generation, Offset: offset, Sequence: lastSequence}, true, nil
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
					return Watermark{Generation: generation, Offset: offset, Sequence: lastSequence}, true, nil
				}
				return Watermark{}, false, err
			}
			copy(previousHash[:], chainTail[:chainPreviousHashSize])
			copy(frameMAC[:], chainTail[chainPreviousHashSize:])
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Watermark{Generation: generation, Offset: offset, Sequence: lastSequence}, true, nil
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
			if err := visit(Record{Generation: generation, Sequence: sequence, Offset: nextOffset, Epoch: epoch, Event: event}); err != nil {
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
