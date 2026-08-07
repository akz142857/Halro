package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/sourcelimit"
)

// internal/config keeps itself free of internal imports, so the tracking
// ceiling is written out there rather than referenced. This is the guard that
// keeps the two copies from drifting apart.
func TestSourceRateLimitCeilingMatchesLimiterDefault(t *testing.T) {
	if got := config.Default().Gateway.SourceRateLimit.MaxTrackedSources; got != sourcelimit.DefaultMaxTrackedSources {
		t.Fatalf("default template ceiling = %d, sourcelimit default = %d", got, sourcelimit.DefaultMaxTrackedSources)
	}
	// A config file written before this setting existed decodes to zero, and
	// Normalize has to read that as "the usual ceiling", not "track nothing".
	cfg := testConfig(t)
	cfg.Gateway.SourceRateLimit.MaxTrackedSources = 0
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Gateway.SourceRateLimit.MaxTrackedSources; got != sourcelimit.DefaultMaxTrackedSources {
		t.Fatalf("normalized ceiling = %d, want %d", got, sourcelimit.DefaultMaxTrackedSources)
	}
}

func TestGatewayRouterShedsAnOverBudgetSource(t *testing.T) {
	cfg := testConfig(t)
	budget := 2
	cfg.Gateway.SourceRateLimit = config.SourceRateLimit{RequestsPerMinute: &budget, MaxTrackedSources: 64}
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	router := runtime.gatewayRouter()

	post := func(path, remote string, headers ...[2]string) int {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m","messages":[]}`))
		request.RemoteAddr = remote
		request.Header.Set("Content-Type", "application/json")
		for _, header := range headers {
			request.Header.Set(header[0], header[1])
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if status := post("/v1/chat/completions", "203.0.113.20:5000"); status == http.StatusTooManyRequests {
			t.Fatalf("request %d was shed inside its budget", attempt)
		}
	}
	if status := post("/v1/chat/completions", "203.0.113.20:5000"); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the source is over budget", status)
	}
	// The Anthropic surface shares the limiter and must shed in its own envelope.
	for attempt := 1; attempt <= 3; attempt++ {
		post("/v1/messages", "203.0.113.21:5000")
	}
	if status := post("/v1/messages", "203.0.113.21:5000"); status != http.StatusTooManyRequests {
		t.Fatalf("Anthropic status = %d, want 429 once the source is over budget", status)
	}
	if runtime.sourceLimiter.Rejected() < 2 {
		t.Fatalf("rejected = %d, want the shed requests counted for metrics", runtime.sourceLimiter.Rejected())
	}

	// Order matters, and only the assembled router can show it. A forged key
	// answers 401 at the guard; if the guard ran first, an anonymous flood
	// would still pay for authentication on every request and the limiter
	// would never be the thing that stopped it.
	forged := [2]string{"Authorization", "Bearer gw_forged_key_that_is_long_enough"}
	statuses := make([]int, 0, 3)
	for attempt := 0; attempt < 3; attempt++ {
		statuses = append(statuses, post("/v1/chat/completions", "203.0.113.23:5000", forged))
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("statuses = %v, want the third forged request shed with 429 — the limiter must run ahead of the guard", statuses)
	}

	// Health is deliberately outside the limiter: an orchestrator probes from
	// one address on a fixed interval, and shedding those marks a healthy
	// instance unready instead of shedding any real load.
	for attempt := 0; attempt < 20; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		request.RemoteAddr = "203.0.113.22:5000"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			t.Fatalf("liveness probe %d was shed by the per-source limiter", attempt)
		}
	}
}
