package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type Replayer interface {
	Replay(ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error)
}

// CollectorTarget is the one selected reducer owner. Read errors are never
// converted to genesis or redirected to a second Aggregate.
type CollectorTarget interface {
	ApplyContext(context.Context, ledger.Record) error
	ReadWatermark(context.Context) (ledger.Watermark, error)
}

// CollectorReplay must stream authenticated, role-permitted records after from.
// Its returned semantic head must have the same sequence as the target after
// replay. Physical roll boundaries may differ. The caller owns source/role trust.
type CollectorReplay func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error)

type CollectorStats struct {
	QueueDepth    int
	QueueCapacity int
	Dropped       uint64
	Lagging       bool
}

var (
	ErrCollectorRetired = errors.New("usage collector is retired")
	ErrCollectorBusy    = errors.New("usage collector catch-up slots are occupied")
	ErrCollectorHead    = errors.New("usage collector source and reducer heads differ")
)

// Collector queues only wake notifications, never Record payloads or borrowed
// references. Queue overflow is repaired by a streamed authoritative suffix.
// One Run and at most two admitted catch-ups are allowed; source/reducer IO is
// serialized. The single Run may retain one wake while waiting for admission.
// Retirement joins real calls before clearing borrowed owners.
// These are count/lifetime bounds, not shared physical resource fees.
type Collector struct {
	target        CollectorTarget
	replay        CollectorReplay
	queue         chan struct{}
	applyMu       sync.Mutex
	admissionMu   sync.Mutex
	lifeMu        sync.RWMutex
	observeMu     sync.RWMutex
	slots         chan struct{}
	retired       atomic.Bool
	runStarted    atomic.Bool
	notifications atomic.Uint64
	lagging       atomic.Bool
	dropped       atomic.Uint64
	stopCtx       context.Context
	stop          context.CancelFunc
	retireOnce    sync.Once
	retiredDone   chan struct{}
}

func NewCollector(aggregate *Aggregate, replayer Replayer, capacity int) (*Collector, error) {
	if aggregate == nil || replayer == nil {
		return nil, errors.New("aggregate and replayer are required")
	}
	return NewProjectedCollector(aggregate, func(ctx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		if err := ctx.Err(); err != nil {
			return ledger.Watermark{}, err
		}
		return replayer.Replay(from, visit)
	}, capacity)
}

// NewProjectedCollector borrows an explicitly selected target and streaming
// source. It neither closes their files nor grants trust to persisted content.
// All target writes must be serialized with this collector; external projection
// mutation/replacement requires retiring it and constructing a fresh owner.
func NewProjectedCollector(target CollectorTarget, replay CollectorReplay, capacity int) (*Collector, error) {
	if target == nil || replay == nil || capacity < 1 {
		return nil, errors.New("selected target, streaming source, and positive capacity are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Collector{target: target, replay: replay, queue: make(chan struct{}, capacity), slots: make(chan struct{}, 2), stopCtx: ctx, stop: cancel, retiredDone: make(chan struct{})}, nil
}

func (c *Collector) Observe(_ ledger.Record) {
	if c.retired.Load() {
		return
	}
	c.observeMu.RLock()
	defer c.observeMu.RUnlock()
	if c.retired.Load() {
		return
	}
	c.notifications.Add(1)
	c.lagging.Store(true)
	select {
	case c.queue <- struct{}{}:
	default:
		c.dropped.Add(1)
	}
}

func (c *Collector) Run(ctx context.Context) {
	c.admissionMu.Lock()
	if c.retired.Load() || !c.runStarted.CompareAndSwap(false, true) {
		c.admissionMu.Unlock()
		return
	}
	c.lifeMu.RLock()
	c.admissionMu.Unlock()
	defer c.lifeMu.RUnlock()
	if c.retired.Load() {
		return
	}
	for {
		select {
		case <-c.queue:
			// Run already owns lifeMu: do not recursively acquire it while a retiring
			// writer may be queued. Slots still cover its actual source operation.
			err := c.catchUpOwned(ctx)
			if errors.Is(err, ErrCollectorRetired) || ctx.Err() != nil {
				return
			}
			if errors.Is(err, resourcebudget.ErrWorkQueueWait) {
				// Keep the existing bounded wake until a reducer slot is available.
				// A queue timeout must not require another native event to catch up.
				select {
				case c.queue <- struct{}{}:
				default:
				}
			}
		case <-ctx.Done():
			return
		case <-c.stopCtx.Done():
			return
		}
	}
}

func (c *Collector) CatchUp(ctx context.Context) error {
	c.admissionMu.Lock()
	if c.retired.Load() {
		c.admissionMu.Unlock()
		return ErrCollectorRetired
	}
	if err := ctx.Err(); err != nil {
		c.admissionMu.Unlock()
		return err
	}
	// Admission precedes waiting on lifetime/apply locks and temporary work.
	select {
	case c.slots <- struct{}{}:
	default:
		c.admissionMu.Unlock()
		return ErrCollectorBusy
	}
	c.lifeMu.RLock()
	c.admissionMu.Unlock()
	defer func() { <-c.slots; c.lifeMu.RUnlock() }()
	return c.catchUpWithSlot(ctx)
}
func (c *Collector) catchUpOwned(ctx context.Context) error {
	if c.retired.Load() {
		return ErrCollectorRetired
	}
	waitCtx, stopWait := resourcebudget.WorkQueueContext(ctx)
	defer stopWait()
	select {
	case c.slots <- struct{}{}:
	case <-waitCtx.Done():
		return errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	case <-c.stopCtx.Done():
		return ErrCollectorRetired
	}
	defer func() { <-c.slots }()
	if waitCtx.Err() != nil {
		return errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	}
	stopWait()
	return c.catchUpWithSlot(ctx)
}
func (c *Collector) catchUpWithSlot(ctx context.Context) error {
	if err := c.lockApply(ctx); err != nil {
		return err
	}
	defer c.applyMu.Unlock()
	return c.catchUpLocked(ctx)
}

// WithPausedCatchUp joins reducer IO, advances through the actual applied head,
// and excludes all collector writes through visit. Run and its fixed wake queue
// remain alive; no worker or source handle is replaced. The caller has already
// drained complete producers. Do not reenter this Collector from visit.
func (c *Collector) WithPausedCatchUp(ctx context.Context, visit func(context.Context) error) error {
	return c.WithPausedCatchUpWorkspace(ctx, func(ctx context.Context, catchUp func(context.Context) error) error {
		return catchUp(ctx)
	}, visit)
}

// WithPausedCatchUpWorkspace joins accepted reducer IO before borrowing RAM for
// the final catch-up. The loan ends before visit, while the collector's write
// exclusion remains held. Maintenance may then join other producers without
// holding the workspace those producers need. withWorkspace must invoke its
// callback synchronously and join all its physical borrowers before returning.
// The visitor receives the original context, never the expired RAM capability.
func (c *Collector) WithPausedCatchUpWorkspace(ctx context.Context, withWorkspace func(context.Context, func(context.Context) error) error, visit func(context.Context) error) error {
	if withWorkspace == nil || visit == nil {
		return ErrCollectorHead
	}
	c.admissionMu.Lock()
	if c.retired.Load() || ctx.Err() != nil {
		c.admissionMu.Unlock()
		return errors.Join(ErrCollectorRetired, ctx.Err())
	}
	select {
	case c.slots <- struct{}{}:
	default:
		c.admissionMu.Unlock()
		return ErrCollectorBusy
	}
	c.lifeMu.RLock()
	c.admissionMu.Unlock()
	defer func() { <-c.slots; c.lifeMu.RUnlock() }()
	if err := c.lockApply(ctx); err != nil {
		return err
	}
	defer c.applyMu.Unlock()
	if err := c.catchUpInPausedWorkspace(ctx, withWorkspace); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return visit(ctx)
}

// A slot holder waits finitely for the reducer; actual replay/maintenance uses
// the original context. Close still joins the entered IO and every slot owner.
func (c *Collector) lockApply(ctx context.Context) error {
	waitCtx, stop := resourcebudget.WorkQueueContext(ctx)
	defer stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if waitCtx.Err() != nil {
			return errors.Join(waitCtx.Err(), context.Cause(waitCtx))
		}
		if c.stopCtx.Err() != nil {
			return ErrCollectorRetired
		}
		if c.applyMu.TryLock() {
			return nil
		}
		select {
		case <-waitCtx.Done():
		case <-c.stopCtx.Done():
		case <-ticker.C:
		}
	}
}

// The provider receives a finite loan, not an escaped method that could mutate
// the reducer after applyMu has been released. An entered asynchronous borrower
// is joined even if its provider violates the synchronous callback contract.
func (c *Collector) catchUpInPausedWorkspace(ctx context.Context, withWorkspace func(context.Context, func(context.Context) error) error) (resultErr error) {
	var mu sync.Mutex
	var jobs sync.WaitGroup
	active, called := true, false
	var catchUpErr error
	defer func() {
		mu.Lock()
		active = false
		mu.Unlock()
		jobs.Wait()
		mu.Lock()
		defer mu.Unlock()
		resultErr = errors.Join(resultErr, catchUpErr, ctx.Err())
		if !called && resultErr == nil {
			resultErr = ErrCollectorHead
		}
	}()
	return withWorkspace(ctx, func(jobCtx context.Context) (err error) {
		if jobCtx == nil {
			return ErrCollectorHead
		}
		mu.Lock()
		if !active || called {
			mu.Unlock()
			return ErrCollectorBusy
		}
		called = true
		jobs.Add(1)
		mu.Unlock()
		defer func() {
			mu.Lock()
			catchUpErr = err
			mu.Unlock()
			jobs.Done()
		}()
		jobCtx, cancel := context.WithCancel(jobCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer func() { stop(); cancel() }()
		if ctx.Err() != nil {
			cancel()
		}
		return c.catchUpLocked(jobCtx)
	})
}

func (c *Collector) catchUpLocked(ctx context.Context) error {
	if c.retired.Load() {
		return ErrCollectorRetired
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.stopCtx, cancel)
	defer func() { stop(); cancel() }()
	c.lagging.Store(true)
	if err := ctx.Err(); err != nil {
		return err
	}
	baseline := c.notifications.Load()
	c.drainNotifications()
	from, err := c.target.ReadWatermark(ctx)
	if err != nil {
		return err
	}
	head, err := c.replay(ctx, from, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return c.target.ApplyContext(ctx, record)
	})
	if err != nil {
		return err
	}
	actual, err := c.target.ReadWatermark(ctx)
	if err != nil {
		return err
	}
	if head.Sequence != actual.Sequence || actual.Sequence < from.Sequence {
		return ErrCollectorHead
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.retired.Load() {
		return ErrCollectorRetired
	}
	c.lagging.Store(false)
	if c.notifications.Load() != baseline {
		c.lagging.Store(true)
	}
	// One authoritative scan is the read barrier. A later notification keeps
	// lagging=true for the next scan, rather than extending this job forever.
	return nil
}
func (c *Collector) drainNotifications() {
	for i := 0; i < cap(c.queue); i++ {
		select {
		case <-c.queue:
		default:
			return
		}
	}
}

// Retire permanently withdraws Observe/Run/CatchUp. A timeout only ends this
// caller's wait: the single joiner keeps source/target references until actual
// worker/replay/reducer IO exits. Call before closing borrowed dependencies.
func (c *Collector) Retire(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.retireOnce.Do(func() {
		// Publish withdrawal only after every previously admitted caller owns
		// its lifetime ticket. The joiner cannot overtake an admitted waiter.
		c.admissionMu.Lock()
		c.retired.Store(true)
		c.stop()
		c.admissionMu.Unlock()
		go func() {
			c.lifeMu.Lock()
			defer c.lifeMu.Unlock()
			c.observeMu.Lock()
			defer c.observeMu.Unlock()
			c.drainNotifications()
			c.target = nil
			c.replay = nil
			close(c.retiredDone)
		}()
	})
	select {
	case <-c.retiredDone:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Collector) Stats() CollectorStats {
	return CollectorStats{QueueDepth: len(c.queue), QueueCapacity: cap(c.queue), Dropped: c.dropped.Load(), Lagging: c.lagging.Load()}
}
