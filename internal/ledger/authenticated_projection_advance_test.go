package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func authenticatedAdvanceFixture(t *testing.T, appendBytes int64) (*AuthenticatedLedgerProjection, *Log, []Record) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native := openChained(t, path, nil)
	var records []Record
	for _, event := range append(projectionTestAttribution(), projectionTestLifecycle(1, true)[0]) {
		r, _, err := native.AppendRecordWithReceipt(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	o := authenticatedLedgerTestOptions(t)
	if appendBytes > 0 {
		o.MaxAppendBytes = appendBytes
	}
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, o)
	if err != nil {
		t.Fatal(err)
	}
	native = openChained(t, path, nil)
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("retired candidate leaked resource owner", used)
		}
	})
	return p, native, records
}

func appendAuthenticatedTail(t *testing.T, native *Log, records []Record) []Record {
	t.Helper()
	for _, e := range projectionTestLifecycle(1, true)[1:] {
		r, _, err := native.AppendRecordWithReceipt(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}
func nativeRecordPrefix(r Record) ProjectionPrefix {
	return ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
}
func compareAdvancedCandidate(t *testing.T, p *AuthenticatedLedgerProjection, native *Log, cut ProjectionPrefix) {
	t.Helper()
	report, partial, err := InspectReplayAuthenticated(native.path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	ref, err := BuildLedgerProjectionPrefix(context.Background(), native.path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report, cut)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Discard()
	rows := compareAuthenticatedLedgerRows(t, p, ref)
	actual, err := p.ReadReplayAnchor(context.Background())
	want, refErr := ref.ReadReplayAnchor(context.Background())
	if err != nil || refErr != nil || actual != want {
		t.Fatal(actual, want, err, refErr)
	}
	t.Logf("allowed generation=%d sequence=%d authenticated rows=%d", actual.Head.Generation, actual.Head.Sequence, rows)
}

func TestAuthenticatedLedgerAdvanceNativeAppendsCompressedRollsAndRolePrefix(t *testing.T) {
	p, native, records := authenticatedAdvanceFixture(t, 0)
	records = appendAuthenticatedTail(t, native, records)
	roll, err := native.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Compact(roll.Sealed.Generation); err != nil {
		t.Fatal(err)
	}
	for _, e := range projectionTestLifecycle(2, true) {
		r, _, err := native.AppendRecordWithReceipt(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if _, err := native.Roll(); err != nil {
		t.Fatal(err)
	}
	cuts := []ProjectionPrefix{nativeRecordPrefix(records[3]), {Head: Watermark{Generation: 2, Sequence: 7}, Hash: records[6].Hash}, {Head: Watermark{Generation: 3, Sequence: 12}, Hash: records[11].Hash}}
	prior := uint64(3)
	for i, cut := range cuts {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			receipt, err := p.AdvanceFromNative(context.Background(), native, cut)
			if err != nil || receipt.From.Head.Sequence != prior || receipt.To.Head != cut.Head || receipt.To.Hash != cut.Hash || receipt.AppliedRecords != cut.Head.Sequence-prior || receipt.VerifiedRecords != 2*(12-prior) || receipt.NativeHead.Head != cuts[2].Head {
				t.Fatal(receipt, prior, err)
			}
			t.Logf("delta facts=%d; verified suffix facts across both passes=%d", receipt.AppliedRecords, receipt.VerifiedRecords)
			compareAdvancedCandidate(t, p, native, cut)
			prior = cut.Head.Sequence
		})
	}
	receipt, err := p.AdvanceFromNative(context.Background(), native, cuts[2])
	if err != nil || receipt.AppliedRecords != 0 || receipt.VerifiedRecords != 0 || receipt.From != receipt.To {
		t.Fatal("exact-cut retry replayed history", receipt, err)
	}
	if _, err := p.AdvanceFromNative(context.Background(), native, cuts[0]); !errors.Is(err, ErrProjectionCursor) {
		t.Fatal("role shrink accepted without fresh reconstruction", err)
	}
	compareAdvancedCandidate(t, p, native, cuts[2])
}

func TestAuthenticatedLedgerAdvanceRejectsCallerCutsOwnerPressureAndBusyWithoutChangingRows(t *testing.T) {
	p, native, records := authenticatedAdvanceFixture(t, 0)
	records = appendAuthenticatedTail(t, native, records)
	from, err := p.ReadReplayAnchor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cut := nativeRecordPrefix(records[3])
	for _, mode := range []string{"wrong-hash", "frame-interior", "beyond-head", "shrink", "wrong-path", "wrong-key", "pressure", "busy-native", "busy-projection", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			allowed, target := cut, native
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := ErrProjectionCursor
			reset := func() {}
			switch mode {
			case "wrong-hash":
				allowed.Hash[0] ^= 1
			case "frame-interior":
				allowed.Head.Offset--
			case "beyond-head":
				allowed.Head.Generation++
			case "shrink":
				allowed = nativeRecordPrefix(records[1])
			case "wrong-path":
				target = openChained(t, filepath.Join(t.TempDir(), "other.wal"), nil)
				reset = func() { _ = target.Close() }
				want = ErrProjectionSource
			case "wrong-key":
				native.mu.Lock()
				native.chainKey[0] ^= 1
				native.mu.Unlock()
				reset = func() { native.mu.Lock(); native.chainKey[0] ^= 1; native.mu.Unlock() }
				want = ErrProjectionSource
			case "pressure":
				if err := p.budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
				reset = func() { _ = p.budget.SetPressure(resourcebudget.Healthy) }
				want = resourcebudget.ErrPressure
				if _, err := p.ReadReplayAnchor(context.Background()); !errors.Is(err, resourcebudget.ErrPressure) || errors.Is(err, ErrProjectionCorrupt) {
					reset()
					t.Fatal("pressure was mislabeled as content corruption", err)
				}
			case "busy-native":
				native.mu.Lock()
				reset = native.mu.Unlock
				want = authenticatedstate.ErrBusy
			case "busy-projection":
				p.mu.Lock()
				reset = p.mu.Unlock
				want = authenticatedstate.ErrBusy
			case "cancel":
				cancel()
				want = context.Canceled
			}
			before := p.budget.Snapshot().Used
			receipt, err := p.AdvanceFromNative(ctx, target, allowed)
			reset()
			if !errors.Is(err, want) || receipt != (AuthenticatedLedgerAdvance{}) {
				t.Fatal(receipt, err, want)
			}
			if after := p.budget.Snapshot().Used; after != before {
				t.Fatal("rejected entry retained work fees", before, after)
			}
			actual, err := p.ReadReplayAnchor(context.Background())
			if err != nil || actual != from || native.Status().Load() != AccountingHealthy {
				t.Fatal(actual, from, err, native.Status().Load())
			}
		})
	}
	compareAdvancedCandidate(t, p, native, ProjectionPrefix{Head: from.Head, Hash: from.Hash})
}

func TestAuthenticatedLedgerAdvanceAuthenticatesExcludedSuffixBeforeAndAfterMutation(t *testing.T) {
	for _, stage := range []string{"first-pass", "second-pass"} {
		t.Run(stage, func(t *testing.T) {
			p, native, records := authenticatedAdvanceFixture(t, 0)
			records = appendAuthenticatedTail(t, native, records)
			cut := nativeRecordPrefix(records[3])
			tamper := func() { tamperResumeFrame(t, native.path, records[5].Offset) }
			var hook func(string)
			if stage == "first-pass" {
				tamper()
			} else {
				hook = func(phase string) {
					if phase == "source-verified" {
						tamper()
					}
				}
			}
			receipt, err := p.advanceFromNative(context.Background(), native, cut, hook)
			if !errors.Is(err, ErrTampered) || receipt != (AuthenticatedLedgerAdvance{}) || native.Status().Load() != AccountingRecoveryRequired {
				t.Fatal(receipt, err, native.Status().Load())
			}
			for _, id := range []string{records[0].Event.EventID, "invented"} {
				if _, found, err := p.ReadEventPosition(context.Background(), id); found || !errors.Is(err, ErrProjectionUnavailable) || !errors.Is(err, ErrProjectionSource) {
					t.Fatal("failed tail published partial candidate", found, err)
				}
			}
			if _, err := p.AdvanceFromNative(context.Background(), native, cut); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("failed candidate revived by retry", err)
			}
			view, err := p.store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			key, _ := authenticatedLedgerKey("meta", []byte("position"))
			raw, found, err := view.Get(context.Background(), key)
			closeErr := view.Close()
			var position projectionPosition
			if err != nil || closeErr != nil || !found || json.Unmarshal(raw, &position) != nil {
				t.Fatal(found, err, closeErr)
			}
			want := uint64(3)
			if stage == "second-pass" {
				want = 4 // Durable private partial root; public queries remain retired.
			}
			if position.Watermark.Sequence != want {
				t.Fatal(position, want)
			}
		})
	}
}

func TestAuthenticatedLedgerAdvanceCancellationQuotaAndNoIntermediateQuery(t *testing.T) {
	for _, mode := range []string{"cancel-before-apply", "cancel-after-apply", "append-quota"} {
		t.Run(mode, func(t *testing.T) {
			quota := int64(0)
			if mode == "append-quota" {
				quota = 256 << 10
			}
			p, native, records := authenticatedAdvanceFixture(t, quota)
			reservation, _, err := native.AppendRecordWithReceipt(context.Background(), projectionTestLifecycle(1, true)[1])
			if err != nil {
				t.Fatal(err)
			}
			last := reservation
			if mode == "append-quota" {
				e := projectionTestLifecycle(2, true)[0]
				e.RequestedModel = strings.Repeat("m", 900<<10)
				last, _, err = native.AppendRecordWithReceipt(context.Background(), e)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := func(phase string) {
				if _, _, err := p.ReadEventPosition(context.Background(), records[0].Event.EventID); !errors.Is(err, authenticatedstate.ErrBusy) {
					t.Fatal("query joined an in-progress publication", err)
				}
				if used := p.budget.Snapshot().Used; used.RAM < AuthenticatedLedgerSourceRAMBytes {
					t.Fatal("source work released before IO finished", used)
				}
				if mode == "cancel-before-apply" && phase == "source-verified" || mode == "cancel-after-apply" && phase == "record-applied" {
					cancel()
				}
			}
			receipt, err := p.advanceFromNative(ctx, native, nativeRecordPrefix(last), hook)
			want := context.Canceled
			if mode == "append-quota" {
				want = authenticatedstate.ErrLimit
			}
			if receipt != (AuthenticatedLedgerAdvance{}) || !errors.Is(err, want) || native.Status().Load() != AccountingHealthy {
				t.Fatal(receipt, err, native.Status().Load())
			}
			if mode == "cancel-before-apply" {
				actual, err := p.ReadReplayAnchor(context.Background())
				if err != nil || actual.Head.Sequence != 3 {
					t.Fatal(actual, err)
				}
			} else if _, err := p.ReadReplayAnchor(context.Background()); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("failed reduction became queryable", err)
			}
		})
	}
}

func TestAuthenticatedLedgerAdvanceCloseCancelsAndJoinsActualWork(t *testing.T) {
	p, native, _ := authenticatedAdvanceFixture(t, 0)
	record, _, err := native.AppendRecordWithReceipt(context.Background(), projectionTestLifecycle(1, true)[1])
	if err != nil {
		t.Fatal(err)
	}
	nativeFile := native.file
	entered, release := make(chan struct{}), make(chan struct{})
	advanceDone, closeDone, nativeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := p.advanceFromNative(context.Background(), native, nativeRecordPrefix(record), func(phase string) {
			if phase == "record-applied" {
				close(entered)
				<-release // Real callback work ignores cancellation until released.
			}
		})
		advanceDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("advance did not enter actual work")
	}
	go func() { closeDone <- p.Close() }()
	go func() { nativeDone <- native.Close() }()
	select {
	case <-p.closeContext.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("owner cancellation not signaled")
	}
	select {
	case err := <-closeDone:
		t.Fatal("Close returned before actual callback exit", err)
	default:
	}
	select {
	case err := <-nativeDone:
		t.Fatal("native Close returned before frozen reader exit", err)
	default:
	}
	if _, err := nativeFile.Stat(); err != nil {
		t.Fatal("native file closed under actual source work", err)
	}
	if used := p.budget.Snapshot().Used; used.RAM < AuthenticatedLedgerSourceRAMBytes {
		t.Fatal("cancellation refunded active source work", used)
	}
	close(release)
	for name, done := range map[string]<-chan error{"advance": advanceDone, "close": closeDone, "native": nativeDone} {
		select {
		case err := <-done:
			if name == "advance" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal(name, "did not join")
		}
	}
	if _, err := nativeFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("native owner did not actually close", err)
	}
	// The canceled source job is joined; only the fixed persistent creator
	// cleanup receipt and its disk obligation survive Close until Discard.
	if used := p.budget.Snapshot().Used; used.RAM != 4096+resourcebudget.ClaimRAMBytes || used.Disk == 0 {
		t.Fatal("physical Close did not retire RAM/preserve persistent disk", used)
	}
}

func TestAuthenticatedLedgerAdvanceDecodesOnlyDeltaAtGrowingActiveHistory(t *testing.T) {
	for _, count := range []int{50, 500} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "ledger.wal")
			native := openChained(t, path, nil)
			for i := 1; i <= count; i++ {
				if _, err := native.Append(ctx, projectionTestLifecycle(i, false)[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := native.Close(); err != nil {
				t.Fatal(err)
			}
			report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
			if err != nil || partial {
				t.Fatal(partial, err)
			}
			o := authenticatedLedgerTestOptions(t)
			p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, o)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			// Open still authenticates the complete history; this test makes no
			// startup IO claim. Only the subsequent live advance is incremental.
			native = openChained(t, path, nil)
			defer native.Close()
			r, _, err := native.AppendRecordWithReceipt(ctx, projectionTestLifecycle(count, false)[1])
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r))
			if err != nil || receipt.From.Head.Sequence != uint64(count) || receipt.To.Head.Sequence != uint64(count+1) || receipt.AppliedRecords != 1 || receipt.VerifiedRecords != 2 {
				t.Fatal(receipt, err)
			}
			compareAdvancedCandidate(t, p, native, nativeRecordPrefix(r))
			t.Logf("history facts=%d; live advance applied=%d; decoded across two native suffix passes=%d", count, receipt.AppliedRecords, receipt.VerifiedRecords)
		})
	}
}

func TestAuthenticatedLedgerCandidateDoesNotUpgradeLegacyCRCIntoNativeAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.wal")
	e := projectionTestLifecycle(1, false)[0]
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encodeFrameVersion(frameVersionLegacy, 1, e.Kind, raw), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ordinary native recovery remains compatible and can append a MAC frame.
	// The later frame authenticates its own data, not the old checksum payload.
	native := openChained(t, path, nil)
	if _, err := native.Append(ctx, projectionTestLifecycle(1, false)[1]); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial || report.ChecksumOnly != 1 || report.Authenticated != 1 {
		t.Fatal(report, partial, err)
	}
	for _, fakeReport := range []bool{false, true} {
		t.Run(fmt.Sprint(fakeReport), func(t *testing.T) {
			expected := report
			if fakeReport {
				expected.ChecksumOnly = 0
			}
			o := authenticatedLedgerTestOptions(t)
			output := filepath.Join(t.TempDir(), "auth")
			p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, output, expected, o)
			if p != nil || !errors.Is(err, ErrProjectionSource) {
				t.Fatal("legacy source gained native authority", p, err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("legacy candidate retained unpublished roots", err)
			}
			if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("legacy refusal retained owners", used)
			}
		})
	}
}
