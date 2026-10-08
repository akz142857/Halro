package usage

// This fresh node-derived projection is rebuilt from authenticated, role-allowed
// Ledger records. Authenticated restore materializes a selected content root
// into another exclusive file; ordinary saved DBs are never trusted. The caller
// owns native/role authentication, publication and disk admission.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
)

var (
	ErrDiskUsageClosed    = errors.New("disk Usage projection is retired")
	ErrDiskUsageCorrupt   = errors.New("disk Usage projection row or index is invalid")
	ErrDiskUsageLimit     = errors.New("disk Usage page or value exceeds its limit")
	ErrDiskUsageBusy      = errors.New("disk Usage reader slots are occupied")
	ErrDiskUsageDiskLimit = errors.New("disk Usage projection disk ceiling exceeded")
)

const diskUsageMaxValue = 3 << 20
const diskUsageVersion = 6

var diskUsageBuckets = []string{"meta", "active", "started", "attempts", "attempt_index", "summaries", "summary_index", "summary_request", "request_versions", "request_version_head", "request_quality", "request_latency_rank", "hourly", "hourly_order", "dashboard_period", "dashboard_group", "dashboard_top", "rollup", "rollup_counts", "rollup_range", "rollup_versions", "rollup_scope"}

type diskUsageState struct {
	Version       int
	Watermark     ledger.Watermark
	Totals        Bucket
	Metrics       Metrics
	LastRecord    [32]byte
	ReplayHash    [32]byte
	ReplayEpoch   uint8
	Window        DiskWindowState
	WindowCleanup diskWindowCleanup
}

type diskStarted struct {
	AttemptID string
	At        time.Time
}

// DiskProjection retains only its DB, reader and generation identity. Each Apply
// hydrates a temporary Aggregate with one request, one start, one hour and constant
// counters. Errors discard that reducer and roll back all derived rows together.
type DiskProjection struct {
	scratchWork       resourcebudget.Permit
	physical          *typedPhysicalUsageOwner
	physicalPublisher *authenticatedstate.SealCandidatePublisher
	physicalJobMu     sync.Mutex
	physicalJobs      sync.WaitGroup
	physicalCtx       context.Context
	physicalCancel    context.CancelFunc
	typedSealDebt     resourcebudget.Permit
	mu                sync.RWMutex
	summaryMu         sync.RWMutex
	retired           atomic.Bool
	retirementCause   atomic.Pointer[usageRetirementCause]
	db                *bbolt.DB
	engine            *usageEngine
	file              *ownedfile.Owner
	image             *authenticatedstate.MaterializedImage
	liveRAM           resourcebudget.Permit
	catchUpMu         sync.Mutex
	catchUpCancel     context.CancelFunc
	catchUpDone       chan struct{}
	catchUpActive     atomic.Bool
	closeErr          error
	typedClosed       bool
	typedWork         resourcebudget.Permit
	typedDebtCredit   resourcebudget.Permit
	reader            FinalizationReader
	generation        [16]byte
	cursorKey         [32]byte
	readers           chan struct{}
	summaries         chan struct{}
}

func CreateDiskProjection(path string, reader FinalizationReader) (*DiskProjection, error) {
	return CreateDiskProjectionWithLimit(path, reader, 0)
}

// CreateDiskProjectionWithLimit bounds the bbolt data file size. A zero limit
// preserves the legacy unlimited behavior; it does not reserve filesystem
// blocks or account for mmap/page-cache use.
func CreateDiskProjectionWithLimit(path string, reader FinalizationReader, maxDiskBytes int64) (*DiskProjection, error) {
	if reader == nil {
		return nil, errors.New("disk Usage requires selected Ledger reader")
	}
	if maxDiskBytes < 0 || uint64(maxDiskBytes) > uint64(^uint(0)>>1) {
		return nil, ErrDiskUsageLimit
	}
	p := &DiskProjection{reader: reader, readers: make(chan struct{}, 2), summaries: make(chan struct{}, 1)}
	db, receipt, err := ownedfile.OpenExclusiveBolt(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType, MaxSize: int(maxDiskBytes)})
	p.file = receipt
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			return nil, normalizeDiskUsageDiskError(err)
		}
		return nil, errors.Join(normalizeDiskUsageDiskError(err), p.file.Retire())
	}
	p.db = db
	p.engine = wrapUsageDatabase(db)
	err = func() error {
		if _, err := rand.Read(p.generation[:]); err != nil {
			return err
		}
		if _, err := rand.Read(p.cursorKey[:]); err != nil {
			return err
		}
		return wrapUsageDatabase(db).Update(func(tx *usageTx) error {
			for _, name := range diskUsageBuckets {
				if _, err := tx.CreateBucket([]byte(name)); err != nil {
					return err
				}
			}
			if err := diskUsagePut(tx, "meta", []byte("rollup_rows"), uint64(0)); err != nil {
				return err
			}
			return diskUsagePut(tx, "meta", []byte("state"), diskUsageState{Version: diskUsageVersion})
		})
	}()
	if err != nil {
		return nil, errors.Join(normalizeDiskUsageDiskError(err), p.Discard())
	}
	return p, nil
}

func normalizeDiskUsageDiskError(err error) error {
	if errors.Is(err, bberrors.ErrMaxSizeReached) {
		return errors.Join(ErrDiskUsageDiskLimit, err)
	}
	return err
}

// Close withdraws admission before joining transactions and reader callbacks.
// It closes this DB, releases reader references, and leaves file removal/fees to
// its owner. Never re-enter Close from a projection-owned reader callback.
func (p *DiskProjection) Close() error {
	if p == nil {
		return nil
	}
	p.retired.Store(true)
	p.physicalJobMu.Lock()
	if p.physicalCancel != nil {
		p.physicalCancel()
	}
	p.physicalJobMu.Unlock()
	p.catchUpMu.Lock()
	done := p.catchUpDone
	if p.catchUpCancel != nil {
		p.catchUpCancel()
	}
	p.catchUpMu.Unlock()
	if done != nil {
		<-done
	}
	p.physicalJobs.Wait()
	p.summaryMu.Lock()
	defer p.summaryMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reader = nil
	if p.engine != nil && p.engine.cow != nil {
		if !p.typedClosed {
			p.typedClosed = true
			physicalErr := p.closePhysicalUsageLocked(context.Background())
			storeErr := p.engine.cow.Close()
			if p.physical != nil && storeErr != nil {
				// The real COW owner's final descriptor retirement is later than
				// common-cut qualification. Its failure withdraws the native Close
				// marker too; otherwise that owner could retire the shared tail.
				p.physical.mu.Lock()
				p.physical.closedQualified = false
				p.physical.failure = errors.Join(p.physical.failure, ownedfile.ErrRetirement, storeErr)
				storeErr = p.physical.failure
				p.physical.mu.Unlock()
			}
			p.closeErr = errors.Join(p.closeErr, physicalErr, storeErr)
		}
		if !errors.Is(p.closeErr, ownedfile.ErrRetirement) && p.liveRAM != (resourcebudget.Permit{}) {
			p.closeErr = errors.Join(p.closeErr, p.liveRAM.Release())
			p.liveRAM = resourcebudget.Permit{}
		}
		if !errors.Is(p.closeErr, ownedfile.ErrRetirement) {
			if p.physical != nil && p.physical.work != nil {
				workErr := p.physical.work.Close()
				p.closeErr = errors.Join(p.closeErr, workErr)
				if workErr == nil {
					activeErr := p.physical.work.Active.Release()
					p.closeErr = errors.Join(p.closeErr, activeErr)
					if activeErr == nil {
						p.physical.work = nil
					}
				}
			}
			p.engine.owner = nil
			p.engine.native = nil
			p.engine.lease = nil
		}
		return p.closeErr
	}
	if p.db == nil {
		if p.engine != nil && p.engine.scratch != nil {
			p.closeErr = errors.Join(p.closeErr, p.engine.scratch.Close())
			if p.closeErr == nil && p.scratchWork != (resourcebudget.Permit{}) {
				p.engine.hotScratch = nil
				p.closeErr = p.scratchWork.Release()
				if p.closeErr == nil {
					p.scratchWork = resourcebudget.Permit{}
				}
			}
		}
		return p.closeErr
	}
	p.closeErr = p.db.Close()
	if p.closeErr != nil {
		p.closeErr = errors.Join(ownedfile.ErrRetirement, p.closeErr)
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

// Discard retires and physically joins this exclusive derivative, closes it,
// and removes only its captured inode. It establishes no filesystem refund.
func (p *DiskProjection) Discard() error {
	if p == nil {
		return nil
	}
	err := p.Close()
	if err != nil {
		return err
	}
	if p.engine != nil && p.engine.cow != nil {
		return p.engine.cow.Discard()
	}
	if p.engine != nil && p.engine.scratch != nil {
		return p.engine.scratch.Discard()
	}
	return errors.Join(p.file.Retire(), p.image.Remove())
}

func diskUsageGet[T any](input any, bucket string, key []byte) (T, bool, error) {
	tx := usageTransaction(input)
	var result T
	b := tx.Bucket([]byte(bucket))
	if b == nil {
		return result, false, ErrDiskUsageCorrupt
	}
	raw := b.Get(key)
	if raw == nil {
		return result, false, nil
	}
	if len(raw) > diskUsageMaxValue || json.Unmarshal(raw, &result) != nil {
		return result, false, ErrDiskUsageCorrupt
	}
	return result, true, nil
}
func diskUsagePut(input any, bucket string, key []byte, value any) error {
	tx := usageTransaction(input)
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > diskUsageMaxValue {
		return ErrDiskUsageLimit
	}
	b := tx.Bucket([]byte(bucket))
	if b == nil {
		return ErrDiskUsageCorrupt
	}
	return b.Put(key, raw)
}
func diskUsageID(id string) []byte { sum := sha256.Sum256([]byte(id)); return sum[:] }
func diskUsageSequence(seq uint64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], seq)
	return key[:]
}

// Prefetch the selected reader before entering the Usage write transaction.
// The reducer never performs external IO while holding that transaction.
type diskUsageEvidence struct {
	first      ledger.Watermark
	found      bool
	final      ledger.Record
	finalFound bool
	legacy     bool
}

func (e diskUsageEvidence) ReadEventPosition(context.Context, string) (ledger.Watermark, bool, error) {
	return e.first, e.found, nil
}
func (e diskUsageEvidence) ReadFinalization(context.Context, string) (ledger.Record, bool, error) {
	return e.final, e.finalFound, nil
}
func (e diskUsageEvidence) ReadLegacyRequest(context.Context, string) (bool, error) {
	return e.legacy, nil
}

// The reducer needs terminal order and attribution, not the terminal payload.
// A short replay record can refer to a near-1MiB future finalization. Retaining
// those independently decoded records for an entire page would exceed the
// page's input/workspace envelope. Validate before replacing any field, then
// borrow the matching input strings for the page's actual lifetime.
func (e *diskUsageEvidence) compactFinalization(record ledger.Record) error {
	if !e.finalFound {
		e.final = ledger.Record{}
		return nil
	}
	f, event := e.final.Event, record.Event
	if e.final.Sequence == 0 || f.Kind != ledger.EventRequestFinalized || f.RequestID != event.RequestID ||
		e.legacy && (f.WorkUnitID != "" || f.RunID != "" || event.WorkUnitID != "" || event.RunID != "") ||
		!e.legacy && (f.ProjectID != event.ProjectID || f.PeriodID != event.PeriodID || f.PeriodTimezoneVersion != event.PeriodTimezoneVersion || f.WorkUnitID != event.WorkUnitID || f.RunID != event.RunID) {
		return errors.New("usage request finalization has inconsistent attribution")
	}
	terminal := ledger.Event{Kind: ledger.EventRequestFinalized, RequestID: event.RequestID}
	if !e.legacy {
		terminal.ProjectID, terminal.PeriodID = event.ProjectID, event.PeriodID
		terminal.PeriodTimezoneVersion = event.PeriodTimezoneVersion
		terminal.WorkUnitID, terminal.RunID = event.WorkUnitID, event.RunID
	}
	e.final = ledger.Record{Sequence: e.final.Sequence, Event: terminal}
	return nil
}

func (p *DiskProjection) Apply(record ledger.Record) error {
	return p.ApplyContext(context.Background(), record)
}
func (p *DiskProjection) ApplyContext(ctx context.Context, record ledger.Record) error {
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if p.retired.Load() {
		return p.unavailableError("projection read")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil {
		return p.unavailableError("projection read")
	}
	if p.engine.cow != nil {
		return p.applyTypedRecordLocked(ctx, record)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	if record.Generation == 0 || record.Sequence == 0 || record.Offset <= 0 {
		return ErrDiskUsageCorrupt
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > diskUsageMaxValue {
		return ErrDiskUsageLimit
	}
	fingerprint := sha256.Sum256(raw)
	e := diskUsageEvidence{}
	e.first, e.found, err = p.reader.ReadEventPosition(ctx, record.Event.EventID)
	if err != nil {
		return err
	}
	if record.Event.Kind < ledger.EventWorkUnitCreated {
		e.final, e.finalFound, err = p.reader.ReadFinalization(ctx, record.Event.RequestID)
		if err != nil {
			return err
		}
		if e.finalFound {
			if source, ok := p.reader.(LegacyRequestReader); ok {
				e.legacy, err = source.ReadLegacyRequest(ctx, record.Event.RequestID)
				if err != nil {
					return err
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.found || e.first.Sequence == 0 || e.first.Sequence > record.Sequence || e.first.Generation == 0 || e.first.Offset <= 0 || (e.first.Sequence == record.Sequence && (e.first.Generation != record.Generation || e.first.Offset != record.Offset)) {
		return errors.New("disk Usage event has no consistent selected-owner position")
	}
	err = p.engine.scoped(ctx).Update(func(tx *usageTx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || state.Version != diskUsageVersion {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		position := ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}
		if record.Sequence == state.Watermark.Sequence && position == state.Watermark && fingerprint == state.LastRecord {
			return nil
		}
		if !position.After(state.Watermark) || record.Sequence <= state.Watermark.Sequence {
			return ErrDiskUsageCorrupt
		}
		a, err := NewAggregateWithFinalizationReader(e)
		if err != nil {
			return err
		}
		a.watermark, a.totals, a.metrics = state.Watermark, state.Totals, state.Metrics
		event := record.Event
		active, hadActive, err := diskUsageGet[RequestSummary](tx, "active", diskUsageID(event.RequestID))
		if err != nil {
			return err
		}
		if hadActive {
			if active.RequestID != event.RequestID || active.Sequence != 0 {
				return ErrDiskUsageCorrupt
			}
			a.requests[event.RequestID] = &requestAccumulator{summary: active}
		}
		start, hasStart, err := diskUsageGet[diskStarted](tx, "started", diskUsageID(event.AttemptID))
		if err != nil {
			return err
		}
		if hasStart {
			if start.AttemptID != event.AttemptID {
				return ErrDiskUsageCorrupt
			}
			a.started[event.AttemptID] = start.At
		}
		hour := event.OccurredAt.UTC().Truncate(time.Hour)
		bucket, hasHour, err := diskUsageGet[Bucket](tx, "hourly", diskUsageSequence(uint64(hour.Unix())))
		if err != nil {
			return err
		}
		if hasHour {
			if !bucket.Hour.Equal(hour) {
				return ErrDiskUsageCorrupt
			}
			a.hourly[hour.Unix()] = bucket
		}
		if err := a.ApplyContext(ctx, record); err != nil {
			return err
		}
		nowActive := a.requests[event.RequestID]
		if hadActive != (nowActive != nil) {
			if nowActive != nil {
				if state.Metrics.ActiveRequests == math.MaxUint64 {
					return ErrDiskUsageCorrupt
				}
				state.Metrics.ActiveRequests++
			} else {
				if state.Metrics.ActiveRequests == 0 {
					return ErrDiskUsageCorrupt
				}
				state.Metrics.ActiveRequests--
			}
		}
		if nowActive != nil {
			if err := diskUsagePut(tx, "active", diskUsageID(event.RequestID), nowActive.summary); err != nil {
				return err
			}
		} else {
			if err := tx.Bucket([]byte("active")).Delete(diskUsageID(event.RequestID)); err != nil {
				return err
			}
		}
		if at, ok := a.started[event.AttemptID]; ok {
			if err := diskUsagePut(tx, "started", diskUsageID(event.AttemptID), diskStarted{event.AttemptID, at}); err != nil {
				return err
			}
		} else {
			if err := tx.Bucket([]byte("started")).Delete(diskUsageID(event.AttemptID)); err != nil {
				return err
			}
		}
		for _, attempt := range a.attempts {
			if state.Window.Attempts == math.MaxUint64 {
				return ErrDiskUsageCorrupt
			}
			state.Window.Attempts++
			if err := diskUsageAddDashboardAttempt(tx, attempt); err != nil {
				return err
			}
			if err := diskUsagePut(tx, "attempts", diskUsageSequence(attempt.Sequence), attempt); err != nil {
				return err
			}
			for _, scope := range diskAttemptScopes(attempt) {
				if err := tx.Bucket([]byte("attempt_index")).Put(diskUsageIndex(scope.kind, scope.value, attempt.Sequence), []byte{}); err != nil {
					return err
				}
			}
		}
		for _, summary := range a.summaries {
			if state.Window.Summaries == math.MaxUint64 {
				return ErrDiskUsageCorrupt
			}
			state.Window.Summaries++
			if err := diskUsageAddRequestQuality(tx, summary); err != nil {
				return err
			}
			if err := diskUsagePut(tx, "summaries", diskUsageSequence(summary.Sequence), summary); err != nil {
				return err
			}
			if err := tx.Bucket([]byte("summary_request")).Put(diskUsageID(summary.RequestID), diskUsageSequence(summary.Sequence)); err != nil {
				return err
			}
			for _, scope := range diskSummaryScopes(summary) {
				if err := tx.Bucket([]byte("summary_index")).Put(diskUsageIndex(scope.kind, scope.value, summary.Sequence), []byte{}); err != nil {
					return err
				}
			}
		}
		// Preserve active/terminal summary observations at their actual Apply
		// sequence. Unchanged reservation/start frames and historical duplicates
		// add no version. Late settlement has no active/new terminal summary:
		// never persist its scratch accumulator over the finalized observation.
		if e.first.Sequence == record.Sequence {
			if nowActive != nil && (!hadActive || nowActive.summary != active) {
				if err := diskUsagePutRequestVersion(tx, record.Sequence, nowActive.summary); err != nil {
					return err
				}
			}
			for _, summary := range a.summaries {
				if err := diskUsagePutRequestVersion(tx, record.Sequence, summary); err != nil {
					return err
				}
			}
		}
		if value, ok := a.hourly[hour.Unix()]; ok {
			// The legacy reducer stores base totals; Dashboard adds estimated token
			// subsets separately. Persist those subsets without altering ReadStats.
			for _, attempt := range a.attempts {
				if attempt.TokensEstimated {
					if err := addInt64(&value.EstimatedInputTokens, attempt.ProviderInputTokens); err != nil {
						return err
					}
					if err := addInt64(&value.EstimatedOutputTokens, attempt.ProviderOutputTokens); err != nil {
						return err
					}
				}
			}
			if err := diskUsagePut(tx, "hourly", diskUsageSequence(uint64(hour.Unix())), value); err != nil {
				return err
			}
			if err := tx.Bucket([]byte("hourly_order")).Put(diskDashboardHourKey(hour.Unix()), diskUsageSequence(uint64(hour.Unix()))); err != nil {
				return err
			}
		}
		for key, delta := range a.rollupDelta {
			if err := diskUsageMergeRollup(tx, key, *delta, record.Sequence); err != nil {
				return err
			}
		}
		if !diskUsageMetricsMonotonic(state.Metrics, a.metrics) {
			return errors.New("usage metrics counter overflow")
		}
		activeCount := state.Metrics.ActiveRequests
		state.Watermark, state.Totals, state.Metrics, state.LastRecord = a.watermark, a.totals, a.metrics, fingerprint
		state.ReplayHash, state.ReplayEpoch = record.Hash, record.Epoch
		state.Metrics.ActiveRequests = activeCount
		if err := diskUsagePut(tx, "meta", []byte("state"), state); err != nil {
			return err
		}
		return ctx.Err()
	})
	return normalizeDiskUsageDiskError(err)
}

type diskRollupRow struct {
	Key domain.RollupKey
	Row domain.DailyRollup
}
type diskRollupCount struct {
	Group string
	Count uint64
}

func diskUsageReadRollup(input any, key domain.RollupKey) (domain.DailyRollup, bool, error) {
	tx := usageTransaction(input)
	stored, found, err := diskUsageGet[diskRollupRow](tx, "rollup", diskUsageID(key.Encode()))
	if err != nil {
		return domain.DailyRollup{}, false, err
	}
	if found && (stored.Key != key || stored.Row.PeriodID != key.PeriodID || stored.Row.TimezoneVersion != key.TimezoneVersion || stored.Row.Version != domain.RollupVersion) {
		return domain.DailyRollup{}, false, ErrDiskUsageCorrupt
	}
	return stored.Row, found, nil
}
func diskUsageMergeRollup(input any, key domain.RollupKey, delta domain.DailyRollup, sequence uint64) error {
	tx := usageTransaction(input)
	if err := key.Validate(); err != nil {
		return err
	}
	if key.PeriodID != delta.PeriodID || key.TimezoneVersion != delta.TimezoneVersion {
		return ErrDiskUsageCorrupt
	}
	if sequence == 0 || delta.FirstSequence != sequence {
		return ErrDiskUsageCorrupt
	}
	if key.Dimension != domain.RollupDimensionTotal {
		group := domain.RollupDimensionPrefix(key.PeriodID, key.TimezoneVersion, key.Dimension)
		count, hasCount, err := diskUsageGet[diskRollupCount](tx, "rollup_counts", diskUsageID(group))
		if err != nil {
			return err
		}
		scope, hasScope, err := diskUsageGet[diskRollupScope](tx, "rollup_scope", diskUsageID(group))
		if err != nil {
			return err
		}
		if hasCount != hasScope || (hasCount && (count.Group != group || count.Count > domain.MaxRollupKeysPerDimension || scope.Group != group || scope.FirstSequence == 0 || scope.FirstSequence > sequence)) {
			return ErrDiskUsageCorrupt
		}
		_, found, err := diskUsageReadRollup(tx, key)
		if err != nil {
			return err
		}
		indexed := tx.Bucket([]byte("rollup_range")).Get(diskRollupRangeKey(key)) != nil
		if found != indexed || (found && !hasScope) {
			return ErrDiskUsageCorrupt
		}
		if !hasScope {
			count = diskRollupCount{Group: group}
			if err := diskUsagePut(tx, "rollup_scope", diskUsageID(group), diskRollupScope{Group: group, FirstSequence: sequence}); err != nil {
				return err
			}
			if err := diskUsagePut(tx, "rollup_counts", diskUsageID(group), count); err != nil {
				return err
			}
		}
		if key.DimensionKey != domain.RollupOtherKey && !found {
			if count.Count == domain.MaxRollupKeysPerDimension {
				key.DimensionKey = domain.RollupOtherKey
			} else {
				count.Count++
				if err := diskUsagePut(tx, "rollup_counts", diskUsageID(group), count); err != nil {
					return err
				}
			}
		}
	}
	row, found, err := diskUsageReadRollup(tx, key)
	if err != nil {
		return err
	}
	previous := row
	if !found {
		count, present, err := diskUsageGet[uint64](tx, "meta", []byte("rollup_rows"))
		if err != nil || !present || count == ^uint64(0) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		if err := diskUsagePut(tx, "meta", []byte("rollup_rows"), count+1); err != nil {
			return err
		}
	}
	if err := row.Add(delta); err != nil {
		return err
	}
	if err := diskUsagePutRollupVersion(tx, key, previous, found, row, sequence); err != nil {
		return err
	}
	return diskUsagePut(tx, "rollup", diskUsageID(key.Encode()), diskRollupRow{key, row})
}

// Legacy counters include unchecked ++ sites. The disk transaction refuses
// wraparound before publishing even when the temporary legacy reducer wrapped.
func diskUsageMetricsMonotonic(before, after Metrics) bool {
	counters := func(m Metrics) []uint64 {
		return []uint64{m.RequestsSuccess, m.RequestsError, m.AttemptsSuccess, m.AttemptsError, m.InputTokens, m.OutputTokens, m.CostMicrosUSD, m.AttemptLatencyMillis, m.RequestLatencyMillis, m.Fallbacks, m.UnknownAttempts}
	}
	a, b := counters(before), counters(after)
	for i := range a {
		if b[i] < a[i] {
			return false
		}
	}
	for i := range before.AttemptLatencyBuckets {
		if after.AttemptLatencyBuckets[i] < before.AttemptLatencyBuckets[i] || after.RequestLatencyBuckets[i] < before.RequestLatencyBuckets[i] {
			return false
		}
	}
	return true
}

// Observations describe one current short View, never a historical cursor cut.
type DiskUsageStats struct {
	// LogicalBytes is the committed DB page extent, not RSS or file allocation.
	LogicalBytes int64
	Watermark    ledger.Watermark
	Totals       Bucket
	Metrics      Metrics
	Window       DiskWindowState
}

func (p *DiskProjection) ReadStats(ctx context.Context) (DiskUsageStats, error) {
	var result DiskUsageStats
	err := p.viewRows(ctx, func(tx *usageTx) error {
		s, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || s.Version != diskUsageVersion {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		result = DiskUsageStats{LogicalBytes: tx.Size(), Watermark: s.Watermark, Totals: s.Totals, Metrics: s.Metrics, Window: s.Window}
		return nil
	})
	if err != nil {
		return DiskUsageStats{}, err
	}
	return result, nil
}

// ReadReplayAnchor returns the chain cut committed atomically with this fresh
// reducer. The caller must have authenticated every input record. This is not
// authority to reopen saved DB bytes or to select a role-permitted prefix.
// Keeping the cut in the same transaction permits retry after a commit whose
// caller observed cancellation, without a separate in-memory progress cache.
func (p *DiskProjection) ReadReplayAnchor(ctx context.Context) (ledger.ReplayAnchor, error) {
	var result ledger.ReplayAnchor
	err := p.viewRows(ctx, func(tx *usageTx) error {
		s, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || s.Version != diskUsageVersion {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		result = ledger.ReplayAnchor{Head: s.Watermark, Hash: s.ReplayHash, Epoch: s.ReplayEpoch}
		return nil
	})
	if err != nil {
		return ledger.ReplayAnchor{}, err
	}
	return result, nil
}
func (p *DiskProjection) ReadRequest(ctx context.Context, id string) (RequestSummary, bool, error) {
	var result RequestSummary
	var found bool
	err := p.viewRows(ctx, func(tx *usageTx) error {
		var err error
		result, found, err = diskUsageGet[RequestSummary](tx, "active", diskUsageID(id))
		if err != nil {
			return err
		}
		if !found {
			index := tx.Bucket([]byte("summary_request"))
			if index == nil {
				return ErrDiskUsageCorrupt
			}
			key := index.Get(diskUsageID(id))
			if key != nil {
				if len(key) != 8 {
					return ErrDiskUsageCorrupt
				}
				result, found, err = diskUsageGet[RequestSummary](tx, "summaries", key)
				if err != nil || !found {
					return errors.Join(err, ErrDiskUsageCorrupt)
				}
			}
		}
		if found && result.RequestID != id {
			return ErrDiskUsageCorrupt
		}
		return nil
	})
	if err != nil {
		return RequestSummary{}, false, err
	}
	return result, found, nil
}
func (p *DiskProjection) ReadRollup(ctx context.Context, key domain.RollupKey) (domain.DailyRollup, bool, error) {
	if err := key.Validate(); err != nil {
		return domain.DailyRollup{}, false, err
	}
	var result domain.DailyRollup
	var found bool
	err := p.viewRows(ctx, func(tx *usageTx) error {
		var err error
		result, found, err = diskUsageReadRollup(tx, key)
		if err == nil && found && (result.PeriodID != key.PeriodID || result.TimezoneVersion != key.TimezoneVersion || result.Version != domain.RollupVersion) {
			return ErrDiskUsageCorrupt
		}
		return err
	})
	if err != nil {
		return domain.DailyRollup{}, false, err
	}
	return result, found, nil
}

func (p *DiskProjection) viewRows(ctx context.Context, fn func(*usageTx) error) error {
	return p.readRows(ctx, func(db *usageEngine) error { return db.View(fn) })
}
func (p *DiskProjection) readRows(ctx context.Context, fn func(*usageEngine) error) error {
	if p.retired.Load() {
		return p.unavailableError("projection read")
	}
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.readers == nil {
		return p.unavailableError("projection read")
	}
	// No queue. Slots cover lock waiting, internal query preparation and the
	// complete View/copy; caller response encoding happens after return.
	select {
	case p.readers <- struct{}{}:
	default:
		return ErrDiskUsageBusy
	}
	p.mu.RLock()
	defer func() { <-p.readers; p.mu.RUnlock() }()
	if p.retired.Load() || p.engine == nil {
		return p.unavailableError("projection read")
	}
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(p.engine.scoped(ctx)); err != nil {
		return err
	}
	return ctx.Err()
}

func (p *DiskProjection) view(ctx context.Context, fn func(*bbolt.Tx) error) error {
	return p.read(ctx, func(db *bbolt.DB) error { return db.View(fn) })
}
func (p *DiskProjection) read(ctx context.Context, fn func(*bbolt.DB) error) error {
	if p.retired.Load() {
		return p.unavailableError("projection read")
	}
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.readers == nil {
		return p.unavailableError("projection read")
	}
	// No queue. Slots cover lock waiting, internal query preparation and the
	// complete View/copy; caller response encoding happens after return.
	select {
	case p.readers <- struct{}{}:
	default:
		return ErrDiskUsageBusy
	}
	p.mu.RLock()
	defer func() { <-p.readers; p.mu.RUnlock() }()
	if p.retired.Load() || p.db == nil {
		return p.unavailableError("projection read")
	}
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(p.db); err != nil {
		return err
	}
	return ctx.Err()
}
