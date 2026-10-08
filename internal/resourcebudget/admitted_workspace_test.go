package resourcebudget

import (
	"context"
	"errors"
	"testing"
)

func TestAdmittedWorkspaceReusesOriginalRAMWithoutTerminalOrPressureAuthority(t *testing.T) {
	m := fixtureManager(t)
	profile := []WorkspaceRequirement{{Class: IO, RAM: 100}, {Class: Background, RAM: 200}}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 100}, Cost{RAM: 300}, profile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := w.Bind(context.Background())
	before := m.Snapshot()
	var escaped context.Context
	ordinary := func(ctx context.Context) error {
		if InCompletionContinuation(ctx) {
			t.Fatal("ordinary request gained terminal authority")
		}
		escaped = ctx
		return m.WithRAMWorkspace(ctx, profile, func(ctx context.Context) error {
			p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
			if err != nil {
				return err
			}
			if after := m.Snapshot(); after.Used != before.Used || after.CompletionReserved != before.CompletionReserved || after.NormalClaims != before.NormalClaims || after.RecoveryClaims != before.RecoveryClaims {
				t.Fatal("ordinary request reacquired its prepaid slots", after)
			}
			if cost, err := p.Cost(); err != nil || cost != (Cost{RAM: 100}) {
				t.Fatal("actual temporary reader cannot inspect its original paid cost", cost, err)
			}
			child, err := p.RetainCovering(m, Cost{RAM: 100})
			if err != nil {
				return err
			}
			if _, err := w.pool.slots[0].permit.Retain(); !errors.Is(err, ErrReserved) {
				t.Fatal("ordinary borrower activated the original terminal owner", err)
			}
			if err := child.Release(); err != nil {
				return err
			}
			return p.Release()
		})
	}
	if err := WithAdmittedWorkspace(ctx, m, ordinary); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TryAcquireContext(escaped, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal("ordinary loan survived its callback", err)
	}
	if err := WithAdmittedWorkspace(ctx, fixtureManager(t), ordinary); !errors.Is(err, ErrInvalid) {
		t.Fatal("foreign manager borrowed original request", err)
	}
	if err := m.SetPressure(Pressured); err != nil {
		t.Fatal(err)
	}
	if err := WithAdmittedWorkspace(ctx, m, func(context.Context) error { t.Fatal("ordinary work bypassed pressure"); return nil }); !errors.Is(err, ErrPressure) {
		t.Fatal(err)
	}
	if err := WithCompletionWorkspace(ctx, func(ctx context.Context) error {
		if !InCompletionContinuation(ctx) {
			t.Fatal("terminal lost its explicit phase")
		}
		if got := m.Snapshot().CompletionReserved; got != (Cost{}) {
			t.Fatal("explicit completion did not activate original reserved slots", got)
		}
		p, err := m.TryAcquireContext(ctx, Background, Cost{RAM: 200})
		if err != nil {
			return err
		}
		return p.Release()
	}); err != nil {
		t.Fatal("owned terminal failed after pressure", err)
	}
	m.CloseAdmission()
	if err := WithAdmittedWorkspace(ctx, m, ordinary); !errors.Is(err, ErrClosed) {
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
