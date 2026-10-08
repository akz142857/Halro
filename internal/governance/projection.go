package governance

// This transitional projection is built into a fresh exclusive file from the
// complete authenticated journal, or restored from a selected content root into
// a fresh exclusive image. It has no trust-existing-ordinary-file API.
// Authenticated checkpoints, role publication and shared budgets remain D-03,
// I-03 and I-11 work. Default Standalone selects a fresh disk owner on every
// Open in Standalone and Primary. Replica retains its opaque native journal.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

var (
	ErrProjectionDiskLimit        = errors.New("governance projection disk limit reached")
	ErrProjectionDiskLimitInvalid = errors.New("governance projection disk limit is invalid")
)

const governanceProjectionMaxRow = 3 * maxPayloadSize
const governanceProjectionBatchRows = 64
const governanceProjectionBatchBytes = 1 << 20

var governanceProjectionBuckets = []string{"meta", "outcomes", "heads", "idempotency", "by_project", "by_work_unit", "by_definition"}

type GovernanceProjection struct {
	physical        *haPhysicalGovernanceProjection
	haReadCtx       context.Context
	haReadCancel    context.CancelFunc
	selectedStartup bool
	sealDebt        resourcebudget.Permit
	mu              sync.RWMutex
	retired         atomic.Bool
	advanceMu       sync.Mutex
	advanceCancel   context.CancelFunc
	advanceDone     chan struct{}
	cow             *authenticatedstate.Store
	cowNative       *Log
	cowBudget       *resourcebudget.Manager
	cowViews        chan struct{}
	cowReaders      atomic.Int32
	cowClosed       bool
	retiredScratch  *GovernanceProjection
	db              *bbolt.DB
	ready           bool
	file            *ownedfile.Owner
	image           *authenticatedstate.MaterializedImage
	liveRAM         resourcebudget.Permit
	closeErr        error
	retirementErr   error
}

// The caller owns a quiescent journal and supplies its trusted, role-permitted
// frozen head. Partial tails, cancellation and any mismatch discard the output.
func BuildGovernanceProjection(ctx context.Context, source string, key []byte, output string, expected Summary) (*GovernanceProjection, error) {
	return BuildGovernanceProjectionWithLimit(ctx, source, key, output, expected, 0)
}

// BuildGovernanceProjectionWithLimit builds the same fresh authenticated
// projection while bounding its bbolt data file when maxBytes is positive.
// Zero retains the library's historical unbounded behavior for maintenance
// callers that supply their own workspace policy.
func BuildGovernanceProjectionWithLimit(ctx context.Context, source string, key []byte, output string, expected Summary, maxBytes int64) (*GovernanceProjection, error) {
	if maxBytes < 0 || maxBytes > int64(^uint(0)>>1) {
		return nil, ErrProjectionDiskLimitInvalid
	}
	return buildGovernanceProjectionWithLimit(ctx, source, key, output, expected, maxBytes, nil, nil)
}

// Private seams observe real file/transaction boundaries, never replace source
// bytes or native authentication. Production always closes the actual os.File.
func buildGovernanceProjection(ctx context.Context, source string, key []byte, output string, expected Summary, phase func(string), closeSource func(*os.File) error) (*GovernanceProjection, error) {
	return buildGovernanceProjectionWithLimit(ctx, source, key, output, expected, 0, phase, closeSource)
}

func buildGovernanceProjectionWithLimit(ctx context.Context, source string, key []byte, output string, expected Summary, maxBytes int64, phase func(string), closeSource func(*os.File) error) (result *GovernanceProjection, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != governanceHMACKeySize {
		return nil, ErrCorrupt
	}
	p := &GovernanceProjection{}
	failed := true
	defer func() {
		if failed {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retirementErr = errors.Join(p.retirementErr, resultErr)
			}
			resultErr = errors.Join(resultErr, p.Discard())
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				result = p // cleanup-only: failed candidates never become ready
			}
		}
	}()
	var err error
	p.db, p.file, err = ownedfile.OpenExclusiveBolt(output, 0o600, &bbolt.Options{MaxSize: int(maxBytes), FreelistType: bbolt.FreelistMapType})
	if err != nil {
		return nil, projectionCapacityError(err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range governanceProjectionBuckets {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		return govPut(tx, "meta", []byte("position"), Summary{})
	}); err != nil {
		return nil, projectionCapacityError(err)
	}
	input, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	if closeSource == nil {
		closeSource = func(f *os.File) error { return f.Close() }
	}
	defer func() {
		if input != nil {
			if closeErr := closeSource(input); closeErr != nil {
				resultErr = errors.Join(resultErr, ownedfile.CloseError(input, closeErr))
			}
		}
	}()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.Bytes {
		return nil, ErrCorrupt
	}
	verifySource := func(checkFD bool) error {
		named, err := os.Stat(source)
		if err != nil || !os.SameFile(info, named) || !named.Mode().IsRegular() || named.Size() != expected.Bytes || !named.ModTime().Equal(info.ModTime()) {
			return errors.Join(ErrCorrupt, err)
		}
		if checkFD {
			actual, err := input.Stat()
			if err != nil || !os.SameFile(info, actual) || actual.Size() != expected.Bytes || !actual.ModTime().Equal(info.ModTime()) {
				return errors.Join(ErrCorrupt, err)
			}
		}
		return ctx.Err()
	}
	if err := verifySource(true); err != nil {
		return nil, err
	}
	batch := make([]Record, 0, governanceProjectionBatchRows)
	defer clear(batch[:cap(batch)])
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
	report, partial, err := scanFrom(governanceContextReader{ctx: ctx, source: input}, key, Summary{}, func(record Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > governanceProjectionMaxRow {
			return ErrCorrupt
		}
		if len(batch) == governanceProjectionBatchRows || batchBytes+len(raw) > governanceProjectionBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += len(raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if partial || report != expected {
		return nil, ErrCorrupt
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if phase != nil {
		phase("source-reduced")
	}
	if err := verifySource(true); err != nil {
		return nil, err
	}
	// Recheck the complete authenticated chain after the actual reduction.
	// A same-size overwrite of bytes already visited cannot hide behind the
	// final FD size, inode or a caller-restored modification timestamp.
	after, partial, err := scanFrom(governanceContextReader{ctx: ctx, source: input}, key, Summary{}, nil)
	if err != nil {
		return nil, err
	}
	if partial || after != expected {
		return nil, ErrCorrupt
	}
	if err := verifySource(true); err != nil {
		return nil, err
	}
	closing := input
	input = nil // actual close is attempted once, including failure/panic.
	if err := closeSource(closing); err != nil {
		return nil, ownedfile.CloseError(closing, err)
	}
	if phase != nil {
		phase("before-publication")
	}
	if err := verifySource(false); err != nil {
		return nil, err
	}
	p.ready = true
	failed = false
	return p, nil
}

func projectionCapacityError(err error) error {
	if errors.Is(err, berrors.ErrMaxSizeReached) {
		return errors.Join(ErrProjectionDiskLimit, err)
	}
	return err
}

func (p *GovernanceProjection) Close() error {
	if p.haReadCancel != nil {
		p.haReadCancel()
	}
	p.retired.Store(true)
	p.advanceMu.Lock()
	done := p.advanceDone
	if p.advanceCancel != nil {
		p.advanceCancel()
	}
	p.advanceMu.Unlock()
	if done != nil {
		<-done
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = false
	if p.cow != nil {
		if !p.cowClosed {
			p.cowClosed = true
			closeCow := func(ctx context.Context) error {
				if p.physical != nil {
					q := p.physical
					q.mu.Lock()
					defer q.mu.Unlock()
					if !q.closedQualified || q.pending != 0 || q.failure != nil {
						return errors.Join(q.failure, authenticatedstate.ErrRecovery, p.cow.HoldPhysicalPreparationForRecovery(ctx), p.cow.Close())
					}
					if q.window != nil {
						if err := q.window.Close(); err != nil {
							q.failure = err
							return errors.Join(err, p.cow.Close())
						}
						q.window = nil
					}
				}
				return p.cow.Close()
			}
			var err error
			closed := false
			joinCow := func(ctx context.Context) error { closed = true; return closeCow(ctx) }
			if p.physical != nil && p.physical.work != nil {
				err = p.physical.work.WithWorkspace(context.Background(), joinCow)
			} else {
				err = joinCow(context.Background())
			}
			if !closed {
				// A refused loan proves no exit. Still join the actual store,
				// retaining every fee because retirement is now uncertain.
				err = errors.Join(err, ownedfile.ErrRetirement, p.cow.Close())
			}
			p.closeErr = errors.Join(p.retirementErr, err)
			if p.closeErr == nil && p.physical != nil && p.physical.work != nil {
				p.closeErr = p.physical.work.Close()
				if p.closeErr == nil {
					p.closeErr = p.physical.work.Active.Release()
				}
			}
		}
		if p.closeErr == nil && p.liveRAM != (resourcebudget.Permit{}) {
			p.closeErr = p.liveRAM.Release()
			if p.closeErr == nil {
				p.liveRAM = resourcebudget.Permit{}
			}
		}
		return p.closeErr
	}
	if p.db == nil {
		if p.retirementErr == nil && p.liveRAM != (resourcebudget.Permit{}) {
			p.closeErr = errors.Join(p.closeErr, p.liveRAM.Release())
			if p.closeErr == nil {
				p.liveRAM = resourcebudget.Permit{}
			}
		}
		if p.closeErr == nil {
			p.closeErr = p.retirementErr
		}
		return p.closeErr
	}
	physicalErr := p.db.Close()
	if physicalErr != nil {
		physicalErr = errors.Join(ownedfile.ErrRetirement, physicalErr)
	}
	p.closeErr = errors.Join(p.closeErr, p.retirementErr, physicalErr)
	if errors.Is(p.closeErr, ownedfile.ErrRetirement) && p.file != nil {
		p.closeErr = errors.Join(p.closeErr, p.file.VerifyName())
	}
	p.db = nil
	if p.closeErr == nil && p.liveRAM != (resourcebudget.Permit{}) {
		p.closeErr = p.liveRAM.Release()
		if p.closeErr == nil {
			p.liveRAM = resourcebudget.Permit{}
		}
	}
	return p.closeErr
}

func (p *GovernanceProjection) Discard() error {
	if p == nil {
		return nil
	}
	if err := p.Close(); err != nil {
		return err
	}
	if p.cow != nil {
		return p.cow.Discard()
	}
	return errors.Join(p.file.Retire(), p.image.Remove())
}

func govPut(tx any, bucket string, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > governanceProjectionMaxRow {
		return ErrOutcomePageLimit
	}
	return govRawPut(tx, bucket, key, raw)
}

func govGet[T any](tx any, bucket string, key []byte) (T, bool, error) {
	var value T
	raw, found, err := govRawGet(tx, bucket, key)
	if err != nil || !found {
		return value, found, err
	}
	if len(raw) == 0 || len(raw) > governanceProjectionMaxRow {
		return value, false, ErrCorrupt
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, false, errors.Join(ErrCorrupt, err)
	}
	return value, true, nil
}

func govPosition(tx any) (Summary, error) {
	switch tx := tx.(type) {
	case *bbolt.Tx:
		for _, name := range governanceProjectionBuckets {
			if tx.Bucket([]byte(name)) == nil {
				return Summary{}, ErrCorrupt
			}
		}
	case *govRows:
		marker, found, err := tx.get([]byte{0})
		if err != nil || !found || !bytes.Equal(marker, typedGovernanceMarker) {
			return Summary{}, errors.Join(ErrCorrupt, err)
		}
	default:
		return Summary{}, ErrCorrupt
	}
	head, found, err := govGet[Summary](tx, "meta", []byte("position"))
	if err != nil || !found || head.Bytes < 0 || (head.Records == 0 && head != (Summary{})) || (head.Records > 0 && (head.Bytes <= 0 || head.LastHash == ([32]byte{}))) {
		return Summary{}, errors.Join(ErrCorrupt, err)
	}
	return head, nil
}

func govKey(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		h.Write([]byte(value))
	}
	return h.Sum(nil)
}

func recordOutcome(r Record) domain.Outcome {
	e := r.Event
	return domain.Outcome{ID: e.OutcomeID, ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID,
		DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion, Value: e.Value,
		ReporterKeyID: e.ReporterKeyID, EvidenceSHA256: e.EvidenceSHA256, EvidenceRef: e.EvidenceRef,
		ObservedAt: e.ObservedAt, IngestedAt: e.IngestedAt, SupersedesOutcomeID: e.SupersedesOutcomeID,
		Revision: e.Revision, GovernanceSequence: r.Sequence}
}

func govHeadKey(project, unit, definition string, version uint64) []byte {
	return govKey(project, unit, definition, strconv.FormatUint(version, 10))
}

func govRecord(tx any, id string, head Summary) (Record, bool, error) {
	r, found, err := govGet[Record](tx, "outcomes", []byte(id))
	if err != nil || !found {
		return Record{}, found, err
	}
	if r.Event.OutcomeID != id || r.Event.Validate() != nil || r.Sequence == 0 || r.Sequence > head.Records || r.Offset <= 0 || r.Offset > head.Bytes || r.Hash == ([32]byte{}) || r.Event.Revision > domain.MaxOutcomeRevisions {
		return Record{}, false, ErrCorrupt
	}
	return r, true, nil
}

func govCurrent(tx any, project, unit, definition string, version uint64, head Summary) (Record, bool, error) {
	id, found, err := govGet[string](tx, "heads", govHeadKey(project, unit, definition, version))
	if err != nil || !found {
		return Record{}, found, err
	}
	r, found, err := govRecord(tx, id, head)
	if err != nil || !found || r.Event.ProjectID != project || r.Event.WorkUnitID != unit || r.Event.DefinitionID != definition || r.Event.DefinitionVersion != version {
		return Record{}, false, errors.Join(ErrCorrupt, err)
	}
	return r, true, nil
}

func applyGovernanceRows(ctx context.Context, tx any, records []Record) error {
	head, err := govPosition(tx)
	if err != nil {
		return err
	}
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := r.Event
		if e.Validate() != nil || r.Sequence != head.Records+1 || r.Offset <= head.Bytes || r.Offset-head.Bytes > maxPayloadSize+frameHeaderSize+frameMACSize || r.Hash == ([32]byte{}) {
			return ErrCorrupt
		}
		payload, err := json.Marshal(e)
		if err != nil || len(payload) > maxPayloadSize {
			return errors.Join(ErrCorrupt, err)
		}
		if _, found, err := govRawGet(tx, "outcomes", []byte(e.OutcomeID)); err != nil {
			return err
		} else if found {
			return ErrRevisionConflict
		}
		current, found, err := govCurrent(tx, e.ProjectID, e.WorkUnitID, e.DefinitionID, e.DefinitionVersion, head)
		if err != nil {
			return err
		}
		if found && current.Event.Revision >= domain.MaxOutcomeRevisions {
			return ErrRevisionLimit
		}
		if !found && (e.Revision != 1 || e.SupersedesOutcomeID != "") || found && (e.Revision != current.Event.Revision+1 || e.SupersedesOutcomeID != current.Event.OutcomeID) {
			return ErrRevisionConflict
		}
		if err := govPut(tx, "outcomes", []byte(e.OutcomeID), r); err != nil {
			return err
		}
		if err := govPut(tx, "heads", govHeadKey(e.ProjectID, e.WorkUnitID, e.DefinitionID, e.DefinitionVersion), e.OutcomeID); err != nil {
			return err
		}
		if err := govPut(tx, "idempotency", govKey(e.ProjectID, e.IdempotencyKeyHash), idemRecord{e.RequestFingerprint, e.OutcomeID}); err != nil {
			return err
		}
		for _, index := range []struct{ name, scope string }{{"by_project", e.ProjectID}, {"by_work_unit", e.WorkUnitID}, {"by_definition", e.DefinitionID}} {
			k := append(govKey(index.scope), e.OutcomeID...)
			if err := govRawPut(tx, index.name, k, binary.BigEndian.AppendUint64(nil, r.Sequence)); err != nil {
				return err
			}
		}
		head = Summary{Records: r.Sequence, Bytes: r.Offset, LastHash: r.Hash}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return govPut(tx, "meta", []byte("position"), head)
}

// applyRecords is also used during construction, before ready is published.
// All domain rows, secondary indexes and the exact native position commit once.
func (p *GovernanceProjection) applyRecords(ctx context.Context, records []Record) error {
	apply := func(tx any) error { return applyGovernanceRows(ctx, tx, records) }
	if p.cow != nil {
		if len(records) == 0 || len(records) > typedGovernanceBatchRows {
			return ErrOutcomePageLimit
		}
		last := records[len(records)-1]
		cut := typedGovernanceCut(Summary{Records: last.Sequence, Bytes: last.Offset, LastHash: last.Hash})
		prepared, finish, err := p.preparePhysicalGovernanceUpdate(ctx, records)
		if err != nil {
			return err
		}
		_, err = p.cow.Update(prepared, cut, func(tx *authenticatedstate.Transaction) error { return apply(&govRows{ctx: prepared, update: tx}) })
		return errors.Join(err, finish(err))
	}
	return projectionCapacityError(p.db.Update(func(tx *bbolt.Tx) error { return apply(tx) }))
}

func (p *GovernanceProjection) Apply(ctx context.Context, r Record) error {
	if p.retired.Load() {
		return ErrUnavailable
	}
	if p.cow != nil {
		return p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error {
			return p.withPhysicalApplyAuthority(ctx, Summary{Records: r.Sequence, Bytes: r.Offset, LastHash: r.Hash}, func(ctx context.Context) error { return p.applyTypedNative(ctx, r) })
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired.Load() || !p.ready || p.db == nil {
		return ErrUnavailable
	}
	return p.applyRecords(ctx, []Record{r})
}

func (p *GovernanceProjection) read(ctx context.Context, visit func(*bbolt.Tx, Summary) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.retired.Load() {
		return ErrUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.retired.Load() || !p.ready || p.db == nil {
		return ErrUnavailable
	}
	return p.db.View(func(tx *bbolt.Tx) error {
		head, err := govPosition(tx)
		if err != nil {
			return err
		}
		if err := visit(tx, head); err != nil {
			return err
		}
		return ctx.Err()
	})
}

func (p *GovernanceProjection) ReadHead(ctx context.Context) (Summary, error) {
	var result Summary
	err := p.readRows(ctx, func(_ any, head Summary) error { result = head; return nil })
	if err != nil {
		return Summary{}, err
	}
	return result, nil
}

func (p *GovernanceProjection) ReadOutcome(ctx context.Context, id string) (domain.Outcome, bool, error) {
	if len(id) > domain.MaxGovernanceIDLength {
		return domain.Outcome{}, false, ErrOutcomePageLimit
	}
	var result domain.Outcome
	var found bool
	err := p.readRows(ctx, func(tx any, head Summary) error {
		r, ok, err := govRecord(tx, id, head)
		found = ok
		result = recordOutcome(r)
		return err
	})
	if err != nil || !found {
		return domain.Outcome{}, false, err
	}
	return result, true, nil
}

func (p *GovernanceProjection) ReadCurrent(ctx context.Context, project, unit, definition string, version uint64) (domain.Outcome, bool, error) {
	if len(project) > maxPayloadSize || len(unit) > domain.MaxGovernanceIDLength || len(definition) > domain.MaxGovernanceIDLength {
		return domain.Outcome{}, false, ErrOutcomePageLimit
	}
	var result domain.Outcome
	var found bool
	err := p.readRows(ctx, func(tx any, head Summary) error {
		r, ok, err := govCurrent(tx, project, unit, definition, version, head)
		found = ok
		result = recordOutcome(r)
		return err
	})
	if err != nil || !found {
		return domain.Outcome{}, false, err
	}
	return result, true, nil
}

func (p *GovernanceProjection) readIdempotency(ctx context.Context, project, hash string) (idemRecord, bool, error) {
	if len(project) > maxPayloadSize || len(hash) > 128 {
		return idemRecord{}, false, ErrOutcomePageLimit
	}
	var result idemRecord
	var found bool
	err := p.readRows(ctx, func(tx any, head Summary) error {
		v, ok, err := govGet[idemRecord](tx, "idempotency", govKey(project, hash))
		if err != nil || !ok {
			return err
		}
		r, ok, err := govRecord(tx, v.OutcomeID, head)
		if err != nil || !ok || r.Event.ProjectID != project || r.Event.IdempotencyKeyHash != hash || r.Event.RequestFingerprint != v.Fingerprint {
			return errors.Join(ErrCorrupt, err)
		}
		result, found = v, true
		return nil
	})
	if err != nil {
		return idemRecord{}, false, err
	}
	return result, found, nil
}

// Each call examines at most 200 physical index rows and 1MiB of source JSON
// inside one short transaction. Filters/frozen-cut exclusions can yield an
// empty page with a continuation. Cursor progress follows the last examined ID.
func (p *GovernanceProjection) readOutcomePage(ctx context.Context, q OutcomePageQuery, after string, cut Summary) (OutcomePage, string, error) {
	page := OutcomePage{Items: make([]domain.Outcome, 0), Head: cut}
	next := ""
	err := p.readRows(ctx, func(tx any, head Summary) error {
		if cut.Records > head.Records || cut.Bytes > head.Bytes || cut.Records == head.Records && cut != head {
			return ErrCorrupt
		}
		bucket, scope := "outcomes", ""
		if q.ProjectID != "" {
			bucket, scope = "by_project", q.ProjectID
		}
		if q.DefinitionID != "" {
			bucket, scope = "by_definition", q.DefinitionID
		}
		if q.WorkUnitID != "" {
			bucket, scope = "by_work_unit", q.WorkUnitID
		}
		var prefix []byte
		if scope != "" {
			prefix = govKey(scope)
		}
		sourceBytes, examined, last := 0, 0, after
		return govWalkRows(tx, bucket, prefix, after, func(k, v []byte) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if examined == OutcomePageMaxRows || len(page.Items) == q.Rows {
				next = last
				return true, nil
			}
			id := string(k[len(prefix):])
			if !domain.ValidOutcomeID(id) {
				return false, ErrCorrupt
			}
			raw := v
			if bucket != "outcomes" {
				if len(v) != 8 {
					return false, ErrCorrupt
				}
				var found bool
				var err error
				raw, found, err = govRawGet(tx, "outcomes", []byte(id))
				if err != nil || !found {
					return false, errors.Join(ErrCorrupt, err)
				}
			}
			if len(raw) == 0 || len(raw) > governanceProjectionMaxRow {
				return false, ErrCorrupt
			}
			if sourceBytes+len(raw) > OutcomePageMaxBytes {
				next = last
				return true, nil
			}
			var r Record
			if err := json.Unmarshal(raw, &r); err != nil {
				return false, errors.Join(ErrCorrupt, err)
			}
			if r.Event.OutcomeID != id || r.Event.Validate() != nil || r.Sequence == 0 || r.Sequence > head.Records || r.Offset <= 0 || r.Offset > head.Bytes || r.Hash == ([32]byte{}) || r.Event.Revision > domain.MaxOutcomeRevisions {
				return false, ErrCorrupt
			}
			if bucket != "outcomes" {
				indexedScope := r.Event.ProjectID
				if bucket == "by_work_unit" {
					indexedScope = r.Event.WorkUnitID
				}
				if bucket == "by_definition" {
					indexedScope = r.Event.DefinitionID
				}
				if binary.BigEndian.Uint64(v) != r.Sequence || !bytes.Equal(prefix, govKey(indexedScope)) {
					return false, ErrCorrupt
				}
			}
			if r.Sequence <= cut.Records && (q.ProjectID == "" || r.Event.ProjectID == q.ProjectID) && (q.WorkUnitID == "" || r.Event.WorkUnitID == q.WorkUnitID) && (q.DefinitionID == "" || r.Event.DefinitionID == q.DefinitionID) {
				row := recordOutcome(r)
				encoded, err := json.Marshal(row)
				if err != nil {
					return false, err
				}
				if len(encoded) > q.Bytes-page.Bytes {
					if len(page.Items) == 0 {
						return false, ErrOutcomePageLimit
					}
					next = last
					return true, nil
				}
				page.Items = append(page.Items, row)
				page.Bytes += len(encoded)
			}
			sourceBytes += len(raw)
			examined++
			last = id
			return false, nil
		})
	})
	if err != nil {
		return OutcomePage{}, "", err
	}
	return page, next, nil
}

// governanceContextReader checks cancellation around every actual source read.
// It never closes an FD to pretend a blocked operating-system read has joined.
type governanceContextReader struct {
	ctx    context.Context
	source io.ReadSeeker
}

func (r governanceContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(b)
	// Cancellation wins over EOF/partial-tail classification in scanFrom.
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
func (r governanceContextReader) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Seek(offset, whence)
	return n, errors.Join(err, r.ctx.Err())
}
