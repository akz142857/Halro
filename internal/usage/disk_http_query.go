package usage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

var ErrDiskUsageCursor = errors.New("usage cursor is invalid for this owner, query or prefix")

// Fixed tokens carry only positions and the bounded failure-join decision.
// The random signing key is independent of the public generation identifier,
// is never persisted, and is distinct for every fresh derived owner. These
// tokens authorize continuation of an authenticated admin query, not native
// facts, saved DB contents, an earlier role, or a historical mutable view.
const diskHTTPCursorBytes = 115

func (p *DiskProjection) encodeHTTPCursor(kind byte, cursor DiskFailureCursor) string {
	var raw [diskHTTPCursorBytes + sha256.Size]byte
	raw[0], raw[1] = 2, kind
	copy(raw[2:18], cursor.base.generation[:])
	copy(raw[18:50], cursor.base.query[:])
	values := [...]uint64{cursor.base.head.Generation, cursor.base.head.Sequence, uint64(cursor.base.head.Offset), cursor.base.before, cursor.pending, cursor.latest, cursor.attemptBefore, cursor.base.windowEpoch}
	for i, value := range values {
		binary.BigEndian.PutUint64(raw[50+i*8:], value)
	}
	if cursor.latestKnown {
		raw[114] |= 1
	}
	if cursor.matched {
		raw[114] |= 2
	}
	if cursor.resolved {
		raw[114] |= 4
	}
	mac := hmac.New(sha256.New, p.cursorKey[:])
	_, _ = mac.Write(raw[:diskHTTPCursorBytes])
	copy(raw[diskHTTPCursorBytes:], mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (p *DiskProjection) decodeHTTPCursor(ctx context.Context, token string, kind byte) (DiskFailureCursor, error) {
	if err := ctx.Err(); err != nil {
		return DiskFailureCursor{}, err
	}
	if p.retired.Load() {
		return DiskFailureCursor{}, ErrDiskUsageClosed
	}
	if token == "" {
		return DiskFailureCursor{}, nil
	}
	if len(token) != base64.RawURLEncoding.EncodedLen(diskHTTPCursorBytes+sha256.Size) {
		return DiskFailureCursor{}, ErrDiskUsageCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != diskHTTPCursorBytes+sha256.Size || raw[0] != 2 || raw[1] != kind || raw[114] > 7 {
		return DiskFailureCursor{}, ErrDiskUsageCursor
	}
	mac := hmac.New(sha256.New, p.cursorKey[:])
	_, _ = mac.Write(raw[:diskHTTPCursorBytes])
	if !hmac.Equal(mac.Sum(nil), raw[diskHTTPCursorBytes:]) {
		return DiskFailureCursor{}, ErrDiskUsageCursor
	}
	var cursor DiskFailureCursor
	copy(cursor.base.generation[:], raw[2:18])
	copy(cursor.base.query[:], raw[18:50])
	if cursor.base.generation != p.generation {
		return DiskFailureCursor{}, ErrDiskUsageCursor
	}
	var values [8]uint64
	for i := range values {
		values[i] = binary.BigEndian.Uint64(raw[50+i*8:])
	}
	if values[2] > math.MaxInt64 {
		return DiskFailureCursor{}, ErrDiskUsageCursor
	}
	cursor.base.head = ledger.Watermark{Generation: values[0], Sequence: values[1], Offset: int64(values[2])}
	cursor.base.before, cursor.pending, cursor.latest, cursor.attemptBefore = values[3], values[4], values[5], values[6]
	cursor.base.windowEpoch = values[7]
	cursor.latestKnown, cursor.matched, cursor.resolved = raw[114]&1 != 0, raw[114]&2 != 0, raw[114]&4 != 0
	if kind == 1 {
		cursor.base.kind = "attempts"
		if cursor.pending != 0 || cursor.latest != 0 || cursor.attemptBefore != 0 || raw[114] != 0 {
			return DiskFailureCursor{}, ErrDiskUsageCursor
		}
	} else {
		cursor.base.kind = "failures"
	}
	return cursor, nil
}

// One physical page per HTTP call, including empty pages. Returning a signed
// continuation on an empty page preserves progress through sparse predicates
// and long request-to-attempt joins without an unbounded scan or a server map.
// BeforeSequence is reserved for internal/legacy adapters; public disk callers
// use the opaque token. Query and limit stay fixed for the continuation.
func (p *DiskProjection) QueryAttemptsHTTP(ctx context.Context, q AttemptQuery, token string, limits DiskUsagePageLimits) (AttemptPage, error) {
	if q.BeforeSequence != 0 {
		return AttemptPage{}, ErrDiskUsageCursor
	}
	cursor, err := p.decodeHTTPCursor(ctx, token, 1)
	if err != nil {
		return AttemptPage{}, err
	}
	page, err := p.AttemptPage(ctx, q, cursor.base, limits)
	if err != nil {
		return AttemptPage{}, err
	}
	result := AttemptPage{Attempts: page.Items}
	if !page.Done {
		result.NextCursor = p.encodeHTTPCursor(1, DiskFailureCursor{base: page.Next})
	}
	return result, nil
}

func (p *DiskProjection) QueryFailedRequestsHTTP(ctx context.Context, q FailureQuery, token string, limits DiskUsagePageLimits) (FailurePage, error) {
	if q.BeforeSequence != 0 {
		return FailurePage{}, ErrDiskUsageCursor
	}
	cursor, err := p.decodeHTTPCursor(ctx, token, 2)
	if err != nil {
		return FailurePage{}, err
	}
	page, err := p.FailurePage(ctx, q, cursor, limits)
	if err != nil {
		return FailurePage{}, err
	}
	result := FailurePage{Failures: page.Items}
	if !page.Done {
		result.NextCursor = p.encodeHTTPCursor(2, page.Next)
	}
	return result, nil
}

type DiskDetailLimits struct {
	Pages, Attempts, InputBytes, OutputBytes int
	MaxDuration                              time.Duration
	Page                                     DiskUsagePageLimits
}

// ReadRequestDetailHTTP preserves the existing complete-detail response. The
// whole job has fixed source/output/time limits and shares the zero-queue heavy
// query slot with Summary. It pins the immutable summary and head across pages,
// joins physical Close even between pages, and returns no partial success.
// The caller owns response admission/encoding and the returned result's fees.
func (p *DiskProjection) ReadRequestDetailHTTP(ctx context.Context, id string, limits DiskDetailLimits) (result RequestDetail, found bool, err error) {
	return p.readRequestDetailHTTP(ctx, id, limits, p.RequestDetailPage)
}

func (p *DiskProjection) readRequestDetailHTTP(ctx context.Context, id string, limits DiskDetailLimits, readPage func(context.Context, DiskRequestDetailQuery, DiskRequestDetailCursor, DiskUsagePageLimits) (DiskRequestDetailPage, error)) (result RequestDetail, found bool, err error) {
	if id == "" || len(id) > 128 || limits.Pages < 1 || limits.Pages > 4096 || limits.Attempts < 1 || limits.Attempts > 16384 || limits.InputBytes < 1 || limits.InputBytes > 128<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return RequestDetail{}, false, ErrDiskUsageLimit
	}
	if p.retired.Load() || p.summaries == nil {
		return RequestDetail{}, false, ErrDiskUsageClosed
	}
	if err := ctx.Err(); err != nil {
		return RequestDetail{}, false, err
	}
	select {
	case p.summaries <- struct{}{}:
	default:
		return RequestDetail{}, false, ErrDiskUsageBusy
	}
	p.summaryMu.RLock()
	defer func() {
		<-p.summaries
		p.summaryMu.RUnlock()
		if err != nil {
			result, found = RequestDetail{}, false
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
	}()
	var cursor DiskRequestDetailCursor
	var first DiskRequestDetailPage
	input, output := 0, 0
	for n := 0; n < limits.Pages; n++ {
		page, pageErr := readPage(ctx, DiskRequestDetailQuery{RequestID: id, Limit: 100}, cursor, limits.Page)
		if pageErr != nil {
			return RequestDetail{}, false, pageErr
		}
		if n == 0 {
			first, result.Summary, found = page, page.Summary, page.Found
			first.Attempts = nil
			result.Attempts = make([]AttemptEvent, 0)
		} else if page.Head != first.Head || page.SummarySequence != first.SummarySequence || page.Summary != first.Summary || page.Found != first.Found {
			return RequestDetail{}, false, ErrDiskUsageCorrupt
		}
		if page.InputBytes > limits.InputBytes-input || page.OutputBytes > limits.OutputBytes-output || len(page.Attempts) > limits.Attempts-len(result.Attempts) {
			return RequestDetail{}, false, ErrDiskUsageLimit
		}
		input, output = input+page.InputBytes, output+page.OutputBytes
		result.Attempts = append(result.Attempts, page.Attempts...)
		if page.Done {
			if !found {
				return RequestDetail{}, false, nil
			}
			raw, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return RequestDetail{}, false, marshalErr
			}
			if len(raw)+1 > limits.OutputBytes {
				return RequestDetail{}, false, ErrDiskUsageLimit
			}
			return result, true, nil
		}
		if page.Next == cursor || page.Next == (DiskRequestDetailCursor{}) {
			return RequestDetail{}, false, ErrDiskUsageCorrupt
		}
		cursor = page.Next
	}
	return RequestDetail{}, false, ErrDiskUsageLimit
}
