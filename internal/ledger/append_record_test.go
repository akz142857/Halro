package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type appendRecordResult struct {
	record  Record
	receipt AppendReceipt
	err     error
}

type appendAdmissionFixture struct{ releases int }

func (a *appendAdmissionFixture) Release() error { a.releases++; return nil }

func TestLedgerAdmissionPrecedesNativeWriteAndTransfersToDurableOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	reserved := &appendAdmissionFixture{}
	var passed AppendAdmission
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1,
		BeforeDurable: func(batch DurableBatch) (AppendAdmission, error) {
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() != 0 || len(batch.Frames) == 0 {
				t.Fatalf("admission did not precede native append: size=%v bytes=%d err=%v", info, len(batch.Frames), statErr)
			}
			return reserved, nil
		},
		AfterDurable: func(batch DurableBatch) (uint64, error) {
			passed = batch.Admission
			return 1, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AppendRecordWithReceipt(context.Background(), validReservation("evt_admission", "att_admission")); err != nil {
		t.Fatal(err)
	}
	if passed != reserved || reserved.releases != 0 {
		t.Fatal("successful append did not transfer the held admission", passed, reserved.releases)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if reserved.releases != 0 {
		t.Fatal("source released an admission after transferring it to the durable owner", reserved.releases)
	}
}

func TestLedgerUncertainAppendRetainsAdmissionUntilClose(t *testing.T) {
	for _, mode := range []string{"write", "fsync"} {
		t.Run(mode, func(t *testing.T) {
			reserved := &appendAdmissionFixture{}
			options := Options{ChainKey: testChainKey, MaxBatch: 1,
				BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return reserved, nil },
				AfterDurable:  func(DurableBatch) (uint64, error) { return 1, nil },
				WaitConfirmed: func(context.Context, uint64) error { return nil },
				WrapDurability: func(f *os.File) DurabilityWriter {
					d := &faultDurability{file: f}
					if mode == "write" {
						d.writeErr = errors.New("injected write error")
					} else {
						d.syncErr = errors.New("injected fsync error")
					}
					return d
				},
			}
			l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Append(context.Background(), validReservation("evt_uncertain_"+mode, "att_uncertain_"+mode)); err == nil {
				t.Fatal("injected uncertain append unexpectedly succeeded")
			}
			if reserved.releases != 0 {
				t.Fatal("uncertain native append released its admission before recovery", reserved.releases)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if reserved.releases != 1 {
				t.Fatal("shutdown did not release the uncertain in-process owner", reserved.releases)
			}
		})
	}
}

func TestLedgerAdmissionRefusalLeavesNativeHeadAndFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{
		ChainKey: testChainKey, MaxBatch: 1,
		BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return nil, resourcebudget.ErrExhausted },
		AfterDurable:  func(DurableBatch) (uint64, error) { return 1, nil },
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	beforeHead, beforeHash, beforeOK := l.ChainHead()
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AppendRecordWithReceipt(context.Background(), validReservation("evt_admission_refused", "att_admission_refused")); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatalf("admission refusal error=%v", err)
	}
	afterHead, afterHash, afterOK := l.ChainHead()
	afterInfo, err := os.Stat(path)
	if err != nil || beforeHead != afterHead || beforeHash != afterHash || beforeOK != afterOK || beforeInfo.Size() != afterInfo.Size() {
		t.Fatalf("refused append changed Ledger source: before=(%d,%x,%t,%d) after=(%d,%x,%t,%v,%v)", beforeHead, beforeHash, beforeOK, beforeInfo.Size(), afterHead, afterHash, afterOK, afterInfo, err)
	}
}

func TestAppendRecordMatchesAuthenticatedReplayAcrossBatchesAndRoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	var orderingIndex uint64
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 8, FlushInterval: time.Millisecond,
		AfterDurable:  func(DurableBatch) (uint64, error) { orderingIndex++; return orderingIndex, nil },
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	returned := make(map[uint64]Record)
	for generation := 1; generation <= 2; generation++ {
		results := make(chan appendRecordResult, 32)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				event := validReservation(fmt.Sprintf("evt_%d_%d", generation, i), fmt.Sprintf("att_%d_%d", generation, i))
				if i%2 == 0 {
					r, receipt, err := l.AppendRecordWithReceipt(context.Background(), event)
					results <- appendRecordResult{r, receipt, err}
				} else {
					_, err := l.AppendWithReceipt(context.Background(), event)
					results <- appendRecordResult{err: err}
				}
			}(i)
		}
		wg.Wait()
		close(results)
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.record.Sequence == 0 {
				continue
			}
			r := result.record
			if r.Generation != uint64(generation) || r.Epoch != frameVersionRunAttribution || r.Hash == ([32]byte{}) || result.receipt.Watermark != (Watermark{r.Generation, r.Offset, r.Sequence}) || result.receipt.ReplicationIndex == 0 || !result.receipt.RequiresConfirmation {
				t.Fatal(result)
			}
			returned[r.Sequence] = r
		}
		if generation == 1 {
			if result, err := l.Roll(); err != nil || !result.Rolled {
				t.Fatal(result, err)
			}
		}
	}
	seen := 0
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(actual Record) error {
		if expected, found := returned[actual.Sequence]; found {
			if !reflect.DeepEqual(expected, actual) {
				return fmt.Errorf("returned record %d differs from authenticated WAL", actual.Sequence)
			}
			seen++
		}
		return nil
	})
	if err != nil || partial || seen != 32 || report.Head.Sequence != 64 {
		t.Fatal(report, partial, seen, err)
	}
}

func TestAppendRecordSnapshotAndAcceptedCancellationWaitForDurability(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1,
		AfterDurable:  func(DurableBatch) (uint64, error) { close(entered); <-release; return 42, nil },
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		once.Do(func() { close(release) })
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	event := validReservation("evt_snapshot", "att_snapshot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan appendRecordResult, 1)
	go func() {
		r, receipt, err := l.AppendRecordWithReceipt(ctx, event)
		done <- appendRecordResult{r, receipt, err}
	}()
	<-entered
	// The pointer is mutated only after Marshal has completed and the ordering
	// hook is blocked. This is a legal ownership change, not a concurrent Marshal.
	*event.ReservationMicrosUSD = 999
	cancel()
	select {
	case result := <-done:
		t.Fatal("accepted append returned before ordering hook", result)
	case <-time.After(10 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	result := <-done
	if result.err != nil || result.receipt.ReplicationIndex != 42 || *result.record.Event.ReservationMicrosUSD != 100 {
		t.Fatal(result)
	}
	var actual Record
	if _, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error { actual = r; return nil }); err != nil || partial || !reflect.DeepEqual(actual, result.record) {
		t.Fatal(actual, result, partial, err)
	}
	*result.record.Event.ReservationMicrosUSD = 888
	if *event.ReservationMicrosUSD != 999 || *actual.Event.ReservationMicrosUSD != 100 {
		t.Fatal("returned evidence aliases caller or replay", event, actual)
	}
}

func TestAppendRecordNeverReturnsEvidenceOnDurabilityFailure(t *testing.T) {
	for _, mode := range []string{"write", "fsync", "ordering", "zero-index"} {
		t.Run(mode, func(t *testing.T) {
			status := NewStatus()
			options := Options{ChainKey: testChainKey, MaxBatch: 1}
			options.WrapDurability = func(f *os.File) DurabilityWriter {
				d := &faultDurability{file: f}
				if mode == "write" {
					d.writeErr = errors.New("injected write failure")
				}
				if mode == "fsync" {
					d.syncErr = errors.New("injected fsync failure")
				}
				return d
			}
			if mode == "ordering" || mode == "zero-index" {
				options.AfterDurable = func(DurableBatch) (uint64, error) {
					if mode == "ordering" {
						return 0, errors.New("injected ordering failure")
					}
					return 0, nil
				}
				options.WaitConfirmed = func(context.Context, uint64) error { return nil }
			}
			l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), status, options)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			r, receipt, err := l.AppendRecordWithReceipt(context.Background(), validReservation("evt_fail", "att_fail"))
			if err == nil || !reflect.DeepEqual(r, Record{}) || receipt.Watermark != (Watermark{}) || status.Load() != AccountingUnavailable {
				t.Fatal(r, receipt, err, status.Load())
			}
			if _, _, err := l.AppendRecordWithReceipt(context.Background(), validReservation("evt_after_fail", "att_after_fail")); err == nil {
				t.Fatal("uncertain append did not close further writes")
			}
		})
	}
}

func TestAppendRecordAppliesActualRecoveryFactsToProjection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	events := projectionTestLifecycle(1, false)
	for _, e := range events[:2] {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	expected, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(expected, partial, err)
	}
	p, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(dir, "projection.db"), expected)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, e := range events[2:] {
		r, receipt, err := l.AppendRecordWithReceipt(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ReplicationIndex != 0 {
			t.Fatal("Standalone has fabricated confirmation", receipt)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		head, hash, err := p.ReadPosition(ctx)
		nativeHead, nativeHash, _ := l.ChainHead()
		if err != nil || head != nativeHead || hash != nativeHash {
			t.Fatal(head, nativeHead, hash, nativeHash, err)
		}
	}
	request, found, err := p.ReadRequest(ctx, events[0].RequestID)
	if err != nil || !found || request.Finalized == nil || request.Finalized.Sequence != 5 {
		t.Fatal(request, found, err)
	}
	page, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
	if err != nil || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
}

func TestAppendApplyRecordKeepsDurableReceiptOnCallbackFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, err := l.AppendApplyRecordWithReceipt(context.Background(), validReservation("evt_nil_apply", "att_nil_apply"), nil); err == nil || l.Stats().Records != 0 {
		t.Fatal("nil Apply admitted a native fact", err)
	}
	failure := errors.New("controlled ordinary Apply failure")
	var applied Record
	record, receipt, err := l.AppendApplyRecordWithReceipt(context.Background(), validReservation("evt_failed_apply", "att_failed_apply"), func(ctx context.Context, r Record) error {
		// ChainHead acquires the native mutex: this also proves callback isolation
		// from the writer lock rather than merely inspecting the implementation.
		head, hash, _ := l.ChainHead()
		if head.Sequence != r.Sequence || hash != r.Hash || ctx.Err() != nil {
			t.Error("callback is not bound to native durable evidence")
		}
		applied = r
		return failure
	})
	if !errors.Is(err, failure) || !reflect.DeepEqual(record, Record{}) || receipt.Watermark.Sequence != 1 || applied.Sequence != 1 || l.Status().Load() != AccountingRecoveryRequired {
		t.Fatal(record, receipt, err, l.Status().Load())
	}
	var actual Record
	if _, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error { actual = r; return nil }); err != nil || partial || !reflect.DeepEqual(applied, actual) {
		t.Fatal("durable callback failure hid or changed native facts", partial, err)
	}
	if _, _, err := l.AppendApplyRecordWithReceipt(context.Background(), validReservation("evt_after_failure", "att_after_failure"), func(context.Context, Record) error { return nil }); err == nil || l.Stats().Records != 1 {
		t.Fatal("failed Apply allowed another append", err)
	}
}

func TestAppendApplyRecordCloseCancelsAndJoinsCallbackBeforeClearingKey(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }); l.Close() }()
	done := make(chan appendRecordResult, 1)
	go func() {
		r, receipt, err := l.AppendApplyRecordWithReceipt(context.Background(), validReservation("evt_close_apply", "att_close_apply"), func(ctx context.Context, r Record) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		})
		done <- appendRecordResult{r, receipt, err}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel ordinary callback context")
	}
	l.mu.Lock()
	keyPresent, filePresent := len(l.chainKey) == 32, l.file != nil
	l.mu.Unlock()
	if !keyPresent || !filePresent {
		t.Fatal("Close cleared resources before callback join")
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before callback", err)
	case <-time.After(10 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	result := <-done
	if !errors.Is(result.err, context.Canceled) || !reflect.DeepEqual(result.record, Record{}) || result.receipt.Watermark.Sequence != 1 {
		t.Fatal(result)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join callback")
	}
	l.mu.Lock()
	cleared := len(l.chainKey) == 0 && l.file == nil
	l.mu.Unlock()
	if !cleared || l.Status().Load() != AccountingRecoveryRequired {
		t.Fatal("Close did not retire failed callback resources")
	}
}

func TestAppendApplyRecordRetainsOwnedContextAfterCallerCancellation(t *testing.T) {
	type ownerKey struct{}
	owner := new(int)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, owner))
	defer cancel()
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{
		ChainKey: testChainKey, MaxBatch: 1,
		AfterDurable: func(batch DurableBatch) (uint64, error) {
			cancel()
			return batch.LastSequence, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	record, receipt, err := l.AppendApplyRecordWithReceipt(ctx, validReservation("evt_context_owner", "att_context_owner"), func(applyCtx context.Context, record Record) error {
		if ctx.Err() == nil || applyCtx.Err() != nil || applyCtx.Value(ownerKey{}) != owner || record.Sequence != 1 {
			t.Fatal("accepted Apply lost its actual owner or inherited caller cancellation", record, applyCtx.Err())
		}
		return nil
	})
	if err != nil || record.Sequence != 1 || receipt.Watermark.Sequence != 1 || l.Status().Load() != AccountingHealthy {
		t.Fatal(record, receipt, err)
	}
}
