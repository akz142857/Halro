package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safelog"
	"github.com/akz142857/Halro/internal/semantic"
)

const (
	outputSecretCanary   = "sk-HALRO_OUTPUT_CANARY_0123456789abcdef"
	googleSecretCanary   = "AIzaHALRO0123456789abcdefghijk"
	awsSecretCanary      = "ASIA0123456789ABCDEF"
	bearerSecretCanary   = "Bearer halro.canary.token.0123456789"
	providerSecretCanary = "sk-HALRO_PROVIDER_CANARY_0123456789abcdef"
	adminPasswordCanary  = "correct horse battery staple"
)

func TestSecretCanaryNeverReachesTelemetryPersistenceOrAdminSurfaces(t *testing.T) {
	cfg := testConfig(t)
	cfg.Metrics.Enabled = true
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "Canary Provider", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "canary-model",
		PublicModel: "chat", ProjectName: "Canary Project",
		DailyBudgetMicrosUSD:  100_000_000,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}, []byte(providerSecretCanary))
	if err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte(adminPasswordCanary),
	); err != nil {
		t.Fatal(err)
	}

	var logOutput bytes.Buffer
	logger := safelog.New(slog.NewJSONHandler(&logOutput, nil))
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = runtime.Close()
		}
	}()

	// Registration requires a profile contract, the same as production, where
	// every adapter is wrapped before it reaches the registry.
	manifest, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	fake, bridgeErr := provider.NewLegacyAdapterBridge(&canaryAdapter{}, manifest,
		domain.EvidenceForCapabilities(
			domain.ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true},
			domain.EvidenceDeclared,
		))
	if bridgeErr != nil {
		t.Fatal(bridgeErr)
	}
	replacement := provider.NewRegistry()
	if err := replacement.Register(provider.Target{
		ID: bootstrap.RouteID, ProviderID: bootstrap.ProviderID,
		PublicModel: "chat", ProviderModel: "canary-model", Adapter: fake,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		Strategy: "ordered",
	}); err != nil {
		t.Fatal(err)
	}
	for _, retired := range runtime.providers.Replace(replacement) {
		retired.Close()
	}

	var exposed bytes.Buffer
	gatewayRequest := httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	gatewayRequest.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	gatewayRequest.Header.Set("Content-Type", "application/json")
	gatewayResponse := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(gatewayResponse, gatewayRequest)
	if gatewayResponse.Code != http.StatusOK ||
		!strings.Contains(gatewayResponse.Body.String(), "[REDACTED]") ||
		strings.Contains(gatewayResponse.Body.String(), outputSecretCanary) {
		t.Fatalf("unsafe gateway response status=%d body=%s",
			gatewayResponse.Code, gatewayResponse.Body.String())
	}
	exposed.Write(gatewayResponse.Body.Bytes())

	if err := runtime.usageCollector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.usage.Snapshot()
	if len(snapshot.Attempts) != 1 || len(snapshot.Requests) != 1 {
		t.Fatalf("usage was not finalized: %#v", snapshot)
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exposed.Write(snapshotJSON)
	if _, err := runtime.usageExporter.Export(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := runtime.usageExporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
	runtime.saveUsageCheckpoint()

	metricsToken, err := MetricsToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Authorization", "Bearer "+string(metricsToken))
	metricsResponse := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", metricsResponse.Code, metricsResponse.Body.String())
	}
	exposed.Write(metricsResponse.Body.Bytes())

	cookie, _ := loginAdminForTest(t, runtime)
	for _, route := range []string{
		"/admin/", "/admin/api/v1/dashboard", "/admin/api/v1/usage",
		"/admin/api/v1/audit", "/admin/api/v1/credentials",
		"/admin/api/v1/providers", "/admin/api/v1/routes",
		"/admin/api/v1/projects", "/admin/api/v1/system/status", "/admin/api/v1/settings",
	} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("admin surface %s status=%d body=%s", route, response.Code, response.Body.String())
		}
		exposed.Write(response.Body.Bytes())
	}

	persistentCanaries := []string{
		outputSecretCanary, googleSecretCanary, awsSecretCanary, bearerSecretCanary,
		providerSecretCanary, bootstrap.GatewayKey, adminPasswordCanary,
		string(metricsToken), cookie.Value,
	}
	clear(metricsToken)
	assertNoCanaries(t, "logs", logOutput.Bytes(), persistentCanaries)
	assertNoCanaries(t, "API/HTML/metrics", exposed.Bytes(), persistentCanaries[:7])
	var heapProfile bytes.Buffer
	goruntime.GC()
	if err := pprof.Lookup("heap").WriteTo(&heapProfile, 0); err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, "heap profile", heapProfile.Bytes(), persistentCanaries)

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := filepath.WalkDir(filepath.Dir(cfg.Storage.DataDir), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoCanaries(t, path, payload, persistentCanaries)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredPanicDoesNotExposePanicRequestOrAuthorizationCanaries(t *testing.T) {
	const panicCanary = "sk-HALRO_PANIC_CANARY_0123456789abcdef"
	var logs bytes.Buffer
	runtime := &Runtime{logger: safelog.New(slog.NewJSONHandler(&logs, nil))}
	handler := runtime.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(panicCanary)
	}))
	request := httptest.NewRequest(
		http.MethodPost, "/panic/"+panicCanary, strings.NewReader(panicCanary),
	)
	request.Header.Set("Authorization", "Bearer "+panicCanary)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "internal server error") {
		t.Fatalf("panic response status=%d body=%s", response.Code, response.Body.String())
	}
	assertNoCanaries(t, "panic response", response.Body.Bytes(), []string{panicCanary})
	assertNoCanaries(t, "panic log", logs.Bytes(), []string{panicCanary})
}

func TestEmbeddedBrowserArtifactsContainNoSecretOrPersistenceCanaries(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dist := filepath.Clean(filepath.Join(workingDirectory, "..", "webui", "dist"))
	forbidden := []string{
		outputSecretCanary, googleSecretCanary, awsSecretCanary, bearerSecretCanary,
		providerSecretCanary, adminPasswordCanary,
		"gw_plaintext-canary", "csrf-canary", "password-canary",
		"sourceMappingURL=", "localStorage", "sessionStorage", "indexedDB",
	}
	files := 0
	err = filepath.WalkDir(dist, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		files++
		if strings.HasSuffix(entry.Name(), ".map") {
			t.Fatalf("production browser artifact contains source map %s", path)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoCanaries(t, path, payload, forbidden)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("embedded browser artifact directory is empty")
	}
}

type canaryAdapter struct{}

func (*canaryAdapter) Type() string { return "openai" }

func (*canaryAdapter) Chat(
	_ context.Context,
	_ provider.ChatCall,
) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{
		ID: "canary-response", Object: "chat.completion", Model: "canary-model",
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{
			Role: "assistant",
			Content: openaiapi.TextContent(strings.Join([]string{
				"provider emitted", outputSecretCanary, googleSecretCanary,
				awsSecretCanary, bearerSecretCanary,
			}, " ")),
		}}},
		Usage: &openaiapi.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	}, nil
}

func (*canaryAdapter) ChatStream(
	_ context.Context,
	_ provider.ChatCall,
	_ func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	return nil, errors.New("not implemented")
}

func (*canaryAdapter) Embed(
	_ context.Context,
	_ provider.EmbeddingCall,
) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, errors.New("not implemented")
}

func (*canaryAdapter) Close() {}

func assertNoCanaries(t *testing.T, surface string, payload []byte, canaries []string) {
	t.Helper()
	for _, canary := range canaries {
		if canary != "" && bytes.Contains(payload, []byte(canary)) {
			t.Fatalf("%s leaked secret canary %q", surface, canary)
		}
	}
}
