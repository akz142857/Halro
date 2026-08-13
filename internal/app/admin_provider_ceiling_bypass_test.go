package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// PutProvider bounds the capabilities field against the profile's ceiling and
// then, when the payload carries bindings, replaces that field with the summary
// of them. The ceiling only ever looked at the field, and the binding-level
// check only at the immutable profiles, so the bindings array was a way to
// declare a capability the profile does not serve and have it become the
// connection's declaration — with evidence recorded as though an operator had
// been entitled to state it.
//
// The two halves of this test are the same capability set arriving by the two
// routes. They have to agree; when they did not, the console's checkbox list
// was the only thing keeping a direct API caller inside the ceiling.
func TestProviderBindingsCannotDeclareBeyondTheProfileCeiling(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Bedrock Runtime", "type": "bedrock",
		"base_url":       "https://bedrock-runtime.us-east-1.amazonaws.com",
		"secret":         `{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY","region":"us-east-1"}`,
		"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
	})
	if credentialResponse.Code != http.StatusCreated {
		t.Fatalf("create credential: status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	// Converse text serves chat, streaming and stream usage. Tools and vision are
	// the two the adapter rejects before Provider I/O, so declaring them buys the
	// caller nothing but a route that offers an operation it answers with an
	// error — and, on the profiles with no per-field rejection behind them, a
	// field that really is sent upstream.
	beyond := map[string]any{
		"chat": true, "streaming": true, "stream_usage": true, "tools": true, "vision": true,
	}

	viaBindings := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Bedrock via bindings", "type": "bedrock",
		"base_url":      "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "enabled": true,
		"bindings": []map[string]any{{
			"profile_id": domain.ProfileBedrockConverseText, "enabled": true, "capabilities": beyond,
		}},
	})
	if viaBindings.Code != http.StatusBadRequest {
		t.Fatalf("the bindings array carried a capability past the ceiling: status=%d body=%s",
			viaBindings.Code, viaBindings.Body.String())
	}

	viaCapabilities := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Bedrock via capabilities", "type": "bedrock",
		"base_url":      "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "enabled": true, "capabilities": beyond,
	})
	if viaCapabilities.Code != http.StatusBadRequest {
		t.Fatalf("the capabilities field carried a capability past the ceiling: status=%d body=%s",
			viaCapabilities.Code, viaCapabilities.Body.String())
	}

	// Narrowing through the same array stays legal, so the fix is a ceiling and
	// not a fixed value: an operator who knows this connection must not stream
	// still has to be able to say so.
	within := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Bedrock within its ceiling", "type": "bedrock",
		"base_url":      "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "enabled": true,
		"bindings": []map[string]any{{
			"profile_id": domain.ProfileBedrockConverseText, "enabled": true,
			"capabilities": map[string]any{"chat": true},
		}},
	})
	if within.Code != http.StatusCreated {
		t.Fatalf("a binding narrower than its ceiling was refused: status=%d body=%s",
			within.Code, within.Body.String())
	}
}

// A credential is sealed against one access surface and one scheme as much as
// against a type and an endpoint. The registry compares all four when it loads,
// and a binding that disagrees with its credential fails the whole load rather
// than withholding that one provider — so a rotation that moved a credential
// onto a surface no binding uses took the data plane down with it, and stayed
// down across restarts until someone rotated it back. The write path compared
// only type and audience, which are exactly the two this rotation leaves alone.
func TestRotatingCredentialCannotChangeAccessSurfaceWhileInUse(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	const endpoint = "https://bedrock-runtime.us-east-1.amazonaws.com"
	const secret = `{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY","region":"us-east-1"}`

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Bedrock Runtime", "type": "bedrock", "base_url": endpoint, "secret": secret,
		"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
	})
	if credentialResponse.Code != http.StatusCreated {
		t.Fatalf("create credential: status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	provider := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Bedrock Converse", "type": "bedrock", "base_url": endpoint,
		"credential_id": credential.ID, "enabled": true,
	})
	if provider.Code != http.StatusCreated {
		t.Fatalf("create provider: status=%d body=%s", provider.Code, provider.Body.String())
	}

	// Same type, same endpoint, same scheme — only the surface moves, and the
	// agent-runtime surface accepts that scheme, so nothing before this check
	// has a reason to object.
	rotation := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+credential.ID, `"1"`,
		map[string]any{
			"name": "Bedrock Runtime", "type": "bedrock", "base_url": endpoint,
			"secret": secret, "current_password": stepUpTestPassword,
			"access_surface": domain.SurfaceBedrockAgentRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
		},
	)
	if rotation.Code != http.StatusConflict {
		t.Fatalf("the rotation was accepted: status=%d body=%s", rotation.Code, rotation.Body.String())
	}
	var refusal map[string]string
	if err := json.Unmarshal(rotation.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal["code"] != "credential_surface_in_use" {
		t.Fatalf("code=%q: %s", refusal["code"], rotation.Body.String())
	}
	// The refusal has to name both sides, or the operator cannot tell which one
	// is the mistake — the credential they are editing, or the provider holding
	// it to the old surface.
	if refusal["provider_name"] != "Bedrock Converse" ||
		refusal["provider_access_surface"] != string(domain.SurfaceBedrockRuntime) ||
		refusal["credential_access_surface"] != string(domain.SurfaceBedrockAgentRuntime) {
		t.Fatalf("the refusal did not name what to reconcile: %s", rotation.Body.String())
	}
}
