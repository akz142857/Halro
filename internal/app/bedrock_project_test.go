package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// A Bedrock Project is provider-level because a profile binding cannot repeat
// within one provider and so cannot carry a dimension an operator needs several
// of. Two projects are two providers — and the point of that shape is that they
// may share one credential without the projects bleeding into each other.
func TestTwoMantleProvidersShareACredentialWithoutSharingAProject(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	endpoint := "https://bedrock-mantle.us-east-1.api.aws"

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Mantle API key", "type": "bedrock", "base_url": endpoint, "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	create := func(name, projectID string) domain.ProviderInstance {
		t.Helper()
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": name, "type": "bedrock", "base_url": endpoint,
			"credential_id": credential.ID, "enabled": true,
			"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleAnthropicMessages,
			"credential_scheme":  domain.CredentialBedrockAPIKey,
			"bedrock_project_id": projectID,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		var instance domain.ProviderInstance
		if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
			t.Fatal(err)
		}
		return instance
	}

	alpha := create("project alpha", "proj_alpha1")
	beta := create("project beta", "proj_beta22")
	shared := create("account default", "")

	if alpha.BedrockProjectID != "proj_alpha1" || beta.BedrockProjectID != "proj_beta22" {
		t.Fatalf("projects did not persist: %q %q", alpha.BedrockProjectID, beta.BedrockProjectID)
	}
	if shared.BedrockProjectID != "" {
		t.Fatalf("an omitted project became %q rather than the account default", shared.BedrockProjectID)
	}
	if alpha.CredentialID != beta.CredentialID || alpha.CredentialID != shared.CredentialID {
		t.Fatal("the three providers did not reuse one credential, so this proves nothing about isolation")
	}

	// Survives a restart as three distinct providers, not one shared setting.
	runtime.Close()
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored := map[string]string{}
	instances, err := reopened.store.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range instances {
		if instance.AccessSurface == domain.SurfaceBedrockMantle {
			stored[instance.ID] = instance.BedrockProjectID
		}
	}
	if stored[alpha.ID] != "proj_alpha1" || stored[beta.ID] != "proj_beta22" || stored[shared.ID] != "" {
		t.Fatalf("projects did not survive a restart intact: %#v", stored)
	}
}

func TestAdminRefusesUnusableBedrockProjectIdentifiers(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	mantleEndpoint := "https://bedrock-mantle.us-east-1.api.aws"

	mantleCredential := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Mantle API key", "type": "bedrock", "base_url": mantleEndpoint, "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	var mantle struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(mantleCredential.Body.Bytes(), &mantle); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ name, projectID string }{
		{"a Claude Platform workspace id", "wrkspc_01AbCdEf23GhIj"},
		{"an unprefixed id", "5d5ykleja6cwpirysbb7"},
		{"a project id with punctuation", "proj_abc-123"},
	} {
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": test.name, "type": "bedrock", "base_url": mantleEndpoint,
			"credential_id": mantle.ID, "enabled": true,
			"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleOpenAIChat,
			"credential_scheme":  domain.CredentialBedrockAPIKey,
			"bedrock_project_id": test.projectID,
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s was accepted: status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}

	// `default` is the account default's own ID; it is accepted and stored as
	// the empty default rather than kept as a second spelling.
	normalized := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "explicit default", "type": "bedrock", "base_url": mantleEndpoint,
		"credential_id": mantle.ID, "enabled": true,
		"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleOpenAIChat,
		"credential_scheme":  domain.CredentialBedrockAPIKey,
		"bedrock_project_id": "default",
	})
	if normalized.Code != http.StatusCreated {
		t.Fatalf("the literal default project was refused: status=%d body=%s", normalized.Code, normalized.Body.String())
	}
	var instance domain.ProviderInstance
	if err := json.Unmarshal(normalized.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.BedrockProjectID != "" {
		t.Fatalf("the literal default was stored as %q rather than normalised away", instance.BedrockProjectID)
	}
}

// The field is Mantle's. On Bedrock Runtime it would be stored and never sent.
func TestAdminRefusesABedrockProjectOnTheRuntimeSurface(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	runtimeEndpoint := "https://bedrock-runtime.us-east-1.amazonaws.com"

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Runtime credential", "type": "bedrock", "base_url": runtimeEndpoint,
		"secret":         `{"access_key_id":"AKIAEXAMPLE","secret_access_key":"secret","session_token":"token"}`,
		"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
	})
	if credentialResponse.Code != http.StatusCreated {
		t.Fatalf("runtime credential: status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "runtime with a project", "type": "bedrock", "base_url": runtimeEndpoint,
		"credential_id": credential.ID, "enabled": true,
		"access_surface": domain.SurfaceBedrockRuntime, "profile_id": domain.ProfileBedrockConverseText,
		"credential_scheme":  domain.CredentialAWSSigV4Explicit,
		"bedrock_project_id": "proj_abc123",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Bedrock Mantle access surface") {
		t.Fatalf("a Runtime provider kept a project id: status=%d body=%s", response.Code, response.Body.String())
	}
}

// The composition root decides which profile renders the project as
// OpenAI-Project and which renders it as anthropic-workspace. That mapping had
// no test: deleting it from all three branches left every other test passing,
// so the feature could have shipped storing a project it never sent.
func TestProviderWiringRendersTheBedrockProjectPerProtocol(t *testing.T) {
	endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
	for _, test := range []struct {
		profile domain.ProviderProfileID
		header  string
	}{
		{domain.ProfileBedrockMantleOpenAIChat, "OpenAI-Project"},
		{domain.ProfileBedrockMantleOpenAIResponses, "OpenAI-Project"},
		{domain.ProfileBedrockMantleAnthropicMessages, "anthropic-workspace"},
	} {
		var seen *http.Request
		client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
			seen = request
			body := `{"object":"list","data":[]}`
			if strings.HasSuffix(request.URL.Path, "/messages") {
				body = `{"id":"msg_1","type":"message","role":"assistant","model":"model-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}
		instance := domain.ProviderInstance{
			ID: "prov_1", Name: "mantle", Type: domain.ProviderBedrock,
			BaseURL: endpoint.String(), CredentialID: "cred_1",
			AccessSurface: domain.SurfaceBedrockMantle, ProfileID: test.profile,
			CredentialScheme: domain.CredentialBedrockAPIKey,
			BedrockProjectID: "proj_abc123",
		}
		binding := domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, test.profile), ProviderID: instance.ID,
			ProfileID: test.profile, AccessSurface: domain.SurfaceBedrockMantle,
			CredentialScheme: domain.CredentialBedrockAPIKey, Enabled: true,
			Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, test.profile),
		}
		adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("bedrock-key"), client)
		if err != nil {
			t.Fatalf("%s: %v", test.profile, err)
		}
		prober, ok := adapter.(provider.Prober)
		if !ok {
			t.Fatalf("%s adapter cannot probe, so this wiring cannot be observed", test.profile)
		}
		if err := prober.Probe(context.Background(), "model-test"); err != nil {
			t.Fatalf("%s probe: %v", test.profile, err)
		}
		if got := seen.Header.Get(test.header); got != "proj_abc123" {
			t.Fatalf("%s did not address its project through %s: %q", test.profile, test.header, got)
		}
		adapter.Close()
	}
}

type recordingTransport func(*http.Request) (*http.Response, error)

func (transport recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
