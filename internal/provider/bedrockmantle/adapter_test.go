package bedrockmantle

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testAdapter(t *testing.T, transport http.RoundTripper) *ResponsesAdapter {
	t.Helper()
	endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewResponses(ResponsesOptions{Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{Transport: transport}, Capabilities: provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestValidateEndpointPinsRegionalMantleOrigin(t *testing.T) {
	for _, raw := range []string{
		"http://bedrock-mantle.us-east-1.api.aws",
		"https://bedrock-runtime.us-east-1.amazonaws.com",
		"https://bedrock-mantle.us-east-1.api.aws/v1",
		"https://bedrock-mantle.us-east-1.api.aws.evil.example",
	} {
		endpoint, _ := url.Parse(raw)
		if err := ValidateEndpoint(endpoint); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", raw)
		}
	}
	endpoint, _ := url.Parse("https://bedrock-mantle.ap-southeast-1.api.aws")
	if err := ValidateEndpoint(endpoint); err != nil {
		t.Fatalf("rejected regional Mantle endpoint: %v", err)
	}
}

func TestResponsesAdapterUsesMantleWireAndDisablesStorage(t *testing.T) {
	adapter := testAdapter(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected operation %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer bedrock-key" || request.Header.Get("x-api-key") != "" {
			t.Fatalf("unexpected authorization headers: %#v", request.Header)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if string(payload["store"]) != "false" || string(payload["model"]) != `"amazon.nova-pro"` {
			t.Fatalf("unsafe or unrouted payload: %s", payload)
		}
		body := `{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"amazon.nova-pro","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{},"total_tokens":3}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}, "x-amzn-requestid": []string{"aws-request-1"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	response, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "request_1", ProviderModel: "amazon.nova-pro", Request: openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "amazon.nova-pro" || len(response.Choices) != 1 || response.Choices[0].Message == nil || string(response.Choices[0].Message.Content) != `"hello"` || response.Usage == nil || response.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestResponsesAdapterValidatesStreamLifecycleAndUsage(t *testing.T) {
	adapter := testAdapter(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("stream accept header missing")
		}
		body := strings.Join([]string{
			`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":7,"status":"in_progress","model":"amazon.nova-pro","output":[]}}`, ``,
			`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"hello"}`, ``,
			`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"amazon.nova-pro","output":[],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{},"total_tokens":3}}}`, ``, ``,
		}, "\n")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	var events []semantic.Event
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{RequestID: "request_1", ProviderModel: "amazon.nova-pro", Request: openaiapi.ChatCompletionRequest{Model: "public", Stream: true, Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}}, func(event semantic.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Outputs[0].Content[0].Text != "hello" || events[1].Outputs[0].Termination != "complete" || events[2].Kind != semantic.EventUsage || usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("unexpected stream events=%#v usage=%#v", events, usage)
	}
}

func TestResponsesAdapterMapsRetryableAWSFailure(t *testing.T) {
	adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}, "X-Amzn-Requestid": []string{"aws-request-2"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"quota"}}`))}, nil
	}))
	_, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "request_1", ProviderModel: "model", Request: openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}})
	providerErr, ok := err.(*provider.Error)
	if !ok || providerErr.Class != provider.ErrorRateLimit || !providerErr.Retryable || providerErr.RetryAfter != 2*time.Second || providerErr.ProviderRequestID != "aws-request-2" {
		t.Fatalf("unexpected provider error: %#v", err)
	}
}
