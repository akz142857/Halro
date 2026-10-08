package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestAdminProposalPagesPreserveCreationOrderContinueAndRefuseChangedFacts(t *testing.T) {
	r, boot, cookie, csrf := openPricingRuntimeForTest(t, "proposal-pages")
	path := "/admin/api/v1/deployments/" + boot.DeploymentID + "/price-proposals"
	create := func(i int) domain.DeploymentPriceProposal {
		t.Helper()
		now := time.Now().UTC()
		body := map[string]any{
			"billing_mode": "metered", "currency": "USD", "input_usd_per_million": "0.40", "cached_input_usd_per_million": "0.04", "output_usd_per_million": "1.60", "fixed_request_usd": "0", "match": "exact", "expires_at": now.Add(time.Hour).Format(time.RFC3339),
			"source": map[string]any{"type": "official_url", "uri": "https://example.test/pricing", "reference": fmt.Sprintf("list-%d", i), "retrieved_at": now.Format(time.RFC3339), "content_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}
		request := adminRequest(t, http.MethodPost, path, body)
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Idempotency-Key", fmt.Sprintf("proposal-page-%d", i))
		response := httptest.NewRecorder()
		r.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatal(response.Code, response.Body.String())
		}
		var p domain.DeploymentPriceProposal
		if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Every create has a separate durable idempotency identity.
	items := []domain.DeploymentPriceProposal{create(0), create(1), create(2)}
	cursor := ""
	savedCursor := ""
	for i := 2; i >= 0; i-- {
		response := authenticatedAdminGet(t, r, cookie, path+"?limit=1&cursor="+url.QueryEscape(cursor))
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
		var page boltstore.DeploymentPriceProposalPage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != items[i].ID {
			t.Fatal(page, i)
		}
		if i > 0 && page.NextCursor == "" || i == 0 && page.NextCursor != "" {
			t.Fatal(page, i)
		}
		if i == 2 {
			savedCursor = page.NextCursor
		}
		cursor = page.NextCursor
	}
	create(3)
	stale := authenticatedAdminGet(t, r, cookie, path+"?limit=1&cursor="+url.QueryEscape(savedCursor))
	if stale.Code != http.StatusConflict || responseErrorCode(t, stale) != "pricing_page_changed" {
		t.Fatal(stale.Code, stale.Body.String())
	}
	for _, query := range []string{"?cursor=bad", "?limit=0", "?limit=101", "?status=pending"} {
		response := authenticatedAdminGet(t, r, cookie, path+query)
		if response.Code != http.StatusBadRequest {
			t.Fatal(query, response.Code, response.Body.String())
		}
	}
}
