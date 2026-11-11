package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestAdminConfigurationTransferRoundTrip(t *testing.T) {
	ctx := context.Background()
	source, bootstrap, _ := openBootstrappedRuntime(t)
	sourceCookie, _ := loginAdminForTest(t, source)
	sourceProvider, err := source.store.GetProvider(ctx, bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	// A query may carry secret material even though it is not in the Vault.
	sourceProvider.BaseURL += "?api-key=endpoint-secret-canary"
	if _, err := source.store.PutProvider(ctx, sourceProvider, sourceProvider.Revision, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	redaction := redactionPolicyInput{Name: "Redact", Enabled: true, Mode: "strict", Rules: []redactionRuleInput{{
		ID: "redact_fixture", Name: "Fixture", Kind: "dictionary", Dictionary: []string{"sample-personal-data"},
		Scopes: []string{"inbound"}, Action: "mask", Enabled: true,
	}}}
	compiled, err := redaction.policy("redaction_test", now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.PutRedactionPolicy(ctx, compiled, 0, nil); err != nil {
		t.Fatal(err)
	}
	guard := tokenGuardInput{Name: "Guard", Enabled: true, Action: "observe", RequestTokens: 1000}
	if _, err := source.store.PutTokenGuardPolicy(ctx, guard.policy("guard_test", now, now), 0, nil); err != nil {
		t.Fatal(err)
	}
	project, err := source.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.RedactionPolicyID = "redaction_test"
	project.TokenGuardPolicyID = "guard_test"
	project.DailyBudgetMicrosUSD = 9_007_199_254_740_993
	if _, err := source.store.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	key, err := source.store.GetGatewayKey(ctx, bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeDiscovery}
	if _, err := source.store.PutGatewayKey(ctx, key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	sourceDeployment, err := source.store.GetDeployment(ctx, bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	sourceDeployment.LastTestStatus = domain.DeploymentTestHealthy
	sourceDeployment.LastTestedAt = &now
	sourceDeployment.LastTestRevision = sourceDeployment.Revision + 1
	sourceDeployment.LastTestRuntimeID = "source-probe-canary"
	if _, err := source.store.PutDeployment(ctx, sourceDeployment, sourceDeployment.Revision, nil); err != nil {
		t.Fatal(err)
	}

	exported := authenticatedAdminGet(t, source, sourceCookie, "/admin/api/v1/configuration/export")
	if exported.Code != http.StatusOK || exported.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(exported.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export failed: %d %s", exported.Code, exported.Body)
	}
	for _, excluded := range []string{
		bootstrap.GatewayKey, sourceProvider.CredentialID, "provider-secret", "endpoint-secret-canary", "source-probe-canary",
		`"credential_id"`, `"ciphertext"`, `"key_hash"`, `"hash_version"`, `"last_test_status"`, `"revision"`,
		`"usage"`, `"audit"`, `"model_capability_snapshot"`, `"source"`,
	} {
		if strings.Contains(exported.Body.String(), excluded) {
			t.Fatal("export contains non-portable data")
		}
	}
	var bundle configurationBundle
	if err := json.Unmarshal(exported.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.recordCount() != 7 || bundle.Providers[0].BaseURL != "" || bundle.Deployments[0].Price == nil {
		t.Fatalf("wrong portable records: %+v", bundle)
	}
	if !bundle.Deployments[0].Enabled || !bundle.Routes[0].Enabled {
		t.Fatal("export lost the source activation state")
	}

	target, session := stepUpTestRuntime(t)
	targetBaseURL := "https://target.example"
	credentialResponse := performAdminMutation(t, target, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Target", "type": "openai", "base_url": targetBaseURL, "secret": "target-credential-canary",
	})
	var credential struct{ ID string }
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil || credential.ID == "" {
		t.Fatalf("create target credential: %d %s", credentialResponse.Code, credentialResponse.Body)
	}
	request := func(path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		return performAdminMutation(t, target, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/configuration/import"+path, "", value)
	}
	previewImport := func(input configurationImportInput) configurationImportPreview {
		t.Helper()
		response := request("/preview", input)
		var preview configurationImportPreview
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &preview) != nil {
			t.Fatalf("preview: %d %s", response.Code, response.Body)
		}
		return preview
	}
	input := configurationImportInput{Bundle: bundle}
	if preview := previewImport(input); preview.PlanID != "" || len(preview.Providers) != 1 || len(preview.Issues) == 0 {
		t.Fatalf("unbound import was accepted: %+v", preview)
	}
	input.Bindings = map[string]configurationProviderTarget{
		bootstrap.ProviderID: {CredentialID: credential.ID, BaseURL: &targetBaseURL},
	}
	mismatchedBaseURL := "https://another.example"
	input.Bindings[bootstrap.ProviderID] = configurationProviderTarget{CredentialID: credential.ID, BaseURL: &mismatchedBaseURL}
	if preview := previewImport(input); preview.PlanID != "" || len(preview.Issues) == 0 {
		t.Fatal("credential audience mismatch was accepted")
	}
	input.Bindings[bootstrap.ProviderID] = configurationProviderTarget{CredentialID: credential.ID, BaseURL: &targetBaseURL}
	input.Bundle.Routes[0].DeploymentID = "missing_deployment"
	if preview := previewImport(input); preview.PlanID != "" || len(preview.Issues) != 1 || preview.Issues[0].Code != "missing_deployment" {
		t.Fatalf("dangling route was accepted: %+v", preview)
	}
	input.Bundle.Routes[0].DeploymentID = bootstrap.DeploymentID
	if response := request("/preview", map[string]any{"bundle": map[string]any{
		"version": 1, "gateway_keys": []map[string]string{{"key_hash": "unexpected"}},
	}}); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown secret fields were accepted: %d", response.Code)
	}
	preview := previewImport(input)
	if preview.PlanID == "" || len(preview.Issues) != 0 || len(preview.DeploymentsToEnable) != 1 || len(preview.RoutesToEnable) != 1 {
		t.Fatalf("valid configuration did not produce a plan: %+v", preview)
	}
	apply := configurationApplyInput{configurationImportInput: input, PlanID: preview.PlanID}
	if response := request("", apply); response.Code != http.StatusUnauthorized {
		t.Fatalf("import skipped step-up: %d", response.Code)
	}
	apply.CurrentPassword = stepUpTestPassword
	apply.PlanID = "outdated-plan"
	if response := request("", apply); response.Code != http.StatusConflict {
		t.Fatalf("import accepted an outdated preview: %d %s", response.Code, response.Body)
	}
	if err := target.store.CheckConfigurationImportTarget(); err != nil {
		t.Fatalf("failed preview or import wrote configuration: %v", err)
	}
	apply.PlanID = preview.PlanID
	imported := request("", apply)
	var result struct {
		Keys  []configurationIssuedKey `json:"gateway_keys"`
		Stale bool                     `json:"activation_stale"`
	}
	if imported.Code != http.StatusCreated || imported.Header().Get("Cache-Control") != "no-store" ||
		imported.Header().Get("Halro-Operation-Id") == "" || json.Unmarshal(imported.Body.Bytes(), &result) != nil ||
		result.Stale || len(result.Keys) != 1 {
		t.Fatalf("apply failed: %d", imported.Code)
	}
	freshKey := result.Keys[0].Key
	if freshKey == bootstrap.GatewayKey || freshKey == "" || result.Keys[0].ProjectID != bootstrap.ProjectID {
		t.Fatal("target did not mint a fresh key with the right project")
	}
	if response := request("", apply); response.Code != http.StatusConflict || strings.Contains(response.Body.String(), freshKey) {
		t.Fatalf("replay did not refuse cleanly: %d", response.Code)
	}

	stored, err := target.store.ReadPortableConfiguration(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Providers) != 1 || stored.Providers[0].CredentialID != credential.ID || stored.Providers[0].BaseURL != targetBaseURL {
		t.Fatal("target did not bind the selected credential and endpoint")
	}
	if len(stored.Projects) != 1 || stored.Projects[0].DailyBudgetMicrosUSD != project.DailyBudgetMicrosUSD ||
		stored.Projects[0].RedactionPolicyID != project.RedactionPolicyID || stored.Projects[0].TokenGuardPolicyID != project.TokenGuardPolicyID {
		t.Fatal("project settings or policy bindings were lost")
	}
	if len(stored.Deployments) != 1 || stored.Deployments[0].Enabled || stored.Deployments[0].LastTestStatus != "" ||
		len(stored.Routes) != 1 || stored.Routes[0].Enabled || stored.Prices[bootstrap.DeploymentID].BillingMode != domain.BillingModeFree {
		t.Fatal("import lost the pricing configuration or bypassed the deployment activation gate")
	}
	if len(stored.GatewayKeys) != 1 || stored.GatewayKeys[0].KeyHash == key.KeyHash ||
		!domain.HasGatewayScope(stored.GatewayKeys[0].Scopes, domain.GatewayScopeDiscovery) {
		t.Fatal("key hash was reused or the scope was lost")
	}

	models := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		target.gatewayRouter().ServeHTTP(response, req)
		return response
	}
	if response := models(bootstrap.GatewayKey); response.Code != http.StatusUnauthorized {
		t.Fatalf("source key authenticated on target: %d", response.Code)
	}
	if response := models(freshKey); response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"id":"chat"`) {
		t.Fatalf("target key failed authentication or the imported route served before validation: %d %s", response.Code, response.Body)
	}
	deployment := stored.Deployments[0]
	probed := probeDeploymentForTest(t, target, session.cookie, session.csrf, bootstrap.ProviderID, probedDeployment{ID: deployment.ID})
	enabled := updateDeploymentForTest(t, target, session.cookie, session.csrf, probed, map[string]any{
		"name": deployment.Name, "provider_id": deployment.ProviderID, "provider_model": deployment.ProviderModel,
		"binding_id": deployment.BindingID, "profile_id": deployment.ProfileID,
		"enabled": true, "max_concurrency": deployment.MaxConcurrency,
	})
	if enabled.Code != http.StatusOK {
		t.Fatalf("enable validated deployment: %d %s", enabled.Code, enabled.Body)
	}
	route := stored.Routes[0]
	routeEnabled := performAdminMutation(t, target, session.cookie, session.csrf, http.MethodPut, "/admin/api/v1/routes/"+route.ID, `"1"`, routeInput{
		PublicModel: route.PublicModel, DeploymentID: route.DeploymentID, Priority: route.Priority, Strategy: route.Strategy, Enabled: true,
	})
	if routeEnabled.Code != http.StatusOK {
		t.Fatalf("enable route: %d %s", routeEnabled.Code, routeEnabled.Body)
	}
	if response := models(freshKey); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"chat"`) {
		t.Fatalf("imported route did not become available after target validation: %d %s", response.Code, response.Body)
	}
}
