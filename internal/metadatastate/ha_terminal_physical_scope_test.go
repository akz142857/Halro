package metadatastate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This tests only the finite authority/workspace borrowing protocol. The Linux
// tests separately exercise actual bound native/COW FD and allocation owners.
func TestHAPhysicalConfirmedApplyScopeRevokesAndCancellationJoins(t *testing.T) {
	for _, paid := range []bool{true, false} {
		name := "actual-frame"
		if !paid {
			name = "unchanged-metadata"
		}
		t.Run(name, func(t *testing.T) { proveHAPhysicalConfirmedApplyScope(t, paid) })
	}
}

func proveHAPhysicalConfirmedApplyScope(t *testing.T, paid bool) {
	b, _, _ := linkedDeltaCandidate(t)
	completion, err := b.acquirePhysicalCompletion()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(completion.Close(), completion.Active.Release()); err != nil {
			t.Error(err)
		}
	}()
	w := &TerminalPhysicalWindow{owner: b, work: completion.Active, completion: completion}
	source := &haPhysicalSource{role: authenticatedstate.SealReplica, receiveWindow: w}
	if !paid {
		source.receiveWindow = nil
	}
	b.haPhysicalSource = source
	b.physicalEnabled = true
	defer func() { b.haPhysicalSource = nil; b.physicalEnabled = false }()
	var escaped func(context.Context) error
	source.withConfirmedApply = func(ctx context.Context, owner *Baseline, expected metadatajournal.Head, through uint64, visit func(context.Context) error) error {
		if owner != b {
			return ErrSource
		}
		escaped = visit
		return visit(ctx)
	}
	calls := 0
	if _, err := b.withHAReplicaReplayWorkspace(context.Background(), b.position, 0, func(ctx context.Context, work *resourcebudget.Permit) (metadatajournal.Head, error) {
		calls++
		if paid && work != &w.work || !paid && work != nil {
			return metadatajournal.Head{}, ErrSource
		}
		return b.position, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("actual finite visit not performed")
	}
	if err := escaped(context.Background()); !errors.Is(err, ErrSource) || calls != 1 {
		t.Fatal("completed authority scope revived", err, calls)
	}
	if !paid {
		if _, err := b.withHAReplicaReplayWorkspace(context.Background(), b.position, b.position.Sequence+1, func(context.Context, *resourcebudget.Permit) (metadatajournal.Head, error) {
			calls++
			return metadatajournal.Head{}, nil
		}); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) || calls != 1 {
			t.Fatal("unchanged-source loan authorized a new Metadata fact", err, calls)
		}
	}
	entered := make(chan struct{})
	source.withConfirmedApply = func(ctx context.Context, _ *Baseline, _ metadatajournal.Head, _ uint64, _ func(context.Context) error) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.withHAReplicaReplayWorkspace(context.Background(), b.position, 0, func(context.Context, *resourcebudget.Permit) (metadatajournal.Head, error) {
			return metadatajournal.Head{}, ErrSource
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authority scope did not enter")
	}
	joined := make(chan struct{})
	go func() { b.cancel(); b.joinHAPhysicalReceives(); close(joined) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("original lifetime withdrawal not propagated", err)
		}
	case <-time.After(time.Second):
		t.Fatal("actual Baseline cancellation did not revoke authority borrower")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("Close's original join did not finish")
	}
}
