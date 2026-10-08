package ledger

// This is the transitional, node-derived Ledger projection. It is rebuilt from
// the complete authenticated WAL into a new exclusive file. There
// is deliberately no reopen/trust-existing-file API: D-03/I-11 must establish
// authenticated content and key-set proofs before production can use it as a
// restart checkpoint. Runtime owners can select only a fresh reconstruction.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
)

var (
	ErrProjectionUnavailable = errors.New("ledger projection is not available")
	ErrProjectionCorrupt     = errors.New("ledger projection has an invalid row or index")
	ErrProjectionPageLimit   = errors.New("ledger projection page limit is invalid or too small")
	ErrProjectionSource      = errors.New("ledger projection native source verification failed")
	ErrProjectionDiskLimit   = errors.New("ledger projection disk ceiling exceeded")
)

const projectionMaxValue = 3 * maxPayloadSize

// Public/live ApplyBatch remains a short 64-record atomic barrier. A fresh,
// unpublished cold-build candidate has no concurrent readers and uses a larger
// row cap while preserving the same 8 MiB encoded-byte limit.
const projectionBatchRows = 64
const projectionBatchBytes = 8 << 20
const projectionBuildBatchRows = 512

var projectionBuckets = []string{"meta", "events", "event_positions", "balances", "pending", "pending_by_time", "leases", "settled", "work_units", "runs", "requests", "idempotency", "runs_by_work_unit", "active_requests", "active_requests_by_work_unit", "attempts_by_request", "pending_by_work_unit", "settled_by_work_unit", "open_work_units_by_project", "active_runs_by_project", "cohort_units", "cohort_versions", "export_work_units", "export_runs"}

type projectionPosition struct {
	Watermark Watermark
	Hash      [32]byte
	Epoch     uint8
}

// ProjectedRequest retains acceptance and finalization separately. A finalized
// request can still own an unresolved attempt; its attribution must survive
// finalization for conservative recovery and a late settlement.
type ProjectedRequest struct {
	Accepted  Record
	Finalized *Record
	// Older un-attributed WALs may start with a lease, with no acceptance fact.
	// Retain the first REAL lease as a separate recovery origin. Never fabricate
	// Accepted. This entry survives settlement so interrupted recovery can still
	// enumerate the request even after its last pending attempt disappears.
	LegacyLease *Record
}

func (r ProjectedRequest) RecoveryOrigin() (Record, error) {
	if r.LegacyLease != nil {
		v := *r.LegacyLease
		if v.Sequence == 0 || v.Event.Kind != EventReservationCreated || v.Event.WorkUnitID != "" || v.Event.RunID != "" {
			return Record{}, ErrProjectionCorrupt
		}
		return v, nil
	}
	if r.Accepted.Sequence == 0 || r.Accepted.Event.Kind != EventRequestAccepted {
		return Record{}, ErrProjectionCorrupt
	}
	return r.Accepted, nil
}

func (r ProjectedRequest) Legacy() bool {
	return r.LegacyLease != nil || r.Accepted.Sequence == 0 || r.Finalized != nil && r.Finalized.Sequence < r.Accepted.Sequence
}

type projectionReservation struct {
	Reservation attemptReservation
	Evidence    Record
}

type LedgerProjection struct {
	mu           sync.RWMutex
	db           *bbolt.DB
	file         *ownedfile.Owner
	closeErr     error
	generation   [16]byte
	ready        bool
	maxDiskBytes int64
}

// BuildLedgerProjection authenticates the entire source and compares its final
// report with a trusted, role-permitted frozen head supplied by the caller. The
// source must be quiescent/exclusively owned during the build. Partial tails,
// changed heads, cancellation and any transaction failure discard the candidate.
// output must not exist; it is never used to open a prior projection.
func BuildLedgerProjection(ctx context.Context, source string, key []byte, output string, expected ChainReport) (*LedgerProjection, error) {
	return buildLedgerProjection(ctx, source, key, output, expected, nil, 0)
}

// BuildLedgerProjectionWithLimit authenticates and reduces the full frozen
// report while enforcing the candidate's bbolt data-file ceiling. The caller
// separately owns the complete workspace/mapping and persistent Disk costs.
func BuildLedgerProjectionWithLimit(ctx context.Context, source string, key []byte, output string, expected ChainReport, maxDiskBytes int64) (*LedgerProjection, error) {
	if maxDiskBytes <= 0 {
		return nil, ErrProjectionDiskLimit
	}
	return buildLedgerProjection(ctx, source, key, output, expected, nil, maxDiskBytes)
}

// ProjectionPrefix binds the exact physical end of a role-permitted prefix.
// Its position/hash must come from authenticated native/ordering evidence under
// exclusive maintenance ownership, never from an existing derived DB. Offset
// zero also names a verified roll boundary. The all-zero value names genesis.
type ProjectionPrefix struct {
	Head Watermark
	Hash [32]byte
}

// BuildLedgerProjectionPrefix authenticates the complete frozen source, while
// reducing only the supplied prefix. The durable suffix never contributes a
// balance, lease, idempotency key or active request. All bytes still must match
// expected; this is an O(history) transitional build, not trusted resume.
func BuildLedgerProjectionPrefix(ctx context.Context, source string, key []byte, output string, expected ChainReport, prefix ProjectionPrefix) (*LedgerProjection, error) {
	return buildLedgerProjection(ctx, source, key, output, expected, &prefix, 0)
}

// BuildLedgerProjectionPrefixWithLimit is the same fresh, authenticated build
// as BuildLedgerProjectionPrefix, with a bbolt data-file ceiling enforced by
// Options.MaxSize during allocation/growth. It does not reserve filesystem
// blocks or account for mmap/page-cache use.
func BuildLedgerProjectionPrefixWithLimit(ctx context.Context, source string, key []byte, output string, expected ChainReport, prefix ProjectionPrefix, maxDiskBytes int64) (*LedgerProjection, error) {
	if maxDiskBytes <= 0 {
		return nil, ErrProjectionDiskLimit
	}
	return buildLedgerProjection(ctx, source, key, output, expected, &prefix, maxDiskBytes)
}

func buildLedgerProjection(ctx context.Context, source string, key []byte, output string, expected ChainReport, prefix *ProjectionPrefix, maxDiskBytes int64) (result *LedgerProjection, resultErr error) {
	p, _, err := buildLedgerProjectionCut(ctx, source, key, output, expected, prefix, nil, maxDiskBytes)
	return p, err
}

type projectionCursorTarget struct {
	generation uint64
	sequence   uint64
}

// buildLedgerProjectionAtCursorWithLimit resolves an authorized logical cursor
// and builds its projection in the same authenticated source pass. expected
// binds the complete source head held by the frozen native owner; the cursor
// itself grants no role or ordering authority.
func buildLedgerProjectionAtCursorWithLimit(ctx context.Context, source string, key []byte, output string, expected ChainReport, generation, sequence uint64, maxDiskBytes int64) (*LedgerProjection, ProjectionPrefix, error) {
	if maxDiskBytes <= 0 {
		return nil, ProjectionPrefix{}, ErrProjectionDiskLimit
	}
	if generation == 0 && sequence != 0 {
		return nil, ProjectionPrefix{}, ErrProjectionCursor
	}
	if generation > expected.Head.Generation || sequence > expected.Head.Sequence {
		return nil, ProjectionPrefix{}, ErrProjectionCursor
	}
	target := &projectionCursorTarget{generation: generation, sequence: sequence}
	return buildLedgerProjectionCut(ctx, source, key, output, expected, nil, target, maxDiskBytes)
}

func buildLedgerProjectionCut(ctx context.Context, source string, key []byte, output string, expected ChainReport, prefix *ProjectionPrefix, target *projectionCursorTarget, maxDiskBytes int64) (result *LedgerProjection, resolved ProjectionPrefix, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	if len(key) != 32 {
		return nil, ProjectionPrefix{}, errors.New("ledger projection requires a 32-byte ledger key")
	}
	p, err := createLedgerProjectionWithLimit(output, maxDiskBytes)
	if err != nil {
		return nil, ProjectionPrefix{}, err
	}
	failed := true
	defer func() {
		if failed {
			resultErr = retainFreshProjectionCleanupDebt(p, resultErr, p.Discard())
		}
	}()
	if prefix != nil && (prefix.Head.Offset < 0 || prefix.Head.Generation == 0 && *prefix != (ProjectionPrefix{})) {
		return nil, ProjectionPrefix{}, ErrProjectionCorrupt
	}
	if prefix != nil && target != nil {
		return nil, ProjectionPrefix{}, ErrProjectionCorrupt
	}
	seenPrefix := prefix != nil && *prefix == (ProjectionPrefix{}) || target != nil && target.generation == 0 && target.sequence == 0
	if seenPrefix && prefix == nil {
		resolved = ProjectionPrefix{}
	}
	var prefixMismatch error
	checkPrefix := func(head Watermark, hash [32]byte) error {
		if prefix != nil && head == prefix.Head {
			if hash != prefix.Hash {
				prefixMismatch = fmt.Errorf("%w: permitted prefix hash differs from native evidence", ErrProjectionCorrupt)
			}
			seenPrefix = true
		}
		if target != nil && head.Generation == target.generation && head.Sequence == target.sequence {
			candidate := ProjectionPrefix{Head: head, Hash: hash}
			if seenPrefix && candidate != resolved {
				return fmt.Errorf("%w: ambiguous physical position", ErrProjectionCursor)
			}
			resolved, seenPrefix = candidate, true
		}
		return nil
	}
	var batch []Record
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := p.applyRecords(ctx, batch)
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	report, partial, err := inspectReplayAuthenticated(ctx, source, key, func(record Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkPrefix(Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}, record.Hash); err != nil {
			return err
		}
		if prefix != nil && (record.Generation > prefix.Head.Generation || record.Generation == prefix.Head.Generation && record.Offset > prefix.Head.Offset) {
			return nil
		}
		if target != nil {
			if !seenPrefix && (record.Generation > target.generation || record.Generation == target.generation && record.Sequence > target.sequence) {
				return nil
			}
			if seenPrefix && (record.Generation > resolved.Head.Generation || record.Generation == resolved.Head.Generation && record.Offset > resolved.Head.Offset) {
				return nil
			}
		}
		raw, err := json.Marshal(record.Event)
		if err != nil {
			return err
		}
		if len(raw) > maxPayloadSize {
			return ErrProjectionPageLimit
		}
		if len(batch) == projectionBuildBatchRows || batchBytes+len(raw) > projectionBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += len(raw)
		return nil
	}, checkPrefix)
	if err != nil {
		if !isCallerVisitError(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, ProjectionPrefix{}, errors.Join(ErrProjectionSource, err)
		}
		return nil, ProjectionPrefix{}, err
	}
	identityMatches := report == expected
	if target != nil {
		identityMatches = projectionReportMatchesHead(report, expected)
	}
	if partial || !identityMatches {
		return nil, ProjectionPrefix{}, errors.Join(ErrProjectionSource, fmt.Errorf("%w: source does not match the complete frozen report", ErrProjectionCorrupt))
	}
	// Authenticate the whole source before classifying a bad caller cut. A
	// second-pass source replacement must not hide behind a missing/wrong cut.
	if prefixMismatch != nil {
		return nil, ProjectionPrefix{}, prefixMismatch
	}
	if prefix != nil && !seenPrefix {
		return nil, ProjectionPrefix{}, fmt.Errorf("%w: permitted prefix is not a native record or roll boundary", ErrProjectionCorrupt)
	}
	if target != nil && !seenPrefix {
		return nil, ProjectionPrefix{}, ErrProjectionCursor
	}
	if err := flush(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	err = p.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		position, _, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil {
			return err
		}
		// A roll/empty active generation can advance generation and reset offset
		// without another record. Publish the verified physical head as well.
		position.Watermark, position.Hash = report.Head, report.ChainHash
		if prefix != nil {
			position.Watermark, position.Hash = prefix.Head, prefix.Hash
		} else if target != nil {
			position.Watermark, position.Hash = resolved.Head, resolved.Hash
		}
		if err := projectionPut(tx, "meta", []byte("position"), position); err != nil {
			return err
		}
		return p.checkDiskLimit(tx)
	})
	err = normalizeProjectionDiskError(err)
	if err != nil {
		return nil, ProjectionPrefix{}, err
	}
	p.ready = true
	failed = false
	if target != nil {
		return p, resolved, nil
	}
	return p, ProjectionPrefix{}, nil
}

func projectionReportMatchesHead(actual, expected ChainReport) bool {
	return actual.Head == expected.Head && actual.ChainSequence == expected.ChainSequence && actual.ChainOffset == expected.ChainOffset && actual.ChainHash == expected.ChainHash && actual.ChainVerified == expected.ChainVerified
}

func createLedgerProjection(path string) (*LedgerProjection, error) {
	return createLedgerProjectionWithLimit(path, 0)
}

func createLedgerProjectionWithLimit(path string, maxDiskBytes int64) (*LedgerProjection, error) {
	if maxDiskBytes < 0 {
		return nil, ErrProjectionDiskLimit
	}
	if uint64(maxDiskBytes) > uint64(^uint(0)>>1) {
		return nil, ErrProjectionDiskLimit
	}
	p := &LedgerProjection{maxDiskBytes: maxDiskBytes}
	db, receipt, err := ownedfile.OpenExclusiveBolt(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType, MaxSize: int(maxDiskBytes)})
	p.file = receipt
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			return nil, retainFreshProjectionCleanupDebt(p, normalizeProjectionDiskError(err), err)
		}
		return nil, retainFreshProjectionCleanupDebt(p, normalizeProjectionDiskError(err), p.file.Retire())
	}
	p.db = db
	if _, err := rand.Read(p.generation[:]); err != nil {
		return nil, retainFreshProjectionCleanupDebt(p, err, p.Discard())
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range append(projectionBuckets, projectionListBuckets...) {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		if err := projectionPut(tx, "meta", []byte("pending_count"), int64(0)); err != nil {
			return err
		}
		for _, name := range []string{"export_work_unit_count", "export_run_count"} {
			if err := projectionPut(tx, "meta", []byte(name), uint64(0)); err != nil {
				return err
			}
		}
		if err := projectionPut(tx, "meta", []byte("position"), projectionPosition{}); err != nil {
			return err
		}
		return p.checkDiskLimit(tx)
	}); err != nil {
		return nil, retainFreshProjectionCleanupDebt(p, normalizeProjectionDiskError(err), p.Discard())
	}
	return p, nil
}

func (p *LedgerProjection) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = false
	if p.db == nil {
		return p.closeErr
	}
	p.closeErr = p.db.Close()
	if p.closeErr != nil {
		p.closeErr = errors.Join(ownedfile.ErrRetirement, p.closeErr)
	}
	p.db = nil
	return p.closeErr
}

// Discard joins and closes this fresh derivative, then removes its captured
// physical inode. Replaced paths survive. Saved bytes never acquire authority.
func (p *LedgerProjection) Discard() error {
	if p == nil {
		return nil
	}
	err := p.Close()
	if err != nil {
		return err
	}
	return p.file.Retire()
}

// Apply is only for an already durable, authenticated next record. It keeps the
// existing synchronous apply barrier: rows and position commit in one local DB
// transaction, with no network waits, nested transactions or asynchronous queue.
func (p *LedgerProjection) Apply(ctx context.Context, record Record) error {
	return p.ApplyBatch(ctx, []Record{record})
}

// ApplyBatch commits a bounded set atomically. A later invalid record rolls
// back earlier row and position changes in the same batch.
func (p *LedgerProjection) ApplyBatch(ctx context.Context, records []Record) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return ErrProjectionUnavailable
	}
	if len(records) == 0 || len(records) > projectionBatchRows {
		return ErrProjectionPageLimit
	}
	size := 0
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := json.Marshal(record.Event)
		if err != nil {
			return err
		}
		if len(raw) > maxPayloadSize || len(raw) > projectionBatchBytes-size {
			return ErrProjectionPageLimit
		}
		size += len(raw)
	}
	return p.applyRecords(ctx, records)
}

func (p *LedgerProjection) applyRecords(ctx context.Context, records []Record) error {
	err := p.db.Update(func(tx *bbolt.Tx) error {
		rows := boltProjectionRows{tx}
		for _, record := range records {
			if err := applyProjectionRecord(ctx, rows, record); err != nil {
				return err
			}
		}
		return p.checkDiskLimit(tx)
	})
	return normalizeProjectionDiskError(err)
}

func normalizeProjectionDiskError(err error) error {
	if errors.Is(err, bberrors.ErrMaxSizeReached) {
		return errors.Join(ErrProjectionDiskLimit, err)
	}
	return err
}

func (p *LedgerProjection) checkDiskLimit(tx *bbolt.Tx) error {
	if p == nil || p.maxDiskBytes <= 0 {
		return nil
	}
	// bbolt MaxSize is the commit-time growth guard. Tx.Size is a secondary
	// check for an already oversized fresh candidate, not a projected-size test.
	if tx.Size() > p.maxDiskBytes {
		return ErrProjectionDiskLimit
	}
	return nil
}

// One native fact and all its domain/index changes share the same transaction.
func applyProjectionRecord(ctx context.Context, tx projectionRows, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	state, request, repeated, err := projectionWorkingState(tx, record)
	if err != nil {
		return err
	}
	previousHead := state.watermark
	if err := state.Apply(record); err != nil {
		return err
	}
	first, found, err := projectionRowsGet[Watermark](tx, "event_positions", projectionIDKey(record.Event.EventID))
	if err != nil {
		return err
	}
	// A governance idempotency alias has no domain effect, but a new
	// EventID still has its own first occurrence. The effect flag from
	// the reducer is not an event-ID existence test.
	_, hasDigest, err := tx.get("events", projectionIDKey(record.Event.EventID))
	if err != nil {
		return err
	}
	// An existing first occurrence must fit the previously committed cut.
	// The incoming duplicate cannot legitimize a damaged future position.
	if found != hasDigest || found && validateProjectionRecordPosition(first, previousHead) != nil {
		return ErrProjectionCorrupt
	}
	if !found {
		if err := projectionRowsPut(tx, "event_positions", projectionIDKey(record.Event.EventID), Watermark{record.Generation, record.Offset, record.Sequence}); err != nil {
			return err
		}
	}
	if err := projectionRowsPut(tx, "events", projectionIDKey(record.Event.EventID), state.eventDigests[record.Event.EventID]); err != nil {
		return err
	}
	if !repeated {
		if err := projectionStoreWorkingState(tx, state, record, request); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := projectionRowsPut(tx, "meta", []byte("position"), projectionPosition{state.watermark, record.Hash, record.Epoch}); err != nil {
		return err
	}
	return nil
}

// Hydrate only the event's dependency set. RunCreated's lifetime limit uses a
// scalar built from genesis in the same transaction as each Run/index. Loading
// every old Run would exceed the byte limit for legal wide native definitions.
// The existing reducer supplies the
// accounting semantics; its partial in-memory mutations on an error are local
// to this disposable working set and never escape the rolled-back transaction.
func projectionWorkingState(tx projectionRows, record Record) (*State, *ProjectedRequest, bool, error) {
	s := NewState()
	e := record.Event
	position, ok, err := projectionRowsGet[projectionPosition](tx, "meta", []byte("position"))
	if err != nil || !ok {
		return nil, nil, false, errors.Join(err, ErrProjectionCorrupt)
	}
	s.watermark = position.Watermark
	// Apply's sequence check remains authoritative; physical generation/offset
	// also cannot regress even for a deterministic duplicate event.
	if record.Generation < s.watermark.Generation || (record.Generation == s.watermark.Generation && record.Offset < s.watermark.Offset) {
		return nil, nil, false, fmt.Errorf("%w: physical record position did not advance", ErrProjectionCorrupt)
	}
	if record.Sequence == s.watermark.Sequence && (record.Generation != s.watermark.Generation || record.Offset != s.watermark.Offset || record.Hash != position.Hash || record.Epoch != position.Epoch) {
		return nil, nil, false, fmt.Errorf("%w: same sequence changed its physical evidence", ErrProjectionCorrupt)
	}
	if record.Sequence > s.watermark.Sequence && record.Generation == s.watermark.Generation && record.Offset == s.watermark.Offset {
		return nil, nil, false, ErrProjectionCorrupt
	}
	digest, repeated, err := projectionRowsGet[[32]byte](tx, "events", projectionIDKey(e.EventID))
	if err != nil {
		return nil, nil, false, err
	}
	if repeated {
		s.eventDigests[e.EventID] = digest
		return s, nil, true, nil
	}
	balanceKey := BalanceKey{ProjectID: e.ProjectID, PeriodID: e.PeriodID, TimezoneVersion: e.PeriodTimezoneVersion}
	balance, _, err := projectionRowsGet[Balance](tx, "balances", projectionBalanceKey(balanceKey))
	if err != nil {
		return nil, nil, false, err
	}
	s.balances[balanceKey] = balance
	// Only attempt events depend on attempt state. Other valid event kinds
	// may carry an AttemptID, but the reducer gives that field no effect.
	if e.Kind >= EventReservationCreated && e.Kind <= EventAttemptSettled {
		reservation, hasReservation, err := projectionRowsGet[projectionReservation](tx, "pending", projectionIDKey(e.AttemptID))
		if err != nil {
			return nil, nil, false, err
		}
		if hasReservation {
			if validateProjectedReservation(reservation, e.AttemptID) != nil || validateProjectionRecordPosition(Watermark{reservation.Evidence.Generation, reservation.Evidence.Offset, reservation.Evidence.Sequence}, s.watermark) != nil {
				return nil, nil, false, ErrProjectionCorrupt
			}
			if reservation.Reservation.Lease.RequestID != e.RequestID {
				return nil, nil, false, errors.New("attempt event changed its request")
			}
			s.reservations[e.AttemptID] = reservation.Reservation
			s.leaseRecords[e.AttemptID] = reservation.Evidence
		}
		settled, hasSettled, err := projectionRowsGet[SettledAttempt](tx, "settled", projectionIDKey(e.AttemptID))
		if err != nil {
			return nil, nil, false, err
		}
		if hasSettled {
			if err := validateProjectedSettlement(settled, e.AttemptID); err != nil {
				return nil, nil, false, err
			}
			s.settled[e.AttemptID] = settled
		}
	}
	if e.WorkUnitID != "" {
		value, found, err := projectionRowsGet[domain.WorkUnit](tx, "work_units", projectionIDKey(e.WorkUnitID))
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			if value.ID != e.WorkUnitID {
				return nil, nil, false, ErrProjectionCorrupt
			}
			s.workUnits[e.WorkUnitID] = value
		}
	}
	if e.RunID != "" {
		value, found, err := projectionRowsGet[domain.Run](tx, "runs", projectionIDKey(e.RunID))
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			if value.ID != e.RunID {
				return nil, nil, false, ErrProjectionCorrupt
			}
			s.runs[e.RunID] = value
		}
	}
	noEffect := false
	if e.Kind >= EventWorkUnitCreated {
		key := governanceIdempotencyMapKey(e.ProjectID, e.Operation, e.IdempotencyKeyHash)
		value, found, err := projectionRowsGet[GovernanceIdempotency](tx, "idempotency", projectionTuple(e.ProjectID, e.Operation, e.IdempotencyKeyHash))
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			if err := validateProjectedGovernanceIdempotency(value, e.ProjectID, e.Operation, e.IdempotencyKeyHash); err != nil {
				return nil, nil, false, err
			}
			s.governanceIdempotency[key] = value
			noEffect = true
		}
	}
	if e.Kind == EventRunCreated && !noEffect {
		// Preserve Apply's existing-ID and invalid-parent error precedence.
		// A governance alias has no effect even at the lifetime limit.
		_, exists := s.runs[e.RunID]
		unit, found := s.workUnits[e.WorkUnitID]
		if !exists && found && unit.ProjectID == e.ProjectID && unit.Status == domain.WorkUnitOpen {
			count, err := projectionRunCount(tx, e.WorkUnitID)
			if err != nil {
				return nil, nil, false, err
			}
			if count == domain.MaxRunsPerWorkUnit {
				return nil, nil, false, errors.New("work unit run limit exceeded")
			}
		}
	}
	var accepted *ProjectedRequest
	if e.Kind <= EventRequestFinalized {
		value, found, err := projectionRowsGet[ProjectedRequest](tx, "requests", projectionIDKey(e.RequestID))
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			if err := validateProjectedRequest(value, e.RequestID, s.watermark); err != nil {
				return nil, nil, false, err
			}
			origin, originErr := value.RecoveryOrigin()
			legacy := value.Legacy()
			if originErr != nil && legacy && value.Finalized != nil {
				origin, originErr = *value.Finalized, nil
			}
			a := origin.Event
			if originErr != nil || a.RequestID != e.RequestID {
				return nil, nil, false, ErrProjectionCorrupt
			}
			if legacy && (e.WorkUnitID != "" || e.RunID != "") {
				return nil, nil, false, ErrProjectionCorrupt
			}
			if !legacy && (a.ProjectID != e.ProjectID || a.PeriodID != e.PeriodID || a.PeriodTimezoneVersion != e.PeriodTimezoneVersion || a.WorkUnitID != e.WorkUnitID || a.RunID != e.RunID) {
				return nil, nil, false, errors.New("request event changed its accepted attribution")
			}
			if !legacy && value.Finalized != nil && e.Kind != EventAttemptSettled && e.Kind != EventRequestFinalized {
				return nil, nil, false, errors.New("request is already finalized")
			}
			accepted = &value
			if a.RunID != "" {
				s.requestRuns[e.RequestID] = requestAttribution{WorkUnitID: a.WorkUnitID, RunID: a.RunID}
			}
		}
	}
	return s, accepted, noEffect, nil
}

// This count is a derived fact, never imported from a saved ordinary DB. Zero
// is seeded with WorkUnitCreated; only a successful, non-alias RunCreated adds
// one. Closing a Run does not free lifetime capacity. Both stores commit it
// atomically with the Run, association index, event digest and native position.
func projectionRunCount(tx projectionRows, workUnitID string) (uint64, error) {
	raw, found, err := tx.get("meta", projectionTuple("work_unit_run_count", workUnitID))
	if err != nil {
		return 0, err
	}
	var count uint64
	if !found || len(raw) == 0 || len(raw) > 2 || json.Unmarshal(raw, &count) != nil || count > domain.MaxRunsPerWorkUnit || !bytes.Equal(raw, []byte(fmt.Sprint(count))) {
		return 0, ErrProjectionCorrupt
	}
	return count, nil
}

func projectionUpdateRunCount(tx projectionRows, e Event) error {
	key := projectionTuple("work_unit_run_count", e.WorkUnitID)
	switch e.Kind {
	case EventWorkUnitCreated:
		_, found, err := tx.get("meta", key)
		if err != nil {
			return err
		}
		if found {
			return ErrProjectionCorrupt
		}
		return projectionRowsPut(tx, "meta", key, uint64(0))
	case EventRunCreated:
		count, err := projectionRunCount(tx, e.WorkUnitID)
		if err != nil {
			return err
		}
		if count == domain.MaxRunsPerWorkUnit {
			return errors.New("work unit run limit exceeded")
		}
		return projectionRowsPut(tx, "meta", key, count+1)
	default:
		return nil
	}
}

func projectionStoreWorkingState(tx projectionRows, s *State, record Record, request *ProjectedRequest) error {
	e := record.Event
	if err := projectionUpdateRunCount(tx, e); err != nil {
		return err
	}
	if err := updateGovernanceExportPositions(tx, s, record); err != nil {
		return err
	}
	if err := projectionUpdateCohort(tx, s, record, request); err != nil {
		return err
	}
	if err := projectionUpdatePendingStatistics(tx, e); err != nil {
		return err
	}
	if e.Kind == EventReservationCreated {
		if err := tx.put("attempts_by_request", projectionTuple(e.RequestID, e.AttemptID), []byte(e.AttemptID)); err != nil {
			return err
		}
	}
	for key, value := range s.balances {
		if err := projectionRowsPut(tx, "balances", projectionBalanceKey(key), value); err != nil {
			return err
		}
	}
	if e.Kind >= EventReservationCreated && e.Kind <= EventAttemptSettled {
		if value, found := s.reservations[e.AttemptID]; found {
			if err := projectionRowsPut(tx, "pending", projectionIDKey(e.AttemptID), projectionReservation{value, s.leaseRecords[e.AttemptID]}); err != nil {
				return err
			}
		} else if err := tx.delete("pending", projectionIDKey(e.AttemptID)); err != nil {
			return err
		}
		if value, found := s.leaseRecords[e.AttemptID]; found {
			if err := projectionRowsPut(tx, "leases", projectionIDKey(e.AttemptID), value); err != nil {
				return err
			}
		}
		if value, found := s.settled[e.AttemptID]; found && e.Kind == EventAttemptSettled {
			if err := projectionRowsPut(tx, "settled", projectionIDKey(e.AttemptID), value); err != nil {
				return err
			}
			if err := projectionRowsPut(tx, "meta", projectionTuple("settlement", e.AttemptID), s.watermark); err != nil {
				return err
			}
		}
	}
	if e.Kind == EventWorkUnitCreated || e.Kind == EventWorkUnitClosed {
		if err := projectionRowsPut(tx, "work_units", projectionIDKey(e.WorkUnitID), s.workUnits[e.WorkUnitID]); err != nil {
			return err
		}
	}
	if e.RunID != "" && (e.Kind == EventRunCreated || e.Kind == EventRunClosed || e.Kind == EventReservationCreated || e.Kind == EventAttemptSettled) {
		if value, found := s.runs[e.RunID]; found {
			if err := projectionRowsPut(tx, "runs", projectionIDKey(e.RunID), value); err != nil {
				return err
			}
		}
	}
	if e.Kind >= EventWorkUnitCreated {
		value := s.governanceIdempotency[governanceIdempotencyMapKey(e.ProjectID, e.Operation, e.IdempotencyKeyHash)]
		if err := projectionRowsPut(tx, "idempotency", projectionTuple(e.ProjectID, e.Operation, e.IdempotencyKeyHash), value); err != nil {
			return err
		}
	}
	if e.Kind == EventRequestAccepted {
		if request == nil {
			request = &ProjectedRequest{}
		}
		if request.Accepted.Sequence == 0 {
			request.Accepted = record
		}
	}
	if e.Kind == EventReservationCreated && e.WorkUnitID == "" && e.RunID == "" {
		if request == nil {
			request = &ProjectedRequest{}
		}
		if request.Legacy() && request.LegacyLease == nil {
			first := record
			request.LegacyLease = &first
		}
	}
	if e.Kind == EventRequestFinalized {
		if request == nil {
			request = &ProjectedRequest{}
		}
		if request.Finalized == nil {
			request.Finalized = &record
		}
	}
	if request != nil {
		if err := projectionRowsPut(tx, "requests", projectionIDKey(e.RequestID), request); err != nil {
			return err
		}
	}
	index := func(bucket string, key []byte, id string, remove bool) error {
		if remove {
			return tx.delete(bucket, key)
		}
		return tx.put(bucket, key, []byte(id))
	}
	if err := projectionRowsStoreListIndexes(tx, e); err != nil {
		return err
	}
	switch e.Kind {
	case EventWorkUnitCreated, EventWorkUnitClosed:
		return index("open_work_units_by_project", projectionTuple(e.ProjectID, e.WorkUnitID), e.WorkUnitID, e.Kind == EventWorkUnitClosed)
	case EventRunCreated:
		if err := index("runs_by_work_unit", projectionTuple(e.WorkUnitID, e.RunID), e.RunID, false); err != nil {
			return err
		}
		return index("active_runs_by_project", projectionActiveRunKey(s.runs[e.RunID]), e.RunID, false)
	case EventRunClosed:
		return index("active_runs_by_project", projectionActiveRunKey(s.runs[e.RunID]), e.RunID, true)
	case EventRequestAccepted:
		if request != nil && request.Finalized != nil {
			return nil
		}
		if e.RunID != "" {
			if err := index("active_requests_by_work_unit", projectionTuple(e.WorkUnitID, e.RequestID), e.RequestID, false); err != nil {
				return err
			}
		}
		return index("active_requests", projectionIDKey(e.RequestID), e.RequestID, false)
	case EventRequestFinalized:
		if request != nil && request.Accepted.Event.RunID != "" {
			if err := index("active_requests_by_work_unit", projectionTuple(request.Accepted.Event.WorkUnitID, e.RequestID), e.RequestID, true); err != nil {
				return err
			}
		}
		return index("active_requests", projectionIDKey(e.RequestID), e.RequestID, true)
	case EventReservationCreated:
		if request != nil && request.LegacyLease != nil && request.Finalized == nil {
			if err := index("active_requests", projectionIDKey(e.RequestID), e.RequestID, false); err != nil {
				return err
			}
		}
		if e.WorkUnitID != "" {
			return index("pending_by_work_unit", projectionTuple(e.WorkUnitID, e.AttemptID), e.AttemptID, false)
		}
	case EventAttemptSettled:
		if e.WorkUnitID != "" {
			if err := index("pending_by_work_unit", projectionTuple(e.WorkUnitID, e.AttemptID), e.AttemptID, true); err != nil {
				return err
			}
			return index("settled_by_work_unit", projectionTuple(e.WorkUnitID, e.AttemptID), e.AttemptID, false)
		}
	}
	return nil
}

func projectionGet[T any](tx *bbolt.Tx, bucket string, key []byte) (T, bool, error) {
	return projectionRowsGet[T](boltProjectionRows{tx}, bucket, key)
}

func projectionPut(tx *bbolt.Tx, bucket string, key []byte, value any) error {
	return projectionRowsPut(boltProjectionRows{tx}, bucket, key, value)
}

// Native WAL identities are bounded by the frame payload, not bbolt's much
// smaller key limit. Fixed SHA-256 keys preserve that input contract. Complete
// original identities remain in domain rows/index values, and event digests
// bind full event contents. These hashes provide lookup keys, not authentication
// of a persisted cache.
func projectionIDKey(id string) []byte {
	if id == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(id))
	return hash[:]
}

// Hash each component separately so a scoped prefix still spans its entire
// association set and combined wide identities never exceed the key limit.
func projectionTuple(parts ...string) []byte {
	result := make([]byte, 0, len(parts)*sha256.Size)
	for _, part := range parts {
		hash := sha256.Sum256([]byte(part))
		result = append(result, hash[:]...)
	}
	return result
}

func projectionBalanceKey(key BalanceKey) []byte {
	return binary.BigEndian.AppendUint64(projectionTuple(key.ProjectID, key.PeriodID), key.TimezoneVersion)
}

func projectionRead[T any](ctx context.Context, p *LedgerProjection, bucket string, key []byte) (T, bool, error) {
	return projectionReadChecked[T](ctx, p, bucket, key, nil)
}

// The row and its validation cut belong to the same Bolt read transaction.
func projectionReadChecked[T any](ctx context.Context, p *LedgerProjection, bucket string, key []byte, validate func(T, Watermark) error) (T, bool, error) {
	var value T
	var found bool
	if err := ctx.Err(); err != nil {
		return value, false, err
	}
	if p == nil {
		return value, false, ErrProjectionUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return value, false, ErrProjectionUnavailable
	}
	err := p.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		value, found, err = projectionGet[T](tx, bucket, key)
		if err != nil {
			return err
		}
		if validate != nil {
			position, hasPosition, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
			if err != nil || !hasPosition {
				return errors.Join(ErrProjectionCorrupt, err)
			}
			if found {
				if err := validate(value, position.Watermark); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	})
	if err != nil {
		var zero T
		return zero, false, err
	}
	return value, found, err
}

func (p *LedgerProjection) ReadBalance(ctx context.Context, key BalanceKey) (Balance, error) {
	v, _, err := projectionRead[Balance](ctx, p, "balances", projectionBalanceKey(key))
	if err == nil && (v.CommittedMicrosUSD < 0 || v.ReservedMicrosUSD < 0 || v.InputTokens < 0 || v.OutputTokens < 0 || v.UnknownAttempts < 0) {
		return Balance{}, ErrProjectionCorrupt
	}
	return v, err
}
func (p *LedgerProjection) ReadLease(ctx context.Context, id string) (Record, bool, error) {
	return projectionReadChecked(ctx, p, "leases", projectionIDKey(id), func(row Record, head Watermark) error {
		return validateProjectedLease(row, id, head)
	})
}
func (p *LedgerProjection) ReadSettlement(ctx context.Context, id string) (SettledAttempt, bool, error) {
	row, found, err := projectionRead[SettledAttempt](ctx, p, "settled", projectionIDKey(id))
	if err == nil && found {
		err = validateProjectedSettlement(row, id)
	}
	if err != nil {
		return SettledAttempt{}, false, err
	}
	return row, found, nil
}
func (p *LedgerProjection) ReadRequest(ctx context.Context, id string) (ProjectedRequest, bool, error) {
	return projectionReadChecked(ctx, p, "requests", projectionIDKey(id), func(row ProjectedRequest, head Watermark) error {
		return validateProjectedRequest(row, id, head)
	})
}

// ReadEventPosition returns the first physical occurrence, preserved across
// later deterministic duplicates. The position and digest commit atomically;
// a partial index is corruption, never evidence of a new event. As with other
// candidate reads, this does not authenticate a reopened or externally edited DB.
func (p *LedgerProjection) ReadEventPosition(ctx context.Context, id string) (Watermark, bool, error) {
	var first Watermark
	var found bool
	if err := ctx.Err(); err != nil {
		return first, false, err
	}
	if p == nil {
		return first, false, ErrProjectionUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return first, false, ErrProjectionUnavailable
	}
	err := p.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		first, found, err = projectionGet[Watermark](tx, "event_positions", projectionIDKey(id))
		if err != nil {
			return err
		}
		_, hasDigest, err := projectionGet[[32]byte](tx, "events", projectionIDKey(id))
		if err != nil {
			return err
		}
		position, hasPosition, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !hasPosition {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		if found != hasDigest || found && validateProjectionRecordPosition(first, position.Watermark) != nil {
			return ErrProjectionCorrupt
		}
		return ctx.Err()
	})
	if err != nil {
		return Watermark{}, false, err
	}
	return first, found, nil
}

// ReadFinalization supplies the original terminal fact and its physical
// sequence, rather than a latest-state boolean. A lagging consumer must compare
// this sequence with the record it is processing. Persisted content/key-set
// trust still follows this candidate's D-03 restriction.
func (p *LedgerProjection) ReadFinalization(ctx context.Context, id string) (Record, bool, error) {
	row, found, err := p.ReadRequest(ctx, id)
	if err != nil || !found {
		return Record{}, false, err
	}
	if row.Finalized == nil {
		if _, err := row.RecoveryOrigin(); err != nil {
			return Record{}, false, err
		}
		return Record{}, false, nil
	}
	final := *row.Finalized
	modern := !row.Legacy()
	if final.Sequence == 0 || final.Event.Kind != EventRequestFinalized || final.Event.RequestID != id || modern && (row.Accepted.Event.Kind != EventRequestAccepted || row.Accepted.Event.RequestID != id || final.Sequence <= row.Accepted.Sequence) || !modern && (final.Event.WorkUnitID != "" || final.Event.RunID != "") {
		return Record{}, false, ErrProjectionCorrupt
	}
	return final, true, nil
}

// ReadLegacyRequest describes the original authenticated request shape, not a
// permission inferred from an absent runtime field. Usage may retain legacy
// per-attempt period/project rules only for this same selected fresh owner.
func (p *LedgerProjection) ReadLegacyRequest(ctx context.Context, id string) (bool, error) {
	r, found, err := p.ReadRequest(ctx, id)
	if err != nil || !found {
		return false, err
	}
	return r.Legacy(), nil
}
func (p *LedgerProjection) ReadRun(ctx context.Context, id string) (domain.Run, bool, error) {
	v, found, err := projectionRead[domain.Run](ctx, p, "runs", projectionIDKey(id))
	if err != nil {
		return domain.Run{}, false, err
	}
	if found && (v.ID != id || v.Validate() != nil) {
		return domain.Run{}, false, ErrProjectionCorrupt
	}
	return v, found, nil
}
func (p *LedgerProjection) ReadWorkUnit(ctx context.Context, id string) (domain.WorkUnit, bool, error) {
	v, found, err := projectionRead[domain.WorkUnit](ctx, p, "work_units", projectionIDKey(id))
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if found && (v.ID != id || v.Validate() != nil) {
		return domain.WorkUnit{}, false, ErrProjectionCorrupt
	}
	return v, found, nil
}
func (p *LedgerProjection) ReadGovernanceIdempotency(ctx context.Context, projectID, operation, keyHash string) (GovernanceIdempotency, bool, error) {
	v, found, err := projectionRead[GovernanceIdempotency](ctx, p, "idempotency", projectionTuple(projectID, operation, keyHash))
	if err != nil {
		return GovernanceIdempotency{}, false, err
	}
	if found && validateProjectedGovernanceIdempotency(v, projectID, operation, keyHash) != nil {
		return GovernanceIdempotency{}, false, ErrProjectionCorrupt
	}
	return v, found, nil
}
func (p *LedgerProjection) ReadPosition(ctx context.Context) (Watermark, [32]byte, error) {
	if p == nil {
		return Watermark{}, [32]byte{}, errors.New("accounting projection is absent")
	}
	v, found, err := projectionRead[projectionPosition](ctx, p, "meta", []byte("position"))
	if err == nil && !found {
		err = ErrProjectionCorrupt
	}
	return v.Watermark, v.Hash, err
}

// ProjectionCursor is local to a fresh projection generation and query. Head
// freezes an enumeration's admission prefix. Recovery may remove completed
// rows between pages; records accepted after Head are excluded. This is not a
// general snapshot query: current pending/active status can change between pages.
type ProjectionCursor struct {
	generation [16]byte
	kind       string
	scope      string
	head       Watermark
	after      []byte
}
type ProjectionPageLimits struct {
	Rows  int
	Bytes int
}
type ProjectionPage[T any] struct {
	Items   []T
	Next    ProjectionCursor
	Done    bool
	Scanned int
	Bytes   int
}

// RecoveryScan freezes both independent input sets at the same applied head.
// The caller must own the maintenance writer exclusively; these cursors do not
// hold a DB reader or prevent new business admission by themselves.
type RecoveryScan struct {
	Head     Watermark
	Pending  ProjectionCursor
	Requests ProjectionCursor
}

func (p *LedgerProjection) BeginRecoveryScan(ctx context.Context) (RecoveryScan, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return RecoveryScan{}, ErrProjectionUnavailable
	}
	var scan RecoveryScan
	err := p.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		position, found, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !found {
			return errors.Join(err, ErrProjectionCorrupt)
		}
		scan = RecoveryScan{Head: position.Watermark,
			Pending:  ProjectionCursor{generation: p.generation, kind: "pending", head: position.Watermark},
			Requests: ProjectionCursor{generation: p.generation, kind: "active_requests", head: position.Watermark},
		}
		return nil
	})
	return scan, err
}

type ProjectedAttempt struct {
	Lease      Record
	Pending    bool
	Settlement *SettledAttempt
}

// RequestAttemptPage enumerates the association rather than inferring request
// existence from pending attempts. Each page releases its reader before a
// maintenance caller can append or wait for confirmation.
func (p *LedgerProjection) RequestAttemptPage(ctx context.Context, requestID string, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[ProjectedAttempt], error) {
	if requestID == "" || len(requestID) > maxPayloadSize {
		return ProjectionPage[ProjectedAttempt]{}, ErrProjectionPageLimit
	}
	return projectionPage(ctx, p, "attempts_by_request", requestID, cursor, limits, func(tx *bbolt.Tx, k, v []byte, head Watermark) (ProjectedAttempt, uint64, int, error) {
		lease, found, err := projectionGet[Record](tx, "leases", projectionIDKey(string(v)))
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		if !found || validateProjectedLease(lease, string(v), head) != nil || lease.Event.RequestID != requestID || !bytes.Equal(v, []byte(lease.Event.AttemptID)) || !bytes.Equal(k, projectionTuple(requestID, lease.Event.AttemptID)) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		settled, hasSettlement, err := projectionGet[SettledAttempt](tx, "settled", projectionIDKey(string(v)))
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		pending, hasPending, err := projectionGet[projectionReservation](tx, "pending", projectionIDKey(string(v)))
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		if hasPending == hasSettlement || (hasPending && (pending.Reservation.Lease.AttemptID != lease.Event.AttemptID || pending.Reservation.Lease.RequestID != requestID)) || (hasSettlement && (settled.Settlement.Kind != EventAttemptSettled || settled.Settlement.RequestID != requestID || settled.Settlement.AttemptID != lease.Event.AttemptID)) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		if hasPending && (validateProjectedReservation(pending, lease.Event.AttemptID) != nil || validateProjectionRecordPosition(Watermark{pending.Evidence.Generation, pending.Evidence.Offset, pending.Evidence.Sequence}, head) != nil) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		row := ProjectedAttempt{Lease: lease, Pending: hasPending}
		if hasSettlement {
			if err := validateProjectedSettlement(settled, lease.Event.AttemptID); err != nil {
				return ProjectedAttempt{}, 0, 0, err
			}
			position, found, err := projectionGet[Watermark](tx, "meta", projectionTuple("settlement", lease.Event.AttemptID))
			if err != nil || !found || validateProjectionRecordPosition(position, head) != nil {
				return ProjectedAttempt{}, 0, 0, errors.Join(ErrProjectionCorrupt, err)
			}
			row.Settlement = &settled
		}
		size := len(k) + len(v) + len(tx.Bucket([]byte("leases")).Get(projectionIDKey(string(v)))) + len(tx.Bucket([]byte("settled")).Get(projectionIDKey(string(v)))) + len(tx.Bucket([]byte("pending")).Get(projectionIDKey(string(v))))
		if hasSettlement {
			size += len(tx.Bucket([]byte("meta")).Get(projectionTuple("settlement", lease.Event.AttemptID)))
		}
		return row, lease.Sequence, size, nil
	})
}

func projectionPage[T any](ctx context.Context, p *LedgerProjection, kind, scope string, cursor ProjectionCursor, limits ProjectionPageLimits, decode func(*bbolt.Tx, []byte, []byte, Watermark) (T, uint64, int, error)) (ProjectionPage[T], error) {
	var page ProjectionPage[T]
	if limits.Rows <= 0 || limits.Rows > 1024 || limits.Bytes <= 0 || limits.Bytes > 16<<20 {
		return page, ErrProjectionPageLimit
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return page, ErrProjectionUnavailable
	}
	err := p.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		position, found, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil {
			return err
		}
		if !found {
			return ErrProjectionCorrupt
		}
		if cursor.kind == "" {
			cursor = ProjectionCursor{generation: p.generation, kind: kind, scope: scope, head: position.Watermark}
		}
		if cursor.kind != kind || cursor.scope != scope || cursor.generation != p.generation || cursor.head.After(position.Watermark) || len(cursor.after) > bbolt.MaxKeySize {
			return ErrProjectionCorrupt
		}
		page.Next = cursor
		b := tx.Bucket([]byte(kind))
		if b == nil {
			return ErrProjectionCorrupt
		}
		c := b.Cursor()
		k, v := c.First()
		var prefix []byte
		if scope != "" {
			prefix = projectionTuple(scope)
			k, v = c.Seek(prefix)
		}
		if len(cursor.after) > 0 {
			k, v = c.Seek(cursor.after)
			if bytes.Equal(k, cursor.after) {
				k, v = c.Next()
			}
		}
		for ; k != nil; k, v = c.Next() {
			if len(prefix) > 0 && !bytes.HasPrefix(k, prefix) {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if page.Scanned == limits.Rows {
				return nil
			}
			item, sequence, size, err := decode(tx, k, v, position.Watermark)
			if err != nil {
				return err
			}
			if size > limits.Bytes {
				return ErrProjectionPageLimit
			}
			if size > limits.Bytes-page.Bytes {
				return nil
			}
			page.Scanned++
			page.Bytes += size
			page.Next.after = bytes.Clone(k)
			if sequence <= cursor.head.Sequence {
				page.Items = append(page.Items, item)
			}
		}
		page.Done = true
		return nil
	})
	if err != nil {
		return ProjectionPage[T]{}, err
	}
	return page, nil
}

func (p *LedgerProjection) PendingLeasePage(ctx context.Context, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[PendingLease], error) {
	return projectionPage(ctx, p, "pending", "", cursor, limits, func(tx *bbolt.Tx, k, v []byte, head Watermark) (PendingLease, uint64, int, error) {
		row, found, err := projectionGet[projectionReservation](tx, "pending", k)
		if err != nil {
			return PendingLease{}, 0, 0, err
		}
		if !found || !bytes.Equal(projectionIDKey(row.Reservation.Lease.AttemptID), k) || validateProjectedReservation(row, row.Reservation.Lease.AttemptID) != nil || validateProjectionRecordPosition(Watermark{row.Evidence.Generation, row.Evidence.Offset, row.Evidence.Sequence}, head) != nil {
			return PendingLease{}, 0, 0, ErrProjectionCorrupt
		}
		return PendingLease{Reservation: row.Reservation.Lease, Started: row.Reservation.Started}, row.Evidence.Sequence, len(v), nil
	})
}
func (p *LedgerProjection) UnfinalizedRequestPage(ctx context.Context, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[ProjectedRequest], error) {
	return projectionPage(ctx, p, "active_requests", "", cursor, limits, func(tx *bbolt.Tx, k, v []byte, head Watermark) (ProjectedRequest, uint64, int, error) {
		row, found, err := projectionGet[ProjectedRequest](tx, "requests", projectionIDKey(string(v)))
		if err != nil {
			return ProjectedRequest{}, 0, 0, err
		}
		origin, originErr := row.RecoveryOrigin()
		if !found || originErr != nil || validateProjectedRequest(row, string(v), head) != nil || row.Finalized != nil || !bytes.Equal(projectionIDKey(origin.Event.RequestID), k) || !bytes.Equal(k, projectionIDKey(string(v))) {
			return ProjectedRequest{}, 0, 0, ErrProjectionCorrupt
		}
		raw := tx.Bucket([]byte("requests")).Get(projectionIDKey(string(v)))
		return row, origin.Sequence, len(raw) + len(k) + len(v), nil
	})
}
func (p *LedgerProjection) SettlementPage(ctx context.Context, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[SettledAttempt], error) {
	return projectionPage(ctx, p, "settled", "", cursor, limits, func(tx *bbolt.Tx, k, v []byte, head Watermark) (SettledAttempt, uint64, int, error) {
		row, found, err := projectionGet[SettledAttempt](tx, "settled", k)
		if err != nil {
			return SettledAttempt{}, 0, 0, err
		}
		if !found || !bytes.Equal(projectionIDKey(row.Settlement.AttemptID), k) || validateProjectedSettlement(row, row.Settlement.AttemptID) != nil {
			return SettledAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		// Settlement events carry no sequence themselves; use a companion event
		// position row to freeze the enumeration below.
		position, found, err := projectionGet[Watermark](tx, "meta", projectionTuple("settlement", row.Settlement.AttemptID))
		if err != nil {
			return SettledAttempt{}, 0, 0, err
		}
		if !found || validateProjectionRecordPosition(position, head) != nil {
			return SettledAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		size := len(v) + len(tx.Bucket([]byte("meta")).Get(projectionTuple("settlement", row.Settlement.AttemptID)))
		return row, position.Sequence, size, nil
	})
}
