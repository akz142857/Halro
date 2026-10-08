package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type pressureSampleFunc func(context.Context) resourcebudget.Sample

func (f pressureSampleFunc) Sample(ctx context.Context) resourcebudget.Sample { return f(ctx) }

func TestOperationDiskHeadroomHonorsConfiguredStopLine(t *testing.T) {
	policy := &config.ResourcePressure{DiskEnterBytes: 100, InodesEnter: 10}
	for _, tc := range []struct {
		name       string
		bytes      int64
		inodes     int64
		wantReject bool
	}{
		{name: "above", bytes: 101, inodes: 11},
		{name: "bytes at stop", bytes: 100, inodes: 100, wantReject: true},
		{name: "inodes at stop", bytes: 1000, inodes: 10, wantReject: true},
		{name: "unknown", bytes: -1, inodes: 100, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationDiskHeadroomBelowStopLine(tc.bytes, tc.inodes, policy); got != tc.wantReject {
				t.Fatalf("rejected=%v want %v", got, tc.wantReject)
			}
		})
	}
}

func runtimePressureFixture() config.ResourcePressure {
	return config.ResourcePressure{RuntimeEnterBytes: 100, RuntimeExitBytes: 80, CgroupEnterBytes: 200, CgroupExitBytes: 160, HeadroomEnterBytes: 20, HeadroomExitBytes: 40, DiskEnterBytes: 100, DiskExitBytes: 200, InodesEnter: 10, InodesExit: 20, MaxAge: config.Duration(time.Second), Cooldown: config.Duration(time.Nanosecond), SampleInterval: config.Duration(time.Millisecond)}
}
func runtimeLowSample() resourcebudget.Sample {
	return resourcebudget.Sample{At: time.Now(), RuntimeUsed: 50, DiskAvailable: 500, InodesAvailable: 50, GroupCount: 1, CgroupID: [32]byte{1}, Groups: [resourcebudget.MaxCgroupDepth]resourcebudget.GroupMemory{{Current: 100, Max: 500, MaxLimited: true}}}
}
func newFixturePressure(t *testing.T, source resourcebudget.SampleSource) *resourcePressureRuntime {
	t.Helper()
	p := runtimePressureFixture()
	m, err := resourcebudget.NewMonitor(source, p.Policy(), p.SampleInterval.Value())
	if err != nil {
		t.Fatal(err)
	}
	return &resourcePressureRuntime{monitor: m}
}
func waitRuntimePressure(t *testing.T, p *resourcePressureRuntime, state resourcebudget.Pressure) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.monitor.Snapshot().State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(p.monitor.Snapshot())
}
func startFixturePressure(t *testing.T, p *resourcePressureRuntime) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.monitor.Run(ctx, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("pressure owner did not stop")
		}
	})
	return cancel
}

type countedBody struct{ reads atomic.Int64 }

func (b *countedBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (*countedBody) Close() error               { return nil }

func TestResourcePressureNewWorkRoutesRefuseBeforeBodyAndKeepRecoveryRoutes(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", nil)
	r.resourcePressure = newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	router := r.gatewayRouter()
	work := []string{"/v1/chat/completions", "/v1/responses", "/v1/embeddings", "/v1/moderations", "/v1/images/generations", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/rerank", "/v1/async/invocations", "/v1/files", "/v1/batches", "/v1/messages", "/v1/messages/count_tokens", "/halro/v1/work-units", "/halro/v1/runs"}
	for _, path := range work {
		t.Run(path, func(t *testing.T) {
			body := &countedBody{}
			req := httptest.NewRequest(http.MethodPost, path, body)
			req.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "resource_pressure") || body.reads.Load() != 0 {
				t.Fatal(path, w.Code, w.Body.String(), body.reads.Load())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w.Header())
			}
			if strings.HasPrefix(path, "/v1/messages") && !strings.Contains(w.Body.String(), "overloaded_error") {
				t.Fatal(w.Body.String())
			}
		})
	}
	if r.resourcePressure.rejected.Load() != uint64(len(work)) {
		t.Fatal(r.resourcePressure.rejected.Load())
	}
	body := &countedBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer invalid-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || body.reads.Load() != 0 || r.resourcePressure.rejected.Load() != uint64(len(work)) {
		t.Fatal("authentication must run first", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/v1/models"}, {"GET", "/v1/responses/missing"}, {"POST", "/v1/responses/missing/cancel"}, {"DELETE", "/v1/responses/missing"}, {"GET", "/v1/async/invocations/missing"}, {"POST", "/v1/async/invocations/missing/cancel"}, {"POST", "/v1/batches/missing/cancel"}, {"POST", "/halro/v1/work-units/missing/close"}, {"POST", "/halro/v1/work-units/missing/outcomes"}, {"POST", "/halro/v1/runs/missing/close"}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), "resource_pressure") {
			t.Fatal("existing work route re-entered new admission", tc, w.Code, w.Body.String())
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "resource_pressure") {
		t.Fatal(w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/health/live", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatal("capacity pressure must not kill liveness", w.Code)
	}
}
func TestResourcePressureAlreadyAdmittedHTTPWorkSurvivesPressure(t *testing.T) {
	var high atomic.Bool
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample {
		s := runtimeLowSample()
		if high.Load() {
			s.RuntimeUsed = 100
		}
		return s
	}))
	startFixturePressure(t, p)
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	accepted := httptest.NewRecorder()
	handler := r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		if req.Context().Err() != nil {
			t.Error("pressure cancelled accepted work")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	go func() { defer close(done); handler.ServeHTTP(accepted, httptest.NewRequest("POST", "/work", nil)) }()
	<-entered
	high.Store(true)
	waitRuntimePressure(t, p, resourcebudget.Pressured)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, httptest.NewRequest("POST", "/work", nil))
	if refused.Code != 503 {
		t.Fatal(refused.Code)
	}
	close(release)
	<-done
	if accepted.Code != http.StatusAccepted {
		t.Fatal(accepted.Code)
	}
	high.Store(false)
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	forwarded := false
	r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/work", nil))
	if !forwarded {
		t.Fatal("cooldown did not restore admission")
	}
}
func TestResourcePressureMetricsDoNotInventMissingOrUnstartedUsage(t *testing.T) {
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample {
		return resourcebudget.Sample{At: time.Now(), Issues: resourcebudget.RuntimeUnavailable | resourcebudget.CgroupUnavailable | resourcebudget.DiskUnavailable}
	}))
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	render := func() string {
		var b bytes.Buffer
		w := bufio.NewWriter(&b)
		r.writeResourcePressureMetrics(w)
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if s := render(); !strings.Contains(s, "halro_resource_pressure_enabled 1") || strings.Contains(s, "halro_resource_pressure_runtime_bytes") {
		t.Fatal(s)
	}
	startFixturePressure(t, p)
	deadline := time.Now().Add(time.Second)
	for p.monitor.Snapshot().Samples == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s := render()
	if p.monitor.Snapshot().Samples == 0 || !strings.Contains(s, "state=\"unknown\"} 1") {
		t.Fatal(s)
	}
	for _, name := range []string{"halro_resource_pressure_runtime_bytes", "halro_resource_pressure_cgroup_bytes", "halro_resource_pressure_disk_available_bytes", "halro_resource_pressure_inodes_available"} {
		if strings.Contains(s, name) {
			t.Fatal("unavailable emitted as zero", s)
		}
	}
	r.resourcePressure = nil
	if s := render(); !strings.Contains(s, "halro_resource_pressure_enabled 0") || strings.Contains(s, "halro_resource_pressure_state{") {
		t.Fatal(s)
	}
}
func TestResourcePressureConfiguredRuntimeStartsAndCloseJoins(t *testing.T) {
	cfg := testConfig(t)
	p := runtimePressureFixture()
	cfg.ResourcePressure = &p
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for r.resourcePressure.monitor.Snapshot().Samples == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.resourcePressure == nil || r.resourcePressure.monitor.Snapshot().Samples == 0 {
		t.Fatal("configured sampler did not start")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if s := r.resourcePressure.monitor.Snapshot(); s.Running || s.State != resourcebudget.Unknown {
		t.Fatal("Close returned before sampler joined", s)
	}
	if _, err = newResourcePressure(config.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePressure = &config.ResourcePressure{}
	if _, err = Open(context.Background(), cfg, slog.Default()); err == nil || !strings.Contains(err.Error(), "resource_pressure") {
		t.Fatal("invalid calibration accepted", err)
	}
}
func TestResourcePressureRuntimeCloseWaitsForOwnedSamplingRead(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { close(entered); <-release; return runtimeLowSample() }))
	r.resourcePressure = p
	r.backgroundWait.Add(1)
	go func() { defer r.backgroundWait.Done(); r.runResourcePressure(r.backgroundCtx, p) }()
	<-entered
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case <-r.backgroundCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel sampling owner")
	}
	if s := p.monitor.Snapshot(); s.State != resourcebudget.Unknown || s.Issues&resourcebudget.SampleCancelled == 0 {
		t.Fatal(s)
	}
	select {
	case err = <-done:
		t.Fatal("Close claimed success with live sample read", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case err = <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join released owner")
	}
	if p.monitor.Snapshot().Running {
		t.Fatal("sampling survived runtime close")
	}
}

func TestResourcePressureMetricsOmitExpiredOrCancelledPhysicalGauges(t *testing.T) {
	var calls atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	source := pressureSampleFunc(func(context.Context) resourcebudget.Sample {
		s := runtimeLowSample()
		if calls.Add(1) == 3 {
			close(entered)
			<-release
		}
		return s
	})
	policy := runtimePressureFixture()
	policy.MaxAge = config.Duration(100 * time.Millisecond)
	policy.SampleInterval = config.Duration(2 * time.Millisecond)
	m, err := resourcebudget.NewMonitor(source, policy.Policy(), policy.SampleInterval.Value())
	if err != nil {
		t.Fatal(err)
	}
	p := &resourcePressureRuntime{monitor: m}
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()
	<-entered
	render := func() string {
		var b bytes.Buffer
		w := bufio.NewWriter(&b)
		r.writeResourcePressureMetrics(w)
		w.Flush()
		return b.String()
	}
	initial := render()
	if !strings.Contains(initial, "halro_resource_pressure_runtime_bytes") {
		t.Fatal("fresh real fixture gauges missing", initial)
	}
	time.Sleep(120 * time.Millisecond)
	for _, phase := range []string{"stale", "cancelled"} {
		if phase == "cancelled" {
			cancel()
		}
		s := render()
		if !strings.Contains(s, "state=\"unknown\"} 1") {
			t.Fatal(phase, s)
		}
		for _, name := range []string{"halro_resource_pressure_runtime_bytes", "halro_resource_pressure_cgroup_bytes", "halro_resource_pressure_disk_available_bytes", "halro_resource_pressure_inodes_available"} {
			if strings.Contains(s, name) {
				t.Fatal(phase, "old reading published as current", s)
			}
		}
	}
	unblock()
	<-done
	if s := render(); !strings.Contains(s, "reason=\"monitor_inactive\"} 1") || strings.Contains(s, "halro_resource_pressure_runtime_bytes") {
		t.Fatal(s)
	}
}
