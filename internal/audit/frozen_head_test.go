package audit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func frozenHeadFixture(t *testing.T) (*Log, *resourcebudget.Manager) {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "native.log"), bytes.Repeat([]byte{31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil && !errors.Is(err, ownedfile.ErrRetirement) {
			t.Error(err)
		}
	})
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 2 << 20, Disk: 2 << 20}, MaxClaims: 16, MaxOwners: 8, MaxPressureAge: time.Hour}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: 1 << 20}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validEvent(1, "key.create")); err != nil {
		t.Fatal(err)
	}
	return l, budget
}

func TestAuditFrozenHeadNestedBorrowersAreBoundedAndJoinedBeforeRelease(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	entered, canceled := make(chan struct{}, 4), make(chan struct{}, 4)
	loanResults := make(chan error, 4)
	lent := make(chan context.Context, 1)
	rootResult := make(chan error, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		rootResult <- l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, head Summary) error {
			for n := 0; n < 4; n++ {
				go func() {
					loanResults <- l.WithFrozenHead(ctx, budget, func(child context.Context, actual Summary) error {
						if actual != head {
							return errors.New("borrower substituted the frozen head")
						}
						entered <- struct{}{}
						<-child.Done()
						canceled <- struct{}{}
						<-release
						return nil
					})
				}()
			}
			for n := 0; n < 4; n++ {
				<-entered
			}
			if err := l.WithFrozenHead(ctx, budget, func(context.Context, Summary) error { return errors.New("fifth borrower entered") }); !errors.Is(err, ErrNativeFrozen) {
				return errors.Join(errors.New("borrower ceiling failed"), err)
			}
			if err := (&Log{}).WithFrozenHead(ctx, budget, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
				return errors.Join(errors.New("borrower substituted native owner"), err)
			}
			if err := l.WithFrozenHead(ctx, &resourcebudget.Manager{}, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
				return errors.Join(errors.New("borrower substituted budget owner"), err)
			}
			lent <- ctx
			return nil // The physical job must still join all four accepted loans.
		})
	}()
	var ctx context.Context
	select {
	case ctx = <-lent:
	case <-time.After(5 * time.Second):
		t.Fatal("nested loans not admitted")
	}
	scope := ctx.Value(frozenHeadScopeKey{}).(*frozenHeadScope)
	deadline := time.Now().Add(5 * time.Second)
	for {
		scope.mu.Lock()
		active := scope.active
		scope.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("outer callback did not withdraw loan admission")
		}
		time.Sleep(time.Millisecond)
	}
	if err := l.WithFrozenHead(ctx, budget, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
		t.Fatal("withdrawn scope admitted new loan", err)
	}
	if err := l.WithFrozenHead(context.Background(), budget, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
		t.Fatal("independent physical freeze entered before loan retirement", err)
	}
	select {
	case err := <-rootResult:
		t.Fatal("root returned before loan retirement", err)
	default:
	}
	used := budget.Snapshot().Used
	if used.RAM < 4096 {
		t.Fatal("whole-job fee vanished with live loans", used)
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	for n := 0; n < 4; n++ {
		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not cancel a nested loan")
		}
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before nested callbacks retired", err)
	default:
	}
	if budget.Snapshot().Used != used {
		t.Fatal("Close refunded live nested callbacks", budget.Snapshot())
	}
	close(release)
	for n := 0; n < 4; n++ {
		if err := <-loanResults; !errors.Is(err, context.Canceled) {
			t.Fatal("canceled borrower returned success", err)
		}
	}
	if err := <-rootResult; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled root returned success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("retired loans leaked fees", budget.Snapshot())
	}
	if err := l.WithFrozenHead(context.WithoutCancel(ctx), budget, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
		t.Fatal("saved context started a successor job", err)
	}
}

func TestAuditFrozenHeadBorrowerActualRetirementDebtWithdrawsLoanAdmission(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	var actual *os.File
	boundaryChecked := false
	err := l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, head Summary) error {
		// Install after the outer source verification, then fail a real nested
		// source-reader Close. The enclosing callback deliberately ignores that
		// failure; the private scope must retain it and withdraw further loans.
		l.closeReplayFile = func(file *os.File) error { actual = file; return errors.Join(file.Close(), file.Close()) }
		if err := l.WithFrozenHead(ctx, budget, func(child context.Context, frozen Summary) error {
			_, err := l.SealReference(child, budget, frozen)
			return err
		}); !errors.Is(err, ownedfile.ErrRetirement) || actual == nil {
			return errors.Join(errors.New("nested actual retirement was not exercised"), err)
		}
		entered := false
		if err := l.WithFrozenHead(ctx, budget, func(context.Context, Summary) error { entered = true; return nil }); !errors.Is(err, ErrNativeFrozen) || !errors.Is(err, ownedfile.ErrRetirement) || entered {
			return errors.Join(errors.New("uncertain borrower admitted another loan"), err)
		}
		boundaryChecked = true
		return nil
	})
	if !boundaryChecked || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("outer job ignored actual borrower debt", err)
	}
	if budget.Snapshot().Used.RAM < 4096 || len(l.key) != 32 {
		t.Fatal("actual borrower debt erased whole-job resources", budget.Snapshot(), len(l.key))
	}
}

func TestAuditFrozenHeadExcludesSemanticPhysicalWritesAndPreservesCandidateRejection(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	before := l.Summary()
	rejection := errors.New("candidate rejected")
	err := l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, head Summary) error {
		if head != before {
			t.Fatal("wrong frozen cut", head, before)
		}
		if _, err := l.Append(ctx, validEvent(2, "key.create")); !errors.Is(err, ErrNativeFrozen) {
			t.Fatal("semantic write admitted", err)
		}
		if err := l.AppendReplicated(2, 2, []byte("must not decode")); !errors.Is(err, ErrNativeFrozen) {
			t.Fatal("physical write admitted", err)
		}
		if err := l.WithFrozenHead(context.Background(), budget, func(context.Context, Summary) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
			t.Fatal("second job admitted", err)
		}
		used := budget.Snapshot().Used
		if err := l.WithFrozenHead(ctx, budget, func(nested context.Context, borrowed Summary) error {
			if borrowed != head || nested.Err() != nil || budget.Snapshot().Used != used {
				t.Fatal("nested read did not borrow the original head/lifetime", borrowed, head, nested.Err(), budget.Snapshot())
			}
			return nil
		}); err != nil {
			t.Fatal("legitimate nested cache capture rejected", err)
		}
		if _, err := l.SealReference(ctx, budget, head); err != nil {
			t.Fatal("frozen source cannot be read", err)
		}
		return rejection
	})
	if !errors.Is(err, rejection) || l.Summary() != before {
		t.Fatal("rejection changed native authority", err, l.Summary())
	}
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("job leaked fees", budget.Snapshot())
	}
	if _, err := l.Append(context.Background(), validEvent(2, "key.create")); err != nil {
		t.Fatal("candidate rejection poisoned healthy native", err)
	}
}

func TestAuditFrozenHeadCloseCancelsAndJoinsWholeCallback(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	returned := make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		returned <- l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, _ Summary) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job not entered")
	}
	before := budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("Close did not join callback", err)
	case <-time.After(25 * time.Millisecond):
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("live callback refunded", before, got)
	}
	close(release)
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatal("closed job returned success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("known exit leaked", got)
	}
}

func TestAuditFrozenHeadActualOldByteTamperWithdrawsWrites(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	info, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	err = l.WithFrozenHead(context.Background(), budget, func(context.Context, Summary) error {
		data, err := os.ReadFile(l.path)
		if err != nil {
			return err
		}
		data[frameHeaderSize+12] ^= 1
		if err := os.WriteFile(l.path, data, 0600); err != nil {
			return err
		}
		return os.Chtimes(l.path, info.ModTime(), info.ModTime())
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("actual old bytes accepted", err)
	}
	if _, err := l.Append(context.Background(), validEvent(2, "key.create")); err == nil {
		t.Fatal("corrupt native still writable")
	}
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known source rejection leaked fees", budget.Snapshot())
	}
}

func TestAuditFrozenHeadSourceAdmissionRejectsBeforeCallbackWithoutPoison(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	held, err := budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: (1 << 20) - 8192})
	if err != nil {
		t.Fatal(err)
	}
	before := budget.Snapshot().Used
	called := false
	err = l.WithFrozenHead(context.Background(), budget, func(context.Context, Summary) error { called = true; return nil })
	if !errors.Is(err, resourcebudget.ErrExhausted) || called {
		t.Fatal("source without its fee entered callback", called, err)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("rejection leaked", got, before)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validEvent(2, "key.create")); err != nil {
		t.Fatal("pre-IO rejection poisoned source", err)
	}
}

func TestAuditFrozenHeadActualReaderExitDebtRetainsWholeJob(t *testing.T) {
	l, budget := frozenHeadFixture(t)
	var actual *os.File
	l.closeReplayFile = func(f *os.File) error { actual = f; return errors.Join(f.Close(), f.Close()) }
	called := false
	err := l.WithFrozenHead(context.Background(), budget, func(context.Context, Summary) error { called = true; return nil })
	if !errors.Is(err, ownedfile.ErrRetirement) || called || actual == nil {
		t.Fatal("unknown actual reader exit accepted", called, actual, err)
	}
	if len(l.key) != 32 || budget.Snapshot().Used.RAM < 4096+AuditSealScanRAMBytes {
		t.Fatal("unknown exit erased resources", len(l.key), budget.Snapshot())
	}
	before := budget.Snapshot().Used
	if !errors.Is(l.Close(), ownedfile.ErrRetirement) || !errors.Is(l.Close(), ownedfile.ErrRetirement) {
		t.Fatal("Close erased exit debt")
	}
	if len(l.key) != 32 || budget.Snapshot().Used != before {
		t.Fatal("repeat Close erased original debt", len(l.key), budget.Snapshot())
	}
}
