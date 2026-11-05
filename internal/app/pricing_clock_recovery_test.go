package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	storelock "github.com/akz142857/Halro/internal/store/lock"
)

func TestAdminClockPricingRecoveryAndOfflineBoundary(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(ctx, cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	seeded, err := Bootstrap(ctx, cfg, BootstrapOptions{ProviderName: "test", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com", ProviderModel: "test", PublicModel: "test", ProjectName: "test", BillingMode: domain.BillingModeMetered, InputMicrosPerMillion: 1, OutputMicrosPerMillion: 1}, []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	now := time.Now().UTC()
	if _, _, _, err := runtime.store.PrepareDeploymentPricePin(ctx, seeded.DeploymentID, "att_clock_original", now, time.Now(), time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runtime.store.PrepareDeploymentPricePin(ctx, seeded.DeploymentID, "att_clock_jump", now.Add(2*time.Minute), time.Now(), time.Second, 30*time.Second); !errors.Is(err, boltstore.ErrPricingQuarantined) {
		t.Fatalf("metered forward jump did not quarantine: %v", err)
	}
	if _, err := ClockPricingOffline(ctx, cfg, seeded.DeploymentID, nil); !errors.Is(err, storelock.ErrAlreadyLocked) {
		t.Fatalf("offline access while live: %v", err)
	}
	cookie, csrf := loginAdminForTest(t, runtime)
	endpoint := "/admin/api/v1/deployments/" + seeded.DeploymentID + "/prices/clock-confirm"
	var review domain.PricingClockReview
	response := authenticatedAdminGet(t, runtime, cookie, endpoint)
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &review) != nil {
		t.Fatalf("review: %d %s", response.Code, response.Body.String())
	}
	post := func(body any, token string) *httptest.ResponseRecorder {
		request := adminRequest(t, http.MethodPost, endpoint, body)
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", token)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}
	body := map[string]any{"confirmation": review.Confirmation, "current_password": "correct horse battery staple"}
	if got := post(body, "wrong"); got.Code != 403 {
		t.Fatalf("CSRF=%d", got.Code)
	}
	if got := post(map[string]any{"confirmation": review.Confirmation}, csrf); got.Code != 401 {
		t.Fatalf("stepup=%d", got.Code)
	}
	user, _ := runtime.store.GetAdminUser(ctx, "admin")
	user.Role = domain.AdminRoleReadOnly
	user, err = runtime.store.PutAdminUser(ctx, user, user.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if got := post(body, csrf); got.Code != 403 {
		t.Fatalf("read only=%d", got.Code)
	}
	user.Role = domain.AdminRoleAdministrator
	if _, err := runtime.store.PutAdminUser(ctx, user, user.Revision); err != nil {
		t.Fatal(err)
	}
	stale := review.Confirmation
	stale.Revision++
	if got := post(map[string]any{"confirmation": stale, "current_password": "correct horse battery staple"}, csrf); got.Code != 412 {
		t.Fatalf("stale=%d %s", got.Code, got.Body.String())
	}
	if got := post(body, csrf); got.Code != 200 {
		t.Fatalf("recover=%d %s", got.Code, got.Body.String())
	}
	if err := runtime.store.PricingReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	if !strings.Contains(audit.Body.String(), "deployment_price.clock_confirm") {
		t.Fatal("clock recovery missing from audit")
	}
	// Quarantine a second time, stop, then exercise the exact offline operator path.
	if _, _, _, err := runtime.store.PrepareDeploymentPricePin(ctx, seeded.DeploymentID, "att_after", time.Now().UTC(), time.Now(), time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runtime.store.PrepareDeploymentPricePin(ctx, seeded.DeploymentID, "att_again", time.Now().UTC().Add(2*time.Minute), time.Now(), time.Second, 30*time.Second); !errors.Is(err, boltstore.ErrPricingQuarantined) {
		t.Fatalf("second metered forward jump did not quarantine: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	offlineReview, err := ClockPricingOffline(ctx, cfg, seeded.DeploymentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ClockPricingOffline(ctx, cfg, seeded.DeploymentID, &offlineReview.Confirmation); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.store.PricingReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	intents, err := restarted.store.ListPendingPricingAuditIntents(ctx)
	if err != nil || len(intents) != 0 {
		t.Fatalf("undelivered audit=%v %v", intents, err)
	}
}
