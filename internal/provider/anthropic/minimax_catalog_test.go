package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// Transcribed from a real MiniMax account, 2026-09-01. Every decision in this
// file rests on this body, and it had to be captured rather than imagined: the
// profile shipped without enumeration on the reasoning that this list would
// credit the account's speech and video models with chat. There are none in it.
const minimaxRealModelCatalog = `{"object":"list","data":[` +
	`{"id":"MiniMax-M3","object":"model","created":1780272000,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.7","object":"model","created":1773799200,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.7-highspeed","object":"model","created":1773799200,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.5","object":"model","created":1770948000,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.5-highspeed","object":"model","created":1770948000,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.1","object":"model","created":1766455200,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2.1-highspeed","object":"model","created":1766455200,"owned_by":"minimax"},` +
	`{"id":"MiniMax-M2","object":"model","created":1761530400,"owned_by":"minimax"}]}`

type probeTransport func(*http.Request) (*http.Response, error)

func (transport probeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func jsonReply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newMiniMaxCatalogAdapter(t *testing.T, respond func(*http.Request) (*http.Response, error)) *Adapter {
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
		CatalogShape: CatalogOpenAI,
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
	adapter := newMiniMaxCatalogAdapter(t, func(request *http.Request) (*http.Response, error) {
		seen = request
		return jsonReply(http.StatusOK, minimaxRealModelCatalog), nil
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

// The profile enumerates. It did not, on the reasoning that this list carries an
// identifier and nothing else, so building targets from it would credit every id
// on the account — speech and video models included — with chat and streaming on
// declared evidence. The list was read on 2026-09-01 and holds eight chat models
// and nothing else; and the reasoning had the wrong subject anyway, because what
// turns an identifier into a capability claim is MapCapabilityClaims, which the
// test below holds silent here.
//
// What the operator gets back is the Refresh control: a model MiniMax publishes
// after this release is reachable by asking the account, not by shipping a new
// binary with a longer bundled list.
func TestMiniMaxEnumeratesFromTheOpenAIShapedCatalogue(t *testing.T) {
	var requests int
	var seen *http.Request
	adapter := newMiniMaxCatalogAdapter(t, func(request *http.Request) (*http.Response, error) {
		requests++
		seen = request
		return jsonReply(http.StatusOK, minimaxRealModelCatalog), nil
	})
	if !adapter.InvocationTargetDiscovery().CanEnumerate {
		t.Fatal("the profile still reports it cannot enumerate")
	}
	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(targets) != 8 {
		t.Fatalf("enumerated %d targets, want the 8 the account listed", len(targets))
	}
	// One request, and none of Anthropic's pagination. This shape defines
	// neither limit nor after_id, and sending them would be asking a question in
	// the wrong language and reading whatever came back as an answer.
	if requests != 1 {
		t.Errorf("enumeration made %d requests; the OpenAI shape has no pagination to follow", requests)
	}
	if query := seen.URL.RawQuery; query != "" {
		t.Errorf("enumeration sent %q; the bare route is what was measured", query)
	}
	byID := map[string]domain.InvocationTargetDescriptor{}
	for _, target := range targets {
		byID[target.TargetID] = target
	}
	m3, ok := byID["MiniMax-M3"]
	if !ok {
		t.Fatalf("MiniMax-M3 was listed and not enumerated; got %v", byID)
	}
	if m3.OwnedBy != "minimax" || m3.CanonicalModelRef != "MiniMax-M3" || m3.TargetKind != domain.TargetModelID {
		t.Errorf("descriptor lost the list's own fields: %#v", m3)
	}
	// Availability is what the account's list is evidence for. Everything else
	// stays empty because this shape says nothing about it: no context window, no
	// output ceiling, no capability flags, and no statement about retirement.
	if m3.Availability != domain.AvailabilityAvailable {
		t.Errorf("a model the account listed is not available: %q", m3.Availability)
	}
	if m3.MetadataSource != domain.MetadataSourceNone {
		t.Errorf("a list carrying no metadata reported source %q", m3.MetadataSource)
	}
	if m3.Lifecycle != domain.TargetLifecycleUnknown {
		t.Errorf("lifecycle %q claims retirement knowledge this list does not carry", m3.Lifecycle)
	}
	if m3.Metadata.MaxContextTokens != 0 || m3.Metadata.MaxOutputTokens != 0 || len(m3.Metadata.SupportedOperations) != 0 {
		t.Errorf("metadata was invented from an identifier: %#v", m3.Metadata)
	}
}

// The half of the old reasoning that was right, kept as a guard. Enumeration
// says who exists; it must not say what they do. The Anthropic mapper claims
// chat and streaming from the fact that an Anthropic Models list describes
// Messages models, and this is not an Anthropic Models list.
//
// The consequence is the safe one for the case the account measured did not
// show: were an account to list a speech model, it would arrive as a target with
// no capability evidence — selectable, and not deployable as chat until someone
// declares that it is.
func TestMiniMaxTargetsCarryNoProviderMetadataClaims(t *testing.T) {
	adapter := newMiniMaxCatalogAdapter(t, func(*http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, minimaxRealModelCatalog), nil
	})
	scope := domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: "MiniMax-M3",
		BindingID: "binding", ProfileID: domain.ProfileMiniMaxAnthropicMessages,
	}
	target := domain.InvocationTargetDescriptor{TargetID: "MiniMax-M3", TargetKind: domain.TargetModelID}
	if claims := adapter.MapCapabilityClaims(target, scope, testInstant()); len(claims) != 0 {
		t.Fatalf("an identifier from an OpenAI-shaped list produced %d capability claims: %#v", len(claims), claims)
	}
}

// The mapper still answers for the surface its premise holds on. Without this,
// silencing it for MiniMax could silence it everywhere and nothing would say so.
func TestDirectAnthropicStillClaimsChatFromItsOwnCatalogue(t *testing.T) {
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
	scope := domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: "claude-sonnet-4-5",
		BindingID: "binding", ProfileID: domain.ProfileAnthropicMessages,
	}
	target := domain.InvocationTargetDescriptor{TargetID: "claude-sonnet-4-5", TargetKind: domain.TargetModelID}
	claims := adapter.MapCapabilityClaims(target, scope, testInstant())
	if len(claims) == 0 {
		t.Fatal("the direct Anthropic profile stopped claiming chat from its own Models API")
	}
}

// Bedrock Mantle's Anthropic profile reaches the same mapper and is deliberately
// untouched: nothing measured here says anything about Mantle, and only one
// catalog entry covers that profile, so silencing the mapper there would move
// every other Mantle model from resolved to unknown.
func TestBedrockMantleAnthropicKeepsItsClaims(t *testing.T) {
	endpoint, _ := url.Parse("https://bedrock.example.com")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("key"), "Authorization")
	if err != nil {
		t.Skipf("Bedrock Mantle credential scheme unavailable in this build: %v", err)
	}
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{},
		ProviderType: string(domain.ProviderBedrock), MessagesPath: "anthropic/v1/messages",
		CredentialScheme: domain.CredentialBedrockAPIKey,
		ProfileID:        domain.ProfileBedrockMantleAnthropicMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	scope := domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: "anthropic.claude-sonnet-4-5",
		BindingID: "binding", ProfileID: domain.ProfileBedrockMantleAnthropicMessages,
	}
	target := domain.InvocationTargetDescriptor{TargetID: "anthropic.claude-sonnet-4-5", TargetKind: domain.TargetModelID}
	if claims := adapter.MapCapabilityClaims(target, scope, testInstant()); len(claims) == 0 {
		t.Fatal("Bedrock Mantle lost claims it had; this change was scoped to MiniMax")
	}
}

// The probe still has to tell a provider from something else answering on the
// same address over HTTPS.
func TestMiniMaxProbeRefusesANonJSONReply(t *testing.T) {
	adapter := newMiniMaxCatalogAdapter(t, func(*http.Request) (*http.Response, error) {
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

// And now it can be stricter than that. The check was "the reply is a JSON
// object" while the shape was only inferred, because asserting a guessed member
// name would have turned a wrong guess into a failed credential test. The reply
// has been read, it carries `data`, so a JSON object without one is no longer
// accepted as a healthy catalogue.
func TestMiniMaxProbeRefusesAJSONObjectThatIsNotAModelList(t *testing.T) {
	adapter := newMiniMaxCatalogAdapter(t, func(*http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, `{"status":"ok","message":"welcome"}`), nil
	})
	if err := adapter.Probe(context.Background(), ""); err == nil {
		t.Fatal("a JSON object carrying no model list read as a healthy catalogue")
	}
}

// An empty account is a real answer and must not read as a broken one.
func TestMiniMaxProbeAcceptsAnEmptyList(t *testing.T) {
	adapter := newMiniMaxCatalogAdapter(t, func(*http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, `{"object":"list","data":[]}`), nil
	})
	if err := adapter.Probe(context.Background(), ""); err != nil {
		t.Fatalf("an account entitled to no models read as a failed connection: %v", err)
	}
}

// A rejected credential has to surface as a rejected credential.
func TestMiniMaxProbeReportsARefusedKey(t *testing.T) {
	adapter := newMiniMaxCatalogAdapter(t, func(*http.Request) (*http.Response, error) {
		return jsonReply(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"login fail"}}`), nil
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

// testInstant keeps the claim assertions off the wall clock.
func testInstant() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }
