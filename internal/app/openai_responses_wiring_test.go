package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestOpenAIResponsesBindingStreamsThroughProductionWrappers(t *testing.T) {
	endpoint, _ := url.Parse("https://api.openai.com")
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected upstream %s", request.URL)
		}
		var wire openaiapi.ResponseRequest
		if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
			t.Fatal(err)
		}
		if !wire.Stream || wire.Reasoning.Effort != "max" || wire.Model != "gpt-6.1-sol" || wire.Tools[0].Strict == nil || *wire.Tools[0].Strict {
			t.Fatalf("wrapper lost controls: %+v", wire)
		}
		body := `data: {"type":"response.created","response":{"id":"resp_1","created_at":1,"model":"gpt-6.1-sol","status":"in_progress"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}

data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"ok"}

data: {"type":"response.completed","response":{"id":"resp_1","created_at":1,"model":"gpt-6.1-sol","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	instance := domain.ProviderInstance{ID: "openai", Type: domain.ProviderOpenAI, BaseURL: endpoint.String()}
	binding := domain.ProviderProfileBinding{ID: "responses", ProviderID: instance.ID, ProfileID: domain.ProfileOpenAIResponses, AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic, Enabled: true, Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, domain.ProfileOpenAIResponses)}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("test-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	manifest, _ := provider.BuiltinProfile(binding.ProfileID)
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := bridge.Operations().Resolve(provider.OperationChatStream)
	if !ok {
		t.Fatal("production wrapper hid Responses streaming")
	}
	canonical, err := openaiwire.DecodeGenerate(openaiapi.ChatCompletionRequest{Model: "alias", Stream: true, StreamOptions: &openaiapi.StreamOptions{IncludeUsage: true}, ReasoningEffort: "max", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}, Tools: []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "ping", Parameters: json.RawMessage(`{"type":"object"}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	validator := semantic.NewStreamValidator()
	usage, err := op.(provider.GenerationAdapter).GenerateStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, validator.Accept)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Finalize(true); err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.OutputTokens != 5 {
		t.Fatal("usage lost through production wrappers")
	}
}
