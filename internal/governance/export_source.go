package governance

import (
	"context"
	"crypto/hmac"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
)

var ErrOutcomeExportIncomplete = errors.New("outcome export facts were not completed")
var ErrOutcomeExportClosed = errors.New("outcome export view is closed")

type OutcomeExportPage struct {
	Items []domain.Outcome
	Bytes int
	Done  bool
}

// Sequential reads bind to one immutable native fact prefix. Complete must
// succeed after all reads and before publishing the export manifest.
type OutcomeExportView interface {
	Head() Summary
	ReadPage(context.Context, int, int) (OutcomeExportPage, error)
	Complete(context.Context) error
}

// WithOutcomeExport does not hold m.mu or a DB reader across the callback.
// The caller must own/join Manager operations before closing its resources,
// including the publication callback. No Runtime generation is published here.
func (m *Manager) WithOutcomeExport(ctx context.Context, visit func(OutcomeExportView) error) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if visit == nil {
		return ErrOutcomeExportIncomplete
	}
	m.mu.Lock()
	head, err := m.ownerHead(ctx)
	if err == nil {
		err = m.ensurePageKey()
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	v := &outcomeExportView{m: m, log: m.log, state: m.state, projection: m.projection,
		ctx: ctx, active: true, fence: OutcomeFence{head: head, authentication: outcomeFenceMAC(m.pageKey, head)}}
	m.mu.Unlock()
	defer v.close()
	if err := visit(v); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.completed {
		return errors.Join(ErrOutcomeExportIncomplete, v.failure)
	}
	return nil
}

type outcomeExportView struct {
	mu                      sync.Mutex
	m                       *Manager
	log                     *Log
	state                   *State
	projection              *GovernanceProjection
	ctx                     context.Context
	fence                   OutcomeFence
	after                   string
	seen                    uint64
	active, done, completed bool
	failure                 error
}

func (v *outcomeExportView) Head() Summary { return v.fence.head }
func (v *outcomeExportView) clearOwner() {
	v.active = false
	v.m, v.log, v.state, v.projection, v.ctx = nil, nil, nil, nil, nil
}
func (v *outcomeExportView) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clearOwner()
}

// Called with v.mu held. Every operation verifies both caller contexts and
// the captured owner identity. The Manager's mutex covers one finite read.
func (v *outcomeExportView) read(ctx context.Context, read func() error) error {
	if !v.active || v.m == nil {
		return ErrOutcomeExportClosed
	}
	if v.failure != nil {
		return v.failure
	}
	var err error
	if err = v.ctx.Err(); err == nil {
		err = ctx.Err()
	}
	if err == nil {
		v.m.mu.Lock()
		if v.log != v.m.log || v.state != v.m.state || v.projection != v.m.projection {
			err = ErrUnavailable
		} else {
			var current Summary
			current, err = v.m.ownerHead(ctx)
			if err == nil {
				expected := outcomeFenceMAC(v.m.pageKey, v.fence.head)
				cut := v.fence.head
				if len(v.m.pageKey) == 0 || !hmac.Equal(expected[:], v.fence.authentication[:]) || cut.Records > current.Records || cut.Bytes > current.Bytes || cut.Records == current.Records && cut != current {
					err = ErrOutcomeFence
				}
			}
			if err == nil {
				err = v.ctx.Err()
			}
			if err == nil {
				err = read()
			}
			if err == nil {
				err = v.ctx.Err()
			}
			if err == nil {
				err = ctx.Err()
			}
		}
		v.m.mu.Unlock()
	}
	// A byte limit permits retrying this same unread position with a larger
	// byte allowance. No successful read/count progress occurs on an error.
	if err != nil && !errors.Is(err, ErrOutcomePageLimit) {
		v.failure = err
	}
	return err
}

func (v *outcomeExportView) ReadPage(ctx context.Context, rows, limit int) (OutcomeExportPage, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active || v.done {
		return OutcomeExportPage{}, ErrOutcomeExportClosed
	}
	if rows < 1 || rows > OutcomePageMaxRows || limit < 1 || limit > OutcomePageMaxBytes {
		return OutcomeExportPage{}, ErrOutcomePageLimit
	}
	var page OutcomePage
	var next string
	err := v.read(ctx, func() error {
		if v.seen == v.fence.head.Records {
			page = OutcomePage{Items: []domain.Outcome{}, Head: v.fence.head}
			return nil
		}
		q := OutcomePageQuery{Rows: rows, Bytes: limit}
		var err error
		if v.projection != nil {
			page, next, err = v.projection.readOutcomePage(ctx, q, v.after, v.fence.head)
		} else {
			page, next, err = v.state.readOutcomePage(ctx, q, v.after, v.fence.head)
		}
		if err != nil {
			return err
		}
		seen := v.seen + uint64(len(page.Items))
		if page.Head != v.fence.head || seen > v.fence.head.Records || next == "" && seen != v.fence.head.Records {
			return ErrCorrupt
		}
		return nil
	})
	if err != nil {
		return OutcomeExportPage{}, err
	}
	v.seen += uint64(len(page.Items))
	v.done = v.seen == v.fence.head.Records
	if !v.done && (next == "" || next <= v.after) {
		v.failure = ErrCorrupt
		return OutcomeExportPage{}, v.failure
	}
	v.after = next
	return OutcomeExportPage{Items: page.Items, Bytes: page.Bytes, Done: v.done}, nil
}

func (v *outcomeExportView) Complete(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active {
		return ErrOutcomeExportClosed
	}
	err := v.read(ctx, func() error {
		if !v.done || v.seen != v.fence.head.Records {
			return ErrOutcomeExportIncomplete
		}
		return nil
	})
	v.completed = err == nil
	v.clearOwner()
	return err
}
