package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

type probeTransport func(*http.Request) (*http.Response, error)

func (transport probeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func newMiniMaxProbeAdapter(t *testing.T, respond func(*http.Request) (*http.Response, error)) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://api.minimax.io")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("minimax-key"), "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{Transport: probeTransport(respond)},
		ProviderType: string(domain.ProviderMiniMax), CredentialScheme: domain.CredentialBearerStatic,
		MessagesPath: "anthropic/v1/messages", ProfileID: domain.ProfileMiniMaxAnthropicMessages,
		CatalogProbeOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

// A credential-only connection test asks whether the key reaches the host. It
// used to be refused outright on this profile, because the refusal was gated on
// whether targets could be enumerated — a different and much larger question.
// The operator was told to bind a deployment before they could find out whether
// their key worked at all.
func TestMiniMaxConnectionTestReachesTheCatalogRoute(t *testing.T) {
	var seen *http.Request
	adapter := newMiniMaxProbeAdapter(t, func(request *http.Request) (*http.Response, error) {
		seen = request
		// OpenAI-shaped, which is what MiniMax serves on this route.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"MiniMax-M3","object":"model"}]}`)),
		}, nil
	})
	if err := adapter.Probe(context.Background(), ""); err != nil {
		t.Fatalf("credential-only probe: %v", err)
	}
	if seen == nil {
		t.Fatal("the probe made no request")
	}
	if seen.URL.Path != "/v1/models" {
		t.Fatalf("probe addressed %q, want /v1/models", seen.URL.Path)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer minimax-key" {
		t.Fatalf("probe did not carry the key as a bearer token: %q", got)
	}
}

// Probing does not make the list enumerable. Building target descriptors from
// an OpenAI-shaped body would credit every identifier on the account — speech
// and video models included — with chat and streaming on declared evidence.
func TestMiniMaxStillDoesNotEnumerateTargets(t *testing.T) {
	adapter := newMiniMaxProbeAdapter(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("enumeration reached the network; it must be refused locally")
		return nil, nil
	})
	if adapter.InvocationTargetDiscovery().CanEnumerate {
		t.Fatal("this profile claims it can enumerate targets from a catalogue it cannot read as Anthropic's")
	}
	if _, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{}); err == nil {
		t.Fatal("enumeration was allowed")
	}
}

// The probe still has to tell a provider from something else answering on the
// same address over HTTPS.
func TestMiniMaxProbeRefusesANonJSONReply(t *testing.T) {
	adapter := newMiniMaxProbeAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(`<html><body>sign in</body></html>`)),
		}, nil
	})
	if err := adapter.Probe(context.Background(), ""); err == nil {
		t.Fatal("an HTML page read as a healthy provider")
	}
}

// A rejected credential has to surface as a rejected credential.
func TestMiniMaxProbeReportsARefusedKey(t *testing.T) {
	adapter := newMiniMaxProbeAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"login fail"}}`)),
		}, nil
	})
	err := adapter.Probe(context.Background(), "")
	if err == nil {
		t.Fatal("a 401 read as a healthy connection")
	}
	providerErr, ok := err.(*provider.Error)
	if !ok || providerErr.Class != provider.ErrorAuthentication {
		t.Fatalf("a refused key surfaced as %v, want an authentication error", err)
	}
}

// The direct Anthropic profile must keep the refusal it had: it enumerates, and
// nothing about this option applies to it.
func TestCatalogProbeOnlyDoesNotChangeDirectAnthropic(t *testing.T) {
	endpoint, _ := url.Parse("https://api.anthropic.com")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialAnthropicAPIKey, "x-api-key", "", []byte("key"), "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if !adapter.InvocationTargetDiscovery().CanEnumerate {
		t.Fatal("the direct Anthropic profile lost enumeration")
	}
}
