package resourcebudget

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func sharedFixture(t *testing.T, count int) (*Manager, *SharedCompletableWorkspace, []*CompletableWork) {
	t.Helper()
	l := fixtureLimits()
	l.MaxClaims, l.MaxOwners = 64, 16
	l.Total.RAM = 1 << 20
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[Recovery] = l.RecoveryReserve
	l.Classes[Control] = Cost{RAM: int64(l.MaxClaims) * ClaimRAMBytes}
	m, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPressure(Healthy)
	profile := SharedWorkProfile{Class: IO, Active: Cost{RAM: 100}, Requirements: []WorkspaceRequirement{{IO, 200}}}
	s, err := m.InstallSharedCompletableWorkspace([]SharedWorkProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	var works []*CompletableWork
	for i := 0; i < count; i++ {
		w, err := m.TryAcquireCompletableWork(IO, profile.Active, Cost{RAM: 200}, profile.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		works = append(works, w)
	}
	return m, s, works
}
func closeShared(t *testing.T, m *Manager, s *SharedCompletableWorkspace, works []*CompletableWork) {
	t.Helper()
	for _, w := range works {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Active.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}
func awaitShared(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("shared state did not converge")
		}
		runtime.Gosched()
	}
}
func sharedResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("shared loan did not join")
		return nil
	}
}

func TestSharedCompletableWorkUnionAndBoundedMembership(t *testing.T) {
	profiles := []SharedWorkProfile{{IO, Cost{RAM: 1}, []WorkspaceRequirement{{IO, 200}, {IO, 200}}}, {Background, Cost{RAM: 2}, []WorkspaceRequirement{{IO, 200}, {Background, 300}}}}
	union, err := SharedWorkspaceRequirements(profiles)
	if err != nil || !reflect.DeepEqual(union, []WorkspaceRequirement{{IO, 200}, {IO, 200}, {Background, 300}}) {
		t.Fatal(union, err)
	}
	m, s, ws := sharedFixture(t, MaxSharedWorkMembers)
	before := m.Snapshot()
	if before.NormalClaims != MaxSharedWorkMembers+2 || before.CompletionReserved.RAM != 200 {
		t.Fatal(before)
	}
	if w, err := m.TryAcquireCompletableWork(IO, Cost{RAM: 100}, Cost{RAM: 200}, []WorkspaceRequirement{{IO, 200}}); w != nil || !errors.Is(err, ErrExhausted) {
		t.Fatal(w, err)
	}
	if _, err := m.InstallSharedCompletableWorkspace(profiles); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != before.Used || got.Owners != before.Owners {
		t.Fatal("failed install changed ownership", got)
	}
	outsider, err := m.TryAcquireCompletableWork(IO, Cost{RAM: 101}, Cost{RAM: 200}, []WorkspaceRequirement{{IO, 200}})
	if err != nil || outsider.shared != nil {
		t.Fatal(outsider, err)
	}
	outsider.Close()
	outsider.Active.Release()
	closeShared(t, m, s, ws)
	if _, err := m.TryAcquireCompletableWork(IO, Cost{RAM: 100}, Cost{RAM: 200}, []WorkspaceRequirement{{IO, 200}}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed binding fell back", err)
	}
}

func TestSharedCompletableWorkNestedLoansUseActualSlotsAndExpire(t *testing.T) {
	m, s, ws := sharedFixture(t, 2)
	m.SetPressure(Pressured)
	m.CloseAdmission()
	var root, nested context.Context
	err := ws[0].WithWorkspace(context.Background(), func(ctx context.Context) error {
		root = ctx
		p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
		if err != nil {
			return err
		}
		err = ws[1].WithWorkspace(WithoutRAMWorkspace(ctx), func(ctx context.Context) error {
			nested = ctx
			if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
				t.Fatal("nested allocated a second pool", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := m.TryAcquireContext(nested, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal("expired child borrowed live root", err)
		}
		if err := RetireRAMWorkspace(nested); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		return p.Release()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws[0].WithWorkspace(root, func(context.Context) error { t.Fatal("expired callback ran"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := m.WithRAMWorkspace(nested, []WorkspaceRequirement{{IO, 200}}, func(context.Context) error { t.Fatal("expired workspace ran"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	closeShared(t, m, s, ws)
}

func TestSharedCompletableWorkFIFOAndCanceledWaiter(t *testing.T) {
	m, s, ws := sharedFixture(t, 4)
	entered, release := make(chan struct{}), make(chan struct{})
	rootDone := make(chan error, 1)
	go func() {
		rootDone <- ws[0].WithWorkspace(context.Background(), func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	canceledDone := make(chan error, 1)
	go func() {
		canceledDone <- ws[1].WithWorkspace(canceled, func(context.Context) error { t.Error("canceled waiter entered"); return nil })
	}()
	awaitShared(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.queueCount == 1 })
	if err := ws[1].WithWorkspace(context.Background(), func(context.Context) error { t.Error("duplicate waiter entered"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	cancel()
	if err := sharedResult(t, canceledDone); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	order := make(chan int, 2)
	dones := []chan error{make(chan error, 1), make(chan error, 1)}
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			dones[i] <- ws[i+2].WithWorkspace(context.Background(), func(context.Context) error { order <- i; return nil })
		}()
		awaitShared(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.queueCount == i+1 })
	}
	if err := ws[0].Close(); !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	// An idle member can retire while a different member owns the root pool.
	if err := ws[1].Close(); err != nil {
		t.Fatal(err)
	}
	ws[1].Active.Release()
	if err := s.Close(); !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	m.SetPressure(Pressured)
	m.CloseAdmission()
	close(release)
	if err := sharedResult(t, rootDone); err != nil {
		t.Fatal(err)
	}
	for _, done := range dones {
		if err := sharedResult(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if first, second := <-order, <-order; first != 0 || second != 1 {
		t.Fatal(first, second)
	}
	closeShared(t, m, s, []*CompletableWork{ws[0], ws[2], ws[3]})
}

func TestSharedCompletableWorkJoinsEnteredNestedJobAndPropagatesError(t *testing.T) {
	m, s, ws := sharedFixture(t, 2)
	nestedEntered, rootReturned, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	rootContext := make(chan context.Context, 1)
	done := make(chan error, 1)
	childDone := make(chan error, 1)
	sentinel := errors.New("actual nested failure")
	go func() {
		done <- ws[0].WithWorkspace(context.Background(), func(ctx context.Context) error {
			rootContext <- ctx
			go func() {
				childDone <- ws[1].WithWorkspace(WithoutRAMWorkspace(ctx), func(childCtx context.Context) error {
					close(nestedEntered)
					<-release
					p, err := m.TryAcquireContext(childCtx, IO, Cost{RAM: 200})
					if err != nil {
						return err
					}
					return errors.Join(sentinel, p.Release())
				})
			}()
			<-nestedEntered
			close(rootReturned)
			return nil
		})
	}()
	<-rootReturned
	ctx := <-rootContext
	awaitShared(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !ctx.Value(sharedRAMLoanKey{}).(*sharedRAMLoan).active
	})
	if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal("withdrawn parent acquired", err)
	}
	select {
	case err := <-done:
		t.Fatal("root returned before entered child", err)
	default:
	}
	if err := ws[1].Close(); !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	close(release)
	if err := sharedResult(t, done); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := sharedResult(t, childDone); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	closeShared(t, m, s, ws)
}

func TestSharedCompletableWorkUnknownChildPreservesPoolUntilFinalOwner(t *testing.T) {
	m, s, ws := sharedFixture(t, 2)
	before := m.Snapshot()
	var child Permit
	err := ws[0].WithWorkspace(context.Background(), func(ctx context.Context) error {
		p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
		if err != nil {
			return err
		}
		child, err = p.Transfer()
		return errors.Join(err, ownedfile.ErrRetirement)
	})
	if !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	for _, w := range ws {
		if err := w.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("debt erased", err)
		}
	}
	if err := s.Close(); !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims {
		t.Fatal("debt returned original fee", got, before)
	}
	if err := ws[1].WithWorkspace(context.Background(), func(context.Context) error { t.Error("poisoned pool reused"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	child.Release()
	closeShared(t, m, s, ws)
}

func TestSharedCompletableWorkOrdinaryPhaseCannotGainCompletionAndForeignLoanRefuses(t *testing.T) {
	m, s, ws := sharedFixture(t, 2)
	other, foreign, outsiders := sharedFixture(t, 1)
	before := m.Snapshot()
	type domainKey struct{}
	ctx := context.WithValue(ws[0].Bind(context.Background()), domainKey{}, "actual source")
	if err := WithAdmittedWorkspace(ctx, m, func(ctx context.Context) error {
		if InCompletionContinuation(ctx) {
			t.Fatal("ordinary root gained completion")
		}
		return ws[1].WithWorkspace(WithoutRAMWorkspace(ctx), func(childCtx context.Context) error {
			if childCtx.Value(domainKey{}) != "actual source" || InCompletionContinuation(childCtx) {
				t.Fatal("nested ordinary phase changed authority")
			}
			p, err := m.TryAcquireContext(childCtx, IO, Cost{RAM: 200})
			if err != nil {
				return err
			}
			if got := m.Snapshot(); got.CompletionReserved != before.CompletionReserved || got.Used != before.Used {
				t.Fatal("ordinary nested borrower activated credits", got)
			}
			if err := outsiders[0].WithWorkspace(WithoutRAMWorkspace(childCtx), func(context.Context) error { t.Error("foreign owner callback ran"); return nil }); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			return p.Release()
		})
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPressure(Pressured)
	if err := WithAdmittedWorkspace(ctx, m, func(context.Context) error { t.Error("ordinary phase bypassed pressure"); return nil }); !errors.Is(err, ErrPressure) {
		t.Fatal(err)
	}
	if err := WithCompletionWorkspace(ctx, func(ctx context.Context) error {
		return ws[1].WithWorkspace(WithoutRAMWorkspace(ctx), func(childCtx context.Context) error {
			if !InCompletionContinuation(childCtx) {
				t.Fatal("actual terminal child lost phase")
			}
			p, err := m.TryAcquireContext(childCtx, IO, Cost{RAM: 200})
			if err != nil {
				return err
			}
			return p.Release()
		})
	}); err != nil {
		t.Fatal(err)
	}
	closeShared(t, m, s, ws)
	closeShared(t, other, foreign, outsiders)
}
