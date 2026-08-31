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

// Adapter construction is the one registration step with no test of its own —
// it needs a credential and a client, so it fails at runtime rather than in the
// suite. This exercises it against a fake transport, which is what the seam in
// newProviderBindingAdapterWithClient exists for.
//
// What it pins is the part a reader cannot check by eye: that each MiniMax
// profile addresses its own route. All three share one host and one key, so a
// branch pointed at the wrong path would still authenticate, still return 200
// against a real account, and answer from a surface the operator never chose.
func TestMiniMaxWiringAddressesOneRoutePerProfile(t *testing.T) {
	endpoint, _ := url.Parse("https://api.minimax.io")
	for _, test := range []struct {
		profile domain.ProviderProfileID
		path    string
	}{
		{domain.ProfileMiniMaxAnthropicMessages, "/anthropic/v1/messages"},
		{domain.ProfileMiniMaxChat, "/v1/chat/completions"},
		{domain.ProfileMiniMaxResponses, "/v1/responses"},
	} {
		var seen *http.Request
		client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
			seen = request
			body := `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"MiniMax-M3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			switch {
			case strings.HasSuffix(request.URL.Path, "/messages"):
				body = `{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
			case strings.HasSuffix(request.URL.Path, "/responses"):
				body = `{"id":"resp_1","object":"response","created_at":1,"model":"MiniMax-M3","status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}
		instance := domain.ProviderInstance{
			ID: "prov_1", Name: "minimax", Type: domain.ProviderMiniMax,
			BaseURL: endpoint.String(), CredentialID: "cred_1",
			AccessSurface: domain.SurfaceMiniMax, ProfileID: test.profile,
			CredentialScheme: domain.CredentialBearerStatic,
		}
		binding := domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, test.profile), ProviderID: instance.ID,
			ProfileID: test.profile, AccessSurface: domain.SurfaceMiniMax,
			CredentialScheme: domain.CredentialBearerStatic, Enabled: true,
			Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderMiniMax, test.profile),
		}
		adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("minimax-key"), client)
		if err != nil {
			t.Fatalf("%s: %v", test.profile, err)
		}
		if err := exerciseMiniMaxAdapter(adapter, test.profile); err != nil {
			t.Fatalf("%s: %v", test.profile, err)
		}
		if seen == nil {
			t.Fatalf("%s made no request, so the route cannot be observed", test.profile)
		}
		if seen.URL.Path != test.path {
			t.Fatalf("%s addressed %q, want %q", test.profile, seen.URL.Path, test.path)
		}
		// One key, three faces. The bearer form is the one MiniMax accepts
		// everywhere; the Anthropic route additionally takes x-api-key, and
		// Authorization wins when both are present.
		if got := seen.Header.Get("Authorization"); got != "Bearer minimax-key" {
			t.Fatalf("%s did not send the key as a bearer token: %q", test.profile, got)
		}
		adapter.Close()
	}
}

// exerciseMiniMaxAdapter drives one generation through whichever entry point the
// profile binds, because that is what decides the route: Chat and the Anthropic
// face generate through Chat, and the Responses face binds a semantic primitive
// and refuses Chat outright.
func exerciseMiniMaxAdapter(adapter provider.Adapter, profileID domain.ProviderProfileID) error {
	if profileID == domain.ProfileMiniMaxResponses {
		generator, ok := adapter.(provider.SemanticGenerator)
		if !ok {
			return errors.New("the Responses profile did not build a semantic generator, so nothing can reach /v1/responses")
		}
		_, err := generator.GenerateSemantic(context.Background(), provider.GenerateCall{
			RequestID: "req_1", ProviderModel: "MiniMax-M3",
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
		RequestID: "req_1", ProviderModel: "MiniMax-M3",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "MiniMax-M3",
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		},
	})
	return err
}
