package bedrockmantle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
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

// The Responses profiles differ only in the route they address. Two models —
// openai.gpt-oss-20b and openai.gpt-oss-120b — answer on /v1/responses, and the
// eleven models on the second route answer on /openai/v1/responses; a request
// sent to the wrong one is refused with "isn't supported on this route", never
// silently served. The prefix therefore has to reach the request path, and the
// default has to stay the bare /v1 join.
func TestResponsesAdapterAddressesTheRouteTheProfileFixed(t *testing.T) {
	for _, test := range []struct {
		name, prefix, want string
	}{
		{name: "default route", prefix: "", want: "/v1/responses"},
		{name: "openai route", prefix: "openai/v1", want: "/openai/v1/responses"},
		{name: "slashes are trimmed", prefix: "/openai/v1/", want: "/openai/v1/responses"},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
			authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
			if err != nil {
				t.Fatal(err)
			}
			seen := ""
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				seen = request.URL.Path
				body := `{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"openai.gpt-oss-20b","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{},"total_tokens":2}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			adapter, err := NewResponses(ResponsesOptions{Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{Transport: transport}, Capabilities: provider.Capabilities{Chat: true}, OperationPathPrefix: test.prefix})
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			if _, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "request_1", ProviderModel: "openai.gpt-oss-20b", Request: openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}}); err != nil {
				t.Fatal(err)
			}
			if seen != test.want {
				t.Fatalf("request path is %q, want %q", seen, test.want)
			}
		})
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
		// Halro addresses the account's default Bedrock project, so OpenAI-Project
		// is never sent. anthropic-workspace-id belongs to Claude Platform on AWS,
		// a different service on a different host, and must never appear here.
		for _, name := range []string{"OpenAI-Project", "OpenAI-Organization", "anthropic-workspace", "anthropic-workspace-id"} {
			if value := request.Header.Get(name); value != "" {
				t.Fatalf("%s was sent while Halro only supports the account default project: %q", name, value)
			}
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

// Enumeration and claim production are separate jobs, and this route can only
// do the first: the OpenAI-shaped catalog carries an identifier and an owner,
// so any capability read off it would be a guess wearing declared evidence.
func TestResponsesAdapterEnumeratesWithoutClaimingAnything(t *testing.T) {
	var requested string
	adapter := testAdapter(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = request.URL.String()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"openai.gpt-5.6-luna","owned_by":"openai"},{"id":"  ","owned_by":"x"},{"id":"moonshotai.kimi-k2.5"}]}`))}, nil
	}))
	discovery := adapter.InvocationTargetDiscovery()
	if !discovery.CanEnumerate || !discovery.CanDescribe || !discovery.CanVerify {
		t.Fatalf("discovery does not advertise enumeration: %#v", discovery)
	}
	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if requested != "https://bedrock-mantle.us-east-1.api.aws/v1/models" {
		t.Fatalf("catalog addressed %q", requested)
	}
	if len(targets) != 2 || targets[0].TargetID != "openai.gpt-5.6-luna" || targets[1].TargetID != "moonshotai.kimi-k2.5" {
		t.Fatalf("targets=%#v", targets)
	}
	for _, target := range targets {
		// No metadata means no metadata source. A target that claimed one would
		// let a downstream merge treat emptiness as an answer.
		if target.MetadataSource != domain.MetadataSourceNone {
			t.Fatalf("target %q claims a metadata source", target.TargetID)
		}
		if len(target.Metadata.InputModalities) != 0 || len(target.Metadata.OutputModalities) != 0 || len(target.Metadata.SupportedOperations) != 0 {
			t.Fatalf("target %q invented metadata: %#v", target.TargetID, target.Metadata)
		}
	}
	// The claim mapper is the thing that must not exist on this route.
	if _, ok := any(adapter).(provider.ProviderMetadataMapper); ok {
		t.Fatal("Mantle Responses claims capabilities it has no evidence for")
	}
}

func TestResponsesAdapterBoundsTheModelCatalog(t *testing.T) {
	entries := make([]string, maxCatalogEntries+1)
	for index := range entries {
		entries[index] = `{"id":"m` + strconv.Itoa(index) + `"}`
	}
	adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[` + strings.Join(entries, ",") + `]}`))}, nil
	}))
	if _, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{}); err == nil {
		t.Fatal("an unbounded catalog was accepted")
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

// A Mantle Responses provider that names a project sends OpenAI-Project; one
// that does not sends no resource header and is served by the account default.
func TestResponsesAdapterRendersTheBedrockProject(t *testing.T) {
	for _, test := range []struct{ projectID, expected string }{
		{"proj_abc123", "proj_abc123"},
		{"", ""},
	} {
		var seen *http.Request
		endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := NewResponses(ResponsesOptions{
			Endpoint: endpoint, Authorizer: authorizer,
			Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				seen = request
				if request.URL.Path == "/v1/responses" {
					if request.Header.Get("Accept") == "text/event-stream" {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
							`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":7,"status":"in_progress","model":"amazon.nova-pro","output":[]}}`, ``,
							`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"amazon.nova-pro","output":[],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{},"total_tokens":3}}}`, ``, ``,
						}, "\n")))}, nil
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(
						`{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"amazon.nova-pro","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{},"total_tokens":3}}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[]}`))}, nil
			})},
			Capabilities:     provider.Capabilities{Chat: true},
			BedrockProjectID: test.projectID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.Probe(context.Background(), "openai.gpt-test"); err != nil {
			t.Fatalf("project %q: %v", test.projectID, err)
		}
		if got := seen.Header.Get("OpenAI-Project"); got != test.expected {
			t.Fatalf("project %q rendered OpenAI-Project=%q", test.projectID, got)
		}
		if seen.Header.Get("anthropic-workspace") != "" || seen.Header.Get("anthropic-workspace-id") != "" {
			t.Fatalf("project %q leaked a foreign resource header: %v", test.projectID, seen.Header)
		}
		if seen.Header.Get("Authorization") != "Bearer bedrock-key" {
			t.Fatalf("project %q disturbed the credential header: %v", test.projectID, seen.Header)
		}
		// The probe is a metadata read. Every billed request goes out through the
		// data plane instead, and it is the data plane whose requests have to be
		// associated with the project — a header the probe sends and inference
		// does not would put the spend on the account default.
		chatRequest := provider.ChatCall{
			RequestID: "request_1", ProviderModel: "amazon.nova-pro",
			Request: openaiapi.ChatCompletionRequest{
				Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
			},
		}
		seen = nil
		if _, err := adapter.Chat(context.Background(), chatRequest); err != nil {
			t.Fatalf("project %q: chat: %v", test.projectID, err)
		}
		if seen == nil || seen.URL.Path != "/v1/responses" {
			t.Fatalf("project %q: chat did not reach the data plane: %#v", test.projectID, seen)
		}
		if got := seen.Header.Get("OpenAI-Project"); got != test.expected {
			t.Fatalf("project %q rendered OpenAI-Project=%q on a Chat request", test.projectID, got)
		}
		streamRequest := chatRequest
		streamRequest.Request.Stream = true
		seen = nil
		if _, err := adapter.ChatStream(context.Background(), streamRequest, func(semantic.Event) error { return nil }); err != nil {
			t.Fatalf("project %q: chat stream: %v", test.projectID, err)
		}
		if seen == nil || seen.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("project %q: the stream did not reach the data plane: %#v", test.projectID, seen)
		}
		if got := seen.Header.Get("OpenAI-Project"); got != test.expected {
			t.Fatalf("project %q rendered OpenAI-Project=%q on a ChatStream request", test.projectID, got)
		}
		adapter.Close()
	}
}

// The Mantle status matrix, one table rather than a test per code. 401 and 403
// are the ones that decide whether an expired key or a project the caller has
// no rights to silently moves traffic to a standby deployment: they must be
// classified as authentication and must not be retryable.
func TestResponsesAdapterClassifiesTheMantleStatusMatrix(t *testing.T) {
	for _, test := range []struct {
		status    int
		class     provider.ErrorClass
		retryable bool
	}{
		{401, provider.ErrorAuthentication, false},
		{403, provider.ErrorAuthentication, false},
		{400, provider.ErrorBadRequest, false},
		{404, provider.ErrorBadRequest, false},
		{408, provider.ErrorTimeout, true},
		{429, provider.ErrorRateLimit, true},
		{500, provider.ErrorProvider5xx, true},
		{502, provider.ErrorProvider5xx, true},
		{503, provider.ErrorProvider5xx, true},
		{504, provider.ErrorTimeout, true},
	} {
		adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: test.status,
				Header:     http.Header{"X-Amzn-Requestid": []string{"aws-request-matrix"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"denied","type":"access_denied"}}`)),
			}, nil
		}))
		_, err := adapter.Chat(context.Background(), mantleChatCall())
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", test.status, err)
		}
		if classified.Class != test.class || classified.Retryable != test.retryable {
			t.Fatalf("status %d classified as %s retryable=%v", test.status, classified.Class, classified.Retryable)
		}
		if classified.ProviderRequestID != "aws-request-matrix" {
			t.Fatalf("status %d lost the AWS request id: %q", test.status, classified.ProviderRequestID)
		}
		if strings.Contains(classified.Message, "access_denied") {
			t.Fatalf("status %d carried the provider error type into the message: %q", test.status, classified.Message)
		}
		// The type is kept, out of the sentence and in the identifier field the
		// attempt log prints. A refusal that reaches the operator as a bare
		// status says a request was refused without saying for what.
		if classified.ProviderCode != "access_denied" {
			t.Fatalf("status %d dropped the refusal identifier: %q", test.status, classified.ProviderCode)
		}
	}
}

// The parameter a refusal names is the part an operator acts on, and Mantle
// answers in the OpenAI error shape, so both halves arrive on the wire. The
// sentence is a provider response body and stays inside the error; the code,
// joined to the parameter, is what leaves it. "unsupported_parameter" without
// the field names a category, not something to change.
func TestResponsesAdapterKeepsTheRefusedParameterInTheIdentifier(t *testing.T) {
	adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"X-Amzn-Requestid": []string{"aws-request-param"}},
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"Unsupported parameter: 'max_completion_tokens'.","type":"invalid_request_error","param":"max_completion_tokens","code":"unsupported_parameter"}}`)),
		}, nil
	}))
	_, err := adapter.Chat(context.Background(), mantleChatCall())
	var classified *provider.Error
	if !errors.As(err, &classified) {
		t.Fatalf("refusal was not classified: %v", err)
	}
	if classified.Class != provider.ErrorBadRequest || classified.ProviderRequestID != "aws-request-param" {
		t.Fatalf("unexpected classification: class=%s request_id=%q", classified.Class, classified.ProviderRequestID)
	}
	if classified.ProviderCode != "unsupported_parameter:max_completion_tokens" {
		t.Fatalf("refusal identifier lost the parameter: %q", classified.ProviderCode)
	}
	if strings.Contains(classified.ProviderCode, "Unsupported parameter") {
		t.Fatalf("provider sentence reached the identifier: %q", classified.ProviderCode)
	}
}

// A project the key has no rights to, or one that has been archived, answers
// 403 with a normal error envelope. It has to land in the same non-retryable
// authentication class as a bad key, because the operator response — fix the
// provider record — is the same and falling back would hide it.
func TestResponsesAdapterTreatsAnUnusableProjectAsAuthenticationFailure(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"The project proj_archived is archived.","type":"invalid_request_error"}}`,
		`{"error":{"message":"not authorized to access project","type":"access_denied"}}`,
	} {
		adapter := testAdapter(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("OpenAI-Project") != "" {
				t.Fatalf("the default-project adapter sent a project header: %v", request.Header)
			}
			return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		_, err := adapter.Chat(context.Background(), mantleChatCall())
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("unclassified project failure: %v", err)
		}
		if classified.Class != provider.ErrorAuthentication || classified.Retryable {
			t.Fatalf("project failure classified as %s retryable=%v", classified.Class, classified.Retryable)
		}
	}
}

// An authentication failure mid-stream must not be retryable either, and the
// stream must not be reported as ambiguous when nothing was emitted.
func TestResponsesAdapterStreamAuthenticationFailureIsTerminal(t *testing.T) {
	for _, status := range []int{401, 403} {
		adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"denied"}}`))}, nil
		}))
		emitted := 0
		_, err := adapter.ChatStream(context.Background(), mantleChatCall(), func(semantic.Event) error {
			emitted++
			return nil
		})
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", status, err)
		}
		if classified.Class != provider.ErrorAuthentication || classified.Retryable || classified.Ambiguous {
			t.Fatalf("status %d stream error: %#v", status, classified)
		}
		if emitted != 0 {
			t.Fatalf("status %d emitted %d events before failing", status, emitted)
		}
	}
}

// A body that is not a well-formed Responses document is a malformed provider
// answer, not a successful one with missing fields.
func TestResponsesAdapterRefusesAMalformedBody(t *testing.T) {
	adapter := testAdapter(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","output":[` + strings.Repeat(`{"type":"message","content":[]},`, 1) + `]}`)),
		}, nil
	}))
	if _, err := adapter.Chat(context.Background(), mantleChatCall()); err == nil {
		t.Fatal("a malformed Responses body was accepted")
	}
}

// The oversize rule is a byte ceiling, tested at its own boundary rather than
// by allocating a 16 MiB fixture: readLimited must refuse limit+1 and accept
// exactly limit, or a large provider answer would be silently truncated into a
// plausible-looking short one.
func TestReadLimitedRefusesRatherThanTruncates(t *testing.T) {
	payload, err := readLimited(strings.NewReader(strings.Repeat("a", 64)), 64)
	if err != nil || len(payload) != 64 {
		t.Fatalf("a body exactly at the limit was rejected: %d %v", len(payload), err)
	}
	if _, err := readLimited(strings.NewReader(strings.Repeat("a", 65)), 64); err == nil {
		t.Fatal("a body past the limit was truncated instead of refused")
	}
}

func mantleChatCall() provider.ChatCall {
	return provider.ChatCall{
		RequestID: "request_1", ProviderModel: "model",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "public",
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		},
	}
}
