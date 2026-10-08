package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func authenticatedWriterOptions(t *testing.T) ledger.AuthenticatedLedgerOptions {
	t.Helper()
	// Functional capacity includes a pinned recovery view and the complete
	// pre-admitted source/COW profile; this is not a Linux capacity acceptance.
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 384 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s := m.Snapshot().Used; s != (resourcebudget.Cost{}) {
			t.Error("authenticated maintenance leaked resources", s)
		}
	})
	return ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-maintenance-test")), Budget: m, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20}
}

func TestAuthenticatedRecoveryWriterStandaloneUsesActualMaintenanceOwner(t *testing.T) {
	var events []ledger.Event
	for _, name := range []string{"empty_attempt", "unstarted", "late"} {
		events = append(events, projectedWriterAcceptance(name))
	}
	for i := 1; i <= 2; i++ {
		e := events[i]
		e.EventID = "lease_" + e.RequestID
		e.Kind = ledger.EventReservationCreated
		e.AttemptID = "attempt_" + e.RequestID
		e.ReservationMicrosUSD = ledger.MicrosUSD(10)
		events = append(events, e)
		if i == 2 {
			started := e
			started.Kind = ledger.EventAttemptStarted
			started.EventID = "start_" + e.RequestID
			events = append(events, started)
			f := events[i]
			f.Kind = ledger.EventRequestFinalized
			f.EventID = "original_terminal"
			f.Outcome = "interrupted"
			events = append(events, f)
		}
	}
	l, path, key := projectedWriterLog(t, events, ledger.Options{})
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	o := authenticatedWriterOptions(t)
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildAuthenticatedLedgerProjection(context.Background(), path, key, filepath.Join(t.TempDir(), "auth"), report, o)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	r := &Runtime{ledger: l}
	r.draining.Store(true)
	w, err := r.newAuthenticatedRecoveryWriter(context.Background(), lease, p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	result, err := budget.RecoverAuthenticatedRequests(context.Background(), p, w, w.role, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.AttemptsSettled != 2 || result.RequestsFinalized != 2 || result.ConfirmedHead.Sequence != uint64(len(events))+4 {
		t.Fatal(result, err)
	}
	final, found, err := p.ReadFinalization(context.Background(), "late")
	if err != nil || !found || final.Event.EventID != "original_terminal" {
		t.Fatal("late recovery rewrote terminal fact", final, err)
	}
	before := l.Stats().Records
	again, err := budget.RecoverAuthenticatedRequests(context.Background(), p, w, w.role, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || l.Stats().Records != before {
		t.Fatal(again, err)
	}
	r.draining.Store(false)
	if err := w.ConfirmRecoveryPrefix(context.Background(), again.ConfirmedHead); err == nil {
		t.Fatal("withdrawn maintenance authority remained live")
	}
	if _, err := w.AppendApplyRecovery(context.Background(), projectedWriterAcceptance("after_revocation")); err == nil || l.Stats().Records != before || l.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("revoked writer changed native history", err)
	}
}

func TestAuthenticatedRecoveryWriterRequiresDrainedMatchingNativeOwner(t *testing.T) {
	l, path, key := projectedWriterLog(t, nil, ledger.Options{})
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	o := authenticatedWriterOptions(t)
	report, _, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ledger.BuildAuthenticatedLedgerProjection(context.Background(), path, key, filepath.Join(t.TempDir(), "auth"), report, o)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	r := &Runtime{ledger: l}
	if w, err := r.newAuthenticatedRecoveryWriter(context.Background(), lease, p); w != nil || err == nil {
		t.Fatal("active business owner accepted maintenance writer")
	}
	r.draining.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w, err := r.newAuthenticatedRecoveryWriter(ctx, lease, p); w != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(w, err)
	}
	if l.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("invalid caller poisoned native facts")
	}
}
