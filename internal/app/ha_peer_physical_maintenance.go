package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// Only the original controller's actual exclusive entry and (on Replica)
// receiver/applier locks install this loan. A tuple or a caller flag cannot do so.
type haPeerPhysicalPauseKey struct{}
type haPeerPhysicalPause struct {
	q            *haPeerPhysicalController
	mu           sync.Mutex
	active, busy bool
	jobs         sync.WaitGroup
	position     replication.ReplicaConfirmedPosition
	ack          replication.Acknowledgement
	err          error
}

func (s *haPeerPhysicalPause) borrow(q *haPeerPhysicalController) (func(error), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.busy || s.q != q {
		return nil, replication.ErrPhysicalCredit
	}
	s.busy = true
	s.jobs.Add(1)
	return func(err error) {
		s.mu.Lock()
		s.err = errors.Join(s.err, err)
		s.busy = false
		s.mu.Unlock()
		s.jobs.Done()
	}, nil
}
func (q *haPeerPhysicalController) visitPhysicalPause(ctx context.Context, p replication.ReplicaConfirmedPosition, ack replication.Acknowledgement, visit func(context.Context) error) error {
	s := &haPeerPhysicalPause{q: q, active: true, position: p, ack: ack}
	err := visit(context.WithValue(ctx, haPeerPhysicalPauseKey{}, s))
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
	// Actual entered IO borrowers must exit before the original receive locks or
	// admission exclusion are released. Cancellation is not evidence of exit.
	s.jobs.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := errors.Join(err, s.err, ctx.Err())
	if result != nil {
		q.mu.Lock()
		q.failure = errors.Join(q.failure, result)
		q.mu.Unlock()
	}
	return result
}
func (f *haSelectedRuntimeFoundation) withHAPhysicalAdmissionPaused(ctx context.Context, visit func(context.Context) error) error {
	if f == nil || f.replication == nil || visit == nil {
		return replication.ErrPhysicalCredit
	}
	q := f.replication.peerPhysical.Load()
	if q == nil {
		return replication.ErrPhysicalCredit
	}
	return q.withAdmissionPaused(ctx, visit)
}

// Refresh retains the original controller, paid fee and installed receive
// policy. It runs only inside its own still-active pause loan, after genuine
// whole publication, NoDebt retirement and original-bank replenishment.
func (f *haSelectedRuntimeFoundation) refreshHAPhysicalPeerBank(ctx context.Context) (resultErr error) {
	if f == nil || f.replication == nil || ctx == nil || ctx.Err() != nil {
		return replication.ErrPhysicalCredit
	}
	q := f.replication.peerPhysical.Load()
	s, ok := ctx.Value(haPeerPhysicalPauseKey{}).(*haPeerPhysicalPause)
	if !ok || s == nil || q == nil {
		return replication.ErrPhysicalCredit
	}
	done, err := s.borrow(q)
	if err != nil {
		return err
	}
	defer func() { done(resultErr) }()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing || q.failure != nil || q.live != nil {
		return replication.ErrPhysicalCredit
	}
	for _, p := range q.peers {
		if p.waiting {
			return replication.ErrPhysicalCredit
		}
	}
	if _, err := q.fee.Cost(); err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			q.failure = errors.Join(q.failure, resultErr)
		}
	}()
	var vector haPeerPhysicalVector
	if !q.primary {
		vector, err = f.capturePeerPhysicalVector(ctx)
		if err != nil {
			return err
		}
		m, p, ack := vector.Member, s.position, s.ack
		if p.NodeID != m.NodeID || p.Term != m.Term || p.PromisedTerm != m.Term || p.Durable != m.DurableIndex || p.Confirmed != m.ConfirmedIndex || p.Applied != m.AppliedIndex || p.Projection != m.Projection || ack.Index != m.DurableIndex || vector.Seal.OrderingIndex != m.DurableIndex {
			return replication.ErrPhysicalCredit
		}
		spent, err := haPeerPhysicalSpent(vector)
		if err != nil || !spent.IsZero() {
			return errors.Join(replication.ErrPhysicalCredit, err)
		}
	}
	return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || cut.Seal.OrderingIndex != m.DurableIndex {
				return replication.ErrPhysicalCredit
			}
			if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.reopened.Owners, f.physicalPublication); err != nil {
				return err
			}
			ordering, err := f.physicalPublication.ObservePhysicalOrderingWindowFrozen(ctx)
			if err != nil {
				return err
			}
			if ordering.Index != m.DurableIndex || ordering.Head != m.OrderingHeadMAC || ordering.RemainingRecords == 0 {
				return replication.ErrPhysicalCredit
			}
			var window [32]byte
			if q.primary {
				if m.Role != replication.RolePrimary {
					return replication.ErrPhysicalCredit
				}
				raw, err := json.Marshal(struct {
					Seal     authenticatedstate.CandidateSeal
					Ordering replication.PhysicalOrderingWindowObservation
				}{cut.Seal, ordering})
				if err != nil {
					return err
				}
				window = sha256.Sum256(raw)
			} else {
				if m.Role != replication.RoleReplica || cut.Seal != vector.Seal || cut.Member.OrderingHeadMAC != vector.Member.OrderingHeadMAC {
					return replication.ErrPhysicalCredit
				}
				window = vector.Window
			}
			original, err := f.physicalPublication.ObservePeerPhysicalPromisesFrozen(ctx)
			if err != nil {
				return err
			}
			for _, t := range original.Tickets {
				if t.Active {
					return replication.ErrPhysicalCredit
				}
			}
			if original.WindowID == window {
				return replication.ErrPhysicalCredit
			}
			if err := f.physicalPublication.RecordPeerFutureWindowFrozen(ctx, window); err != nil {
				return err
			}
			q.bankIndex = m.DurableIndex
			if q.primary {
				q.local = window
				for i := range q.peers {
					q.peers[i].window = replication.PhysicalCreditRecord{}
				}
			} else {
				q.vector, q.spent, q.grant, q.active = vector, replication.PhysicalCreditCost{}, replication.PhysicalCreditCost{}, [2]replication.PeerPhysicalFutureTicketObservation{}
			}
			return nil
		})
	})
}

// Send only after the exclusive pause callback and all source locks have exited.
// Original session authentication carries this complete requalified bank proof.
func (f *haSelectedRuntimeFoundation) advertiseHAPhysicalPeerBank(ctx context.Context) error {
	if f == nil || f.replication == nil || f.replication.manager == nil || ctx == nil || ctx.Err() != nil {
		return replication.ErrPhysicalCredit
	}
	q := f.replication.peerPhysical.Load()
	if q == nil {
		return replication.ErrPhysicalCredit
	}
	if _, ok := ctx.Value(haPeerPhysicalPauseKey{}).(*haPeerPhysicalPause); ok {
		return replication.ErrPhysicalCredit
	}
	if q.primary {
		return nil
	}
	return q.sendReplicaWindow(ctx, func(encoded []byte) error {
		return f.replication.manager.Send(f.owners.Baseline.CurrentPrimary(), encoded)
	})
}
