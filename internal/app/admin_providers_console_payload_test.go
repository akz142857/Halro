package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// The console sends both halves of the same fact when it creates an OpenAI
// connection: the union of every enabled capability at the top level, and one
// binding per profile underneath. Bounding every profile's capabilities then
// rejected exactly that shape, because the union contains media capabilities
// that the chat profile's ceiling does not — and it rejected it on the default
// form, before the operator changed anything, since the OpenAI defaults enable
// all six media capabilities.
//
// The existing lifecycle test sends bindings without top-level capabilities,
// which skips the check entirely and so could not see this. These cases send
// what the browser sends.
func TestAdminProviderAcceptsConsoleCapabilityUnionWithBindings(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	chat := map[string]any{
		"chat": true, "streaming": true, "embeddings": true, "tools": true, "vision": true,
		"json_mode": true, "developer_role": true, "reasoning": true, "stream_usage": true,
	}
	media := map[string]any{
		"moderations": true, "images": true, "transcriptions": true,
		"speech": true, "files": true, "batches": true,
	}
	union := map[string]any{}
	for name, value := range chat {
		union[name] = value
	}
	for name, value := range media {
		union[name] = value
	}

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI production", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"profile_id":   domain.ProfileOpenAIChatEmbeddings,
			"capabilities": union,
			"bindings": []map[string]any{
				{"profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": true, "capabilities": chat},
				{"profile_id": domain.ProfileOpenAIMediaResources, "enabled": true, "capabilities": media},
			},
		})
	if response.Code != http.StatusCreated {
		t.Fatalf("console capability union was rejected: status=%d body=%s", response.Code, response.Body.String())
	}

	var instance domain.ProviderInstance
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	// The stored summary comes from the bindings, not from what was posted, which
	// is why checking the posted value against one profile's ceiling was checking
	// something that gets discarded.
	if !instance.Capabilities.Chat || !instance.Capabilities.Embeddings || !instance.Capabilities.Images ||
		!instance.Capabilities.Batches || len(instance.Bindings) != 2 {
		t.Fatalf("bindings did not produce the union summary: %#v", instance.Capabilities)
	}
	for _, binding := range instance.Bindings {
		switch binding.ProfileID {
		case domain.ProfileOpenAIChatEmbeddings:
			if !binding.Capabilities.Chat || binding.Capabilities.Images {
				t.Fatalf("chat binding kept the wrong capabilities: %#v", binding.Capabilities)
			}
		case domain.ProfileOpenAIMediaResources:
			if !binding.Capabilities.Images || binding.Capabilities.Chat {
				t.Fatalf("media binding kept the wrong capabilities: %#v", binding.Capabilities)
			}
		default:
			t.Fatalf("unexpected binding profile %q", binding.ProfileID)
		}
	}
}

// Skipping the top-level check when bindings are present must not become a way
// past the per-binding ceiling: that bound is the one that decides what routing
// may offer this connection for.
func TestAdminProviderStillBoundsEachBindingWithConsolePayload(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI forged", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"profile_id": domain.ProfileOpenAIChatEmbeddings,
			// A union wide enough to look legitimate, over a binding that claims an
			// operation its profile has no adapter for.
			"capabilities": map[string]any{"chat": true, "streaming": true, "images": true},
			"bindings": []map[string]any{
				{"profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": true,
					"capabilities": map[string]any{"chat": true, "streaming": true, "images": true}},
			},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("binding claiming an unavailable capability was accepted: status=%d body=%s",
			response.Code, response.Body.String())
	}
}

// Without bindings the top-level field is what the connection is built from, so
// it keeps its bound.
func TestAdminProviderBoundsTopLevelCapabilitiesWithoutBindings(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Gemini widened", "type": "gemini",
			"base_url":      "https://generativelanguage.googleapis.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true, "streaming": true, "tools": true},
		})
	if response.Code == http.StatusCreated {
		t.Fatalf("capabilities beyond the profile were accepted without bindings: %s", response.Body.String())
	}
}

func providerConsolePayloadFixture(t *testing.T) (*Runtime, string, *http.Cookie, string) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cookie, csrf := loginAdminForTest(t, runtime)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "provider-secret-canary",
		},
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("credential create status=%d body=%s", response.Code, response.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(response.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	return runtime, credential.ID, cookie, csrf
}
