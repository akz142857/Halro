package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/store/lock"
)

func TestInitializeOpenAndReadiness(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected readiness status: %d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["accounting"] != "healthy" {
		t.Fatalf("unexpected readiness body: %#v", body)
	}

	runtime.status.MarkUnavailable()
	response = httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable accounting must fail readiness, got %d", response.Code)
	}
}

func TestRuntimeHoldsExclusiveDataLock(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != lock.ErrAlreadyLocked {
		t.Fatalf("expected exclusive lock error, got %v", err)
	}
}

func TestLedgerStateIsRebuiltDuringOpen(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	event := ledger.Event{
		EventID:              "evt_reservation",
		Kind:                 ledger.EventReservationCreated,
		RequestID:            "req_1",
		AttemptID:            "req_1:1",
		ProjectID:            "prj_1",
		PeriodID:             "prj_1:2026-07-31:tz1",
		OccurredAt:           time.Now().UTC(),
		ReservationMicrosUSD: 500,
	}
	if _, err := runtime.ledger.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	runtime, err = Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if got := runtime.state.Balance(event.ProjectID, event.PeriodID).ReservedMicrosUSD; got != 500 {
		t.Fatalf("unexpected rebuilt reservation: %d", got)
	}
}

func TestOpenRejectsWrongMasterKey(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	replacement := make([]byte, 32)
	if _, err := rand.Read(replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Storage.MasterKeyFile, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(replacement)
	if _, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("wrong master key must fail during open")
	}
}

func TestRuntimeWritesVerifiableStartupAndShutdownAuditEvents(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 2 {
		t.Fatalf("audit records=%d want=2", summary.Records)
	}
}

func TestMetricsRequireDerivedBearerToken(t *testing.T) {
	cfg := testConfig(t)
	cfg.Metrics.Enabled = true
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized metrics status=%d", response.Code)
	}
	token, err := MetricsToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(token)
	request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+string(token))
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "heimdall_requests_total") ||
		!strings.Contains(response.Body.String(), "heimdall_process_goroutines") ||
		!strings.Contains(response.Body.String(), "heimdall_policy_rejections_total") ||
		!strings.Contains(response.Body.String(), "heimdall_provider_active_requests") ||
		strings.Contains(response.Body.String(), string(token)) {
		t.Fatalf("metrics response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAuditCheckpointDetectsDeletedSuffix(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	startupBytes := runtime.audit.Summary().Bytes
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(cfg.AuditPath(), startupBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(context.Background(), cfg); err == nil {
		t.Fatal("deleted audit suffix must conflict with the bbolt checkpoint")
	}
}

func TestUsageAggregateMatchesReplayAfterRestart(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_1", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveAttemptDetailed(
		context.Background(), request, 0, 10,
		budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1",
			AttemptNumber: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(context.Background(), attempt, budget.Settlement{
		CommittedMicrosUSD: 7, ProviderInputTokens: 3, ProviderOutputTokens: 2,
		Outcome: "success", HTTPStatus: 200, LatencyMillis: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.usageCollector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := runtime.usage.Snapshot()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	after := runtime.usage.Snapshot()
	if before.Totals != after.Totals || len(after.Attempts) != 1 || len(after.Requests) != 1 {
		t.Fatalf("before=%#v after=%#v", before, after)
	}
	if after.Attempts[0].ProviderID != "provider_1" || after.Attempts[0].KeyID != "key_1" {
		t.Fatalf("usage metadata was not preserved: %#v", after.Attempts[0])
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		Version: config.SchemaVersion,
		Server: config.Server{
			GatewayListen:     "127.0.0.1:18080",
			AdminListen:       "127.0.0.1:18081",
			MetricsListen:     "127.0.0.1:19090",
			ReadHeaderTimeout: config.Duration(5 * time.Second),
			ReadBodyTimeout:   config.Duration(15 * time.Second),
			MaxHeaderBytes:    32768,
			MaxRequestBytes:   10 << 20,
		},
		Storage: config.Storage{
			DataDir:       filepath.Join(root, "data"),
			MetadataFile:  "heimdall.db",
			MasterKeyFile: filepath.Join(root, "master.key"),
		},
		Usage: config.Usage{Durability: "balanced", Timezone: "UTC"},
		Gateway: config.Gateway{
			RouteTotalTimeout:            config.Duration(2 * time.Minute),
			AttemptConnectTimeout:        config.Duration(5 * time.Second),
			AttemptResponseHeaderTimeout: config.Duration(time.Minute),
			StreamIdleTimeout:            config.Duration(time.Minute),
			DownstreamWriteTimeout:       config.Duration(15 * time.Second),
			StreamMaxDuration:            config.Duration(10 * time.Minute),
			MaxTotalAttempts:             3,
		},
		Metrics: config.Metrics{Enabled: false, RequireAuth: true},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	return cfg
}
