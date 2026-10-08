package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReplicaReadMaintenanceRefusesWritesAndJoinsActualRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	primary, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append(ctx, validReservation("evt_read_only", "att_read_only")); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.BeginReplicaReadMaintenance(ctx); err == nil {
		t.Fatal("Standalone acquired Replica read lease")
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	native, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if _, err := native.BeginMaintenance(ctx); err == nil {
		t.Fatal("ordinary semantic maintenance granted to Replica")
	}
	lease, err := native.BeginReplicaReadMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
		t.Fatal("actual Replica mode lost", replicated, err)
	}
	called := false
	if _, _, err := lease.AppendApply(ctx, validReservation("evt_forbidden", "att_forbidden"), func(context.Context, Record) error { called = true; return nil }); !errors.Is(err, ErrLedgerMaintenance) || called {
		t.Fatal("read lease granted semantic Apply", err, called)
	}
	if err := lease.WithHead(ctx, func(context.Context, Watermark, [32]byte) error { called = true; return nil }); !errors.Is(err, ErrLedgerMaintenance) || called {
		t.Fatal("read lease granted recovery confirmation", err, called)
	}
	if _, err := native.Roll(); err == nil {
		t.Fatal("read job allowed structural mutation", err)
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
	go func() {
		done <- lease.ReadFrozenHead(ctx, func(readCtx context.Context, head Watermark, hash [32]byte) error {
			if head.Sequence != 1 || hash == ([32]byte{}) {
				return ErrCorrupt
			}
			actual, err := native.ReplayAuthenticated(readCtx, ReplayAnchor{}, nil)
			if err != nil || actual != head {
				return errors.Join(ErrCorrupt, err)
			}
			close(entered)
			<-readCtx.Done()
			close(canceled)
			<-release
			return readCtx.Err()
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("actual read did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- lease.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("actual Close did not cancel borrowed read")
	}
	select {
	case err := <-closed:
		t.Fatal("read lease Close skipped callback join", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read returned success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := lease.ReadFrozenHead(ctx, func(context.Context, Watermark, [32]byte) error { called = true; return nil }); err == nil || called {
		t.Fatal("closed read lease reused", err)
	}
	if head, _, _ := native.ChainHead(); head.Sequence != 1 {
		t.Fatal("read-only lease changed native facts", head)
	}
}
