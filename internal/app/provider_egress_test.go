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
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safetransport"
)

func TestProviderProbeFailureKeepsOnlyBoundedProxyDiagnostics(t *testing.T) {
	failure := describeProbeFailure(provider.NewTransportError("probe failed", &safetransport.ProxyError{
		ProxyID: "corp-egress", Stage: safetransport.ProxyStageStatus, StatusCode: 407,
	}))
	if failure.ProxyStage != string(safetransport.ProxyStageStatus) || failure.ProxyStatus != 407 {
		t.Fatalf("proxy diagnostics=%#v", failure)
	}
	result := map[string]any{}
	failure.addTo(result)
	if result["proxy_stage"] != string(safetransport.ProxyStageStatus) || result["proxy_status"] != 407 {
		t.Fatalf("safe response=%#v", result)
	}
	for key := range result {
		if key == "proxy_id" || key == "endpoint" || key == "reason_phrase" {
			t.Fatalf("unsafe proxy diagnostic %q escaped", key)
		}
	}
	if rendered, err := json.Marshal(result); err != nil || strings.Contains(string(rendered), "corp-egress") {
		t.Fatalf("proxy ID leaked through safe diagnostics: %s err=%v", rendered, err)
	}
}

func TestProviderEgressFingerprintIgnoresDisplayOnlyRevision(t *testing.T) {
	description := providerEgressDescription{
		ID: "corp-egress", Name: "Corporate egress", Kind: domain.ProviderEgressProxyKindHTTPConnect,
		Endpoint: "https://proxy.example.com:8443", EndpointScheme: "https",
		EndpointHost: "proxy.example.com", EndpointPort: 8443, Authenticated: true, Revision: 1,
	}
	initial := providerEgressFingerprint(description, 2)
	description.Name = "Renamed egress"
	description.Revision++
	if renamed := providerEgressFingerprint(description, 2); renamed != initial {
		t.Fatalf("display-only edit changed runtime fingerprint: before=%q after=%q", initial, renamed)
	}
	description.Endpoint = "https://proxy.example.com:9443"
	description.EndpointPort = 9443
	if changed := providerEgressFingerprint(description, 2); changed == initial {
		t.Fatal("runtime endpoint edit did not change fingerprint")
	}
}

func TestAdminManagedProviderEgressProxyIsEncryptedAndHotActivated(t *testing.T) {
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
	beforeRuntimeID := runtime.providerEgress.Current().runtimeID

	created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/provider-egress-proxies", "", map[string]any{
		"name": "Corporate egress", "kind": "http_connect", "endpoint": "http://127.0.0.1:18080",
		"allow_loopback_endpoint": true, "allow_cleartext_basic_auth": true,
		"username": "halro", "password": "secret-canary", "current_password": "correct horse battery staple",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var proxy providerEgressDescription
	if err := json.Unmarshal(created.Body.Bytes(), &proxy); err != nil {
		t.Fatal(err)
	}
	if proxy.ID == "" || !proxy.Authenticated || proxy.Revision != 1 {
		t.Fatalf("created proxy=%#v", proxy)
	}
	active := runtime.providerEgress.Current()
	if active.runtimeID == beforeRuntimeID {
		t.Fatal("proxy create did not hot-activate a new runtime snapshot")
	}
	if _, ok := active.connector(proxy.ID); !ok {
		t.Fatal("created proxy is not available without restart")
	}

	stored, err := runtime.store.GetProviderEgressProxy(context.Background(), proxy.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := runtime.store.GetCredential(context.Background(), stored.BasicAuthCredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(credential.Ciphertext), "secret-canary") || credential.Type != providerEgressCredentialType {
		t.Fatalf("proxy authentication was not stored as an encrypted internal credential: %#v", credential)
	}
	credentials := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials", "", nil)
	if credentials.Code != http.StatusOK || strings.Contains(credentials.Body.String(), stored.BasicAuthCredentialID) {
		t.Fatalf("internal proxy credential escaped through credential API: status=%d body=%s", credentials.Code, credentials.Body.String())
	}

	oldRuntimeID := active.runtimeID
	updated := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/provider-egress-proxies/"+proxy.ID, `"1"`, map[string]any{
		"name": "Corporate egress", "kind": "http_connect", "endpoint": "https://127.0.0.1:18082",
		"allow_loopback_endpoint": true, "allow_cleartext_basic_auth": true,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &proxy); err != nil {
		t.Fatal(err)
	}
	if proxy.Revision != 2 || proxy.AllowCleartextBasicAuth || runtime.providerEgress.Current().runtimeID == oldRuntimeID {
		t.Fatalf("proxy update was not hot-activated: proxy=%#v runtime=%q", proxy, runtime.providerEgress.Current().runtimeID)
	}
	rotated, err := runtime.store.GetCredential(context.Background(), stored.BasicAuthCredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Audience != "https://127.0.0.1:18082" || rotated.KeyVersion != credential.KeyVersion+1 {
		t.Fatalf("endpoint-bound proxy authentication was not resealed: %#v", rotated)
	}

	providerCredential := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com", "secret": "provider-secret",
	})
	if providerCredential.Code != http.StatusCreated {
		t.Fatalf("credential status=%d body=%s", providerCredential.Code, providerCredential.Body.String())
	}
	var providerSecret credentialView
	if err := json.Unmarshal(providerCredential.Body.Bytes(), &providerSecret); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com",
		"credential_id": providerSecret.ID, "enabled": true,
		"capabilities": map[string]any{"chat": true, "streaming": true}, "egress_proxy_id": proxy.ID,
	})
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	var instance domain.ProviderInstance
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.EgressProxyID != proxy.ID {
		t.Fatalf("stored egress=%q", instance.EgressProxyID)
	}

	updatePayload := map[string]any{
		"name": "OpenAI renamed", "type": "openai", "base_url": instance.BaseURL,
		"credential_id": instance.CredentialID, "enabled": true, "capabilities": instance.Capabilities,
	}
	providerUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/providers/"+instance.ID, `"`+strconv.FormatUint(instance.Revision, 10)+`"`, updatePayload)
	if providerUpdate.Code != http.StatusOK {
		t.Fatalf("omitted egress update status=%d body=%s", providerUpdate.Code, providerUpdate.Body.String())
	}
	if err := json.Unmarshal(providerUpdate.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.EgressProxyID != proxy.ID {
		t.Fatalf("omitted Provider update cleared egress: %#v", instance)
	}
	now := runtime.clockNow().UTC()
	deploymentCapabilities := domain.ProviderCapabilities{Chat: true}
	deployment, err := runtime.store.PutDeployment(context.Background(), domain.Deployment{
		ID: "deployment_proxy_lock", Name: "Proxy lock", ProviderID: instance.ID, ProviderModel: "gpt-test",
		AccessSurface: instance.AccessSurface, ProfileID: instance.ProfileID, Capabilities: deploymentCapabilities,
		CapabilityEvidence:      domain.EvidenceForCapabilities(deploymentCapabilities, domain.EvidenceDeclared),
		ModelCapabilitySnapshot: domain.DeclaredCapabilitySnapshot("gpt-test", "sha256:proxy-lock", deploymentCapabilities, now),
		CreatedAt:               now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	deployment.LastTestStatus = domain.DeploymentTestHealthy
	deployment.LastTestedAt = &now
	deployment.LastTestRevision = deployment.Revision + 1
	deployment.LastTestRuntimeID = runtime.providerEgressTestRuntimeID(proxy.ID)
	deployment, err = runtime.store.PutDeployment(context.Background(), deployment, deployment.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.deploymentTestIsCurrent(context.Background(), deployment) {
		t.Fatal("fresh deployment test evidence was not current before the proxy runtime changed")
	}
	changedRuntime := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/provider-egress-proxies/"+proxy.ID, `"2"`, map[string]any{
		"name": "Corporate egress", "kind": "http_connect", "endpoint": "https://127.0.0.1:18083",
		"allow_loopback_endpoint": true,
	})
	if changedRuntime.Code != http.StatusOK {
		t.Fatalf("runtime-changing proxy update status=%d body=%s", changedRuntime.Code, changedRuntime.Body.String())
	}
	if runtime.deploymentTestIsCurrent(context.Background(), deployment) {
		t.Fatal("deployment test evidence remained current after its proxy runtime changed")
	}
	enableStoredDeploymentForTest(t, runtime, deployment.ID)
	blockedUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/provider-egress-proxies/"+proxy.ID, `"3"`, map[string]any{
		"name": "Corporate egress", "kind": "http_connect", "endpoint": "http://127.0.0.1:18084",
		"allow_loopback_endpoint": true, "allow_cleartext_basic_auth": true,
	})
	if blockedUpdate.Code != http.StatusBadRequest || !strings.Contains(blockedUpdate.Body.String(), "provider_egress_proxy_change_locked_by_deployments") {
		t.Fatalf("active deployment proxy update status=%d body=%s", blockedUpdate.Code, blockedUpdate.Body.String())
	}

	deletion := performAdminMutation(t, runtime, cookie, csrf, http.MethodDelete, "/admin/api/v1/provider-egress-proxies/"+proxy.ID, `"3"`, nil)
	if deletion.Code != http.StatusConflict || !strings.Contains(deletion.Body.String(), "provider_egress_proxy_in_use") {
		t.Fatalf("referenced proxy deletion status=%d body=%s", deletion.Code, deletion.Body.String())
	}
}
