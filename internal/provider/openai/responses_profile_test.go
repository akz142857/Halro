package openai

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
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// The Responses profile only works if the primitive resolves to the semantic
// path, and that resolution has one specific way to fail silently.
//
// The adapter reaches the registry inside a profile wrapper that embeds the
// Adapter interface. Method sets do not promote through an embedded interface,
// so asserting the wrapper against a semantic interface always fails — which
// would leave the binding unresolvable, the target dropped from every route,
// and an operator told their connection serves nothing, with no error naming a
// cause. Resolving through the real wrapper is the only way to know it does not
// happen.
func TestResponsesProfileResolvesToTheSemanticPrimitive(t *testing.T) {
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(unreachableRoundTrip)})
	manifest, ok := provider.BuiltinProfile(domain.ProfileOpenAIResponses)
	if !ok {
		t.Fatal("the Responses profile has no manifest")
	}
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest,
		domain.EvidenceForCapabilities(domain.DefaultProviderCapabilitiesForProfile(
			domain.ProviderOpenAI, domain.ProfileOpenAIResponses), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := bridge.Operations().Resolve(provider.OperationChat)
	if !ok {
		t.Fatal("the Responses profile binds a primitive nothing can resolve")
	}
	if resolved.ProviderPrimitive() != provider.PrimitiveOpenAIResponses {
		t.Fatalf("primitive=%q", resolved.ProviderPrimitive())
	}

	// A stream primitive is deliberately unbound: this profile answers one
	// request with one response, and a streaming request is meant to be routed
	// away rather than served by an adapter improvising chunks.
	if _, streams := bridge.Operations().Resolve(provider.OperationChatStream); streams {
		t.Fatal("the Responses profile resolved a stream primitive it does not bind")
	}
}

// What the caller asked for has to reach the wire as a Responses body, on the
// Responses path, with the provider-executed tool still in it. Going through
// the Chat wire on the way — which is what every other OpenAI profile does —
// would drop the tool, because the Chat request has no member for it.
func TestResponsesProfileSendsWebSearchToTheResponsesEndpoint(t *testing.T) {
	var seen *http.Request
	var body []byte
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request
		body, _ = io.ReadAll(request.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_1","object":"response","model":"gpt-test","status":"completed",
				"output":[
					{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"who won"}},
					{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[
						{"type":"output_text","text":"Halro won.","annotations":[
							{"type":"url_citation","url":"https://example.test/a","title":"A","start_index":0,"end_index":5}
						]}
					]}
				]}`)),
		}, nil
	})}
	adapter := newResponsesAdapter(t, client)

	request := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
		Source:         semantic.Source{ProfileID: string(domain.ProfileOpenAIResponses), ProfileRevision: 1},
		RequestedModel: "public",
		Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{
			{Kind: semantic.ContentText, Text: "who won?"},
		}}},
		Tools: []semantic.Tool{{Name: semantic.ProviderToolWebSearch, Execution: semantic.ToolExecutionProvider}},
	}
	request.Requirements = request.DeriveRequirements()

	result, err := adapter.GenerateSemantic(context.Background(), provider.GenerateCall{
		RequestID: "req_1", ProviderModel: "gpt-test", Request: request,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.URL.String() != "https://api.openai.com/v1/responses" {
		t.Fatalf("request did not reach the Responses endpoint: %+v", seen)
	}
	var wire struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
		Store *bool `json:"store"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Type != "web_search" {
		t.Fatalf("the provider-executed tool did not survive to the wire: %s", body)
	}
	// Halro holds no provider-side conversation state, so the request says so
	// rather than relying on the endpoint's default.
	if wire.Store == nil || *wire.Store {
		t.Fatalf("store was not sent as false: %s", body)
	}

	content := result.Choices[0].Message.Content
	if len(content) != 2 || content[0].Kind != semantic.ContentProviderToolCall || content[0].Text != "who won" {
		t.Fatalf("the search the upstream ran was not reported: %#v", content)
	}
	if len(content[1].Citations) != 1 || content[1].Citations[0].URL != "https://example.test/a" {
		t.Fatalf("the answer came back without its sources: %#v", content[1])
	}
}

// The same adapter type serves the chat profile, so the surface it addresses
// has to come from the profile rather than from whatever a request asks for.
func TestChatProfileAdapterRefusesTheResponsesSurface(t *testing.T) {
	adapter := newMantleChatAdapter(t, &http.Client{Transport: roundTripFunc(unreachableRoundTrip)})
	_, err := adapter.GenerateSemantic(context.Background(), provider.GenerateCall{
		RequestID: "req_1", ProviderModel: "gpt-test",
	})
	if err == nil {
		t.Fatal("a chat-profile connection reached an endpoint its operator never chose")
	}
}

// unreachableRoundTrip fails the moment a test that should never reach the
// network reaches it.
func unreachableRoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("this test must not reach the network")
}

func newResponsesAdapter(t *testing.T, client *http.Client) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://api.openai.com")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("openai-key"), "api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: client,
		ProviderType: string(domain.ProviderOpenAI), Responses: true,
		Capabilities: provider.Capabilities(domain.DefaultProviderCapabilitiesForProfile(
			domain.ProviderOpenAI, domain.ProfileOpenAIResponses)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}
