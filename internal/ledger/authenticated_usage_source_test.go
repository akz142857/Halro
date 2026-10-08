package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestAuthenticatedUsageSourceJoinsNativeReaderAndRevalidatesCancellation(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "joined-reader", true: "canceled-reader"}[cancelWaiting], func(t *testing.T) {
			p, native, _ := authenticatedAdvanceFixture(t, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ref, err := p.SealReference(ctx)
			if err != nil {
				t.Fatal(err)
			}
			fees := p.budget.Snapshot().Used
			native.mu.Lock()
			held := true
			defer func() {
				if held {
					native.mu.Unlock()
				}
			}()
			done := make(chan error, 1)
			called := false
			go func() {
				done <- p.WithFrozenUsageSource(ctx, native, p.budget, ref.Scope, func(ctx context.Context, source AuthenticatedUsageSource) error {
					called = true
					return source.VerifyAnchors(ctx, [2]ReplayAnchor{ref.Anchor, ref.Anchor})
				})
			}()
			for {
				native.admission.Lock()
				admitted := native.mutating > 0
				native.admission.Unlock()
				if admitted {
					break
				}
				select {
				case err := <-done:
					t.Fatal("source rejected an ordinary native reader overlap", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			if cancelWaiting {
				cancel()
			}
			native.mu.Unlock()
			held = false
			err = <-done
			if cancelWaiting {
				if !errors.Is(err, context.Canceled) || called {
					t.Fatal("canceled source entered callback", called, err)
				}
			} else if err != nil || !called {
				t.Fatal("joined source did not authenticate", called, err)
			}
			if p.budget.Snapshot().Used != fees {
				t.Fatal("joined source retained fees", p.budget.Snapshot())
			}
		})
	}
}

func TestAuthenticatedUsageSourceRejectsDifferentActualNativeAfterSerializedJoin(t *testing.T) {
	p, _, _ := authenticatedAdvanceFixture(t, 0)
	_, foreign, _ := authenticatedAdvanceFixture(t, 0)
	ctx := context.Background()
	ref, err := p.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fees := p.budget.Snapshot().Used
	called := false
	err = p.WithFrozenUsageSource(ctx, foreign, p.budget, ref.Scope, func(context.Context, AuthenticatedUsageSource) error { called = true; return nil })
	if !errors.Is(err, ErrProjectionSource) || called || p.budget.Snapshot().Used != fees {
		t.Fatal("foreign native accepted or fees changed", called, err)
	}
}

func TestAuthenticatedLiveUsageSourceCloseCancelsAndJoinsWholeJob(t *testing.T) {
	for _, closeNative := range []bool{false, true} {
		t.Run(map[bool]string{false: "ledger-root", true: "native-log"}[closeNative], func(t *testing.T) {
			p, native, _ := authenticatedAdvanceFixture(t, 0)
			ref, err := p.SealReference(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			done := make(chan error, 1)
			var escaped AuthenticatedUsageSource
			go func() {
				done <- p.WithFrozenUsageSource(context.Background(), native, p.budget, ref.Scope, func(ctx context.Context, source AuthenticatedUsageSource) error {
					escaped = source
					if err := source.VerifyAnchors(ctx, [2]ReplayAnchor{ref.Anchor, ref.Anchor}); err != nil {
						return err
					}
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					return nil
				})
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("source unavailable", err)
			case <-time.After(5 * time.Second):
				t.Fatal("source not entered")
			}
			fees := p.budget.Snapshot().Used
			closed := make(chan error, 1)
			go func() {
				if closeNative {
					closed <- native.Close()
				} else {
					closed <- p.Close()
				}
			}()
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("actual Close did not cancel source job")
			}
			select {
			case err := <-closed:
				t.Fatal("actual Close skipped whole callback join", err)
			case <-time.After(25 * time.Millisecond):
			}
			if p.budget.Snapshot().Used != fees {
				t.Fatal("active source fee was refunded")
			}
			close(release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("closed source accepted", err)
			}
			if err := <-closed; err != nil {
				t.Fatal(err)
			}
			if err := escaped.Check(context.Background()); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("escaped source survived join", err)
			}
		})
	}
}

func TestAuthenticatedLiveUsageSourceWrongOwnerAndReentryRemainKnownRefusals(t *testing.T) {
	p, native, records := authenticatedAdvanceFixture(t, 0)
	ctx := context.Background()
	ref, err := p.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fees := p.budget.Snapshot().Used
	called := false
	if err := p.WithFrozenUsageSource(ctx, native, p.budget, [32]byte{99}, func(context.Context, AuthenticatedUsageSource) error { called = true; return nil }); err == nil || called || fees != p.budget.Snapshot().Used {
		t.Fatal("foreign scope changed fees", err)
	}
	err = p.WithFrozenUsageSource(ctx, native, p.budget, ref.Scope, func(ctx context.Context, source AuthenticatedUsageSource) error {
		_, err := source.ReplayAuthenticated(ctx, ReplayAnchor{}, func(r Record) error {
			if _, err := source.ReplayAuthenticated(ctx, ReplayAnchor{}, nil); !errors.Is(err, authenticatedstate.ErrBusy) {
				t.Fatal("recursive replay admitted", err)
			}
			got, found, err := source.ReadEventPosition(ctx, r.Event.EventID)
			if err != nil || !found || got != nativeRecordPrefix(r).Head {
				t.Fatal("source dependency differs", got, err)
			}
			return nil
		})
		return err
	})
	if err != nil || len(records) == 0 || p.budget.Snapshot().Used != fees {
		t.Fatal("known live job leaked fees", err)
	}
}
