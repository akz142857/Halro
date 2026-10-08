package app

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
)

// Default physical admission requires every original peer's actual native/COW
// window promise before RequestAccepted can be durable locally. Hello and an ACK
// without the wire3 ticket supply no terminal credit.
func (f *haSelectedRuntimeFoundation) peerPhysicalTerminalAdmission(ctx context.Context, count int) (context.Context, func() error, error) {
	if f == nil || f.replication == nil || ctx == nil || ctx.Err() != nil {
		return ctx, nil, replication.ErrPhysicalCredit
	}
	q := f.replication.peerPhysical.Load()
	if q == nil || !q.primary {
		return ctx, nil, replication.ErrPhysicalCredit
	}
	cost, err := haPeerPhysicalTerminalCost(count)
	if err != nil {
		return ctx, nil, err
	}
	var digest [32]byte
	if _, err = rand.Read(digest[:]); err != nil {
		return ctx, nil, err
	}
	admitted, ticket, err := q.reserve(ctx, digest, cost)
	if err != nil {
		return ctx, nil, err
	}
	bound, closeLocal, err := f.physicalTerminalAdmission(admitted, count)
	if err != nil {
		// No context was exposed to semantic work. Original local constructors have
		// synchronously returned and their known cleanup ran; independent complete
		// source proofs decide whether the remote promise is now safely releasable.
		joined := f.finishOwnedPeerPhysicalTicket(admitted, q, ticket)
		if joined != nil {
			return ctx, nil, errors.Join(err, ownedfile.ErrRetirement, joined)
		}
		return ctx, nil, err
	}
	return bound, func() error {
		if err := closeLocal(); err != nil {
			// A local terminal owner with unknown exit retains its original companion
			// future ticket. It cannot be converted to an unused peer cancellation.
			return errors.Join(ownedfile.ErrRetirement, err)
		}
		if err := f.finishOwnedPeerPhysicalTicket(bound, q, ticket); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
		return nil
	}, nil
}

// A caller cancel cannot withdraw an accepted peer's persistent IO promise.
// Preserve the original ticket/workspace values until actual release is proven;
// the owning foundation and shutdown deadline still bound this continuation.
func (f *haSelectedRuntimeFoundation) finishOwnedPeerPhysicalTicket(ctx context.Context, q *haPeerPhysicalController, ticket *haPeerPhysicalTicket) error {
	joined, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.cfg.Server.ShutdownTimeout.Value())
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	return q.finishTicket(joined, ticket)
}
