package gateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	openaiadapter "github.com/akz142857/Halro/internal/provider/openai"
)

type acceptedResponseTransport func(*http.Request) (*http.Response, error)

func (f acceptedResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAcceptedMalformedResponseIsNotRetriedAndSettlesConservatively(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()

	calls := 0
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := openaiadapter.New(endpoint, []byte("provider-key"), &http.Client{
		Transport: acceptedResponseTransport(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"accepted"`)),
				Request:    request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	manifest, ok := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	if !ok {
		t.Fatal("OpenAI Chat profile is not registered")
	}
	profiled, err := provider.NewLegacyAdapterBridge(adapter, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}

	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{
		ID: "accepted_primary", DeploymentID: "accepted_primary", PublicModel: "chat", ProviderModel: "provider-model",
		ProfileID: manifest.ID, AccessSurface: manifest.AccessSurface, Adapter: profiled,
		Capabilities:          provider.Capabilities{Chat: true},
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	fallback := &fakeAdapter{response: openaiapi.ChatCompletionResponse{
		ID: "fallback", Object: "chat.completion", Model: "provider-model",
		Choices: []openaiapi.Choice{{Index: 0}},
	}}
	if err := registry.Register(provider.Target{
		ID: "accepted_fallback", DeploymentID: "accepted_fallback", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(f.service.auth, registry, f.accounting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("accepted malformed response unexpectedly succeeded")
	}
	balance := f.state.Balance(f.project.ID, time.Now().UTC().Format("2006-01-02"), testTimezoneVersion)
	if calls != 1 || fallback.calls != 0 {
		t.Fatalf("accepted response was repeated: primary=%d fallback=%d", calls, fallback.calls)
	}
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD == 0 {
		t.Fatalf("accepted response did not settle conservatively: %#v", balance)
	}
}
