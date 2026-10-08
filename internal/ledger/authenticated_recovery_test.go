package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func recoveryViewFixture(t *testing.T) (*AuthenticatedLedgerProjection, *Log, *LedgerProjection, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "native.wal")
	native := openChained(t, path, nil)
	appendFact := func(e Event) {
		t.Helper()
		if _, _, err := native.AppendRecordWithReceipt(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range projectionTestAttribution() {
		appendFact(e)
	}
	for i := 1; i <= 8; i++ {
		events := projectionTestLifecycle(i, true)
		// Every active request has a pending, a settled, or no attempt at all.
		// Finalized requests can still have a pending attempt and need input stream 1.
		end := 2
		if i == 1 {
			end = 1
		}
		if i == 2 {
			end = 4
		}
		for _, e := range events[:end] {
			appendFact(e)
		}
		if i == 3 {
			appendFact(events[4])
		}
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	options := authenticatedLedgerTestOptions(t)
	directory := filepath.Join(t.TempDir(), "auth")
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, directory, report, options)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if err := ref.Discard(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if used := options.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("resource leak", used)
		}
	})
	return p, native, ref, directory
}
func drainRecoveryInputs(t *testing.T, v AuthenticatedRecoveryView) {
	t.Helper()
	ctx := context.Background()
	cursor := ""
	for {
		page, err := v.PendingLeasePage(ctx, cursor, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if page.Done {
			break
		}
		cursor = page.Next
	}
	cursor = ""
	for {
		page, err := v.UnfinalizedRequestPage(ctx, cursor, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if page.Done {
			break
		}
		cursor = page.Next
	}
}

func TestAuthenticatedRecoveryFixedRootInputsAndNativeAdvance(t *testing.T) {
	p, native, ref, _ := recoveryViewFixture(t)
	ctx := context.Background()
	expectedPending := map[string]PendingLease{}
	expectedRequests := map[string]ProjectedRequest{}
	scan, err := ref.BeginRecoveryScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for c := scan.Pending; ; {
		page, err := ref.PendingLeasePage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			expectedPending[item.Reservation.AttemptID] = item
		}
		if page.Done {
			break
		}
		c = page.Next
	}
	for c := scan.Requests; ; {
		page, err := ref.UnfinalizedRequestPage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			origin, err := item.RecoveryOrigin()
			if err != nil {
				t.Fatal(err)
			}
			expectedRequests[origin.Event.RequestID] = item
		}
		if page.Done {
			break
		}
		c = page.Next
	}
	var escaped AuthenticatedRecoveryView
	err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		escaped = v
		if v.Head().Head != scan.Head {
			t.Fatal("input sets differ")
		}
		if err := p.WithRecoveryView(ctx, func(AuthenticatedRecoveryView) error { return nil }); !errors.Is(err, authenticatedstate.ErrBusy) {
			t.Fatal("unbounded nested callback", err)
		}
		pending := map[string]PendingLease{}
		first, err := v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range first.Items {
			pending[item.Reservation.AttemptID] = item
		}
		// A real terminal fact and a new admission update the active root between
		// input pages. The pinned input must retain the terminal attempt and exclude
		// all newly admitted input, while the current root can continue advancing.
		var last Record
		for _, e := range append(projectionTestLifecycle(4, true)[3:4], projectionTestLifecycle(99, true)[:2]...) {
			r, _, err := native.AppendRecordWithReceipt(ctx, e)
			if err != nil {
				return err
			}
			last = r
		}
		if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(last)); err != nil {
			return err
		}
		originalID := projectionTestLifecycle(4, true)[0].RequestID
		original, found, err := v.ReadRequest(ctx, originalID)
		wantOriginal, wantFound, wantErr := ref.ReadRequest(ctx, originalID)
		if err != nil || wantErr != nil || !found || !wantFound || !reflect.DeepEqual(original, wantOriginal) {
			t.Fatal("request dependency moved off original cut", original, wantOriginal, err, wantErr)
		}
		if _, found, err := v.ReadRequest(ctx, projectionTestLifecycle(99, true)[0].RequestID); err != nil || found {
			t.Fatal("old request pin included a newly admitted request", found, err)
		}
		for c := first.Next; !first.Done; {
			page, err := v.PendingLeasePage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				pending[item.Reservation.AttemptID] = item
			}
			if page.Done {
				break
			}
			c = page.Next
		}
		if !reflect.DeepEqual(pending, expectedPending) {
			t.Fatal("pending input changed")
		}
		requests := map[string]ProjectedRequest{}
		for c := ""; ; {
			page, err := v.UnfinalizedRequestPage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				origin, err := item.RecoveryOrigin()
				if err != nil {
					return err
				}
				requests[origin.Event.RequestID] = item
				// Entire request->attempt membership, including settled attempts, is
				// authenticated independently of the pending input set.
				want := []ProjectedAttempt{}
				for rc := (ProjectionCursor{}); ; {
					pp, err := ref.RequestAttemptPage(ctx, origin.Event.RequestID, rc, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
					if err != nil {
						return err
					}
					want = append(want, pp.Items...)
					if pp.Done {
						break
					}
					rc = pp.Next
				}
				got := []ProjectedAttempt{}
				for ac := ""; ; {
					pp, err := v.RequestAttemptPage(ctx, origin.Event.RequestID, ac, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
					if err != nil {
						return err
					}
					got = append(got, pp.Items...)
					if pp.Done {
						break
					}
					ac = pp.Next
				}
				if !reflect.DeepEqual(want, got) {
					t.Fatal("attempt membership differs", origin.Event.RequestID)
				}
			}
			if page.Done {
				break
			}
			c = page.Next
		}
		if !reflect.DeepEqual(requests, expectedRequests) {
			t.Fatal("request input changed")
		}
		lease, found, err := v.ReadLease(ctx, projectionTestLifecycle(4, true)[1].AttemptID)
		if err != nil || !found || lease.Event.Kind != EventReservationCreated {
			t.Fatal(found, err)
		}
		if _, found, err := v.ReadLease(ctx, "never_existed"); err != nil || found {
			t.Fatal(found, err)
		}
		t.Logf("fixed sequence=%d pending=%d requests=%d; current sequence=%d", v.Head().Head.Sequence, len(pending), len(requests), last.Sequence)
		return v.Complete(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.ReadLease(ctx, "never_existed"); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("escaped view usable", err)
	}
	if _, _, err := escaped.ReadRequest(ctx, "never_existed"); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("escaped request dependency usable", err)
	}
}

func TestAuthenticatedRecoveryCompleteAndCursorLimits(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	before := p.budget.Snapshot().Used
	err := p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		if err := v.Complete(ctx); !errors.Is(err, ErrAuthenticatedRecoveryIncomplete) {
			t.Fatal(err)
		}
		page, err := v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1})
		if !errors.Is(err, ErrProjectionPageLimit) || len(page.Items) != 0 {
			t.Fatal(page, err)
		}
		page, err = v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.UnfinalizedRequestPage(ctx, page.Next, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}); !errors.Is(err, ErrProjectionCursor) {
			t.Fatal("cross-stream cursor accepted", err)
		}
		// Continue the real pending cursor; wrong cursors/too-small pages did not
		// advance the request stream or retire a healthy immutable input root.
		for c := page.Next; !page.Done; {
			pp, err := v.PendingLeasePage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			if pp.Done {
				break
			}
			c = pp.Next
		}
		for c := ""; ; {
			pp, err := v.UnfinalizedRequestPage(ctx, c, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			if pp.Done {
				break
			}
			c = pp.Next
		}
		return v.Complete(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if after := p.budget.Snapshot().Used; after != before {
		t.Fatal("scope leak", before, after)
	}
	if err := p.WithRecoveryView(ctx, func(AuthenticatedRecoveryView) error { return nil }); !errors.Is(err, ErrAuthenticatedRecoveryIncomplete) {
		t.Fatal(err)
	}
}

func TestAuthenticatedRecoveryCloseJoinsWholeCallback(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	baseline := p.budget.Snapshot().Used
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	go func() {
		returned <- p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
			drainRecoveryInputs(t, v)
			if err := v.Complete(ctx); err != nil {
				return err
			}
			close(entered)
			<-release // Simulate an accepted callback that cannot stop until real IO exits.
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-returned:
		t.Fatal("callback exited before barrier", err)
	case <-time.After(5 * time.Second):
		t.Fatal("callback failed to enter")
	}
	during := p.budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close returned before callback", err)
	case <-time.After(30 * time.Millisecond):
	}
	if used := p.budget.Snapshot().Used; used != during {
		t.Fatal("Close refunded live workspace", during, used)
	}
	if _, err := p.ReadReplayAnchor(ctx); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("Close still allowed query", err)
	}
	close(release)
	if err := <-returned; err == nil {
		t.Fatal("callback completed after owner cancellation")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if used := p.budget.Snapshot().Used; used != baseline {
		t.Fatal("Close lifetime fees", used)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if used := p.budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
		t.Fatal("physical retirement retained fees", used)
	}
}

func TestAuthenticatedRecoveryCorruptionHasNoPartialPageOrFalseAbsence(t *testing.T) {
	p, _, ref, directory := recoveryViewFixture(t)
	ctx := context.Background()
	// Locate a real pending blob from the fresh reference; modifying only its
	// bytes leaves the signed root and complete key set unchanged.
	var target []byte
	err := ref.db.View(func(tx *bbolt.Tx) error {
		target = bytes.Clone(tx.Bucket([]byte("pending")).Get(projectionIDKey("att_000004")))
		return nil
	})
	if err != nil || len(target) == 0 {
		t.Fatal(err)
	}
	err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		path := filepath.Join(directory, "pages")
		pack, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		at := bytes.Index(pack, target)
		if at < 0 {
			t.Fatal("real pending blob absent")
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteAt([]byte{target[0] ^ 1}, int64(at))
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
		page, err := v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
		if !errors.Is(err, authenticatedstate.ErrCorrupt) || len(page.Items) != 0 || page.Next != "" {
			t.Fatal("corruption delivered partial page", page, err)
		}
		for _, id := range []string{"att_000004", "invented_attempt"} {
			if _, found, err := v.ReadLease(ctx, id); err == nil || found {
				t.Fatal("fault turned into absence", found, err)
			}
		}
		for _, id := range []string{projectionTestLifecycle(4, true)[0].RequestID, "invented_request"} {
			if _, found, err := v.ReadRequest(ctx, id); err == nil || found {
				t.Fatal("fault turned request dependency into absence", found, err)
			}
		}
		if err := v.Complete(ctx); err == nil {
			t.Fatal("ignored corruption completed")
		}
		return nil // Ignoring the inner error still cannot complete the scope.
	})
	if !errors.Is(err, authenticatedstate.ErrCorrupt) {
		t.Fatal(err)
	}
	if _, found, err := p.ReadEventPosition(ctx, "invented_event"); err == nil || found {
		t.Fatal("failed candidate remained usable", found, err)
	}
}

func TestAuthenticatedRecoveryGenesisRolePrefixAndCancellation(t *testing.T) {
	_, native, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	report, partial, err := InspectReplayAuthenticated(native.path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	options := authenticatedLedgerTestOptions(t)
	options.Prefix = &ProjectionPrefix{}
	p, err := BuildAuthenticatedLedgerProjection(ctx, native.path, testChainKey, filepath.Join(t.TempDir(), "prefix"), report, options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		if v.Head() != (ReplayAnchor{}) {
			t.Fatal("excluded suffix affected Head", v.Head())
		}
		pending, err := v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil || !pending.Done || len(pending.Items) != 0 {
			t.Fatal(pending, err)
		}
		requests, err := v.UnfinalizedRequestPage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil || !requests.Done || len(requests.Items) != 0 {
			t.Fatal(requests, err)
		}
		return v.Complete(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	called := false
	err = p.WithRecoveryView(cancelled, func(AuthenticatedRecoveryView) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal(err, called)
	}
	if _, err := p.ReadReplayAnchor(ctx); err != nil {
		t.Fatal("caller cancellation retired healthy candidate", err)
	}
}

func TestAuthenticatedRecoveryPressureAndFailedAdvanceWithdrawOldView(t *testing.T) {
	p, native, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	before := p.budget.Snapshot().Used
	if err := p.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	called := false
	err := p.WithRecoveryView(ctx, func(AuthenticatedRecoveryView) error { called = true; return nil })
	if !errors.Is(err, resourcebudget.ErrPressure) || errors.Is(err, ErrProjectionCorrupt) || called {
		t.Fatal(err, called)
	}
	if after := p.budget.Snapshot().Used; after != before {
		t.Fatal("rejected scope retained credit")
	}
	if err := p.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		drainRecoveryInputs(t, v)
		bad := projectionTestLifecycle(42, false)[2] // valid native event, absent reservation
		r, _, err := native.AppendRecordWithReceipt(ctx, bad)
		if err != nil {
			return err
		}
		receipt, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r))
		if err == nil || receipt != (AuthenticatedLedgerAdvance{}) {
			t.Fatal("invalid derived transition committed", receipt, err)
		}
		if _, found, err := v.ReadLease(ctx, "invented_attempt"); !errors.Is(err, ErrProjectionUnavailable) || found {
			t.Fatal("retired owner allowed old-root absence", found, err)
		}
		if err := v.Complete(ctx); !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal("withdrawn input completed", err)
		}
		return nil // Final scope fence must also reject the ignored withdrawal.
	})
	if !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
	if after := p.budget.Snapshot().Used; after != before {
		t.Fatal("retired scope leaked RAM/pin", before, after)
	}
}
