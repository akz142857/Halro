package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type selectedCompletionAdapter struct {
	*contentCanaryAdapter
	beforeReturn func() error
}

func (a *selectedCompletionAdapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	if err := a.beforeReturn(); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	return a.contentCanaryAdapter.Chat(ctx, call)
}

func assertSelectedRuntimeHTTPCompletion(t *testing.T, r *Runtime, credentials BootstrapResult, sampledPressure *atomic.Bool) {
	t.Helper()
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		requestID := fmt.Sprintf("req_selected_http_%t", canceled)
		adapter := &selectedCompletionAdapter{contentCanaryAdapter: &contentCanaryAdapter{}, beforeReturn: func() error {
			if got := r.resourcePressure.budget.Snapshot(); got.CompletionReserved.RAM != r.resourcePressure.completionRAMEnvelope() {
				return fmt.Errorf("actual HTTP did not reserve terminal work before provider: %+v", got)
			}
			sampledPressure.Store(true)
			deadline := time.Now().Add(2 * time.Second)
			for r.resourcePressure.monitor.Snapshot().State != resourcebudget.Pressured {
				if time.Now().After(deadline) {
					return fmt.Errorf("actual monitor did not enter pressure")
				}
				time.Sleep(time.Millisecond)
			}
			if canceled {
				cancel()
			}
			return nil
		}}
		profiled, err := provider.NewLegacyAdapterBridge(adapter, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
		if err != nil {
			t.Fatal(err)
		}
		registry := provider.NewRegistry()
		if err := registry.Register(provider.Target{ID: credentials.RouteID, DeploymentID: credentials.DeploymentID, ProviderID: credentials.ProviderID,
			PublicModel: "chat", ProviderModel: "canary-model", Adapter: profiled, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
			t.Fatal(err)
		}
		for _, retired := range r.providers.Replace(registry) {
			retired.Close()
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
		req = req.WithContext(requestmeta.WithRequestID(ctx, requestID))
		req.Header.Set("Authorization", "Bearer "+credentials.GatewayKey)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		before, _, _ := r.ledger.ChainHead()
		r.gatewayRouter().ServeHTTP(response, req)
		requestID = response.Header().Get("X-Request-ID")
		cancel()
		after, _, _ := r.ledger.ChainHead()
		if !canceled && response.Code != http.StatusOK {
			t.Fatal("actual selected HTTP completion failed", response.Code, response.Body.String())
		}
		if after.Sequence != before.Sequence+5 || r.status.Load() != ledger.AccountingHealthy || r.resourcePressure.monitor.Snapshot().State != resourcebudget.Pressured {
			t.Fatal("actual selected native lifecycle did not finish under monitored pressure", before, after, r.status.Load())
		}
		if got := r.resourcePressure.budget.Snapshot(); got.CompletionReserved != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Completion] != (resourcebudget.Cost{}) {
			t.Fatal("HTTP returned before actual completion credit retirement", got)
		}
		sampledPressure.Store(false)
		waitRuntimePressure(t, r.resourcePressure, resourcebudget.Healthy)
		request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(context.Background(), requestID)
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("selected COW root lost actual terminal fact", request, found, err)
		}
	}
}
