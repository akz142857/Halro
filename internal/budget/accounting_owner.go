package budget

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// Reading and applying belong to one owner for the Manager lifetime. Only
// constructors bind it; replacing a runtime generation requires a new Manager
// after withdrawing and draining business admission. The private reads field
// exists for fault injection, not for independently selecting a live reader.
type accountingOwner interface {
	ledger.AccountingAdmissionReader
	appliedPosition(ctx context.Context) (ledger.Watermark, error)
	applyRecord(context.Context, ledger.Record) error
}

type stateAccountingOwner struct{ *ledger.State }

func (s stateAccountingOwner) appliedPosition(ctx context.Context) (ledger.Watermark, error) {
	head, _, err := s.ReadPosition(context.WithoutCancel(ctx))
	return head, err
}
func (s stateAccountingOwner) applyRecord(ctx context.Context, r ledger.Record) error {
	return s.Apply(r)
}

type projectedAccountingOwner struct{ *ledger.LedgerProjection }

func (p projectedAccountingOwner) appliedPosition(ctx context.Context) (ledger.Watermark, error) {
	head, _, err := p.ReadPosition(context.WithoutCancel(ctx))
	return head, err
}
func (p projectedAccountingOwner) applyRecord(ctx context.Context, r ledger.Record) error {
	// Acceptance has already persisted this fact. Caller/native Close
	// cancellation may interrupt a predecessor wait, but cannot skip a fact
	// whose turn is ready. Native mutation ownership lasts through this Apply.
	return p.Apply(context.WithoutCancel(ctx), r)
}

// NewWithProjection binds normal accounting reads and durable Apply to a fresh
// projection, while this exact native log is still exclusively in maintenance.
// Position/hash are checked under that lease; a mismatch latches recovery.
// This is not a runtime publication or HA role/confirmation/readiness gate.
// After draining whole business requests/background producers, the caller must
// successfully Retire/join the old Manager before acquiring this native lease.
// A native lease alone cannot exclude stale Manager reads or post-Apply ACKs.
// The caller must finish two-level recovery, withdraw/drain old business work,
// establish permitted-prefix trust, resource owners and generation authority,
// and release the lease before enabling normal business. Close/join the native
// log before closing the projection; the Manager does not own either resource.
func NewWithProjection(ctx context.Context, log *ledger.Log, projection *ledger.LedgerProjection, periods *PeriodResolver, lease *ledger.MaintenanceLease, options Options) (*Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if log == nil || projection == nil || periods == nil {
		return nil, errors.New("native log, fresh projection, and period resolver are required")
	}
	if _, err := lease.ValidateOwner(log); err != nil {
		return nil, err
	}
	if err := lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		actual, actualHash, err := projection.ReadPosition(ctx)
		if err != nil {
			return err
		}
		if actual != head || actualHash != hash {
			return errors.New("accounting projection differs from exclusive native position/hash")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newWithOwner(log, projectedAccountingOwner{projection}, periods, options), nil
}

func newWithOwner(log *ledger.Log, owner accountingOwner, periods *PeriodResolver, options Options) *Manager {
	if options.Now == nil {
		options.Now = time.Now
	}
	m := &Manager{log: log, owner: owner, reads: owner, periods: periods, now: options.Now}
	m.applyCond = sync.NewCond(&m.applyMu)
	return m
}
