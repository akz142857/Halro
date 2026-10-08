package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func physicalRenewConfirmedFixture(t *testing.T) (*StatePublisher, *OrderingJournal, *PhysicalPublication, *resourcebudget.Manager, resourcebudget.Permit) {
	t.Helper()
	p, j, m, fee := physicalPublicationFixture(t)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 2)
	record, err := j.Append(testLeadershipRecord(1, p.state.Term, "anchor"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	return p, j, o, m, fee
}
func withPhysicalRenewScope(ctx context.Context, p *StatePublisher, j *OrderingJournal, m *resourcebudget.Manager, visit func(context.Context) error) error {
	return p.WithReadAuthority(ctx, m, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
		return j.WithFrozenHead(ctx, m, func(ctx context.Context, _ FrozenOrderingHead) error {
			return authorize(func() error { return visit(ctx) })
		})
	})
}
func TestPhysicalPublicationRenewRequiresActualWholeScopeAndHealthy(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	before := m.Snapshot().Used
	end := o.end
	if err := o.ReplenishFrozen(context.Background(), 8); !errors.Is(err, ErrMemberAuthority) || o.end != end || m.Snapshot().Used != before {
		t.Fatal("literal context renewed source", err)
	}
	var saved context.Context
	if err := withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error {
		saved = ctx
		if err := m.SetPressure(resourcebudget.Pressured); err != nil {
			return err
		}
		if err := o.ReplenishFrozen(ctx, 8); !errors.Is(err, resourcebudget.ErrPressure) {
			return errors.Join(errors.New("pressure renewed range"), err)
		}
		if o.end != end {
			return errors.New("pressure changed original end")
		}
		return m.SetPressure(resourcebudget.Healthy)
	}); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("ignored borrower failure returned success", err)
	}
	if err := o.ReplenishFrozen(saved, 8); !errors.Is(err, ErrMemberAuthority) || m.Snapshot().Used != before {
		t.Fatal("escaped scope accepted", err)
	}
	if err := p.WithLedgerReadAuthority(context.Background(), m, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
		return j.WithFrozenHead(ctx, m, func(ctx context.Context, _ FrozenOrderingHead) error {
			return authorize(func() error {
				if err := o.ReplenishFrozen(ctx, 8); !errors.Is(err, ErrMemberAuthority) {
					return errors.Join(errors.New("live ledger context upgraded"), err)
				}
				return nil
			})
		})
	}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.ReplenishFrozen(canceled, 8); err == nil || o.end != end {
		t.Fatal("canceled renewal changed bytes", err)
	}
	if err := withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) }); err != nil {
		t.Fatal("original actual scope renewal", err)
	}
	if o.renewalGeneration != 1 || o.end != j.nextOffset+8*orderingRecordBytes || m.Snapshot().Used != before {
		t.Fatal(o.renewalGeneration, o.end, m.Snapshot())
	}
	if err := o.ValidateParents(context.Background(), p, j, m); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalPublicationRenewRestoresSameBirthAndPreservesAcceptedSlots(t *testing.T) {
	p, j, o, m, fee := physicalRenewConfirmedFixture(t)
	receiptBefore, err := os.ReadFile(p.path + ".physical.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) }); err != nil {
		t.Fatal(err)
	}
	receiptAfter, err := os.ReadFile(p.path + ".physical.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(receiptAfter) != 8192 || !bytes.Equal(receiptBefore[:physicalRenewStaticBytes], receiptAfter[:physicalRenewStaticBytes]) || !bytes.Equal(receiptBefore[receiveReceiptHeaderBytes:], receiptAfter[receiveReceiptHeaderBytes:]) {
		t.Fatal("renewal replaced immutable source or acceptance proof")
	}
	expected := o.end
	key := bytes.Clone(p.key[:])
	state := p.Snapshot()
	path := j.file.Name()
	bankDisk, _ := o.DiskReservations()
	p.Close()
	if err = errors.Join(p.PhysicalCloseError(), j.Close()); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewStatePublisher(p.path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	next, err := OpenExistingOrderingJournalWithOptions(path, key, state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	restored, err := fresh.PreparePhysicalPublication(context.Background(), next, m, PhysicalPublicationOptions{Records: 2, OrderingDisk: fee, BankDisk: bankDisk, Restart: true})
	if err != nil {
		t.Fatal("original same FD window did not restore", err)
	}
	if restored.end != expected || restored.renewalGeneration != 1 {
		t.Fatal(restored.end, expected, restored.renewalGeneration)
	}
	if err = restored.ValidateParents(context.Background(), fresh, next, m); err != nil {
		t.Fatal(err)
	}
	// A torn next-slot write occurs before any allocation. The previous slot and
	// its complete actual source limit must remain independently recoverable.
	raw := bytes.Clone(receiptAfter)
	raw[physicalRenewStaticBytes+9] = 0xff
	if err = os.WriteFile(fresh.path+".physical.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	next.mu.Lock()
	err = restored.restoreRenewalWindowLocked(context.Background())
	next.mu.Unlock()
	fresh.mu.Unlock()
	if err != nil || restored.end != expected || restored.renewalGeneration != 1 {
		t.Fatal("torn next slot lost last durable window", err)
	}
}

func TestMemberWholeReadScopeJoinsActualBorrowerAndRevokes(t *testing.T) {
	p, j, m, _ := physicalPublicationFixture(t)
	entered := make(chan struct{})
	exit := make(chan struct{})
	returned := make(chan error, 1)
	borrowDone := make(chan error, 1)
	go func() {
		returned <- p.WithReadAuthority(context.Background(), m, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
			return authorize(func() error {
				go func() {
					borrowDone <- borrowMemberWholeReadScope(ctx, p, func(MemberState) error { close(entered); <-exit; return nil })
				}()
				<-entered
				if err := borrowMemberWholeReadScope(ctx, p, func(MemberState) error { return errors.New("second callback entered") }); !errors.Is(err, ErrMemberAuthority) {
					return errors.Join(errors.New("second borrower admitted"), err)
				}
				return nil
			})
		})
	}()
	<-entered
	select {
	case err := <-returned:
		t.Fatal("original publisher mutex returned before borrowed IO joined", err)
	case <-time.After(20 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close left actual borrowed callback alive")
	case <-time.After(20 * time.Millisecond):
	}
	close(exit)
	if err := <-borrowDone; err != nil {
		t.Fatal(err)
	}
	if err := <-returned; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-closed
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

type physicalRenewCrashInput struct {
	StatePath, OrderingPath, Phase, Ready string
	Key                                   []byte
}

func openPhysicalRenewCrashSource(t *testing.T, in physicalRenewCrashInput) (*StatePublisher, *OrderingJournal, *PhysicalPublication, *resourcebudget.Manager) {
	t.Helper()
	state, err := ReadState(in.StatePath, in.Key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewStatePublisher(in.StatePath, in.Key, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	j, err := OpenExistingOrderingJournalWithOptions(in.OrderingPath, in.Key, state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := j.Close(); e != nil {
			t.Error(e)
		}
	})
	m := physicalPublicationBudget(t)
	fee, err := m.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	o, err := p.PreparePhysicalPublication(context.Background(), j, m, PhysicalPublicationOptions{Records: 2, OrderingDisk: fee, Restart: true})
	if err != nil {
		t.Fatal(err)
	}
	return p, j, o, m
}
func TestPhysicalPublicationRenewActualSIGKILLRestoresIndependentMember(t *testing.T) {
	for _, phase := range []string{"renewal-receipt-synced", "renewal-ordering-allocated"} {
		t.Run(phase, func(t *testing.T) {
			p, j, o, _, _ := physicalRenewConfirmedFixture(t)
			before := p.Snapshot()
			expected := j.nextOffset + 8*orderingRecordBytes
			in := physicalRenewCrashInput{StatePath: p.path, OrderingPath: j.file.Name(), Key: bytes.Clone(p.key[:]), Phase: phase, Ready: filepath.Join(t.TempDir(), "ready")}
			original, err := os.ReadFile(p.path + ".physical.json")
			if err != nil {
				t.Fatal(err)
			}
			p.Close()
			if err = errors.Join(p.PhysicalCloseError(), j.Close()); err != nil {
				t.Fatal(err)
			}
			_ = o
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "input.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestPhysicalPublicationRenewActualChild$", "-test.count=1", "-test.timeout=30s")
			child.Env = append(os.Environ(), "HALRO_ORDERING_RENEW_INPUT="+path)
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
					t.Fatal("actual renewal phase missing", output.String())
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
			fresh, next, restored, m := openPhysicalRenewCrashSource(t, in)
			actualIndex, _, _ := next.Head()
			if !sameCompleteMemberState(before, fresh.Snapshot()) || actualIndex != before.DurableIndex || restored.end != expected || restored.renewalGeneration != 1 {
				t.Fatal("original authority/progress changed or window lost", fresh.Snapshot(), restored.end, expected)
			}
			after, err := os.ReadFile(in.StatePath + ".physical.json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original[:physicalRenewStaticBytes], after[:physicalRenewStaticBytes]) || !bytes.Equal(original[receiveReceiptHeaderBytes:], after[receiveReceiptHeaderBytes:]) {
				t.Fatal("renewal modified source or accepted authority")
			}
			if err = restored.ValidateParents(context.Background(), fresh, next, m); err != nil {
				t.Fatal(err)
			}
			if err = resourcebudget.CheckPhysicalRange(next.file, next.nextOffset, expected); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestPhysicalPublicationRenewActualChild(t *testing.T) {
	path := os.Getenv("HALRO_ORDERING_RENEW_INPUT")
	if path == "" {
		t.Skip("actual process helper")
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
	if err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) }); err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent must SIGKILL before original callback returns")
}
func TestMemberWholeReadScopeCannotIgnoreBorrowerFailure(t *testing.T) {
	p, j, m, _ := physicalPublicationFixture(t)
	defer j.Close()
	want := errors.New("actual borrower failed")
	err := p.WithReadAuthority(context.Background(), m, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
		return authorize(func() error {
			_ = borrowMemberWholeReadScope(ctx, p, func(MemberState) error { return want })
			return nil
		})
	})
	if !errors.Is(err, want) {
		t.Fatal("ignored actual borrower failure granted successful barrier", err)
	}
}

func TestPhysicalPublicationRenewForeignOwnerAndUnconfirmedCutRefuse(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	other, _, otherO, _, _ := physicalRenewConfirmedFixture(t)
	before := m.Snapshot().Used
	end := o.end
	otherEnd := otherO.end
	err := withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return otherO.ReplenishFrozen(ctx, 8) })
	if !errors.Is(err, ErrMemberAuthority) || o.end != end || otherO.end != otherEnd || m.Snapshot().Used != before {
		t.Fatal("foreign original owner accepted", err)
	}
	_ = other
	pending := testOrderingRecord(2, p.state.Term, "pending")
	pending.StoreSequenceFirst, pending.StoreSequenceLast = 1, 1
	record, err := j.Append(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	before = m.Snapshot().Used
	err = withPhysicalRenewScope(context.Background(), p, j, m, func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) })
	if !errors.Is(err, ErrMemberAuthority) || o.end != end || m.Snapshot().Used != before {
		t.Fatal("unconfirmed suffix upgraded IO cut", err)
	}
}

func TestPhysicalPublicationRenewInterruptedSyncRetainsOriginalDebt(t *testing.T) {
	p, j, o, m, _ := physicalRenewConfirmedFixture(t)
	before := m.Snapshot().Used
	oldEnd := o.end
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.phase = func(phase string) error {
		if phase == "renewal-receipt-synced" {
			cancel()
		}
		return nil
	}
	err := withPhysicalRenewScope(ctx, p, j, m, func(ctx context.Context) error { return o.ReplenishFrozen(ctx, 8) })
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, context.Canceled) || o.end != oldEnd || o.renewalDebt == (resourcebudget.Permit{}) || o.receiptFile == nil || o.failure() == nil {
		t.Fatal("post-sync interruption lost original owner/debt", err, o.end, o.renewalDebt)
	}
	if after := m.Snapshot().Used; after.Disk != before.Disk || after.RAM <= before.RAM {
		t.Fatal("unknown durable IO refunded its borrowed fee", before, after)
	}
	p.Close()
	if !errors.Is(p.PhysicalCloseError(), ownedfile.ErrRetirement) {
		t.Fatal("source close hid original debt", p.PhysicalCloseError())
	}
	if err = j.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("journal close hid original debt", err)
	}
}

func TestPhysicalPublicationV3FixedHeaderCoversLegalIdentityAndRejectsPadding(t *testing.T) {
	r := physicalPublicationReceipt{Version: 3, ClusterID: string(bytes.Repeat([]byte{1}, MaxIdentityBytes)), Incarnation: string(bytes.Repeat([]byte{'"'}, MaxIdentityBytes)), NodeID: string(bytes.Repeat([]byte{92}, MaxIdentityBytes)), OrderingEnd: 1 << 20}
	key := bytes.Repeat([]byte{9}, 32)
	var err error
	r.MAC, err = physicalReceiptMAC(r, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodePhysicalPublicationReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded physicalPublicationReceipt
	if err = decodePhysicalPublicationReceipt(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ClusterID != r.ClusterID || decoded.Incarnation != r.Incarnation || decoded.NodeID != r.NodeID || !bytes.Equal(decoded.MAC, r.MAC) {
		t.Fatal("legal original identity changed")
	}
	raw[physicalRenewStaticBytes-1] = 1
	if err = decodePhysicalPublicationReceipt(raw, &decoded); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("noncanonical padding accepted", err)
	}
}
