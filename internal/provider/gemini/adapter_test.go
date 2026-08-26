package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestChatTranslatesOpenAITextAndUsesHeaderAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1beta/models/gemini-test:generateContent" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		if request.Header.Get("x-goog-api-key") != "secret-key" || request.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected auth headers: %#v", request.Header)
		}
		var payload generateRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.SystemInstruction == nil || partsText(payload.SystemInstruction.Parts) != "be concise" ||
			len(payload.Contents) != 2 || payload.Contents[0].Role != "user" || payload.Contents[1].Role != "model" {
			t.Fatalf("translated payload=%#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello "},{"text":"world"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6},"modelVersion":"gemini-test","responseId":"resp_1"}`))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	response, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "gemini-test",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{
			{Role: "system", Content: openaiapi.TextContent("be concise")},
			{Role: "user", Content: openaiapi.TextContent("hello")},
			{Role: "assistant", Content: openaiapi.TextContent("hi")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := openaiapi.DecodeTextContent(response.Choices[0].Message.Content)
	if !ok || text != "hello world" || response.ID != "resp_1" || response.Usage == nil || response.Usage.TotalTokens != 6 {
		t.Fatalf("response=%#v text=%q", response, text)
	}
}

func TestTranslateChatRejectsFieldsGeminiWouldDrop(t *testing.T) {
	seed := int64(7)
	request := openaiapi.ChatCompletionRequest{
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		Seed:     &seed,
	}
	if _, err := translateChat(request); err == nil {
		t.Fatal("seed was silently dropped")
	}
}

func TestTranslateChatRejectsMessageName(t *testing.T) {
	request := openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Name: "customer", Content: openaiapi.TextContent("hi")}}}
	if _, err := translateChat(request); err == nil {
		t.Fatal("messages[].name was silently dropped")
	}
}

func TestStreamNormalizesGeminiSSEAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("alt") != "sse" {
			t.Fatalf("query=%s", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]},\"index\":0}],\"responseId\":\"resp_stream\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"l\"}]},\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"o\"}]},\"finishReason\":\"MAX_TOKENS\",\"index\":0}]}\n\n"))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	var events []semantic.Event
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID: "req_stream", ProviderModel: "gemini-test",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	}, func(event semantic.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Outputs[0].Role != semantic.RoleAssistant || events[1].Usage != nil ||
		events[2].Outputs[0].NativeTermination != "length" || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("events=%#v usage=%#v", events, usage)
	}
}

func TestUnknownGeminiFinishReasonIsPreserved(t *testing.T) {
	response, err := translateResponse(generateResponse{Candidates: []candidate{{Content: content{Parts: []part{{Text: "blocked"}}}, FinishReason: "FUTURE_SAFETY_REASON"}}}, "model", "request")
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].FinishReason == nil || *response.Choices[0].FinishReason != "FUTURE_SAFETY_REASON" {
		t.Fatalf("unknown finish reason was not preserved: %#v", response.Choices[0].FinishReason)
	}
	event, _, err := translateStreamChunk(generateResponse{Candidates: []candidate{{Content: content{Parts: []part{{Text: "blocked"}}}, FinishReason: "FUTURE_SAFETY_REASON"}}}, "model", "request", true)
	if err != nil {
		t.Fatal(err)
	}
	if event.Outputs[0].NativeTermination != "FUTURE_SAFETY_REASON" || event.Outputs[0].Termination != "unknown" {
		t.Fatalf("unknown stream termination was not preserved: %#v", event.Outputs[0])
	}
}

func TestGeminiAcceptedHTTPFailuresAreAmbiguous(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusInternalServerError} {
		err := httpError(status, strings.NewReader("provider detail"))
		if !err.Ambiguous || err.Retryable {
			t.Fatalf("status %d classification=%#v", status, err)
		}
	}
}

func TestEmbeddingArrayAndProbe(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			if request.URL.Path != "/v1beta/models" {
				t.Fatalf("probe path=%s", request.URL.Path)
			}
			_, _ = writer.Write([]byte(`{"models":[]}`))
			return
		}
		requests++
		if request.URL.Path != "/v1beta/models/gemini-embedding:embedContent" {
			t.Fatalf("embedding path=%s", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"embedding":{"values":[0.1,0.2]}}`))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	if err := adapter.Probe(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Embed(context.Background(), provider.EmbeddingCall{
		RequestID: "req_embed", ProviderModel: "gemini-embedding",
		Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`["one","two"]`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(result.Data) != 2 || result.Data[1].Index != 1 {
		t.Fatalf("requests=%d result=%#v", requests, result)
	}
}

func TestErrorsAreClassifiedWithoutReturningProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"secret-key echoed"}}`))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_error", ProviderModel: "gemini-test",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	})
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorAuthentication || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error=%v classified=%#v", err, classified)
	}
}

func testAdapter(t *testing.T, endpoint string) *Adapter {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{Endpoint: parsed, APIKey: []byte("secret-key"), Client: serverClient()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func serverClient() *http.Client { return &http.Client{} }

// A Gemini refusal used to reach the operator as a bare "Gemini error (400)":
// the body was drained and discarded, so the one part that says which field was
// refused was thrown away with the sentence beside it. Only identifiers are
// kept — the RPC status and the violated field path — and the description
// Google wrote about the request is not one.
func TestGeminiRefusalKeepsIdentifiersAndDropsTheSentence(t *testing.T) {
	body := `{"error":{"code":400,"message":"secret-key echoed","status":"INVALID_ARGUMENT",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":` +
		`[{"field":"generation_config.thinking_config","description":"thinking is not supported by this model"}]}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(body))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_error", ProviderModel: "gemini-test",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	})
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorBadRequest {
		t.Fatalf("error=%v classified=%#v", err, classified)
	}
	if classified.ProviderCode != "INVALID_ARGUMENT:generation_config.thinking_config" {
		t.Fatalf("refusal identifier lost: %q", classified.ProviderCode)
	}
	// The RPC status names the class of refusal, not the field, so the refusal
	// stays unattributed however specific the field violation beside it looks.
	if classified.Refusal != provider.RefusalInvalid {
		t.Fatalf("refusal kind=%q", classified.Refusal)
	}
	if strings.Contains(err.Error(), "secret-key") || strings.Contains(classified.ProviderCode, "not supported") {
		t.Fatalf("provider sentence reached the error: %v / %q", err, classified.ProviderCode)
	}

	// A refusal that is not about the request body carries no kind, and a body
	// that is not a Google RPC envelope carries no identifier.
	throttled := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`<html>too many</html>`))
	}))
	defer throttled.Close()
	limited := testAdapter(t, throttled.URL)
	defer limited.Close()
	_, err = limited.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_error", ProviderModel: "gemini-test",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	})
	if !errors.As(err, &classified) || classified.Refusal != provider.RefusalNone || classified.ProviderCode != "" {
		t.Fatalf("classified=%#v", classified)
	}
}

// End to end: the adapter reads Google's RPC status, the detector turns the
// normalized kind into a capability answer. Gemini's error body used to be
// discarded outright, so this family could never record anything but
// "inconclusive" for a refused probe.
func TestGeminiRefusalBecomesAnUnsupportedVerdictOnlyForADependentProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"code":400,"message":"not supported","status":"INVALID_ARGUMENT",` +
			`"details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"system_instruction"}]}]}}`))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()
	manifest, ok := provider.BuiltinProfile(domain.ProfileGeminiText)
	if !ok {
		t.Fatal("profile missing")
	}
	capabilities := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderGemini, domain.ProfileGeminiText)
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := provider.ModelCapabilityDetectionTarget{ProviderModel: "gemini-test", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}

	dependent := bridge.DetectCapability(context.Background(), target,
		provider.CapabilityProbe{Capability: "developer_role", Kind: "developer_message", DependsOn: []string{"chat"}, MaxOutputTokens: 16})
	if dependent.Status != domain.ProbeUnsupported {
		t.Fatalf("dependent probe recorded %s, want unsupported: %#v", dependent.Status, dependent)
	}
	root := bridge.DetectCapability(context.Background(), target,
		provider.CapabilityProbe{Capability: "chat", Kind: "minimal_chat", MaxOutputTokens: 16})
	if root.Status != domain.ProbeInconclusive {
		t.Fatalf("baseline probe recorded %s, want inconclusive: %#v", root.Status, root)
	}
}
