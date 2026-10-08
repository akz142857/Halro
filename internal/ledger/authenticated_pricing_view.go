package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// AuthenticatedPricingView exposes finite pages from one fresh native-built
// root. Its cursor and lease lookups cannot escape the joined callback pin.
type AuthenticatedPricingView interface {
	AccountingLeaseReader
	PendingLeasePage(context.Context, ProjectionCursor, ProjectionPageLimits) (ProjectionPage[PendingLease], error)
	SettlementPage(context.Context, ProjectionCursor, ProjectionPageLimits) (ProjectionPage[SettledAttempt], error)
}

type authenticatedPricingView struct {
	mu          sync.Mutex
	v           *authenticatedRecoveryView
	generation  [16]byte
	settlements recoveryProgress
	failure     error
	active      bool
}

// The offline caller owns the excluded source. This API accepts fresh roots
// only; a selected business root cannot evade its native/role read authority.
func (p *AuthenticatedLedgerProjection) WithFreshPricingView(ctx context.Context, visit func(AuthenticatedPricingView) error) error {
	if p == nil || visit == nil || p.selectedNative.Load() != nil {
		return ErrProjectionSource
	}
	return p.withAuthenticatedPinnedView(ctx, func(v *authenticatedRecoveryView) error {
		if p.selectedNative.Load() != nil {
			return ErrProjectionSource
		}
		r := &authenticatedPricingView{v: v, active: true}
		defer func() {
			r.mu.Lock()
			r.active = false
			r.mu.Unlock()
		}()
		if _, err := rand.Read(r.generation[:]); err != nil {
			return err
		}
		err := visit(r)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.active = false
		err = errors.Join(err, r.failure)
		if p.selectedNative.Load() != nil {
			return errors.Join(err, ErrProjectionSource)
		}
		if err == nil && (!v.pending.done || !r.settlements.done) {
			return ErrAuthenticatedRecoveryIncomplete
		}
		return err
	}, false)
}

func (r *authenticatedPricingView) enter() error {
	if !r.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	if !r.active {
		r.mu.Unlock()
		return ErrProjectionUnavailable
	}
	if r.failure != nil {
		err := r.failure
		r.mu.Unlock()
		return err
	}
	return nil
}

func (r *authenticatedPricingView) ReadPosition(ctx context.Context) (head Watermark, hash [32]byte, err error) {
	if err = r.enter(); err != nil {
		return head, hash, err
	}
	defer r.mu.Unlock()
	c, end, err := r.v.begin(ctx)
	if err != nil {
		r.failure = errors.Join(r.failure, err)
		return head, hash, err
	}
	defer end()
	err = r.v.finish(c, nil)
	r.failure = errors.Join(r.failure, err)
	if err != nil {
		return head, hash, err
	}
	return r.v.head.Head, r.v.head.Hash, nil
}

func (r *authenticatedPricingView) ReadLease(ctx context.Context, id string) (Record, bool, error) {
	if err := r.enter(); err != nil {
		return Record{}, false, err
	}
	defer r.mu.Unlock()
	v, found, err := r.v.ReadLease(ctx, id)
	r.failure = errors.Join(r.failure, err)
	return v, found, err
}

func pricingCursor(r *authenticatedPricingView, cursor ProjectionCursor, kind string, progress recoveryProgress) (string, error) {
	if progress.done {
		return "", ErrProjectionCursor
	}
	if cursor.generation == ([16]byte{}) && cursor.kind == "" && cursor.scope == "" && cursor.head == (Watermark{}) && len(cursor.after) == 0 && progress.next == "" {
		return "", nil
	}
	if cursor.generation != r.generation || cursor.kind != kind || cursor.scope != string(r.v.head.Hash[:]) || cursor.head != r.v.head.Head || string(cursor.after) != progress.next {
		return "", ErrProjectionCursor
	}
	return progress.next, nil
}

func pricingPage[T any](r *authenticatedPricingView, kind string, page AuthenticatedRecoveryPage[T]) ProjectionPage[T] {
	return ProjectionPage[T]{Items: page.Items, Done: page.Done, Bytes: page.Bytes, Scanned: len(page.Items), Next: ProjectionCursor{generation: r.generation, kind: kind, scope: string(r.v.head.Hash[:]), head: r.v.head.Head, after: []byte(page.Next)}}
}

func (r *authenticatedPricingView) PendingLeasePage(ctx context.Context, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[PendingLease], error) {
	if err := r.enter(); err != nil {
		return ProjectionPage[PendingLease]{}, err
	}
	defer r.mu.Unlock()
	position, err := pricingCursor(r, cursor, "pricing-pending", r.v.pending)
	if err != nil {
		r.failure = errors.Join(r.failure, err)
		return ProjectionPage[PendingLease]{}, err
	}
	page, err := r.v.PendingLeasePage(ctx, position, limits)
	r.failure = errors.Join(r.failure, err)
	if err != nil {
		return ProjectionPage[PendingLease]{}, err
	}
	return pricingPage(r, "pricing-pending", page), nil
}

func (r *authenticatedPricingView) SettlementPage(ctx context.Context, cursor ProjectionCursor, limits ProjectionPageLimits) (ProjectionPage[SettledAttempt], error) {
	if err := r.enter(); err != nil {
		return ProjectionPage[SettledAttempt]{}, err
	}
	defer r.mu.Unlock()
	position, err := pricingCursor(r, cursor, "pricing-settled", r.settlements)
	if err != nil {
		r.failure = errors.Join(r.failure, err)
		return ProjectionPage[SettledAttempt]{}, err
	}
	page, err := recoveryPage(ctx, r.v, "settled", nil, position, limits, func(ctx context.Context, k, raw []byte) (SettledAttempt, uint64, int, error) {
		row, err := recoveryDecode[SettledAttempt](raw)
		if err != nil || validateProjectedSettlement(row, row.Settlement.AttemptID) != nil || !bytes.Equal(projectionIDKey(row.Settlement.AttemptID), k) {
			return row, 0, 0, errors.Join(ErrProjectionCorrupt, err)
		}
		position, found, size, err := recoveryGet[Watermark](r.v, ctx, "meta", projectionTuple("settlement", row.Settlement.AttemptID))
		if err != nil || !found || validateProjectionRecordPosition(position, r.v.head.Head) != nil {
			return row, 0, 0, errors.Join(ErrProjectionCorrupt, err)
		}
		return row, position.Sequence, size, nil
	})
	r.failure = errors.Join(r.failure, err)
	if err != nil {
		return ProjectionPage[SettledAttempt]{}, err
	}
	r.settlements = recoveryProgress{next: page.Next, done: page.Done}
	return pricingPage(r, "pricing-settled", page), nil
}
