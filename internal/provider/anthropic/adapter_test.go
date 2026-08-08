package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func newTestAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialAnthropicAPIKey, "x-api-key", "", []byte("provider-secret"), "Authorization")
	if err != nil {
		var classified *provider.Error
		if errors.As(err, &classified) {
			t.Fatalf("%v: %v", err, classified.Cause)
		}
		t.Fatal(err)
	}
	adapter, err := New(Options{Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(), Capabilities: provider.Capabilities{Chat: true, Streaming: true, Tools: true, Reasoning: true, StreamUsage: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestMessagesNativePreservesThinkingBlockOrderAndUsesProviderCredential(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-api-key") != "provider-secret" || request.Header.Get("anthropic-version") != anthropicapi.SupportedVersion {
			t.Errorf("unexpected headers: %v", request.Header)
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct{ Type, Signature string } `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "claude-provider" || len(body.Messages) == 0 || len(body.Messages[0].Content) < 2 || body.Messages[0].Content[0].Type != "thinking" || body.Messages[0].Content[0].Signature != "opaque-sig" || body.Messages[0].Content[1].Type != "tool_use" {
			t.Errorf("thinking round-trip changed: %#v", body)
		}
		writer.Header().Set("request-id", "req_provider")
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_provider","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-provider","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":4,"output_tokens":1}}`))
	})
	payload := []byte(`{"model":"public","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"opaque-sig"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	result, err := adapter.MessagesNative(context.Background(), provider.NativeMessageCall{RequestID: "req_gateway", ProviderModel: "claude-provider", Version: anthropicapi.SupportedVersion, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderRequestID != "req_provider" {
		t.Fatalf("request id=%q", result.ProviderRequestID)
	}
}

func TestBedrockMantleMessagesUsesNativePathAndAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/anthropic/v1/messages" || request.Header.Get("x-api-key") != "bedrock-key" || request.Header.Get("Authorization") != "" || request.Header.Get("anthropic-version") != anthropicapi.SupportedVersion {
			t.Errorf("unexpected Mantle request: %s %#v", request.URL.Path, request.Header)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["model"]) != `"anthropic.claude-provider"` || !strings.Contains(string(body["messages"]), "opaque-sig") {
			t.Errorf("native payload changed: %s", body)
		}
		writer.Header().Set("content-type", "application/json")
		writer.Header().Set("x-amzn-requestid", "aws-request-1")
		_, _ = writer.Write([]byte(`{"id":"msg_provider","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"anthropic.claude-provider","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":4,"output_tokens":1}}`))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("bedrock-key"), "Authorization", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(), ProviderType: string(domain.ProviderBedrock), CredentialScheme: domain.CredentialBedrockAPIKey, MessagesPath: "anthropic/v1/messages", Capabilities: provider.Capabilities{Chat: true, Tools: true, Reasoning: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	payload := []byte(`{"model":"public","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"opaque-sig"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]}`)
	result, err := adapter.MessagesNative(context.Background(), provider.NativeMessageCall{RequestID: "req", ProviderModel: "anthropic.claude-provider", Version: anthropicapi.SupportedVersion, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderRequestID != "aws-request-1" {
		t.Fatalf("AWS request id=%q", result.ProviderRequestID)
	}
}

func TestMessagesNativeStreamValidatesAndPreservesEvents(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	payload := []byte(`{"model":"public","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	var types []string
	usage, err := adapter.MessagesNativeStream(context.Background(), provider.NativeMessageCall{RequestID: "req", ProviderModel: "claude", Version: anthropicapi.SupportedVersion, Payload: payload}, func(event anthropicapi.RawStreamEvent) error {
		types = append(types, event.Type)
		if event.Type == "content_block_delta" && !strings.Contains(string(event.Data), "opaque") {
			t.Fatal("signature delta changed")
		}
		return nil
	})
	if err != nil {
		var classified *provider.Error
		if errors.As(err, &classified) {
			t.Fatalf("%v: %v", err, classified.Cause)
		}
		t.Fatal(err)
	}
	if len(types) != 6 || usage == nil || usage.InputTokens != 2 || usage.OutputTokens != 1 {
		t.Fatalf("types=%v usage=%#v", types, usage)
	}
}

func TestAnthropicHTTPErrorClassification(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("request-id", "req_rate")
		writer.Header().Set("retry-after", "3")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_rate"}`))
	})
	_, err := adapter.MessagesNative(context.Background(), provider.NativeMessageCall{RequestID: "req", ProviderModel: "claude", Version: anthropicapi.SupportedVersion, Payload: []byte(`{"model":"public","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)})
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorRateLimit || classified.ProviderCode != "rate_limit_error" || classified.RetryAfter.Seconds() != 3 {
		t.Fatalf("unexpected error: %#v", err)
	}
}
