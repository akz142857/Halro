package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestDefaultHASelectedRuntimeUsesActualSevenRootsAndTLSReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture, old, _ := prepareHASelectedTransportFixture(t, ctx)
	proveDefaultHASelectedRuntime(t, ctx, fixture, old)
}

func proveDefaultHASelectedRuntime(t *testing.T, ctx context.Context, fixture haMetadataMaintenanceFixture, old [2]*haSelectedRuntimeFoundation) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, owner := range old {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := owner.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var runtimes [2]*Runtime
	var pressureHigh atomic.Bool
	for i, cfg := range fixture.configs {
		limits := *cfg.ResourceBudget
		limits.MemoryStateRequired = true
		cfg.ResourceBudget = &limits
		options := OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample {
			s := runtimeLowSample()
			if i == 0 && pressureHigh.Load() {
				s.RuntimeUsed = 100
			}
			return s
		})}
		var err error
		runtimes[i], err = OpenWithOptions(ctx, cfg, logger, options)
		if err != nil {
			t.Fatal("actual default HA Open", i, err)
		}
		r := runtimes[i]
		t.Cleanup(func() {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		})
		if !r.haSelectedApplicationOwner || r.haSelectedState == nil || r.selectedState != nil || r.accountingState.projection != nil || r.usage.Aggregate != nil || r.governance.owner != nil || r.store != r.haSelectedState.metadataStore {
			t.Fatal("default HA opened a compatibility owner", i)
		}
		info, err := os.Stat(cfg.MetadataPath())
		if err != nil || !info.IsDir() {
			t.Fatal("default HA touched legacy metadata trap", err)
		}
	}
	primary, replica := runtimes[0], runtimes[1]
	c := primary.haSelectedState.reopened.Owners.Components
	if err := c.NativeLedger.ValidateHASelectedFrozenMaintenanceRead(ctx, c.Lease); err == nil {
		t.Fatal("ordinary context impersonated frozen seven-root read")
	}
	var expired context.Context
	if err := c.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		expired = ctx
		if err := c.NativeLedger.ValidateHASelectedFrozenMaintenanceRead(ctx, c.Lease); err != nil {
			return err
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := c.NativeLedger.ValidateHASelectedFrozenMaintenanceRead(canceled, c.Lease); err == nil {
			return errors.New("cancelled actual frozen loan remained usable")
		}
		return nil
	}); err != nil {
		t.Fatal("actual frozen maintenance read", err)
	}
	if err := c.NativeLedger.ValidateHASelectedFrozenMaintenanceRead(expired, c.Lease); err == nil {
		t.Fatal("expired frozen loan remained usable")
	}
	if _, err := primary.haSelectedState.reopened.Derivative.CatchUpAuthenticatedFromNative(ctx, c.NativeLedger, c.Lease, c.Budget); err == nil {
		t.Fatal("generic API upgraded activated HA maintenance source")
	}
	if primary.gateway == nil || primary.gatewayService == nil || primary.accounting != primary.haSelectedState.business.accounting || primary.governance.manager == nil || replica.governance.manager == nil || replica.accounting != nil {
		t.Fatal("actual role services missing")
	}
	before := httptest.NewRecorder()
	primary.ready(before, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if before.Code != http.StatusServiceUnavailable {
		t.Fatal("default HA Ready before real peers", before.Code, before.Body.String())
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, r := range runtimes {
		go func(r *Runtime) { done <- r.haSelectedState.serveSelectedTransport(runCtx) }(r)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	for {
		ready := httptest.NewRecorder()
		primary.ready(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if ready.Code == http.StatusOK {
			break
		}
		select {
		case err := <-primary.replication.fatal:
			t.Fatal("default real startup", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err(), ready.Body.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	unauth := httptest.NewRecorder()
	primary.gatewayRouter().ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal("actual default gateway unavailable", unauth.Code, unauth.Body.String())
	}
	proveDefaultHAAcceptedHTTPCompletion(t, ctx, primary, replica, &pressureHigh)
	request, err := primary.accounting.BeginRequestDetailed(ctx, "prj_actual_default", "key_actual_default", "actual-default-ha-request", "chat")
	if err != nil {
		t.Fatal("actual default accounting admission", err)
	}
	if err := primary.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal("actual default confirmed terminal", err)
	}
	if _, err := primary.haSelectedState.reopened.Derivative.CatchUpHASelectedStartup(ctx, c.NativeLedger, c.Lease, c.Budget); err == nil {
		t.Fatal("closed actual startup lease reauthorized Usage")
	}
	head, _, err := primary.haSelectedState.reopened.Owners.Components.Ledger.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := primary.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.replication.coordinator.WaitRecoveryTarget(ctx, complete); err != nil {
		t.Fatal(err)
	}
	target := primary.replication.publisher.Snapshot().AppliedIndex
	for replica.replication.publisher.Snapshot().AppliedIndex < target {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, found, err := replica.haSelectedState.reopened.Owners.Components.Ledger.ReadFinalization(ctx, "actual-default-ha-request"); err != nil || !found {
		t.Fatal("actual default Replica applied query", found, err, primary.replication.publisher.Snapshot(), replica.replication.publisher.Snapshot(), head)
	}
	if err := primary.haSelectedState.businessHealthError(ctx); err != nil {
		t.Fatal("actual healthy selected owner", err)
	}
	cancelledHealth, cancelHealth := context.WithCancel(ctx)
	cancelHealth()
	if err := primary.haSelectedState.businessHealthError(cancelledHealth); err == nil {
		t.Fatal("cancelled health observation accepted")
	}
	// Terminal retirement still needs current physical observations after all
	// business goroutines join. The actual foundation must own the sampler.
	primary.stopBackgroundWork()
	samples := primary.resourcePressure.monitor.Snapshot().Samples
	for primary.resourcePressure.monitor.Snapshot().Samples == samples {
		select {
		case <-ctx.Done():
			t.Fatal("business drain stopped the physical sampler", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !primary.resourcePressure.monitor.Snapshot().Running {
		t.Fatal("sampler stopped before actual foundation retirement")
	}
	t.Cleanup(func() {
		if err := primary.Close(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-primary.haSelectedState.pressureDone:
		default:
			t.Error("actual foundation Close did not join its sampler")
		}
		if primary.resourcePressure.monitor.Snapshot().Running {
			t.Error("retired foundation kept sampling")
		}
	})
	// An existing Ready flag cannot retain service after an original source
	// withdraws its accounting grant. This check captures no fresh common cut.
	primary.status.RequireRecovery()
	withdrawn := httptest.NewRecorder()
	primary.ready(withdrawn, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if withdrawn.Code != http.StatusServiceUnavailable {
		t.Fatal("actual HA source fault retained Ready", withdrawn.Code, withdrawn.Body.String())
	}
}

// This adapter supplies no state or replication authority. Every acceptance,
// price pin, attempt and terminal fact uses the production selected owners.
type haSelectedHTTPAdapter struct {
	*contentCanaryAdapter
	calls        atomic.Int64
	beforeReturn func(context.Context) error
}

func (a *haSelectedHTTPAdapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	a.calls.Add(1)
	if err := a.beforeReturn(ctx); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	return a.contentCanaryAdapter.Chat(ctx, call)
}

func proveDefaultHAAcceptedHTTPCompletion(t *testing.T, ctx context.Context, primary, replica *Runtime, high *atomic.Bool) {
	t.Helper()
	projects, err := primary.store.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatal("actual selected gateway project", projects, err)
	}
	plain, key, err := auth.GenerateGatewayKey(projects[0].ID, "actual-ha-http", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.store.PutGatewayKey(ctx, key, 0, nil); err != nil {
		t.Fatal("actual replicated gateway key", err)
	}
	if err := primary.auth.Refresh(ctx, primary.store); err != nil {
		t.Fatal(err)
	}
	routes, err := primary.store.ListRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatal(routes, err)
	}
	route := routes[0]
	deployment, err := primary.store.GetDeployment(ctx, route.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	a := &haSelectedHTTPAdapter{contentCanaryAdapter: &contentCanaryAdapter{}}
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	bridge, err := provider.NewLegacyAdapterBridge(a, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: route.ID, DeploymentID: deployment.ID, ProviderID: deployment.ProviderID, PublicModel: route.PublicModel, ProviderModel: deployment.ProviderModel, Adapter: bridge, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
		t.Fatal(err)
	}
	for _, old := range primary.providers.Replace(registry) {
		old.Close()
	}
	for _, canceled := range []bool{false, true} {
		id := "actual-ha-http-success"
		if canceled {
			id = "actual-ha-http-cancel"
		}
		reservedBefore := primary.resourcePressure.budget.Snapshot().CompletionReserved
		callCtx, cancel := context.WithCancel(requestmeta.WithRequestID(ctx, id))
		a.beforeReturn = func(context.Context) error {
			owned := primary.resourcePressure.budget.Snapshot()
			if owned.CompletionReserved.RAM != reservedBefore.RAM+primary.resourcePressure.completionRAMEnvelope() || owned.CompletionReserved.Disk != reservedBefore.Disk+primary.resourcePressure.completionDiskEnvelope() {
				err := fmt.Errorf("actual HTTP provider lacks its pre-admitted terminal resources: CompletionReserved=%+v wantRAM=%d wantDisk=%d Used=%+v Classes=%+v", owned.CompletionReserved, primary.resourcePressure.completionRAMEnvelope(), primary.resourcePressure.completionDiskEnvelope(), owned.Used, owned.Classes)
				t.Log(err)
				return err
			}
			high.Store(true)
			waitRuntimePressure(t, primary.resourcePressure, resourcebudget.Pressured)
			if canceled {
				cancel()
			}
			return nil
		}
		body := `{"model":"` + route.PublicModel + `","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)).WithContext(callCtx)
		req.Header.Set("Authorization", "Bearer "+plain)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		primary.gatewayRouter().ServeHTTP(response, req)
		// The actual facade mints its own request identity; the caller context
		// is not a durable identity grant.
		id = response.Header().Get("X-Request-ID")
		if id == "" {
			t.Fatal("actual HTTP did not return its request identity")
		}
		cancel()
		if !canceled && response.Code != http.StatusOK {
			t.Fatal("accepted actual HA HTTP failed terminal continuation", response.Code, response.Body.String())
		}
		refused := httptest.NewRecorder()
		primary.gatewayRouter().ServeHTTP(refused, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)))
		if refused.Code != http.StatusServiceUnavailable {
			t.Fatal("new actual HA work admitted during pressure", refused.Code, refused.Body.String())
		}
		if got := primary.resourcePressure.budget.Snapshot(); got.CompletionReserved != reservedBefore {
			t.Fatal("actual HTTP terminal resources not joined", got)
		}
		high.Store(false)
		waitRuntimePressure(t, primary.resourcePressure, resourcebudget.Healthy)
		p := primary.haSelectedState.reopened.Owners.Components.Ledger
		final, found, err := p.ReadFinalization(ctx, id)
		if err != nil || !found || final.Event.Outcome == "" {
			t.Fatal("actual accepted HTTP left terminal debt", id, found, final, err)
		}
		head, _, err := p.ReadPosition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target, err := primary.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.replication.coordinator.WaitRecoveryTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
		if _, found, err := replica.haSelectedState.reopened.Owners.Components.Ledger.ReadFinalization(ctx, id); err != nil || !found {
			t.Fatal("actual HTTP terminal was not Replica applied", id, found, err)
		}
	}
	if a.calls.Load() != 2 {
		t.Fatal("actual mock provider invocation count", a.calls.Load())
	}
}
