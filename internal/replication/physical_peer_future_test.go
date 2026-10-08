package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This fixture tests only persistent IO bookkeeping. Literal test capacities
// intentionally cannot supply Runtime source/root/role or native append grants.
func peerBookkeepingCost() PhysicalCreditCost {
	return PhysicalCreditCost{NativeBytes: [4]uint64{88 << 20, 8 << 20, 8 << 20, 48 << 20}, NativeRecords: [4]uint32{11, 1, 1, 6}, COWRecords: [4]uint32{11, 0, 1, 6}, UsageFrames: 11, OrderingRecords: 19}
}
func peerBookkeepingTicket(window [32]byte, serial uint64) (id [16]byte) {
	copy(id[:8], window[:8])
	binary.BigEndian.PutUint64(id[8:], serial)
	return
}
func TestPeerPhysicalControlFixedBinaryExactAndNoOverflow(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	r := physicalPublicationReceipt{Version: 3, MAC: bytes.Repeat([]byte{9}, 32)}
	w := physicalRenewalWindow{Generation: 2, Term: 7, Index: 3, Head: [32]byte{3}, End: 4096, Peer: &physicalPeerFutureState{WindowID: [32]byte{1}, Serial: 1, Granted: peerBookkeepingCost()}}
	w.Peer.Tickets[0] = physicalPeerFutureTicket{ID: peerBookkeepingTicket(w.Peer.WindowID, 1), Window: w.Peer.WindowID, Request: [32]byte{2}, Cost: peerBookkeepingCost(), Active: true}
	var err error
	w.MAC, err = physicalRenewalMAC(w, r, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodePhysicalRenewal(w)
	if err != nil || len(raw) != physicalRenewSlotBytes {
		t.Fatal(err, len(raw))
	}
	decoded, err := decodePhysicalRenewal(raw, r, key)
	if err != nil || decoded.Peer == nil || *decoded.Peer != *w.Peer || !bytes.Equal(decoded.MAC, w.MAC) {
		t.Fatal(decoded, err)
	}
	changed := bytes.Clone(raw)
	changed[len(changed)-1] = 1
	if _, err = decodePhysicalRenewal(changed, r, key); err == nil {
		t.Fatal("unknown control padding accepted")
	}
	changed = bytes.Clone(raw)
	changed[90] ^= 1
	if _, err = decodePhysicalRenewal(changed, r, key); err == nil {
		t.Fatal("forged future bookkeeping accepted")
	}
	w.Peer.Granted.NativeBytes[0] = 1 << 32
	if _, err = encodePhysicalRenewal(w); err == nil {
		t.Fatal("compact IO counter wrapped")
	}
}
func TestPeerPhysicalBookkeepingDuplicateAndRenewalCannotRefundLivePromise(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	window := [32]byte{5}
	cost := peerBookkeepingCost()
	available, err := cost.Add(cost)
	if err != nil {
		t.Fatal(err)
	}
	available.OrderingRecords = 64
	first := peerBookkeepingTicket(window, 1)
	second := peerBookkeepingTicket(window, 2)
	used := PhysicalCreditCost{NativeBytes: [4]uint64{2048}, NativeRecords: [4]uint32{2}, COWRecords: [4]uint32{2}, UsageFrames: 2, OrderingRecords: 2}
	req := [32]byte{8}
	run := func(visit func(context.Context) error) error {
		return withPhysicalRenewScope(context.Background(), p, j, m, visit)
	}
	if err = run(func(ctx context.Context) error { return o.RecordPeerFutureWindowFrozen(ctx, window) }); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, first, req, cost, available, 0)
	}); err != nil {
		t.Fatal(err)
	}
	var before PeerPhysicalFutureObservation
	if err = run(func(ctx context.Context) error {
		var err error
		before, err = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, first, req, cost, available, 0)
	}); err != nil {
		t.Fatal(err)
	}
	var after PeerPhysicalFutureObservation
	if err = run(func(ctx context.Context) error {
		var err error
		after, err = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil || after != before {
		t.Fatal("duplicate minted/refunded future IO", err, before, after)
	}
	if err = run(func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) }); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("DCA equality retired live future IO", err)
	}
	if err = run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("promise hidden from common barrier", err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, second, [32]byte{9}, cost, available, 0)
	}); err != nil {
		t.Fatal(err)
	}
	third := peerBookkeepingTicket(window, 3)
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, third, [32]byte{10}, cost, available, 0)
	}); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("third request overwrote live slot", err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, window, first, used, used)
	}); err != nil {
		t.Fatal(err)
	}
	var released PeerPhysicalFutureObservation
	if err = run(func(ctx context.Context) error {
		var err error
		released, err = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, window, first, used, used)
	}); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error {
		var err error
		after, err = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil || after != released {
		t.Fatal("duplicate release refunded IO twice", err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, window, second, used, used)
	}); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) }); err != nil {
		t.Fatal(err)
	}
	next := [32]byte{6}
	if err = run(func(ctx context.Context) error { return o.RecordPeerFutureWindowFrozen(ctx, next) }); err != nil {
		t.Fatal(err)
	}
	if err = run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, window, second, used, used)
	}); err != nil {
		t.Fatal("lost prior-window release ACK could not replay", err)
	}
	_ = resourcebudget.Healthy
}

func TestPeerPhysicalPromiseActualSIGKILLNeverRefundsFutureIO(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	window := [32]byte{5}
	if err := withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.RecordPeerFutureWindowFrozen(ctx, window) }); err != nil {
		t.Fatal(err)
	}
	before := p.Snapshot()
	in := physicalRenewCrashInput{StatePath: p.path, OrderingPath: j.file.Name(), Key: bytes.Clone(p.key[:]), Phase: "peer-future-promise-synced", Ready: filepath.Join(t.TempDir(), "ready")}
	p.Close()
	if err := errors.Join(p.PhysicalCloseError(), j.Close()); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestPeerPhysicalPromiseActualChild$", "-test.count=1", "-test.timeout=30s")
	child.Env = append(os.Environ(), "HALRO_PEER_FUTURE_INPUT="+path)
	var output bytes.Buffer
	child.Stdout = &output
	child.Stderr = &output
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		if !joined {
			child.Process.Kill()
			child.Wait()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err = os.Stat(in.Ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual synced promise missing", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	joined = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err, output.String())
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal(status)
	}
	fresh, next, restored, budget := openPhysicalRenewCrashSource(t, in)
	if !sameCompleteMemberState(before, fresh.Snapshot()) {
		t.Fatal("future IO receipt changed member permission")
	}
	var got PeerPhysicalFutureObservation
	run := func(visit func(context.Context) error) error {
		return withPhysicalRenewScope(context.Background(), fresh, next, budget, visit)
	}
	if err = run(func(ctx context.Context) error {
		var e error
		got, e = restored.ObservePeerPhysicalPromisesFrozen(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if got.WindowID != window || !got.Tickets[0].Active || got.Granted != peerBookkeepingCost() {
		t.Fatal("synced promise lost/refunded", got)
	}
	if err = run(func(ctx context.Context) error { return restored.RequireNoPeerPhysicalPromises(ctx) }); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("unknown semantic owner allowed common retirement", err)
	}
	if err = run(func(ctx context.Context) error { return restored.ReplenishFrozen(ctx, 8) }); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("restart gave new allocation despite live promise", err)
	}
	// A damaged newer promise slot must not fall back to the previous zero-promise
	// control. Retaining/refusing unknown debt is different from claiming no debt.
	original, err := os.ReadFile(in.StatePath + ".physical.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Clone(original)
	off := physicalRenewStaticBytes + int(restored.renewalGeneration%2)*physicalRenewSlotBytes
	changed[off+90] ^= 1
	if err = os.WriteFile(in.StatePath+".physical.json", changed, 0600); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	next.mu.Lock()
	err = restored.restoreRenewalWindowLocked(context.Background())
	next.mu.Unlock()
	fresh.mu.Unlock()
	if !errors.Is(err, ErrPhysicalCredit) || !physicalPeerFutureActive(restored.currentControl.Peer) {
		t.Fatal("torn promise fell back to unpromised credit", err)
	}
	if err = os.WriteFile(in.StatePath+".physical.json", original, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestPeerPhysicalPromiseActualChild(t *testing.T) {
	path := os.Getenv("HALRO_PEER_FUTURE_INPUT")
	if path == "" {
		t.Skip("actual child process")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var in physicalRenewCrashInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	p, j, o, m := openPhysicalRenewCrashSource(t, in)
	o.phase = func(phase string) error {
		if phase == in.Phase {
			if err := os.WriteFile(in.Ready, []byte(phase), 0600); err != nil {
				return err
			}
			select {}
		}
		return nil
	}
	window := [32]byte{5}
	cost := peerBookkeepingCost()
	if err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, peerBookkeepingTicket(window, 1), [32]byte{8}, cost, cost, 0)
	}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent must SIGKILL actual callback")
}
func TestPeerPhysicalPromiseCancellationRetainsOriginalOwnerAndFees(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	before := m.Snapshot().Used
	ctx, cancel := context.WithCancel(context.Background())
	o.phase = func(phase string) error {
		if phase == "peer-future-promise-synced" {
			cancel()
		}
		return nil
	}
	window := [32]byte{5}
	err := withPhysicalRenewScope(ctx, p, j, m, func(ctx context.Context) error {
		return o.RecordPeerFuturePromiseFrozen(ctx, window, peerBookkeepingTicket(window, 1), [32]byte{8}, peerBookkeepingCost(), peerBookkeepingCost(), 0)
	})
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, context.Canceled) || o.receiptFile == nil || o.renewalDebt == (resourcebudget.Permit{}) || o.failure() == nil || m.Snapshot().Used.RAM <= before.RAM {
		t.Fatal("unknown persisted promise lost owner/debt", err, m.Snapshot(), o.renewalDebt)
	}
}

// Checking that no future promise exists cannot make a legitimately unconfirmed
// Primary source disappear. It adds no allocation/selection/ready permission.
func TestPeerPhysicalNoPromiseObservationPreservesPendingSourcePermission(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	pending := testOrderingRecord(2, p.state.Term, "pending")
	pending.StoreSequenceFirst, pending.StoreSequenceLast = 1, 1
	record, err := j.Append(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: p.Snapshot().Projection}); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	if err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); err != nil || m.Snapshot().Used != before {
		t.Fatal("read-only no-promise check revoked existing pending permission", err)
	}
	if err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.RecordPeerFutureWindowFrozen(ctx, [32]byte{9}) }); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("pending observation acquired new future bank", err)
	}
}

func TestPeerPhysicalPrimaryMirrorsTwoDistinctActualPeerBanksWithoutJoiningCredit(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	state := p.Snapshot()
	key := bytes.Clone(p.key[:])
	path := p.path
	p.Close()
	state.Peers = append(state.Peers, StatePeer{Name: "halro-2", Address: "halro-2.internal:9910", SPKISHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	var err error
	p, err = NewStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 2)
	r, err := j.Append(testLeadershipRecord(1, p.state.Term, "anchor"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: r.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	if len(p.state.Peers) != 2 {
		t.Fatal("actual three-member configuration expected", p.state.Peers)
	}
	local := [32]byte{9}
	remote0 := [32]byte{1}
	remote1 := [32]byte{2}
	cost := peerBookkeepingCost()
	run := func(visit func(context.Context) error) error {
		return withPhysicalRenewScope(context.Background(), p, j, m, visit)
	}
	for peer, window := range [][32]byte{remote0, remote1} {
		ticket := peerBookkeepingTicket(window, 1)
		if err := run(func(ctx context.Context) error {
			return o.RecordPeerFuturePromiseForPeerFrozen(ctx, local, window, ticket, [32]byte{8}, cost, cost, uint8(peer))
		}); err != nil {
			t.Fatal("distinct remote bank denied/joined into first bank", peer, err)
		}
	}
	var got PeerPhysicalFutureObservation
	if err := run(func(ctx context.Context) error {
		var e error
		got, e = o.ObservePeerPhysicalPromisesFrozen(ctx)
		return e
	}); err != nil || got.Tickets[0].Window != remote0 || got.Tickets[1].Window != remote1 || !got.Tickets[0].Active || !got.Tickets[1].Active {
		t.Fatal(got, err)
	}
	if err := run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, remote0, peerBookkeepingTicket(remote1, 1), cost, PhysicalCreditCost{})
	}); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("one peer closed a distinct other's promise", err)
	}
	if err := run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, remote0, peerBookkeepingTicket(remote0, 1), cost, PhysicalCreditCost{})
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal("first peer exit erased second peer's obligation", err)
	}
	if err := run(func(ctx context.Context) error {
		return o.RecordPeerFutureReleaseFrozen(ctx, remote1, peerBookkeepingTicket(remote1, 1), cost, PhysicalCreditCost{})
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(func(ctx context.Context) error { return o.RequireNoPeerPhysicalPromises(ctx) }); err != nil {
		t.Fatal(err)
	}
}
