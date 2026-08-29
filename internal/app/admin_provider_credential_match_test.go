package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// A credential is sealed against one provider type, one access surface and one
// endpoint. All three refusals used to arrive as one sentence — "credential type
// or audience does not match provider" — which names neither of the two values
// that disagreed and does not say which of them to change. The console cannot
// explain what it is not told, so each refusal carries its own code and both
// compared values as data.
func TestAdminExplainsWhichCredentialDimensionDidNotMatch(t *testing.T) {
	// The access-surface arm of this needs two Bedrock surfaces that disagree, and
	// only Mantle is offered while the Runtime profiles are withheld.
	if domain.IsWithheldProfile(domain.ProfileBedrockConverseText) {
		t.Skip("Bedrock Runtime is withheld from this build, so no credential can be created on it")
	}
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	credential := func(name, providerType, baseURL string, surface domain.AccessSurface, scheme domain.CredentialScheme, secret string) string {
		t.Helper()
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": name, "type": providerType, "base_url": baseURL, "secret": secret,
			"access_surface": surface, "scheme": scheme,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create credential %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}

	openAI := credential("OpenAI", "openai", "https://api.openai.com", domain.SurfaceOpenAI, domain.CredentialBearerStatic, "provider-secret")
	runtimeCredential := credential(
		"Bedrock Runtime", "bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com",
		domain.SurfaceBedrockRuntime, domain.CredentialAWSSigV4Explicit, awsCredentialForTest("us-east-1"),
	)
	// Sealed to the Agent Runtime endpoint, which is a legal binding for its own
	// surface. Pointing a Runtime provider at that same endpoint leaves the type
	// and the endpoint agreeing and the access surface as the only thing left to
	// refuse. (A Runtime credential bound to the Mantle endpoint would have been
	// the shorter fixture, and is no longer reachable: the write path now runs
	// the credential through the same constructor the load does, and a SigV4
	// document cannot sign for a host outside its own service and region.)
	agentRuntimeCredential := credential(
		"Bedrock Agent Runtime", "bedrock", "https://bedrock-agent-runtime.us-east-1.amazonaws.com",
		domain.SurfaceBedrockAgentRuntime, domain.CredentialAWSSigV4Explicit, awsCredentialForTest("us-east-1"),
	)

	for _, test := range []struct {
		name    string
		payload map[string]any
		code    string
		fields  map[string]string
	}{
		{
			// The likeliest way in: the credential was saved first, and the base
			// URL was edited afterwards.
			name: "an endpoint the credential was not sealed to",
			payload: map[string]any{
				"name": "OpenAI elsewhere", "type": "openai", "base_url": "https://gateway.example",
				"credential_id": openAI, "enabled": true,
			},
			code: "credential_base_url_mismatch",
			fields: map[string]string{
				"credential_base_url": "https://api.openai.com:443",
				"provider_base_url":   "https://gateway.example:443",
			},
		},
		{
			name: "a credential from another provider type",
			payload: map[string]any{
				"name": "Anthropic", "type": "anthropic", "base_url": "https://api.anthropic.com",
				"credential_id": openAI, "enabled": true,
			},
			code: "credential_type_mismatch",
			fields: map[string]string{
				"credential_provider_type": "openai",
				"provider_type":            "anthropic",
			},
		},
		{
			// Same type and same account, different access surface: separate
			// endpoints, separate credential schemes, separate quotas.
			name: "an Agent Runtime credential on the Runtime surface",
			payload: map[string]any{
				"name": "Runtime", "type": "bedrock", "base_url": "https://bedrock-agent-runtime.us-east-1.amazonaws.com",
				"credential_id": agentRuntimeCredential, "enabled": true,
				"access_surface": domain.SurfaceBedrockRuntime, "profile_id": domain.ProfileBedrockConverseText,
				"credential_scheme": domain.CredentialAWSSigV4Explicit,
			},
			code: "credential_surface_mismatch",
			fields: map[string]string{
				"credential_access_surface": string(domain.SurfaceBedrockAgentRuntime),
				"provider_access_surface":   string(domain.SurfaceBedrockRuntime),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performAdminMutation(
				t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", test.payload,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("accepted: status=%d body=%s", response.Code, response.Body.String())
			}
			var refusal map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
				t.Fatal(err)
			}
			if refusal["code"] != test.code {
				t.Fatalf("code=%q want %q: %s", refusal["code"], test.code, response.Body.String())
			}
			for key, want := range test.fields {
				if refusal[key] != want {
					t.Fatalf("%s=%q want %q: %s", key, refusal[key], want, response.Body.String())
				}
			}
			// No secret material rides along with the explanation.
			if _, ok := refusal["secret"]; ok {
				t.Fatalf("refusal carried a secret field: %s", response.Body.String())
			}
		})
	}

	// The same pairing seen from the credential form. A rotation that moves the
	// credential to another endpoint strands the provider still using it there,
	// and the refusal used to be a bare 409 — which the console reads as "someone
	// else edited this, refresh", the one action that cannot help.
	provider := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "OpenAI production", "type": "openai", "base_url": "https://api.openai.com",
		"credential_id": openAI, "enabled": true,
	})
	if provider.Code != http.StatusCreated {
		t.Fatalf("create provider: status=%d body=%s", provider.Code, provider.Body.String())
	}
	rotation := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+openAI, `"1"`,
		map[string]any{
			"name": "OpenAI", "type": "openai", "base_url": "https://gateway.example",
			"secret": "rotated-provider-secret", "current_password": stepUpTestPassword,
		},
	)
	var rotationRefusal map[string]string
	if err := json.Unmarshal(rotation.Body.Bytes(), &rotationRefusal); err != nil {
		t.Fatal(err)
	}
	if rotation.Code != http.StatusConflict || rotationRefusal["code"] != "credential_endpoint_in_use" {
		t.Fatalf("status=%d body=%s", rotation.Code, rotation.Body.String())
	}
	if rotationRefusal["provider_name"] != "OpenAI production" ||
		rotationRefusal["provider_base_url"] != "https://api.openai.com:443" ||
		rotationRefusal["credential_base_url"] != "https://gateway.example:443" {
		t.Fatalf("the refusal did not name what to reconcile: %s", rotation.Body.String())
	}
	if strings.Contains(rotation.Body.String(), "rotated-provider-secret") {
		t.Fatalf("the refusal echoed the submitted secret: %s", rotation.Body.String())
	}

	// The other axis of the same rotation. Reporting it as an endpoint conflict
	// printed the same URL as both sides and sent the operator to correct a base
	// URL that was already right.
	typeRotation := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+openAI, `"1"`,
		map[string]any{
			"name": "OpenAI", "type": "anthropic", "base_url": "https://api.anthropic.com",
			"access_surface": domain.SurfaceAnthropic, "scheme": domain.CredentialAnthropicAPIKey,
			"secret": "rotated-provider-secret", "current_password": stepUpTestPassword,
		},
	)
	var typeRefusal map[string]string
	if err := json.Unmarshal(typeRotation.Body.Bytes(), &typeRefusal); err != nil {
		t.Fatal(err)
	}
	if typeRotation.Code != http.StatusConflict || typeRefusal["code"] != "credential_type_in_use" {
		t.Fatalf("status=%d body=%s", typeRotation.Code, typeRotation.Body.String())
	}
	if typeRefusal["provider_provider_type"] != "openai" || typeRefusal["credential_provider_type"] != "anthropic" ||
		typeRefusal["provider_name"] != "OpenAI production" {
		t.Fatalf("the refusal did not name the type it would change: %s", typeRotation.Body.String())
	}

	// A credential for a Bedrock surface names that surface, but the type check
	// runs first, so a bedrock credential on an openai provider is refused as a
	// type mismatch rather than a surface one.
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "OpenAI with a Bedrock credential", "type": "openai", "base_url": "https://api.openai.com",
		"credential_id": runtimeCredential, "enabled": true,
	})
	var refusal map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusBadRequest || refusal["code"] != "credential_type_mismatch" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
