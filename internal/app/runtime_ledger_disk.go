package app

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var errLedgerAppendDiskLimit = errors.New("native Ledger append capacity exhausted")

// runtimeLedgerAppendDiskOwner prepays a fixed additional growth envelope.
// One counter covers Standalone, Primary and Replica writes and auxiliary
// products. Published/uncertain products stay consumed, and the counter does
// not reset at generation Roll. The startup inventory owns existing names;
// this owner never substitutes for it or for confirmation/Completion credits.
// No map, per-frame permit or historical payload is retained.
type runtimeLedgerAppendDiskOwner struct {
	mu                sync.Mutex
	credit            resourcebudget.Permit
	ceiling, admitted int64
	physicalUnused    int64
	published, closed bool
}

func prepareRuntimeLedgerAppendDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeLedgerAppendDiskOwner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.LedgerAppendDiskBytes == 0 {
		return nil, nil
	}
	ceiling := cfg.ResourceBudget.LedgerAppendDiskBytes
	if ceiling < 0 || ceiling > 1<<50 || cfg.ResourceBudget.PersistentInventoryDiskBytes <= 0 || pressure == nil || pressure.logicalBudget() == nil {
		return nil, resourcebudget.ErrInvalid
	}
	credit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{Disk: ceiling})
	if err != nil {
		return nil, err
	}
	return &runtimeLedgerAppendDiskOwner{credit: credit, ceiling: ceiling}, nil
}

// reserve is called only after native frame validation/complete encoding and
// before the first source seek/write. It consumes the ENTIRE possible batch,
// even when IO later partially succeeds or has an uncertain outcome. Already
// accepted bytes remain consumed on subsequent IO failure; no guess refunds
// them. A definite rejection consumes nothing and does not poison native state.
func (o *runtimeLedgerAppendDiskOwner) reserve(bytes int64) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return resourcebudget.ErrClosed
	}
	if bytes <= 0 {
		return resourcebudget.ErrInvalid
	}
	if bytes > o.ceiling-o.admitted-o.physicalUnused {
		return errors.Join(errLedgerAppendDiskLimit, resourcebudget.ErrExhausted)
	}
	o.admitted += bytes
	return nil
}

// Same owner and mutex as actual native/auxiliary admission. A prepared target
// range remains charged until actual native bytes consume it or a captured FD
// tail retirement joins. This is accounting, never filesystem allocation.
func (o *runtimeLedgerAppendDiskOwner) ReservePhysicalRange(bytes int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return resourcebudget.ErrClosed
	}
	if bytes <= 0 {
		return resourcebudget.ErrInvalid
	}
	if bytes > o.ceiling-o.admitted-o.physicalUnused {
		return errors.Join(errLedgerAppendDiskLimit, resourcebudget.ErrExhausted)
	}
	o.physicalUnused += bytes
	return nil
}

func (o *runtimeLedgerAppendDiskOwner) ConsumePhysicalRange(bytes int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return resourcebudget.ErrClosed
	}
	if bytes <= 0 || bytes > o.physicalUnused {
		return resourcebudget.ErrInvalid
	}
	o.physicalUnused -= bytes
	o.admitted += bytes
	return nil
}

func (o *runtimeLedgerAppendDiskOwner) RetirePhysicalRange(bytes int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if bytes <= 0 || bytes > o.physicalUnused {
		return resourcebudget.ErrInvalid
	}
	o.physicalUnused -= bytes
	return nil
}

func (o *runtimeLedgerAppendDiskOwner) publish() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return resourcebudget.ErrClosed
	}
	if o.published {
		return nil
	}
	p, err := o.credit.Reclassify(resourcebudget.Background)
	if err == nil {
		o.credit = p
		o.published = true
	}
	return err
}

// close is an admission stop after the caller joins the actual native owner.
// It NEVER refunds persistent fees or unused ceiling. A native Close error,
// generation rename, a confirmed record or missing old path does not prove
// physical bytes retired. Future fine-grained retirement needs actual receipts.
func (o *runtimeLedgerAppendDiskOwner) close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	return nil
}

func (o *runtimeLedgerAppendDiskOwner) consumed() int64 {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.admitted
}

type runtimeLedgerNativeFileToken struct {
	owner   *runtimeLedgerAppendDiskOwner
	bytes   int64
	retired bool // guarded by owner.mu
}

func (o *runtimeLedgerAppendDiskOwner) reserveNativeFile(bytes int64) (ledger.NativeFileDiskReservation, error) {
	if o == nil {
		return nil, nil
	}
	if err := o.reserve(bytes); err != nil {
		return nil, err
	}
	return &runtimeLedgerNativeFileToken{owner: o, bytes: bytes}, nil
}

func (t *runtimeLedgerNativeFileToken) RetireKnown() error {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.retired {
		return nil
	}
	if t.bytes <= 0 || t.bytes > t.owner.admitted {
		return resourcebudget.ErrInvalid
	}
	t.owner.admitted -= t.bytes
	t.retired = true
	// Only the reusable counter is refunded. The aggregate persistent Manager
	// claim remains prepaid through Close, including all published old inodes.
	return nil
}
