package resourcebudget

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestCompletableWorkReusesActualCreditsAfterPressureAndCancellation(t *testing.T) {
	m := fixtureManager(t)
	profile := []WorkspaceRequirement{{IO, 100}, {Background, 200}}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 400, Disk: 100}, profile)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	if before.CompletionReserved != (Cost{RAM: 400, Disk: 100}) || before.NormalClaims != 4 {
		t.Fatal(before)
	}
	ctx, cancel := context.WithCancel(w.Bind(context.Background()))
	cancel()
	m.SetPressure(Pressured)
	m.CloseAdmission()
	if _, err := m.TryAcquireContext(context.WithoutCancel(ctx), IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal("ordinary bound context borrowed completion", err)
	}
	var escaped context.Context
	for pass := 0; pass < 3; pass++ {
		err := WithCompletionWorkspace(context.WithoutCancel(ctx), func(ctx context.Context) error {
			if escaped != nil {
				if _, err := m.TryAcquireContext(escaped, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
					t.Fatal("old phase borrowed a successor", err)
				}
			}
			escaped = ctx
			return m.WithRAMWorkspace(ctx, profile, func(ctx context.Context) error {
				for _, slot := range profile {
					p, err := m.TryAcquireContext(ctx, slot.Class, Cost{RAM: slot.RAM})
					if err != nil {
						return err
					}
					if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims {
						t.Fatal("terminal reacquired or lost fees", before, got)
					}
					if err := p.Release(); err != nil {
						return err
					}
				}
				return RetireRAMWorkspace(ctx)
			})
		})
		if err != nil {
			t.Fatal(pass, err)
		}
		if _, err := m.TryAcquireContext(escaped, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal("ended terminal context retained capability", err)
		}
	}
	if got := m.Snapshot(); got.CompletionReserved != (Cost{RAM: 100, Disk: 100}) {
		t.Fatal("unused physical reservation lost", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Active.Release(); err != nil {
		t.Fatal(err)
	}
	if err := WithCompletionWorkspace(context.WithoutCancel(ctx), func(context.Context) error { t.Fatal("closed completion ran"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestCompletableWorkUnknownChildRetainsOriginalPool(t *testing.T) {
	m := fixtureManager(t)
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 400, Disk: 100}, []WorkspaceRequirement{{IO, 100}, {Background, 200}})
	if err != nil {
		t.Fatal(err)
	}
	var child Permit
	err = WithCompletionWorkspace(w.Bind(context.Background()), func(ctx context.Context) error {
		p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
		if err != nil {
			return err
		}
		child, err = p.Transfer()
		return errors.Join(err, ownedfile.ErrRetirement)
	})
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(w.Close(), ownedfile.ErrRetirement) {
		t.Fatal("unknown physical owner erased", err)
	}
	w.Active.Release()
	if got := m.Snapshot(); got.Classes[Completion] != (Cost{RAM: 400, Disk: 100}) || got.NormalClaims != 3 {
		t.Fatal("request return retired actual child fees", got)
	}
	if err := child.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}

func TestCompletableWorkAdmissionIsAtomicAndLoansAreScoped(t *testing.T) {
	m := fixtureManager(t)
	before := m.Snapshot()
	for _, profile := range [][]WorkspaceRequirement{{{IO, 401}}, {{Active, 100}}, {{IO, 100}, {IO, 100}, {IO, 100}, {IO, 100}, {IO, 100}, {IO, 100}}} {
		if w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 400}, profile); w != nil || err == nil {
			t.Fatal(w, err)
		}
		if got := m.Snapshot(); got.Used != before.Used || got.Owners != before.Owners {
			t.Fatal("partial completion admitted", got)
		}
	}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 400}, []WorkspaceRequirement{{IO, 100}})
	if err != nil {
		t.Fatal(err)
	}
	err = WithCompletionWorkspace(w.Bind(context.Background()), func(ctx context.Context) error {
		if _, err := fixtureManager(t).TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
			t.Fatal("foreign manager borrowed pool", err)
		}
		for _, cost := range []Cost{{RAM: 1, Disk: 1}, {RAM: 1, Unconfirmed: 1}} {
			if _, err := m.TryAcquireContext(ctx, IO, cost); !errors.Is(err, ErrInvalid) {
				t.Fatal("persistent fee borrowed RAM", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w.Active.Release()
	zeroUsage(t, m)
}

func TestCompletableWorkNestedLoanKeepsOriginalPoolAndRefusesExpiredOrForeignLoan(t *testing.T) {
	m := fixtureManager(t)
	profile := []WorkspaceRequirement{{IO, 100}}
	first, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 100}, profile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 100}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(Pressured); err != nil {
		t.Fatal(err)
	}
	var expired context.Context
	entered := 0
	err = first.WithWorkspace(context.Background(), func(ctx context.Context) error {
		expired = ctx
		ordinary := WithoutRAMWorkspace(ctx)
		if ordinary.Done() != ctx.Done() || ordinary.Err() != ctx.Err() {
			t.Fatal("withdrawing RAM loan changed caller lifetime")
		}
		if _, err := m.TryAcquireContext(ordinary, IO, Cost{RAM: 100}); !errors.Is(err, ErrPressure) {
			t.Fatal("independent work borrowed parent's terminal RAM", err)
		}
		if err := second.WithWorkspace(ctx, func(context.Context) error {
			t.Fatal("foreign original pool replaced active loan")
			return nil
		}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		return first.WithWorkspace(ctx, func(ctx context.Context) error {
			entered++
			p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
			if err != nil {
				return err
			}
			if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrExhausted) {
				t.Fatal("nested callback created another slot", err)
			}
			return p.Release()
		})
	})
	if err != nil || entered != 1 {
		t.Fatal(entered, err)
	}
	if err := first.WithWorkspace(expired, func(context.Context) error {
		t.Fatal("expired original loan reactivated")
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	for _, work := range []*CompletableWork{first, second} {
		if err := errors.Join(work.Close(), work.Active.Release()); err != nil {
			t.Fatal(err)
		}
	}
	zeroUsage(t, m)
}
