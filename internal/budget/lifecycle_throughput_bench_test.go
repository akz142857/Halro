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

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
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
