package budget

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type authenticatedAccountingOwner struct {
	*ledger.AuthenticatedLedgerProjection
	native     *ledger.Log
	appendGate *authenticatedAppendGate
}

type authenticatedAppendGate struct {
	lane    chan struct{}
	waiters atomic.Int64
}

func newAuthenticatedAccountingOwner(p *ledger.AuthenticatedLedgerProjection, native *ledger.Log) authenticatedAccountingOwner {
	return authenticatedAccountingOwner{AuthenticatedLedgerProjection: p, native: native, appendGate: &authenticatedAppendGate{lane: make(chan struct{}, 1)}}
}

// Reserve every temporary COW/source/position slot before the native append.
// The sole append owner lends these same fixed credits through synchronous
// Apply; pressure after durability cannot require fresh RAM admission.
// Native disk growth and HA confirmation retain their independent gates.
func (p authenticatedAccountingOwner) withAppendWorkspace(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	manager, err := p.ResourceManager(ctx)
	if err != nil {
		return err
	}
	return resourcebudget.WithAdmittedWorkspace(ctx, manager, func(ctx context.Context) error {
		return p.withOwnedAppendWorkspace(ctx, visit)
	})
}

func (p authenticatedAccountingOwner) withOwnedAppendWorkspace(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	if p.appendGate == nil {
		return resourcebudget.ErrInvalid
	}
	g := p.appendGate
	if g.waiters.Add(1) > 16 {
		g.waiters.Add(-1)
		return resourcebudget.ErrExhausted
	}
	defer g.waiters.Add(-1)
	manager, err := p.ResourceManager(ctx)
	if err != nil {
		return err
	}
	ticket, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ticket.Release()) }()
	if err := resourcebudget.AcquireWorkLane(ctx, g.lane); err != nil {
		return err
	}
	defer func() { <-g.lane }()
	return p.WithPreparedPhysicalAppend(ctx, func(ctx context.Context) error {
		return p.WithNativeApplyWorkspace(ctx, visit)
	})
}

var _ accountingOwner = authenticatedAccountingOwner{}

type selectedAuthenticatedAccountingOwner struct{ authenticatedAccountingOwner }

func (p selectedAuthenticatedAccountingOwner) validateAccountingOperation(ctx context.Context) error {
	return p.native.ValidateSelectedStartupRead(ctx)
}

func (p authenticatedAccountingOwner) appliedPosition(ctx context.Context) (ledger.Watermark, error) {
	head, _, err := p.ReadPosition(context.WithoutCancel(ctx))
	return head, err
}

func (p authenticatedAccountingOwner) applyRecord(ctx context.Context, r ledger.Record) error {
	// The append callback owns this native mutation until authentication and
	// publication finish. Caller cancellation cannot skip an accepted fact.
	_, err := p.AdvanceOneFromNative(context.WithoutCancel(ctx), p.native, ledger.ProjectionPrefix{
		Head: ledger.Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}, Hash: r.Hash,
	})
	return err
}

// NewWithAuthenticatedProjection binds normal reads and durable Apply to one
// fresh, MAC-native-built owner. Native source identity and the complete head
// must match under a live lease. The caller must drain/Retire old Managers,
// finish two-level recovery and role/confirmation/resource gates, then release
// maintenance before opening business admission. This constructor does not
// switch Runtime defaults or enable saved-root/incremental startup. Retire the
// Manager and Close/join the native Log before closing this borrowed projection.
func NewWithAuthenticatedProjection(ctx context.Context, log *ledger.Log, projection *ledger.AuthenticatedLedgerProjection, periods *PeriodResolver, lease *ledger.MaintenanceLease, options Options) (*Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if log == nil || projection == nil || periods == nil {
		return nil, errors.New("native log, fresh authenticated projection, and period resolver are required")
	}
	if err := projection.ValidateFreshNativeWriter(ctx, log, lease); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newWithOwner(log, newAuthenticatedAccountingOwner(projection, log), periods, options), nil
}

// NewWithSelectedAuthenticatedProjection adopts the actual selected COW owner
// after its native finite history grant permits ordinary reads. The same shared
// budget, actual native source and complete applied head are required under the
// live maintenance lease. All point reads and durable Advance remain bound to
// this owner; expiration/cancellation/corruption of its grant propagates through
// those actual operations. Construction does not authorize structural seals or
// enable Runtime business admission, and the Manager borrows both resources.
func NewWithSelectedAuthenticatedProjection(ctx context.Context, log *ledger.Log, projection *ledger.AuthenticatedLedgerProjection, periods *PeriodResolver, lease *ledger.MaintenanceLease, manager *resourcebudget.Manager, options Options) (*Manager, error) {
	return newWithSelectedAuthenticatedProjection(ctx, log, projection, periods, lease, manager, options, false)
}

// NewWithHASelectedAuthenticatedProjection requires the actual selected HA
// native constructor and its independently frozen Primary authority, complete
// historical audit, original shared Manager and live exclusive maintenance
// lease. The native primary durability/confirmation hooks remain installed.
// Construction grants no Ready or structural publication permission. Runtime
// must recover pending facts, confirm the target and release maintenance before
// admitting business, then Retire this Manager when its role is withdrawn.
func NewWithHASelectedAuthenticatedProjection(ctx context.Context, log *ledger.Log, projection *ledger.AuthenticatedLedgerProjection, periods *PeriodResolver, lease *ledger.MaintenanceLease, manager *resourcebudget.Manager, options Options) (*Manager, error) {
	return newWithSelectedAuthenticatedProjection(ctx, log, projection, periods, lease, manager, options, true)
}

func newWithSelectedAuthenticatedProjection(ctx context.Context, log *ledger.Log, projection *ledger.AuthenticatedLedgerProjection, periods *PeriodResolver, lease *ledger.MaintenanceLease, manager *resourcebudget.Manager, options Options, ha bool) (*Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if log == nil || projection == nil || periods == nil {
		return nil, errors.New("selected native log, authenticated projection, and period resolver are required")
	}
	validate := projection.ValidateSelectedNativeWriter
	if ha {
		validate = projection.ValidateHASelectedNativeWriter
	}
	if err := validate(ctx, log, lease, manager); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newWithOwner(log, selectedAuthenticatedAccountingOwner{newAuthenticatedAccountingOwner(projection, log)}, periods, options), nil
}
