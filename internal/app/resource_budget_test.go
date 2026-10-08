package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func budgetedNativeCachesAndPricingRAM() int64 {
	return governancelimits.MinCacheRAMBytes + auditlimits.MinCacheRAMBytes + pricinglimits.MinLifetimeRAMBytes
}

func runtimeRAMBudgetFixture() config.ResourceBudget {
	return config.ResourceBudget{AccountingRecoveryRAMBytes: accountinglimits.MinRecoveryRAMBytes, GovernanceLogRAMBytes: governancelimits.MinCacheRAMBytes, AuditLogRAMBytes: auditlimits.MinCacheRAMBytes, PricingLifetimeRAMBytes: pricinglimits.MinLifetimeRAMBytes, PricingIORAMBytes: pricinglimits.MinIORAMBytes, IORAMBytes: pricinglimits.MinIORAMBytes, TotalRAMBytes: (226 << 20) + resourcebudget.ClaimRAMBytes, ActiveRAMBytes: 24 << 20, BackgroundRAMBytes: 24 << 20, ControlRAMBytes: 10 * resourcebudget.ClaimRAMBytes, RecoveryReserveRAMBytes: (170 << 20) + resourcebudget.ClaimRAMBytes, RecoveryRAMBytes: (170 << 20) - 3*resourcebudget.ClaimRAMBytes, MaxClaims: 10, RecoveryClaims: 5, MaxOwners: 4, RequestRAMBytes: 16 << 20, DeferredRAMBytes: 16 << 20}
}

func TestResourceBudgetCaptureOpenRequiresEnvelopeAndCloseSealsStoreBeforeReopen(t *testing.T) {
	r, _, reopen := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, budget := runtimePressureFixture(), runtimeRAMBudgetFixture()
		budget.CaptureRAMBytes = 16 << 20
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &budget
		cfg.Gateway.FailureCapture.Enabled = true
	})
	if r.failureCapture == nil || r.resourcePressure.captureEnvelope() != 16<<20 || r.resourcePressure.logicalBudget() == nil {
		t.Fatal("Open lost capture resource configuration")
	}
	manager := r.resourcePressure.logicalBudget()
	if manager.Snapshot().Acquired[resourcebudget.Recovery] < 7 || manager.Snapshot().RecoveryClaims != 3 || manager.Snapshot().Classes[resourcebudget.Recovery].RAM != budgetedNativeCachesAndPricingRAM() {
		t.Fatal("Standalone did not retain shared pricing/Governance lifetime owners", manager.Snapshot())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("Runtime Close leaked shared owners", manager.Snapshot())
	}
	if _, err := r.failureCapture.PutContext(context.Background(), failurecapture.Record{}); !errors.Is(err, failurecapture.ErrClosed) {
		t.Fatal("Runtime Close left capture store writable", err)
	}
	round := reopen()
	t.Cleanup(func() { _ = round.Close() })
	if round.failureCapture == nil || round.resourcePressure.captureEnvelope() != 16<<20 {
		t.Fatal("capture budget not restored on reopen")
	}
	// Open also validates callers that bypass Config.Validate before startup.
	cfg := r.config
	budget := *cfg.ResourceBudget
	budget.CaptureRAMBytes = 0
	cfg.ResourceBudget = &budget
	if _, err := newResourcePressure(cfg); err == nil || !strings.Contains(err.Error(), "resource_budget") {
		t.Fatal("startup accepted unowned capture", err)
	}
}
func attachRuntimeFixtureBudget(t *testing.T, p *resourcePressureRuntime) {
	t.Helper()
	cfg := runtimeRAMBudgetFixture()
	b, err := resourcebudget.New(cfg.Limits(runtimePressureFixture().MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.monitor.AttachManager(b); err != nil {
		t.Fatal(err)
	}
	p.budget, p.requestRAM, p.deferredRAM = b, cfg.RequestRAMBytes, cfg.DeferredRAMBytes
}

func TestResourceBudgetHTTPEnvelopeSharedWithBackgroundSurvivesCancellationAndReturnsAfterHandler(t *testing.T) {
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	attachRuntimeFixtureBudget(t, p)
	cancel := startFixturePressure(t, p)
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	accepted := httptest.NewRecorder()
	h := r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	go func() { defer close(done); h.ServeHTTP(accepted, httptest.NewRequest("POST", "/work", nil)) }()
	<-entered
	defer func() {
		select {
		case <-done:
		default:
			close(release)
			<-done
		}
	}()
	if s := p.budget.Snapshot(); s.Classes[resourcebudget.Active].RAM != p.requestRAM || s.Used.RAM != p.requestRAM+resourcebudget.ClaimRAMBytes {
		t.Fatal(s)
	}
	body := &countedBody{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/work", body))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "resource_budget") || body.reads.Load() != 0 {
		t.Fatal(w.Code, w.Body.String(), body.reads.Load())
	}
	io, err := p.budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: pricinglimits.MinIORAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer io.Release()
	if _, err := p.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: p.deferredRAM}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("HTTP and background do not share total", err)
	}
	cancel()
	if s := p.budget.Snapshot(); s.Pressure != resourcebudget.Unknown || s.Classes[resourcebudget.Active].RAM != p.requestRAM {
		t.Fatal("cancel released admitted work", s)
	}
	close(release)
	<-done
	if accepted.Code != http.StatusAccepted {
		t.Fatal(accepted.Code)
	}
	if err := io.Release(); err != nil {
		t.Fatal(err)
	}
	if s := p.budget.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 {
		t.Fatal("HTTP leaked envelope", s)
	}
	var metrics bytes.Buffer
	bw := bufio.NewWriter(&metrics)
	r.writeResourcePressureMetrics(bw)
	_ = bw.Flush()
	for _, want := range []string{"halro_resource_budget_enabled 1", "halro_resource_budget_ram_bytes 0", "halro_resource_budget_disk_bytes 0", "halro_resource_budget_class_disk_bytes{class=\"background\"} 0", "halro_resource_budget_rejections_total{class=\"active\"} 1", "halro_resource_budget_rejections_total{class=\"background\"} 1", "halro_resource_budget_pressure_state{state=\"unknown\"} 1"} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatal("missing metric", want, metrics.String())
		}
	}
}

func TestResourceBudgetHTTPReservesCompletionRAMAndDiskAtomically(t *testing.T) {
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	cfg := runtimeRAMBudgetFixture()
	cfg.CompletionRAMBytes = 4096
	cfg.CompletionDiskBytes = 32 << 10
	cfg.TotalDiskBytes = 1 << 20
	cfg.RecoveryReserveDiskBytes = 64 << 10
	m, err := resourcebudget.New(cfg.Limits(runtimePressureFixture().MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.monitor.AttachManager(m); err != nil {
		t.Fatal(err)
	}
	p.budget, p.requestRAM = m, cfg.RequestRAMBytes
	p.completionRAM, p.completionDisk = cfg.CompletionRAMBytes, cfg.CompletionDiskBytes
	cancel := startFixturePressure(t, p)
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/work", nil))
	}()
	<-entered
	defer func() {
		select {
		case <-done:
		default:
			close(release)
			<-done
		}
	}()
	if got := m.Snapshot(); got.Classes[resourcebudget.Active].RAM != cfg.RequestRAMBytes || got.CompletionReserved != (resourcebudget.Cost{RAM: cfg.CompletionRAMBytes, Disk: cfg.CompletionDiskBytes}) {
		t.Fatal("new work did not atomically reserve its completion envelope", got)
	}
	body := &countedBody{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/work", body))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "resource_budget") || body.reads.Load() != 0 {
		t.Fatal("completion-capacity refusal reached the request body", w.Code, w.Body.String(), body.reads.Load())
	}
	cancel()
	close(release)
	<-done
	if got := m.Snapshot(); got.Used != (resourcebudget.Cost{}) || got.CompletionReserved != (resourcebudget.Cost{}) || got.Owners != 0 {
		t.Fatal("HTTP handler exit leaked active or completion capacity", got)
	}
}

func TestSelectedHTTPCompletionProfileSharesClassWithIndependentWork(t *testing.T) {
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	cfg := runtimeRAMBudgetFixture()
	cfg.CompletionRAMBytes = 16 << 10
	m, err := resourcebudget.New(cfg.Limits(runtimePressureFixture().MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.monitor.AttachManager(m); err != nil {
		t.Fatal(err)
	}
	p.budget, p.requestRAM, p.completionRAM = m, cfg.RequestRAMBytes, cfg.CompletionRAMBytes
	if err := p.setSelectedCompletionProfile([]resourcebudget.WorkspaceRequirement{{Class: resourcebudget.IO, RAM: 8 << 10}}); err != nil {
		t.Fatal(err)
	}
	cancel := startFixturePressure(t, p)
	defer cancel()
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	independent, err := m.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: 4096}, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.IO, RAM: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Active.Release()
	defer independent.Close()
	beforeHTTP := m.Snapshot()
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: p}}
	entered := false
	h := r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entered = true
		if got := m.Snapshot(); got.CompletionReserved.RAM != 12<<10 {
			t.Fatal("request consumed the class ceiling or lost independent work", got)
		}
		if err := resourcebudget.WithCompletionWorkspace(req.Context(), func(ctx context.Context) error {
			return m.WithRAMWorkspace(ctx, p.completionProfile(), func(context.Context) error { return nil })
		}); err != nil {
			t.Fatal("actual request workspace was not funded", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/work", nil))
	if !entered || w.Code != http.StatusNoContent {
		t.Fatal("independent work prevented healthy admission", w.Code, w.Body.String())
	}
	if got := m.Snapshot(); got.CompletionReserved != beforeHTTP.CompletionReserved || got.Used != beforeHTTP.Used || got.Owners != beforeHTTP.Owners {
		t.Fatal("HTTP retirement released independent work or leaked request owner", got)
	}
}

func TestResourceBudgetHTTPPanicReturnsEnvelopeAndActualRoutesRefuseBeforeBody(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", nil)
	p := newFixturePressure(t, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	attachRuntimeFixtureBudget(t, p)
	r.resourcePressure = p
	startFixturePressure(t, p)
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("fixture") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/work", nil))
	}()
	if s := p.budget.Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal(s)
	}
	held, err := p.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: p.deferredRAM})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	io, err := p.budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: pricinglimits.MinIORAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer io.Release()
	router := r.gatewayRouter()
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/embeddings", "/v1/moderations", "/v1/images/generations", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/rerank", "/v1/async/invocations", "/v1/files", "/v1/batches", "/v1/messages", "/v1/messages/count_tokens", "/halro/v1/work-units", "/halro/v1/runs"} {
		body := &countedBody{}
		req := httptest.NewRequest("POST", path, body)
		req.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "resource_budget") || body.reads.Load() != 0 {
			t.Fatal(path, w.Code, w.Body.String(), body.reads.Load())
		}
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", &countedBody{})
	req.Header.Set("Authorization", "Bearer invalid")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("budget precedes authentication", w.Code)
	}
}

func runtimeCandidateBudgetFixture() config.ResourceBudget {
	b := runtimeRAMBudgetFixture()
	b.TotalRAMBytes = 1 << 30
	b.ReplicationCandidateRAMBytes = replication.MinPrimaryCandidateRAMBytes
	b.ReplicationQueueRAMBytes = replicationlimits.MinQueueRAMBytes
	b.ReplicationReadRAMBytes = replicationlimits.MinReadRAMBytes
	b.ReplicationReaderRAMBytes = replicationlimits.MinNativeReaderRAMBytes
	b.RecoveryClaims = 12
	b.MaxClaims = 15
	b.ControlRAMBytes = 15 * resourcebudget.ClaimRAMBytes
	b.RecoveryRAMBytes = 3*(b.ReplicationCandidateRAMBytes+b.ReplicationQueueRAMBytes+b.ReplicationReadRAMBytes) + b.ReplicationReaderRAMBytes + b.GovernanceLogRAMBytes + b.AuditLogRAMBytes + b.PricingLifetimeRAMBytes + b.PricingIORAMBytes + b.AccountingRecoveryRAMBytes
	b.RecoveryReserveRAMBytes = b.RecoveryRAMBytes + 12*resourcebudget.ClaimRAMBytes
	return b
}
func TestResourceBudgetHAOpenRejectsCandidateEnvelopeBeforeStartup(t *testing.T) {
	cfg := testConfig(t)
	p, b := runtimePressureFixture(), runtimeCandidateBudgetFixture()
	cfg.ResourcePressure, cfg.ResourceBudget, cfg.Replication = &p, &b, &config.Replication{}
	pressure, err := newResourcePressure(cfg)
	if err != nil || pressure.logicalBudget() == nil {
		t.Fatal("HA shared budget missing", err)
	}
	for _, shape := range []func(*config.ResourceBudget){
		func(b *config.ResourceBudget) { b.ReplicationCandidateRAMBytes = 0 },
		func(b *config.ResourceBudget) { b.ReplicationQueueRAMBytes = 0 },
		func(b *config.ResourceBudget) { b.ReplicationReadRAMBytes = 0 },
		func(b *config.ResourceBudget) { b.ReplicationReaderRAMBytes = 0 },
		func(b *config.ResourceBudget) { b.GovernanceLogRAMBytes = 0 },
		func(b *config.ResourceBudget) { b.RecoveryClaims = 2 },
		func(b *config.ResourceBudget) { b.RecoveryRAMBytes-- },
		func(b *config.ResourceBudget) { b.RecoveryReserveRAMBytes-- },
	} {
		copy := b
		shape(&copy)
		cfg.ResourceBudget = &copy
		if _, err := newResourcePressure(cfg); err == nil || !strings.Contains(err.Error(), "resource_budget") {
			t.Fatal("runtime startup bypass accepted unowned reconstruction", err)
		}
	}
}
