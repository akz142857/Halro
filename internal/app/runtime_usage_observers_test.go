package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func observerEnvelope(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUsageDiskObserversPublicWireAndSelectedMetrics(t *testing.T) {
	r, cookie := openSeededRuntime(t)
	wantDashboard := observerEnvelope(t, authenticatedAdminGet(t, r, cookie, "/admin/api/v1/dashboard"))
	wantOnboarding := observerEnvelope(t, authenticatedAdminGet(t, r, cookie, "/admin/api/v1/onboarding/readiness"))
	delete(wantOnboarding, "evaluated_at")
	wantStats, err := r.usage.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := installDiskUsageQueries(t, r)
	gotDashboard := observerEnvelope(t, authenticatedAdminGet(t, r, cookie, "/admin/api/v1/dashboard"))
	if !reflect.DeepEqual(gotDashboard["usage"], wantDashboard["usage"]) || gotDashboard["first_value_reached"] != wantDashboard["first_value_reached"] {
		t.Fatal("dashboard differs", gotDashboard, wantDashboard)
	}
	gotOnboarding := observerEnvelope(t, authenticatedAdminGet(t, r, cookie, "/admin/api/v1/onboarding/readiness"))
	delete(gotOnboarding, "evaluated_at")
	if !reflect.DeepEqual(gotOnboarding, wantOnboarding) {
		t.Fatal("onboarding differs", gotOnboarding, wantOnboarding)
	}
	gotStats, err := r.usage.observe(context.Background())
	if err != nil || gotStats.Metrics != wantStats.Metrics || gotStats.Watermark != wantStats.Watermark || gotStats.LogicalBytes <= 0 || gotStats.Window != nil || gotStats.Checkpoint != nil {
		t.Fatal("selected observation borrowed resident state", gotStats, err)
	}
	system := observerEnvelope(t, authenticatedAdminGet(t, r, cookie, "/admin/api/v1/system/status"))
	raw, _ := json.Marshal(wantStats.Watermark)
	var watermark any
	_ = json.Unmarshal(raw, &watermark)
	if !reflect.DeepEqual(system["usage_watermark"], watermark) {
		t.Fatal("system status lost disk watermark", system)
	}
	metrics := renderMetricsForTest(t, r)
	for _, series := range []string{"halro_usage_projection_selected 1\n", "halro_usage_read_unavailable 0\n", fmt.Sprintf("halro_usage_watermark_sequence %d\n", wantStats.Watermark.Sequence), fmt.Sprintf("halro_requests_total{status=\"success\"} %d\n", wantStats.Metrics.RequestsSuccess)} {
		if !strings.Contains(metrics, series) {
			t.Fatal("missing selected metric", series)
		}
	}
	if strings.Contains(metrics, "halro_usage_window_") || strings.Contains(metrics, "halro_usage_checkpoint_") {
		t.Fatal("disk credited resident/checkpoint metrics")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/api/v1/dashboard", "/admin/api/v1/onboarding/readiness", "/admin/api/v1/system/status"} {
		w := authenticatedAdminGet(t, r, cookie, path)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatal("failed owner returned success", path, w.Code)
		}
	}
	metrics = renderMetricsForTest(t, r)
	if !strings.Contains(metrics, "halro_usage_read_unavailable 1\n") || !strings.Contains(metrics, "halro_activation_stale ") {
		t.Fatal("failed usage silenced diagnosis")
	}
	for _, family := range []string{"halro_requests_total", "halro_attempts_total", "halro_active_requests", "halro_usage_watermark_sequence", "halro_usage_projection_logical_bytes", "halro_fallbacks_total", "halro_pricing_unknown_attempts_total"} {
		if strings.Contains(metrics, family) {
			t.Fatal("failed read fabricated series", family)
		}
	}
}

func TestUsageDiskOnboardingPreservesCurrentTrafficProof(t *testing.T) {
	r, _ := openSeededRuntime(t)
	snapshot, metrics := usageSnapshotForTest(t, r), usageMetricsForTest(t, r)
	installDiskUsageQueries(t, r)
	at := snapshot.Attempts[0].CompletedAt
	for _, edit := range []time.Duration{-time.Second, 0, time.Nanosecond} {
		resources := readyOnboardingResources(at)
		resources.Deployments[0].ID = "dep_1"
		resources.PriceReady = map[string]bool{"dep_1": true}
		resources.Routes[0].DeploymentID = "dep_1"
		resources.Providers[0].LastTestStatus = ""
		resources.Deployments[0].LastTestStatus = ""
		resources.Providers[0].UpdatedAt = at.Add(edit)
		resources.Deployments[0].UpdatedAt = at.Add(edit)
		want := evaluateOnboardingReadiness(at, resources, metrics, snapshot)
		evidence, err := r.usage.onboarding(context.Background(), resources)
		if err != nil {
			t.Fatal(err)
		}
		got := evaluateOnboardingWithEvidence(at, resources, evidence)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("revision-bound proof differs", edit, got, want)
		}
		if edit > 0 && got.Goals[0].State == onboardingGoalComplete {
			t.Fatal("old traffic survived provider edit")
		}
	}
	resources := onboardingResources{Providers: make([]domain.ProviderInstance, 4097)}
	if _, err := r.usage.onboarding(context.Background(), resources); err == nil {
		t.Fatal("unbounded target preparation accepted")
	}
}

func TestUsageDiskMetricsStayReadableDuringBusinessDrainAndJoinTerminalClose(t *testing.T) {
	r, _ := openSeededRuntime(t)
	installDiskUsageQueries(t, r)
	// Business retirement must retain HA/pressure diagnosis.
	if err := r.drainAccountingBusiness(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	metrics := renderMetricsForTest(t, r)
	if !strings.Contains(metrics, "halro_usage_projection_selected 1\n") {
		t.Fatal("business drain silenced metrics")
	}
	var writers []*blockingExportResponse
	var done []chan error
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			for _, w := range writers {
				close(w.release)
			}
		})
	}
	t.Cleanup(unblock)
	for i := 0; i < r.config.Metrics.MaxConcurrentScrapes; i++ {
		w := &blockingExportResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		writers = append(writers, w)
		d := make(chan error, 1)
		done = append(done, d)
		go func() { d <- r.writeMetrics(context.Background(), w) }()
		awaitDrainSignal(t, w.entered)
	}
	busy := httptest.NewRecorder()
	if err := r.writeMetrics(context.Background(), busy); err == nil || busy.Code != http.StatusServiceUnavailable {
		t.Fatal("metrics cap queued or admitted", err, busy.Code)
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.usage.metrics.mu.Lock()
		retired := r.usage.metrics.retired
		r.usage.metrics.mu.Unlock()
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not retire metrics")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed physical metrics Write", err)
	default:
	}
	if _, err := r.store.ReadMetadataAppliedPosition(context.Background()); err != nil {
		t.Fatal("Metadata closed during metrics response", err)
	}
	late := httptest.NewRecorder()
	if err := r.writeMetrics(context.Background(), late); err == nil || late.Code != http.StatusServiceUnavailable {
		t.Fatal("late scrape admitted", err, late.Code)
	}
	unblock()
	for _, d := range done {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
