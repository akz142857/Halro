package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestTypedUsageFrozenSealQualifiesIdentityAndExcludesApply(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	p, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	fees := f.options.Budget.Snapshot().Used
	called := false
	if err := p.WithFrozenTypedSealReference(ctx, f.options.Budget, [32]byte{88}, func(context.Context, TypedUsageReference) error { called = true; return nil }); !errors.Is(err, authenticatedstate.ErrRecovery) || called || fees != f.options.Budget.Snapshot().Used {
		t.Fatal("foreign identity entered", called, err)
	}
	rejected := errors.New("common candidate refused")
	err = p.WithFrozenTypedSealReference(ctx, f.options.Budget, f.options.Identity, func(ctx context.Context, r TypedUsageReference) error {
		if r != ref {
			t.Fatal("root/cut split", r, ref)
		}
		if err := p.ApplyContext(ctx, f.records[0]); !errors.Is(err, ErrDiskUsageBusy) {
			t.Fatal("write entered frozen root", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || fees != f.options.Budget.Snapshot().Used {
		t.Fatal("candidate refusal changed fee", err, f.options.Budget.Snapshot())
	}
	after, err := p.TypedSealReference(ctx)
	if err != nil || after != ref {
		t.Fatal("rejection retired healthy root", after, err)
	}
}
func TestTypedUsageFrozenSealCloseCancelsAndJoinsActualCallback(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	p, _, err := BuildTypedUsageProjection(context.Background(), f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
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
		done <- p.WithFrozenTypedSealReference(context.Background(), f.options.Budget, f.options.Identity, func(ctx context.Context, _ TypedUsageReference) error {
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
	fees := f.options.Budget.Snapshot().Used
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
	if f.options.Budget.Snapshot().Used != fees {
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
