package app

import (
	"context"
	"encoding/json"
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

// Step 6 of docs/contracts/adding-a-platform.md — building the adapter from a
// stored connection — was the one registration with no guard. The contract said
// so in as many words: it needs a credential and a client, so it fails at
// runtime instead, with "provider profile is not implemented", on the first
// attempt to build a connection.
//
// It does not need a real credential or a real client. It needs a plausible one
// of each, which is what this supplies. What it proves is narrow and exactly the
// gap: every profile an operator can reach has a branch that builds something.
// Now that construction is a table rather than a switch, it can be enumerated,
// and both directions are worth holding.
//
// A profile with no row is the defect this file was written for. An orphan row —
// a builder for a profile the domain table does not have — is the newly visible
// one: a switch could carry a dead case forever, because nothing could list the
// cases. It matters because a removed profile whose construction survives reads
// as still supported to anyone who greps for it.
func TestAdapterBuilderTableCoversExactlyTheRegisteredProfiles(t *testing.T) {
	registered := map[domain.ProviderProfileID]bool{}
	for _, profile := range domain.AllProviderProfiles() {
		registered[profile.ID] = true
		if _, ok := adapterBuilders[profile.ID]; !ok {
			t.Errorf("%s is in the profile table and has no adapter construction row", profile.ID)
		}
	}
	for profileID := range adapterBuilders {
		if !registered[profileID] {
			t.Errorf("%s has an adapter construction row and is not a registered profile", profileID)
		}
	}
}

// Every row has to be able to run all three of its stages. A nil authorize or
// build would panic at request time rather than fail, and a table is easy to
// add a half-filled row to.
func TestEveryAdapterBuilderRowIsComplete(t *testing.T) {
	for profileID, builder := range adapterBuilders {
		if builder.authorize == nil {
			t.Errorf("%s has no authorize stage", profileID)
		}
		if builder.build == nil {
			t.Errorf("%s has no build stage", profileID)
		}
	}
}

func TestEveryReachableProfileBuildsAnAdapter(t *testing.T) {
	// One fake secret per credential scheme. The shapes matter only where a
	// constructor parses them.
	secrets := map[domain.CredentialScheme][]byte{
		domain.CredentialBearerStatic:    []byte("test-key"),
		domain.CredentialAnthropicAPIKey: []byte("test-key"),
		domain.CredentialAzureAPIKey:     []byte("test-key"),
		domain.CredentialGoogleAPIKey:    []byte("test-key"),
		domain.CredentialBedrockAPIKey:   []byte("test-key"),
	}
	for _, profile := range domain.AllProviderProfiles() {
		// Withheld profiles are refused by every write path, so no connection can
		// exist on one and no adapter is ever built for it. Guarding them would
		// assert something the product does not offer.
		if profile.Withheld {
			continue
		}
		secret, ok := secrets[profile.CredentialScheme]
		if !ok {
			t.Errorf("%s uses credential scheme %s and this guard has no fixture for it; add one rather than skipping, or the profile goes unchecked",
				profile.ID, profile.CredentialScheme)
			continue
		}
		rawEndpoint := domain.ResolveBaseURL(profile.ID, "us-east-1")
		if rawEndpoint == "" {
			// Azure and the OpenAI-compatible profile deliberately prefill no
			// endpoint, because a wrong one is worse than none. They still have to
			// build against the address an operator supplies.
			rawEndpoint = "https://provider.example.invalid"
		}
		endpoint, err := url.Parse(rawEndpoint)
		if err != nil {
			t.Errorf("%s: endpoint %q: %v", profile.ID, rawEndpoint, err)
			continue
		}
		instance := domain.ProviderInstance{
			ID: "prov_1", Name: "n", Type: profile.Type, BaseURL: endpoint.String(),
			CredentialID: "cred_1", AccessSurface: profile.AccessSurface,
			ProfileID: profile.ID, CredentialScheme: profile.CredentialScheme,
		}
		if profile.Type == domain.ProviderAzureOpenAI {
			instance.APIVersion = "2024-10-21"
		}
		binding := domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, profile.ID), ProviderID: instance.ID,
			ProfileID: profile.ID, AccessSurface: profile.AccessSurface,
			CredentialScheme: profile.CredentialScheme, Enabled: true,
			Capabilities: profile.Defaults,
		}
		adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, secret, &http.Client{})
		if err != nil {
			t.Errorf("%s has no adapter construction branch: %v", profile.ID, err)
			continue
		}
		if adapter == nil {
			t.Errorf("%s built a nil adapter", profile.ID)
			continue
		}
		if adapter.Type() != string(profile.Type) {
			t.Errorf("%s built an adapter reporting type %q, want %q", profile.ID, adapter.Type(), profile.Type)
		}
		adapter.Close()
	}
}

// Building an adapter is not the same as being able to call it, and the gap
// between them is where a registration can still be missing.
//
// It drives one generation per reachable profile through the same dispatch
// production uses and asserts the one thing that distinguishes a wiring defect
// from an ordinary upstream failure: **a request reached the transport**. What
// comes back does not matter — a decode failure against a generic body is a
// healthy adapter talking to a fake server. Never reaching the network is not.
//
// What it catches, established by removing each registration and watching it
// fail: a profile with no construction branch, and a primitive declared in
// semanticGenerationPrimitives whose adapter is not a SemanticGenerator.
//
// What it does not catch, established the same way — by trying: the reverse
// omission, a semantic primitive left out of that map. That was the case this
// test was written for, and driving it proved the premise wrong. Nothing
// refuses; Resolve falls back to the legacy Chat path and the OpenAI adapter's
// Responses branch translates and addresses the same endpoint anyway. The cost
// is a lossier translation, not a failure, so no assertion about reachability
// can see it. It is recorded in docs/contracts/adding-a-platform.md as a step
// with no guard rather than left looking covered.
func TestEveryReachableProfileReachesTheNetworkWhenCalled(t *testing.T) {
	secrets := map[domain.CredentialScheme][]byte{
		domain.CredentialBearerStatic:    []byte("test-key"),
		domain.CredentialAnthropicAPIKey: []byte("test-key"),
		domain.CredentialAzureAPIKey:     []byte("test-key"),
		domain.CredentialGoogleAPIKey:    []byte("test-key"),
		domain.CredentialBedrockAPIKey:   []byte("test-key"),
	}
	for _, profile := range domain.AllProviderProfiles() {
		if profile.Withheld {
			continue
		}
		manifest, ok := provider.BuiltinProfile(profile.ID)
		if !ok {
			t.Errorf("%s has no profile manifest", profile.ID)
			continue
		}
		var chatPrimitive provider.Primitive
		for _, binding := range manifest.PrimitiveBindings {
			if binding.LegacyOperation == provider.OperationChat {
				chatPrimitive = binding.Primitive
			}
		}
		if chatPrimitive == "" {
			// A media, embedding or rerank profile. Its own operations are covered
			// by the construction guard above; there is no chat call to drive.
			continue
		}
		rawEndpoint := domain.ResolveBaseURL(profile.ID, "us-east-1")
		if rawEndpoint == "" {
			rawEndpoint = "https://provider.example.invalid"
		}
		endpoint, _ := url.Parse(rawEndpoint)
		reached := false
		client := &http.Client{Transport: recordingTransport(func(*http.Request) (*http.Response, error) {
			reached = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})}
		instance := domain.ProviderInstance{
			ID: "prov_1", Name: "n", Type: profile.Type, BaseURL: endpoint.String(),
			CredentialID: "cred_1", AccessSurface: profile.AccessSurface,
			ProfileID: profile.ID, CredentialScheme: profile.CredentialScheme,
		}
		if profile.Type == domain.ProviderAzureOpenAI {
			instance.APIVersion = "2024-10-21"
		}
		binding := domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, profile.ID), ProviderID: instance.ID,
			ProfileID: profile.ID, AccessSurface: profile.AccessSurface,
			CredentialScheme: profile.CredentialScheme, Enabled: true,
			Capabilities: profile.Defaults,
		}
		adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, secrets[profile.CredentialScheme], client)
		if err != nil {
			t.Errorf("%s: %v", profile.ID, err)
			continue
		}
		// Dispatch the way the registry does: a primitive declared semantic is
		// invoked through GenerateSemantic, everything else through Chat. Getting
		// this pairing wrong is the defect being guarded against, so the test
		// reads the same declaration production reads rather than guessing.
		if provider.IsSemanticGenerationPrimitive(chatPrimitive) {
			generator, ok := adapter.(provider.SemanticGenerator)
			if !ok {
				t.Errorf("%s binds %s as a semantic generation primitive and its adapter is not a SemanticGenerator", profile.ID, chatPrimitive)
				adapter.Close()
				continue
			}
			_, _ = generator.GenerateSemantic(context.Background(), provider.GenerateCall{
				RequestID: "guard", ProviderModel: "model-test",
				Request: semantic.GenerateRequest{
					Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
					Source:         semantic.Source{ProfileID: "guard", ProfileRevision: 1},
					RequestedModel: "public",
					Messages:       []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
				},
			})
		} else {
			_, _ = adapter.Chat(context.Background(), provider.ChatCall{
				RequestID: "guard", ProviderModel: "model-test",
				Request: openaiapi.ChatCompletionRequest{
					Model:    "model-test",
					Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
				},
			})
		}
		adapter.Close()
		if !reached {
			t.Errorf("%s refused its own chat call before reaching the network; something in its registration is missing", profile.ID)
		}
	}
}
