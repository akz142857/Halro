package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestAdminPricePageErrorsPreserveStorageFailureBoundary(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{errors.New("persisted price decode failed"), http.StatusServiceUnavailable},
		{boltstore.ErrPricingAuditLimit, http.StatusServiceUnavailable},
		{boltstore.ErrPricingPageCursor, http.StatusBadRequest},
		{boltstore.ErrPricingPageChanged, http.StatusConflict},
		{boltstore.ErrNotFound, http.StatusNotFound},
	} {
		writer := httptest.NewRecorder()
		writePricePageError(writer, test.err)
		if writer.Code != test.status {
			t.Fatal(test.err, writer.Code, writer.Body.String())
		}
	}
}

func TestAdminPricePagesContinueAndCarryOffPageCurrentAndScheduledPrices(t *testing.T) {
	r, bootstrap, cookie, _ := openPricingRuntimeForTest(t, "price-page")
	whole, err := r.store.ListDeploymentPriceVersions(context.Background(), bootstrap.DeploymentID)
	if err != nil || len(whole) != 1 {
		t.Fatal(whole, err)
	}
	baseline := whole[0]
	now := time.Now().UTC()
	create := func(i int) {
		t.Helper()
		price := baseline
		price.ID, price.Version, price.Revision = fmt.Sprintf("price_api_page_%d", i), 0, 0
		price.CreatedAt, price.Source.ReceivedAt = now, now
		price.EffectiveFrom = now.Add(time.Duration(i) * time.Hour)
		if _, err := r.store.CreateDeploymentPriceVersion(context.Background(), price); err != nil {
			t.Fatal(err)
		}
	}
	create(1)
	create(2)
	path := "/admin/api/v1/deployments/" + bootstrap.DeploymentID + "/prices?limit=1"
	read := func(cursor string) boltstore.DeploymentPricePage {
		t.Helper()
		target := path
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		response := authenticatedAdminGet(t, r, cookie, target)
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
		var page boltstore.DeploymentPricePage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.ActivePrice == nil || page.ActivePrice.ID != baseline.ID || page.NextScheduledPrice.ID != "price_api_page_1" || page.BlockingPrice.ID != "price_api_page_2" {
			t.Fatal(page)
		}
		return page
	}
	first := read("")
	if len(first.Items) != 1 || first.Items[0].ID != baseline.ID || first.NextCursor == "" {
		t.Fatal(first)
	}
	second := read(first.NextCursor)
	if len(second.Items) != 1 || second.Items[0].ID != "price_api_page_1" || second.NextCursor == "" {
		t.Fatal(second)
	}
	third := read(second.NextCursor)
	if len(third.Items) != 1 || third.Items[0].ID != "price_api_page_2" || third.NextCursor != "" {
		t.Fatal(third)
	}
	create(3)
	stale := authenticatedAdminGet(t, r, cookie, path+"&cursor="+url.QueryEscape(first.NextCursor))
	if stale.Code != http.StatusConflict || !json.Valid(stale.Body.Bytes()) {
		t.Fatal(stale.Code, stale.Body.String())
	}
	for _, query := range []string{"cursor=invalid", "limit=0", "limit=101", "filter=active"} {
		response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/prices?"+query)
		if response.Code != http.StatusBadRequest {
			t.Fatal(query, response.Code, response.Body.String())
		}
	}
}
