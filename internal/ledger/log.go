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

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
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
	// frameVersionRunAttribution introduces Work Unit and Run lifecycle events
	// and nullable attribution on the existing request/attempt events. It keeps
	// the epoch-4 authenticated frame shape; the new epoch is a reader gate for
	// event semantics, not a weakening or replacement of the chain.
	frameVersionRunAttribution = 5
	frameHeaderSize            = 24
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

// ReplicationFormatVersion is the Ledger reader/writer epoch advertised in an
// HA Hello. Keep it tied to the actual frame writer rather than duplicating a
// number in the app runtime.
func ReplicationFormatVersion() uint16 { return uint16(frameVersionRunAttribution) }

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
	epoch     byte
}

func authenticatedFrameEpoch(epoch byte) bool {
	return epoch == frameVersionLedgerIntegrity || epoch == frameVersionRunAttribution
}

func (v *chainVerifier) verify() bool { return v != nil && len(v.key) == 32 }

type Log struct {
	mu                  sync.Mutex
	anchors             verifiedAnchorCache
	lifecycle           sync.RWMutex
	admission           sync.Mutex
	mutating            int
	drain               chan struct{}
	maintenance         *MaintenanceLease
	file                *os.File
	durability          DurabilityWriter
	path                string
	sequence            uint64
	offset              int64
	status              *Status
	options             Options
	uncertainAdmissions []AppendAdmission
	appendQueue         chan appendRequest
	closeSignal         chan struct{}
	closeContext        context.Context
	closeCancel         context.CancelFunc
	writerDone          chan struct{}
	closed              bool
	closeErr            error
	closeJoinMu         sync.Mutex
	closeJoinErr        error // closeJoinMu; unknown original joins remain debt
	ioClosed            bool  // under mu; the actual IO retirement is attempted once
	batches             atomic.Uint64
	writtenRecords      atomic.Uint64
	appendErrors        atomic.Uint64
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
	chainEpoch     byte

	// directory, generation, segments and the two anchors describe sealing.
	// generation is what the active file is writing; segments is the sealed
	// history in ascending order; chainAnchor and startSequence are where this
	// generation began, which a roll needs in order to record what it sealed
	// and which the next generation inherits.
	directory        string
	directoryInfo    os.FileInfo
	generation       uint64
	segmentDirectory *segmentDirectoryIndex
	segmentSummary   SegmentSummary
	// compacting admits one compression workspace without queueing callers.
	// Guarded by mu and held through publication and physical cleanup.
	maintenancePhase func(string) error // private per-owner filesystem boundary seam; guarded by mu
	compacting       bool
	chainAnchor      [32]byte
	startSequence    uint64
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
	plainDigest         hash.Hash
	startupAuditPending bool // guarded by mu; saved-prefix Open excludes ordinary writes until audited
	startupBudget       *resourcebudget.Manager
	startupSaved        ReplayAnchor
	startupProvenance   *selectedStartupProvenance
	startupAudit        *StartupAudit
	startupAuditPtr     atomic.Pointer[StartupAudit]
	startupVerified     atomic.Bool
	startupAuditWG      sync.WaitGroup
	startupAuditDone    chan struct{} // mu; actual complete callback/FD exit
	startupReaderDebt   *startupAuditReaderDebt
	replayReaderDebt    *segmentReader
	replaySegmentOpen   func(string, Segment) (*segmentReader, error) // private actual-stream lifecycle test seam
	startupCleanup      *selectedStartupCleanup
	physical            atomic.Pointer[nativePhysicalOwner]
}

type Options struct {
	selectedStartup *selectedNativeStartup // established only by OpenWithSelectedSeal
	// ReserveAppendDisk consumes already owned native-growth capacity for the
	// complete encoded frame batch before source IO. It is independent of HA
	// confirmation admission, runs under the writer lock and must not reenter
	// this Log. Failure is a definite pre-write rejection and keeps the log
	// healthy. Success remains owned across partial IO, Roll and Close; this
	// hook does not charge compaction, manifest replacement or snapshots.
	ReserveAppendDisk func(int64) error
	// ReserveNativeFileDisk prepays one manifest/compression output envelope.
	// Published or uncertain inode fees outlive the transient token and Close.
	ReserveNativeFileDisk func(int64) (NativeFileDiskReservation, error)
	QueueCapacity         int
	MaxBatch              int
	FlushInterval         time.Duration
	// MaxSegmentDirectoryIndexBytes caps the rebuildable fixed-slot directory
	// locator created under scratch storage. Zero leaves the ceiling disabled.
	MaxSegmentDirectoryIndexBytes int64
	// WrapDurability is an internal test/integration seam for deterministic
	// write and fsync fault injection. Production callers leave it nil.
	WrapDurability func(*os.File) DurabilityWriter
	// ChainKey is the 32-byte Ledger frame HMAC key (ADR 0016). It is
	// required to Append, since every event this build writes is epoch 4.
	// Read-only callers (offline inspection, best-effort tooling) may open
	// without it: epoch-4 frames are still decoded, just not authenticated —
	// the same checksum-only treatment epochs 1-3 always get.
	ChainKey []byte
	// AfterDurable runs under the log writer lock after the source bytes fsync
	// and before append callers receive success. HA uses it to allocate the
	// global replication index and fsync the ordering record. The batch labels
	// whether its caller must later wait for confirmation; the callback itself
	// must not wait on the network while holding the writer lock. Nil preserves
	// the Standalone path exactly.
	AfterDurable func(DurableBatch) (uint64, error)
	// BeforeDurable reserves ownership for the complete encoded batch before
	// the first source write. The returned admission is transferred to
	// AfterDurable on success. A definite pre-write failure releases it; an
	// uncertain write/fsync failure keeps it until Close so startup recovery can
	// account for the exact durable suffix before readiness.
	BeforeDurable func(DurableBatch) (AppendAdmission, error)
	// WaitConfirmed is called by the accounting manager only after the durable
	// record has been applied to the local State, and only for the two event
	// kinds whose request path must wait for a Replica ACK. The uint64 is the
	// replication index returned by AfterDurable. Both callbacks must be set
	// together; nil preserves the Standalone path exactly.
	WaitConfirmed func(context.Context, uint64) error
	// Replica refuses semantic appends. Native frames may only enter through
	// AppendReplicated after their MAC and chain continuation are verified.
	Replica bool
	// RequireExisting is the HA Primary mode: ordinary appends and crash repair
	// remain enabled, but missing seeded directories/files are never created.
	RequireExisting bool
	// BeforeRoll proves the active generation's last frame is confirmed before
	// any structural mutation. AfterRoll records the now-durable Segment in the
	// global replication order. Both are nil in Standalone and Replica modes.
	BeforeRoll func(generation, lastSequence uint64) error
	AfterRoll  func(Segment) error
}

type DurableBatch struct {
	// Context belongs to an isolated terminal fact, never to unrelated work
	// combined in the same fsync batch. It grants no native role authority.
	Context              context.Context
	Generation           uint64
	FirstSequence        uint64
	LastSequence         uint64
	RequiresConfirmation bool
	Frames               []byte
	Admission            AppendAdmission
}

// AppendAdmission represents capacity reserved before a native durable append.
// The replication owner releases it only after the corresponding prefix is
// durably confirmed. A source that cannot establish the append outcome retains
// it until shutdown and lets startup recovery account for the exact suffix.
type AppendAdmission interface {
	Release() error
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
	ctx     context.Context
	event   Event
	payload []byte
	result  chan appendResult
	record  *Record
}

type appendResult struct {
	receipt AppendReceipt
	err     error
}

// AppendReceipt binds one locally durable Ledger record to the replication
// batch that contains it. Several records may share ReplicationIndex because
// the writer group-commits them. Callers must use WaitConfirmed rather than
// interpreting a zero index themselves; zero is the Standalone receipt.
type AppendReceipt struct {
	Watermark            Watermark
	ReplicationIndex     uint64
	RequiresConfirmation bool
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
	return InspectReplayContext(context.Background(), path, visit)
}

// InspectReplayContext retains the read-only checksum-only contract while
// checking cancellation during both manifest passes and every physical frame.
func InspectReplayContext(ctx context.Context, path string, visit func(Record) error) (head Watermark, partial bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Watermark{}, false, err
	}
	directory := filepath.Dir(path)
	manifest, err := openInspectionManifest(directory)
	if err != nil {
		return Watermark{}, false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, manifest.close(), ctx.Err())
		if resultErr != nil {
			head, partial = Watermark{}, false
		}
	}()
	first, firstDigest, err := manifest.walk(func(segment Segment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return checkSegmentPresent(directory, segment)
	})
	if err != nil {
		return Watermark{}, false, err
	}
	var sequence uint64
	consume := func(record Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if visit != nil {
			return visit(record)
		}
		return nil
	}
	second, secondDigest, err := manifest.walk(func(segment Segment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return err
		}
		head, partial, err := scan(reader, segment.Generation, 0, sequence, consume, nil)
		closeErr := reader.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
		// A sealed generation is immutable, so a short read is damage rather
		// than a torn tail an open would repair.
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return fmt.Errorf("%w: sealed generation %d ends at %d/%d, manifest says %d/%d",
				ErrCorrupt, segment.Generation, head.Offset, head.Sequence,
				segment.Length, segment.LastSequence)
		}
		sequence = head.Sequence
		return nil
	})
	if err != nil {
		return Watermark{}, false, err
	}
	if first != second || firstDigest != secondDigest {
		return Watermark{}, false, fmt.Errorf("%w: ledger segment manifest changed between replay passes", ErrCorrupt)
	}
	if err := manifest.check(); err != nil {
		return Watermark{}, false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Watermark{}, false, err
	}
	head, partial, scanErr := scan(file, second.active, 0, sequence, consume, nil)
	if err := errors.Join(scanErr, file.Close(), manifest.check()); err != nil {
		return Watermark{}, false, err
	}
	return head, partial, nil
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

// HoldsFrames reports whether the log contains anything a chain could have
// covered. A report with everything at zero describes a directory that was
// initialized and never written to, which is the one case where an
// unauthenticated chain is the honest answer rather than a missing one.
//
// It answers for the report it is given, not for the log: VerifyChain fills in
// the active generation only, and the sealed counts are added by the caller
// that also ran VerifySegments (internal/app.VerifyLedger, internal/app's
// doctor). Asking a bare VerifyChain report about a sealed-only log therefore
// answers false, so call it on a report that has both halves.
func (r ChainReport) HoldsFrames() bool {
	return r.Authenticated > 0 || r.ChecksumOnly > 0 ||
		r.SealedAuthenticated > 0 || r.SealedGenerations > 0 ||
		r.ChainSequence > 0 || r.Head.Sequence > 0 || r.Head.Offset > 0
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
// VerifySegmentsSummary is the other half, and a caller that means "verify the ledger"
// needs both — which is why the report has somewhere to put the sealed counts.
func VerifyChain(path string, key []byte) (ChainReport, bool, error) {
	manifest, err := walkReplicationManifest(filepath.Dir(path), nil)
	if err != nil {
		return ChainReport{}, false, err
	}
	anchor, err := decodeChainHash(manifest.segment.EndHash)
	if !manifest.sealed {
		anchor = [32]byte{}
	}
	if err != nil {
		return ChainReport{}, false, err
	}
	sealedSequence, sealedEpoch := manifest.segment.LastSequence, segmentEndEpoch(manifest.segment)
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
		key: key, hash: anchor, sequence: sealedSequence, sawFrames: manifest.sealed, epoch: sealedEpoch,
	}
	var report ChainReport
	watermark, partial, err := scan(file, manifest.active, 0, sealedSequence, func(record Record) error {
		if authenticatedFrameEpoch(record.Epoch) && verifier.verify() {
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
	if options.MaxSegmentDirectoryIndexBytes < 0 {
		return nil, errors.New("ledger segment directory index disk limit cannot be negative")
	}
	if (options.AfterDurable == nil) != (options.WaitConfirmed == nil) {
		return nil, errors.New("ledger replication durability and confirmation callbacks must be configured together")
	}
	if options.BeforeDurable != nil && options.AfterDurable == nil {
		return nil, errors.New("ledger append admission requires a durable replication owner")
	}
	if (options.BeforeRoll == nil) != (options.AfterRoll == nil) {
		return nil, errors.New("ledger replication roll callbacks must be configured together")
	}
	if options.Replica && (options.AfterDurable != nil || options.BeforeRoll != nil) {
		return nil, errors.New("replica ledger cannot install Primary replication callbacks")
	}
	directory := filepath.Dir(path)
	// Before the file is read: finish or abandon a roll that a crash caught
	// mid-flight, so the chain the scan below verifies is the one the manifest
	// claims. Doing it after would verify the active file against the wrong
	// anchor and report tampering.
	if !options.Replica {
		if options.RequireExisting {
			if info, statErr := os.Stat(directory); statErr != nil || !info.IsDir() {
				if statErr == nil {
					statErr = errors.New("ledger path is not a directory")
				}
				return nil, fmt.Errorf("open seeded ledger directory: %w", statErr)
			}
		} else if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create ledger directory: %w", err)
		}
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() {
		return nil, errors.Join(err, errors.New("Ledger native parent must be a real directory"))
	}
	segmentDirectory, err := openRuntimeSegmentDirectoryForStartupWithDisk(directory, options.Replica, options.MaxSegmentDirectoryIndexBytes, options.ChainKey, options.selectedStartup, options.ReserveNativeFileDisk)
	if err != nil {
		status.RequireRecovery()
		return nil, err
	}
	transferredDirectory := false
	defer func() {
		if !transferredDirectory {
			retireSelectedStartupDirectory(segmentDirectory, options.selectedStartup)
		}
	}()
	summary := segmentDirectory.summary
	var anchor [32]byte
	var sealedSequence uint64
	var sealedEpoch byte
	if segmentDirectory.parsed.count > 0 {
		last := segmentDirectory.parsed.last
		anchor, err = decodeChainHash(last.EndHash)
		if err != nil {
			status.RequireRecovery()
			return nil, err
		}
		sealedSequence, sealedEpoch = last.LastSequence, segmentEndEpoch(last)
	}
	generation := segmentDirectory.parsed.count + 1
	flags := os.O_RDWR
	if !options.Replica && !options.RequireExisting {
		flags |= os.O_CREATE
	}
	file, err := openNativeActiveWithDisk(path, flags, options.ReserveNativeFileDisk)
	if err != nil {
		status.MarkUnavailable()
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	// The active file's first frame links to the chain head of the last sealed
	// generation, so the verifier is seeded there rather than at zero. A
	// directory that has never been sealed seeds at zero, which is what every
	// log did before sealing existed.
	verifier := &chainVerifier{key: options.ChainKey, hash: anchor, sequence: sealedSequence, epoch: sealedEpoch}
	fromOffset, fromSequence := int64(0), sealedSequence
	if selected := options.selectedStartup; selected != nil {
		if err := selected.validateDirectory(segmentDirectory, generation, file); err != nil {
			retireSelectedStartupFile(file, options.selectedStartup)
			status.RequireRecovery()
			return nil, err
		}
		fromOffset, fromSequence = selected.anchor.Head.Offset, selected.anchor.Head.Sequence
		verifier.hash, verifier.sequence, verifier.epoch = selected.anchor.Hash, fromSequence, selected.anchor.Epoch
		verifier.offset, verifier.sawFrames = fromOffset, selected.anchor.Hash != ([32]byte{})
		if generation > selected.anchor.Head.Generation {
			if err := selected.verifyRolledSuffix(segmentDirectory, directory, generation, verifier); err != nil {
				retireSelectedStartupFile(file, selected)
				status.RequireRecovery()
				return nil, err
			}
			fromOffset, fromSequence = 0, verifier.sequence
			verifier.offset = 0
		}
	}
	var startupVisit func(Record) error
	if selected := options.selectedStartup; selected != nil {
		startupVisit = func(Record) error { selected.decoded++; return selected.ctx.Err() }
	}
	last, partial, err := scan(file, generation, fromOffset, fromSequence, startupVisit, verifier)
	if err != nil {
		retireSelectedStartupFile(file, options.selectedStartup)
		status.RequireRecovery()
		return nil, err
	}
	if selected := options.selectedStartup; selected != nil {
		if err := selected.validateActive(file); err != nil {
			retireSelectedStartupFile(file, selected)
			status.RequireRecovery()
			return nil, err
		}
	}
	if partial {
		if options.Replica {
			retireSelectedStartupFile(file, options.selectedStartup)
			status.RequireRecovery()
			return nil, fmt.Errorf("%w: replica Ledger has a partial tail that must be reconciled against ordering", ErrCorrupt)
		}
		if err := file.Truncate(last.Offset); err != nil {
			retireSelectedStartupFile(file, options.selectedStartup)
			status.RequireRecovery()
			return nil, fmt.Errorf("truncate partial ledger tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			retireSelectedStartupFile(file, options.selectedStartup)
			status.RequireRecovery()
			return nil, fmt.Errorf("sync repaired ledger tail: %w", err)
		}
	}
	if _, err := file.Seek(last.Offset, io.SeekStart); err != nil {
		retireSelectedStartupFile(file, options.selectedStartup)
		return nil, fmt.Errorf("seek ledger end: %w", err)
	}
	// Seed the rolling checksum from what is already on disk. This is the one
	// full pass over the active generation, and it happens on a path that has
	// just read every one of those bytes anyway; every roll afterwards reads
	// none of them.
	var plainDigest hash.Hash
	if options.selectedStartup == nil {
		plainDigest, err = digestFilePrefix(path, last.Offset)
	}
	if err != nil {
		retireSelectedStartupFile(file, options.selectedStartup)
		status.RequireRecovery()
		return nil, err
	}
	closeContext, closeCancel := context.WithCancel(context.Background())
	log := &Log{
		file:                file,
		durability:          file,
		path:                path,
		sequence:            last.Sequence,
		offset:              last.Offset,
		status:              status,
		options:             options,
		appendQueue:         make(chan appendRequest, options.QueueCapacity),
		closeSignal:         make(chan struct{}),
		closeContext:        closeContext,
		closeCancel:         closeCancel,
		writerDone:          make(chan struct{}),
		chainKey:            append([]byte(nil), options.ChainKey...),
		chainHash:           verifier.hash,
		chainSequence:       verifier.sequence,
		chainOffset:         verifier.offset,
		chainSawFrames:      verifier.sawFrames || segmentDirectory.parsed.count > 0,
		chainEpoch:          verifier.epoch,
		directory:           directory,
		directoryInfo:       directoryInfo,
		generation:          generation,
		segmentDirectory:    segmentDirectory,
		segmentSummary:      summary,
		chainAnchor:         anchor,
		startSequence:       sealedSequence,
		plainDigest:         plainDigest,
		startupAuditPending: options.selectedStartup != nil,
	}
	if options.WrapDurability != nil {
		log.durability = options.WrapDurability(file)
		if log.durability == nil {
			closeCancel()
			retireSelectedStartupFile(file, options.selectedStartup)
			return nil, errors.New("ledger durability wrapper returned nil")
		}
	}
	transferredDirectory = true
	go log.writerLoop()
	return log, nil
}

func (l *Log) Append(ctx context.Context, event Event) (Watermark, error) {
	receipt, err := l.AppendWithReceipt(ctx, event)
	return receipt.Watermark, err
}

// AppendWithReceipt returns after local durability and ordering persistence.
// It deliberately does not wait for a network ACK: the accounting manager
// must first apply the event to local State, then call WaitConfirmed.
func (l *Log) AppendWithReceipt(ctx context.Context, event Event) (AppendReceipt, error) {
	return l.appendWithReceipt(ctx, event, nil, nil, nil)
}

// AppendRecordWithReceipt returns the exact event snapshot, epoch, position and
// chain hash written by this append, only after native fsync and any ordering
// hook succeed. It permits a derived projection to Apply without rereading the
// history. The receipt is not a quorum confirmation or permission to publish a
// projection; callers still own ordering, maintenance and role authorization.
func (l *Log) AppendRecordWithReceipt(ctx context.Context, event Event) (Record, AppendReceipt, error) {
	var record Record
	receipt, err := l.appendWithReceipt(ctx, event, &record, nil, nil)
	if err != nil {
		return Record{}, receipt, err
	}
	return record, receipt, nil
}

// AppendApplyRecordWithReceipt keeps normal mutation admission registered
// through the durable record's synchronous Apply callback. Maintenance/Close
// join that callback before taking ownership or clearing native resources.
// Apply runs outside the native writer lock; it must not reenter this Log's
// append, maintenance or Close operations. The callback context is canceled by
// Log.Close, not by cancellation of an already accepted caller. Callback errors
// keep receipt facts but return no record and latch recovery-required.
// Confirmation remains an explicit operation after local Apply; this primitive
// is not a business drain or an HA role/publication authorization protocol.
func (l *Log) AppendApplyRecordWithReceipt(ctx context.Context, event Event, apply func(context.Context, Record) error) (Record, AppendReceipt, error) {
	if apply == nil {
		return Record{}, AppendReceipt{}, errors.New("Ledger apply callback is required")
	}
	var record Record
	receipt, err := l.appendWithReceipt(ctx, event, &record, nil, func(r Record) error {
		// Preserve resource/operation ownership, but an accepted fact outlives
		// its caller's cancellation. Only actual native Close cancels Apply.
		applyCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stop := context.AfterFunc(l.closeContext, cancel)
		defer cancel()
		defer stop()
		if l.closeContext.Err() != nil {
			cancel()
		}
		return apply(applyCtx, r)
	})
	if err != nil {
		return Record{}, receipt, err
	}
	return record, receipt, nil
}

func (l *Log) appendWithReceipt(ctx context.Context, event Event, record *Record, owner *MaintenanceLease, apply func(Record) error) (AppendReceipt, error) {
	if l.startupBudget != nil && !l.SelectedStartupReady() {
		return AppendReceipt{}, ErrStartupAuditPending
	}
	if l.options.Replica {
		return AppendReceipt{}, errors.New("ledger is replica-owned and refuses local append")
	}
	if err := event.validateForAppend(); err != nil {
		return AppendReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return AppendReceipt{}, err
	}
	finish, err := l.beginMutation(owner)
	if err != nil {
		return AppendReceipt{}, err
	}
	defer finish()
	payload, err := json.Marshal(event)
	if err != nil {
		return AppendReceipt{}, fmt.Errorf("encode ledger event: %w", err)
	}
	if len(payload) > maxPayloadSize {
		return AppendReceipt{}, fmt.Errorf("ledger payload exceeds %d bytes", maxPayloadSize)
	}
	if record != nil {
		// Event has pointer/slice fields. Decode the already-frozen bytes so the
		// returned evidence cannot alias caller-owned objects or diverge from WAL.
		var frozen Event
		if err := json.Unmarshal(payload, &frozen); err != nil {
			return AppendReceipt{}, fmt.Errorf("decode ledger append snapshot: %w", err)
		}
		event = frozen
	}

	if l.status.Load() != AccountingHealthy {
		return AppendReceipt{}, errors.New("accounting is not healthy")
	}

	request := appendRequest{
		ctx: context.WithoutCancel(ctx), event: event, payload: payload, result: make(chan appendResult, 1), record: record,
	}
	l.lifecycle.RLock()
	if l.closed || l.closeContext.Err() != nil {
		l.lifecycle.RUnlock()
		return AppendReceipt{}, errors.New("ledger is closed")
	}
	select {
	case l.appendQueue <- request:
		l.lifecycle.RUnlock()
	case <-ctx.Done():
		l.lifecycle.RUnlock()
		return AppendReceipt{}, ctx.Err()
	case <-l.closeContext.Done():
		l.lifecycle.RUnlock()
		return AppendReceipt{}, errors.New("ledger is closed")
	}
	// Once accepted, wait for the durability result even if the caller is
	// canceled: returning early would make an eventually committed event look
	// uncommitted to its caller.
	result := <-request.result
	if result.err == nil && apply != nil {
		if err := apply(*record); err != nil {
			l.status.RequireRecovery()
			return result.receipt, fmt.Errorf("apply durable Ledger record: %w", err)
		}
	}
	return result.receipt, result.err
}

// WaitConfirmed enforces the request-path ACK rule represented by a receipt.
// Standalone and ordinary events return immediately.
func (l *Log) WaitConfirmed(ctx context.Context, receipt AppendReceipt) error {
	if !receipt.RequiresConfirmation || l.options.WaitConfirmed == nil {
		return nil
	}
	if receipt.ReplicationIndex == 0 {
		return errors.New("ledger confirmation receipt has no replication index")
	}
	return l.options.WaitConfirmed(ctx, receipt.ReplicationIndex)
}

func (l *Log) Replay(from Watermark, visit func(Record) error) (Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closeContext.Err() != nil || l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	if l.replayReaderDebt != nil {
		return Watermark{}, errors.Join(ErrProjectionUnavailable, l.closeErr)
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
	for generation := uint64(1); generation < l.generation; generation++ {
		segment, found, err := l.lookupSegmentLocked(generation)
		if err != nil {
			return fail(err)
		}
		if !found {
			return fail(ErrSegmentMissing)
		}
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
		verifier, err := l.sealedVerifier(segment, offset)
		if err != nil {
			return fail(err)
		}
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
func (l *Log) sealedVerifier(segment Segment, offset int64) (*chainVerifier, error) {
	if len(l.chainKey) != 32 || offset != 0 {
		return nil, nil
	}
	start, err := decodeChainHash(segment.StartHash)
	if err != nil {
		return nil, err
	}
	epoch, err := l.segmentStartEpoch(segment.Generation)
	if err != nil {
		return nil, err
	}
	return &chainVerifier{key: l.chainKey, hash: start, sequence: segment.FirstSequence - 1, epoch: epoch}, nil
}
func (l *Log) segmentStartEpoch(generation uint64) (byte, error) {
	if generation <= 1 {
		return 0, nil
	}
	previous, found, err := l.lookupSegmentLocked(generation - 1)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrSegmentMissing
	}
	return segmentEndEpoch(previous), nil
}

// Snapshot copies exactly the committed prefix while holding the writer lock.
// Appends may queue concurrently, but no frame after the returned watermark can
// enter the snapshot file.
func (l *Log) Snapshot(path string) (Watermark, error) {
	return l.SnapshotWithDisk(path, nil)
}

// SnapshotWithDisk consumes an independent fixed staging-tree owner's quota.
// Successful output remains charged after native Close. Only a failed,
// exclusively created output with known Close/Retire/Sync can recycle quota.
func (l *Log) SnapshotWithDisk(path string, reserve func(int64) (NativeFileDiskReservation, error)) (result Watermark, resultErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Watermark{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closeContext.Err() != nil || l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	var reservation NativeFileDiskReservation
	var envelope int64
	var err error
	if reserve != nil {
		envelope, err = nativeFileEnvelope(l.offset)
		if err != nil {
			return Watermark{}, err
		}
		reservation, err = reserve(envelope)
		if err != nil {
			return Watermark{}, err
		}
	}
	output, owner, err := ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		return Watermark{}, keepNativeFileDiskDebt(reservation, fmt.Errorf("create ledger snapshot: %w", err))
	}
	closed := false
	defer func() {
		if !closed {
			closeErr := ownedfile.CloseFile(output)
			if closeErr != nil {
				resultErr = errors.Join(resultErr, ErrSegmentRetirement, closeErr)
			}
		}
		if resultErr != nil && !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
			retireErr := owner.Retire()
			if retireErr == nil {
				retireErr = durable.SyncDirectory(filepath.Dir(path))
			}
			if retireErr == nil && reservation != nil {
				retireErr = reservation.RetireKnown()
				if retireErr == nil {
					reservation = nil
				}
			}
			resultErr = errors.Join(resultErr, retireErr)
		}
		resultErr = keepNativeFileDiskDebt(reservation, resultErr)
		if resultErr != nil {
			result = Watermark{}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			l.closeErr = errors.Join(l.closeErr, resultErr)
		}
	}()
	var writer io.Writer = output
	if reservation != nil {
		writer = &nativeDiskWriter{output: output, limit: envelope}
	}
	if _, err := io.CopyN(writer, io.NewSectionReader(l.file, 0, l.offset), l.offset); err != nil {
		return Watermark{}, fmt.Errorf("copy ledger snapshot: %w", err)
	}
	if err := output.Sync(); err != nil {
		return Watermark{}, fmt.Errorf("sync ledger snapshot: %w", err)
	}
	if err := owner.VerifyFile(output); err != nil {
		return Watermark{}, err
	}
	if reservation != nil {
		if err := verifyNativeFileEnvelope(output, envelope); err != nil {
			return Watermark{}, err
		}
	}
	closeErr := ownedfile.CloseFile(output)
	closed = true
	if closeErr != nil {
		return Watermark{}, errors.Join(ErrSegmentRetirement, fmt.Errorf("close ledger snapshot: %w", closeErr))
	}
	if err := errors.Join(durable.SyncDirectory(filepath.Dir(path)), owner.VerifyName()); err != nil {
		return Watermark{}, errors.Join(ErrSegmentRetirement, err)
	}
	return Watermark{Generation: l.generation, Offset: l.offset, Sequence: l.sequence}, nil
}

func (l *Log) Close() error {
	return l.CloseContext(context.Background())
}

// CloseContext can borrow the original shutdown/checkpoint workspace for
// physical selection qualification. An expired original join retains retirement
// debt; only an actually completed join permits descriptor retirement.
func (l *Log) CloseContext(ctx context.Context) error {
	// Withdraw enqueue before waiting for its lifecycle readers: a full queue
	// can otherwise keep a reader blocked behind the very writer being joined.
	// The writer's closeSignal is separate and closes only after all possible
	// senders have left that gate, so its final drain cannot miss an enqueue.
	l.closeCancel()
	if err := lockNativeCloseMutex(ctx, &l.lifecycle); err != nil {
		return l.retainUnknownCloseJoin(err)
	}
	if !l.closed {
		l.closed = true
		close(l.closeSignal)
	}
	l.lifecycle.Unlock()
	l.admission.Lock()
	if l.maintenance != nil {
		l.maintenance.closed = true
		l.maintenance.cancel()
	}
	l.admission.Unlock()
	if err := waitNativeCloseJoin(ctx, l.writerDone); err != nil {
		return l.retainUnknownCloseJoin(err)
	}
	if err := l.joinCloseMutations(ctx); err != nil {
		return l.retainUnknownCloseJoin(err)
	}
	if err := l.lockNativeClose(ctx); err != nil {
		return l.retainUnknownCloseJoin(err)
	}
	if l.startupAudit != nil {
		l.startupAudit.Cancel()
	}
	auditJob := l.startupAudit
	auditDone := l.startupAuditDone
	l.mu.Unlock()
	if auditDone != nil {
		if err := waitNativeCloseJoin(ctx, auditDone); err != nil {
			return l.retainUnknownCloseJoin(err)
		}
	}
	if auditJob != nil {
		if err := auditJob.Wait(ctx); err != nil && ctx.Err() != nil && !auditJob.Snapshot().Complete {
			return l.retainUnknownCloseJoin(ctx.Err())
		}
	}
	l.closeJoinMu.Lock()
	joinErr := l.closeJoinErr
	l.closeJoinMu.Unlock()
	if err := l.lockNativeClose(ctx); err != nil {
		return l.retainUnknownCloseJoin(err)
	}
	defer l.mu.Unlock()
	if l.ioClosed {
		return l.closeErr
	}
	l.ioClosed = true
	var fileErr error
	if physical := l.physical.Load(); physical != nil && physical.failure == nil && physical.debt == 0 && physical.active == 0 {
		fileErr = l.checkPhysicalCloseSelectionLocked(ctx)
		if fileErr == nil {
			fileErr = l.preparePhysicalDerivedLocked(ctx, PhysicalDerivedClose, 0)
		}
		if fileErr == nil {
			fileErr = l.retirePhysicalRollTailLocked()
		} else {
			fileErr = errors.Join(fileErr, physical.projection.store.HoldPhysicalPreparationForRecovery(context.Background()))
		}
		if fileErr != nil {
			physical.failure = fileErr
			l.status.RequireRecovery()
		}
	}
	if l.file != nil {
		closeErr := ownedfile.CloseFile(l.file)
		if closeErr != nil {
			fileErr = errors.Join(fileErr, ErrSegmentRetirement, closeErr)
		} else {
			l.file = nil
		}
	}
	directoryErr := l.segmentDirectory.Close()
	if directoryErr == nil {
		l.segmentDirectory = nil
	}
	err := errors.Join(l.closeErr, joinErr, fileErr, directoryErr)
	if physical := l.physical.Load(); physical != nil && (physical.failure != nil || physical.debt != 0 || physical.active != 0) {
		err = errors.Join(err, authenticatedstate.ErrRecovery, physical.failure)
	}
	if l.startupReaderDebt != nil {
		err = errors.Join(err, l.startupReaderDebt.err)
	}
	if l.startupCleanup != nil {
		err = errors.Join(err, l.startupCleanup.close())
	}
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ErrSegmentRetirement) {
		l.status.RequireRecovery()
	} else {
		clear(l.chainKey)
		l.chainKey = nil
	}
	if err == nil {
		if physical := l.physical.Load(); physical != nil && physical.feeHeld {
			err = errors.Join(err, physical.fee.Release())
			if err == nil {
				physical.feeHeld = false
			}
		}
		for _, admission := range l.uncertainAdmissions {
			if admission != nil {
				err = errors.Join(err, admission.Release())
			}
		}
		if err == nil {
			l.uncertainAdmissions = nil
		}
	}
	l.closeErr = err
	return l.closeErr
}

// Status exposes the accounting health this log already reports into. A caller
// whose own state machine has failed can put the shared signal into the same
// state instead of keeping a second, invisible one beside it.
func (l *Log) Status() *Status {
	return l.status
}

// HasPersistentDirectoryIndex reports whether this Log owns the authenticated
// on-disk locator whose configured disk claim must outlive the Log.
func (l *Log) HasPersistentDirectoryIndex() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.segmentDirectory != nil && l.segmentDirectory.persistent
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
			l.writeCollectedBatch(batch)
			l.batchNanos.Add(int64(time.Since(started)))
		case <-l.closeSignal:
			for {
				select {
				case request := <-l.appendQueue:
					started := time.Now()
					l.writeCollectedBatch(l.collectBatch(request))
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

func (l *Log) writeCollectedBatch(batch []appendRequest) {
	if l.physical.Load() != nil {
		for i := range batch {
			l.writeBatch(batch[i : i+1])
		}
		return
	}
	start := 0
	for i, request := range batch {
		if (request.event.Kind == EventAttemptSettled || request.event.Kind == EventRequestFinalized) && resourcebudget.InCompletionContinuation(request.ctx) {
			if start < i {
				l.writeBatch(batch[start:i])
			}
			l.writeBatch(batch[i : i+1])
			start = i + 1
		}
	}
	if start < len(batch) {
		l.writeBatch(batch[start:])
	}
}

func (l *Log) writeBatch(batch []appendRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.selectedStartupAppendAllowedLocked() {
		respondBatch(batch, nil, ErrStartupAuditPending)
		return
	}
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
	requiresConfirmation := false
	var encoded bytes.Buffer
	for index, request := range batch {
		if request.event.Kind == EventReservationCreated || request.event.Kind == EventAttemptStarted {
			requiresConfirmation = true
		}
		sequence++
		frame, nextHash := encodeChainFrameVersion(frameVersionRunAttribution, l.chainKey, sequence, request.event.Kind, request.payload, previousHash)
		if _, err := encoded.Write(frame); err != nil {
			respondBatch(batch, nil, fmt.Errorf("encode ledger batch: %w", err))
			return
		}
		offset += int64(len(frame))
		previousHash = nextHash
		watermarks[index] = Watermark{Generation: l.generation, Offset: offset, Sequence: sequence}
		if request.record != nil {
			*request.record = Record{Generation: l.generation, Sequence: sequence, Offset: offset,
				Epoch: frameVersionRunAttribution, Event: request.event, Hash: nextHash}
		}
	}
	durableBatch := DurableBatch{
		Generation: l.generation, FirstSequence: watermarks[0].Sequence,
		LastSequence:         watermarks[len(watermarks)-1].Sequence,
		RequiresConfirmation: requiresConfirmation,
		Frames:               append([]byte(nil), encoded.Bytes()...),
	}
	if len(batch) == 1 {
		durableBatch.Context = batch[0].ctx
	}
	if l.options.BeforeDurable != nil {
		admission, err := l.options.BeforeDurable(durableBatch)
		if err != nil {
			l.appendErrors.Add(uint64(len(batch)))
			respondBatch(batch, nil, fmt.Errorf("admit ledger batch before durable append: %w", err))
			return
		}
		durableBatch.Admission = admission
	}
	releaseAdmission := func() {
		if durableBatch.Admission != nil {
			_ = durableBatch.Admission.Release()
			durableBatch.Admission = nil
		}
	}
	physicalAppend, physicalErr := l.validatePhysicalAppendLocked(batch, int64(encoded.Len()))
	if physicalErr != nil {
		releaseAdmission()
		respondBatch(batch, nil, physicalErr)
		return
	}
	reserveAppendDisk := l.options.ReserveAppendDisk
	if physicalAppend != nil {
		reserveAppendDisk = l.physical.Load().capacity.ConsumePhysicalRange
	}
	if reserveAppendDisk != nil {
		if err := reserveAppendDisk(int64(encoded.Len())); err != nil {
			releaseAdmission()
			l.appendErrors.Add(uint64(len(batch)))
			respondBatch(batch, nil, fmt.Errorf("admit Ledger native growth before source IO: %w", err))
			return
		}
	}
	// Position the write at the committed tail the log tracks, never at
	// wherever the shared descriptor's cursor happens to sit. Replay scans
	// this same descriptor, and a scan that stopped early — a canceled visit,
	// a corrupt frame — leaves the cursor mid-file; a write that trusted it
	// would land inside already-durable frames and silently overwrite them.
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		releaseAdmission()
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("seek ledger tail: %w", err))
		return
	}
	if physicalAppend != nil {
		physicalAppend.accepted.Store(true)
	}
	if err := writeFull(l.durability, encoded.Bytes()); err != nil {
		if durableBatch.Admission != nil {
			l.uncertainAdmissions = append(l.uncertainAdmissions, durableBatch.Admission)
			durableBatch.Admission = nil
		}
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
		if durableBatch.Admission != nil {
			l.uncertainAdmissions = append(l.uncertainAdmissions, durableBatch.Admission)
			durableBatch.Admission = nil
		}
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("sync ledger: %w", syncErr))
		return
	}
	// Past the barrier: these bytes are committed, so they join the generation's
	// running checksum here and nowhere else. Hashing before the sync would
	// fold in a write that may not have survived.
	if l.plainDigest != nil {
		l.plainDigest.Write(encoded.Bytes())
	}
	l.sequence = sequence
	l.offset = offset
	l.chainHash = previousHash
	l.chainSequence = sequence
	l.chainOffset = offset
	l.chainSawFrames = true
	l.chainEpoch = frameVersionRunAttribution
	l.batches.Add(1)
	l.writtenRecords.Add(uint64(len(batch)))
	replicationIndex := uint64(0)
	if l.options.AfterDurable != nil {
		var err error
		replicationIndex, err = l.options.AfterDurable(durableBatch)
		if err != nil {
			l.status.MarkUnavailable()
			l.appendErrors.Add(uint64(len(batch)))
			respondBatch(batch, nil, fmt.Errorf("record durable ledger batch for replication: %w", err))
			return
		}
		if replicationIndex == 0 {
			l.status.MarkUnavailable()
			l.appendErrors.Add(uint64(len(batch)))
			respondBatch(batch, nil, errors.New("record durable ledger batch for replication: callback returned index zero"))
			return
		}
	} else {
		releaseAdmission()
	}
	respondBatchWithReplication(batch, watermarks, replicationIndex, nil)
}

func respondBatch(batch []appendRequest, watermarks []Watermark, err error) {
	respondBatchWithReplication(batch, watermarks, 0, err)
}

func respondBatchWithReplication(batch []appendRequest, watermarks []Watermark, replicationIndex uint64, err error) {
	for index, request := range batch {
		var watermark Watermark
		if index < len(watermarks) {
			watermark = watermarks[index]
		}
		request.result <- appendResult{receipt: AppendReceipt{
			Watermark: watermark, ReplicationIndex: replicationIndex,
			RequiresConfirmation: request.event.Kind == EventReservationCreated || request.event.Kind == EventAttemptStarted,
		}, err: err}
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
	return frameVersionRunAttribution
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
	return encodeChainFrameVersion(frameVersionLedgerIntegrity, key, sequence, kind, payload, previousHash)
}

func encodeChainFrameVersion(version byte, key []byte, sequence uint64, kind EventKind, payload []byte, previousHash [32]byte) ([]byte, [32]byte) {
	frame := make([]byte, chainHeaderSize+len(payload))
	copy(frame[:4], frameMagic)
	frame[4] = version
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
	return scanFrom(file, generation, fromOffset, fromOffset, initialSequence, visit, verifier)
}

func scanFrom(file io.ReadSeeker, generation uint64, sourceOffset, logicalOffset int64, initialSequence uint64, visit func(Record) error, verifier *chainVerifier) (Watermark, bool, error) {
	return scanFromFrames(file, generation, sourceOffset, logicalOffset, initialSequence, visit, nil, verifier)
}

// nativeFrameParts borrows the exact bytes just authenticated by the scanner.
// A frame consumer must copy what it needs before returning. No re-encoding,
// second disk read, or additional whole-frame allocation is needed.
type nativeFrameParts struct {
	header, chainTail, payload []byte
}

func (f nativeFrameParts) size() int { return len(f.header) + len(f.chainTail) + len(f.payload) }

func scanFromFrames(file io.ReadSeeker, generation uint64, sourceOffset, logicalOffset int64, initialSequence uint64, visit func(Record) error, frames func(Record, nativeFrameParts) error, verifier *chainVerifier) (Watermark, bool, error) {
	if actual, ok := file.(*os.File); ok {
		info, err := actual.Stat()
		if err != nil {
			return Watermark{}, false, err
		}
		file = boundedfilecache.NewReader(actual, info.Size())
	}
	if sourceOffset < 0 || logicalOffset < 0 {
		return Watermark{}, false, errors.New("ledger offset cannot be negative")
	}
	if _, err := file.Seek(sourceOffset, io.SeekStart); err != nil {
		return Watermark{}, false, err
	}
	offset := logicalOffset
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
	highestAuthenticatedEpoch := byte(0)
	if verifier != nil {
		highestAuthenticatedEpoch = verifier.epoch
	}
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
		if epoch != frameVersionLegacy && epoch != frameVersionCurrent && epoch != frameVersionPeriod && !authenticatedFrameEpoch(epoch) {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: frame epoch %d", ErrUnsupportedVersion, offset, epoch)
		}
		if highestAuthenticatedEpoch > 0 && !authenticatedFrameEpoch(epoch) {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: frame epoch %d follows an authenticated frame", ErrTampered, offset, epoch)
		}
		if highestAuthenticatedEpoch == frameVersionRunAttribution && epoch == frameVersionLedgerIntegrity {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: frame epoch %d follows run-attribution epoch", ErrTampered, offset, epoch)
		}
		if authenticatedFrameEpoch(epoch) && epoch > highestAuthenticatedEpoch {
			highestAuthenticatedEpoch = epoch
			if verifier != nil {
				verifier.epoch = epoch
			}
		}
		kind := EventKind(header[5])
		if !kind.Valid() || epoch < frameVersionRunAttribution && kind > EventRequestFinalized {
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
		var chainTail []byte
		if authenticatedFrameEpoch(epoch) {
			chainTail = make([]byte, chainPreviousHashSize+chainMACSize)
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
		if authenticatedFrameEpoch(epoch) && verifier.verify() {
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
		if (epoch == frameVersionPeriod || authenticatedFrameEpoch(epoch)) && event.PeriodTimezone == "" {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: v%d event is missing its period identity", ErrCorrupt, offset, epoch)
		}
		nextOffset := offset + int64(headerSizeForEpoch(epoch)) + int64(payloadLength)
		if authenticatedFrameEpoch(epoch) && verifier.verify() {
			verifier.offset = nextOffset
		}
		if visit != nil || frames != nil {
			var recordHash [32]byte
			if authenticatedFrameEpoch(epoch) && verifier.verify() {
				recordHash = verifier.hash
			}
			record := Record{Generation: generation, Sequence: sequence, Offset: nextOffset, Epoch: epoch, Event: event, Hash: recordHash}
			if visit != nil {
				if err := visit(record); err != nil {
					return Watermark{}, false, visitError{err}
				}
			}
			if frames != nil {
				if err := frames(record, nativeFrameParts{header: header, chainTail: chainTail, payload: payload}); err != nil {
					return Watermark{}, false, visitError{err}
				}
			}
		}
		offset = nextOffset
		lastSequence = sequence
	}
}

func headerSizeForEpoch(epoch byte) int {
	if authenticatedFrameEpoch(epoch) {
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
