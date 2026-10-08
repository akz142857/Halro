package replication

import (
	"context"
	"errors"
	"testing"
)

func TestPeerPhysicalNegativeDecisionAdvancesOnlyOriginalSerial(t *testing.T) {
	// Component IO bookkeeping fixture only; the signed Replica role below is
	// independent input, never a Runtime promotion or source grant.
	p, j, m, fee := physicalPublicationFixture(t)
	p.state.Role = RoleReplica
	if err := WriteState(p.path, p.state, p.key[:]); err != nil {
		t.Fatal(err)
	}
	o := requireActualPhysicalPublication(t, p, j, m, fee, 8)
	record, err := j.Append(testLeadershipRecord(1, p.state.Term, "negative-anchor"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PublishReplica(ReplicaProgress{DurableIndex: 1, ConfirmedIndex: 1, AppliedIndex: 1, OrderingHeadMAC: record.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	run := func(visit func(context.Context) error) error {
		return withPhysicalRenewScope(context.Background(), p, j, m, visit)
	}
	window := [32]byte{12}
	first := peerBookkeepingTicket(window, 1)
	request := [32]byte{13}
	if err = run(func(ctx context.Context) error { return o.RecordPeerFutureWindowFrozen(ctx, window) }); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	if err = o.RecordPeerFutureRejectionFrozen(context.Background(), window, first, request, 0); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("literal context recorded negative", err)
	}
	var escaped context.Context
	if err = run(func(ctx context.Context) error {
		escaped = ctx
		return o.RecordPeerFutureRejectionFrozen(ctx, window, first, request, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err = o.RecordPeerFutureRejectionFrozen(escaped, window, first, request, 0); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("escaped negative scope", err)
	}
	var original PeerPhysicalFutureObservation
	if err = run(func(ctx context.Context) error {
		var err error
		original, err = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if original.Serial != 1 || !original.Granted.IsZero() || original.Tickets[0].Active || !original.Tickets[0].Cost.IsZero() || original.Tickets[0].ID != first || m.Snapshot().Used != before {
		t.Fatal("negative minted credit", original, m.Snapshot())
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureRejectionFrozen(ctx, window, first, request, 0)
	}); err != nil {
		t.Fatal("duplicate negative", err)
	}
	for _, tc := range []struct {
		window  [32]byte
		id      [16]byte
		request [32]byte
		peer    uint8
	}{
		{window, first, [32]byte{14}, 0}, {[32]byte{99}, first, request, 0}, {window, peerBookkeepingTicket(window, 3), request, 0}, {window, first, request, 2},
	} {
		if err = run(func(ctx context.Context) error {
			return o.RecordPeerFutureRejectionFrozen(ctx, tc.window, tc.id, tc.request, tc.peer)
		}); !errors.Is(err, ErrPhysicalCredit) {
			t.Fatal("foreign/gap negative", err)
		}
	}
	second := peerBookkeepingTicket(window, 2)
	cost := peerBookkeepingCost()
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, second, [32]byte{15}, cost, cost, 0)
	}); err != nil {
		t.Fatal("negative stalled next genuine promise", err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureRejectionFrozen(ctx, window, second, [32]byte{15}, 0)
	}); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("negative erased actual active promise", err)
	}
	if err = run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("actual future forgotten", err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, window, second, PhysicalCreditCost{}, PhysicalCreditCost{})
	}); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); err != nil {
		t.Fatal(err)
	}
}
