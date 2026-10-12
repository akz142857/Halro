package usage

import (
	"bytes"
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
	bbolt "go.etcd.io/bbolt"
)

// Private facade keeps the ordinary and authenticated reducers/queries shared.
// The callback's first authentication failure is sticky and checked before any
// result or mutation is published, including callers ignoring a cursor error.
type usageEngine struct {
	hotScratch      *scratchstate.RowBuffer
	viewLane        chan struct{}
	bolt            *bbolt.DB
	scratch         *scratchstate.Store
	cow             *authenticatedstate.Store
	ctx             context.Context
	native          *ledger.Log
	lease           *ledger.MaintenanceLease
	owner           *ledger.AuthenticatedLedgerProjection
	budget          *resourcebudget.Manager
	identity        [32]byte
	live            bool
	selectedStartup bool
	selectedHA      bool
	pinned          func() // Test only; nil in production.
}

type usageTx struct {
	hotScratch  *scratchstate.RowBuffer
	rankScratch *rankScratchBatch
	bolt        *bbolt.Tx
	scratch     *scratchstate.Transaction
	pin         *authenticatedstate.Snapshot
	update      *authenticatedstate.Transaction
	ctx         context.Context
	failure     error
}

type usageBucket struct {
	tx   *usageTx
	bolt *bbolt.Bucket
	name string
}
type usageCursor struct {
	bucket   *usageBucket
	bolt     *bbolt.Cursor
	current  []byte
	position int
}

func usageTransaction(input any) *usageTx {
	switch tx := input.(type) {
	case *usageTx:
		return tx
	case *bbolt.Tx:
		return &usageTx{bolt: tx, ctx: context.Background()}
	default:
		return &usageTx{ctx: context.Background(), failure: ErrDiskUsageCorrupt}
	}
}
func usageBucketOf(input any) *usageBucket {
	switch b := input.(type) {
	case *usageBucket:
		return b
	case *bbolt.Bucket:
		return &usageBucket{bolt: b, tx: &usageTx{ctx: context.Background()}}
	default:
		return &usageBucket{tx: &usageTx{ctx: context.Background(), failure: ErrDiskUsageCorrupt}}
	}
}
func wrapUsageDatabase(input any) *usageEngine {
	if e, ok := input.(*usageEngine); ok {
		return e
	}
	db, ok := input.(*bbolt.DB)
	if !ok {
		return &usageEngine{ctx: context.Background()}
	}

	return &usageEngine{bolt: db, ctx: context.Background()}
}
func (e *usageEngine) scoped(ctx context.Context) *usageEngine { out := *e; out.ctx = ctx; return &out }
func (e *usageEngine) View(fn func(*usageTx) error) (resultErr error) {
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if e.bolt != nil {
		return e.bolt.View(func(tx *bbolt.Tx) error {
			row := usageTransaction(tx)
			row.ctx = ctx
			return errors.Join(fn(row), row.failure, ctx.Err())
		})
	}
	if e.scratch != nil {
		return e.scratch.View(ctx, func(tx *scratchstate.Transaction) error {
			row := &usageTx{scratch: tx, ctx: ctx}
			return errors.Join(fn(row), row.failure, ctx.Err())
		})
	}
	if e.cow == nil {
		return ErrDiskUsageClosed
	}
	frozenHA := e.selectedHA && e.live && e.native != nil && e.lease != nil && e.native.ValidateHASelectedFrozenMaintenanceRead(ctx, e.lease) == nil
	if frozenHA {
		defer func() {
			resultErr = errors.Join(resultErr, e.native.ValidateHASelectedFrozenMaintenanceRead(ctx, e.lease))
		}()
	}
	if e.selectedHA && e.live && !frozenHA {
		scope, ok := ctx.Value(typedLiveSourceKey{}).(*typedLiveSourceScope)
		if !ok || scope.cow != e.cow || scope.native != e.native || scope.owner != e.owner {
			if e.owner == nil || e.native == nil || e.budget == nil {
				return ErrDiskUsageClosed
			}
			ref, err := e.owner.ReadUsageSourceReference(ctx)
			if err != nil {
				return err
			}
			return e.owner.WithHASelectedFrozenUsageSource(ctx, e.native, e.budget, ref.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
				ctx = context.WithValue(ctx, typedLiveSourceKey{}, &typedLiveSourceScope{cow: e.cow, native: e.native, owner: e.owner, source: source})
				return e.scoped(ctx).View(fn)
			})
		}
	}
	if e.viewLane != nil {
		if err := resourcebudget.AcquireWorkLane(ctx, e.viewLane); err != nil {
			return err
		}
		defer func() { <-e.viewLane }()
	}
	if e.native == nil || e.lease == nil {
		return ErrDiskUsageClosed
	}
	if e.selectedStartup {
		if err := e.native.ValidateSelectedStartupRead(ctx); err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, e.native.ValidateSelectedStartupRead(ctx)) }()
	}
	if e.live && !frozenHA {
		if scope, ok := ctx.Value(typedLiveSourceKey{}).(*typedLiveSourceScope); ok && scope.cow == e.cow && scope.native == e.native && scope.owner == e.owner {
			if err := scope.source.Check(ctx); err != nil {
				return err
			}
		} else if e.selectedHA {
			return ErrDiskUsageClosed
		} else if err := e.native.ValidateStandaloneReadSource(ctx); err != nil {
			return errors.Join(ErrDiskUsageClosed, err)
		}
	} else if _, err := e.lease.ValidateOwner(e.native); err != nil {
		return errors.Join(ErrDiskUsageClosed, err)
	}
	pin, err := e.cow.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	if e.pinned != nil {
		e.pinned()
	}
	tx := &usageTx{pin: pin, ctx: ctx}
	marker, found, err := pin.Get(ctx, []byte{0})
	if usageReadRefused(err) {
		return err
	}
	if err != nil || !found || !bytes.Equal(marker, []byte{byte(TypedUsageSealCodec)}) {
		return errors.Join(ErrDiskUsageCorrupt, err)
	}
	state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
	if usageReadRefused(tx.failure) {
		return tx.failure
	}
	if err != nil || !found || state.Version != diskUsageVersion {
		return errors.Join(ErrDiskUsageCorrupt, err, tx.failure)
	}
	identity, err := e.cow.RootIdentity(ctx)
	if err != nil || identity.Cut != typedUsageCut(usageStateAnchor(state)) {
		return errors.Join(ErrDiskUsageCorrupt, err)
	}
	callbackErr := fn(tx)
	if usageReadRefused(tx.failure) {
		return errors.Join(tx.failure, ctx.Err())
	}
	return errors.Join(callbackErr, tx.failure, ctx.Err())
}

// usageReadRefused reports a read that failed without reading the rows, such
// as a pressure refusal. Whatever the caller concluded from the missing row
// is not evidence of corruption, so the refusal is reported alone.
func usageReadRefused(err error) bool {
	return err != nil && !errors.Is(err, ErrDiskUsageCorrupt) && !errors.Is(err, authenticatedstate.ErrCorrupt)
}
func (e *usageEngine) Update(fn func(*usageTx) error) error {
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if e.bolt != nil {
		return e.bolt.Update(func(tx *bbolt.Tx) error {
			row := usageTransaction(tx)
			row.ctx = ctx
			return errors.Join(fn(row), row.failure, ctx.Err())
		})
	}
	if e.scratch != nil {
		return e.scratch.Update(ctx, func(tx *scratchstate.Transaction) error {
			row := &usageTx{scratch: tx, ctx: ctx}
			defer func() {
				if row.hotScratch != nil {
					row.hotScratch.Reset()
				}
			}()
			err := fn(row)
			if err == nil && row.failure == nil && ctx.Err() == nil && row.hotScratch != nil {
				err = row.hotScratch.Flush(tx)
			}
			return errors.Join(err, row.failure, ctx.Err())
		})
	}
	// Native reduction uses the private verified-frame entry point; ordinary
	// callbacks cannot manufacture a durable native cut or maintenance proof.
	return ErrDiskUsageClosed
}
func (tx *usageTx) fail(err error) error {
	if tx.failure == nil {
		tx.failure = err
	}
	return err
}
func typedUsageKey(name string, key []byte) ([]byte, error) {
	for i, n := range diskUsageBuckets {
		if n == name && len(key) > 0 && len(key)+2 <= authenticatedstate.MaxKeyBytes {
			return append([]byte{1, byte(i)}, key...), nil
		}
	}
	return nil, ErrDiskUsageCorrupt
}
func (tx *usageTx) Bucket(name []byte) *usageBucket {
	if tx.bolt != nil {
		b := tx.bolt.Bucket(name)
		if b == nil {
			return nil
		}
		return &usageBucket{tx: tx, bolt: b, name: string(name)}
	}
	for _, n := range diskUsageBuckets {
		if n == string(name) {
			return &usageBucket{tx: tx, name: n}
		}
	}
	tx.fail(ErrDiskUsageCorrupt)
	return nil
}
func (tx *usageTx) CreateBucket(name []byte) (*usageBucket, error) {
	if tx.bolt == nil {
		return nil, tx.fail(ErrDiskUsageCorrupt)
	}
	b, err := tx.bolt.CreateBucket(name)
	if err != nil {
		return nil, err
	}
	return &usageBucket{tx: tx, bolt: b, name: string(name)}, nil
}
func (b *usageBucket) Get(key []byte) []byte {
	if b.bolt != nil {
		return b.bolt.Get(key)
	}
	if b.tx.failure != nil {
		return nil
	}
	k, err := typedUsageKey(b.name, key)
	if err != nil {
		b.tx.fail(err)
		return nil
	}
	var v []byte
	var found bool
	if b.tx.scratch != nil {
		if b.tx.hotScratch != nil && len(k) <= 256 {
			v, found, err = b.tx.hotScratch.Get(b.tx.scratch, k)
		} else {
			v, found, err = b.tx.scratch.Get(k)
		}
	} else if b.tx.update != nil {
		v, found, err = b.tx.update.Get(k)
	} else if b.tx.pin != nil {
		v, found, err = b.tx.pin.Get(b.tx.ctx, k)
	} else {
		err = ErrDiskUsageCorrupt
	}
	if err != nil {
		b.tx.fail(err)
		return nil
	}
	if !found {
		return nil
	}
	return v
}
func (b *usageBucket) Put(key, value []byte) error {
	if b.bolt != nil {
		return b.bolt.Put(key, value)
	}
	if b.tx.failure != nil {
		return b.tx.failure
	}
	k, err := typedUsageKey(b.name, key)
	if err == nil && b.tx.scratch != nil {
		if b.tx.hotScratch != nil && len(k) <= 256 {
			return b.tx.fail(b.tx.hotScratch.Put(b.tx.scratch, k, value))
		}
		return b.tx.fail(b.tx.scratch.Put(k, value))
	}
	if err != nil || b.tx.update == nil {
		return b.tx.fail(errors.Join(ErrDiskUsageCorrupt, err))
	}
	return b.tx.fail(b.tx.update.Put(k, value))
}
func (b *usageBucket) Delete(key []byte) error {
	if b.bolt != nil {
		return b.bolt.Delete(key)
	}
	if b.tx.failure != nil {
		return b.tx.failure
	}
	k, err := typedUsageKey(b.name, key)
	if err == nil && b.tx.scratch != nil {
		if b.tx.hotScratch != nil && len(k) <= 256 {
			return b.tx.fail(b.tx.hotScratch.Delete(b.tx.scratch, k))
		}
		return b.tx.fail(b.tx.scratch.Delete(k))
	}
	if err != nil || b.tx.update == nil {
		return b.tx.fail(errors.Join(ErrDiskUsageCorrupt, err))
	}
	return b.tx.fail(b.tx.update.Delete(k))
}
func (b *usageBucket) Cursor() *usageCursor {
	c := &usageCursor{bucket: b}
	if b.bolt != nil {
		c.bolt = b.bolt.Cursor()
	}
	return c
}
func (c *usageCursor) seek(key []byte, reverse, exclusive bool) ([]byte, []byte) {
	tx := c.bucket.tx
	if tx.failure != nil {
		return nil, nil
	}
	lo, err := typedUsageKey(c.bucket.name, []byte{0})
	if err != nil {
		tx.fail(err)
		return nil, nil
	}
	lo = lo[:2]
	hi := []byte{1, lo[1] + 1}
	full := append(bytes.Clone(lo), key...)
	if reverse && key == nil {
		full = hi
		exclusive = true
	}
	var row authenticatedstate.Row
	var found bool
	if tx.scratch != nil {
		if tx.hotScratch != nil {
			if err := tx.hotScratch.Flush(tx.scratch); err != nil {
				tx.fail(err)
				return nil, nil
			}
		}
		var fresh scratchstate.Row
		switch {
		case reverse && exclusive:
			fresh, found, err = tx.scratch.Before(full)
		case reverse:
			fresh, found, err = tx.scratch.Floor(full)
		case exclusive:
			fresh, found, err = tx.scratch.After(full)
		default:
			fresh, found, err = tx.scratch.Seek(full)
		}
		row = authenticatedstate.Row{Key: fresh.Key, Value: fresh.Value}
	} else if tx.pin != nil {
		row, found, err = tx.pin.Seek(tx.ctx, authenticatedstate.SeekOptions{Key: full, Reverse: reverse, Exclusive: exclusive})
	} else if tx.update != nil {
		row, found, err = tx.update.Seek(authenticatedstate.SeekOptions{Key: full, Reverse: reverse, Exclusive: exclusive})
	} else {
		err = ErrDiskUsageCorrupt
	}
	if err != nil {
		tx.fail(err)
		return nil, nil
	}
	if !found || len(row.Key) < 3 || !bytes.Equal(row.Key[:2], lo) {
		c.current = nil
		c.position = 1
		if reverse {
			c.position = -1
		}
		return nil, nil
	}
	c.current = bytes.Clone(row.Key[2:])
	c.position = 0
	return row.Key[2:], row.Value
}
func (c *usageCursor) First() ([]byte, []byte) {
	if c.bolt != nil {
		return c.bolt.First()
	}
	return c.seek(nil, false, false)
}
func (c *usageCursor) Last() ([]byte, []byte) {
	if c.bolt != nil {
		return c.bolt.Last()
	}
	return c.seek(nil, true, false)
}
func (c *usageCursor) Seek(k []byte) ([]byte, []byte) {
	if c.bolt != nil {
		return c.bolt.Seek(k)
	}
	return c.seek(k, false, false)
}
func (c *usageCursor) Next() ([]byte, []byte) {
	if c.bolt != nil {
		return c.bolt.Next()
	}
	if c.position > 0 {
		return nil, nil
	}
	return c.seek(c.current, false, c.current != nil)
}
func (c *usageCursor) Prev() ([]byte, []byte) {
	if c.bolt != nil {
		return c.bolt.Prev()
	}
	if c.position < 0 {
		return nil, nil
	}
	if c.current == nil {
		return c.Last()
	}
	return c.seek(c.current, true, true)
}

func (tx *usageTx) Size() int64 {
	if tx.bolt != nil {
		return tx.bolt.Size()
	}
	if tx.pin != nil {
		size, err := tx.pin.CommittedBytes(tx.ctx)
		tx.fail(err)
		return size
	}
	tx.fail(ErrDiskUsageCorrupt)
	return 0
}

// Called only by the private window reducer after its existing independently
// authenticated export proof. Neither this facade nor a saved content root
// grants archive deletion/retention permission.
func (p *DiskProjection) updateWindowRowsWithLease(ctx context.Context, lease *ledger.MaintenanceLease, fn func(*usageTx) error) error {
	e := p.engine.scoped(ctx)
	if e.bolt != nil {
		return e.Update(fn)
	}
	if e.cow == nil {
		return ErrDiskUsageClosed
	}
	var anchor ledger.ReplayAnchor
	if err := e.View(func(tx *usageTx) error {
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		anchor = usageStateAnchor(state)
		return nil
	}); err != nil {
		return err
	}
	apply := func(ctx context.Context) error {
		_, err := e.cow.UpdateDerived(ctx, typedUsageCut(anchor), func(update *authenticatedstate.Transaction) error {
			tx := &usageTx{update: update, ctx: ctx}
			err := fn(tx)
			state, found, stateErr := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if !found || usageStateAnchor(state) != anchor {
				stateErr = errors.Join(stateErr, ErrDiskUsageCorrupt)
			}
			return errors.Join(err, tx.failure, stateErr, ctx.Err())
		}, func() error { return ctx.Err() })
		return err
	}
	if p.physical != nil {
		return p.updatePhysicalUsageWindow(ctx, lease, anchor, apply)
	}
	if lease != nil {
		if _, err := lease.ValidateOwner(e.native); err != nil {
			return err
		}
	}
	return apply(ctx)
}
