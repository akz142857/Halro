package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// The same shape as the MiniMax wiring test and for the same reason: adapter
// construction needs a credential and a client, so it fails at runtime rather
// than in the suite unless something drives it.
//
// What it pins is that each Kimi profile addresses its own route. Both share one
// host and one key, so a branch pointed at the wrong path would still
// authenticate, still return 200 against a real account, and answer from a
// surface the operator never chose.
//
// It also pins the header. Kimi's authentication section names
// Authorization: Bearer and nothing else — there is no x-api-key alternative
// the way MiniMax's Anthropic route has one.
func TestKimiWiringAddressesOneRoutePerProfile(t *testing.T) {
	endpoint, _ := url.Parse("https://api.moonshot.ai")
	for _, test := range []struct {
		profile domain.ProviderProfileID
		path    string
	}{
		{domain.ProfileKimiChat, "/v1/chat/completions"},
		{domain.ProfileKimiAnthropicMessages, "/anthropic/v1/messages"},
		{domain.ProfileKimiResponses, "/v1/responses"},
	} {
		var seen *http.Request
		client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
			seen = request
			body := `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			switch {
			case strings.HasSuffix(request.URL.Path, "/messages"):
				// Kimi's own shape, measured: the thinking span is nested under
				// output_tokens_details rather than sitting flat on usage.
				body = `{"id":"msg_1","type":"message","role":"assistant","model":"kimi-k3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":3,"output_tokens_details":{"thinking_tokens":2}}}`
			case strings.HasSuffix(request.URL.Path, "/responses"):
				body = `{"id":"resp_1","object":"response","created_at":1,"model":"kimi-k3","status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}
		instance := domain.ProviderInstance{
			ID: "prov_1", Name: "kimi", Type: domain.ProviderKimi,
			BaseURL: endpoint.String(), CredentialID: "cred_1",
			AccessSurface: domain.SurfaceKimi, ProfileID: test.profile,
			CredentialScheme: domain.CredentialBearerStatic,
		}
		binding := domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, test.profile), ProviderID: instance.ID,
			ProfileID: test.profile, AccessSurface: domain.SurfaceKimi,
			CredentialScheme: domain.CredentialBearerStatic, Enabled: true,
			Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderKimi, test.profile),
		}
		adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("kimi-key"), client)
		if err != nil {
			t.Fatalf("%s: %v", test.profile, err)
		}
		if err := exerciseKimiAdapter(adapter, test.profile); err != nil {
			t.Fatalf("%s: %v", test.profile, err)
		}
		if seen == nil {
			t.Fatalf("%s made no request, so the route cannot be observed", test.profile)
		}
		if seen.URL.Path != test.path {
			t.Fatalf("%s addressed %q, want %q", test.profile, seen.URL.Path, test.path)
		}
		if got := seen.Header.Get("Authorization"); got != "Bearer kimi-key" {
			t.Fatalf("%s did not send the key as a bearer token: %q", test.profile, got)
		}
		adapter.Close()
	}
}

// TestKimiChatWiringRendersTheKimiDialect is the half a route assertion cannot
// make. Both checks here fail if the adapter marshals the OpenAI request
// straight out — which is what it does for every upstream that needs no dialect,
// so dropping the Kimi branch is a one-line regression a route test would not
// see.
//
//   - A request naming temperature must be refused before anything is sent.
//     Kimi has no such member and answers a value it did not expect with an
//     error, after the reservation is taken.
//   - kimi-k2.6 must be sent thinking, not the top-level reasoning_effort. This
//     is the expensive direction: k2.6 ignores the member it does not read,
//     reasons at its default, and bills for it, so nothing anywhere reports that
//     the caller's request was not honoured.
func TestKimiChatWiringRendersTheKimiDialect(t *testing.T) {
	endpoint, _ := url.Parse("https://api.moonshot.ai")
	var body []byte
	var calls int
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Body != nil {
			body, _ = io.ReadAll(request.Body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"kimi-k2.6","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
		}, nil
	})}
	instance := domain.ProviderInstance{
		ID: "prov_1", Name: "kimi", Type: domain.ProviderKimi,
		BaseURL: endpoint.String(), CredentialID: "cred_1",
		AccessSurface: domain.SurfaceKimi, ProfileID: domain.ProfileKimiChat,
		CredentialScheme: domain.CredentialBearerStatic,
	}
	binding := domain.ProviderProfileBinding{
		ID: domain.DefaultProviderProfileBindingID(instance.ID, domain.ProfileKimiChat), ProviderID: instance.ID,
		ProfileID: domain.ProfileKimiChat, AccessSurface: domain.SurfaceKimi,
		CredentialScheme: domain.CredentialBearerStatic, Enabled: true,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderKimi, domain.ProfileKimiChat),
	}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("kimi-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	sampling := 0.5
	sampled := openaiapi.ChatCompletionRequest{
		Model:       "kimi-k3",
		Messages:    []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Temperature: &sampling,
	}
	if _, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "kimi-k3", Request: sampled,
	}); err == nil {
		t.Error("a request naming temperature reached Kimi; it has no member for one")
	}
	if calls != 0 {
		t.Error("the refused request was still sent upstream")
	}

	if _, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_2", ProviderModel: "kimi-k2.6",
		Request: openaiapi.ChatCompletionRequest{
			Model:           "kimi-k2.6",
			Messages:        []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			ReasoningEffort: "high",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the rendered body did not parse: %v", err)
	}
	if _, present := sent["reasoning_effort"]; present {
		t.Error("kimi-k2.6 was sent the top-level reasoning_effort, which it does not read")
	}
	if got := string(sent["thinking"]); got != `{"type":"enabled"}` {
		t.Errorf("kimi-k2.6 was sent thinking %s, want {\"type\":\"enabled\"}", got)
	}
}

// exerciseKimiAdapter drives one generation through whichever entry point the
// profile binds, because that is what decides the route: Chat generates through
// Chat, and the Responses face binds a semantic primitive and refuses Chat
// outright.
func exerciseKimiAdapter(adapter provider.Adapter, profileID domain.ProviderProfileID) error {
	if profileID == domain.ProfileKimiResponses {
		generator, ok := adapter.(provider.SemanticGenerator)
		if !ok {
			return errors.New("the Responses profile did not build a semantic generator, so nothing can reach /v1/responses")
		}
		_, err := generator.GenerateSemantic(context.Background(), provider.GenerateCall{
			RequestID: "req_1", ProviderModel: "kimi-k3",
			Request: semantic.GenerateRequest{
				Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
				Source:         semantic.Source{ProfileID: "test", ProfileRevision: 1},
				RequestedModel: "public",
				Messages:       []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
			},
		})
		return err
	}
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "kimi-k3",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "kimi-k3",
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		},
	})
	return err
}

// Kimi's Anthropic face reports the thinking span under output_tokens_details,
// which Anthropic itself does not. A decoder reading the flat member alone
// reports every Kimi reasoning span as zero, and reasoning tokens are what the
// usage view splits an output charge by.
func TestKimiAnthropicWiringReadsTheNestedThinkingTokens(t *testing.T) {
	endpoint, _ := url.Parse("https://api.moonshot.ai")
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","model":"kimi-k3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":4,"output_tokens":58,"output_tokens_details":{"thinking_tokens":42}}}`)),
		}, nil
	})}
	instance := domain.ProviderInstance{
		ID: "prov_1", Name: "kimi", Type: domain.ProviderKimi,
		BaseURL: endpoint.String(), CredentialID: "cred_1",
		AccessSurface: domain.SurfaceKimi, ProfileID: domain.ProfileKimiAnthropicMessages,
		CredentialScheme: domain.CredentialBearerStatic,
	}
	binding := domain.ProviderProfileBinding{
		ID: domain.DefaultProviderProfileBindingID(instance.ID, domain.ProfileKimiAnthropicMessages), ProviderID: instance.ID,
		ProfileID: domain.ProfileKimiAnthropicMessages, AccessSurface: domain.SurfaceKimi,
		CredentialScheme: domain.CredentialBearerStatic, Enabled: true,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderKimi, domain.ProfileKimiAnthropicMessages),
	}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("kimi-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	response, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "kimi-k3",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "kimi-k3",
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil {
		t.Fatal("no usage was reported")
	}
	if got := response.Usage.ReasoningTokens(); got != 42 {
		t.Errorf("reasoning tokens read as %d, want 42 — the nested spelling was not read", got)
	}
	// The prompt span still has to be recovered from Anthropic's cache-exclusive
	// convention, which Kimi follows: 10 + 4.
	if response.Usage.PromptTokens != 14 {
		t.Errorf("prompt tokens read as %d, want 14", response.Usage.PromptTokens)
	}
}
