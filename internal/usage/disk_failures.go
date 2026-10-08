package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// This local cursor contains positions and a predicate decision, never request
// history or decoded rows. It must be retained intact until Done, including on
// empty pages while an expensive request-to-attempt join is still unresolved.
type DiskFailureCursor struct {
	base          DiskUsageCursor
	pending       uint64
	latest        uint64
	attemptBefore uint64
	latestKnown   bool
	matched       bool
	resolved      bool
}

type DiskFailurePage struct {
	Items                            []RequestFailure
	Next                             DiskFailureCursor
	Head                             ledger.Watermark
	Done                             bool
	Scanned, InputBytes, OutputBytes int
}

var errDiskFailureBudget = errors.New("disk failure join page budget exhausted")

type diskFailureBudget struct {
	limits DiskUsagePageLimits
	rows   int
	bytes  int
}

// Association index plus its companion counts as one scanned association; all
// key/value bytes and repeated point reads are charged. Fixed metadata is separate.
func (b *diskFailureBudget) take(raw []byte, keyBytes int) error {
	if len(raw) > diskUsageMaxValue {
		return ErrDiskUsageCorrupt
	}
	if b.rows == b.limits.ScanRows || len(raw)+keyBytes > b.limits.InputBytes-b.bytes {
		return errDiskFailureBudget
	}
	b.rows++
	b.bytes += len(raw) + keyBytes
	return nil
}

func failureSummaryMatches(s RequestSummary, q FailureQuery) bool {
	return s.Outcome != "" && s.Outcome != "success" && (q.ProjectID == "" || s.ProjectID == q.ProjectID) && (q.RequestID == "" || s.RequestID == q.RequestID) && (q.RequestedModel == "" || s.RequestedModel == q.RequestedModel) && (q.Start.IsZero() || !s.CompletedAt.Before(q.Start)) && (q.End.IsZero() || s.CompletedAt.Before(q.End))
}
func failureAttemptMatches(a AttemptEvent, q FailureQuery) bool {
	return (q.ProviderID == "" || a.ProviderID == q.ProviderID) && (q.OfferingID == "" || a.OfferingID == q.OfferingID) && (q.AccountRegionID == "" || a.AccountRegionID == q.AccountRegionID) && (q.ProviderFailureReason == "" || a.ProviderFailureReason == q.ProviderFailureReason) && (q.DeploymentID == "" || a.DeploymentID == q.DeploymentID) && (q.ProviderModel == "" || a.ProviderModel == q.ProviderModel)
}
func diskRequestFailure(s RequestSummary, latest *AttemptEvent) RequestFailure {
	r := RequestFailure{RequestID: s.RequestID, ProjectID: s.ProjectID, KeyID: s.KeyID, RequestedModel: s.RequestedModel, Outcome: s.Outcome, Sequence: s.Sequence, AcceptedAt: s.AcceptedAt, CompletedAt: s.CompletedAt, Attempts: s.Attempts, Fallbacks: s.Fallbacks}
	if latest != nil && latest.Status != "success" {
		a := latest
		r.LastFailure = &FailureContext{AttemptID: a.AttemptID, AttemptNumber: a.AttemptNumber, ErrorClass: a.ErrorClass, ProviderStatus: a.HTTPStatus, ProviderID: a.ProviderID, DeploymentID: a.DeploymentID, OfferingID: a.OfferingID, ProfileID: a.ProfileID, AccountRegionID: a.AccountRegionID, ProviderModel: a.ProviderModel, ProviderCode: a.ProviderCode, ProviderFailureReason: a.ProviderFailureReason, ProviderRequestID: a.ProviderRequestID, FailurePhase: a.FailurePhase, Retryable: a.Retryable, Ambiguous: a.Ambiguous, FailureSemanticsRecorded: a.FailureSemanticsRecorded, CompletedAt: a.CompletedAt}
	}
	return r
}

func diskFailureSummary(input any, seq uint64, budget *diskFailureBudget) (RequestSummary, error) {
	tx := usageTransaction(input)
	b := tx.Bucket([]byte("summaries"))
	if b == nil {
		return RequestSummary{}, ErrDiskUsageCorrupt
	}
	raw := b.Get(diskUsageSequence(seq))
	if raw == nil {
		return RequestSummary{}, ErrDiskUsageCorrupt
	}
	if err := budget.take(raw, 8); err != nil {
		return RequestSummary{}, err
	}
	var s RequestSummary
	if json.Unmarshal(raw, &s) != nil || s.Sequence != seq || s.RequestID == "" {
		return RequestSummary{}, ErrDiskUsageCorrupt
	}
	return s, nil
}
func diskFailureAttempt(input any, seq uint64, id string, budget *diskFailureBudget) (AttemptEvent, error) {
	tx := usageTransaction(input)
	b := tx.Bucket([]byte("attempts"))
	if b == nil {
		return AttemptEvent{}, ErrDiskUsageCorrupt
	}
	raw := b.Get(diskUsageSequence(seq))
	if raw == nil {
		return AttemptEvent{}, ErrDiskUsageCorrupt
	}
	if err := budget.take(raw, 8); err != nil {
		return AttemptEvent{}, err
	}
	var a AttemptEvent
	if json.Unmarshal(raw, &a) != nil || a.Sequence != seq || a.RequestID != id {
		return AttemptEvent{}, ErrDiskUsageCorrupt
	}
	return a, nil
}

// Seek one immutable association at or below H, with an exclusive continuation
// position. Its physical index and companion are verified before any predicate.
func diskFailureNextAttempt(input any, id string, head, before uint64, budget *diskFailureBudget) (AttemptEvent, bool, error) {
	tx := usageTransaction(input)
	index, records := tx.Bucket([]byte("attempt_index")), tx.Bucket([]byte("attempts"))
	if index == nil || records == nil {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	prefix := diskUsageIndex(1, id, 0)[:33]
	bound := head
	if before > 0 && before-1 < bound {
		bound = before - 1
	}
	seek := diskUsageIndex(1, id, bound)
	c := index.Cursor()
	k, v := c.Seek(seek)
	if k == nil {
		k, v = c.Last()
	} else if bytes.Compare(k, seek) > 0 {
		k, v = c.Prev()
	}
	if k == nil || !bytes.HasPrefix(k, prefix) {
		return AttemptEvent{}, false, nil
	}
	if len(k) != 41 || len(v) != 0 {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	seq := binary.BigEndian.Uint64(k[33:])
	if seq == 0 || seq > head || (before > 0 && seq >= before) {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	raw := records.Get(diskUsageSequence(seq))
	if raw == nil {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	if err := budget.take(raw, len(k)+len(v)+8); err != nil {
		return AttemptEvent{}, false, err
	}
	var a AttemptEvent
	if json.Unmarshal(raw, &a) != nil || a.Sequence != seq || a.RequestID != id {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	return a, true, nil
}

func diskFailureNextSummary(input any, q FailureQuery, head, before uint64, budget *diskFailureBudget) (RequestSummary, bool, error) {
	tx := usageTransaction(input)
	records := tx.Bucket([]byte("summaries"))
	if records == nil {
		return RequestSummary{}, false, ErrDiskUsageCorrupt
	}
	b := records
	var scope *diskAttemptScope
	for _, value := range []diskAttemptScope{{1, q.RequestID}, {2, q.ProjectID}, {3, q.RequestedModel}} {
		if value.value != "" {
			copy := value
			scope = &copy
			break
		}
	}
	var prefix []byte
	if scope != nil {
		b = tx.Bucket([]byte("summary_index"))
		if b == nil {
			return RequestSummary{}, false, ErrDiskUsageCorrupt
		}
		prefix = diskUsageIndex(scope.kind, scope.value, 0)[:33]
	}
	bound := head
	if before > 0 && before-1 < bound {
		bound = before - 1
	}
	seek := append(append([]byte(nil), prefix...), diskUsageSequence(bound)...)
	c := b.Cursor()
	k, v := c.Seek(seek)
	if k == nil {
		k, v = c.Last()
	} else if bytes.Compare(k, seek) > 0 {
		k, v = c.Prev()
	}
	if k == nil || (scope != nil && !bytes.HasPrefix(k, prefix)) {
		return RequestSummary{}, false, nil
	}
	if len(k) != len(prefix)+8 {
		return RequestSummary{}, false, ErrDiskUsageCorrupt
	}
	seq := binary.BigEndian.Uint64(k[len(prefix):])
	if seq == 0 || seq > head || (before > 0 && seq >= before) {
		return RequestSummary{}, false, ErrDiskUsageCorrupt
	}
	raw := v
	keyBytes := len(k)
	if scope != nil {
		if len(v) != 0 {
			return RequestSummary{}, false, ErrDiskUsageCorrupt
		}
		raw = records.Get(diskUsageSequence(seq))
		if raw == nil {
			return RequestSummary{}, false, ErrDiskUsageCorrupt
		}
		keyBytes += len(v) + 8
	}
	if err := budget.take(raw, keyBytes); err != nil {
		return RequestSummary{}, false, err
	}
	var s RequestSummary
	if json.Unmarshal(raw, &s) != nil || s.Sequence != seq || s.RequestID == "" {
		return RequestSummary{}, false, ErrDiskUsageCorrupt
	}
	if scope != nil {
		matched := false
		for _, actual := range diskSummaryScopes(s) {
			if actual.kind == scope.kind {
				matched = actual.value == scope.value
				break
			}
		}
		if !matched {
			return RequestSummary{}, false, ErrDiskUsageCorrupt
		}
	}
	return s, true, nil
}

// FailurePage preserves the legacy request/attempt predicates while allowing a
// wide association to span pages. It never turns an unresolved predicate into
// "no match", and its context comes from the latest settlement, independently
// of which attempt satisfied the filter. Late settlements through H participate.
func (p *DiskProjection) FailurePage(ctx context.Context, q FailureQuery, cursor DiskFailureCursor, limits DiskUsagePageLimits) (DiskFailurePage, error) {
	if q.Limit < 1 || q.Limit > 100 || (q.AccountRegionID != domain.RegionNone && q.AccountRegionID != domain.RegionCN && q.AccountRegionID != domain.RegionGlobal) || !q.ProviderFailureReason.Valid() || limits.ScanRows < 1 || limits.ScanRows > 256 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 || !diskUsageQueryWidth(q) {
		return DiskFailurePage{}, ErrDiskUsageLimit
	}
	page := DiskFailurePage{}
	err := p.readRows(ctx, func(db *usageEngine) error {
		raw, err := json.Marshal(q)
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return ErrDiskUsageLimit
		}
		signature := sha256.Sum256(raw)
		page.Items = make([]RequestFailure, 0, q.Limit)
		// Initialize and validate the frozen cut before candidate reads. Each
		// candidate View copies only one summary and its latest settlement;
		// all output measurement/encoding happens after that View closes.
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			if cursor == (DiskFailureCursor{}) {
				cursor.base = DiskUsageCursor{generation: p.generation, kind: "failures", query: signature, head: state.Watermark, before: q.BeforeSequence, windowEpoch: state.Window.Epoch}
			} else if cursor.base.generation != p.generation || cursor.base.kind != "failures" || cursor.base.query != signature || cursor.base.head.After(state.Watermark) || cursor.base.windowEpoch != state.Window.Epoch {
				return ErrDiskUsageCursor
			}
			return nil
		}); err != nil {
			return err
		}
		initial := cursor
		page.Head = cursor.base.head
		budget := diskFailureBudget{limits: limits}
		stop := false
		for !stop && !page.Done && len(page.Items) < q.Limit {
			var result RequestFailure
			ready := false
			err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
				for !stop {
					if err := ctx.Err(); err != nil {
						return err
					}
					var summary RequestSummary
					if cursor.pending == 0 {
						var found bool
						summary, found, err = diskFailureNextSummary(tx, q, cursor.base.head.Sequence, cursor.base.before, &budget)
						if errors.Is(err, errDiskFailureBudget) {
							stop = true
							return nil
						}
						if err != nil {
							return err
						}
						if !found {
							page.Done = true
							return nil
						}
						cursor.base.before = summary.Sequence
						if !failureSummaryMatches(summary, q) {
							continue
						}
						cursor.pending = summary.Sequence
					} else {
						summary, err = diskFailureSummary(tx, cursor.pending, &budget)
						if errors.Is(err, errDiskFailureBudget) {
							stop = true
							return nil
						}
						if err != nil {
							return err
						}
						if !failureSummaryMatches(summary, q) {
							return ErrDiskUsageCorrupt
						}
					}
					var latest *AttemptEvent
					for !cursor.resolved {
						if err := ctx.Err(); err != nil {
							return err
						}
						a, found, err := diskFailureNextAttempt(tx, summary.RequestID, cursor.base.head.Sequence, cursor.attemptBefore, &budget)
						if errors.Is(err, errDiskFailureBudget) {
							stop = true
							return nil
						}
						if err != nil {
							return err
						}
						if !found {
							cursor.latestKnown, cursor.resolved = true, true
							cursor.matched = !attemptFiltered(q)
							break
						}
						if !cursor.latestKnown {
							cursor.latestKnown = true
							cursor.latest = a.Sequence
							copy := a
							latest = &copy
						}
						cursor.attemptBefore = a.Sequence
						if !attemptFiltered(q) || failureAttemptMatches(a, q) {
							cursor.matched, cursor.resolved = true, true
						}
					}
					if cursor.matched {
						if cursor.latest > 0 && latest == nil {
							a, err := diskFailureAttempt(tx, cursor.latest, summary.RequestID, &budget)
							if errors.Is(err, errDiskFailureBudget) {
								stop = true
								return nil
							}
							if err != nil {
								return err
							}
							latest = &a
						}
						result = diskRequestFailure(summary, latest)
						ready = true
						return nil
					}
					cursor.clearPending()
				}
				return nil
			})
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return err
			}
			if len(encoded) > limits.OutputBytes {
				return ErrDiskUsageLimit
			}
			if len(encoded) > limits.OutputBytes-page.OutputBytes {
				break
			}
			page.Items = append(page.Items, result)
			page.OutputBytes += len(encoded)
			cursor.clearPending()
		}
		page.Scanned, page.InputBytes = budget.rows, budget.bytes
		if !page.Done {
			if cursor == initial {
				return ErrDiskUsageLimit
			}
			page.Next = cursor
		}
		return nil
	})
	if err != nil {
		return DiskFailurePage{}, err
	}
	return page, nil
}

func (c *DiskFailureCursor) clearPending() {
	c.pending, c.latest, c.attemptBefore = 0, 0, 0
	c.latestKnown, c.matched, c.resolved = false, false, false
}
