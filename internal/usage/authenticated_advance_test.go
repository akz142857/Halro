package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

type usageAdvanceFixture struct {
	native     *ledger.Log
	owner      *ledger.AuthenticatedLedgerProjection
	source     *AuthenticatedUsageProjection
	derivative *LedgerDerivative
	live       *DiskProjection
	budget     *resourcebudget.Manager
	path       string
}

func newUsageAdvanceFixture(t *testing.T) *usageAdvanceFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "ledger.wal")
	key := bytes.Repeat([]byte{0x63}, 32)
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	// The snapshot precedes finalization and the late settlement.
	for _, r := range finalizationFixture(true, false)[:3] {
		if _, err := native.Append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 512 << 20}, MaxClaims: 64, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	owner, err := ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(root, "ledger-auth"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("delta-ledger")), MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Discard(); err != nil {
			t.Error(err)
		}
	})
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
	})
	source, err := BuildAuthenticatedUsageProjection(ctx, AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: sha256.Sum256([]byte("delta-usage")), UsageKey: bytes.Repeat([]byte{0x91}, 32), ScratchPath: filepath.Join(root, "scratch.db"), RootDirectory: filepath.Join(root, "usage-auth"), ScratchMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20, ScratchReservation: credit, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Discard(); err != nil {
			t.Error(err)
		}
	})
	d, p, err := RestoreDiskLedgerDerivative(ctx, source, owner, filepath.Join(root, "restored.db"), 16<<20, credit, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Close()
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	return &usageAdvanceFixture{native: native, owner: owner, source: source, derivative: d, live: p, budget: budget, path: path}
}
func (f *usageAdvanceFixture) append(t *testing.T, n int) []ledger.Record {
	t.Helper()
	var records []ledger.Record
	for i := 0; i < n; i++ {
		e := finalizationFixture(false, false)[0].Event
		e.EventID = fmt.Sprint("delta-event-", i)
		e.RequestID = fmt.Sprint("delta-request-", i)
		r, _, err := f.native.AppendRecordWithReceipt(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}
func (f *usageAdvanceFixture) advance(t *testing.T, r ledger.Record) {
	t.Helper()
	_, err := f.owner.AdvanceFromNative(context.Background(), f.native, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *usageAdvanceFixture) lease(t *testing.T) *ledger.MaintenanceLease {
	t.Helper()
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return lease
}

func TestRestoredUsageAdvancePermittedPrefixAndLateSettlement(t *testing.T) {
	f := newUsageAdvanceFixture(t)
	ctx := context.Background()
	var tail []ledger.Record
	for _, fact := range finalizationFixture(true, false)[3:] {
		r, _, err := f.native.AppendRecordWithReceipt(ctx, fact.Event)
		if err != nil {
			t.Fatal(err)
		}
		tail = append(tail, r)
	}
	f.advance(t, tail[0])
	lease := f.lease(t)
	receipt, err := f.derivative.CatchUpAuthenticatedFromNative(ctx, f.native, lease, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AppliedRecords != 1 || receipt.VerifiedRecords != 4 || receipt.To.Head.Sequence != tail[0].Sequence || receipt.NativeHead.Sequence != tail[1].Sequence {
		t.Fatal(receipt)
	}
	first, err := f.live.ReadStats(ctx)
	if err != nil || first.Metrics.ActiveRequests != 0 {
		t.Fatal(first, err)
	}
	// Advance the already selected Ledger to the second explicitly permitted cut
	// while the native lease continues to exclude semantic writers.
	f.advance(t, tail[1])
	receipt, err = f.derivative.CatchUpAuthenticatedFromNative(ctx, f.native, lease, f.budget)
	if err != nil || receipt.AppliedRecords != 1 {
		t.Fatal(receipt, err)
	}
	last, err := f.live.ReadStats(ctx)
	if err != nil || last.Metrics.ActiveRequests != 0 || last.Totals.CostMicrosUSD != 7 {
		t.Fatal(last, err)
	}
	retry, err := f.derivative.CatchUpAuthenticatedFromNative(ctx, f.native, lease, f.budget)
	if err != nil || retry.AppliedRecords != 0 {
		t.Fatal(retry, err)
	}
	after, err := f.live.ReadStats(ctx)
	if err != nil || after != last {
		t.Fatal("retry changed stats", after, err)
	}
	// A cold independent reducer at the same permitted cut agrees exactly.
	coldD, cold, err := NewDiskLedgerDerivativeWithLimit(f.owner, filepath.Join(t.TempDir(), "cold.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		coldD.Close()
		if err := cold.Discard(); err != nil {
			t.Error(err)
		}
	}()
	var facts []ledger.Record
	if _, err := f.native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error { facts = append(facts, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := coldD.ApplyReplayBatchContext(ctx, facts); err != nil {
		t.Fatal(err)
	}
	expected, err := cold.ReadStats(ctx)
	if err != nil || expected.Watermark != last.Watermark || expected.Metrics != last.Metrics || expected.Totals != last.Totals {
		t.Fatal("cold/recovered semantic divergence", expected, last, err)
	}

}

func TestRestoredUsageAdvanceRefusesExcludedTailTamperBeforeMutation(t *testing.T) {
	f := newUsageAdvanceFixture(t)
	records := f.append(t, 2)
	f.advance(t, records[0])
	lease := f.lease(t)
	before, err := f.live.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	offset := records[0].Offset + 16
	if _, err := file.ReadAt(b[:], offset); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := file.WriteAt(b[:], offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.derivative.CatchUpAuthenticatedFromNative(context.Background(), f.native, lease, f.budget)
	if err == nil || receipt != (AuthenticatedUsageAdvance{}) {
		t.Fatal(receipt, err)
	}
	if !f.live.retired.Load() {
		t.Fatal("source corruption did not withdraw owner")
	}
	// Internal metadata remains unchanged even though publication is withdrawn.
	f.live.mu.Lock()
	var got diskUsageState
	readErr := f.live.db.View(func(tx *bbolt.Tx) error {
		v, _, e := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		got = v
		return e
	})
	f.live.mu.Unlock()
	if readErr != nil || got.Watermark != before.Watermark {
		t.Fatal("unpermitted/corrupt tail mutated view", got, readErr)
	}
}

func TestRestoredUsageAdvanceCancellationAfterBatchWithdrawsOwner(t *testing.T) {
	f := newUsageAdvanceFixture(t)
	records := f.append(t, 2)
	f.advance(t, records[1])
	lease := f.lease(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receipt, err := f.derivative.catchUpAuthenticatedFromNative(ctx, f.native, lease, f.budget, func(phase string) {
		if phase == "batch-applied" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || receipt != (AuthenticatedUsageAdvance{}) || !f.live.retired.Load() {
		t.Fatal(receipt, err)
	}
	if _, err := f.live.ReadStats(context.Background()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal("partial job remained published", err)
	}
}

func TestRestoredUsageCloseCancelsAndJoinsWholeAdvance(t *testing.T) {
	for _, closeDerivative := range []bool{false, true} {
		t.Run(fmt.Sprint("derivative=", closeDerivative), func(t *testing.T) {
			f := newUsageAdvanceFixture(t)
			records := f.append(t, 2)
			f.advance(t, records[1])
			lease := f.lease(t)
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := f.derivative.catchUpAuthenticatedFromNative(context.Background(), f.native, lease, f.budget, func(phase string) {
					if phase == "source-verified" {
						close(entered)
						<-release
					}
				})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("source phase not reached")
			}
			closed := make(chan error, 1)
			go func() {
				if closeDerivative {
					f.derivative.Close()
					closed <- nil
				} else {
					closed <- f.live.Close()
				}
			}()
			select {
			case err := <-closed:
				t.Fatal("Close did not join active source job", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("advance did not cancel")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close did not join")
			}
			f.live.catchUpMu.Lock()
			pending := f.live.catchUpDone != nil || f.live.catchUpCancel != nil
			f.live.catchUpMu.Unlock()
			if pending || f.live.catchUpActive.Load() {
				t.Fatal("active job slot retained")
			}
		})
	}
}
