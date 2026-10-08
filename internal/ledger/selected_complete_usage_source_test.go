package ledger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func openSelectedCompleteUsageFixture(t *testing.T, sealed, compressed bool) (*selectedStartupFixture, *AuthenticatedLedgerProjection, *MaintenanceLease) {
	t.Helper()
	f := newSelectedStartupCompressedFixture(t, sealed, compressed)
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := f.ledgerOptions
	o.Prefix = &ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil && !(f.expectDebt && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, p, lease
}

func TestSelectedCompleteUsageSourceRequiresFinishedAuditAndKeepsRoutineBoundary(t *testing.T) {
	f, p, lease := openSelectedCompleteUsageFixture(t, false, false)
	ctx := context.Background()
	ref := f.ledgerOptions.SavedAnchor
	scope := p.scope
	called := false
	visit := func(context.Context, AuthenticatedUsageSource) error { called = true; return nil }
	before := p.budget.Snapshot().Used
	if err := p.WithSelectedCompleteUsageSource(ctx, f.native, lease, p.budget, scope, visit); !errors.Is(err, ErrStartupAuditPending) || called {
		t.Fatal("unaudited complete source admitted", err, called)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(ctx, p.budget, StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 5 * time.Second}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := f.native.ValidateSelectedStartupRead(ctx); err != nil {
		t.Fatal("finite read readiness absent", err)
	}
	if err := p.WithSelectedCompleteUsageSource(ctx, f.native, lease, p.budget, scope, visit); !errors.Is(err, ErrStartupAuditPending) || called {
		t.Fatal("incomplete ready audit admitted", err, called)
	}
	close(leave)
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.WithOfflineMaintenanceUsageSource(ctx, f.native, lease, p.budget, scope, visit); !errors.Is(err, ErrProjectionSource) || called {
		t.Fatal("ordinary offline guard changed", err, called)
	}
	if err := p.WithSelectedMaintenanceUsageSource(ctx, f.native, lease, p.budget, scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
		_, err := s.ReplayAuthenticated(ctx, ReplayAnchor{}, nil)
		if !errors.Is(err, ErrProjectionCursor) {
			t.Fatal("routine genesis allowed", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped AuthenticatedUsageSource
	if err := p.WithSelectedCompleteUsageSource(ctx, f.native, lease, p.budget, scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
		escaped = s
		if err := s.VerifyAnchors(ctx, [2]ReplayAnchor{ref, s.NativeHead()}); err != nil {
			return err
		}
		var count uint64
		_, err := s.ReplayAuthenticated(ctx, ReplayAnchor{}, func(r Record) error { count++; return nil })
		if count != ref.Head.Sequence {
			t.Fatal("complete history omitted saved prefix", count, ref.Head.Sequence)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.Check(ctx); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("escaped source survived", err)
	}
	if after := p.budget.Snapshot().Used; after != before {
		t.Fatal("completed source fees survived", before, after)
	}
}

func TestSelectedCompleteUsageSourceReauthenticatesOldPrefixAcrossCompressedRoll(t *testing.T) {
	f, p, lease := openSelectedCompleteUsageFixture(t, true, true)
	ctx := context.Background()
	if err := f.native.AuditSelectedStartup(ctx, p.budget); err != nil {
		t.Fatal(err)
	}
	if err := p.WithSelectedCompleteUsageSource(ctx, f.native, lease, p.budget, p.scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
		return s.VerifyAnchors(ctx, [2]ReplayAnchor{s.Reference().Anchor, s.NativeHead()})
	}); err != nil {
		t.Fatal(err)
	}
	// The full scan crossed a real compressed sealed generation. Damage a
	// saved frame in the active generation after audit; routine suffix replay
	// would skip this frame, while the reference must authenticate it again.
	file, err := os.OpenFile(f.path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	pos := f.native.startupSaved.Head.Offset - 1
	if _, err := file.ReadAt(b[:], pos); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := file.WriteAt(b[:], pos); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.WithSelectedCompleteUsageSource(ctx, f.native, lease, p.budget, p.scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
		return s.VerifyAnchors(ctx, [2]ReplayAnchor{s.Reference().Anchor, s.NativeHead()})
	}); err == nil {
		t.Fatal("completed audit hid later old-prefix corruption")
	} else if errors.Is(err, ownedfile.ErrRetirement) {
		// A gzip read failure can also make its actual Close fail. That debt
		// keeps the native reader, verification key and work credit alive.
		f.expectDebt = true
	}
}
