package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
)

// Fresh generation format v2 adds immutable summary observations. These are
// derived from the same authenticated facts as attempts, never trusted by reopen.
// Keep all versions until generation retirement; retention needs cursor pins.
type diskRequestVersion struct {
	Sequence uint64
	Summary  RequestSummary
}

func diskRequestVersionKey(id string, seq uint64) []byte {
	key := make([]byte, 40)
	copy(key, diskUsageID(id))
	binary.BigEndian.PutUint64(key[32:], seq)
	return key
}

func diskUsagePutRequestVersion(input any, seq uint64, s RequestSummary) error {
	tx := usageTransaction(input)
	b := tx.Bucket([]byte("request_versions"))
	if b == nil || seq == 0 || s.RequestID == "" || (s.Sequence != 0 && s.Sequence != seq) {
		return ErrDiskUsageCorrupt
	}
	key := diskRequestVersionKey(s.RequestID, seq)
	if b.Get(key) != nil {
		return ErrDiskUsageCorrupt
	}
	if err := diskUsagePut(tx, "request_versions", key, diskRequestVersion{seq, s}); err != nil {
		return err
	}
	head := tx.Bucket([]byte("request_version_head"))
	if head == nil {
		return ErrDiskUsageCorrupt
	}
	return head.Put(diskUsageID(s.RequestID), diskUsageSequence(seq))
}

type DiskRequestDetailQuery struct {
	RequestID string
	Limit     int
}

// A fixed local handle, not an HTTP token. Page size may change without changing
// request/cut. No decoded summary/history or shared transaction survives a call.
type DiskRequestDetailCursor struct {
	generation  [16]byte
	request     [32]byte
	head        ledger.Watermark
	after       uint64
	summary     uint64
	windowEpoch uint64
}

type DiskRequestDetailPage struct {
	Summary                          RequestSummary
	Attempts                         []AttemptEvent
	Found, Done                      bool
	Next                             DiskRequestDetailCursor
	Head                             ledger.Watermark
	SummarySequence                  uint64
	Scanned, InputBytes, OutputBytes int
}

// The first page selects the current version through a bounded point index in
// the same frozen View. Continuations pin its exact immutable key, so a missing
// selected version cannot silently fall back to an older summary.
func diskDetailSummary(input any, id string, head, selected uint64, budget *diskFailureBudget) (diskRequestVersion, bool, error) {
	tx := usageTransaction(input)
	b := tx.Bucket([]byte("request_versions"))
	if b == nil {
		return diskRequestVersion{}, false, ErrDiskUsageCorrupt
	}
	indexBytes := 0
	if selected == 0 {
		index, active, terminal := tx.Bucket([]byte("request_version_head")), tx.Bucket([]byte("active")), tx.Bucket([]byte("summary_request"))
		if index == nil || active == nil || terminal == nil {
			return diskRequestVersion{}, false, ErrDiskUsageCorrupt
		}
		key := diskUsageID(id)
		position := index.Get(key)
		if position == nil {
			// Constant-size existence probes, no current-summary decode or
			// fallback. Complete deletion of all keys remains a D-03 concern.
			if active.Get(key) != nil || terminal.Get(key) != nil {
				return diskRequestVersion{}, false, ErrDiskUsageCorrupt
			}
			return diskRequestVersion{}, false, nil
		}
		if len(position) != 8 {
			return diskRequestVersion{}, false, ErrDiskUsageCorrupt
		}
		selected = binary.BigEndian.Uint64(position)
		indexBytes = len(key) + len(position)
	}
	if selected == 0 || selected > head {
		return diskRequestVersion{}, false, ErrDiskUsageCorrupt
	}
	key := diskRequestVersionKey(id, selected)
	raw := b.Get(key)
	if raw == nil {
		return diskRequestVersion{}, false, ErrDiskUsageCorrupt
	}
	if err := budget.take(raw, len(key)+indexBytes); err != nil {
		return diskRequestVersion{}, false, err
	}
	var v diskRequestVersion
	if json.Unmarshal(raw, &v) != nil || v.Sequence != selected || v.Summary.RequestID != id || (v.Summary.Sequence != 0 && v.Summary.Sequence != selected) {
		return diskRequestVersion{}, false, ErrDiskUsageCorrupt
	}
	return v, true, nil
}

// Arrival order is settlement sequence, not event time or AttemptNumber. Read
// one index/companion at a time with an exclusive after and immutable head fence.
func diskDetailNextAttempt(input any, id string, head, after uint64, budget *diskFailureBudget) (AttemptEvent, bool, error) {
	tx := usageTransaction(input)
	index, records := tx.Bucket([]byte("attempt_index")), tx.Bucket([]byte("attempts"))
	if index == nil || records == nil {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	seek := diskUsageIndex(1, id, after)
	c := index.Cursor()
	k, v := c.Seek(seek)
	if k != nil && bytes.Equal(k, seek) {
		k, v = c.Next()
	}
	if k == nil || !bytes.HasPrefix(k, seek[:33]) {
		return AttemptEvent{}, false, nil
	}
	if len(k) != 41 || len(v) != 0 {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	seq := binary.BigEndian.Uint64(k[33:])
	if seq == 0 || seq <= after {
		return AttemptEvent{}, false, ErrDiskUsageCorrupt
	}
	if seq > head {
		return AttemptEvent{}, false, nil
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

// RequestDetailPage repeats the same immutable summary version on every page,
// with attempts in ascending arrival order through H. Versions and all derived
// rows commit with the Usage head. Each candidate is copied under a short View;
// content measurement is outside the transaction and shares one page budget.
func (p *DiskProjection) RequestDetailPage(ctx context.Context, q DiskRequestDetailQuery, cursor DiskRequestDetailCursor, limits DiskUsagePageLimits) (DiskRequestDetailPage, error) {
	if q.RequestID == "" || len(q.RequestID) > 1<<20 || q.Limit < 1 || q.Limit > 100 || limits.ScanRows < 1 || limits.ScanRows > 256 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return DiskRequestDetailPage{}, ErrDiskUsageLimit
	}
	page := DiskRequestDetailPage{}
	err := p.readRows(ctx, func(db *usageEngine) error {
		encodedID, err := json.Marshal(q.RequestID)
		if err != nil {
			return err
		}
		if len(encodedID) > 1<<20 {
			return ErrDiskUsageLimit
		}
		signature := sha256.Sum256(encodedID)
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			if cursor == (DiskRequestDetailCursor{}) {
				cursor = DiskRequestDetailCursor{generation: p.generation, request: signature, head: state.Watermark, windowEpoch: state.Window.Epoch}
			} else if cursor.generation != p.generation || cursor.request != signature || cursor.head.After(state.Watermark) || cursor.after > cursor.head.Sequence || cursor.summary == 0 || cursor.summary > cursor.head.Sequence || cursor.windowEpoch != state.Window.Epoch {
				return ErrDiskUsageCursor
			}
			page.Head = cursor.head
			return nil
		}); err != nil {
			return err
		}
		initial := cursor
		budget := diskFailureBudget{limits: limits}
		var version diskRequestVersion
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			var err error
			version, page.Found, err = diskDetailSummary(tx, q.RequestID, cursor.head.Sequence, cursor.summary, &budget)
			return err
		}); err != nil {
			if errors.Is(err, errDiskFailureBudget) {
				return ErrDiskUsageLimit
			}
			return err
		}
		if !page.Found {
			page.Done = true
			return nil
		}
		page.Summary, page.SummarySequence = version.Summary, version.Sequence
		cursor.summary = version.Sequence
		summaryBytes, err := json.Marshal(page.Summary)
		if err != nil {
			return err
		}
		if len(summaryBytes) > limits.OutputBytes {
			return ErrDiskUsageLimit
		}
		page.OutputBytes = len(summaryBytes)
		page.Attempts = make([]AttemptEvent, 0, q.Limit)
		for len(page.Attempts) < q.Limit {
			if err := ctx.Err(); err != nil {
				return err
			}
			var a AttemptEvent
			var found bool
			err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
				var err error
				a, found, err = diskDetailNextAttempt(tx, q.RequestID, cursor.head.Sequence, cursor.after, &budget)
				return err
			})
			if errors.Is(err, errDiskFailureBudget) {
				break
			}
			if err != nil {
				return err
			}
			if !found {
				page.Done = true
				break
			}
			raw, err := json.Marshal(a)
			if err != nil {
				return err
			}
			if len(raw) > limits.OutputBytes-len(summaryBytes) {
				return ErrDiskUsageLimit
			}
			if len(raw) > limits.OutputBytes-page.OutputBytes {
				break
			}
			page.Attempts = append(page.Attempts, a)
			page.OutputBytes += len(raw)
			cursor.after = a.Sequence
		}
		page.Scanned, page.InputBytes = budget.rows, budget.bytes
		if !page.Done {
			if cursor.after == initial.after {
				return ErrDiskUsageLimit
			}
			page.Next = cursor
		}
		return nil
	})
	if err != nil {
		return DiskRequestDetailPage{}, err
	}
	return page, nil
}
