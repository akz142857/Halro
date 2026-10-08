package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

const (
	diskReplayBatchRows  = 512
	diskReplayBatchBytes = 8 << 20
)

type diskUsagePositionReader interface {
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
}

type diskUsageReplayRecord struct {
	record      ledger.Record
	evidence    diskUsageEvidence
	fingerprint [32]byte
}

// ApplyReplayBatchContext atomically applies one bounded page from an
// authenticated replay. The selected Ledger owner must remain at one fixed
// position while the page's evidence is read. Live durable appends continue to
// use ApplyContext and its per-record commit barrier. Cold pages are limited to
// 512 rows and 8 MiB of encoded records.
func (p *DiskProjection) ApplyReplayBatchContext(ctx context.Context, records []ledger.Record) error {
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if p.retired.Load() {
		return ErrDiskUsageClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.catchUpActive.Load() {
		return ErrDiskUsageBusy
	}
	if p.engine != nil && p.engine.cow != nil {
		return p.applyTypedBatchLocked(ctx, records)
	}
	return p.applyReplayBatchLocked(ctx, records)
}

func (p *DiskProjection) applyReplayBatchLocked(ctx context.Context, records []ledger.Record) error {
	return p.applyReplayBatchWithFillLocked(ctx, records, false)
}

// A private offline scratch has no published reader/cursor. Its rollup history
// appends monotonically within each rollup prefix, so dense splits avoid storing
// every cumulative version in a half-empty leaf. Live/public replay keeps the
// ordinary split policy and identical reducer rows.
func (p *DiskProjection) applyReplayBatchWithFillLocked(ctx context.Context, records []ledger.Record, packed bool) error {
	if p.retired.Load() || p.db == nil && (p.engine == nil || p.engine.scratch == nil) {
		return ErrDiskUsageClosed
	}
	if len(records) == 0 || len(records) > diskReplayBatchRows {
		return ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	positionReader, ok := p.reader.(diskUsagePositionReader)
	if !ok {
		return errors.New("disk Usage replay batch requires a position-aware selected Ledger owner")
	}
	before, _, err := positionReader.ReadPosition(ctx)
	if err != nil {
		return err
	}
	prepared := make([]diskUsageReplayRecord, 0, len(records))
	totalBytes := 0
	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := record.Event.Validate(); err != nil {
			return err
		}
		if record.Generation == 0 || record.Sequence == 0 || record.Offset <= 0 || (i > 0 && record.Sequence <= records[i-1].Sequence) {
			return ErrDiskUsageCorrupt
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > diskUsageMaxValue || len(raw) > diskReplayBatchBytes-totalBytes {
			return ErrDiskUsageLimit
		}
		totalBytes += len(raw)
		item := diskUsageReplayRecord{record: record, fingerprint: sha256.Sum256(raw)}
		e := &item.evidence
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
			if err := e.compactFinalization(record); err != nil {
				return err
			}
		}
		if !e.found || e.first.Sequence == 0 || e.first.Sequence > record.Sequence || e.first.Generation == 0 || e.first.Offset <= 0 || (e.first.Sequence == record.Sequence && (e.first.Generation != record.Generation || e.first.Offset != record.Offset)) {
			return errors.New("disk Usage event has no consistent selected-owner position")
		}
		prepared = append(prepared, item)
	}
	after, _, err := positionReader.ReadPosition(ctx)
	if err != nil {
		return err
	}
	if after != before || records[len(records)-1].Sequence > after.Sequence {
		return errors.New("disk Usage replay batch selected Ledger cut changed during evidence read")
	}
	err = p.engine.scoped(ctx).Update(func(tx *usageTx) error {
		if packed && tx.scratch != nil {
			tx.rankScratch = new(rankScratchBatch)
			tx.hotScratch = p.engine.hotScratch
		}
		if packed && tx.scratch == nil {
			if tx.bolt == nil {
				return ErrDiskUsageCorrupt
			}
			history := tx.bolt.Bucket([]byte("rollup_versions"))
			if history == nil {
				return ErrDiskUsageCorrupt
			}
			history.FillPercent = 1
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || state.Version != diskUsageVersion {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		for _, item := range prepared {
			if tx.rankScratch != nil {
				if err := tx.rankScratch.beforeRecord(tx); err != nil {
					return err
				}
			}
			position := ledger.Watermark{Generation: item.record.Generation, Sequence: item.record.Sequence, Offset: item.record.Offset}
			if item.record.Sequence < state.Watermark.Sequence {
				continue // Authenticated replay retry: this prefix already committed.
			}
			if item.record.Sequence == state.Watermark.Sequence {
				if position == state.Watermark && item.fingerprint == state.LastRecord {
					continue
				}
				return ErrDiskUsageCorrupt
			}
			if !position.After(state.Watermark) {
				return ErrDiskUsageCorrupt
			}
			if err := applyDiskUsageReplayRecord(ctx, tx, item, &state); err != nil {
				return err
			}
		}
		if tx.rankScratch != nil {
			if err := tx.rankScratch.flush(tx); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	return normalizeDiskUsageDiskError(err)
}

func applyDiskUsageReplayRecord(ctx context.Context, input any, item diskUsageReplayRecord, state *diskUsageState) error {
	tx := usageTransaction(input)
	record, e, fingerprint := item.record, item.evidence, item.fingerprint
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
	} else if err := tx.Bucket([]byte("active")).Delete(diskUsageID(event.RequestID)); err != nil {
		return err
	}
	if at, ok := a.started[event.AttemptID]; ok {
		if err := diskUsagePut(tx, "started", diskUsageID(event.AttemptID), diskStarted{event.AttemptID, at}); err != nil {
			return err
		}
	} else if err := tx.Bucket([]byte("started")).Delete(diskUsageID(event.AttemptID)); err != nil {
		return err
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
	if err := diskUsagePut(tx, "meta", []byte("state"), *state); err != nil {
		return err
	}
	return ctx.Err()
}
