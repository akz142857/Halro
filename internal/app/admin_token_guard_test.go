package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminTokenGuardPolicyLifecycleAndPreview(t *testing.T) {
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
	body := map[string]any{
		"name": "Production guard", "enabled": true, "action": "temporary_block",
		"request_tokens": int64(10_000), "tokens_per_minute": int64(100_000),
		"cost_micros_per_minute": int64(1_000_000), "error_rate": 0.2,
		"minimum_samples": int64(2), "concurrency": int64(20),
		"unique_ips_per_minute": int64(10), "violations_before_block": int64(2),
		"block_ttl_seconds": int64(300), "cooldown_seconds": int64(60),
		"ewma_enabled": true, "ewma_alpha": 0.2, "ewma_multiplier": 3.0,
		"ewma_minimum_samples": int64(100), "ewma_warmup_seconds": int64(3600),
		"ewma_evaluation_window_seconds": int64(60), "ewma_cooldown_seconds": int64(300),
		"ewma_absolute_rpm": int64(60), "ewma_absolute_tpm": int64(50_000),
		"ewma_absolute_tokens_per_request": 4_000.0,
		"ewma_absolute_cost_micros_per_minute": int64(1_000_000),
	}
	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/token-guard-policies", "", body)
	if created.Code != http.StatusCreated || created.Header().Get("ETag") != `"1"` {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var policy tokenGuardView
	if err := json.Unmarshal(created.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if !runtime.tokenGuard.HasPolicy(policy.ID) || policy.BlockTTLSeconds != 300 ||
		!policy.EWMAEnabled || policy.EWMAEvaluationWindowSeconds != 60 || policy.EWMAMultiplier != 3 {
		t.Fatalf("policy was not hot loaded: %#v", policy)
	}
	preview := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/token-guard-policies/"+policy.ID+"/test", "",
		map[string]any{
			"estimated_tokens": int64(20_000), "estimated_cost_micros_usd": int64(0),
			"concurrency": int64(1), "has_new_source": false,
			"window": map[string]int64{},
		})
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var result struct {
		Violated bool   `json:"violated"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Violated || result.Reason != "request_tokens" {
		t.Fatalf("unexpected preview: %#v", result)
	}
	now := time.Now().UTC()
	if _, err := runtime.store.PutProject(context.Background(), domain.Project{
		ID: "prj_guard", Name: "Guarded", Enabled: true,
		TokenGuardPolicyID: policy.ID, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	blocked := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/token-guard-policies/"+policy.ID, `"1"`, nil)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("referenced delete status=%d body=%s", blocked.Code, blocked.Body.String())
	}
}
