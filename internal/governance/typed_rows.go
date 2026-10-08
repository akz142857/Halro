package governance

import (
	"bytes"
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
	bbolt "go.etcd.io/bbolt"
)

const typedGovernanceBatchRows = 8 // six indexes/facts per record plus position < 64 mutations

var typedGovernanceMarker = []byte{2}

// govRows is private to one immutable pin or one scoped COW transaction. The
// ordinary projection uses the same reducer through its existing bbolt Tx.
type govRows struct {
	ctx      context.Context
	scratch  *scratchstate.Transaction
	snapshot *authenticatedstate.Snapshot
	update   *authenticatedstate.Transaction
}

func typedGovernanceKey(bucket string, key []byte) ([]byte, error) {
	for i, name := range governanceProjectionBuckets {
		if name == bucket && len(key) > 0 && len(key)+2 <= authenticatedstate.MaxKeyBytes {
			return append([]byte{1, byte(i)}, key...), nil
		}
	}
	return nil, ErrCorrupt
}

func (r *govRows) get(key []byte) ([]byte, bool, error) {
	if r.scratch != nil {
		return r.scratch.Get(key)
	}
	if r.update != nil {
		return r.update.Get(key)
	}
	if r.snapshot != nil {
		return r.snapshot.Get(r.ctx, key)
	}
	return nil, false, ErrCorrupt
}

func govRawGet(tx any, bucket string, key []byte) ([]byte, bool, error) {
	switch tx := tx.(type) {
	case *bbolt.Tx:
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil, false, ErrCorrupt
		}
		v := b.Get(key)
		return v, v != nil, nil
	case *govRows:
		k, err := typedGovernanceKey(bucket, key)
		if err != nil {
			return nil, false, err
		}
		return tx.get(k)
	default:
		return nil, false, ErrCorrupt
	}
}

func govRawPut(tx any, bucket string, key, value []byte) error {
	switch tx := tx.(type) {
	case *bbolt.Tx:
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return ErrCorrupt
		}
		return b.Put(key, value)
	case *govRows:
		k, err := typedGovernanceKey(bucket, key)
		if err != nil {
			return err
		}
		if tx.scratch != nil {
			return tx.scratch.Put(k, value)
		}
		if tx.update == nil {
			return errors.Join(ErrCorrupt, err)
		}
		return tx.update.Put(k, value)
	default:
		return ErrCorrupt
	}
}

// No historic slice or cursor cache: one physical index row per authenticated
// Range, with the exact pin's continuation; old and typed paths share stopping
// and semantic checks in readOutcomePage.
func govWalkRows(tx any, bucket string, prefix []byte, after string, visit func([]byte, []byte) (bool, error)) error {
	seek := append(bytes.Clone(prefix), after...)
	switch tx := tx.(type) {
	case *bbolt.Tx:
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return ErrCorrupt
		}
		c := b.Cursor()
		k, v := c.Seek(seek)
		if bytes.Equal(k, seek) {
			k, v = c.Next()
		}
		for k != nil && bytes.HasPrefix(k, prefix) {
			stop, err := visit(k, v)
			if err != nil || stop {
				return err
			}
			k, v = c.Next()
		}
		return nil
	case *govRows:
		if tx.snapshot == nil {
			return ErrCorrupt
		}
		// The next namespace ordinal is the exclusive upper bound. Filtering
		// the hashed scope below preserves the legacy raw key ordering.
		lo, err := typedGovernanceKey(bucket, seek)
		if len(seek) == 0 {
			lo, err = typedGovernanceKey(bucket, []byte{0})
			if err == nil {
				lo = lo[:2]
			}
		}
		if err != nil {
			return err
		}
		hi := []byte{1, lo[1] + 1}
		opts := authenticatedstate.RangeOptions{Lo: lo, Hi: hi, MaxRows: 1, MaxBytes: governanceProjectionMaxRow + authenticatedstate.MaxKeyBytes, MaxPages: 4096}
		for {
			page, err := tx.snapshot.Range(tx.ctx, opts)
			if err != nil {
				return err
			}
			if len(page.Rows) > 0 {
				row := page.Rows[0]
				if len(row.Key) < 3 || row.Key[0] != 1 || row.Key[1] != lo[1] {
					return ErrCorrupt
				}
				k := row.Key[2:]
				if !bytes.HasPrefix(k, prefix) {
					return nil
				}
				if !bytes.Equal(k, seek) {
					stop, err := visit(k, row.Value)
					if err != nil || stop {
						return err
					}
				}
			}
			if page.Done {
				return nil
			}
			if page.Cursor == "" || page.Cursor == opts.Cursor {
				return ErrCorrupt
			}
			opts.Cursor = page.Cursor
		}
	default:
		return ErrCorrupt
	}
}

func (p *GovernanceProjection) viewRows(ctx context.Context, visit func(any, Summary) error) (resultErr error) {
	if p.selectedStartup {
		if err := p.cowNative.ValidateSelectedStartupRead(ctx); err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, p.cowNative.ValidateSelectedStartupRead(ctx)) }()
	}
	if p.cow == nil {
		return p.db.View(func(tx *bbolt.Tx) error {
			head, err := govPosition(tx)
			if err != nil {
				return err
			}
			return visit(tx, head)
		})
	}
	select {
	case p.cowViews <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.cowViews }()
	pin, err := p.cow.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	rows := &govRows{ctx: ctx, snapshot: pin}
	head, err := govPosition(rows)
	if err != nil {
		return err
	}
	identity, err := p.cow.RootIdentity(ctx)
	if err != nil || identity.Cut != typedGovernanceCut(head) {
		return errors.Join(ErrCorrupt, err)
	}
	return visit(rows, head)
}

func (p *GovernanceProjection) readRows(ctx context.Context, visit func(any, Summary) error) (resultErr error) {
	if p.haReadCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(p.haReadCtx, cancel)
		defer func() { stop(); cancel() }()
	}
	if p.cow != nil {
		if p.cowReaders.Add(1) > 16 {
			p.cowReaders.Add(-1)
			return ErrUnavailable
		}
		defer p.cowReaders.Add(-1)
		fee, err := p.cowBudget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, fee.Release()) }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.retired.Load() {
		return ErrUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.retired.Load() || !p.ready || p.db == nil && p.cow == nil {
		return ErrUnavailable
	}
	read := func(ctx context.Context, permitted *HASelectedNativePosition) error {
		return p.viewRows(ctx, func(tx any, head Summary) error {
			if permitted != nil && (head.Records > permitted.Sequence || head.Records == permitted.Sequence && head.LastHash != permitted.Hash) {
				return ErrUnavailable
			}
			if err := visit(tx, head); err != nil {
				return err
			}
			return ctx.Err()
		})
	}
	if p.selectedStartup && p.cowNative.startupProvenance.ha != nil {
		return p.cowNative.startupProvenance.ha.withReadScope(ctx, func(ctx context.Context, permitted HASelectedNativePosition) error { return read(ctx, &permitted) })
	}
	return read(ctx, nil)
}
