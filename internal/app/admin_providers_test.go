package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
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
	if !credential.SecretConfigured || credential.KeyVersion != 1 ||
		credential.AccessSurface != domain.SurfaceOpenAI || credential.Scheme != domain.CredentialBearerStatic {
		t.Fatalf("unexpected credential view: %#v", credential)
	}

	mediaOnlyResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI media", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"bindings": []map[string]any{
				{"id": "forged-chat", "profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": false, "capabilities": map[string]any{}},
				{"id": "forged-media", "profile_id": domain.ProfileOpenAIMediaResources, "enabled": true,
					"capabilities":        map[string]any{"images": true, "files": true},
					"capability_evidence": map[string]any{"images": domain.EvidenceVerified}},
			},
		})
	if mediaOnlyResponse.Code != http.StatusCreated {
		t.Fatalf("media-only provider create status=%d body=%s", mediaOnlyResponse.Code, mediaOnlyResponse.Body.String())
	}
	var mediaOnly domain.ProviderInstance
	if err := json.Unmarshal(mediaOnlyResponse.Body.Bytes(), &mediaOnly); err != nil {
		t.Fatal(err)
	}
	if mediaOnly.ProfileID != domain.ProfileOpenAIMediaResources || !mediaOnly.Capabilities.Images || !mediaOnly.Capabilities.Files ||
		len(mediaOnly.Bindings) != 2 || mediaOnly.Bindings[0].ID != domain.DefaultProviderProfileBindingID(mediaOnly.ID, domain.ProfileOpenAIMediaResources) ||
		mediaOnly.Bindings[0].CapabilityEvidence["images"] != domain.EvidenceDeclared {
		t.Fatalf("media-only provider was not canonicalized: %#v", mediaOnly)
	}
	allDisabled := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Disabled bindings", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"bindings": []map[string]any{{"profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": false, "capabilities": map[string]any{}}},
		})
	if allDisabled.Code != http.StatusBadRequest {
		t.Fatalf("all-disabled bindings accepted: %d %s", allDisabled.Code, allDisabled.Body.String())
	}
	multiBindingResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "OpenAI complete", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"bindings": []map[string]any{
				{"profile_id": domain.ProfileOpenAIChatEmbeddings, "enabled": true, "capabilities": map[string]any{"chat": true, "streaming": true, "embeddings": true}},
				{"profile_id": domain.ProfileOpenAIMediaResources, "enabled": true, "capabilities": map[string]any{"images": true, "files": true, "batches": true}},
			},
		})
	if multiBindingResponse.Code != http.StatusCreated {
		t.Fatalf("multi-binding provider create status=%d body=%s", multiBindingResponse.Code, multiBindingResponse.Body.String())
	}
	var multiBinding domain.ProviderInstance
	if err := json.Unmarshal(multiBindingResponse.Body.Bytes(), &multiBinding); err != nil {
		t.Fatal(err)
	}
	if len(multiBinding.Bindings) != 2 || !multiBinding.Capabilities.Chat || !multiBinding.Capabilities.Images {
		t.Fatalf("multi-binding provider summary=%#v", multiBinding)
	}
	for _, binding := range multiBinding.Bindings {
		if _, ok := runtime.providers.AdapterForBinding(multiBinding.ID, binding.ID); !ok {
			t.Fatalf("binding adapter %q was not loaded", binding.ID)
		}
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
		ID                 string                       `json:"id"`
		AccessSurface      domain.AccessSurface         `json:"access_surface"`
		ProfileID          domain.ProviderProfileID     `json:"profile_id"`
		CredentialScheme   domain.CredentialScheme      `json:"credential_scheme"`
		CapabilityEvidence domain.CapabilityEvidenceSet `json:"capability_evidence"`
	}
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.AccessSurface != domain.SurfaceOpenAI || instance.ProfileID != domain.ProfileOpenAIChatEmbeddings ||
		instance.CredentialScheme != domain.CredentialBearerStatic || instance.CapabilityEvidence["chat"] != domain.EvidenceDeclared {
		t.Fatalf("unexpected provider profile: %#v", instance)
	}
	forgedEvidence := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Forged", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id":       credential.ID,
			"capability_evidence": map[string]any{"chat": "verified"},
		})
	if forgedEvidence.Code != http.StatusBadRequest {
		t.Fatalf("admin accepted forged capability evidence: %d %s", forgedEvidence.Code, forgedEvidence.Body.String())
	}
	blockedCredentialDelete := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/credentials/"+credential.ID, `"1"`, stepUp(),
	)
	if blockedCredentialDelete.Code != http.StatusConflict {
		t.Fatalf("credential delete with provider reference status=%d body=%s",
			blockedCredentialDelete.Code, blockedCredentialDelete.Body.String())
	}
	rejectedDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "Invalid limits", "provider_id": instance.ID, "provider_model": "gpt-test",
			"enabled": false,
			"capabilities": map[string]any{
				"chat": true, "streaming": true,
				"max_context_tokens": int64(256), "max_output_tokens": int64(64),
			},
		},
	)
	if rejectedDeployment.Code != http.StatusBadRequest {
		t.Fatalf("deployment exceeded provider capability status=%d body=%s", rejectedDeployment.Code, rejectedDeployment.Body.String())
	}
	rejectedEnabledDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "Unsafe enabled", "provider_id": instance.ID, "provider_model": "gpt-test",
			"enabled": true,
		},
	)
	if rejectedEnabledDeployment.Code != http.StatusConflict {
		t.Fatalf("enabled deployment create status=%d body=%s", rejectedEnabledDeployment.Code, rejectedEnabledDeployment.Body.String())
	}
	legacyPriceWrite := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "Legacy price write", "provider_id": instance.ID, "provider_model": "gpt-test",
			"input_micros_per_million": int64(1), "enabled": false,
		},
	)
	if legacyPriceWrite.Code != http.StatusBadRequest {
		t.Fatalf("legacy deployment price write status=%d body=%s", legacyPriceWrite.Code, legacyPriceWrite.Body.String())
	}

	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "GPT test", "provider_id": instance.ID, "provider_model": "gpt-test",
			// gpt-test is not in the model catalog, so its capabilities are an
			// explicit operator declaration rather than the profile ceiling.
			"mode": "operator_declared",
			"capabilities": map[string]any{
				"chat": true, "streaming": true, "embeddings": true, "tools": true,
				"vision": true, "json_mode": true, "developer_role": true,
				"reasoning": true, "stream_usage": true,
				// The provider declares 128/64; a deployment may narrow a
				// non-zero limit but may not leave it undeclared.
				"max_context_tokens": int64(128), "max_output_tokens": int64(64),
			},
			"max_concurrency": int64(2), "enabled": false,
		},
	)
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment create status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment struct {
		ID                 string                       `json:"id"`
		Revision           uint64                       `json:"revision"`
		LastTestStatus     domain.DeploymentTestStatus  `json:"last_test_status"`
		LastTestRevision   uint64                       `json:"last_test_revision"`
		AccessSurface      domain.AccessSurface         `json:"access_surface"`
		ProfileID          domain.ProviderProfileID     `json:"profile_id"`
		TargetKind         domain.DeploymentTargetKind  `json:"target_kind"`
		CapabilityEvidence domain.CapabilityEvidenceSet `json:"capability_evidence"`
	}
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.AccessSurface != domain.SurfaceOpenAI || deployment.ProfileID != domain.ProfileOpenAIChatEmbeddings || deployment.TargetKind != domain.TargetModelID ||
		deployment.CapabilityEvidence["chat"] != domain.EvidenceDeclared {
		t.Fatalf("unexpected deployment profile: %#v", deployment)
	}
	createEffectiveMeteredPriceForTest(t, runtime, deployment.ID)
	directEnable := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/deployments/"+deployment.ID, `"1"`,
		map[string]any{
			"name": "GPT test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"max_concurrency": int64(2), "enabled": true,
		},
	)
	if directEnable.Code != http.StatusConflict {
		t.Fatalf("untested deployment enable status=%d body=%s", directEnable.Code, directEnable.Body.String())
	}
	probe := &adminProbeAdapter{targets: []domain.InvocationTargetDescriptor{{TargetID: "gpt-z", TargetKind: domain.TargetModelID}, {TargetID: "gpt-a", TargetKind: domain.TargetModelID, OwnedBy: "openai"}, {TargetID: "gpt-a", TargetKind: domain.TargetModelID}}}
	probeRegistry := provider.NewRegistry()
	if err := probeRegistry.RegisterAdapter(instance.ID, probe); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(probeRegistry)
	initialTest := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments/"+deployment.ID+"/test", "", nil,
	)
	if initialTest.Code != http.StatusOK || probe.probes != 1 {
		t.Fatalf("initial deployment test status=%d probes=%d body=%s", initialTest.Code, probe.probes, initialTest.Body.String())
	}
	var testedDeployment struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(initialTest.Body.Bytes(), &testedDeployment); err != nil {
		t.Fatal(err)
	}
	enableDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(testedDeployment.Revision, 10)+`"`,
		map[string]any{
			"name": "GPT test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"max_concurrency": int64(2), "enabled": true,
		},
	)
	if enableDeployment.Code != http.StatusOK {
		t.Fatalf("validated deployment enable status=%d body=%s", enableDeployment.Code, enableDeployment.Body.String())
	}
	if err := json.Unmarshal(enableDeployment.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.LastTestStatus != domain.DeploymentTestHealthy || deployment.LastTestRevision != deployment.Revision {
		t.Fatalf("enabling deployment made its current validation stale: %#v", deployment)
	}
	profileChange := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/providers/"+instance.ID, `"1"`, map[string]any{
			"name": "Changed", "type": "deepseek", "base_url": "https://api.deepseek.com",
			"credential_id": credential.ID, "enabled": true,
		})
	if profileChange.Code != http.StatusBadRequest {
		t.Fatalf("referenced provider profile changed: %d %s", profileChange.Code, profileChange.Body.String())
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
		target.Capabilities.MaxContextTokens != 128 || target.Capabilities.MaxOutputTokens != 64 ||
		target.AccessSurface != domain.SurfaceOpenAI || target.ProfileID != domain.ProfileOpenAIChatEmbeddings ||
		target.CapabilityEvidence["chat"] != domain.EvidenceDeclared {
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
		http.MethodDelete, "/admin/api/v1/providers/"+instance.ID, `"1"`, stepUp(),
	)
	if blockedProviderDelete.Code != http.StatusConflict {
		t.Fatalf("provider delete with active deployment status=%d body=%s",
			blockedProviderDelete.Code, blockedProviderDelete.Body.String())
	}
	blockedDeploymentDelete := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(deployment.Revision, 10)+`"`, stepUp(),
	)
	if blockedDeploymentDelete.Code != http.StatusConflict {
		t.Fatalf("deployment delete with active route status=%d body=%s",
			blockedDeploymentDelete.Code, blockedDeploymentDelete.Body.String())
	}
	blockedTargetChange := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(deployment.Revision, 10)+`"`,
		map[string]any{
			"name": "Changed target", "provider_id": instance.ID, "provider_model": "gpt-other",
			"max_concurrency": int64(2), "enabled": true,
		},
	)
	if blockedTargetChange.Code != http.StatusConflict {
		t.Fatalf("deployment target change with active route status=%d body=%s",
			blockedTargetChange.Code, blockedTargetChange.Body.String())
	}

	var route struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(routeResponse.Body.Bytes(), &route); err != nil {
		t.Fatal(err)
	}
	probe = &adminProbeAdapter{targets: []domain.InvocationTargetDescriptor{{TargetID: "gpt-z", TargetKind: domain.TargetModelID}, {TargetID: "gpt-a", TargetKind: domain.TargetModelID, OwnedBy: "openai"}, {TargetID: "gpt-a", TargetKind: domain.TargetModelID}}}
	nextRegistry := provider.NewRegistry()
	storedInstance, err := runtime.store.GetProvider(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	selectedBinding := storedInstance.EffectiveProfileBindings()[0]
	manifest, ok := provider.BuiltinProfile(selectedBinding.ProfileID)
	if !ok {
		t.Fatalf("profile %q is unavailable", selectedBinding.ProfileID)
	}
	profiledProbe, err := provider.NewLegacyAdapterBridge(probe, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nextRegistry.RegisterBindingAdapter(instance.ID, selectedBinding.ID, profiledProbe); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(nextRegistry)
	for index, step := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/api/v1/providers/" + instance.ID + "/invocation-targets"},
		{http.MethodGet, "/admin/api/v1/providers/" + instance.ID + "/invocation-targets"},
		{http.MethodPost, "/admin/api/v1/providers/" + instance.ID + "/invocation-targets"},
	} {
		request := adminRequest(t, step.method, step.path, nil)
		request.AddCookie(cookie)
		if step.method == http.MethodPost {
			request.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("model catalog request %d status=%d body=%s", index, response.Code, response.Body.String())
		}
		var catalog invocationTargetCatalogResponse
		if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
			t.Fatal(err)
		}
		if len(catalog.Items) != 2 || catalog.Items[0].TargetID != "gpt-a" || catalog.Items[1].TargetID != "gpt-z" {
			t.Fatalf("model catalog=%#v", catalog)
		}
		if index == 1 && !catalog.Cached {
			t.Fatal("second model catalog response was not cached")
		}
	}
	if probe.targetLists != 2 {
		t.Fatalf("invocation target upstream calls=%d", probe.targetLists)
	}
	routeTest := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/routes/"+route.ID+"/test", "", nil,
	)
	if routeTest.Code != http.StatusOK || probe.probes != 1 || probe.model != "gpt-test" {
		t.Fatalf("route test status=%d probes=%d model=%q body=%s",
			routeTest.Code, probe.probes, probe.model, routeTest.Body.String())
	}
	var testedRoute domain.Route
	if err := json.Unmarshal(routeTest.Body.Bytes(), &testedRoute); err != nil {
		t.Fatal(err)
	}
	persistedRoute, err := runtime.store.GetRoute(context.Background(), route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRoute.LastTestStatus != domain.DeploymentTestHealthy ||
		persistedRoute.LastTestRevision != persistedRoute.Revision || persistedRoute.LastTestedAt == nil {
		t.Fatalf("route validation was not persisted: %#v", persistedRoute)
	}
	deleteRoute := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/routes/"+route.ID, `"`+strconv.FormatUint(testedRoute.Revision, 10)+`"`, stepUp(),
	)
	if deleteRoute.Code != http.StatusNoContent {
		t.Fatalf("route delete status=%d body=%s", deleteRoute.Code, deleteRoute.Body.String())
	}
	if _, ok := runtime.providers.Resolve("chat"); ok {
		t.Fatal("deleted route remained active")
	}
	immutableTarget := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(deployment.Revision, 10)+`"`,
		map[string]any{
			"name": "Changed target", "provider_id": instance.ID, "provider_model": "gpt-other",
			"target_kind": domain.TargetModelID, "max_concurrency": int64(2), "enabled": false,
		},
	)
	if immutableTarget.Code != http.StatusConflict {
		t.Fatalf("unreferenced deployment target mutation status=%d body=%s", immutableTarget.Code, immutableTarget.Body.String())
	}
	disableDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(deployment.Revision, 10)+`"`,
		map[string]any{
			"name": "GPT test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"max_concurrency": int64(2), "enabled": false,
		},
	)
	if disableDeployment.Code != http.StatusOK {
		t.Fatalf("disable deployment status=%d body=%s", disableDeployment.Code, disableDeployment.Body.String())
	}
	var disabledDeployment struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(disableDeployment.Body.Bytes(), &disabledDeployment); err != nil {
		t.Fatal(err)
	}
	nextRegistry = provider.NewRegistry()
	if err := nextRegistry.RegisterAdapter(instance.ID, probe); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(nextRegistry)
	disabledTest := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments/"+deployment.ID+"/test", "", nil,
	)
	if disabledTest.Code != http.StatusOK || probe.probes != 2 {
		t.Fatalf("disabled deployment test status=%d probes=%d body=%s", disabledTest.Code, probe.probes, disabledTest.Body.String())
	}
	if err := json.Unmarshal(disabledTest.Body.Bytes(), &disabledDeployment); err != nil {
		t.Fatal(err)
	}
	persistedTest, err := runtime.store.GetDeployment(context.Background(), deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedTest.LastTestStatus != domain.DeploymentTestHealthy || persistedTest.LastTestRevision != persistedTest.Revision || persistedTest.LastTestedAt == nil {
		t.Fatalf("deployment validation was not persisted: %#v", persistedTest)
	}
	deleteDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(disabledDeployment.Revision, 10)+`"`, stepUp(),
	)
	if deleteDeployment.Code != http.StatusNoContent {
		t.Fatalf("deployment delete status=%d body=%s", deleteDeployment.Code, deleteDeployment.Body.String())
	}
	deleteProvider := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/providers/"+instance.ID, `"1"`, stepUp(),
	)
	if deleteProvider.Code != http.StatusNoContent {
		t.Fatalf("provider delete status=%d body=%s", deleteProvider.Code, deleteProvider.Body.String())
	}
	deleteMediaProvider := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/providers/"+mediaOnly.ID, `"1"`, stepUp(),
	)
	if deleteMediaProvider.Code != http.StatusNoContent {
		t.Fatalf("media provider delete status=%d body=%s", deleteMediaProvider.Code, deleteMediaProvider.Body.String())
	}
	deleteMultiBindingProvider := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/providers/"+multiBinding.ID, `"1"`, stepUp(),
	)
	if deleteMultiBindingProvider.Code != http.StatusNoContent {
		t.Fatalf("multi-binding provider delete status=%d body=%s", deleteMultiBindingProvider.Code, deleteMultiBindingProvider.Body.String())
	}
	deleteCredential := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/credentials/"+credential.ID,
		`"`+strconv.FormatUint(credential.Revision, 10)+`"`, stepUp(),
	)
	if deleteCredential.Code != http.StatusNoContent {
		t.Fatalf("credential delete status=%d body=%s", deleteCredential.Code, deleteCredential.Body.String())
	}
	if _, err := runtime.store.GetCredential(context.Background(), credential.ID); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("deleted credential remained in store: %v", err)
	}
}

func TestAdminCredentialViewPreservesBedrockBoundBaseURLForRotation(t *testing.T) {
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

	tests := []struct {
		name    string
		baseURL string
		surface domain.AccessSurface
		scheme  domain.CredentialScheme
	}{
		{name: "agent runtime", baseURL: "https://bedrock-agent-runtime.eu-west-1.amazonaws.com", surface: domain.SurfaceBedrockAgentRuntime, scheme: domain.CredentialAWSSigV4Explicit},
		{name: "mantle", baseURL: "https://bedrock-mantle.ap-southeast-1.api.aws", surface: domain.SurfaceBedrockMantle, scheme: domain.CredentialBedrockAPIKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
				"name": test.name, "type": "bedrock", "base_url": test.baseURL,
				"access_surface": test.surface, "scheme": test.scheme, "secret": "test-secret",
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
			}
			var view credentialView
			if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.BoundBaseURL != test.baseURL+":443" || view.AccessSurface != test.surface || view.Scheme != test.scheme {
				t.Fatalf("credential view=%#v", view)
			}
			detail := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials/"+view.ID, "", nil)
			if detail.Code != http.StatusOK {
				t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
			}
			var detailView credentialView
			if err := json.Unmarshal(detail.Body.Bytes(), &detailView); err != nil {
				t.Fatal(err)
			}
			if detailView.BoundBaseURL != test.baseURL+":443" {
				t.Fatalf("detail dropped bound base URL: %#v", detailView)
			}
			list := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials", "", nil)
			if list.Code != http.StatusOK {
				t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
			}
			var page struct {
				Items []credentialView `json:"items"`
			}
			if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			listedIndex := slices.IndexFunc(page.Items, func(item credentialView) bool { return item.ID == view.ID })
			if listedIndex < 0 || page.Items[listedIndex].BoundBaseURL != test.baseURL+":443" {
				t.Fatalf("list dropped bound base URL: %#v", page.Items)
			}
			rotated := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/credentials/"+view.ID, `"1"`, map[string]any{
				"name": test.name, "type": "bedrock", "base_url": view.BoundBaseURL,
				"access_surface": test.surface, "scheme": test.scheme, "secret": "rotated-secret",
			})
			if rotated.Code != http.StatusOK {
				t.Fatalf("rotate status=%d body=%s", rotated.Code, rotated.Body.String())
			}
			if err := json.Unmarshal(rotated.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.BoundBaseURL != test.baseURL+":443" || view.KeyVersion != 2 {
				t.Fatalf("rotated credential view=%#v", view)
			}
		})
	}
}

func TestCapabilityEvidenceIsNotSilentlyUpgradedAndDisabledCapabilitiesDowngrade(t *testing.T) {
	providerCapabilities := domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true}
	declared := domain.EvidenceForCapabilities(providerCapabilities, domain.EvidenceDeclared)
	updated := preserveCapabilityEvidence(providerCapabilities, declared)
	if updated["chat"] != domain.EvidenceDeclared || updated["tools"] != domain.EvidenceDeclared {
		t.Fatalf("provider evidence was silently upgraded: %#v", updated)
	}

	deploymentCapabilities := domain.ProviderCapabilities{Chat: true, Streaming: true}
	deployment := deploymentCapabilityEvidence(deploymentCapabilities, declared, nil, "builtin_catalog")
	if deployment["chat"] != domain.EvidenceDeclared || deployment["tools"] != domain.EvidenceUnsupported {
		t.Fatalf("deployment subset evidence=%#v", deployment)
	}
	verified := domain.EvidenceForCapabilities(deploymentCapabilities, domain.EvidenceVerified)
	fresh := deploymentCapabilityEvidence(deploymentCapabilities, verified, nil, "builtin_catalog")
	if fresh["chat"] != domain.EvidenceDeclared {
		t.Fatalf("new deployment inherited verified evidence: %#v", fresh)
	}
	preserved := deploymentCapabilityEvidence(deploymentCapabilities, verified, verified, "builtin_catalog")
	if preserved["chat"] != domain.EvidenceVerified {
		t.Fatalf("unchanged deployment lost verified evidence: %#v", preserved)
	}

	// The cap is the source's, not a blanket demotion. Every source that exists
	// today stops at declared, so the observable answer is unchanged — but a
	// probe result is allowed to keep what it measured, which is the difference
	// between applying the rule and reproducing its current output.
	probed := deploymentCapabilityEvidence(deploymentCapabilities, verified, nil, "verified_probe")
	if probed["chat"] != domain.EvidenceVerified {
		t.Fatalf("a probe result was demoted: %#v", probed)
	}
	unsourced := deploymentCapabilityEvidence(deploymentCapabilities, verified, nil, "")
	if unsourced["chat"] != domain.EvidenceUnsupported {
		t.Fatalf("evidence survived a source that establishes nothing: %#v", unsourced)
	}
}

type adminProbeAdapter struct {
	canaryAdapter
	probes      int
	model       string
	targets     []domain.InvocationTargetDescriptor
	targetLists int
}

func (a *adminProbeAdapter) Probe(_ context.Context, model string) error {
	a.probes++
	a.model = model
	return nil
}

func (a *adminProbeAdapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	return domain.InvocationTargetDiscoveryCapabilities{TargetKinds: []domain.DeploymentTargetKind{domain.TargetModelID}, CanEnumerate: true, CanVerify: true}
}

func (a *adminProbeAdapter) ListInvocationTargets(context.Context, domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	a.targetLists++
	return slices.Clone(a.targets), nil
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
			"mode":         "operator_declared",
			"capabilities": map[string]any{"chat": true, "streaming": true, "stream_usage": true},
			"enabled":      false,
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
	enableStoredDeploymentForTest(t, runtime, deployment.ID)
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

func TestAdminBedrockTitanEmbeddingProfilePinsModelFamily(t *testing.T) {
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
	secret := `{"access_key_id":"AKIDEXAMPLE12345678","secret_access_key":"test-secret-access-key-value","region":"us-east-1"}`
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Bedrock Runtime", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com", "secret": secret,
	})
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	forgedCapabilities := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Forged Titan", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "profile_id": domain.ProfileBedrockInvokeTitanEmbedV2, "enabled": true,
		"capabilities": map[string]any{"chat": true, "embeddings": true, "max_context_tokens": int64(8192)},
	})
	if forgedCapabilities.Code != http.StatusBadRequest {
		t.Fatalf("forged capabilities status=%d body=%s", forgedCapabilities.Code, forgedCapabilities.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Titan embeddings", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "profile_id": domain.ProfileBedrockInvokeTitanEmbedV2, "enabled": true,
	})
	var instance domain.ProviderInstance
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	if !instance.Capabilities.Embeddings || instance.Capabilities.Chat || instance.Capabilities.Streaming || instance.Capabilities.MaxContextTokens != 8192 {
		t.Fatalf("unexpected Titan capabilities: %#v", instance.Capabilities)
	}
	wrong := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Wrong family", "provider_id": instance.ID, "provider_model": "cohere.embed-v4:0", "enabled": false,
	})
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong model status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Titan V2", "provider_id": instance.ID, "provider_model": "amazon.titan-embed-text-v2:0", "enabled": false,
	})
	var deployment domain.Deployment
	if deploymentResponse.Code != http.StatusCreated || json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment) != nil {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	enableStoredDeploymentForTest(t, runtime, deployment.ID)
	routeResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/routes", "", map[string]any{
		"public_model": "embedding", "deployment_id": deployment.ID, "strategy": "ordered", "enabled": true,
	})
	if routeResponse.Code != http.StatusCreated {
		t.Fatalf("route status=%d body=%s", routeResponse.Code, routeResponse.Body.String())
	}
	target, ok := runtime.providers.Resolve("embedding")
	if !ok || target.ProfileID != domain.ProfileBedrockInvokeTitanEmbedV2 || !target.Capabilities.Embeddings || target.Capabilities.Chat {
		t.Fatalf("unexpected Titan target: %#v", target)
	}
}

func TestAdminBedrockMantleProfilesAreSelectableAndSurfaceIsolated(t *testing.T) {
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
	endpoint := "https://bedrock-mantle.us-east-1.api.aws"
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Mantle API key", "type": "bedrock", "base_url": endpoint, "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	if credentialResponse.Code != http.StatusCreated || strings.Contains(credentialResponse.Body.String(), "bedrock-api-key") {
		t.Fatalf("credential create status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	if credential.AccessSurface != domain.SurfaceBedrockMantle || credential.Scheme != domain.CredentialBedrockAPIKey {
		t.Fatalf("unexpected credential binding: %#v", credential)
	}
	profiles := []domain.ProviderProfileID{
		domain.ProfileBedrockMantleOpenAIChat,
		domain.ProfileBedrockMantleOpenAIResponses,
		domain.ProfileBedrockMantleAnthropicMessages,
	}
	for _, profileID := range profiles {
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": string(profileID), "type": "bedrock", "base_url": endpoint, "credential_id": credential.ID, "enabled": true,
			"access_surface": domain.SurfaceBedrockMantle, "profile_id": profileID, "credential_scheme": domain.CredentialBedrockAPIKey,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s status=%d body=%s", profileID, response.Code, response.Body.String())
		}
		var instance domain.ProviderInstance
		if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
			t.Fatal(err)
		}
		if instance.ProfileID != profileID || instance.AccessSurface != domain.SurfaceBedrockMantle || instance.CredentialScheme != domain.CredentialBedrockAPIKey || !instance.Capabilities.Chat || !instance.Capabilities.Streaming {
			t.Fatalf("unexpected %s provider: %#v", profileID, instance)
		}
		if profileID == domain.ProfileBedrockMantleOpenAIResponses && instance.Capabilities.Reasoning {
			t.Fatal("stateless Responses profile advertised unrepresentable reasoning output")
		}
	}
	crossSurface := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "cross-surface", "type": "bedrock", "base_url": endpoint, "credential_id": credential.ID, "enabled": true,
		"profile_id": domain.ProfileBedrockConverseText,
	})
	if crossSurface.Code != http.StatusBadRequest {
		t.Fatalf("cross-surface provider status=%d body=%s", crossSurface.Code, crossSurface.Body.String())
	}
	unsafeEndpoint := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "unsafe", "type": "bedrock", "base_url": "https://example.com", "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	if unsafeEndpoint.Code != http.StatusBadRequest {
		t.Fatalf("unsafe Mantle endpoint status=%d body=%s", unsafeEndpoint.Code, unsafeEndpoint.Body.String())
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

func enableStoredDeploymentForTest(t *testing.T, runtime *Runtime, deploymentID string) {
	t.Helper()
	if _, err := runtime.store.SelectDeploymentPriceVersion(context.Background(), deploymentID, time.Now().UTC()); err != nil {
		createEffectiveFreePriceForTest(t, runtime, deploymentID)
	}
	deployment, err := runtime.store.GetDeployment(context.Background(), deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	deployment.Enabled = true
	deployment.UpdatedAt = time.Now().UTC()
	if _, err := runtime.store.PutDeployment(context.Background(), deployment, deployment.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func createEffectiveFreePriceForTest(t *testing.T, runtime *Runtime, deploymentID string) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	_, err := runtime.store.CreateDeploymentPriceVersion(context.Background(), domain.DeploymentPriceVersion{
		ID: "price_free_" + deploymentID, DeploymentID: deploymentID,
		BillingMode: domain.BillingModeFree, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		EffectiveFrom: now, CreatedBy: "test", CreatedAt: now,
		Source: domain.PriceSource{
			Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: now,
			ContentSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Reference:     "test fixture", AssertedWithoutArchive: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func createEffectiveMeteredPriceForTest(t *testing.T, runtime *Runtime, deploymentID string) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	_, err := runtime.store.CreateDeploymentPriceVersion(context.Background(), domain.DeploymentPriceVersion{
		ID: "price_" + deploymentID, DeploymentID: deploymentID,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		EffectiveFrom: now, CreatedBy: "test", CreatedAt: now,
		Source: domain.PriceSource{
			Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: now,
			ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference:     "test fixture", AssertedWithoutArchive: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Provider and Deployment both refuse to leave service while something
// downstream names them. The Profile Binding level had no such rule, and it is
// the level the console actually drives: the provider form derives each
// binding's enabled flag from which capabilities are ticked, so unticking chat
// and embeddings on a provider whose deployment runs on that interface used to
// be accepted, land in the store, and leave a deployment bound to a binding that
// produces no adapter.
func TestProviderRefusesToSwitchOffAnInterfaceADeploymentRunsOn(t *testing.T) {
	runtime, bootstrap, session := verifyingProviderRuntime(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	chat := instance.EffectiveProfileBindings()[0]
	chat.ProfileID = domain.ProfileOpenAIChatEmbeddings
	chat.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, chat.ProfileID)
	chat.CapabilityEvidence = domain.EvidenceForCapabilities(chat.Capabilities, domain.EvidenceDeclared)
	media := chat
	media.ProfileID = domain.ProfileOpenAIMediaResources
	media.ID = domain.DefaultProviderProfileBindingID(instance.ID, media.ProfileID)
	media.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, media.ProfileID)
	media.CapabilityEvidence = domain.EvidenceForCapabilities(media.Capabilities, domain.EvidenceDeclared)

	// Chat off, media on: the Provider record itself stays valid, which is why
	// domain validation never caught this.
	chat.Enabled, media.Enabled = false, true
	body := map[string]any{
		"name": instance.Name, "type": instance.Type, "base_url": instance.BaseURL,
		"credential_id": instance.CredentialID, "max_concurrency": instance.MaxConcurrency,
		"enabled": true, "bindings": []domain.ProviderProfileBinding{chat, media},
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/providers/"+instance.ID, session, body)
	request.Header.Set("If-Match", revisionETag(instance.Revision))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["code"] != "binding_referenced_by_deployment" {
		t.Fatalf("code=%q error=%q", decoded["code"], decoded["error"])
	}
	// A refusal that already wrote is not a refusal.
	stored, err := runtime.store.GetProvider(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != instance.Revision {
		t.Fatalf("revision moved from %d to %d on a refused edit", instance.Revision, stored.Revision)
	}
	for _, binding := range stored.EffectiveProfileBindings() {
		if !binding.Enabled {
			t.Fatalf("binding %q was switched off by a refused edit", binding.ID)
		}
	}
}

func TestOmittingAReferencedBindingIsRefusedLikeSwitchingItOff(t *testing.T) {
	runtime, bootstrap, session := verifyingProviderRuntime(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	chat := instance.EffectiveProfileBindings()[0]
	media := chat
	media.ID = domain.DefaultProviderProfileBindingID(instance.ID, domain.ProfileOpenAIMediaResources)
	media.ProfileID = domain.ProfileOpenAIMediaResources
	media.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, media.ProfileID)
	media.CapabilityEvidence = domain.EvidenceForCapabilities(media.Capabilities, domain.EvidenceDeclared)
	body := map[string]any{
		"name": instance.Name, "type": instance.Type, "base_url": instance.BaseURL,
		"credential_id": instance.CredentialID, "max_concurrency": instance.MaxConcurrency,
		"enabled": true, "bindings": []domain.ProviderProfileBinding{media},
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/providers/"+instance.ID, session, body)
	request.Header.Set("If-Match", revisionETag(instance.Revision))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "binding_referenced_by_deployment") {
		t.Fatalf("omitted referenced binding status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := runtime.store.GetProvider(context.Background(), instance.ID)
	if err != nil || stored.Revision != instance.Revision {
		t.Fatalf("refused omission changed provider revision=%d err=%v", stored.Revision, err)
	}
}

func TestEmptyBindingsAreRefusedBeforeLegacyFallback(t *testing.T) {
	runtime, bootstrap, session := verifyingProviderRuntime(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"name": instance.Name, "type": instance.Type, "base_url": instance.BaseURL,
		"credential_id": instance.CredentialID, "max_concurrency": instance.MaxConcurrency,
		"enabled": instance.Enabled, "bindings": []domain.ProviderProfileBinding{},
		"profile_id": instance.ProfileID, "access_surface": instance.AccessSurface,
		"credential_scheme": instance.CredentialScheme, "capabilities": instance.Capabilities,
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/providers/"+instance.ID, session, body)
	request.Header.Set("If-Match", revisionETag(instance.Revision))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "at least one profile binding") {
		t.Fatalf("empty bindings status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := runtime.store.GetProvider(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != instance.Revision || len(stored.EffectiveProfileBindings()) == 0 {
		t.Fatalf("empty binding refusal changed provider: %#v", stored)
	}
}

// Switching off an interface nothing runs on is ordinary configuration and must
// still work, or the guard above would make multi-interface providers uneditable.
func TestProviderAllowsSwitchingOffAnUnusedInterface(t *testing.T) {
	runtime, bootstrap, session := verifyingProviderRuntime(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	chat := instance.EffectiveProfileBindings()[0]
	chat.ProfileID = domain.ProfileOpenAIChatEmbeddings
	chat.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, chat.ProfileID)
	chat.CapabilityEvidence = domain.EvidenceForCapabilities(chat.Capabilities, domain.EvidenceDeclared)
	media := chat
	media.ProfileID = domain.ProfileOpenAIMediaResources
	media.ID = domain.DefaultProviderProfileBindingID(instance.ID, media.ProfileID)
	media.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, media.ProfileID)
	media.CapabilityEvidence = domain.EvidenceForCapabilities(media.Capabilities, domain.EvidenceDeclared)

	// The deployment runs on chat, so switching media off costs nothing.
	chat.Enabled, media.Enabled = true, false
	body := map[string]any{
		"name": instance.Name, "type": instance.Type, "base_url": instance.BaseURL,
		"credential_id": instance.CredentialID, "max_concurrency": instance.MaxConcurrency,
		"enabled": true, "bindings": []domain.ProviderProfileBinding{chat, media},
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/providers/"+instance.ID, session, body)
	request.Header.Set("If-Match", revisionETag(instance.Revision))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func verifyingProviderRuntime(t *testing.T) (*Runtime, BootstrapResult, loggedInAdmin) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
		ProjectName: "Guard", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return runtime, bootstrap, loginTestAdmin(t, runtime, "admin", stepUpTestPassword)
}
