package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

type diskAttemptScope struct {
	kind  byte
	value string
}

func diskAttemptScopes(a AttemptEvent) []diskAttemptScope {
	return []diskAttemptScope{{1, a.RequestID}, {2, a.ProjectID}, {3, a.WorkUnitID}, {4, a.RunID}, {5, a.ProviderID}, {6, string(a.OfferingID)}, {7, string(a.AccountRegionID)}, {8, string(a.ProviderFailureReason)}, {9, a.DeploymentID}, {10, a.RequestedModel}, {11, a.ProviderModel}, {12, a.Status}}
}
func diskUsageIndex(kind byte, value string, seq uint64) []byte {
	sum := sha256.Sum256([]byte(value))
	key := make([]byte, 41)
	key[0] = kind
	copy(key[1:33], sum[:])
	binary.BigEndian.PutUint64(key[33:], seq)
	return key
}

// Cursors are local owner handles, not HTTP tokens or snapshot authentication.
// Immutable records are fenced at Head; mutable rollups/stats are current-View
// observations and cannot use this cursor to reconstruct an earlier head.
type DiskUsageCursor struct {
	generation  [16]byte
	kind        string
	query       [32]byte
	head        ledger.Watermark
	before      uint64
	windowEpoch uint64
}
type DiskUsagePageLimits struct{ ScanRows, InputBytes, OutputBytes int }
type DiskUsagePage[T any] struct {
	Items                            []T
	Next                             DiskUsageCursor
	Head                             ledger.Watermark
	Done                             bool
	Scanned, InputBytes, OutputBytes int
}

func (p *DiskProjection) AttemptPage(ctx context.Context, q AttemptQuery, cursor DiskUsageCursor, limits DiskUsagePageLimits) (DiskUsagePage[AttemptEvent], error) {
	if q.Limit < 1 || q.Limit > 100 || (q.AccountRegionID != domain.RegionNone && q.AccountRegionID != domain.RegionCN && q.AccountRegionID != domain.RegionGlobal) || !q.ProviderFailureReason.Valid() {
		return DiskUsagePage[AttemptEvent]{}, ErrDiskUsageLimit
	}
	values := diskAttemptScopes(AttemptEvent{RequestID: q.RequestID, ProjectID: q.ProjectID, WorkUnitID: q.WorkUnitID, RunID: q.RunID, ProviderID: q.ProviderID, OfferingID: q.OfferingID, AccountRegionID: q.AccountRegionID, ProviderFailureReason: q.ProviderFailureReason, DeploymentID: q.DeploymentID, RequestedModel: q.RequestedModel, ProviderModel: q.ProviderModel, Status: q.Status})
	var scope *diskAttemptScope
	for i := range values {
		if values[i].value != "" {
			scope = &values[i]
			break
		}
	}
	return diskUsagePage(ctx, p, "attempts", "attempt_index", scope, q, q.BeforeSequence, q.Limit, cursor, limits,
		func(a AttemptEvent) uint64 { return a.Sequence },
		func(a AttemptEvent) bool {
			return (q.ProjectID == "" || a.ProjectID == q.ProjectID) && (q.WorkUnitID == "" || a.WorkUnitID == q.WorkUnitID) && (q.RunID == "" || a.RunID == q.RunID) && (q.ProviderID == "" || a.ProviderID == q.ProviderID) && (q.OfferingID == "" || a.OfferingID == q.OfferingID) && (q.AccountRegionID == "" || a.AccountRegionID == q.AccountRegionID) && (q.ProviderFailureReason == "" || a.ProviderFailureReason == q.ProviderFailureReason) && (q.DeploymentID == "" || a.DeploymentID == q.DeploymentID) && (q.RequestID == "" || a.RequestID == q.RequestID) && (q.RequestedModel == "" || a.RequestedModel == q.RequestedModel) && (q.ProviderModel == "" || a.ProviderModel == q.ProviderModel) && (q.Status == "" || a.Status == q.Status) && (q.Start.IsZero() || !a.CompletedAt.Before(q.Start)) && (q.End.IsZero() || a.CompletedAt.Before(q.End))
		}, func(a AttemptEvent) bool {
			if scope == nil {
				return true
			}
			for _, actual := range diskAttemptScopes(a) {
				if actual.kind == scope.kind {
					return actual.value == scope.value
				}
			}
			return false
		})
}

type DiskSummaryQuery struct {
	BeforeSequence                       uint64
	Limit                                int
	ProjectID, RequestID, RequestedModel string
	Start, End                           time.Time
	FailedOnly                           bool
}

func diskSummaryScopes(s RequestSummary) []diskAttemptScope {
	return []diskAttemptScope{{1, s.RequestID}, {2, s.ProjectID}, {3, s.RequestedModel}}
}
func (p *DiskProjection) SummaryPage(ctx context.Context, q DiskSummaryQuery, cursor DiskUsageCursor, limits DiskUsagePageLimits) (DiskUsagePage[RequestSummary], error) {
	if q.Limit < 1 || q.Limit > 100 {
		return DiskUsagePage[RequestSummary]{}, ErrDiskUsageLimit
	}
	values := diskSummaryScopes(RequestSummary{RequestID: q.RequestID, ProjectID: q.ProjectID, RequestedModel: q.RequestedModel})
	var scope *diskAttemptScope
	for i := range values {
		if values[i].value != "" {
			scope = &values[i]
			break
		}
	}
	return diskUsagePage(ctx, p, "summaries", "summary_index", scope, q, q.BeforeSequence, q.Limit, cursor, limits,
		func(s RequestSummary) uint64 { return s.Sequence },
		func(s RequestSummary) bool {
			return (q.ProjectID == "" || s.ProjectID == q.ProjectID) && (q.RequestID == "" || s.RequestID == q.RequestID) && (q.RequestedModel == "" || s.RequestedModel == q.RequestedModel) && (!q.FailedOnly || s.Outcome != "success") && (q.Start.IsZero() || !s.CompletedAt.Before(q.Start)) && (q.End.IsZero() || s.CompletedAt.Before(q.End))
		},
		func(s RequestSummary) bool {
			if scope == nil {
				return true
			}
			for _, actual := range diskSummaryScopes(s) {
				if actual.kind == scope.kind {
					return actual.value == scope.value
				}
			}
			return false
		})
}

func diskUsagePage[T any](ctx context.Context, p *DiskProjection, bucket, index string, scope *diskAttemptScope, query any, before uint64, rows int, cursor DiskUsageCursor, limits DiskUsagePageLimits, sequence func(T) uint64, match, scopeMatch func(T) bool) (DiskUsagePage[T], error) {
	if limits.ScanRows < 1 || limits.ScanRows > 256 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return DiskUsagePage[T]{}, ErrDiskUsageLimit
	}
	// Reject oversized caller fields before JSON escaping allocates a copy.
	// With <=1MiB raw text the temporary encoder is also bounded (JSON can
	// expand one byte to six); encoded query still has its own 1MiB gate below.
	if !diskUsageQueryWidth(query) {
		return DiskUsagePage[T]{}, ErrDiskUsageLimit
	}
	page := DiskUsagePage[T]{}
	err := p.readRows(ctx, func(db *usageEngine) error {
		queryBytes, err := json.Marshal(query)
		if err != nil {
			return err
		}
		if len(queryBytes) > 1<<20 {
			return ErrDiskUsageLimit
		}
		signature := sha256.Sum256(queryBytes)
		page.Items = make([]T, 0, rows)
		return wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			if cursor == (DiskUsageCursor{}) {
				cursor = DiskUsageCursor{generation: p.generation, kind: bucket, query: signature, head: state.Watermark, windowEpoch: state.Window.Epoch}
				cursor.before = before
			} else if cursor.generation != p.generation || cursor.kind != bucket || cursor.query != signature || cursor.head.After(state.Watermark) || cursor.windowEpoch != state.Window.Epoch {
				return ErrDiskUsageCursor
			}
			page.Head = cursor.head
			var prefix []byte
			b := tx.Bucket([]byte(bucket))
			if scope != nil {
				b = tx.Bucket([]byte(index))
				prefix = diskUsageIndex(scope.kind, scope.value, 0)[:33]
			}
			if b == nil {
				return ErrDiskUsageCorrupt
			}
			c := b.Cursor()
			bound := cursor.head.Sequence
			if cursor.before > 0 && cursor.before-1 < bound {
				bound = cursor.before - 1
			}
			seek := append(append([]byte(nil), prefix...), diskUsageSequence(bound)...)
			k, v := c.Seek(seek)
			if k == nil {
				k, v = c.Last()
			} else if bytes.Compare(k, seek) > 0 {
				k, v = c.Prev()
			}
			last := cursor.before
			for k != nil && (scope == nil || bytes.HasPrefix(k, prefix)) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(k) != len(prefix)+8 {
					return ErrDiskUsageCorrupt
				}
				seq := binary.BigEndian.Uint64(k[len(prefix):])
				if seq == 0 || seq > cursor.head.Sequence || (cursor.before > 0 && seq >= cursor.before) {
					return ErrDiskUsageCorrupt
				}
				raw := v
				if scope != nil {
					if len(v) != 0 {
						return ErrDiskUsageCorrupt
					}
					raw = tx.Bucket([]byte(bucket)).Get(diskUsageSequence(seq))
					if raw == nil {
						return ErrDiskUsageCorrupt
					}
				}
				size := len(k) + len(v)
				if scope != nil {
					size += 8 + len(raw)
				}
				if len(raw) > diskUsageMaxValue {
					return ErrDiskUsageCorrupt
				}
				if page.Scanned == limits.ScanRows || size > limits.InputBytes-page.InputBytes {
					if page.Scanned == 0 {
						return ErrDiskUsageLimit
					}
					break
				}
				var item T
				if json.Unmarshal(raw, &item) != nil || sequence(item) != seq || !scopeMatch(item) {
					return ErrDiskUsageCorrupt
				}
				if match(item) {
					if len(raw) > limits.OutputBytes-page.OutputBytes {
						if len(page.Items) == 0 && page.Scanned == 0 {
							return ErrDiskUsageLimit
						}
						break
					}
					page.Items = append(page.Items, item)
					page.OutputBytes += len(raw)
				}
				page.Scanned++
				page.InputBytes += size
				last = seq
				k, v = c.Prev()
				if len(page.Items) == rows {
					break
				}
			}
			page.Done = k == nil || (scope != nil && !bytes.HasPrefix(k, prefix))
			if !page.Done {
				if last == cursor.before {
					return ErrDiskUsageLimit
				}
				cursor.before = last
				page.Next = cursor
			}
			return nil
		})
	})
	if err != nil {
		return DiskUsagePage[T]{}, err
	}
	return page, nil
}

func diskUsageQueryWidth(query any) bool {
	var fields []string
	switch q := query.(type) {
	case AttemptQuery:
		fields = []string{q.ProjectID, q.WorkUnitID, q.RunID, q.ProviderID, string(q.OfferingID), string(q.AccountRegionID), string(q.ProviderFailureReason), q.DeploymentID, q.RequestID, q.RequestedModel, q.ProviderModel, q.Status}
	case DiskSummaryQuery:
		fields = []string{q.ProjectID, q.RequestID, q.RequestedModel}
	case FailureQuery:
		fields = []string{q.ProjectID, q.RequestID, q.ProviderID, string(q.OfferingID), string(q.AccountRegionID), string(q.ProviderFailureReason), q.DeploymentID, q.ProviderModel, q.RequestedModel}
	default:
		return false
	}
	remaining := 1 << 20
	for _, field := range fields {
		if len(field) > remaining {
			return false
		}
		remaining -= len(field)
	}
	return true
}
