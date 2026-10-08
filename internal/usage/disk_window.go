package usage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// These positions describe removed derived rows, never native trim permission.
// Floor is shared; AttemptAfter and SummaryAfter describe independent prefixes.
type DiskWindowState struct {
	Floor, AttemptAfter, SummaryAfter    uint64
	Attempts, Summaries, TrimmedAttempts uint64
	Epoch                                uint64
}

type diskWindowCleanup struct {
	Request [32]byte
	Through uint64
}

// Limits apply separately to attempts and summaries (including their versions).
// Thus a call scans <=2*Rows and <=2*InputBytes, without a historical work list.
type DiskWindowLimits struct{ Rows, InputBytes int }
type DiskWindowProgress struct {
	Head                                               ledger.Watermark
	Window                                             DiskWindowState
	Attempts, Summaries, Versions, Scanned, InputBytes int
	Done                                               bool
}

type diskWindowBudget struct {
	limits      DiskWindowLimits
	rows, input int
}

func (b *diskWindowBudget) take(size int) (bool, error) {
	if b.rows == b.limits.Rows || size > b.limits.InputBytes-b.input {
		if b.rows == 0 {
			return false, ErrDiskUsageLimit
		}
		return false, nil
	}
	b.rows++
	b.input += size
	return true, nil
}

// pruneWindowStep removes only expired, already exported prefixes. The caller
// must independently establish exportedThrough from an authenticated complete
// archive publication for this selected owner; this scalar is not such proof.
// No default Runtime or archive deletion is authorized by this primitive.
// Heavy jobs and page readers exclude this zero-queue maintenance operation.
func (p *DiskProjection) pruneWindowStep(ctx context.Context, cutoff time.Time, exportedThrough uint64, limits DiskWindowLimits) (out DiskWindowProgress, err error) {
	if cutoff.IsZero() || limits.Rows < 1 || limits.Rows > 128 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 {
		return out, ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if p.retired.Load() {
		return out, ErrDiskUsageClosed
	}
	if !p.summaryMu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer p.summaryMu.Unlock()
	return p.pruneWindowStepHeld(ctx, cutoff, exportedThrough, limits)
}

// The caller holds the heavy/maintenance lifetime through its actual cleanup.
func (p *DiskProjection) pruneWindowStepHeld(ctx context.Context, cutoff time.Time, exportedThrough uint64, limits DiskWindowLimits) (DiskWindowProgress, error) {
	return p.pruneWindowStepHeldWithLease(ctx, nil, cutoff, exportedThrough, limits)
}

func (p *DiskProjection) pruneWindowStepHeldWithLease(ctx context.Context, lease *ledger.MaintenanceLease, cutoff time.Time, exportedThrough uint64, limits DiskWindowLimits) (out DiskWindowProgress, err error) {
	if cutoff.IsZero() || limits.Rows < 1 || limits.Rows > 128 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 {
		return out, ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !p.mu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil {
		return out, ErrDiskUsageClosed
	}
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			out = DiskWindowProgress{}
		}
	}()
	err = p.updateWindowRowsWithLease(ctx, lease, func(tx *usageTx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || state.Version != diskUsageVersion {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		if state.Window.AttemptAfter > state.Watermark.Sequence || state.Window.SummaryAfter > state.Watermark.Sequence || state.Window.Attempts > state.Watermark.Sequence || state.Window.Summaries > state.Watermark.Sequence || (state.WindowCleanup.Through != 0 && state.WindowCleanup.Through != state.Window.SummaryAfter) {
			return ErrDiskUsageCorrupt
		}
		if exportedThrough > state.Watermark.Sequence {
			return ErrDiskUsageCursor
		}
		out.Head, out.Window = state.Watermark, state.Window
		if exportedThrough == 0 {
			out.Done = state.WindowCleanup.Through == 0
			return nil
		}
		ab, sb := diskWindowBudget{limits: limits}, diskWindowBudget{limits: limits}
		if state.WindowCleanup.Through != 0 {
			if _, err := diskWindowDeleteVersions(ctx, tx, &state, &sb, &out); err != nil {
				return err
			}
			// A completed cleanup is progress, not proof of the next prefixes.
			out.Done = false
		} else {
			attemptDone, err := diskWindowDeleteAttempts(ctx, tx, &state, cutoff, exportedThrough, &ab, &out)
			if err != nil {
				return err
			}
			summaryDone, err := diskWindowDeleteSummaries(ctx, tx, &state, cutoff, exportedThrough, &sb, &out)
			if err != nil {
				return err
			}
			out.Done = attemptDone && summaryDone && state.WindowCleanup.Through == 0
		}
		if out.Attempts+out.Summaries+out.Versions > 0 {
			if state.Window.Epoch == math.MaxUint64 {
				return ErrDiskUsageCorrupt
			}
			state.Window.Epoch++
		}
		if out.Attempts+out.Summaries > 0 {
			floor, err := diskWindowFloor(tx, state.Window)
			if err != nil {
				return err
			}
			if floor < state.Window.Floor {
				return ErrDiskUsageCorrupt
			}
			state.Window.Floor = floor
		}
		out.Scanned, out.InputBytes = ab.rows+sb.rows, ab.input+sb.input
		out.Window = state.Window
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.retired.Load() {
			return ErrDiskUsageClosed
		}
		return diskUsagePut(tx, "meta", []byte("state"), state)
	})
	return out, normalizeDiskUsageDiskError(err)
}

func diskWindowDeleteAttempts(ctx context.Context, input any, state *diskUsageState, cutoff time.Time, exported uint64, budget *diskWindowBudget, out *DiskWindowProgress) (bool, error) {
	tx := usageTransaction(input)
	b, index := tx.Bucket([]byte("attempts")), tx.Bucket([]byte("attempt_index"))
	if b == nil || index == nil {
		return false, ErrDiskUsageCorrupt
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		key, raw := b.Cursor().First()
		if key == nil {
			if state.Window.Attempts != 0 {
				return false, ErrDiskUsageCorrupt
			}
			return true, nil
		}
		if len(key) != 8 || len(raw) > diskUsageMaxValue {
			return false, ErrDiskUsageCorrupt
		}
		ok, err := budget.take(len(raw) + 8 + 12*41)
		if err != nil || !ok {
			return false, err
		}
		seq := binary.BigEndian.Uint64(key)
		var a AttemptEvent
		if seq == 0 || seq > state.Watermark.Sequence || seq <= state.Window.AttemptAfter || json.Unmarshal(raw, &a) != nil || a.Sequence != seq || a.RequestID == "" || a.AttemptID == "" {
			return false, ErrDiskUsageCorrupt
		}
		if seq > exported || !a.CompletedAt.Before(cutoff) {
			return true, nil
		}
		for _, scope := range diskAttemptScopes(a) {
			k := diskUsageIndex(scope.kind, scope.value, seq)
			v := index.Get(k)
			if v == nil || len(v) != 0 {
				return false, ErrDiskUsageCorrupt
			}
			if err := index.Delete(k); err != nil {
				return false, err
			}
		}
		if state.Window.Attempts == 0 || state.Window.TrimmedAttempts == math.MaxUint64 {
			return false, ErrDiskUsageCorrupt
		}
		if err := b.Delete(diskUsageSequence(seq)); err != nil {
			return false, err
		}
		state.Window.Attempts--
		state.Window.TrimmedAttempts++
		state.Window.AttemptAfter = seq
		out.Attempts++
	}
}

func diskWindowDeleteSummaries(ctx context.Context, input any, state *diskUsageState, cutoff time.Time, exported uint64, budget *diskWindowBudget, out *DiskWindowProgress) (bool, error) {
	tx := usageTransaction(input)
	b, index, requests, versions := tx.Bucket([]byte("summaries")), tx.Bucket([]byte("summary_index")), tx.Bucket([]byte("summary_request")), tx.Bucket([]byte("request_version_head"))
	if b == nil || index == nil || requests == nil || versions == nil || tx.Bucket([]byte("active")) == nil {
		return false, ErrDiskUsageCorrupt
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		key, raw := b.Cursor().First()
		if key == nil {
			if state.Window.Summaries != 0 {
				return false, ErrDiskUsageCorrupt
			}
			return true, nil
		}
		if len(key) != 8 || len(raw) > diskUsageMaxValue {
			return false, ErrDiskUsageCorrupt
		}
		ok, err := budget.take(len(raw) + 8 + 3*41 + 2*(32+8))
		if err != nil || !ok {
			return false, err
		}
		seq := binary.BigEndian.Uint64(key)
		var s RequestSummary
		if seq == 0 || seq > state.Watermark.Sequence || seq <= state.Window.SummaryAfter || json.Unmarshal(raw, &s) != nil || s.Sequence != seq || s.RequestID == "" {
			return false, ErrDiskUsageCorrupt
		}
		if seq > exported || !s.CompletedAt.Before(cutoff) {
			return true, nil
		}
		id := diskUsageID(s.RequestID)
		terminalBucket := tx.Bucket([]byte("request_versions"))
		if terminalBucket == nil {
			return false, ErrDiskUsageCorrupt
		}
		terminalRaw := terminalBucket.Get(diskRequestVersionKey(s.RequestID, seq))
		if terminalRaw == nil || len(terminalRaw) > diskUsageMaxValue {
			return false, ErrDiskUsageCorrupt
		}
		if len(terminalRaw)+40 > budget.limits.InputBytes-budget.input {
			if out.Summaries > 0 {
				return false, nil
			}
			return false, ErrDiskUsageLimit
		}
		budget.input += len(terminalRaw) + 40
		var terminal diskRequestVersion
		if json.Unmarshal(terminalRaw, &terminal) != nil || terminal.Sequence != seq || terminal.Summary != s {
			return false, ErrDiskUsageCorrupt
		}
		if !bytes.Equal(requests.Get(id), key) || !bytes.Equal(versions.Get(id), key) || tx.Bucket([]byte("active")).Get(id) != nil {
			return false, ErrDiskUsageCorrupt
		}
		for _, scope := range diskSummaryScopes(s) {
			k := diskUsageIndex(scope.kind, scope.value, seq)
			v := index.Get(k)
			if v == nil || len(v) != 0 {
				return false, ErrDiskUsageCorrupt
			}
			if err := index.Delete(k); err != nil {
				return false, err
			}
		}
		if state.Window.Summaries == 0 {
			return false, ErrDiskUsageCorrupt
		}
		for _, pair := range []struct {
			b   *usageBucket
			key []byte
		}{{b, diskUsageSequence(seq)}, {requests, id}, {versions, id}} {
			if err := pair.b.Delete(pair.key); err != nil {
				return false, err
			}
		}
		state.Window.Summaries--
		state.Window.SummaryAfter = seq
		out.Summaries++
		copy(state.WindowCleanup.Request[:], id)
		state.WindowCleanup.Through = seq
		done, err := diskWindowDeleteVersions(ctx, tx, state, budget, out)
		if err != nil || !done {
			return false, err
		}
	}
}

func diskWindowDeleteVersions(ctx context.Context, input any, state *diskUsageState, budget *diskWindowBudget, out *DiskWindowProgress) (bool, error) {
	tx := usageTransaction(input)
	b := tx.Bucket([]byte("request_versions"))
	if b == nil {
		return false, ErrDiskUsageCorrupt
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		key, raw := b.Cursor().Seek(state.WindowCleanup.Request[:])
		if key == nil || !bytes.HasPrefix(key, state.WindowCleanup.Request[:]) {
			state.WindowCleanup = diskWindowCleanup{}
			return true, nil
		}
		if len(key) != 40 || len(raw) > diskUsageMaxValue {
			return false, ErrDiskUsageCorrupt
		}
		ok, err := budget.take(len(raw) + 40)
		if err != nil || !ok {
			return false, err
		}
		seq := binary.BigEndian.Uint64(key[32:])
		var v diskRequestVersion
		if seq == 0 || seq > state.WindowCleanup.Through || json.Unmarshal(raw, &v) != nil || v.Sequence != seq || !bytes.Equal(diskUsageID(v.Summary.RequestID), state.WindowCleanup.Request[:]) || (v.Summary.Sequence != 0 && v.Summary.Sequence != seq) {
			return false, ErrDiskUsageCorrupt
		}
		var owned [40]byte
		copy(owned[:], key)
		if err := b.Delete(owned[:]); err != nil {
			return false, err
		}
		out.Versions++
	}
}

func diskWindowFloor(input any, state DiskWindowState) (uint64, error) {
	tx := usageTransaction(input)
	floor := uint64(0)
	for _, name := range []string{"attempts", "summaries"} {
		b := tx.Bucket([]byte(name))
		if b == nil {
			return 0, ErrDiskUsageCorrupt
		}
		key, _ := b.Cursor().First()
		if key == nil {
			continue
		}
		if len(key) != 8 {
			return 0, ErrDiskUsageCorrupt
		}
		seq := binary.BigEndian.Uint64(key)
		if seq == 0 {
			return 0, ErrDiskUsageCorrupt
		}
		if floor == 0 || seq < floor {
			floor = seq
		}
	}
	if floor == 0 {
		last := max(state.AttemptAfter, state.SummaryAfter)
		if last == math.MaxUint64 {
			return 0, ErrDiskUsageCorrupt
		}
		floor = last + 1
	}
	return floor, nil
}
