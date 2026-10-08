package budget

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

const legacyRecoveryRequests = 129 // More than two maximum-sized recovery pages.

type legacyRecoveryReader interface {
	usage.FinalizationReader
	ledger.AccountingWorkUnitReader
	ReadBalance(context.Context, ledger.BalanceKey) (ledger.Balance, error)
	ReadRequest(context.Context, string) (ledger.ProjectedRequest, bool, error)
	ReadPendingLeaseStats(context.Context, time.Time) (ledger.PendingLeaseStatistics, error)
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
}

func legacyRecoveryEvents(t *testing.T, attributed bool) []ledger.Event {
	t.Helper()
	var events []ledger.Event
	for i := 0; i < legacyRecoveryRequests; i++ {
		shape := []string{"accepted", "unstarted", "started", "settled", "late"}[i%5]
		events = append(events, projectedRecoveryFixture(t, i, shape, attributed)...)
	}
	return events
}

func legacyRecoveryResources(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	// The occupied Active class is additional to the existing 256 MiB
	// authenticated recovery fixture (callback + nested reads/update scratch).
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 384 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Active] = resourcebudget.Cost{RAM: 128 << 20}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	resources, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if got := resources.Snapshot(); got.Used != (resourcebudget.Cost{}) || got.Owners != 0 {
			t.Error("legacy recovery retained resources", got)
		}
	})
	return resources
}

func occupyLegacyNewAdmission(t *testing.T, resources *resourcebudget.Manager) {
	t.Helper()
	for i := 0; i < 4; i++ {
		permit, err := resources.TryAcquire(resourcebudget.Active, resourcebudget.Cost{RAM: 32 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := permit.Release(); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := resources.TryAcquire(resourcebudget.Active, resourcebudget.Cost{RAM: 32 << 20}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("new admission is not actually exhausted", err)
	}
}

func legacyAuthenticatedCandidate(t *testing.T, native *ledger.Log, path string, key []byte, resources *resourcebudget.Manager) *authenticatedRecoveryTestWriter {
	t.Helper()
	ctx := context.Background()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	var p *ledger.AuthenticatedLedgerProjection
	err = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("legacy recovery fixture native cut mismatch"))
		}
		p, err = ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "auth"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("oversized-legacy-recovery")), Budget: resources, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	if err := p.ValidateNativeWriter(ctx, native, lease); err != nil {
		t.Fatal(err)
	}
	return &authenticatedRecoveryTestWriter{t: t, native: native, p: p, lease: lease}
}

func legacyDiskUsage(t *testing.T, native *ledger.Log, reader usage.FinalizationReader) *usage.DiskProjection {
	t.Helper()
	p, err := usage.CreateDiskProjectionWithLimit(filepath.Join(t.TempDir(), "usage.db"), reader, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	if _, err := native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, p.Apply); err != nil {
		t.Fatal(err)
	}
	return p
}

func verifyLegacyRecovery(t *testing.T, native *ledger.Log, reader legacyRecoveryReader, disk *usage.DiskProjection, originals map[string]usage.RequestSummary) {
	t.Helper()
	ctx := context.Background()
	stats, err := reader.ReadPendingLeaseStats(ctx, time.Now())
	if err != nil || stats.Count != 0 {
		t.Fatal("pending attempts remain", stats, err)
	}
	balance, err := reader.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_recovery", PeriodID: "2026-10-02", TimezoneVersion: 3})
	if err != nil || balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 3850 {
		t.Fatal("recovery lost spend or retained monetary reservations", balance, err)
	}
	for i := 0; i < legacyRecoveryRequests; i++ {
		id := fmt.Sprint("req_", i)
		request, found, err := reader.ReadRequest(ctx, id)
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("unfinalized/orphan request", id, found, err)
		}
		if i%5 != 4 && !reflect.DeepEqual(request.Finalized.Event, recoveryFinalization(request.Accepted.Event)) {
			t.Fatal("unstable recovery terminal payload", id)
		}
		if original := request.Accepted.Event; original.WorkUnitID != "" {
			view, found, err := reader.ReadWorkUnitAccounting(ctx, original.ProjectID, original.WorkUnitID, true)
			wantCost := int64(0)
			if i%5 >= 2 {
				wantCost = 50
			}
			if err != nil || !found || view.Inflight || len(view.Runs) != 1 || view.Runs[0].ReservedMicrosUSD != 0 || view.Runs[0].CommittedMicrosUSD != wantCost {
				t.Fatal("recovery retained governance activity/reservations or lost Run attribution", id, view, err)
			}
		}
		if original, exists := originals[id]; exists {
			summary, found, err := disk.ReadRequest(ctx, id)
			if err != nil || !found || !reflect.DeepEqual(summary, original) {
				t.Fatal("late settlement rewrote original Usage summary", id, summary, original, err)
			}
		}
	}
	usageStats, err := disk.ReadStats(ctx)
	head, hash, headErr := reader.ReadPosition(ctx)
	if err != nil || headErr != nil || usageStats.Watermark != head || usageStats.Metrics.ActiveRequests != 0 || usageStats.Totals.Requests != 129 || usageStats.Totals.CostMicrosUSD != 3850 {
		t.Fatal("Usage did not completely catch up or retained activity", usageStats, head, err, headErr)
	}
	anchor, err := disk.ReadReplayAnchor(ctx)
	if err != nil || anchor.Head != head || anchor.Hash != hash {
		t.Fatal("Usage lost the authenticated native chain identity", anchor, head, err)
	}
	// Count the real native occurrences, not just the idempotent derived result.
	seen := make(map[string]bool)
	settlements, finalized := 0, 0
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventAttemptSettled || record.Event.Kind == ledger.EventRequestFinalized {
			if seen[record.Event.EventID] {
				return errors.New("recovery appended a duplicate terminal fact")
			}
			seen[record.Event.EventID] = true
			if record.Event.Kind == ledger.EventAttemptSettled {
				settlements++
			} else {
				finalized++
			}
		}
		return nil
	}); err != nil || settlements != 103 || finalized != 129 {
		t.Fatal("native recovery facts lost or duplicated", settlements, finalized, err)
	}
}

func TestLegacyRecoveryAboveNewAdmissionBudgetCompletesBothSets(t *testing.T) {
	for _, auth := range []bool{false, true} {
		for _, rows := range []int{1, 64} {
			for _, attributed := range []bool{false, true} {
				t.Run(fmt.Sprintf("authenticated=%v/rows=%d/run=%v", auth, rows, attributed), func(t *testing.T) {
					ctx := context.Background()
					base, path, key := newProjectedRecoveryTest(t, legacyRecoveryEvents(t, attributed))
					resources := legacyRecoveryResources(t)
					var reader legacyRecoveryReader = base.p
					var writer RecoveryWriter = base
					var candidate *authenticatedRecoveryTestWriter
					if auth {
						candidate = legacyAuthenticatedCandidate(t, base.log, path, key, resources)
						reader, writer = candidate.p, candidate
					}
					occupyLegacyNewAdmission(t, resources)
					disk := legacyDiskUsage(t, base.log, reader)
					originals := make(map[string]usage.RequestSummary)
					for i := 4; i < legacyRecoveryRequests; i += 5 {
						id := fmt.Sprint("req_", i)
						summary, found, err := disk.ReadRequest(ctx, id)
						if err != nil || !found {
							t.Fatal(found, err)
						}
						originals[id] = summary
					}
					before := resources.Snapshot()
					cut, _, err := reader.ReadPosition(ctx)
					if err != nil {
						t.Fatal(err)
					}
					limits := ledger.ProjectionPageLimits{Rows: rows, Bytes: 1 << 20}
					recover := func() (ProjectedRecoveryResult, error) {
						if auth {
							return RecoverAuthenticatedRequests(ctx, candidate.p, writer, RecoveryStandalone, limits)
						}
						return RecoverProjectedRequests(ctx, base.p, writer, RecoveryStandalone, limits)
					}
					result, err := recover()
					if err != nil || result.AttemptsSettled != 77 || result.RequestsFinalized != 104 || result.InputHead != cut {
						t.Fatal("oversized legacy input was blocked/truncated", result, err)
					}
					usageAnchor, err := disk.ReadReplayAnchor(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := base.log.ReplayAuthenticated(ctx, usageAnchor, disk.Apply); err != nil {
						t.Fatal(err)
					}
					verifyLegacyRecovery(t, base.log, reader, disk, originals)
					again, err := recover()
					if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || again.ConfirmedHead != result.ConfirmedHead {
						t.Fatal("completed retry repeated work/skipped confirmation", again, err)
					}
					if after := resources.Snapshot(); after.Classes[resourcebudget.Active] != before.Classes[resourcebudget.Active] || after.Acquired[resourcebudget.Active] != before.Acquired[resourcebudget.Active] || after.Rejected[resourcebudget.Active] != before.Rejected[resourcebudget.Active] {
						t.Fatal("recovery consumed new request admission", before, after)
					}
				})
			}
		}
	}
}

type legacyInterruptedWriter struct {
	RecoveryWriter
	phase     string
	configure func(ledger.Event)
	settled   int
	finalized int
}

func (w *legacyInterruptedWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	if event.Kind == ledger.EventAttemptSettled {
		w.settled++
	} else if event.Kind == ledger.EventRequestFinalized {
		w.finalized++
	}
	stop := strings.Contains(w.phase, "settlement") && w.settled == 64 || strings.Contains(w.phase, "finalization") && w.finalized == 64
	if stop && strings.HasPrefix(w.phase, "durable-") {
		w.configure(event)
	}
	record, err := w.RecoveryWriter.AppendApplyRecovery(ctx, event)
	if err == nil && stop && strings.HasPrefix(w.phase, "applied-") {
		return record, errRecoveryInjected
	}
	return record, err
}

func (w *legacyInterruptedWriter) ConfirmRecoveryPrefix(ctx context.Context, head ledger.Watermark) error {
	if w.phase == "confirm-finalization" && w.finalized == 64 {
		return errRecoveryInjected
	}
	return w.RecoveryWriter.ConfirmRecoveryPrefix(ctx, head)
}

func TestLegacyRecoveryInterruptedAfterPageBoundaryRebuildsWithoutDuplicates(t *testing.T) {
	for _, auth := range []bool{false, true} {
		for _, phase := range []string{"durable-settlement", "applied-settlement", "durable-finalization", "applied-finalization", "confirm-finalization"} {
			t.Run(fmt.Sprintf("authenticated=%v/%s", auth, phase), func(t *testing.T) {
				ctx := context.Background()
				base, path, key := newProjectedRecoveryTest(t, legacyRecoveryEvents(t, true))
				resources := legacyRecoveryResources(t)
				var reader legacyRecoveryReader = base.p
				interrupt := &legacyInterruptedWriter{RecoveryWriter: base, phase: phase, configure: func(event ledger.Event) { base.failKind, base.failAfterDurable = event.Kind, true }}
				var candidate *authenticatedRecoveryTestWriter
				if auth {
					candidate = legacyAuthenticatedCandidate(t, base.log, path, key, resources)
					reader, interrupt.RecoveryWriter = candidate.p, candidate
					interrupt.configure = func(event ledger.Event) { candidate.failKind, candidate.failAfterDurable = event.Kind, true }
				}
				occupyLegacyNewAdmission(t, resources)
				initialUsage := legacyDiskUsage(t, base.log, reader)
				originals := make(map[string]usage.RequestSummary)
				for i := 4; i < legacyRecoveryRequests; i += 5 {
					id := fmt.Sprint("req_", i)
					row, found, err := initialUsage.ReadRequest(ctx, id)
					if err != nil || !found {
						t.Fatal(found, err)
					}
					originals[id] = row
				}
				limits := ledger.ProjectionPageLimits{Rows: 64, Bytes: 1 << 20}
				var first ProjectedRecoveryResult
				var err error
				if auth {
					first, err = RecoverAuthenticatedRequests(ctx, candidate.p, interrupt, RecoveryStandalone, limits)
				} else {
					first, err = RecoverProjectedRequests(ctx, base.p, interrupt, RecoveryStandalone, limits)
				}
				if !errors.Is(err, errRecoveryInjected) || strings.Contains(phase, "settlement") && first.RequestsFinalized != 0 {
					t.Fatal("interruption did not stop before publishing success", first, err)
				}
				if err := initialUsage.Discard(); err != nil {
					t.Fatal(err)
				}
				if auth {
					if err := candidate.lease.Close(); err != nil {
						t.Fatal(err)
					}
					if err := candidate.p.Discard(); err != nil {
						t.Fatal(err)
					}
				}
				if err := base.p.Discard(); err != nil {
					t.Fatal(err)
				}
				if err := base.log.Close(); err != nil {
					t.Fatal(err)
				}
				base.log, err = ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
				if err != nil {
					t.Fatal(err)
				}
				base.p = rebuildRecoveryTestProjection(t, path, key, "retry.db")
				base.failKind, base.failAfterDurable = 0, false
				reader = base.p
				var retry ProjectedRecoveryResult
				if auth {
					candidate = legacyAuthenticatedCandidate(t, base.log, path, key, resources)
					reader = candidate.p
					// Base's cleanup already joins the reopened native owner.
					retry, err = RecoverAuthenticatedRequests(ctx, candidate.p, candidate, RecoveryStandalone, limits)
				} else {
					retry, err = RecoverProjectedRequests(ctx, base.p, base, RecoveryStandalone, limits)
				}
				wantSettled, wantFinalized := uint64(13), uint64(104)
				if strings.Contains(phase, "finalization") {
					wantSettled, wantFinalized = 0, 40
				}
				if err != nil || retry.AttemptsSettled != wantSettled || retry.RequestsFinalized != wantFinalized {
					t.Fatal("fresh retry lost/repeated durable progress", retry, err)
				}
				freshUsage := legacyDiskUsage(t, base.log, reader)
				verifyLegacyRecovery(t, base.log, reader, freshUsage, originals)
			})
		}
	}
}
