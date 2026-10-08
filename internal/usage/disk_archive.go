package usage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type DiskArchiveLimits struct {
	Pages, Rows, InputBytes, OutputBytes      int
	PageRows, PageInputBytes, PageOutputBytes int
	MaxDuration                               time.Duration
}

// This is an in-process receipt for (AfterSequence, ThroughSequence], fenced
// by the actual source Head. ThroughSequence can precede Head.Sequence in a
// finite step or reference interval. Head is never a fabricated physical cut.
// It is not an archive publication,
// content authentication, restart checkpoint or permission to prune anything.
type DiskArchiveCut struct {
	Head                                 ledger.Watermark
	AfterSequence, ThroughSequence       uint64
	Pages, Rows, InputBytes, OutputBytes int
}

// StreamArchive traverses every attempt in (after, frozen Head] in native
// sequence order, independent of completion dates. Callback slices are borrowed
// only until return. No DB transaction or mutation lock spans callback IO. The
// whole job holds the heavy-query slot/lifetime, so Close joins physical callback
// completion even when cancellation has already requested exit. Callbacks must
// not call Close or re-enter heavy queries on this owner.
//
// A failed page/visitor/deadline/retirement clears the receipt. Earlier visitor
// side effects are incomplete output and cannot advance a manifest high-water
// mark. Caller/publisher still owns encoder, output, disk admission and cleanup.
func (p *DiskProjection) StreamArchive(ctx context.Context, after uint64, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error) (result DiskArchiveCut, err error) {
	return p.streamArchive(ctx, after, limits, visit, nil)
}

// complete runs inside the same physical lifetime as the last source page.
// Archive publication must not escape Close's join after receiving the cut.
func (p *DiskProjection) streamArchive(ctx context.Context, after uint64, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (result DiskArchiveCut, err error) {
	if visit == nil {
		return DiskArchiveCut{}, ErrDiskUsageLimit
	}
	return p.archiveJob(ctx, limits, func(ctx context.Context) (DiskArchiveCut, error) {
		return p.streamArchiveHeld(ctx, after, limits, visit, complete)
	})
}

// The whole operation includes catalog reads, output setup and cleanup. Only
// archiveJob owns the heavy slot and physical retirement/join obligation.
func (p *DiskProjection) archiveJob(ctx context.Context, limits DiskArchiveLimits, run func(context.Context) (DiskArchiveCut, error)) (result DiskArchiveCut, err error) {
	if run == nil || limits.Pages < 1 || limits.Pages > 4096 || limits.Rows < 1 || limits.Rows > 1<<20 || limits.InputBytes < 1 || limits.InputBytes > 128<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 128<<20 || limits.PageRows < 1 || limits.PageRows > 256 || limits.PageInputBytes < 1 || limits.PageInputBytes > 8<<20 || limits.PageOutputBytes < 1 || limits.PageOutputBytes > 8<<20 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return DiskArchiveCut{}, ErrDiskUsageLimit
	}
	if p.retired.Load() || p.summaries == nil {
		return DiskArchiveCut{}, ErrDiskUsageClosed
	}
	if err := ctx.Err(); err != nil {
		return DiskArchiveCut{}, err
	}
	select {
	case p.summaries <- struct{}{}:
	default:
		return DiskArchiveCut{}, ErrDiskUsageBusy
	}
	p.summaryMu.RLock()
	defer func() {
		<-p.summaries
		p.summaryMu.RUnlock()
		if err != nil {
			result = DiskArchiveCut{}
		}
	}()
	if p.retired.Load() {
		return DiskArchiveCut{}, ErrDiskUsageClosed
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
	}()
	return run(ctx)
}

func (p *DiskProjection) streamArchiveHeld(ctx context.Context, after uint64, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (result DiskArchiveCut, err error) {
	return p.streamArchiveBoundedHeld(ctx, after, 0, false, limits, visit, complete)
}

func (p *DiskProjection) streamArchiveBoundedHeld(ctx context.Context, after, through uint64, limited bool, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (result DiskArchiveCut, err error) {
	return p.streamArchiveIntervalHeld(ctx, after, through, limited, false, limits, visit, complete)
}

func (p *DiskProjection) streamArchiveStepHeld(ctx context.Context, after uint64, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (DiskArchiveCut, error) {
	return p.streamArchiveIntervalHeld(ctx, after, 0, false, true, limits, visit, complete)
}

// Only step mode turns a whole-job page/row/byte boundary into a successful
// lower complete prefix. Data/IO/visitor/time/lifetime errors always refuse it.
func (p *DiskProjection) streamArchiveIntervalHeld(ctx context.Context, after, through uint64, limited, step bool, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (result DiskArchiveCut, err error) {
	stats, err := p.ReadStats(ctx)
	if err != nil {
		return DiskArchiveCut{}, err
	}
	if limited && (through > stats.Watermark.Sequence || after > through) {
		return DiskArchiveCut{}, ErrDiskUsageCursor
	}
	if !limited {
		through = stats.Watermark.Sequence
	}
	if after > stats.Watermark.Sequence || after < stats.Window.AttemptAfter {
		return DiskArchiveCut{}, ErrDiskUsageCursor
	}
	result.Head, result.AfterSequence, result.ThroughSequence = stats.Watermark, after, through
	finish := func() (DiskArchiveCut, error) {
		if err := ctx.Err(); err != nil {
			return DiskArchiveCut{}, err
		}
		if p.retired.Load() {
			return DiskArchiveCut{}, ErrDiskUsageClosed
		}
		if result.ThroughSequence < after || result.ThroughSequence > result.Head.Sequence {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		if complete != nil {
			if err := complete(ctx, result); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		return result, nil
	}
	position := after
	for {
		if err := ctx.Err(); err != nil {
			return DiskArchiveCut{}, err
		}
		if p.retired.Load() {
			return DiskArchiveCut{}, ErrDiskUsageClosed
		}
		if result.Pages == limits.Pages {
			if !step || result.Rows == 0 {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			result.ThroughSequence = position
			return finish()
		}
		page := make([]AttemptEvent, 0, limits.PageRows)
		pageInput, pageOutput, done := 0, 2, false
		err := p.viewRows(ctx, func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion || state.Watermark.Sequence < result.Head.Sequence {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			b := tx.Bucket([]byte("attempts"))
			if b == nil {
				return ErrDiskUsageCorrupt
			}
			cursor := b.Cursor()
			seek := diskUsageSequence(position)
			key, raw := cursor.Seek(seek)
			if key != nil && bytes.Equal(key, seek) {
				key, raw = cursor.Next()
			}
			for key != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(key) != 8 {
					return ErrDiskUsageCorrupt
				}
				seq := binary.BigEndian.Uint64(key)
				if seq <= position {
					return ErrDiskUsageCorrupt
				}
				if seq > through {
					done = true
					return nil
				}
				if len(page) == limits.PageRows {
					return nil
				}
				if len(raw) > diskUsageMaxValue {
					return ErrDiskUsageCorrupt
				}
				cost := len(raw) + len(key)
				if cost > limits.PageInputBytes-pageInput {
					if len(page) == 0 {
						return ErrDiskUsageLimit
					}
					return nil
				}
				if result.Rows == limits.Rows || cost > limits.InputBytes-result.InputBytes {
					if !step || result.Rows == 0 {
						return ErrDiskUsageLimit
					}
					result.ThroughSequence = position
					done = true
					return nil
				}
				var a AttemptEvent
				if json.Unmarshal(raw, &a) != nil || a.Sequence != seq || a.RequestID == "" || a.AttemptID == "" {
					return ErrDiskUsageCorrupt
				}
				if step {
					encoded, err := json.Marshal(a)
					if err != nil {
						return err
					}
					outputCost := len(encoded)
					if len(page) > 0 {
						outputCost++
					}
					if outputCost > limits.PageOutputBytes-pageOutput {
						if len(page) == 0 {
							return ErrDiskUsageLimit
						}
						return nil
					}
					if outputCost > limits.OutputBytes-result.OutputBytes-pageOutput {
						if result.Rows == 0 {
							return ErrDiskUsageLimit
						}
						result.ThroughSequence = position
						done = true
						return nil
					}
					pageOutput += outputCost
				}
				page = append(page, a)
				position = seq
				pageInput += cost
				result.Rows++
				result.InputBytes += cost
				key, raw = cursor.Next()
			}
			done = true
			return nil
		})
		if err != nil {
			return DiskArchiveCut{}, err
		}
		result.Pages++
		if step && len(page) == 0 {
			if !done {
				return DiskArchiveCut{}, ErrDiskUsageCorrupt
			}
			return finish()
		}
		raw, err := json.Marshal(page)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if step && len(raw) != pageOutput {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		if len(raw) > limits.PageOutputBytes || len(raw) > limits.OutputBytes-result.OutputBytes {
			return DiskArchiveCut{}, ErrDiskUsageLimit
		}
		result.OutputBytes += len(raw)
		raw = nil // encoder input is accounted separately from the caller's sink
		if len(page) > 0 {
			if err := visit(ctx, page); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		page = nil
		if done {
			return finish()
		}
	}
}
