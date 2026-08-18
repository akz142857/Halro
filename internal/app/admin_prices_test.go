package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
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
		"input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0",
		"effective_from": effective.Format(time.RFC3339),
		"source": map[string]any{
			"type": "manual", "reference": "official_public_price", "asserted_without_archive": true,
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
	if created.Source.ContentSHA256 != "" || !created.Source.AssertedWithoutArchive {
		t.Fatalf("manual source was not stored without an invented digest: %#v", created.Source)
	}
	if _, err := domain.NewVersionedPriceSnapshot(created, effective.Add(time.Second)); err != nil {
		t.Fatalf("manual price could not cross the PriceVersion to Snapshot boundary: %v", err)
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
	auditResponse := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	for _, evidence := range [][]byte{[]byte(`"request_sha256"`), []byte(`"source_assurance":"asserted"`), []byte(`"source_reference":"official_public_price"`), []byte(`"source_without_archive":true`)} {
		if !bytes.Contains(auditResponse.Body.Bytes(), evidence) {
			t.Fatalf("audit response omitted manual source evidence %s: %s", evidence, auditResponse.Body.String())
		}
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

func TestImmediateFreePriceUsesServerTimeAndIsImmediatelySelectable(t *testing.T) {
	now := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	price, err := priceVersionFromInput("price_immediate", "deployment_immediate", "admin", now, createPriceInput{
		BillingMode: domain.BillingModeFree, Currency: "USD",
		InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", FixedRequestUSD: "0",
		EffectiveImmediately: true,
		Source:               priceSourceInput{Type: domain.PriceSourceManual, Reference: "temporary_estimate", AssertedWithoutArchive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !price.EffectiveFrom.Equal(now) {
		t.Fatalf("immediate effective_from=%s want server now=%s", price.EffectiveFrom, now)
	}
	price.Version, price.Revision = 1, 1
	selected, err := domain.SelectDeploymentPriceVersion([]domain.DeploymentPriceVersion{price}, price.DeploymentID, now)
	if err != nil {
		t.Fatalf("immediate free price was not selectable: %v", err)
	}
	if selected.ID != price.ID || selected.BillingMode != domain.BillingModeFree {
		t.Fatalf("selected price=%#v", selected)
	}
}

func TestAdminImmediateFreePriceIsActiveAndIdempotent(t *testing.T) {
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
		ProjectName: "Immediate pricing", BillingMode: domain.BillingModeFree,
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
	body := map[string]any{
		"billing_mode": "free", "currency": "USD",
		"input_usd_per_million": "0", "cached_input_usd_per_million": "0", "output_usd_per_million": "0", "fixed_request_usd": "0",
		"effective_immediately": true, "current_password": "correct horse battery staple",
		"source": map[string]any{"type": "manual", "reference": "temporary_estimate", "asserted_without_archive": true},
	}
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "immediate-free-price")
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create immediate price status=%d body=%s", response.Code, response.Body.String())
	}
	var created domain.DeploymentPriceVersion
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	selected, err := runtime.store.SelectDeploymentPriceVersion(context.Background(), bootstrap.DeploymentID, time.Now().UTC())
	if err != nil || selected.ID != created.ID || selected.BillingMode != domain.BillingModeFree {
		t.Fatalf("immediate price was not active: selected=%#v created=%#v err=%v", selected, created, err)
	}

	retry := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", body)
	retry.AddCookie(cookie)
	retry.Header.Set("X-CSRF-Token", csrf)
	retry.Header.Set("Idempotency-Key", "immediate-free-price")
	retryResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusCreated || retryResponse.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("immediate replay status=%d headers=%v body=%s", retryResponse.Code, retryResponse.Header(), retryResponse.Body.String())
	}
}

// openPricingRuntimeForTest is the smallest instance that can price a
// deployment: one provider, one deployment, one administrator session.
func openPricingRuntimeForTest(t *testing.T, project string) (*Runtime, BootstrapResult, *http.Cookie, string) {
	t.Helper()
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
		ProjectName: project, BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cookie, csrf := loginAdminForTest(t, runtime)
	return runtime, bootstrap, cookie, csrf
}

func createPriceForTest(t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf, deploymentID, idempotencyKey string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+deploymentID+"/prices", body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}

func auditRecordsForAction(t *testing.T, runtime *Runtime, cookie *http.Cookie, action string) []map[string]any {
	t.Helper()
	var matches []map[string]any
	for _, record := range auditActions(t, runtime, cookie) {
		if record["action"] == action {
			matches = append(matches, record)
		}
	}
	return matches
}

func responseErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not a JSON error: %s", response.Body.String())
	}
	return body.Code
}

// The audit record is the only durable statement of where a price came from,
// and it is append-only. A creation path that reports no assurance, no
// reference and "the operator did not assert an unarchived source" is not
// merely incomplete: the last of those is a false statement about a submission
// that said the opposite. Whichever way a price takes effect, the evidence has
// to be the same, because it is the same submitted evidence.
func TestPriceCreationAuditCarriesTheSameSourceEvidenceWhicheverWayItTakesEffect(t *testing.T) {
	runtime, bootstrap, cookie, csrf := openPricingRuntimeForTest(t, "Pricing evidence")
	source := map[string]any{
		"type": "manual", "reference": "official_public_price", "asserted_without_archive": true,
	}
	price := func(extra map[string]any) map[string]any {
		body := map[string]any{
			"billing_mode": "metered", "currency": "USD", "current_password": "correct horse battery staple",
			"input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0",
			"source": source,
		}
		for key, value := range extra {
			body[key] = value
		}
		return body
	}
	immediate := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, "evidence-immediate",
		price(map[string]any{"effective_immediately": true}))
	if immediate.Code != http.StatusCreated {
		t.Fatalf("immediate create status=%d body=%s", immediate.Code, immediate.Body.String())
	}
	scheduled := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, "evidence-scheduled",
		price(map[string]any{"effective_from": time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)}))
	if scheduled.Code != http.StatusCreated {
		t.Fatalf("scheduled create status=%d body=%s", scheduled.Code, scheduled.Body.String())
	}
	records := auditRecordsForAction(t, runtime, cookie, "deployment_price.create")
	if len(records) != 2 {
		t.Fatalf("want one audit record per creation path, got %d: %#v", len(records), records)
	}
	var evidence []map[string]any
	for _, record := range records {
		metadata, ok := record["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("audit record carried no metadata: %#v", record)
		}
		if metadata["source_assurance"] != "asserted" || metadata["source_type"] != "manual" ||
			metadata["source_reference"] != "official_public_price" || metadata["source_without_archive"] != true {
			t.Fatalf("audit record lost the submitted source evidence: %#v", metadata)
		}
		evidence = append(evidence, map[string]any{
			"source_assurance": metadata["source_assurance"], "source_reference": metadata["source_reference"],
			"source_without_archive": metadata["source_without_archive"],
		})
	}
	if fmt.Sprintf("%v", evidence[0]) != fmt.Sprintf("%v", evidence[1]) {
		t.Fatalf("the two creation paths recorded different evidence for the same source: %#v", evidence)
	}
}

// effective_from and effective_immediately are two ways to say when a price
// starts applying, and exactly one of them must be given. Both together is
// ambiguous and neither leaves the price with no start at all.
func TestPriceCreationRequiresExactlyOneWayOfTakingEffect(t *testing.T) {
	runtime, bootstrap, cookie, csrf := openPricingRuntimeForTest(t, "Pricing effect")
	for _, testCase := range []struct {
		name   string
		when   map[string]any
		status int
		code   string
	}{
		{
			name:   "both",
			when:   map[string]any{"effective_from": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "effective_immediately": true},
			status: http.StatusBadRequest, code: "price_effective_from_conflict",
		},
		{
			name: "neither", when: map[string]any{},
			status: http.StatusBadRequest, code: "price_effective_from_required",
		},
		{
			name: "immediate only", when: map[string]any{"effective_immediately": true},
			status: http.StatusCreated, code: "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := map[string]any{
				"billing_mode": "metered", "currency": "USD", "current_password": "correct horse battery staple",
				"input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0",
				"source": map[string]any{"type": "manual", "reference": "official_public_price", "asserted_without_archive": true},
			}
			for key, value := range testCase.when {
				body[key] = value
			}
			before := time.Now().UTC()
			response := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, "effect-"+testCase.name, body)
			after := time.Now().UTC()
			if response.Code != testCase.status {
				t.Fatalf("status=%d want %d body=%s", response.Code, testCase.status, response.Body.String())
			}
			if testCase.code != "" {
				if code := responseErrorCode(t, response); code != testCase.code {
					t.Fatalf("error code=%q want %q body=%s", code, testCase.code, response.Body.String())
				}
				return
			}
			var created domain.DeploymentPriceVersion
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			// The server picks the instant, so the assertion is the interval the
			// request was in flight for and not an equality against a clock read
			// the handler never saw.
			if created.EffectiveFrom.Before(before) || created.EffectiveFrom.After(after) {
				t.Fatalf("immediate effective_from=%s outside the request window [%s, %s]", created.EffectiveFrom, before, after)
			}
		})
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
		"billing_mode": "free", "currency": "USD", "input_usd_per_million": "0", "cached_input_usd_per_million": "0", "output_usd_per_million": "0", "fixed_request_usd": "0",
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
		"billing_mode": "metered", "currency": "USD", "input_usd_per_million": "0.50", "cached_input_usd_per_million": "0.05",
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

// Adoption is the second way a price version comes into existence, so it takes
// effect on the same terms direct creation does. An adoption that could only
// schedule would leave the operator creating the price by hand to get it live
// now, discarding the reviewed proposal's provenance in the process.
func TestAdoptingAPriceProposalTakesEffectOnTheSameTermsAsCreatingOne(t *testing.T) {
	runtime, bootstrap, cookie, csrf := openPricingRuntimeForTest(t, "Proposal adoption")
	now := time.Now().UTC()
	proposalRequest := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/price-proposals", map[string]any{
		"billing_mode": "metered", "currency": "USD", "input_usd_per_million": "0.50", "cached_input_usd_per_million": "0.05",
		"output_usd_per_million": "2.00", "fixed_request_usd": "0", "match": "exact",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339),
		"source": map[string]any{
			"type": "official_url", "uri": "https://example.test/pricing", "retrieved_at": now.Format(time.RFC3339),
			"content_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"reference":      "public standard tier",
		},
	})
	proposalRequest.AddCookie(cookie)
	proposalRequest.Header.Set("X-CSRF-Token", csrf)
	proposalRequest.Header.Set("Idempotency-Key", "adoption-proposal")
	proposalResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(proposalResponse, proposalRequest)
	if proposalResponse.Code != http.StatusCreated {
		t.Fatalf("create proposal status=%d body=%s", proposalResponse.Code, proposalResponse.Body.String())
	}
	var proposal domain.DeploymentPriceProposal
	if err := json.Unmarshal(proposalResponse.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	adopt := "/admin/api/v1/deployments/" + bootstrap.DeploymentID + "/price-proposals/" + proposal.ID + "/adopt"

	ambiguous := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, adopt, `"1"`, map[string]any{
		"effective_from": now.Add(2 * time.Hour).Format(time.RFC3339), "effective_immediately": true,
		"confirm": true, "current_password": "correct horse battery staple",
	})
	if ambiguous.Code != http.StatusBadRequest || responseErrorCode(t, ambiguous) != "price_effective_from_conflict" {
		t.Fatalf("ambiguous adoption status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}

	before := time.Now().UTC()
	adopted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, adopt, `"1"`, map[string]any{
		"effective_immediately": true, "confirm": true, "current_password": "correct horse battery staple",
	})
	after := time.Now().UTC()
	if adopted.Code != http.StatusCreated {
		t.Fatalf("immediate adoption status=%d body=%s", adopted.Code, adopted.Body.String())
	}
	var result struct {
		PriceVersion domain.DeploymentPriceVersion `json:"price_version"`
	}
	if err := json.Unmarshal(adopted.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.PriceVersion.EffectiveFrom.Before(before) || result.PriceVersion.EffectiveFrom.After(after) {
		t.Fatalf("adopted effective_from=%s outside the request window [%s, %s]", result.PriceVersion.EffectiveFrom, before, after)
	}
	selected, err := runtime.store.SelectDeploymentPriceVersion(context.Background(), bootstrap.DeploymentID, time.Now().UTC())
	if err != nil || selected.ID != result.PriceVersion.ID {
		t.Fatalf("adopted price was not active: selected=%#v adopted=%#v err=%v", selected, result.PriceVersion, err)
	}
	records := auditRecordsForAction(t, runtime, cookie, "deployment_price.proposal_adopt")
	if len(records) != 1 {
		t.Fatalf("adoption audit records=%#v", records)
	}
	metadata, ok := records[0]["metadata"].(map[string]any)
	if !ok || metadata["source_assurance"] != "asserted" || metadata["source_type"] != "official_url" ||
		metadata["source_reference"] != "public standard tier" {
		t.Fatalf("adoption audit lost the proposal's source evidence: %#v", records[0])
	}
}

// A price that bills by time of day has to survive the whole Admin path — the
// operator's clock strings in, a stored rule table out — and the preview has to
// answer for a named instant rather than for "now", because with a schedule
// there is no single cost.
func TestScheduledPriceRoundTripsThroughTheAdminAPIAndPricesEveryTier(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "DeepSeek", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.deepseek.com", ProviderModel: "deepseek-chat", PublicModel: "chat",
		ProjectName: "Pricing", BillingMode: domain.BillingModeFree,
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
	// Off-peak is the version's own rate and peak is the window, matching how
	// the provider publishes it: a discount away from the standard price.
	body := map[string]any{
		"billing_mode": "metered", "currency": "USD",
		"current_password":      "correct horse battery staple",
		"input_usd_per_million": "0.20", "cached_input_usd_per_million": "0.02", "output_usd_per_million": "0.80", "fixed_request_usd": "0",
		"schedule": map[string]any{
			"timezone": "Asia/Shanghai",
			"windows": []map[string]any{
				{"start": "09:00", "end": "12:00", "input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0"},
				{"start": "14:00", "end": "18:00", "input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0"},
			},
		},
		"effective_from": effective.Format(time.RFC3339),
		"source":         map[string]any{"type": "manual", "reference": "official_public_price", "asserted_without_archive": true},
	}
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices", body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "create-scheduled-price-1")
	created := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create scheduled price status=%d body=%s", created.Code, created.Body.String())
	}
	var stored domain.DeploymentPriceVersion
	if err := json.Unmarshal(created.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Schedule == nil || stored.Schedule.Timezone != "Asia/Shanghai" || len(stored.Schedule.Windows) != 2 {
		t.Fatalf("stored schedule=%#v", stored.Schedule)
	}
	if stored.Schedule.Windows[0].StartMinute != 9*60 || stored.Schedule.Windows[1].EndMinute != 18*60 {
		t.Fatalf("clock strings did not become minutes: %#v", stored.Schedule.Windows)
	}
	// The audit trail carries the rule, not just the fact that one exists.
	auditResponse := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	if !bytes.Contains(auditResponse.Body.Bytes(), []byte("Asia/Shanghai")) || !bytes.Contains(auditResponse.Body.Bytes(), []byte("540-720")) {
		t.Fatalf("audit omitted the schedule: %s", auditResponse.Body.String())
	}

	previewAt := func(instant string) map[string]any {
		previewBody := map[string]any{}
		for key, value := range body {
			previewBody[key] = value
		}
		previewBody["input_tokens"], previewBody["output_tokens"], previewBody["at"] = 1_000_000, 0, instant
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
			"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices/preview", "", previewBody)
		if response.Code != http.StatusOK {
			t.Fatalf("preview status=%d body=%s", response.Code, response.Body.String())
		}
		var result struct {
			Cost  domain.PriceCostBreakdown `json:"cost"`
			Tiers []struct {
				Source   string                    `json:"source"`
				Start    string                    `json:"start"`
				End      string                    `json:"end"`
				Selected bool                      `json:"selected"`
				Cost     domain.PriceCostBreakdown `json:"cost"`
			} `json:"tiers"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		selected := ""
		for _, tier := range result.Tiers {
			if tier.Selected {
				selected = tier.Source + tier.Start
			}
		}
		return map[string]any{"cost": result.Cost.TotalCostMicrosUSD, "tiers": len(result.Tiers), "selected": selected}
	}
	// 02:00Z is 10:00 in Shanghai and inside the morning peak.
	peak := previewAt("2026-08-18T02:00:00Z")
	if peak["cost"] != int64(400_000) || peak["selected"] != "window09:00" || peak["tiers"] != 3 {
		t.Fatalf("peak preview=%#v", peak)
	}
	// 05:00Z is 13:00, the gap between the two peaks, billed at the base rate.
	gap := previewAt("2026-08-18T05:00:00Z")
	if gap["cost"] != int64(200_000) || gap["selected"] != "base" {
		t.Fatalf("gap preview=%#v", gap)
	}
}
