package usage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/ledger"
)

// LedgerDerivativeOwner is the same permitted read/reducer owner selected by
// the caller. Its durable event index always takes precedence over the current
// borrowed record. This interface grants no native or role authorization.
type LedgerDerivativeOwner interface {
	FinalizationReader
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
}

var ErrLedgerDerivativeRetired = errors.New("usage Ledger derivative is retired")

// LedgerDerivative supports the existing derivative-before-Ledger-Apply order.
// It borrows only the current authenticated event identity and physical position,
// never a historical ID set. The caller authenticates/orders records and joins
// all Apply operations before retiring the owner. Use Aggregate for observations
// and checkpoints; all live writes must use this derivative's ApplyContext.
type LedgerDerivative struct {
	retired      atomic.Bool
	ownerMu      sync.RWMutex
	mu           sync.Mutex
	replayMu     sync.Mutex
	replayCancel context.CancelFunc
	owner        LedgerDerivativeOwner
	aggregate    *Aggregate
	disk         *DiskProjection
	currentMu    sync.RWMutex
	currentID    string
	current      ledger.Watermark
}

// restore binds this reader before restoring/replaying Usage. A nil restore
// creates a fresh aggregate. The callback must use the supplied reader; returning
// an unbound/differently bound aggregate is rejected.
func NewLedgerDerivative(owner LedgerDerivativeOwner, restore func(FinalizationReader) (*Aggregate, error)) (*LedgerDerivative, error) {
	if owner == nil {
		return nil, errors.New("usage derivative requires selected Ledger owner")
	}
	d := &LedgerDerivative{owner: owner}
	var err error
	if restore == nil {
		d.aggregate, err = NewAggregateWithFinalizationReader(d)
	} else {
		d.aggregate, err = restore(d)
	}
	if err != nil {
		return nil, err
	}
	if d.aggregate == nil || d.aggregate.finalizationReader != d {
		return nil, errors.New("usage derivative aggregate must bind selected reader")
	}
	return d, nil
}

// NewDiskLedgerDerivative binds the same live-event bridge to fresh disk Usage.
// The returned projection is caller-owned: after derivative.Close joins Apply,
// the caller closes/removes the DB and releases disk fees. There is no checkpoint
// restore or permission to trust an existing file; CreateDiskProjection is exclusive.
func NewDiskLedgerDerivative(owner LedgerDerivativeOwner, path string) (*LedgerDerivative, *DiskProjection, error) {
	return NewDiskLedgerDerivativeWithLimit(owner, path, 0)
}

func NewDiskLedgerDerivativeWithLimit(owner LedgerDerivativeOwner, path string, maxDiskBytes int64) (*LedgerDerivative, *DiskProjection, error) {
	if owner == nil {
		return nil, nil, errors.New("usage derivative requires selected Ledger owner")
	}
	d := &LedgerDerivative{owner: owner}
	p, err := CreateDiskProjectionWithLimit(path, d, maxDiskBytes)
	if err != nil {
		return nil, nil, err
	}
	d.disk = p
	return d, p, nil
}

func (d *LedgerDerivative) Aggregate() *Aggregate {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.aggregate
}

// ReadWatermark reads this same selected reducer. It neither reads a native
// durable tail nor substitutes a legacy Aggregate when disk reads fail.
func (d *LedgerDerivative) ReadWatermark(ctx context.Context) (ledger.Watermark, error) {
	if d.retired.Load() {
		return ledger.Watermark{}, ErrLedgerDerivativeRetired
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.retired.Load() {
		return ledger.Watermark{}, ErrLedgerDerivativeRetired
	}
	if d.disk != nil {
		stats, err := d.disk.ReadStats(ctx)
		return stats.Watermark, err
	}
	return d.aggregate.ReadWatermark(ctx)
}

// ReadPosition exposes the exact position of the selected Ledger reader. It is
// used to reject replay batches whose evidence pages observe a moving owner.
func (d *LedgerDerivative) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	if d.retired.Load() {
		return ledger.Watermark{}, [32]byte{}, ErrLedgerDerivativeRetired
	}
	d.ownerMu.RLock()
	owner := d.owner
	d.ownerMu.RUnlock()
	if owner == nil {
		return ledger.Watermark{}, [32]byte{}, ErrLedgerDerivativeRetired
	}
	return owner.ReadPosition(ctx)
}

// ApplyReplayBatchContext applies one authenticated replay page to a disk
// derivative under one bbolt transaction. The selected owner must already
// contain the page; ordinary live appends keep the one-record ApplyContext
// barrier and its borrowed-current-event semantics.
func (d *LedgerDerivative) ApplyReplayBatchContext(ctx context.Context, records []ledger.Record) error {
	if d.retired.Load() {
		return ErrLedgerDerivativeRetired
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.retired.Load() {
		return ErrLedgerDerivativeRetired
	}
	if d.disk == nil {
		return errors.New("Usage replay batch requires a disk derivative")
	}
	d.ownerMu.RLock()
	owner := d.owner
	d.ownerMu.RUnlock()
	if owner == nil {
		return ErrLedgerDerivativeRetired
	}
	head, _, err := owner.ReadPosition(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 || records[len(records)-1].Sequence > head.Sequence {
		return errors.New("Usage replay batch exceeds selected Ledger owner")
	}
	return d.disk.ApplyReplayBatchContext(ctx, records)
}

// Close permanently withdraws live Apply, joins admitted Apply and reader IO,
// then breaks the Aggregate-to-derivative-to-owner reference chain. It does not
// close the caller-owned Ledger DB. Never call Close from its own owner/Apply
// callback. Previously borrowed Aggregate aliases remain the caller's lifetime
// responsibility; their bound reader now fails rather than becoming unbound.
func (d *LedgerDerivative) Close() {
	if d == nil {
		return
	}
	d.retired.Store(true)
	d.replayMu.Lock()
	if d.replayCancel != nil {
		d.replayCancel()
	}
	d.replayMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	d.owner = nil
	d.aggregate = nil
	d.disk = nil
}

func (d *LedgerDerivative) Apply(record ledger.Record) error {
	return d.ApplyContext(context.Background(), record)
}

func (d *LedgerDerivative) ApplyContext(ctx context.Context, record ledger.Record) error {
	if d.retired.Load() {
		return ErrLedgerDerivativeRetired
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.retired.Load() {
		return ErrLedgerDerivativeRetired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	if record.Sequence == 0 || record.Generation == 0 || record.Offset <= 0 {
		return errors.New("usage derivative requires actual native position")
	}
	d.ownerMu.RLock()
	head, _, err := d.owner.ReadPosition(ctx)
	d.ownerMu.RUnlock()
	if err != nil {
		return fmt.Errorf("read usage Ledger owner: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A historical replay must find its event in the owner. Only the immediate
	// next native record can supply a temporarily absent current event position.
	if head.Sequence < record.Sequence {
		if record.Sequence != head.Sequence+1 ||
			(head.Generation != record.Generation && !(head == (ledger.Watermark{}) && record.Generation == 1)) ||
			(head.Generation == record.Generation && record.Offset <= head.Offset) {
			return errors.New("usage derivative record does not follow selected Ledger owner")
		}
		d.currentMu.Lock()
		d.currentID = record.Event.EventID
		d.current = ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}
		d.currentMu.Unlock()
		defer func() { d.currentMu.Lock(); d.currentID = ""; d.current = ledger.Watermark{}; d.currentMu.Unlock() }()
	}
	if d.disk != nil {
		return d.disk.ApplyContext(ctx, record)
	}
	return d.aggregate.ApplyContext(ctx, record)
}

func (d *LedgerDerivative) ReadFinalization(ctx context.Context, id string) (ledger.Record, bool, error) {
	d.ownerMu.RLock()
	defer d.ownerMu.RUnlock()
	if d.owner == nil {
		return ledger.Record{}, false, ErrLedgerDerivativeRetired
	}
	return d.owner.ReadFinalization(ctx, id)
}

// ReadLegacyRequest preserves the selected owner's evidence about old WAL
// origins. Owners without that evidence retain strict modern attribution.
func (d *LedgerDerivative) ReadLegacyRequest(ctx context.Context, id string) (bool, error) {
	d.ownerMu.RLock()
	defer d.ownerMu.RUnlock()
	if d.owner == nil {
		return false, ErrLedgerDerivativeRetired
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if source, ok := d.owner.(LegacyRequestReader); ok {
		return source.ReadLegacyRequest(ctx, id)
	}
	return false, nil
}

func (d *LedgerDerivative) ReadEventPosition(ctx context.Context, id string) (ledger.Watermark, bool, error) {
	d.ownerMu.RLock()
	defer d.ownerMu.RUnlock()
	if d.owner == nil {
		return ledger.Watermark{}, false, ErrLedgerDerivativeRetired
	}
	first, found, err := d.owner.ReadEventPosition(ctx, id)
	if err != nil || found {
		return first, found, err
	}
	if err := ctx.Err(); err != nil {
		return ledger.Watermark{}, false, err
	}
	d.currentMu.RLock()
	defer d.currentMu.RUnlock()
	if id == d.currentID && d.currentID != "" {
		return d.current, true, nil
	}
	return ledger.Watermark{}, false, nil
}
