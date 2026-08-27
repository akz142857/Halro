package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// What the console sends for an OpenAI connection is one flat capability set,
// and the two bindings it needs come back from the server.
//
// This payload is where the whole change started: the console used to send the
// split itself alongside the union, and bounding the union against the anchor
// profile's ceiling rejected it — on the default form, before the operator
// changed anything, because the OpenAI defaults enable all six media
// capabilities and no single profile serves both halves.
func TestAdminProviderSplitsTheConsoleCapabilitySetAcrossProfiles(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI production", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{
				"chat": true, "streaming": true, "embeddings": true, "tools": true, "vision": true,
				"json_object": true, "structured_outputs": true, "developer_role": true, "reasoning": true, "stream_usage": true,
				"moderations": true, "images": true, "transcriptions": true,
				"speech": true, "files": true, "batches": true,
			},
		})
	if response.Code != http.StatusCreated {
		t.Fatalf("the console capability set was rejected: status=%d body=%s", response.Code, response.Body.String())
	}

	var instance domain.ProviderInstance
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if !instance.Capabilities.Chat || !instance.Capabilities.Embeddings || !instance.Capabilities.Images ||
		!instance.Capabilities.Batches || len(instance.Bindings) != 2 {
		t.Fatalf("the flat set did not become the union summary: %#v", instance.Capabilities)
	}
	// The anchor stays the connection's primary profile, so the projection every
	// v1 reader still uses names the profile the operator's type implies.
	if instance.ProfileID != domain.ProfileOpenAIChatEmbeddings {
		t.Fatalf("primary profile is %q", instance.ProfileID)
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

// A token limit belongs to the profile that declares one — only Titan Embed
// does — so a connection whose profiles declare none has nowhere to hold it.
//
// Accepting it was worse than refusing: the stored summary reports the loosest
// bound across the connection's bindings, so a value accepted here came back on
// the next read and, applied again, landed on every other binding. A Bedrock
// chat binding ended up capped at Titan's 8192 by an edit that changed nothing.
func TestATokenLimitNoProfileHoldsIsRefusedByName(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI bounded", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true, "max_context_tokens": int64(128)},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a bound nothing holds was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
	var refusal struct {
		Code         string `json:"code"`
		Capabilities string `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "capabilities_limit_unavailable" || refusal.Capabilities != "max_context_tokens" {
		t.Fatalf("the refusal did not name the limit: %s", response.Body.String())
	}
}

// A PUT that says nothing about capabilities changes nothing about them.
//
// It used to resolve to the anchor profile's defaults, so renaming an OpenAI
// connection dropped its media binding and re-widened the chat one — a 200, with
// the connection quietly serving something else afterwards.
func TestAPutThatOmitsCapabilitiesKeepsTheConnectionIntact(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI production", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			// Narrower than the profile default on purpose: the point is that an
			// unrelated edit does not put back what the operator turned off.
			"capabilities": map[string]any{"chat": true, "images": true},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var before domain.ProviderInstance
	if err := json.Unmarshal(created.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if len(before.Bindings) != 2 {
		t.Fatalf("expected a chat and a media binding: %#v", before.Bindings)
	}

	renamed := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/providers/"+before.ID, `"`+strconv.FormatUint(before.Revision, 10)+`"`,
		map[string]any{
			"name": "OpenAI renamed", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
		})
	if renamed.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	var after domain.ProviderInstance
	if err := json.Unmarshal(renamed.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Bindings) != 2 {
		t.Fatalf("the rename dropped a binding: %#v", after.Bindings)
	}
	if after.Capabilities.Streaming || after.Capabilities.Embeddings {
		t.Fatalf("the rename re-widened the capability set: %#v", after.Capabilities)
	}
	if !after.Capabilities.Chat || !after.Capabilities.Images {
		t.Fatalf("the rename lost what the connection served: %#v", after.Capabilities)
	}
}

// A bound the operator narrowed survives an edit that does not mention it.
//
// The console has no field for the token limits and sends zero for both, so
// "unspecified" is the normal case rather than an exotic one. Reading zero as
// "restore the profile's own bound" widened a Titan Embed binding from a chosen
// 4096 back to 8192 on the next enable/disable — removing a routing guard
// nobody asked to remove, and doing it silently.
func TestANarrowedBoundSurvivesAnEditThatDoesNotMentionIt(t *testing.T) {
	runtime, cookie, csrf, credentialID := bedrockRuntimeFixture(t)

	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Bedrock narrowed", "type": "bedrock",
			"base_url":      "https://bedrock-runtime.us-east-1.amazonaws.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true, "embeddings": true, "max_context_tokens": int64(4096)},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var before domain.ProviderInstance
	if err := json.Unmarshal(created.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if titan := bindingFor(before, domain.ProfileBedrockInvokeTitanEmbedV2); titan.Capabilities.MaxContextTokens != 4096 {
		t.Fatalf("the narrowed bound was not stored: %#v", titan.Capabilities)
	}

	// What the console sends on any later edit: the same capability set with both
	// limits at zero.
	toggled := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/providers/"+before.ID, `"`+strconv.FormatUint(before.Revision, 10)+`"`,
		map[string]any{
			"name": before.Name, "type": before.Type, "base_url": before.BaseURL,
			"credential_id": before.CredentialID, "enabled": true,
			"capabilities": map[string]any{
				"chat": true, "embeddings": true,
				"max_context_tokens": int64(0), "max_output_tokens": int64(0),
			},
		})
	if toggled.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", toggled.Code, toggled.Body.String())
	}
	var after domain.ProviderInstance
	if err := json.Unmarshal(toggled.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if titan := bindingFor(after, domain.ProfileBedrockInvokeTitanEmbedV2); titan.Capabilities.MaxContextTokens != 4096 {
		t.Fatalf("an edit that said nothing about the bound widened it: %#v", titan.Capabilities)
	}
	// And naming a value still changes it.
	widened := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/providers/"+after.ID, `"`+strconv.FormatUint(after.Revision, 10)+`"`,
		map[string]any{
			"name": after.Name, "type": after.Type, "base_url": after.BaseURL,
			"credential_id": after.CredentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true, "embeddings": true, "max_context_tokens": int64(8192)},
		})
	if widened.Code != http.StatusOK {
		t.Fatalf("widen status=%d body=%s", widened.Code, widened.Body.String())
	}
	var final domain.ProviderInstance
	if err := json.Unmarshal(widened.Body.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if titan := bindingFor(final, domain.ProfileBedrockInvokeTitanEmbedV2); titan.Capabilities.MaxContextTokens != 8192 {
		t.Fatalf("a named bound was ignored: %#v", titan.Capabilities)
	}
}

func bindingFor(instance domain.ProviderInstance, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	for _, binding := range instance.Bindings {
		if binding.ProfileID == profile {
			return binding
		}
	}
	return domain.ProviderProfileBinding{}
}

func bedrockRuntimeFixture(t *testing.T) (*Runtime, *http.Cookie, string, string) {
	t.Helper()
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": "Bedrock Runtime", "type": "bedrock",
			"base_url":       "https://bedrock-runtime.us-east-1.amazonaws.com",
			"secret":         `{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY","region":"us-east-1"}`,
			"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
		})
	if response.Code != http.StatusCreated {
		t.Fatalf("credential status=%d body=%s", response.Code, response.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(response.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	return runtime, cookie, csrf, credential.ID
}

// Naming an implementation that ends up serving nothing is refused rather than
// answered with a different one. The connection's profile projection comes from
// the first binding, so accepting it would return a profile_id the caller never
// asked for.
func TestAnImplementationThatServesNothingIsRefused(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI media only", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"profile_id":   domain.ProfileOpenAIChatEmbeddings,
			"capabilities": map[string]any{"images": true},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "serves none of the enabled capabilities") {
		t.Fatalf("the refusal did not say why: %s", response.Body.String())
	}

	// Without naming one, the same capability set is fine and lands on the
	// profile that serves it.
	unnamed := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI media only", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"images": true},
		})
	if unnamed.Code != http.StatusCreated {
		t.Fatalf("unnamed status=%d body=%s", unnamed.Code, unnamed.Body.String())
	}
	var instance domain.ProviderInstance
	if err := json.Unmarshal(unnamed.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.ProfileID != domain.ProfileOpenAIMediaResources {
		t.Fatalf("primary profile is %q", instance.ProfileID)
	}
}

// The bindings array is not accepted alongside the flat set, and not accepted
// instead of it. Keeping it would have left two ways to say the same thing, one
// of which the server has to reconcile against the other — which is how the
// union-versus-single-profile refusal happened in the first place.
func TestAdminProviderRefusesABindingsArray(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI production", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true},
			"bindings": []map[string]any{
				{"profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": true,
					"capabilities": map[string]any{"chat": true}},
			},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a bindings array was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}

// The flat set is what the connection is built from, so it is bounded by what
// the connection's profiles can serve.
func TestAdminProviderBoundsTopLevelCapabilities(t *testing.T) {
	runtime, credentialID, cookie, csrf := providerConsolePayloadFixture(t)

	// Rerank exists, and no profile an OpenAI credential can reach serves it.
	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI widened", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credentialID, "enabled": true,
			"capabilities": map[string]any{"chat": true, "rerank": true},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a capability no profile serves was accepted: status=%d body=%s",
			response.Code, response.Body.String())
	}
	var refusal struct {
		Code         string `json:"code"`
		Capabilities string `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "capabilities_unservable" || refusal.Capabilities != "rerank" {
		t.Fatalf("the refusal did not name rerank: %s", response.Body.String())
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
