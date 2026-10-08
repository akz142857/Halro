package authenticatedstate

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

var (
	ErrAuditDeadline = errors.New("authenticated content audit maximum duration exceeded")
	ErrAuditStalled  = errors.New("authenticated content audit made no progress within deadline")
)

// AuditOptions has no production defaults. The owner must choose independently
// accepted limits; progress means actual authenticated node/value reads, never
// a callback heartbeat. Progress must not reenter this pinned Snapshot.
type AuditOptions struct {
	MaxDuration, MaxNoProgress time.Duration
	Progress                   func(AuditProgress) error
}
type AuditProgress struct {
	Rows, Nodes, ValueBytes, ReadBytes uint64
}
type AuditReceipt struct {
	Root     RootIdentity
	Verified AuditProgress
}

func (o AuditOptions) valid() bool {
	return o.MaxDuration > 0 && o.MaxNoProgress > 0 && o.MaxNoProgress <= o.MaxDuration
}

// Audit authenticates every reachable node/value of the pinned root, with a
// height/fanout-bounded DFS stack and one value. It does not read orphan pages,
// other roots or native log history and grants no resume/role/business credit.
func (s *Snapshot) Audit(ctx context.Context, o AuditOptions) (result AuditProgress, resultErr error) {
	if !o.valid() {
		return result, ErrLimit
	}
	if !s.mu.TryLock() {
		return result, ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var permit resourcebudget.Permit
	acquired := false
	defer func() {
		if acquired {
			s.end(ctx, permit, &resultErr)
		} else if errors.Is(resultErr, ErrAuditDeadline) || errors.Is(resultErr, ErrAuditStalled) {
			s.mu.Lock()
			s.errorState = resultErr
			s.mu.Unlock()
		}
		if resultErr != nil {
			result = AuditProgress{}
		}
	}()
	started := time.Now()
	var lastProgress atomic.Int64
	var watchFailure atomic.Pointer[storeFault]
	failExpired := func() error {
		elapsed := time.Since(started)
		idle := elapsed - time.Duration(lastProgress.Load())
		var err error
		if elapsed >= o.MaxDuration {
			err = ErrAuditDeadline
		} else if idle >= o.MaxNoProgress {
			err = ErrAuditStalled
		}
		if err != nil && watchFailure.CompareAndSwap(nil, &storeFault{err}) {
			// Never wait on the pin mutex held by actual IO.
			if s.onCorrupt != nil {
				s.onCorrupt(err)
			}
			cancel()
		}
		if f := watchFailure.Load(); f != nil {
			return f.err
		}
		return nil
	}
	stopWatch, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		timer := time.NewTimer(min(o.MaxDuration, o.MaxNoProgress))
		defer timer.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-timer.C:
			}
			if failExpired() != nil {
				return
			}
			elapsed := time.Since(started)
			idle := elapsed - time.Duration(lastProgress.Load())
			timer.Reset(max(time.Nanosecond, min(o.MaxDuration-elapsed, o.MaxNoProgress-idle)))
		}
	}()
	defer func() {
		close(stopWatch)
		<-watchDone
		// The scheduler cannot turn an overdue audit into a successful receipt.
		resultErr = errors.Join(resultErr, failExpired())
	}()
	var err error
	permit, err = s.beginLocked(ctx)
	if err != nil {
		return result, err
	}
	acquired = true
	var verified AuditProgress
	add := func(target *uint64, value uint64) error {
		if value > math.MaxUint64-*target {
			return ErrLimit
		}
		*target += value
		return nil
	}
	progress := func() error {
		if err := failExpired(); err != nil {
			return err
		}
		lastProgress.Store(time.Since(started).Nanoseconds())
		if err := ctx.Err(); err != nil {
			return err
		}
		if o.Progress != nil {
			if err := o.Progress(verified); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	var walk func(descriptor) error
	walk = func(d descriptor) error {
		q := query{ctx: ctx, snapshot: s, maxReads: 1}
		n, err := q.load(d)
		if err != nil {
			return err
		}
		if err := add(&verified.Nodes, 1); err != nil {
			return err
		}
		if err := add(&verified.ReadBytes, uint64(d.ref.length)); err != nil {
			return err
		}
		if err := progress(); err != nil {
			return err
		}
		if n.height == 1 {
			q.reads = 0
			value, err := q.value(n.value)
			if err != nil {
				return err
			}
			if err := add(&verified.Rows, 1); err != nil {
				return err
			}
			if err := add(&verified.ValueBytes, uint64(len(value))); err != nil {
				return err
			}
			if err := add(&verified.ReadBytes, uint64(n.value.length)); err != nil {
				return err
			}
			return progress()
		}
		for _, child := range n.children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if s.current.count > 0 {
		if err := walk(descriptor{count: s.current.count, height: s.current.height, ref: s.current.root}); err != nil {
			return result, err
		}
	} else if err := progress(); err != nil {
		return result, err
	}
	if verified.Rows != s.current.count {
		return result, ErrCorrupt
	}
	return verified, nil
}

// Audit retains one fixed Store pin and audits its exact root while unrelated
// updates may publish successors. Close cancels and joins the complete actual
// job, including a blocked progress callback or filesystem IO, before retiring
// the pin/workspace. Mandatory timeout/progress failures poison every Store pin.
func (s *Store) Audit(ctx context.Context, o AuditOptions) (result AuditReceipt, resultErr error) {
	if !o.valid() {
		return result, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !s.mu.TryLock() {
		return result, ErrBusy
	}
	if s.auditCancel != nil {
		s.mu.Unlock()
		return result, ErrBusy
	}
	pin, err := s.snapshotLocked(ctx)
	if err != nil {
		s.mu.Unlock()
		return result, err
	}
	root := RootIdentity{Scope: s.current.content.context, Cut: s.current.cut, Digest: s.rootDigest, ID: s.current.content.id, Revision: s.current.revision, Rows: s.current.content.count}
	ctx, cancel := context.WithCancel(ctx)
	s.auditCancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		resultErr = errors.Join(resultErr, pin.Close())
		s.mu.Lock()
		s.auditCancel = nil
		closing := s.closing || s.closed
		s.mu.Unlock()
		if closing {
			resultErr = errors.Join(resultErr, ErrClosed)
		}
		if resultErr != nil {
			result = AuditReceipt{}
		}
	}()
	verified, err := pin.Audit(ctx, o)
	if err != nil {
		return result, err
	}
	return AuditReceipt{Root: root, Verified: verified}, nil
}
