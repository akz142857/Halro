package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/safelog"
	"github.com/akz142857/Heimdall/internal/semantic"
)

// Content canaries are deliberately benign. The secret canaries in
// secret_canary_test.go are credential-shaped, so redaction catches them and a
// clean data directory only proves the redaction engine works. These carry no
// credential shape, no key prefix, and no PII pattern, so nothing along the
// path has grounds to rewrite them: if they are absent from a persisted file it
// is because that surface never records request or response content at all.
//
// Every canary is asserted present in the client's own response before the
// persistence sweep runs. Without that guard a future redaction rule that
// happened to match one of these strings would turn its absence on disk into a
// silent pass.
const (
	promptContentCanary     = "HEIMDALL-CONTENT-CANARY-PROMPT-quokka-lamplight"
	systemContentCanary     = "HEIMDALL-CONTENT-CANARY-SYSTEM-marmot-driftwood"
	toolArgumentsCanary     = "HEIMDALL-CONTENT-CANARY-TOOLARGS-pangolin-ashfall"
	toolResultContentCanary = "HEIMDALL-CONTENT-CANARY-TOOLRESULT-tapir-glasswing"
	completionContentCanary = "HEIMDALL-CONTENT-CANARY-COMPLETION-vicuna-saltmarsh"
	streamContentCanary     = "HEIMDALL-CONTENT-CANARY-STREAM-caracal-thornfield"
)

func TestContentCanaryNeverPersistsOutsideTheResponsePath(t *testing.T) {
	cfg := testConfig(t)
	cfg.Metrics.Enabled = true
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "Content Canary Provider", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "canary-model",
		PublicModel: "chat", ProjectName: "Content Canary Project",
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

	fake := &contentCanaryAdapter{}
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

	// Buffered request-shaped completion: system text, user text, an assistant
	// tool call whose arguments carry a canary, and a tool result. Tool
	// arguments and tool results travel a different content path than plain
	// message text, so each gets its own canary.
	unaryBody := contentCanaryRequestBody(t, false)
	unary := httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions", bytes.NewReader(unaryBody),
	)
	unary.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	unary.Header.Set("Content-Type", "application/json")
	unaryResponse := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(unaryResponse, unary)
	if unaryResponse.Code != http.StatusOK {
		t.Fatalf("buffered gateway status=%d body=%s",
			unaryResponse.Code, unaryResponse.Body.String())
	}

	// Streaming travels a separate redaction and accounting path, including the
	// chunk-boundary handling, so it is exercised as its own request.
	streamBody := contentCanaryRequestBody(t, true)
	stream := httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions", bytes.NewReader(streamBody),
	)
	stream.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	stream.Header.Set("Content-Type", "application/json")
	streamResponse := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(streamResponse, stream)
	if streamResponse.Code != http.StatusOK {
		t.Fatalf("streaming gateway status=%d body=%s",
			streamResponse.Code, streamResponse.Body.String())
	}

	// Anti-vacuity guard. The provider echoes back what it received, so these
	// assertions prove three things at once: the canaries reached the upstream
	// adapter, they came back through the facade unmodified, and the persistence
	// sweep below is therefore looking for strings that genuinely existed on the
	// wire rather than ones redaction had already erased.
	received := fake.observed()
	for name, canary := range map[string]string{
		"system":      systemContentCanary,
		"prompt":      promptContentCanary,
		"tool args":   toolArgumentsCanary,
		"tool result": toolResultContentCanary,
	} {
		if !strings.Contains(received, canary) {
			t.Fatalf("%s canary never reached the provider adapter; "+
				"the persistence sweep would pass vacuously", name)
		}
	}
	if !strings.Contains(unaryResponse.Body.String(), completionContentCanary) {
		t.Fatal("completion canary was rewritten before reaching the client; " +
			"choose a canary that redaction does not match")
	}
	if !strings.Contains(streamResponse.Body.String(), streamContentCanary) {
		t.Fatal("stream canary was rewritten before reaching the client; " +
			"choose a canary that redaction does not match")
	}

	// Force every derivative to disk. Without this the sweep races the
	// collector and a leak could hide in an unflushed buffer.
	if err := runtime.usageCollector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.usage.Snapshot()
	if len(snapshot.Requests) != 2 {
		t.Fatalf("expected both requests to finalize, got %#v", snapshot)
	}
	if _, err := runtime.usageExporter.Export(snapshot); err != nil {
		t.Fatal(err)
	}
	runtime.saveUsageCheckpoint()

	contentCanaries := []string{
		promptContentCanary, systemContentCanary, toolArgumentsCanary,
		toolResultContentCanary, completionContentCanary, streamContentCanary,
	}

	// Read-back surfaces. The gateway response itself is excluded on purpose:
	// returning content to the caller is the one-time response path, which is
	// the whole point of the Gateway. Everything below is a surface that
	// outlives the request.
	var exposed bytes.Buffer
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exposed.Write(snapshotJSON)

	metricsToken, err := MetricsToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Authorization", "Bearer "+string(metricsToken))
	metricsResponse := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s",
			metricsResponse.Code, metricsResponse.Body.String())
	}
	exposed.Write(metricsResponse.Body.Bytes())
	clear(metricsToken)

	cookie, _ := loginAdminForTest(t, runtime)
	for _, route := range []string{
		"/admin/api/v1/dashboard", "/admin/api/v1/usage", "/admin/api/v1/audit",
		"/admin/api/v1/projects", "/admin/api/v1/routes",
		"/admin/api/v1/system/status",
	} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("admin surface %s status=%d body=%s",
				route, response.Code, response.Body.String())
		}
		exposed.Write(response.Body.Bytes())
	}

	assertNoCanaries(t, "logs", logOutput.Bytes(), contentCanaries)
	assertNoCanaries(t, "admin API/metrics/usage snapshot", exposed.Bytes(), contentCanaries)

	// Close first so the WAL, bolt metadata, Parquet partitions, and checkpoints
	// are all fsynced and consistent before the byte scan.
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	scanned := 0
	if err := filepath.WalkDir(
		filepath.Dir(cfg.Storage.DataDir),
		func(path string, entry fs.DirEntry, walkErr error) error {
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
			scanned++
			assertNoCanaries(t, path, payload, contentCanaries)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no files were scanned; the sweep proved nothing")
	}
}

// contentCanaryRequestBody builds a request whose every content-bearing field
// carries a canary.
func contentCanaryRequestBody(t *testing.T, streaming bool) []byte {
	t.Helper()
	text := func(value string) json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	request := openaiapi.ChatCompletionRequest{
		Model:  "chat",
		Stream: streaming,
		Messages: []openaiapi.Message{
			{Role: "system", Content: text("policy: " + systemContentCanary)},
			{Role: "user", Content: text("question: " + promptContentCanary)},
			{Role: "assistant", ToolCalls: []openaiapi.ToolCall{{
				ID: "call_canary_1", Type: "function",
				Function: openaiapi.ToolCallFunction{
					Name:      "lookup",
					Arguments: fmt.Sprintf(`{"query":%q}`, toolArgumentsCanary),
				},
			}}},
			{Role: "tool", ToolCallID: "call_canary_1",
				Content: text("result: " + toolResultContentCanary)},
		},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// contentCanaryAdapter records the request content it was handed so the test
// can prove the canaries survived the facade, then emits canary-bearing output
// on both the buffered and streaming paths.
type contentCanaryAdapter struct {
	mu       sync.Mutex
	received strings.Builder
}

func (a *contentCanaryAdapter) Type() string { return "openai" }

func (a *contentCanaryAdapter) observe(request openaiapi.ChatCompletionRequest) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.received.Write(encoded)
}

func (a *contentCanaryAdapter) observed() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.received.String()
}

func (a *contentCanaryAdapter) Chat(
	_ context.Context,
	call provider.ChatCall,
) (openaiapi.ChatCompletionResponse, error) {
	a.observe(call.Request)
	return openaiapi.ChatCompletionResponse{
		ID: "canary-response", Object: "chat.completion", Model: "canary-model",
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{
			Role:    "assistant",
			Content: openaiapi.TextContent("answer: " + completionContentCanary),
		}}},
		Usage: &openaiapi.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
	}, nil
}

func (a *contentCanaryAdapter) ChatStream(
	_ context.Context,
	call provider.ChatCall,
	emit func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	a.observe(call.Request)
	// Split across two deltas so the canary straddles a chunk boundary — the
	// case where a streaming rewriter is most likely to buffer content.
	head, tail := streamContentCanary[:20], streamContentCanary[20:]
	for _, fragment := range []string{"answer: " + head, tail} {
		if err := emit(semantic.Event{
			Kind: semantic.EventDelta, ID: "canary-stream", Model: "canary-model",
			Outputs: []semantic.OutputDelta{{
				Index: 0, Role: semantic.RoleAssistant,
				Content: []semantic.ContentDelta{{
					Kind: semantic.ContentText, Text: fragment,
				}},
			}},
		}); err != nil {
			return nil, err
		}
	}
	if err := emit(semantic.Event{
		Kind: semantic.EventUsage, ID: "canary-stream", Model: "canary-model",
		Usage: &semantic.Usage{
			InputTokens: 7, OutputTokens: 5, TotalTokens: 12,
			Source: semantic.UsageProviderReported,
		},
	}); err != nil {
		return nil, err
	}
	return &openaiapi.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}, nil
}

func (a *contentCanaryAdapter) Embed(
	_ context.Context,
	_ provider.EmbeddingCall,
) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}

func (a *contentCanaryAdapter) Close() {}
