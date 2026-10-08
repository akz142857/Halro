// Package startupaudit owns a finite readiness grant while native historical
// bytes are being authenticated by an independent bounded reader.
package startupaudit

import (
	"context"
	"errors"
	"sync"
	"time"
)

const MaxDeadline = 15 * time.Minute
const MaxProgressTimeout = 30 * time.Second

var ErrDeadline = errors.New("startup historical audit deadline exceeded")
var ErrProgress = errors.New("startup historical audit made no authenticated progress")
var ErrOptions = errors.New("invalid startup historical audit deadline")

type Options struct {
	Deadline, ProgressTimeout time.Duration
	// ObserveProgress receives only already authenticated scalar progress on
	// the actual reader thread. It supplies no source or readiness authority;
	// failure withdraws the same finite grant and Close joins the callback.
	ObserveProgress func(context.Context, Progress) error
	// OnFailure lets the original composite owner withdraw global business
	// readiness when one of its actual historical readers fails. Called once.
	OnFailure func(error)
}
type Progress struct {
	Records         uint64
	Bytes           int64
	Complete, Ready bool
	Err             error
}
type Owner struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	options       Options
	started, last time.Time
	progress      Progress
	ready, done   chan struct{}
	monitorDone   chan struct{}
	readyOnce     sync.Once
	withdrawOnce  sync.Once
	withdraw      func(error)
}

func Validate(o Options) error {
	if o.Deadline <= 0 || o.Deadline > MaxDeadline || o.ProgressTimeout <= 0 || o.ProgressTimeout > MaxProgressTimeout || o.ProgressTimeout > o.Deadline {
		return ErrOptions
	}
	return nil
}

// Start calls run once. Only run receives the progress capability; a frame
// must already have passed native chain, MAC and payload validation before it
// invokes that capability. Ready never outlives either finite deadline.
func Start(ctx context.Context, o Options, withdraw func(error), run func(context.Context, func(uint64, int64)) error) *Owner {
	jobCtx, cancel := context.WithCancel(ctx)
	now := time.Now()
	a := &Owner{ctx: jobCtx, cancel: cancel, options: o, started: now, last: now, ready: make(chan struct{}), done: make(chan struct{}), monitorDone: make(chan struct{}), withdraw: withdraw}
	go a.monitor()
	go func() {
		err := run(jobCtx, func(records uint64, bytes int64) {
			a.mu.Lock()
			a.checkTimeLocked(time.Now())
			advanced := false
			if a.progress.Err == nil && records > a.progress.Records && bytes > a.progress.Bytes {
				a.progress.Records, a.progress.Bytes, a.last = records, bytes, time.Now()
				a.progress.Ready = true
				a.readyOnce.Do(func() { close(a.ready) })
				advanced = true
			}
			p := a.progress
			a.mu.Unlock()
			if advanced && o.ObserveProgress != nil {
				if err := o.ObserveProgress(jobCtx, p); err != nil {
					a.fail(err)
				}
			}
		})
		if err == nil {
			err = jobCtx.Err()
		}
		if err != nil {
			a.fail(err)
		}
		a.mu.Lock()
		a.checkTimeLocked(time.Now())
		if a.progress.Err == nil {
			a.progress.Complete, a.progress.Ready = true, true
		}
		a.readyOnce.Do(func() { close(a.ready) })
		a.mu.Unlock()
		cancel()
		<-a.monitorDone
		close(a.done)
	}()
	return a
}

func (a *Owner) fail(err error) {
	a.mu.Lock()
	if a.progress.Complete {
		a.mu.Unlock()
		return
	}
	if a.progress.Err == nil {
		a.progress.Err = err
	} else if err != nil && !errors.Is(err, context.Canceled) {
		a.progress.Err = errors.Join(a.progress.Err, err)
	}
	a.progress.Ready = false
	effective := a.progress.Err
	a.readyOnce.Do(func() { close(a.ready) })
	a.mu.Unlock()
	a.cancel()
	a.withdrawOnce.Do(func() {
		if a.withdraw != nil {
			a.withdraw(effective)
		}
		if a.options.OnFailure != nil {
			a.options.OnFailure(effective)
		}
	})
}

// Called with the state mutex held. Expiry is latched before any new
// progress/completion can be accepted. Withdrawal itself belongs to the fixed
// monitor, because admission callers may already hold their native mutex.
func (a *Owner) checkTimeLocked(now time.Time) {
	if a.progress.Complete || a.progress.Err != nil {
		return
	}
	err := a.expired(now)
	if err == nil {
		err = a.ctx.Err()
	}
	if err != nil {
		a.progress.Err, a.progress.Ready = err, false
		a.readyOnce.Do(func() { close(a.ready) })
		a.cancel()
	}
}

func (a *Owner) expired(now time.Time) error {
	if now.Sub(a.started) >= a.options.Deadline {
		return ErrDeadline
	}
	if now.Sub(a.last) >= a.options.ProgressTimeout {
		return ErrProgress
	}
	return nil
}

func (a *Owner) monitor() {
	defer close(a.monitorDone)
	interval := a.options.ProgressTimeout / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-a.ctx.Done():
			a.fail(a.ctx.Err())
			return
		case now := <-t.C:
			a.mu.Lock()
			a.checkTimeLocked(now)
			complete := a.progress.Complete
			err := a.progress.Err
			a.mu.Unlock()
			if !complete && err != nil {
				a.fail(err)
				return
			}
		}
	}
}

func (a *Owner) Snapshot() Progress {
	if a == nil {
		return Progress{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checkTimeLocked(time.Now())
	p := a.progress
	return p
}
func (a *Owner) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ready:
	}
	p := a.Snapshot()
	if !p.Ready {
		return errors.Join(p.Err, context.Canceled)
	}
	return nil
}
func (a *Owner) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
	}
	return a.Snapshot().Err
}
func (a *Owner) Cancel() {
	if a != nil {
		a.cancel()
	}
}

// OnWithdrawal links an admitted native read job to failed/expired audit
// permission. Successful completion also stops the audit control context, so
// that control cancellation must not abort a healthy dependent read job.
func (a *Owner) OnWithdrawal(cancel context.CancelFunc) func() bool {
	return context.AfterFunc(a.ctx, func() {
		if a.Snapshot().Err != nil {
			cancel()
		}
	})
}
