package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestAdminPriceReplaysLegacyImmediateScheduledPrice(t *testing.T) {
	runtime, bootstrap, cookie, csrf := openPricingRuntimeForTest(t, "Legacy price retry")
	ctx := context.Background()
	now := time.Now().UTC()
	key := "legacy-price"
	digest := pricingIdempotencyDigest("admin", bootstrap.DeploymentID, key)
	price := domain.DeploymentPriceVersion{
		ID: deterministicMutationID("price", digest), DeploymentID: bootstrap.DeploymentID,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 400_000, CachedInputMicrosPerMillion: 40_000,
		CacheWriteMicrosPerMillion: 400_000, CacheWriteLongMicrosPerMillion: 400_000,
		OutputMicrosPerMillion: 1_600_000,
		Schedule: &domain.PriceSchedule{Timezone: "Etc/UTC", Windows: []domain.PriceWindow{{
			StartMinute: 540, EndMinute: 720, InputMicrosPerMillion: 600_000, CachedInputMicrosPerMillion: 50_000,
			CacheWriteMicrosPerMillion: 600_000, CacheWriteLongMicrosPerMillion: 600_000, OutputMicrosPerMillion: 2_000_000,
		}}},
		EffectiveFrom: now, CreatedBy: "admin", CreatedAt: now,
		Source: domain.PriceSource{
			Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, Reference: "Legacy quote",
			AssertedWithoutArchive: true, ReceivedAt: now,
		},
	}
	// Snapshot of the pre-upgrade canonical JSON, independently specified with
	// its original key order. In particular, immediate writes zero the time.
	legacyPayload := json.RawMessage(fmt.Sprintf(`{
		"price": {
			"id": "", "deployment_id": %q, "version": 0, "billing_mode": "metered",
			"currency": "USD", "formula_version": "usd_token_v1",
			"input_micros_per_million": 400000, "cached_input_micros_per_million": 40000,
			"output_micros_per_million": 1600000, "fixed_request_micros_usd": 0,
			"schedule": {"timezone": "Etc/UTC", "windows": [{
				"start_minute": 540, "end_minute": 720, "input_micros_per_million": 600000,
				"cached_input_micros_per_million": 50000, "output_micros_per_million": 2000000,
				"fixed_request_micros_usd": 0
			}]},
			"effective_from": "0001-01-01T00:00:00Z",
			"source": {"type": "manual", "assurance": "asserted", "received_at": "0001-01-01T00:00:00Z",
				"reference": "Legacy quote", "asserted_without_archive": true},
			"created_by": "", "created_at": "0001-01-01T00:00:00Z", "revision": 0
		},
		"effective_immediately": true
	}`, bootstrap.DeploymentID))
	intent, err := newPricingAuditIntent("deployment_price.create", price.ID, "admin", "", legacyPayload, &price.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runtime.store.CreateDeploymentPriceVersionIdempotent(ctx, price, intent, digest); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"billing_mode": "metered", "currency": "USD", "effective_immediately": true, "current_password": stepUpTestPassword,
		"input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.6", "fixed_request_usd": "0",
		"source": map[string]any{"type": "manual", "reference": "Legacy quote", "asserted_without_archive": true},
		"schedule": map[string]any{"timezone": "Etc/UTC", "windows": []map[string]any{{
			"start": "09:00", "end": "12:00", "input_usd_per_million": "0.60", "cached_input_usd_per_million": "0.05",
			"output_usd_per_million": "2", "fixed_request_usd": "0",
		}}},
	}
	replayed := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, key, body)
	if replayed.Code != http.StatusCreated || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("legacy retry did not replay: %d %s", replayed.Code, replayed.Body)
	}
	var restored domain.DeploymentPriceVersion
	if err := json.Unmarshal(replayed.Body.Bytes(), &restored); err != nil || restored.ID != price.ID ||
		restored.CacheWriteLongMicrosPerMillion != 400_000 || restored.Schedule == nil || restored.Schedule.Windows[0].CacheWriteLongMicrosPerMillion != 600_000 {
		t.Fatalf("legacy replay returned a different price: %s (%v)", replayed.Body, err)
	}
	if fresh := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, "unused-key", body); fresh.Code != http.StatusBadRequest {
		t.Fatalf("missing rates created a new price: %d %s", fresh.Code, fresh.Body)
	}
	body["output_usd_per_million"] = "3"
	if mismatch := createPriceForTest(t, runtime, cookie, csrf, bootstrap.DeploymentID, key, body); mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched legacy request replayed: %d %s", mismatch.Code, mismatch.Body)
	}
}

func TestAdminPriceProposalReplaysLegacyScheduledProposal(t *testing.T) {
	runtime, bootstrap, cookie, csrf := openPricingRuntimeForTest(t, "Legacy proposal retry")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(time.Hour)
	path := "/admin/api/v1/deployments/" + bootstrap.DeploymentID + "/price-proposals"
	key := "legacy-proposal"
	keyDigest := pricingIdempotencyDigest("admin", bootstrap.DeploymentID, "proposal:"+key)
	deployment, err := runtime.store.GetDeployment(ctx, bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	contentHash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	proposal := domain.DeploymentPriceProposal{
		ID: deterministicMutationID("proposal", keyDigest), DeploymentID: deployment.ID, ProviderID: deployment.ProviderID,
		ProviderModel: deployment.ProviderModel, Region: deployment.Region,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 400_000, CachedInputMicrosPerMillion: 40_000,
		CacheWriteMicrosPerMillion: 400_000, CacheWriteLongMicrosPerMillion: 400_000,
		OutputMicrosPerMillion: 1_600_000,
		Schedule: &domain.PriceSchedule{Timezone: "Etc/UTC", Windows: []domain.PriceWindow{{
			StartMinute: 540, EndMinute: 720, InputMicrosPerMillion: 600_000, CachedInputMicrosPerMillion: 50_000,
			CacheWriteMicrosPerMillion: 600_000, CacheWriteLongMicrosPerMillion: 600_000, OutputMicrosPerMillion: 2_000_000,
		}}},
		Source: domain.PriceSource{
			Type: domain.PriceSourceOfficialURL, Assurance: domain.PriceAssuranceAsserted, URI: "https://example.test/pricing",
			RetrievedAt: &now, ReceivedAt: now, ContentSHA256: contentHash, Reference: "Legacy quote",
		},
		Match: domain.PriceProposalMatchExact, FetchedAt: now, ExpiresAt: expiry, Status: domain.PriceProposalPending,
		CreatedBy: "admin", CreatedAt: now, Revision: 1,
	}
	proposal.Digest, err = proposal.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(fmt.Sprintf(`{
		"billing_mode": "metered", "currency": "USD",
		"input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04",
		"output_usd_per_million": "1.6", "fixed_request_usd": "0",
		"schedule": {"timezone": "Etc/UTC", "windows": [{
			"start": "09:00", "end": "12:00", "input_usd_per_million": "0.60",
			"cached_input_usd_per_million": "0.05", "output_usd_per_million": "2", "fixed_request_usd": "0"
		}]},
		"match": "exact", "expires_at": %q,
		"source": {"type": "official_url", "uri": "https://example.test/pricing",
			"retrieved_at": %q, "content_sha256": %q, "reference": "Legacy quote"}
	}`, expiry.Format(time.RFC3339), now.Format(time.RFC3339), contentHash))
	legacyPayload := json.RawMessage(fmt.Sprintf(`{"deployment_id":%q,"input":%s}`, deployment.ID, body))
	intent, err := newPricingAuditIntent("deployment_price.proposal_create", proposal.ID, "admin", "", legacyPayload, &proposal.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runtime.store.CreateDeploymentPriceProposal(ctx, proposal, intent, keyDigest); err != nil {
		t.Fatal(err)
	}
	post := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		request := adminRequest(t, http.MethodPost, path, body)
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}
	replayed := post(key)
	if replayed.Code != http.StatusCreated || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("legacy proposal retry did not replay: %d %s", replayed.Code, replayed.Body)
	}
	var restored domain.DeploymentPriceProposal
	if err := json.Unmarshal(replayed.Body.Bytes(), &restored); err != nil || restored.ID != proposal.ID ||
		restored.CacheWriteLongMicrosPerMillion != 400_000 {
		t.Fatalf("legacy replay returned a different proposal: %s (%v)", replayed.Body, err)
	}
	if fresh := post("unused-key"); fresh.Code != http.StatusBadRequest {
		t.Fatalf("missing rates created a new proposal: %d %s", fresh.Code, fresh.Body)
	}
}
