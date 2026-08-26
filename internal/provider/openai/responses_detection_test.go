package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// Capability detection speaks Chat, and every probe reaches the adapter through
// the profile wrapper, which does not override it. On this profile that question
// is only answerable by the endpoint the profile addresses: a probe that reached
// /v1/chat/completions would record verified evidence for a surface this profile
// never calls, so a key scoped to only one of the two passes detection green and
// then fails the first real request, after the reservation.
func TestCapabilityDetectionOnTheResponsesProfileProbesTheResponsesEndpoint(t *testing.T) {
	var seen []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.URL.String())
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"resp_1","created_at":1,"model":"gpt-5.6","status":"completed","output":[` +
					`{"id":"msg_1","type":"message","status":"completed","role":"assistant",` +
					`"content":[{"type":"output_text","text":"ok"}]}]}`)),
			Request: request,
		}, nil
	})}
	manifest, ok := provider.BuiltinProfile(domain.ProfileOpenAIResponses)
	if !ok {
		t.Fatal("the Responses profile has no manifest")
	}
	bridge, err := provider.NewLegacyAdapterBridge(newResponsesAdapter(t, client), manifest, nil)
	if err != nil {
		t.Fatal(err)
	}

	result := bridge.DetectCapability(context.Background(),
		provider.ModelCapabilityDetectionTarget{
			ProviderModel: "gpt-5.6", BindingID: "binding", ProfileID: domain.ProfileOpenAIResponses,
		},
		provider.CapabilityProbe{Capability: "chat", Kind: "minimal_chat", MaxOutputTokens: 16},
	)

	if len(seen) != 1 || seen[0] != "https://api.openai.com/v1/responses" {
		t.Fatalf("the probe addressed %v", seen)
	}
	if result.Status != domain.ProbeSupported || result.Evidence != domain.EvidenceVerified {
		t.Fatalf("a probe that reached the profile's own surface was not believed: %#v", result)
	}
}

// The profile binds no stream primitive, so nothing should arrive here. Falling
// through to the Chat endpoint would stream from a surface this profile does not
// address.
func TestResponsesProfileAdapterRefusesToStream(t *testing.T) {
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(unreachableRoundTrip)})

	_, err := adapter.ChatStream(context.Background(),
		provider.ChatCall{RequestID: "req", ProviderModel: "gpt-5.6", Request: openaiapi.ChatCompletionRequest{
			Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		}},
		func(semantic.Event) error { return nil },
	)
	var providerErr *provider.Error
	if !errors.As(err, &providerErr) || providerErr.Class != provider.ErrorBadRequest {
		t.Fatalf("err=%#v", err)
	}
}
