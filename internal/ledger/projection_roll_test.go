package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func projectionRollFixture(t *testing.T) (*Log, *LedgerProjection, *State, Segment) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	t.Cleanup(func() { _ = log.Close() })
	for _, event := range projectionTestLifecycle(1, false) {
		if _, err := log.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	rolled, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	var last Record
	state := NewState()
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error { last = r; return state.Apply(r) })
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	p, err := BuildLedgerProjectionPrefix(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report, ProjectionPrefix{Head: Watermark{last.Generation, last.Offset, last.Sequence}, Hash: last.Hash})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return log, p, state, rolled.Sealed
}

func TestLedgerProjectionVerifiedRollMovesOnlyPositionAndIsIdempotent(t *testing.T) {
	log, p, state, segment := projectionRollFixture(t)
	ctx := context.Background()
	before, err := p.ReadBalance(ctx, BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-02", TimezoneVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Compact(segment.Generation); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := log.WithVerifiedProjectionRoll(ctx, segment, func(roll VerifiedProjectionRoll) error {
			if err := p.ApplyVerifiedRoll(ctx, roll); err != nil {
				return err
			}
			return state.ApplyVerifiedRoll(ctx, roll)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	next, err := p.ReadReplayAnchor(ctx)
	expected := anchor
	expected.Head = Watermark{Generation: 2, Sequence: 5}
	if err != nil || next != expected || state.Watermark() != expected.Head {
		t.Fatal(next, state.Watermark(), err)
	}
	after, err := p.ReadBalance(ctx, BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-02", TimezoneVersion: 2})
	if err != nil || after != before {
		t.Fatal(before, after, err)
	}
	request, found, err := p.ReadRequest(ctx, "req_000001")
	if err != nil || !found || request.Finalized == nil {
		t.Fatal(request, found, err)
	}
	if err := p.ApplyVerifiedRoll(ctx, VerifiedProjectionRoll{}); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("forged proof accepted", err)
	}
	if err := (*LedgerProjection)(nil).ApplyVerifiedRoll(ctx, VerifiedProjectionRoll{}); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
}

func TestLedgerProjectionVerifiedRollRefusesWrongIdentityMissingSemanticWorkAndCancellation(t *testing.T) {
	log, p, _, segment := projectionRollFixture(t)
	ctx := context.Background()
	before, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed := segment
	changed.PlainChecksum = "wrong"
	called := false
	if err := log.WithVerifiedProjectionRoll(ctx, changed, func(VerifiedProjectionRoll) error { called = true; return nil }); err == nil || called {
		t.Fatal(called, err)
	}
	empty := projectionTestStore(t)
	if err := log.WithVerifiedProjectionRoll(ctx, segment, func(roll VerifiedProjectionRoll) error { return empty.ApplyVerifiedRoll(ctx, roll) }); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := log.WithVerifiedProjectionRoll(canceled, segment, func(VerifiedProjectionRoll) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal(called, err)
	}
	after, err := p.ReadReplayAnchor(ctx)
	if err != nil || after != before || log.Status().Load() != AccountingHealthy {
		t.Fatal(before, after, log.Status().Load(), err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.WithVerifiedProjectionRoll(ctx, segment, func(roll VerifiedProjectionRoll) error { return p.ApplyVerifiedRoll(ctx, roll) }); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
	if log.Status().Load() != AccountingHealthy {
		t.Fatal("consumer failure poisoned native", log.Status().Load())
	}
}

func TestLedgerProjectionVerifiedRollAuthenticatesEveryRetryIncludingCompressedTrailer(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "CRC-repaired-MAC", true: "gzip-trailer"}[compressed], func(t *testing.T) {
			log, p, _, segment := projectionRollFixture(t)
			ctx := context.Background()
			if err := log.WithVerifiedProjectionRoll(ctx, segment, func(roll VerifiedProjectionRoll) error { return p.ApplyVerifiedRoll(ctx, roll) }); err != nil {
				t.Fatal(err)
			}
			changed := filepath.Join(log.directory, segment.File)
			if compressed {
				compacted, err := log.Compact(segment.Generation)
				if err != nil {
					t.Fatal(err)
				}
				changed = filepath.Join(log.directory, compacted.File)
				raw, err := os.ReadFile(changed)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-8] ^= 1
				if err := os.WriteFile(changed, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				tamperWithSealedFrame(t, changed)
			}
			called := false
			if err := log.WithVerifiedProjectionRoll(ctx, segment, func(VerifiedProjectionRoll) error { called = true; return nil }); err == nil || called {
				t.Fatal("already advanced owner skipped native proof", called, err)
			}
			if log.Status().Load() != AccountingRecoveryRequired {
				t.Fatal(log.Status().Load())
			}
			if err := log.WithVerifiedProjectionRoll(ctx, segment, func(VerifiedProjectionRoll) error { called = true; return nil }); err == nil || called {
				t.Fatal(called, err)
			}
		})
	}
}

func TestLedgerProjectionVerifiedRollJoinsNativeCloseThroughCallback(t *testing.T) {
	log, p, _, segment := projectionRollFixture(t)
	entered, release, applied, closed := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		applied <- log.WithVerifiedProjectionRoll(context.Background(), segment, func(roll VerifiedProjectionRoll) error {
			close(entered)
			<-release
			return p.ApplyVerifiedRoll(context.Background(), roll)
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native proof callback did not enter")
	}
	go func() { closed <- log.Close() }()
	<-log.closeSignal
	select {
	case err := <-closed:
		t.Fatal("native Close exited before selected owner commit", err)
	default:
	}
	close(release)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
