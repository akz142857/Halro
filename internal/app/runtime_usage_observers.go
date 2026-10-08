package app

import (
	"context"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

type usageObservation struct {
	usage.DiskUsageStats
	Window     *usage.WindowState
	Checkpoint *usage.CheckpointState
}

// Status needs only position, so the legacy branch must not enumerate
// checkpoint segments just because metrics use the richer observation.
func (u *runtimeUsage) readPosition(ctx context.Context) (ledger.Watermark, error) {
	if u.disk != nil {
		stats, err := u.disk.ReadStats(ctx)
		return stats.Watermark, err
	}
	if err := ctx.Err(); err != nil {
		return ledger.Watermark{}, err
	}
	return u.Aggregate.Watermark(), nil
}

func (u *runtimeUsage) observe(ctx context.Context) (usageObservation, error) {
	if u.disk != nil {
		stats, err := u.disk.ReadStats(ctx)
		return usageObservation{DiskUsageStats: stats}, err
	}
	window, checkpoint := u.Aggregate.Windowed(), u.Aggregate.Checkpointed()
	return usageObservation{DiskUsageStats: usage.DiskUsageStats{Watermark: u.Aggregate.Watermark(), Metrics: u.Aggregate.Metrics()}, Window: &window, Checkpoint: &checkpoint}, nil
}

func (u *runtimeUsage) dashboard(ctx context.Context, now time.Time, period usage.Period) (usage.Dashboard, bool, error) {
	if u.disk != nil {
		result, err := u.disk.ReadDashboard(ctx, now, period, usage.DiskDashboardLimits{Hours: 256, ScanRows: 65536, InputBytes: 8 << 20, OutputBytes: 8 << 20})
		return result.Dashboard, result.Metrics.RequestsSuccess > 0, err
	}
	return u.Aggregate.Dashboard(now, period), u.Aggregate.Metrics().RequestsSuccess > 0, ctx.Err()
}

func (r *Runtime) usageUnavailable(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage unavailable"})
}

func (u *runtimeUsage) onboarding(ctx context.Context, resources onboardingResources) (usage.OnboardingEvidence, error) {
	if u.disk == nil {
		return onboardingEvidenceFromSnapshot(u.Aggregate.Metrics(), u.Aggregate.Snapshot()), ctx.Err()
	}
	// Bound target preparation before adding a second configuration copy.
	if len(resources.Providers) > 4096 || len(resources.Deployments) > 4096-len(resources.Providers) || len(resources.Routes) > 4096-len(resources.Providers)-len(resources.Deployments) {
		return usage.OnboardingEvidence{}, usage.ErrDiskUsageLimit
	}
	var targets usage.OnboardingTargets
	for _, v := range resources.Providers {
		if v.DeletedAt == nil {
			targets.Providers = append(targets.Providers, v.ID)
		}
	}
	for _, v := range resources.Deployments {
		if v.DeletedAt == nil {
			targets.Deployments = append(targets.Deployments, v.ID)
		}
	}
	for _, v := range resources.Routes {
		if v.DeletedAt == nil {
			targets.Routes = append(targets.Routes, v.ID)
		}
	}
	return u.disk.ReadOnboarding(ctx, targets, usage.DiskOnboardingLimits{Rows: 65536, InputBytes: 64 << 20, OutputBytes: 8 << 20, Targets: 4096, MaxDuration: time.Minute})
}

// Legacy adapter preserves its existing Snapshot contract. Disk reads never
// invoke it or construct a historical request-ID set.
func onboardingEvidenceFromSnapshot(metrics usage.Metrics, snapshot usage.Snapshot) usage.OnboardingEvidence {
	e := usage.OnboardingEvidence{Metrics: metrics, Providers: map[string]time.Time{}, Deployments: map[string]time.Time{}, Routes: map[string]time.Time{}}
	requests := map[string]struct{}{}
	for _, r := range snapshot.Requests {
		if r.Outcome == "success" {
			requests[r.RequestID] = struct{}{}
		}
	}
	update := func(m map[string]time.Time, id string, at time.Time) {
		if id != "" && at.After(m[id]) {
			m[id] = at
		}
	}
	for _, a := range snapshot.Attempts {
		if _, found := requests[a.RequestID]; found && a.Status == "success" {
			if newest, seen := e.Providers[a.ProviderID]; a.ProviderID != "" && (!seen || a.CompletedAt.After(newest)) {
				e.Providers[a.ProviderID] = a.CompletedAt
			}
			update(e.Deployments, a.DeploymentID, a.CompletedAt)
			update(e.Routes, a.RouteID, a.CompletedAt)
		}
	}
	if len(snapshot.Requests) > 0 {
		r := snapshot.Requests[len(snapshot.Requests)-1]
		e.LastRequest = &r
		for i := len(snapshot.Attempts) - 1; i >= 0; i-- {
			if snapshot.Attempts[i].RequestID == r.RequestID {
				a := snapshot.Attempts[i]
				e.LastAttempt = &a
				break
			}
		}
	}
	return e
}

func verificationFromOnboardingEvidence(e usage.OnboardingEvidence) *onboardingVerification {
	if e.LastRequest == nil {
		return nil
	}
	r := e.LastRequest
	v := &onboardingVerification{Outcome: r.Outcome, RequestID: r.RequestID, RequestedModel: r.RequestedModel, CompletedAt: r.CompletedAt}
	if a := e.LastAttempt; a != nil {
		v.HTTPStatus, v.ErrorClass = a.HTTPStatus, a.ErrorClass
	}
	return v
}
