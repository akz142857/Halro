package resourcebudget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestPreAdmittedWorkspaceSurvivesPressureWithoutNewClaims(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	var escaped context.Context
	err := m.WithRAMWorkspace(context.Background(), []WorkspaceRequirement{{IO, 100}, {Background, 200}}, func(ctx context.Context) error {
		escaped = ctx
		before := m.Snapshot()
		m.SetPressure(Pressured)
		m.CloseAdmission()
		if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal("ordinary work escaped closed admission", err)
		}
		for _, requirement := range []WorkspaceRequirement{{IO, 100}, {Background, 200}} {
			child, err := m.TryAcquireContext(ctx, requirement.Class, Cost{RAM: requirement.RAM})
			if err != nil {
				t.Fatal(err)
			}
			if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims || got.Owners != before.Owners+1 {
				t.Fatal("workspace reacquired or undercharged its actual child", before, got)
			}
			if _, err := m.TryAcquireContext(ctx, requirement.Class, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
				t.Fatal("one physical workspace served two children", err)
			}
			for _, forbidden := range []Cost{{Disk: 1}, {RAM: 1, Unconfirmed: 1}} {
				if _, err := m.TryAcquireContext(ctx, IO, forbidden); !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
			}
			if _, err := scopeManager(t).TryAcquireContext(ctx, requirement.Class, Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
				t.Fatal("foreign manager borrowed the actual fee", err)
			}
			if err := child.Release(); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TryAcquireContext(escaped, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal("escaped context retained admission", err)
	}
	zeroUsage(t, m)
}

func TestFixedMaximumWorkspaceSlotsRemainDistinctThroughNestedLoan(t *testing.T) {
	l := fixtureLimits()
	l.MaxClaims, l.MaxOwners = MaxWorkspaceSlots+8, 16
	l.Classes[Control] = Cost{RAM: int64(l.MaxClaims) * ClaimRAMBytes}
	m, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPressure(Healthy)
	profile := make([]WorkspaceRequirement, MaxWorkspaceSlots)
	for i := range profile {
		profile[i] = WorkspaceRequirement{IO, 1}
	}
	if _, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 1}, Cost{RAM: MaxWorkspaceSlots + 1}, append(profile, WorkspaceRequirement{IO, 1})); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded slot profile accepted", err)
	}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 1}, Cost{RAM: MaxWorkspaceSlots}, profile)
	if err != nil {
		t.Fatal(err)
	}
	err = WithCompletionWorkspace(w.Bind(context.Background()), func(ctx context.Context) error {
		m.SetPressure(Pressured)
		return m.WithRAMWorkspace(ctx, profile, func(ctx context.Context) error {
			var children [MaxWorkspaceSlots]Permit
			for i := range children {
				child, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1})
				if err != nil {
					return err
				}
				children[i] = child
				defer child.Release()
			}
			if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
				return errors.New("overlapping children shared one slot")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Active.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestPreAdmittedWorkspaceJoinsRetainedTransferredChildren(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	retained := make(chan Permit, 1)
	returned := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithRAMWorkspace(context.Background(), []WorkspaceRequirement{{IO, 100}}, func(ctx context.Context) error {
			child, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
			if err != nil {
				return err
			}
			alias, err := child.Retain()
			if err != nil {
				return err
			}
			alias, err = alias.Transfer()
			if err != nil {
				return err
			}
			if err := child.Release(); err != nil {
				return err
			}
			if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
				t.Error("retained owner did not occupy the actual slot", err)
			}
			retained <- alias
			close(returned)
			return nil
		})
	}()
	alias := <-retained
	<-returned
	select {
	case err := <-done:
		t.Fatal("callback released still-live RAM", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := alias.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("final physical retirement did not join")
	}
	zeroUsage(t, m)
}

func TestPreAdmittedWorkspacePreservesUnknownPhysicalRetirement(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	var child Permit
	err := m.WithRAMWorkspace(context.Background(), []WorkspaceRequirement{{IO, 100}, {Background, 200}}, func(ctx context.Context) error {
		var err error
		child, err = m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
		if err != nil {
			return err
		}
		return ownedfile.ErrRetirement
	})
	var debt *WorkspaceRetirement
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || debt.owner == nil || m.Snapshot().NormalClaims != 2 {
		t.Fatal("unknown physical child lost the original owner", err, m.Snapshot())
	}
	if err := child.Release(); err != nil {
		t.Fatal(err)
	}
	<-debt.owner.done
	zeroUsage(t, m)
}

func TestPreAdmittedWorkspaceRefusesBeforeCallbackAndReturnsPartialFees(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	err := m.WithRAMWorkspace(context.Background(), []WorkspaceRequirement{{IO, 100}, {Background, 1 << 30}}, func(context.Context) error {
		t.Fatal("accepted callback before complete workspace admission")
		return nil
	})
	if !errors.Is(err, ErrExhausted) {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestPreAdmittedWorkspacePhaseRetiresOnlyAfterActualChildren(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	err := m.WithRAMWorkspace(context.Background(), []WorkspaceRequirement{{IO, 100}}, func(ctx context.Context) error {
		child, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
		if err != nil {
			return err
		}
		if err := RetireRAMWorkspace(ctx); !errors.Is(err, ErrExhausted) || m.Snapshot().Used.RAM == 0 {
			t.Fatal("retired a phase with a physical child", err)
		}
		if err := child.Release(); err != nil {
			return err
		}
		if err := RetireRAMWorkspace(ctx); err != nil {
			return err
		}
		zeroUsage(t, m)
		if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal("completed phase retained protected authority", err)
		}
		ordinary, err := m.TryAcquire(IO, Cost{RAM: 100})
		if err != nil {
			return err
		}
		return ordinary.Release()
	})
	if err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}
