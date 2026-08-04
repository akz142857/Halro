package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminDeploymentPriceLifecycleUsesVersionedTimelineAndAuditIntent(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
		ProjectName: "Pricing", InputMicrosPerMillion: 0, OutputMicrosPerMillion: 0,
		BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	effective := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	body := map[string]any{
		"billing_mode": "metered", "currency": "USD",
		"current_password":      "correct horse battery staple",
		"input_usd_per_million": "0.40", "output_usd_per_million": "1.60", "fixed_request_usd": "0",
		"effective_from": effective.Format(time.RFC3339),
		"source": map[string]any{
			"type": "official_url", "uri": "https://example.test/pricing",
			"retrieved_at":   time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
			"content_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"reference":      "standard tier",
		},
	}
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "create-price-1")
	createdResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createdResponse, request)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create price status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created domain.DeploymentPriceVersion
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Version != 2 || created.InputMicrosPerMillion != 400_000 || created.OutputMicrosPerMillion != 1_600_000 {
		t.Fatalf("created price=%#v", created)
	}
	retryRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", body)
	retryRequest.AddCookie(cookie)
	retryRequest.Header.Set("X-CSRF-Token", csrf)
	retryRequest.Header.Set("Idempotency-Key", "create-price-1")
	retryResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(retryResponse, retryRequest)
	var replayed domain.DeploymentPriceVersion
	if err := json.Unmarshal(retryResponse.Body.Bytes(), &replayed); err != nil || retryResponse.Code != http.StatusCreated ||
		retryResponse.Header().Get("Idempotent-Replayed") != "true" || replayed.ID != created.ID {
		t.Fatalf("idempotent replay status=%d headers=%v body=%s err=%v", retryResponse.Code, retryResponse.Header(), retryResponse.Body.String(), err)
	}
	conflictBody := map[string]any{}
	for key, value := range body {
		conflictBody[key] = value
	}
	conflictBody["output_usd_per_million"] = "2.00"
	conflictRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", conflictBody)
	conflictRequest.AddCookie(cookie)
	conflictRequest.Header.Set("X-CSRF-Token", csrf)
	conflictRequest.Header.Set("Idempotency-Key", "create-price-1")
	conflictResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(conflictResponse, conflictRequest)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
	pending, err := runtime.store.ListPendingPricingAuditIntents(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pricing audit was not delivered: %#v err=%v", pending, err)
	}

	listed := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices")
	if listed.Code != http.StatusOK || !json.Valid(listed.Body.Bytes()) {
		t.Fatalf("list prices status=%d body=%s", listed.Code, listed.Body.String())
	}

	previewBody := body
	previewBody["input_tokens"], previewBody["output_tokens"] = 1, 1
	preview := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices/preview", "", previewBody)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewResult struct {
		Cost domain.PriceCostBreakdown `json:"cost"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewResult); err != nil || previewResult.Cost.TotalCostMicrosUSD != 3 {
		t.Fatalf("preview=%s err=%v", preview.Body.String(), err)
	}

	quarantined, err := runtime.store.QuarantineRestoredScheduledPrices(context.Background(), effective.Add(-time.Minute), effective.Add(time.Minute))
	if err != nil || quarantined != 1 {
		t.Fatalf("restore quarantine count=%d err=%v", quarantined, err)
	}
	withoutReauth := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices/restore-confirm", "", map[string]any{})
	if withoutReauth.Code != http.StatusUnauthorized {
		t.Fatalf("restore confirmation without reauth status=%d body=%s", withoutReauth.Code, withoutReauth.Body.String())
	}
	confirmed := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices/restore-confirm", "", map[string]any{
			"current_password": "correct horse battery staple",
		})
	if confirmed.Code != http.StatusOK {
		t.Fatalf("restore confirmation status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	if err := runtime.store.PricingReadiness(context.Background()); err != nil {
		t.Fatalf("restore confirmation did not clear quarantine: %v", err)
	}

	cancelled := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices/"+created.ID+"/cancel", `"1"`, nil)
	if cancelled.Code != http.StatusOK || cancelled.Header().Get("ETag") != `"2"` {
		t.Fatalf("cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
}

func TestAdminFreePriceRequiresReauthentication(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/missing/prices", map[string]any{
		"billing_mode": "free", "currency": "USD", "input_usd_per_million": "0", "output_usd_per_million": "0", "fixed_request_usd": "0",
		"effective_from": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"source":         map[string]any{"type": "manual", "reference": "free tier", "asserted_without_archive": true},
	})
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "free-price-1")
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("free price without reauth status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminPriceProposalNeverActivatesUntilReviewedAndAdopted(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Proposal", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	now := time.Now().UTC()
	body := map[string]any{
		"billing_mode": "metered", "currency": "USD", "input_usd_per_million": "0.50",
		"output_usd_per_million": "2.00", "fixed_request_usd": "0", "match": "exact",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339), "warnings": []string{"LLM-extracted; verify source before adoption"},
		"source": map[string]any{
			"type": "official_url", "uri": "https://example.test/pricing", "retrieved_at": now.Format(time.RFC3339),
			"content_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "reference": "public standard tier",
		},
	}
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/price-proposals", body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "proposal-1")
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create proposal status=%d body=%s", response.Code, response.Body.String())
	}
	retry := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/price-proposals", body)
	retry.AddCookie(cookie)
	retry.Header.Set("X-CSRF-Token", csrf)
	retry.Header.Set("Idempotency-Key", "proposal-1")
	retryResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusCreated || retryResponse.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("proposal replay status=%d headers=%v body=%s", retryResponse.Code, retryResponse.Header(), retryResponse.Body.String())
	}
	forged := map[string]any{}
	for key, value := range body {
		forged[key] = value
	}
	forged["source"] = map[string]any{"type": "provider_api", "adapter": "client-claimed", "provider_request_id": "req_forged", "content_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	forgedRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/price-proposals", forged)
	forgedRequest.AddCookie(cookie)
	forgedRequest.Header.Set("X-CSRF-Token", csrf)
	forgedRequest.Header.Set("Idempotency-Key", "proposal-forged")
	forgedResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(forgedResponse, forgedRequest)
	if forgedResponse.Code == http.StatusCreated {
		t.Fatalf("client forged verified_api assurance: %s", forgedResponse.Body.String())
	}
	var proposal domain.DeploymentPriceProposal
	if err := json.Unmarshal(response.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	prices, err := runtime.store.ListDeploymentPriceVersions(context.Background(), bootstrap.DeploymentID)
	if err != nil || len(prices) != 1 {
		t.Fatalf("proposal changed production timeline: prices=%#v err=%v", prices, err)
	}

	adopted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/price-proposals/"+proposal.ID+"/adopt", `"1"`, map[string]any{
			"effective_from": now.Add(2 * time.Hour).Format(time.RFC3339), "confirm": true,
			"current_password": "correct horse battery staple",
		})
	if adopted.Code != http.StatusCreated {
		t.Fatalf("adopt proposal status=%d body=%s", adopted.Code, adopted.Body.String())
	}
	prices, err = runtime.store.ListDeploymentPriceVersions(context.Background(), bootstrap.DeploymentID)
	if err != nil || len(prices) != 2 || prices[1].InputMicrosPerMillion != 500_000 || prices[1].OutputMicrosPerMillion != 2_000_000 {
		t.Fatalf("adopted prices=%#v err=%v", prices, err)
	}
	pending, err := runtime.store.ListPendingPricingAuditIntents(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("proposal audit intents=%#v err=%v", pending, err)
	}
}
