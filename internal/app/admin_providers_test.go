package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/provider"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

func TestAdminProviderCredentialRouteLifecycle(t *testing.T) {
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
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "provider-secret-canary",
		},
	)
	if credentialResponse.Code != http.StatusCreated ||
		strings.Contains(credentialResponse.Body.String(), "provider-secret-canary") ||
		strings.Contains(credentialResponse.Body.String(), "ciphertext") {
		t.Fatalf("credential create status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	if !credential.SecretConfigured || credential.KeyVersion != 1 {
		t.Fatalf("unexpected credential view: %#v", credential)
	}

	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "max_concurrency": int64(3), "enabled": true,
			"capabilities": map[string]any{
				"chat": true, "streaming": true, "embeddings": true, "tools": true,
				"vision": true, "json_mode": true, "developer_role": true,
				"reasoning": true, "stream_usage": true,
				"max_context_tokens": int64(128), "max_output_tokens": int64(64),
			},
		},
	)
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider create status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	var instance struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	blockedCredentialDelete := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/credentials/"+credential.ID, `"1"`, nil,
	)
	if blockedCredentialDelete.Code != http.StatusConflict {
		t.Fatalf("credential delete with provider reference status=%d body=%s",
			blockedCredentialDelete.Code, blockedCredentialDelete.Body.String())
	}
	rejectedDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "Invalid limits", "provider_id": instance.ID, "provider_model": "gpt-test",
			"priority": 10, "weight": 1, "enabled": true,
			"capabilities": map[string]any{
				"chat": true, "streaming": true,
				"max_context_tokens": int64(256), "max_output_tokens": int64(64),
			},
		},
	)
	if rejectedDeployment.Code != http.StatusBadRequest {
		t.Fatalf("deployment exceeded provider capability status=%d body=%s", rejectedDeployment.Code, rejectedDeployment.Body.String())
	}

	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "GPT test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"input_micros_per_million": int64(1_000_000), "output_micros_per_million": int64(2_000_000),
			"max_concurrency": int64(2), "priority": 10, "weight": 1, "enabled": true,
		},
	)
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment create status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment struct {
		ID       string `json:"id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}

	routeResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/routes", "",
		map[string]any{
			"public_model": "chat", "deployment_id": deployment.ID,
			"priority": 10, "strategy": "ordered", "enabled": true,
		},
	)
	if routeResponse.Code != http.StatusCreated {
		t.Fatalf("route create status=%d body=%s", routeResponse.Code, routeResponse.Body.String())
	}
	target, ok := runtime.providers.Resolve("chat")
	if !ok || target.ProviderID != instance.ID || target.ProviderModel != "gpt-test" ||
		target.MaxConcurrency != 3 || target.DeploymentID != deployment.ID || target.DeploymentConcurrency != 2 ||
		!target.Capabilities.DeveloperRole || !target.Capabilities.Reasoning ||
		target.Capabilities.MaxContextTokens != 128 || target.Capabilities.MaxOutputTokens != 64 {
		t.Fatalf("route was not hot activated: %#v", target)
	}

	rotationResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+credential.ID, `"1"`,
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "rotated-provider-secret-canary",
		},
	)
	if rotationResponse.Code != http.StatusOK ||
		strings.Contains(rotationResponse.Body.String(), "rotated-provider-secret-canary") {
		t.Fatalf("credential rotation status=%d body=%s", rotationResponse.Code, rotationResponse.Body.String())
	}
	if err := json.Unmarshal(rotationResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	if credential.KeyVersion != 2 {
		t.Fatalf("credential rotation version=%d", credential.KeyVersion)
	}
	if target, ok = runtime.providers.Resolve("chat"); !ok || target.ProviderID != instance.ID {
		t.Fatal("route disappeared after credential rotation")
	}

	blockedProviderDelete := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/providers/"+instance.ID, `"1"`, nil,
	)
	if blockedProviderDelete.Code != http.StatusConflict {
		t.Fatalf("provider delete with active deployment status=%d body=%s",
			blockedProviderDelete.Code, blockedProviderDelete.Body.String())
	}
	blockedDeploymentDelete := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/deployments/"+deployment.ID, `"1"`, nil,
	)
	if blockedDeploymentDelete.Code != http.StatusConflict {
		t.Fatalf("deployment delete with active route status=%d body=%s",
			blockedDeploymentDelete.Code, blockedDeploymentDelete.Body.String())
	}

	var route struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(routeResponse.Body.Bytes(), &route); err != nil {
		t.Fatal(err)
	}
	probe := &adminProbeAdapter{}
	nextRegistry := provider.NewRegistry()
	if err := nextRegistry.RegisterAdapter(instance.ID, probe); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(nextRegistry)
	routeTest := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/routes/"+route.ID+"/test", "", nil,
	)
	if routeTest.Code != http.StatusOK || probe.probes != 1 || probe.model != "gpt-test" {
		t.Fatalf("route test status=%d probes=%d model=%q body=%s",
			routeTest.Code, probe.probes, probe.model, routeTest.Body.String())
	}
	deleteRoute := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/routes/"+route.ID, `"1"`, nil,
	)
	if deleteRoute.Code != http.StatusNoContent {
		t.Fatalf("route delete status=%d body=%s", deleteRoute.Code, deleteRoute.Body.String())
	}
	if _, ok := runtime.providers.Resolve("chat"); ok {
		t.Fatal("deleted route remained active")
	}
	deleteDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/deployments/"+deployment.ID, `"1"`, nil,
	)
	if deleteDeployment.Code != http.StatusNoContent {
		t.Fatalf("deployment delete status=%d body=%s", deleteDeployment.Code, deleteDeployment.Body.String())
	}
	deleteProvider := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/providers/"+instance.ID, `"1"`, nil,
	)
	if deleteProvider.Code != http.StatusNoContent {
		t.Fatalf("provider delete status=%d body=%s", deleteProvider.Code, deleteProvider.Body.String())
	}
	deleteCredential := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/credentials/"+credential.ID,
		`"`+strconv.FormatUint(credential.Revision, 10)+`"`, nil,
	)
	if deleteCredential.Code != http.StatusNoContent {
		t.Fatalf("credential delete status=%d body=%s", deleteCredential.Code, deleteCredential.Body.String())
	}
	if _, err := runtime.store.GetCredential(context.Background(), credential.ID); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("deleted credential remained in store: %v", err)
	}
}

type adminProbeAdapter struct {
	canaryAdapter
	probes int
	model  string
}

func (a *adminProbeAdapter) Probe(_ context.Context, model string) error {
	a.probes++
	a.model = model
	return nil
}

func TestAdminProviderRejectsCredentialAudienceMismatch(t *testing.T) {
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
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "Bound credential", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "secret",
		},
	)
	var credential credentialView
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "Wrong audience", "type": "openai",
			"base_url": "https://example.com", "credential_id": credential.ID, "enabled": true,
		},
	)
	if providerResponse.Code != http.StatusBadRequest {
		t.Fatalf("audience mismatch status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
}

func TestAdminBedrockProviderHotLoadsConverseCapabilities(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	secret := `{"access_key_id":"AKIDEXAMPLE12345678","secret_access_key":"test-secret-access-key-value","session_token":"session-token","region":"us-east-1"}`
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": "Bedrock test", "type": "bedrock",
			"base_url": "https://bedrock-runtime.us-east-1.amazonaws.com", "secret": secret,
		})
	if credentialResponse.Code != http.StatusCreated || strings.Contains(credentialResponse.Body.String(), "AKIDEXAMPLE") {
		t.Fatalf("credential create status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Bedrock", "type": "bedrock",
			"base_url":      "https://bedrock-runtime.us-east-1.amazonaws.com",
			"credential_id": credential.ID, "enabled": true,
		})
	if providerResponse.Code != http.StatusCreated || strings.Contains(providerResponse.Body.String(), "AKIDEXAMPLE") {
		t.Fatalf("provider create status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	var instance struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
			"name": "Claude", "provider_id": instance.ID, "provider_model": "anthropic.claude-test-v1:0",
			"priority": 10, "weight": 1, "enabled": true,
		})
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment create status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	routeResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/routes", "", map[string]any{
			"public_model": "bedrock-chat", "deployment_id": deployment.ID,
			"priority": 10, "strategy": "ordered", "enabled": true,
		})
	if routeResponse.Code != http.StatusCreated {
		t.Fatalf("route create status=%d body=%s", routeResponse.Code, routeResponse.Body.String())
	}
	target, ok := runtime.providers.Resolve("bedrock-chat")
	if !ok || target.ProviderID != instance.ID || !target.Capabilities.Chat ||
		!target.Capabilities.Streaming || !target.Capabilities.StreamUsage ||
		target.Capabilities.Embeddings || target.Capabilities.Tools || target.Capabilities.Vision {
		t.Fatalf("unexpected Bedrock target: %#v", target)
	}
}

func performAdminMutation(
	t *testing.T,
	runtime *Runtime,
	cookie *http.Cookie,
	csrf string,
	method string,
	path string,
	ifMatch string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	request := adminRequest(t, method, path, body)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}
