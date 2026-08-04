package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
)

func TestAdminCostAdjustmentPreviewCreateAndIdempotentReplay(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Adjustments", BillingMode: domain.BillingModeFree}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	now := time.Now().UTC()
	price := domain.DeploymentPriceVersion{ID: "price_adjust_test", DeploymentID: bootstrap.DeploymentID, Version: 1, Revision: 1, BillingMode: domain.BillingModeFree,
		Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1, EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: now.Add(-time.Hour), Reference: "free", AssertedWithoutArchive: true,
			ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	snapshot, err := domain.NewVersionedPriceSnapshot(price, now)
	if err != nil {
		t.Fatal(err)
	}
	request, err := runtime.accounting.BeginRequest(context.Background(), bootstrap.ProjectID, "request_adjust_api")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveLeaseDetailed(context.Background(), request, 50_000_000, budget.LeaseSpec{Mode: ledger.LeaseModeFree,
		PriceSnapshot: &snapshot, TokenGuardPricingViewDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, budget.AttemptMetadata{DeploymentID: bootstrap.DeploymentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(context.Background(), attempt, budget.Settlement{Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdminForTest(t, runtime)
	body := map[string]any{"mode": "explicit_delta", "explicit_delta_micros_usd": 7, "expected_sequence": 0, "expected_net_cost_micros_usd": 0,
		"reason_code": "invoice_difference", "reason": "late provider invoice", "evidence_digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	preview := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/usage/attempts/"+attempt.AttemptID+"/cost-adjustments/preview", "", body)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	body["confirm"], body["current_password"] = true, "correct horse battery staple"
	// performAdminMutation does not set the operation-specific header; send the
	// authoritative request explicitly.
	createRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/usage/attempts/"+attempt.AttemptID+"/cost-adjustments", body)
	createRequest.AddCookie(cookie)
	createRequest.Header.Set("X-CSRF-Token", csrf)
	createRequest.Header.Set("Idempotency-Key", "adjust-1")
	created := performRequest(runtime, createRequest)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	retryRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/usage/attempts/"+attempt.AttemptID+"/cost-adjustments", body)
	retryRequest.AddCookie(cookie)
	retryRequest.Header.Set("X-CSRF-Token", csrf)
	retryRequest.Header.Set("Idempotency-Key", "adjust-1")
	retry := performRequest(runtime, retryRequest)
	if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry status=%d headers=%v body=%s", retry.Code, retry.Header(), retry.Body.String())
	}
	balance := runtime.state.Balance(bootstrap.ProjectID, request.PeriodID)
	if balance.OriginalCommittedMicrosUSD != 0 || balance.AdjustmentDeltaMicrosUSD != 7 || balance.CommittedMicrosUSD != 7 {
		t.Fatalf("balance=%#v", balance)
	}
	pending, err := runtime.store.ListPendingCostAdjustmentIntents(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func performRequest(runtime *Runtime, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}
