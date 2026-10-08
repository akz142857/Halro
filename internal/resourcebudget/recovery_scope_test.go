package resourcebudget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func scopeManager(t *testing.T) *Manager {
	t.Helper()
	l := fixtureLimits()
	l.RecoveryClaims = 5
	l.RecoveryReserve.RAM = 16384
	l.Classes[Recovery] = l.RecoveryReserve
	m, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRecoveryScopeLimitsAuthorityAndClosesEscapedContext(t *testing.T) {
	for _, pressure := range []Pressure{Unknown, Pressured} {
		m := scopeManager(t)
		m.SetPressure(pressure)
		parent, err := m.TryAcquireRecovery(Cost{RAM: 100})
		if err != nil {
			t.Fatal(err)
		}
		var escaped context.Context
		err = parent.WithRecoveryScope(context.Background(), func(ctx context.Context) error {
			escaped = ctx
			if _, err := m.TryAcquire(IO, Cost{RAM: 1}); !errors.Is(err, ErrPressure) {
				t.Fatal(err)
			}
			for _, class := range []Class{Configuration, Active, Cache, Completion, Recovery, Control} {
				if _, err := m.TryAcquireContext(ctx, class, Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
					t.Fatal(class, err)
				}
			}
			for _, cost := range []Cost{{RAM: 1, Disk: 1}, {RAM: 1, Unconfirmed: 1}, {Disk: 1}, {}} {
				if _, err := m.TryAcquireContext(ctx, IO, cost); !errors.Is(err, ErrInvalid) {
					t.Fatal(cost, err)
				}
			}
			foreign := scopeManager(t)
			if _, err := foreign.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			for _, class := range []Class{IO, Background} {
				child, err := m.TryAcquireContext(ctx, class, Cost{RAM: 200})
				if err != nil {
					t.Fatal(err)
				}
				if m.Snapshot().Classes[class].RAM != 0 || m.Snapshot().Classes[Recovery].RAM != 300 {
					t.Fatal(m.Snapshot())
				}
				if err := child.WithRecoveryScope(ctx, func(context.Context) error { t.Fatal("nested authority"); return nil }); !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
				if err := child.Release(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 16384}); !errors.Is(err, ErrExhausted) {
				t.Fatal(err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		m.SetPressure(Healthy)
		if _, err := m.TryAcquireContext(escaped, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal("escaped authority", err)
		}
		parent.Release()
		zeroUsage(t, m)
	}
}

func TestRecoveryScopeJoinsRetainedTransferredOwnersAfterCancellation(t *testing.T) {
	m := scopeManager(t)
	parent, _ := m.TryAcquireRecovery(Cost{RAM: 100})
	ctx, cancel := context.WithCancel(context.Background())
	childReady := make(chan Permit, 1)
	callbackReturned := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- parent.WithRecoveryScope(ctx, func(ctx context.Context) error {
			child, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
			if err != nil {
				return err
			}
			retained, err := child.Retain()
			if err != nil {
				return err
			}
			transferred, err := retained.Transfer()
			if err != nil {
				return err
			}
			if err := child.Release(); err != nil {
				return err
			}
			childReady <- transferred
			<-ctx.Done()
			close(callbackReturned)
			return ctx.Err()
		})
	}()
	var child Permit
	select {
	case child = <-childReady:
	case <-time.After(5 * time.Second):
		t.Fatal("child missing")
	}
	if err := parent.Release(); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-callbackReturned
	select {
	case err := <-done:
		t.Fatal("retained owner retired early", err)
	default:
	}
	if s := m.Snapshot(); s.RecoveryClaims != 2 || s.Classes[Recovery].RAM != 300 {
		t.Fatal(s)
	}
	if err := child.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scope did not join")
	}
	zeroUsage(t, m)
}

func TestRecoveryScopeMergeAndConcurrentAdmissionRespectJoin(t *testing.T) {
	m := scopeManager(t)
	parent, _ := m.TryAcquireRecovery(Cost{RAM: 100})
	outside, _ := m.TryAcquireRecovery(Cost{RAM: 100})
	err := parent.WithRecoveryScope(context.Background(), func(ctx context.Context) error {
		a, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
		if err != nil {
			return err
		}
		b, err := m.TryAcquireContext(ctx, Background, Cost{RAM: 100})
		if err != nil {
			return err
		}
		if _, err := a.Merge(outside); !errors.Is(err, ErrInvalid) {
			t.Fatal("cross-scope merge", err)
		}
		merged, err := a.Merge(b)
		if err != nil {
			return err
		}
		if err := merged.Release(); err != nil {
			return err
		}
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
				if errors.Is(err, ErrExhausted) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				if err := p.Release(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		m.CloseAdmission()
		if _, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 1}); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent.Release()
	outside.Release()
	zeroUsage(t, m)
}

func TestRecoveryScopePanicRetiresBorrowedParent(t *testing.T) {
	m := scopeManager(t)
	parent, _ := m.TryAcquireRecovery(Cost{RAM: 100})
	func() {
		defer func() {
			if recover() != "callback" {
				t.Fatal("panic lost")
			}
		}()
		parent.WithRecoveryScope(context.Background(), func(ctx context.Context) error {
			child, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 100})
			if err != nil {
				t.Fatal(err)
			}
			defer child.Release()
			panic("callback")
		})
	}()
	if s := m.Snapshot(); s.Owners != 1 || s.RecoveryClaims != 1 {
		t.Fatal(s)
	}
	parent.Release()
	zeroUsage(t, m)
}

func TestRecoveryScopeRejectsInvalidParentAndPreservesNormalContext(t *testing.T) {
	m := scopeManager(t)
	m.SetPressure(Healthy)
	ordinary, err := m.TryAcquireContext(context.Background(), IO, Cost{RAM: 100})
	if err != nil || m.Snapshot().NormalClaims != 1 {
		t.Fatal(err, m.Snapshot())
	}
	if err := ordinary.WithRecoveryScope(context.Background(), func(context.Context) error { t.Fatal("ordinary parent lent authority"); return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ordinary.Release()
	parent, _ := m.TryAcquireRecovery(Cost{RAM: 100})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := parent.WithRecoveryScope(ctx, func(context.Context) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := parent.WithRecoveryScope(context.Background(), func(ctx context.Context) error {
		if err := parent.WithRecoveryScope(ctx, func(context.Context) error { t.Fatal("same parent reused concurrently"); return nil }); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parent.Release()
	if err := parent.WithRecoveryScope(context.Background(), func(context.Context) error { t.Fatal("released parent lent authority"); return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}
