package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func authenticatedRecoveryResourceBudget(t *testing.T) (config.Config, *resourcePressureRuntime) {
	return authenticatedRecoveryResourceBudgetWithDisk(t, true)
}

func authenticatedRecoveryResourceBudgetWithDisk(t *testing.T, diskEnabled bool) (config.Config, *resourcePressureRuntime) {
	t.Helper()
	b := runtimeRAMBudgetFixture()
	b.TotalRAMBytes, b.IORAMBytes, b.BackgroundRAMBytes = 768<<20, 256<<20, 128<<20
	b.MaxClaims, b.ControlRAMBytes = 20, 20*resourcebudget.ClaimRAMBytes
	// The whole-job envelope, pinned original input and pre-admitted COW
	// slots are simultaneous. This is explicit functional fixture capacity.
	b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes, b.RecoveryClaims = 512<<20, (512<<20)+8*resourcebudget.ClaimRAMBytes, 8
	if diskEnabled {
		// Explicit fixture capacity; this is not a measured production value.
		b.TotalDiskBytes = 256 << 20
		b.BackgroundDiskBytes = 256 << 20
	}
	limits := b.Limits(time.Hour)
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return config.Config{ResourceBudget: &b}, &resourcePressureRuntime{budget: m}
}

func authenticatedRecoveryResourceCandidate(t *testing.T, cfg config.Config, pressure *resourcePressureRuntime, native *ledger.Log, path string, key []byte) (*Runtime, *ledger.AuthenticatedLedgerProjection, *ledger.MaintenanceLease) {
	t.Helper()
	ctx := context.Background()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p *ledger.AuthenticatedLedgerProjection
	err = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture native cut mismatch"))
		}
		p, err = ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "auth"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-shared-recovery-test")), Budget: pressure.logicalBudget(), MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20})
		return err
	})
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	r := &Runtime{config: cfg, ledger: native, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: runtimeAccountingState{recovery: &atomic.Bool{}}}
	r.draining.Store(true)
	t.Cleanup(func() {
		lease.Close()
		native.Close()
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	return r, p, lease
}

func TestAuthenticatedRecoveryResourcesShareOwnerAndJoinCancelledNativeIO(t *testing.T) {
	var io *projectedWriterBlockedDurability
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{WrapDurability: func(f *os.File) ledger.DurabilityWriter {
		io = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		return io
	}})
	var once sync.Once
	unblock := func() { once.Do(func() { close(io.release) }) }
	defer unblock()
	cfg, pressure := authenticatedRecoveryResourceBudget(t)
	r, p, lease := authenticatedRecoveryResourceCandidate(t, cfg, pressure, l, path, key)
	m := pressure.logicalBudget()
	baseline := m.Snapshot().Used
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	io.block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := r.recoverAuthenticatedAccounting(ctx, lease, p, budget.RecoveryStandalone)
		done <- err
	}()
	select {
	case <-io.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual recovery native fsync not reached")
	}
	checkHeld := func() {
		t.Helper()
		s := m.Snapshot()
		const protected = ledger.AuthenticatedLedgerSourceRAMBytes + ledger.AuthenticatedLedgerReadRAMBytes + authenticatedstate.ReadRAMBytes + authenticatedstate.UpdateRAMBytes + 4096
		if !r.accountingState.recovery.Load() || s.RecoveryClaims != 8 || s.Classes[resourcebudget.Recovery].RAM != cfg.ResourceBudget.AccountingRecoveryRAMBytes+ledger.AuthenticatedRecoveryRAMBytes+4096+protected || s.Used.RAM < baseline.RAM+cfg.ResourceBudget.AccountingRecoveryRAMBytes+ledger.AuthenticatedRecoveryRAMBytes+protected {
			t.Fatal("whole job/pinned-input fees or owner slot retired early", s)
		}
	}
	checkHeld()
	alias := &Runtime{config: cfg, ledger: l, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: r.accountingState}
	alias.draining.Store(true)
	if _, err := alias.recoverAuthenticatedAccounting(context.Background(), lease, p, budget.RecoveryStandalone); !errors.Is(err, errAccountingRecoveryBusy) {
		t.Fatal("alias queued or acquired a second workspace", err)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("canceled native IO retired before fsync returned", err)
	case <-time.After(10 * time.Millisecond):
	}
	checkHeld()
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("business")); !errors.Is(err, ledger.ErrLedgerMaintenance) {
		t.Fatal("native exclusivity retired before actual IO", err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled durable recovery succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not join returned native IO")
	}
	if s := m.Snapshot(); s.Used != baseline || s.RecoveryClaims != 0 || r.accountingState.recovery.Load() {
		t.Fatal("whole job leaked fees/slot or refunded persistent candidate bytes", s, baseline)
	}
	if l.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("durable canceled recovery did not latch recovery-required")
	}
	lease.Close()
	l.Close()
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot().Used; s != (resourcebudget.Cost{}) {
		t.Fatal("actual candidate Discard did not refund persistent disk", s)
	}
	reopened, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	r2, fresh, nextLease := authenticatedRecoveryResourceCandidate(t, cfg, pressure, reopened, path, key)
	freshBaseline := m.Snapshot().Used
	result, err := r2.recoverAuthenticatedAccounting(context.Background(), nextLease, fresh, budget.RecoveryStandalone)
	if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized != 0 || result.ConfirmedHead.Sequence != 2 {
		t.Fatal("fresh retry duplicated/lost durable request terminal", result, err)
	}
	if s := m.Snapshot(); s.Used != freshBaseline || s.RecoveryClaims != 0 || s.Acquired[resourcebudget.Recovery] < 3 {
		t.Fatal("empty retry lost persistent owner or retained whole job", s, freshBaseline)
	}
}

func TestAuthenticatedRecoveryResourcesRefuseWithoutFactsOrLeaseTransfer(t *testing.T) {
	for _, shape := range []string{"missing_manager", "foreign_manager", "missing_config", "missing_slot", "active_business", "closed_candidate", "exhausted", "undersized_reserve", "too_few_claims", "canceled"} {
		t.Run(shape, func(t *testing.T) {
			l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
			cfg, pressure := authenticatedRecoveryResourceBudget(t)
			r, p, lease := authenticatedRecoveryResourceCandidate(t, cfg, pressure, l, path, key)
			m := pressure.logicalBudget()
			ctx := context.Background()
			want := error(nil)
			var held resourcebudget.Permit
			switch shape {
			case "missing_manager":
				r.resourcePressure = nil
			case "foreign_manager":
				_, r.resourcePressure = authenticatedRecoveryResourceBudget(t)
			case "missing_config":
				r.config.ResourceBudget = nil
			case "missing_slot":
				r.accountingState.recovery = nil
			case "active_business":
				r.draining.Store(false)
			case "closed_candidate":
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			case "exhausted":
				var err error
				held, err = m.TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes})
				if err != nil {
					t.Fatal(err)
				}
				defer held.Release()
				want = resourcebudget.ErrExhausted
			case "undersized_reserve":
				r.config.ResourceBudget.RecoveryReserveRAMBytes = 128 << 20
			case "too_few_claims":
				r.config.ResourceBudget.RecoveryClaims = 6
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			baseline := m.Snapshot().Used
			head, hash, _ := l.ChainHead()
			result, err := r.recoverAuthenticatedAccounting(ctx, lease, p, budget.RecoveryStandalone)
			if err == nil || want != nil && !errors.Is(err, want) || result != (budget.ProjectedRecoveryResult{}) {
				t.Fatal("invalid resource job was admitted", result, err)
			}
			after, afterHash, _ := l.ChainHead()
			if after != head || afterHash != hash || m.Snapshot().Used != baseline || l.Status().Load() != ledger.AccountingHealthy {
				t.Fatal("refusal changed facts, fees or native health", err)
			}
			if _, err := lease.ValidateOwner(l); err != nil {
				t.Fatal("refused job transferred caller's lease", err)
			}
			if r.accountingState.recovery != nil && r.accountingState.recovery.Load() {
				t.Fatal("refused job retained shared slot")
			}
		})
	}
}

func TestAuthenticatedRecoveryResourcesRAMOnlyConfigurationStillRejectsDiskOwner(t *testing.T) {
	cfg, _ := authenticatedRecoveryResourceBudgetWithDisk(t, false)
	limits := cfg.ResourceBudget.Limits(time.Hour)
	if limits.Total.Disk != 0 || limits.Classes[resourcebudget.Background].Disk != 0 {
		t.Fatal("production configuration silently enabled unreconciled disk ownership")
	}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildAuthenticatedLedgerProjection(context.Background(), path, key, filepath.Join(t.TempDir(), "auth"), report, ledger.AuthenticatedLedgerOptions{
		Identity: sha256.Sum256([]byte("same-production-manager-test")), Budget: m, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20,
	})
	if !errors.Is(err, resourcebudget.ErrExhausted) || p != nil {
		t.Fatal("RAM-only configured pool admitted a persistent disk owner", p, err)
	}
	if s := m.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Acquired[resourcebudget.IO] == 0 || s.Rejected[resourcebudget.Background] == 0 {
		t.Fatal("refusal did not isolate actual disk admission or leaked source fee", s)
	}
	head, hash, _ := l.ChainHead()
	if head != report.Head || hash != report.ChainHash || l.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("configured disk refusal changed native facts/health")
	}
}

func TestAuthenticatedRecoveryResourcesConfiguredDiskBudgetChargesOwnerUntilDiscard(t *testing.T) {
	cfg, pressure := authenticatedRecoveryResourceBudget(t)
	limits := cfg.ResourceBudget.Limits(time.Hour)
	if limits.Total.Disk != 256<<20 || limits.Classes[resourcebudget.Background].Disk != 256<<20 {
		t.Fatal("owner fixture does not exercise the product disk config", limits)
	}
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
	defer l.Close()
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildAuthenticatedLedgerProjection(context.Background(), path, key, filepath.Join(t.TempDir(), "auth"), report, ledger.AuthenticatedLedgerOptions{
		Identity: sha256.Sum256([]byte("configured-disk-owner-lifetime")), Budget: pressure.logicalBudget(), MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pressure.logicalBudget().Snapshot().Used.Disk; got != 128<<20 {
		t.Fatal("persistent projection did not retain its configured logical ceiling", got)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if got := pressure.logicalBudget().Snapshot().Used.Disk; got != 0 {
		t.Fatal("disk charge retired before/after the wrong owner boundary", got)
	}
}

func TestAuthenticatedRecoveryResourcesUseDedicatedScopeUnderPressure(t *testing.T) {
	for _, state := range []resourcebudget.Pressure{resourcebudget.Unknown, resourcebudget.Pressured} {
		t.Run(map[resourcebudget.Pressure]string{resourcebudget.Unknown: "unknown", resourcebudget.Pressured: "pressured"}[state], func(t *testing.T) {
			var events []ledger.Event
			for _, id := range []string{"request", "unstarted", "late"} {
				events = append(events, projectedWriterAcceptance(id))
			}
			for i := 1; i <= 2; i++ {
				e := events[i]
				e.Kind, e.EventID, e.AttemptID = ledger.EventReservationCreated, "lease_"+e.RequestID, "attempt_"+e.RequestID
				e.ReservationMicrosUSD = ledger.MicrosUSD(10)
				events = append(events, e)
				if i == 2 {
					e.Kind, e.EventID = ledger.EventAttemptStarted, "started_late"
					events = append(events, e)
					terminal := events[i]
					terminal.Kind, terminal.EventID, terminal.Outcome = ledger.EventRequestFinalized, "original_terminal", "interrupted"
					events = append(events, terminal)
				}
			}
			l, path, key := projectedWriterLog(t, events, ledger.Options{})
			cfg, pressure := authenticatedRecoveryResourceBudget(t)
			r, p, lease := authenticatedRecoveryResourceCandidate(t, cfg, pressure, l, path, key)
			m := pressure.logicalBudget()
			if err := m.SetPressure(state); err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.ReadPosition(context.Background()); !errors.Is(err, resourcebudget.ErrPressure) {
				t.Fatal("ordinary admission", err)
			}
			before := m.Snapshot()
			result, err := r.recoverAuthenticatedAccounting(context.Background(), lease, p, budget.RecoveryStandalone)
			if err != nil || result.AttemptsSettled != 2 || result.RequestsFinalized != 2 || result.ConfirmedHead.Sequence != uint64(len(events))+4 {
				t.Fatal(result, err)
			}
			after := m.Snapshot()
			if after.Used != before.Used || after.Normal != before.Normal || after.RecoveryClaims != 0 || after.Pressure != state || after.Acquired[resourcebudget.IO] != before.Acquired[resourcebudget.IO] || after.Acquired[resourcebudget.Background] != before.Acquired[resourcebudget.Background] || after.Acquired[resourcebudget.Recovery] <= before.Acquired[resourcebudget.Recovery]+1 || r.accountingState.recovery.Load() {
				t.Fatal("resource or authority leak", before, after)
			}
			if _, _, err := p.ReadPosition(context.Background()); !errors.Is(err, resourcebudget.ErrPressure) {
				t.Fatal("maintenance authority escaped", err)
			}
			head, _, ok := l.ChainHead()
			if !ok || head != result.ConfirmedHead {
				t.Fatal("actual native terminal missing", head, ok)
			}
			if err := m.SetPressure(resourcebudget.Healthy); err != nil {
				t.Fatal(err)
			}
			final, found, err := p.ReadFinalization(context.Background(), "late")
			if err != nil || !found || final.Event.EventID != "original_terminal" {
				t.Fatal("late settlement rewrote terminal", final, found, err)
			}
			pending, err := p.ReadPendingLeaseStats(context.Background(), time.Now())
			if err != nil || pending.Count != 0 {
				t.Fatal("attempt obligations remain", pending, err)
			}

		})
	}
}

func TestAuthenticatedRecoveryResourcesConstructorFailureReleasesWholeJob(t *testing.T) {
	var events []ledger.Event
	for _, id := range []string{"request", "unstarted", "late"} {
		events = append(events, projectedWriterAcceptance(id))
	}
	for i := 1; i <= 2; i++ {
		e := events[i]
		e.Kind, e.EventID, e.AttemptID = ledger.EventReservationCreated, "lease_"+e.RequestID, "attempt_"+e.RequestID
		e.ReservationMicrosUSD = ledger.MicrosUSD(10)
		events = append(events, e)
		if i == 2 {
			e.Kind, e.EventID = ledger.EventAttemptStarted, "started_late"
			events = append(events, e)
			terminal := events[i]
			terminal.Kind, terminal.EventID, terminal.Outcome = ledger.EventRequestFinalized, "original_terminal", "interrupted"
			events = append(events, terminal)
		}
	}
	l, path, key := projectedWriterLog(t, events, ledger.Options{})
	cfg, pressure := authenticatedRecoveryResourceBudget(t)
	r, p, lease := authenticatedRecoveryResourceCandidate(t, cfg, pressure, l, path, key)
	m := pressure.logicalBudget()
	baseline := m.Snapshot().Used
	// Change only the permitted owner relation. Constructor failure must retire
	// its workspace/slot without consuming or closing another Log's lease.
	other, _, _ := projectedWriterLog(t, nil, ledger.Options{})
	r.ledger = other
	if _, err := r.recoverAuthenticatedAccounting(context.Background(), lease, p, budget.RecoveryStandalone); err == nil {
		t.Fatal("foreign native writer was admitted")
	}
	if s := m.Snapshot(); s.Used != baseline || s.RecoveryClaims != 0 || r.accountingState.recovery.Load() {
		t.Fatal("constructor failure retained shared workspace", s)
	}
	if _, err := lease.ValidateOwner(l); err != nil {
		t.Fatal("failed constructor closed caller lease", err)
	}
}
