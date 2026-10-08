package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrAuthenticatedRecoveryIncomplete = errors.New("authenticated Ledger recovery input was not completely enumerated")

// This workspace covers bounded decoded/returned rows and one lookahead while
// the callback is live, separately from Snapshot's per-query workspace. It is a
// conservative logical estimate, not a physical capacity acceptance.
const AuthenticatedRecoveryRAMBytes int64 = 64 << 20

type AuthenticatedRecoveryPage[T any] struct {
	Items []T
	Next  string
	Done  bool
	Bytes int
}

// A view pins one native-built root. Advance may publish later component roots
// between pages; both recovery input sets and point dependencies stay at Head.
// Complete proves enumeration and owner availability, not recovery completion,
// current HA permission, cross-component publication or restart eligibility.
type AuthenticatedRecoveryView interface {
	Head() ReplayAnchor
	PendingLeasePage(context.Context, string, ProjectionPageLimits) (AuthenticatedRecoveryPage[PendingLease], error)
	UnfinalizedRequestPage(context.Context, string, ProjectionPageLimits) (AuthenticatedRecoveryPage[ProjectedRequest], error)
	RequestAttemptPage(context.Context, string, string, ProjectionPageLimits) (AuthenticatedRecoveryPage[ProjectedAttempt], error)
	ReadLease(context.Context, string) (Record, bool, error)
	ReadRequest(context.Context, string) (ProjectedRequest, bool, error)
	Complete(context.Context) error
}

type recoveryProgress struct {
	next string
	done bool
}
type authenticatedRecoveryView struct {
	mu                sync.Mutex
	owner             *AuthenticatedLedgerProjection
	snapshot          *authenticatedstate.Snapshot
	ctx               context.Context
	head              ReplayAnchor
	active, completed bool
	failure           error
	pending, requests recoveryProgress
}

// WithRecoveryView allows one callback per candidate, with no waiting queue.
// The caller must keep only bounded pages/terminal work, and call Complete after
// both independent input streams reach Done. Close cancels and joins the whole
// callback; calling Close from that same callback would deadlock and is invalid.
// No source DB, saved root or caller-supplied resume anchor is accepted here.
func (p *AuthenticatedLedgerProjection) WithRecoveryView(ctx context.Context, visit func(AuthenticatedRecoveryView) error) error {
	if visit == nil {
		return ErrProjectionUnavailable
	}
	return p.withAuthenticatedPinnedView(ctx, func(v *authenticatedRecoveryView) error { return visit(v) }, true)
}

// Recovery and cohort visitors share one fixed callback slot. Callbacks run
// without owner/State locks or DB transactions. Close joins recovery; cohort
// Close joins physical pins but keeps the visitor fee until the callback exits.
func (p *AuthenticatedLedgerProjection) withAuthenticatedPinnedView(ctx context.Context, visit func(*authenticatedRecoveryView) error, requireComplete bool) (resultErr error) {
	if p == nil || visit == nil {
		return ErrProjectionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	if !p.ready || p.readFailure.Load() != nil || p.closeContext.Err() != nil {
		p.mu.Unlock()
		return errors.Join(ErrProjectionUnavailable, p.readFault())
	}
	if p.recoveryDone != nil || p.cohortDone != nil {
		p.mu.Unlock()
		return authenticatedstate.ErrBusy
	}
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedRecoveryRAMBytes})
	if err != nil {
		p.mu.Unlock()
		return err
	}
	pin, err := p.store.Snapshot(ctx)
	if err != nil {
		p.mu.Unlock()
		return errors.Join(err, permit.Release())
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.closeContext, cancel)
	v := &authenticatedRecoveryView{owner: p, snapshot: pin, ctx: opCtx, active: true}
	position, found, _, err := recoveryGet[projectionPosition](v, opCtx, "meta", []byte("position"))
	if err != nil || !found {
		if err == nil {
			err = ErrProjectionCorrupt
		}
		if errors.Is(err, ErrProjectionCorrupt) || errors.Is(err, authenticatedstate.ErrCorrupt) {
			p.ready = false
			p.failure = errors.Join(p.failure, err)
		}
		p.mu.Unlock()
		stop()
		cancel()
		return errors.Join(err, pin.Close(), permit.Release())
	}

	v.head = ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
	done := make(chan struct{})
	if requireComplete {
		p.recoveryDone = done
	} else {
		p.cohortDone = done
	}
	p.mu.Unlock()
	returned := false
	defer func() {
		stop()
		cancel()
		v.mu.Lock()
		v.active = false
		cleanupErr := errors.Join(pin.Close(), permit.Release())
		resultErr = errors.Join(resultErr, v.failure, cleanupErr)
		v.owner, v.snapshot, v.ctx = nil, nil, nil
		v.pending, v.requests = recoveryProgress{}, recoveryProgress{}
		v.mu.Unlock()
		p.mu.Lock()
		if errors.Is(resultErr, ErrProjectionCorrupt) || errors.Is(resultErr, authenticatedstate.ErrCorrupt) || !returned || cleanupErr != nil {
			p.ready = false
			p.failure = errors.Join(p.failure, ErrProjectionSource, resultErr)
		}
		if requireComplete {
			p.recoveryDone = nil
		} else {
			p.cohortDone = nil
		}
		close(done) // Cleanup/physical Close/credit retirement precede the join signal.
		p.mu.Unlock()
	}()
	if err := v.check(ctx); err != nil {
		returned = true
		return err
	}
	resultErr = visit(v)
	returned = true
	v.mu.Lock()
	if resultErr == nil {
		resultErr = v.check(ctx)
	}
	if resultErr == nil && requireComplete && !v.completed {
		resultErr = ErrAuthenticatedRecoveryIncomplete
	}
	v.mu.Unlock()
	return resultErr
}

func (v *authenticatedRecoveryView) Head() ReplayAnchor { return v.head }
func (v *authenticatedRecoveryView) check(ctx context.Context) error {
	if !v.active || v.owner == nil {
		return ErrProjectionUnavailable
	}
	if v.failure != nil {
		return v.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.ctx.Err(); err != nil {
		return err
	}
	p := v.owner
	if p.closeContext.Err() != nil {
		return ErrProjectionUnavailable
	}
	if !p.mu.TryRLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	if !p.ready || p.readFailure.Load() != nil {
		return errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	return nil
}
func (v *authenticatedRecoveryView) begin(ctx context.Context) (context.Context, func(), error) {
	if !v.mu.TryLock() {
		return nil, nil, authenticatedstate.ErrBusy
	}
	if err := v.check(ctx); err != nil {
		v.mu.Unlock()
		return nil, nil, err
	}
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(v.ctx, cancel)
	return c, func() { stop(); cancel(); v.mu.Unlock() }, nil
}
func (v *authenticatedRecoveryView) finish(ctx context.Context, err error) error {
	if err == nil {
		err = v.check(ctx)
	}
	if errors.Is(err, ErrProjectionCorrupt) || errors.Is(err, authenticatedstate.ErrCorrupt) {
		v.failure = err
	}
	return err
}
func recoveryDecode[T any](raw []byte) (T, error) {
	var value T
	if len(raw) > projectionMaxValue || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return value, ErrProjectionCorrupt
	}
	return value, nil
}
func recoveryGet[T any](v *authenticatedRecoveryView, ctx context.Context, bucket string, key []byte) (T, bool, int, error) {
	var value T
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return value, false, 0, err
	}
	raw, found, err := v.snapshot.Get(ctx, k)
	if err != nil || !found {
		return value, false, 0, err
	}
	value, err = recoveryDecode[T](raw)
	return value, found, len(k) + len(raw), err
}
func recoveryBounds(bucket string, prefix []byte) ([]byte, []byte, error) {
	ordinal, err := authenticatedLedgerBucket(bucket)
	if err != nil {
		return nil, nil, err
	}
	lo := append([]byte{ordinal}, prefix...)
	hi := bytes.Clone(lo)
	for len(hi) > 0 && hi[len(hi)-1] == 255 {
		hi = hi[:len(hi)-1]
	}
	if len(hi) == 0 {
		return nil, nil, ErrProjectionCorrupt
	}
	hi[len(hi)-1]++
	return lo, hi, nil
}

// One authenticated range row at a time lets dependent point reads consume the
// same output-byte allowance without losing a range cursor on a partial page.
// The working set includes only the returned bounded page and one lookahead.
func recoveryPage[T any](ctx context.Context, v *authenticatedRecoveryView, bucket string, prefix []byte, cursor string, limits ProjectionPageLimits, decode func(context.Context, []byte, []byte) (T, uint64, int, error)) (page AuthenticatedRecoveryPage[T], resultErr error) {
	if limits.Rows < 1 || limits.Rows > 64 || limits.Bytes < 1 || limits.Bytes > 8<<20 {
		return page, ErrProjectionPageLimit
	}
	c, end, err := v.begin(ctx)
	if err != nil {
		return page, err
	}
	defer end()
	progress := (*recoveryProgress)(nil)
	defer func() {
		resultErr = v.finish(c, resultErr)
		if resultErr != nil {
			page = AuthenticatedRecoveryPage[T]{}
		} else if progress != nil {
			*progress = recoveryProgress{page.Next, page.Done}
		}
	}()
	if bucket == "pending" {
		progress = &v.pending
	}
	if bucket == "active_requests" {
		progress = &v.requests
	}
	if progress != nil && (progress.next != cursor || progress.done) {
		return page, ErrProjectionCursor
	}
	lo, hi, err := recoveryBounds(bucket, prefix)
	if err != nil {
		return page, err
	}
	page.Next = cursor
	for len(page.Items) < limits.Rows {
		raw, err := v.snapshot.Range(c, authenticatedstate.RangeOptions{Lo: lo, Hi: hi, Cursor: page.Next, MaxRows: 1, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return page, err
		}
		if len(raw.Rows) == 0 {
			page.Done = raw.Done
			break
		}
		row := raw.Rows[0]
		if !bytes.HasPrefix(row.Key, lo) {
			return page, ErrProjectionCorrupt
		}
		value, sequence, size, err := decode(c, row.Key[1:], row.Value)
		if err != nil {
			return page, err
		}
		if sequence == 0 || sequence > v.head.Head.Sequence {
			return page, ErrProjectionCorrupt
		}
		size += len(row.Key) + len(row.Value)
		if size > limits.Bytes {
			return page, ErrProjectionPageLimit
		}
		if size > limits.Bytes-page.Bytes {
			break
		}
		page.Items = append(page.Items, value)
		page.Bytes += size
		page.Next = raw.Cursor
		page.Done = raw.Done
		if page.Done {
			break
		}
	}
	// The deferred publication barrier commits progress only with the page.
	return page, nil
}

func (v *authenticatedRecoveryView) PendingLeasePage(ctx context.Context, cursor string, limits ProjectionPageLimits) (AuthenticatedRecoveryPage[PendingLease], error) {
	return recoveryPage(ctx, v, "pending", nil, cursor, limits, func(_ context.Context, k, raw []byte) (PendingLease, uint64, int, error) {
		row, err := recoveryDecode[projectionReservation](raw)
		if err != nil {
			return PendingLease{}, 0, 0, err
		}
		e := row.Evidence.Event
		if validateProjectedReservation(row, e.AttemptID) != nil || validateProjectionRecordPosition(Watermark{row.Evidence.Generation, row.Evidence.Offset, row.Evidence.Sequence}, v.head.Head) != nil || !bytes.Equal(projectionIDKey(row.Reservation.Lease.AttemptID), k) {
			return PendingLease{}, 0, 0, ErrProjectionCorrupt
		}
		return PendingLease{Reservation: row.Reservation.Lease, Started: row.Reservation.Started}, row.Evidence.Sequence, 0, nil
	})
}
func (v *authenticatedRecoveryView) UnfinalizedRequestPage(ctx context.Context, cursor string, limits ProjectionPageLimits) (AuthenticatedRecoveryPage[ProjectedRequest], error) {
	return recoveryPage(ctx, v, "active_requests", nil, cursor, limits, func(c context.Context, k, raw []byte) (ProjectedRequest, uint64, int, error) {
		row, found, size, err := recoveryGet[ProjectedRequest](v, c, "requests", projectionIDKey(string(raw)))
		if err != nil {
			return row, 0, 0, err
		}
		origin, originErr := row.RecoveryOrigin()
		if !found || originErr != nil || validateProjectedRequest(row, string(raw), v.head.Head) != nil || row.Finalized != nil || origin.Event.RequestID != string(raw) || !bytes.Equal(projectionIDKey(origin.Event.RequestID), k) {
			return row, 0, 0, ErrProjectionCorrupt
		}
		return row, origin.Sequence, size, nil
	})
}
func (v *authenticatedRecoveryView) RequestAttemptPage(ctx context.Context, requestID, cursor string, limits ProjectionPageLimits) (AuthenticatedRecoveryPage[ProjectedAttempt], error) {
	if requestID == "" || len(requestID) > maxPayloadSize {
		return AuthenticatedRecoveryPage[ProjectedAttempt]{}, ErrProjectionPageLimit
	}
	return recoveryPage(ctx, v, "attempts_by_request", projectionTuple(requestID), cursor, limits, func(c context.Context, k, raw []byte) (ProjectedAttempt, uint64, int, error) {
		id := string(raw)
		lease, found, size, err := recoveryGet[Record](v, c, "leases", projectionIDKey(id))
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		if !found || validateProjectedLease(lease, id, v.head.Head) != nil || lease.Event.RequestID != requestID || !bytes.Equal(k, projectionTuple(requestID, id)) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		settled, hasSettled, n, err := recoveryGet[SettledAttempt](v, c, "settled", projectionIDKey(id))
		size += n
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		pending, hasPending, n, err := recoveryGet[projectionReservation](v, c, "pending", projectionIDKey(id))
		size += n
		if err != nil {
			return ProjectedAttempt{}, 0, 0, err
		}
		if hasPending == hasSettled || hasPending && (pending.Reservation.Lease.AttemptID != id || pending.Reservation.Lease.RequestID != requestID) || hasSettled && (settled.Settlement.Kind != EventAttemptSettled || settled.Settlement.AttemptID != id || settled.Settlement.RequestID != requestID || settled.Settlement.Validate() != nil) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		if hasPending && (validateProjectedReservation(pending, id) != nil || validateProjectionRecordPosition(Watermark{pending.Evidence.Generation, pending.Evidence.Offset, pending.Evidence.Sequence}, v.head.Head) != nil) {
			return ProjectedAttempt{}, 0, 0, ErrProjectionCorrupt
		}
		item := ProjectedAttempt{Lease: lease, Pending: hasPending}
		if hasSettled {
			if err := validateProjectedSettlement(settled, id); err != nil {
				return ProjectedAttempt{}, 0, 0, err
			}
			position, found, n, err := recoveryGet[Watermark](v, c, "meta", projectionTuple("settlement", id))
			size += n
			if err != nil || !found || validateProjectionRecordPosition(position, v.head.Head) != nil {
				return ProjectedAttempt{}, 0, 0, errors.Join(ErrProjectionCorrupt, err)
			}
			item.Settlement = &settled
		}
		return item, lease.Sequence, size, nil
	})
}
func (v *authenticatedRecoveryView) ReadLease(ctx context.Context, id string) (record Record, found bool, resultErr error) {
	if id == "" || len(id) > maxPayloadSize {
		return record, false, ErrProjectionPageLimit
	}
	c, end, err := v.begin(ctx)
	if err != nil {
		return record, false, err
	}
	defer end()
	record, found, _, resultErr = recoveryGet[Record](v, c, "leases", projectionIDKey(id))
	if resultErr == nil && found {
		resultErr = validateProjectedLease(record, id, v.head.Head)
	}
	resultErr = v.finish(c, resultErr)
	if resultErr != nil {
		return Record{}, false, resultErr
	}
	return record, found, nil
}

// ReadRequest joins a pending input with its ORIGINAL acceptance/legacy origin
// and terminal fact. Later settlements must not move this dependency to a new cut.
func (v *authenticatedRecoveryView) ReadRequest(ctx context.Context, id string) (record ProjectedRequest, found bool, resultErr error) {
	if err := authenticatedPointID(id); err != nil {
		return record, false, err
	}
	c, end, err := v.begin(ctx)
	if err != nil {
		return record, false, err
	}
	defer end()
	record, found, _, resultErr = recoveryGet[ProjectedRequest](v, c, "requests", projectionIDKey(id))
	if resultErr == nil && found {
		resultErr = authenticatedRequestValid(record, id, v.head)
	}
	resultErr = v.finish(c, resultErr)
	if resultErr != nil {
		return ProjectedRequest{}, false, resultErr
	}
	return record, found, nil
}
func (v *authenticatedRecoveryView) Complete(ctx context.Context) error {
	_, end, err := v.begin(ctx)
	if err != nil {
		return err
	}
	defer end()
	if !v.pending.done || !v.requests.done {
		return ErrAuthenticatedRecoveryIncomplete
	}
	if err := v.finish(ctx, nil); err != nil {
		return err
	}
	v.completed = true
	return nil
}

// Cancel is already visible before taking mu. Release mu while joining the
// callback so its final query/cleanup can acquire the owner and retire the pin.
func (p *AuthenticatedLedgerProjection) joinRecovery() {
	p.mu.Lock()
	p.ready = false
	done := p.recoveryDone
	p.mu.Unlock()
	if done != nil {
		<-done
	}
}
