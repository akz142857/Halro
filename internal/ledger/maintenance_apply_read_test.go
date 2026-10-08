package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceAcceptedApplyReadUsesActualScopeAndRevokesEscapedContext(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if found, err := m.ReadAcceptedApplyHead(context.Background(), func(context.Context, Watermark, [32]byte) error { return nil }); found || err != nil {
		t.Fatal("raw context fabricated evidence", found, err)
	}
	var saved context.Context
	called := 0
	actual, _, err := m.AppendApply(context.Background(), validReservation("actual", "actual"), func(ctx context.Context, r Record) error {
		saved = ctx
		found, err := m.ReadAcceptedApplyHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
			called++
			if head != (Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}) || hash != r.Hash {
				t.Fatal("scope not actual durable fact", head, r)
			}
			_, err := l.ReplayAuthenticated(ctx, ReplayAnchor{}, nil)
			return err
		})
		if !found {
			return errors.New("real current scope absent")
		}
		return err
	})
	if err != nil || called != 1 || actual.Sequence != 1 {
		t.Fatal(actual, called, err)
	}
	if found, err := m.ReadAcceptedApplyHead(saved, func(context.Context, Watermark, [32]byte) error { called++; return nil }); !found || err == nil || called != 1 {
		t.Fatal("escaped scope authorized", found, called, err)
	}
}

func TestMaintenanceAcceptedApplyReadCannotTransferToAnotherLease(t *testing.T) {
	makeLog := func() (*Log, *MaintenanceLease) {
		l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
		if err != nil {
			t.Fatal(err)
		}
		m, err := l.BeginMaintenance(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close(); l.Close() })
		return l, m
	}
	_, m := makeLog()
	_, other := makeLog()
	_, _, err := m.AppendApply(context.Background(), validReservation("actual", "actual"), func(ctx context.Context, _ Record) error {
		called := false
		found, err := other.ReadAcceptedApplyHead(ctx, func(context.Context, Watermark, [32]byte) error { called = true; return nil })
		if !found || !errors.Is(err, ErrLedgerMaintenance) || called {
			t.Fatal("transferred native scope", found, called, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceAcceptedApplyReadJoinsBorrowerBeforeNativeMutationReturns(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	readResult := make(chan error, 1)
	appended := make(chan error, 1)
	go func() {
		_, _, err := m.AppendApply(context.Background(), validReservation("joined", "joined"), func(ctx context.Context, _ Record) error {
			go func() {
				_, err := m.ReadAcceptedApplyHead(ctx, func(ctx context.Context, _ Watermark, _ [32]byte) error {
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					return ctx.Err()
				})
				readResult <- err
			}()
			<-entered
			return nil // Scope retirement must still join the actual borrowed read.
		})
		appended <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("borrower not entered")
	}
	select {
	case err := <-appended:
		t.Fatal("mutation skipped borrower join", err)
	case <-time.After(25 * time.Millisecond):
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("native Close did not cancel borrower")
	}
	select {
	case err := <-closed:
		t.Fatal("native Close skipped actual join", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-readResult; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-appended // Accepted durable result may report Close's cancellation.
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
