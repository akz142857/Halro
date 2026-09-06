package budget

// Throughput harness for the second ceiling ADR 0012's "Amendment 2026-08-07"
// identifies and puts out of scope: lockProject is held across appendApplyRecord,
// so same-project requests cannot share a WAL batch no matter how many are in
// flight. The amendment reports roughly 30 request lifecycles per second per
// project on the reference host regardless of concurrency; this makes that a
// standing measurement rather than a remark, and gives the accounting protocol's
// own decision record a starting number.
//
// The project count is a parameter because it is the axis that distinguishes the
// two ceilings: raising the pricing ceiling is invisible to a single-project load
// precisely because this one takes over.
//
//	go test ./internal/budget/ -run '^$' -bench Lifecycle -benchtime 200x

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// BenchmarkRequestLifecycle walks the five Ledger events of one request
// lifecycle: request begin, lease reservation, start, settlement, finalize.
func BenchmarkRequestLifecycle(b *testing.B) {
	for _, projects := range []int{1, 8, 64} {
		for _, workers := range []int{1, 8, 64} {
			b.Run(fmt.Sprintf("projects=%d/workers=%d", projects, workers), func(b *testing.B) {
				manager, _, closeLog := newTestManager(b)
				defer closeLog()
				snapshot := testPriceSnapshot(b, domain.BillingModeMetered)
				ctx := context.Background()

				var next atomic.Int64
				var failures atomic.Int64
				var group sync.WaitGroup
				b.ResetTimer()
				start := time.Now()
				for worker := 0; worker < workers; worker++ {
					group.Add(1)
					go func() {
						defer group.Done()
						for {
							index := int(next.Add(1)) - 1
							if index >= b.N {
								return
							}
							if err := runLifecycle(ctx, manager, snapshot, fmt.Sprintf("project_bench_%d", index%projects), index); err != nil {
								if failures.Add(1) == 1 {
									b.Error(err)
								}
								return
							}
						}
					}()
				}
				group.Wait()
				elapsed := time.Since(start)
				b.StopTimer()
				if elapsed > 0 {
					b.ReportMetric(float64(b.N)/elapsed.Seconds(), "lifecycles/s")
					b.ReportMetric(float64(b.N*5)/elapsed.Seconds(), "events/s")
				}
			})
		}
	}
}

// BenchmarkRunAttributedRequestLifecycle measures the production path added by
// Run Governance. Work Unit and Run creation stay outside the timed region so
// the result isolates the per-request cost of dual project/run admission and
// attribution across the same five-event request lifecycle.
func BenchmarkRunAttributedRequestLifecycle(b *testing.B) {
	for _, projects := range []int{1, 8, 64} {
		for _, workers := range []int{1, 8, 64} {
			b.Run(fmt.Sprintf("projects=%d/workers=%d", projects, workers), func(b *testing.B) {
				manager, _, closeLog := newTestManager(b)
				defer closeLog()
				snapshot := testPriceSnapshot(b, domain.BillingModeMetered)
				ctx := context.Background()
				type attribution struct {
					projectID  string
					workUnitID string
					runID      string
				}
				attributions := make([]attribution, projects)
				for project := 0; project < projects; project++ {
					projectID := fmt.Sprintf("project_bench_%d", project)
					workUnit, _, err := manager.CreateWorkUnit(ctx, projectID, "key_bench", domain.MaxOpenWorkUnits, testGovernanceIntent(fmt.Sprintf("benchmark-create-work-unit-%d", project)))
					if err != nil {
						b.Fatal(err)
					}
					run, _, err := manager.CreateRun(ctx, projectID, "key_bench", workUnit.ID, 1_000_000_000, 24*time.Hour, domain.MaxActiveRuns, testGovernanceIntent(fmt.Sprintf("benchmark-create-run-%d", project)))
					if err != nil {
						b.Fatal(err)
					}
					attributions[project] = attribution{projectID: projectID, workUnitID: workUnit.ID, runID: run.ID}
				}

				var next atomic.Int64
				var failures atomic.Int64
				var group sync.WaitGroup
				b.ResetTimer()
				start := time.Now()
				for worker := 0; worker < workers; worker++ {
					group.Add(1)
					go func() {
						defer group.Done()
						for {
							index := int(next.Add(1)) - 1
							if index >= b.N {
								return
							}
							attribution := attributions[index%projects]
							if err := runAttributedLifecycle(ctx, manager, snapshot, attribution.projectID, attribution.workUnitID, attribution.runID, index); err != nil {
								if failures.Add(1) == 1 {
									b.Error(err)
								}
								return
							}
						}
					}()
				}
				group.Wait()
				elapsed := time.Since(start)
				b.StopTimer()
				if elapsed > 0 {
					b.ReportMetric(float64(b.N)/elapsed.Seconds(), "lifecycles/s")
					b.ReportMetric(float64(b.N*5)/elapsed.Seconds(), "events/s")
				}
			})
		}
	}
}

func runLifecycle(ctx context.Context, manager *Manager, snapshot *domain.PriceSnapshot, projectID string, index int) error {
	request, err := manager.BeginRequest(ctx, projectID, fmt.Sprintf("req_bench_%d", index))
	if err != nil {
		return fmt.Errorf("begin request: %w", err)
	}
	attempt, err := manager.ReserveLeaseDetailed(ctx, request, 1_000_000_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 50, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{
		RouteID: "route_bench", DeploymentID: "dep_bench", ProviderID: "provider_bench",
		ProviderModel: "model-bench", AttemptNumber: 1,
	})
	if err != nil {
		return fmt.Errorf("reserve lease: %w", err)
	}
	if err := manager.MarkStarted(ctx, attempt); err != nil {
		return fmt.Errorf("mark started: %w", err)
	}
	if err := manager.Settle(ctx, attempt, Settlement{
		Outcome: "success", ProviderInputTokens: 10, ProviderOutputTokens: 20, CommittedMicrosUSD: 50,
	}); err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	if err := manager.Finalize(ctx, request, "success"); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	return nil
}

func runAttributedLifecycle(ctx context.Context, manager *Manager, snapshot *domain.PriceSnapshot, projectID, workUnitID, runID string, index int) error {
	request, err := manager.BeginRequestAttributed(ctx, projectID, "key_bench", fmt.Sprintf("req_bench_%d", index), "model-bench", workUnitID, runID)
	if err != nil {
		return fmt.Errorf("begin attributed request: %w", err)
	}
	attempt, err := manager.ReserveLeaseDetailed(ctx, request, 1_000_000_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 50, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{
		RouteID: "route_bench", DeploymentID: "dep_bench", ProviderID: "provider_bench",
		ProviderModel: "model-bench", AttemptNumber: 1,
	})
	if err != nil {
		return fmt.Errorf("reserve attributed lease: %w", err)
	}
	if err := manager.MarkStarted(ctx, attempt); err != nil {
		return fmt.Errorf("mark attributed attempt started: %w", err)
	}
	if err := manager.Settle(ctx, attempt, Settlement{
		Outcome: "success", ProviderInputTokens: 10, ProviderOutputTokens: 20, CommittedMicrosUSD: 50,
	}); err != nil {
		return fmt.Errorf("settle attributed attempt: %w", err)
	}
	if err := manager.Finalize(ctx, request, "success"); err != nil {
		return fmt.Errorf("finalize attributed request: %w", err)
	}
	return nil
}

// The project lock counters are what make ADR 0018's per-project ceiling visible
// in production, where the only other symptom is latency rising for no stated
// reason. Assert they move, and that contention is actually observable: with
// several workers on one project, waiting must dominate holding.
func TestProjectLockStatsExposeContention(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	ctx := context.Background()

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			if err := runLifecycle(ctx, manager, snapshot, "project_contended", worker); err != nil {
				t.Error(err)
			}
		}(worker)
	}
	group.Wait()

	stats := manager.ProjectLockStats()
	// Two acquisitions per lifecycle, not five: only the reservation makes a
	// decision, and it takes the lock once to admit and once to hand the amount
	// over to the Ledger. The other four events carry no decision and take no
	// lock at all — if this climbs back toward five, a durable step has been put
	// back inside the critical section (ADR 0018).
	if stats.Acquisitions != 16 {
		t.Fatalf("counted %d acquisitions for 8 lifecycles, want 16 (two per lifecycle)", stats.Acquisitions)
	}
	if stats.HeldDuration <= 0 {
		t.Fatal("lock hold time went unmeasured")
	}
	if stats.WaitDuration <= 0 {
		t.Fatal("eight workers on one project produced no measured wait")
	}
	// The hold is now in-memory arithmetic rather than a durable append, so it
	// should be far below one fsync. This is the property the change bought, and
	// stating it as a bound keeps a future durable step from hiding here.
	meanHold := stats.HeldDuration / time.Duration(stats.Acquisitions)
	if meanHold > time.Millisecond {
		t.Fatalf("mean hold is %s; the admission decision should be arithmetic, not I/O", meanHold)
	}
}
