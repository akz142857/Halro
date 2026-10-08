package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAuthenticatedOfflineUsageSourceLeaseCloseCancelsAndJoins(t *testing.T) {
	p, native, _ := authenticatedAdvanceFixture(t, 0)
	ctx := context.Background()
	ref, err := p.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	baseline := p.budget.Snapshot().Used
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
		done <- p.WithOfflineMaintenanceUsageSource(ctx, native, lease, p.budget, ref.Scope, func(ctx context.Context, source AuthenticatedUsageSource) error {
			escaped = source
			if err := source.VerifyAnchors(ctx, [2]ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
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
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("source did not enter")
	}
	charged := p.budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- lease.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("lease did not cancel actual source callback")
	}
	select {
	case err := <-closed:
		t.Fatal("lease skipped actual callback join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if p.budget.Snapshot().Used != charged {
		t.Fatal("active callback was refunded")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled source returned success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := escaped.Check(ctx); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("escaped source survived", err)
	}
	if p.budget.Snapshot().Used != baseline {
		t.Fatal("actual joined callback leaked fees")
	}
}
