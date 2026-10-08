package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestTypedGovernanceFrozenSealQualifiesIdentityAndKeepsCompleteCut(t *testing.T) {
	_, native, _, _, _ := testManager(t)
	if _, err := native.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	p, ref, o := typedGovernanceFixture(t, native)
	defer p.Discard()
	fees := o.Budget.Snapshot().Used
	called := false
	if err := p.WithFrozenTypedSealReference(context.Background(), o.Budget, [32]byte{88}, func(context.Context, TypedGovernanceReference) error { called = true; return nil }); !errors.Is(err, authenticatedstate.ErrRecovery) || called || fees != o.Budget.Snapshot().Used {
		t.Fatal("foreign identity entered", called, err)
	}
	rejected := errors.New("common candidate rejected")
	err := native.WithFrozenHead(context.Background(), o.Budget, func(ctx context.Context, _ Summary) error {
		return p.WithFrozenTypedSealReference(ctx, o.Budget, o.Identity, func(ctx context.Context, r TypedGovernanceReference) error {
			if r != ref {
				t.Fatal("root/native cut split", r, ref)
			}
			if _, err := native.Append(ctx, projectionEvent(2)); !errors.Is(err, ErrNativeFrozen) {
				t.Fatal("native writer entered common freeze", err)
			}
			return rejected
		})
	})
	if !errors.Is(err, rejected) || fees != o.Budget.Snapshot().Used {
		t.Fatal("candidate rejection changed lifetime", err, o.Budget.Snapshot())
	}
	after, err := p.TypedSealReference(context.Background())
	if err != nil || after != ref {
		t.Fatal("healthy root changed", after, err)
	}
}
func TestTypedGovernanceFrozenSealCloseCancelsAndJoinsCallback(t *testing.T) {
	_, native, _, _, _ := testManager(t)
	p, _, o := typedGovernanceFixture(t, native)
	defer p.Discard()
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- p.WithFrozenTypedSealReference(context.Background(), o.Budget, o.Identity, func(ctx context.Context, _ TypedGovernanceReference) error {
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
		t.Fatal("callback not entered")
	}
	fees := o.Budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped actual join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used != fees {
		t.Fatal("live callback refunded")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("closed candidate accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
