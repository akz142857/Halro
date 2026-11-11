package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
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
	project.AllowedModels = append(project.AllowedModels, "retired-alias")
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
	sourceDeployment.OperatorDisabled = []string{"images"}
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
	if preview.PlanID == "" || len(preview.Issues) != 0 || len(preview.DeploymentsToEnable) != 1 || len(preview.RoutesToEnable) != 1 ||
		len(preview.Declarations) != 1 || len(preview.Notices) != 1 || preview.Notices[0].Alias != "retired-alias" {
		t.Fatalf("valid configuration did not produce a plan: %+v", preview)
	}
	apply := configurationApplyInput{configurationImportInput: input, PlanID: preview.PlanID}
	for i := 0; i < 6; i++ {
		if response := request("", apply); response.Code != http.StatusUnauthorized {
			t.Fatalf("empty re-authentication prompt %d: %d", i, response.Code)
		}
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
	if response := request("", apply); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "configuration_declarations_required") {
		t.Fatalf("missing declaration approval: %d %s", response.Code, response.Body)
	}
	apply.AcceptOperatorDeclarations = true
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
	if err := target.drainAdminAuditIntents(ctx); err != nil {
		t.Fatal(err)
	}
	audit := findAudit(auditActions(t, target, session.cookie), auditOperatorCapabilitiesDeclared)
	metadata, _ := audit["metadata"].(map[string]any)
	if metadata["provider_model"] != sourceDeployment.ProviderModel || metadata["capabilities"] == "" ||
		metadata["configuration_import_id"] != imported.Header().Get("Halro-Operation-Id") {
		t.Fatalf("missing auditable declaration: %v", audit)
	}

	stored, err := target.store.ReadPortableConfiguration(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Providers) != 1 || stored.Providers[0].CredentialID != credential.ID || stored.Providers[0].BaseURL != targetBaseURL {
		t.Fatal("target did not bind the selected credential and endpoint")
	}
	if len(stored.Projects) != 1 || stored.Projects[0].DailyBudgetMicrosUSD != project.DailyBudgetMicrosUSD ||
		stored.Projects[0].RedactionPolicyID != project.RedactionPolicyID || stored.Projects[0].TokenGuardPolicyID != project.TokenGuardPolicyID ||
		!slices.Equal(stored.Projects[0].AllowedModels, project.AllowedModels) {
		t.Fatal("project settings or policy bindings were lost")
	}
	if len(stored.Deployments) != 1 || stored.Deployments[0].Enabled || stored.Deployments[0].LastTestStatus != "" ||
		!slices.Contains(stored.Deployments[0].OperatorDisabled, "images") ||
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

func TestConfigurationImportKeepsMappedModelWhenEnabled(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	ctx := context.Background()
	endpoint := "https://review-target.openai.azure.com"
	credentialResponse := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "review fixture", "type": "azure_openai", "base_url": endpoint, "secret": "review-only-fixture",
	})
	var credential struct{ ID string }
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body)
	}
	caps := domain.ProviderCapabilities{Chat: true, Streaming: true}
	bundle := configurationBundle{Version: 1,
		Providers: []configurationProvider{{ID: "azure_review", Name: "Azure", Type: domain.ProviderAzureOpenAI, BaseURL: endpoint, APIVersion: "2024-10-21", Enabled: true,
			Bindings: []configurationBinding{{ProfileID: domain.ProfileAzureChatEmbeddings, Capabilities: caps, Enabled: true}}}},
		Deployments: []configurationDeployment{{ID: "dep_review", Name: "Review", ProviderID: "azure_review", ProviderModel: "review-tenant-alias",
			CapabilityModel: "gpt-5", TargetKind: domain.TargetAzureDeployment, ProfileID: domain.ProfileAzureChatEmbeddings, Capabilities: caps, Enabled: true,
			Price: &configurationPrice{BillingMode: domain.BillingModeFree}}},
	}
	input := configurationImportInput{Bundle: bundle, Bindings: map[string]configurationProviderTarget{"azure_review": {CredentialID: credential.ID}}}
	response := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/configuration/import/preview", "", input)
	var plan configurationImportPreview
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &plan) != nil || plan.PlanID == "" {
		t.Fatalf("preview status=%d body=%s", response.Code, response.Body)
	}
	applied := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/configuration/import", "",
		configurationApplyInput{configurationImportInput: input, stepUpMaterial: stepUpMaterial{CurrentPassword: stepUpTestPassword}, PlanID: plan.PlanID, AcceptOperatorDeclarations: true})
	if applied.Code != http.StatusCreated {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body)
	}
	before, err := runtime.store.GetDeployment(ctx, "dep_review")
	if err != nil || before.ModelCapabilitySnapshot.CanonicalModelRef != "gpt-5" {
		t.Fatalf("mapping not imported: %v", err)
	}
	probed := probeDeploymentForTest(t, runtime, session.cookie, session.csrf, before.ProviderID, probedDeployment{ID: before.ID})
	enabled := updateDeploymentForTest(t, runtime, session.cookie, session.csrf, probed, map[string]any{
		"name": before.Name, "provider_id": before.ProviderID, "binding_id": before.BindingID, "provider_model": before.ProviderModel,
		"target_kind": before.TargetKind, "capabilities": before.Capabilities, "region": before.Region, "max_concurrency": before.MaxConcurrency, "enabled": true,
	})
	if enabled.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", enabled.Code, enabled.Body)
	}
	after, err := runtime.store.GetDeployment(ctx, before.ID)
	if err != nil || after.ModelCapabilitySnapshot.CanonicalModelRef != "gpt-5" {
		t.Fatalf("enable dropped the imported model mapping: %v", err)
	}
	if after.LastTestRevision != after.Revision {
		t.Fatal("enable left the test on the prior revision")
	}
}

func TestConfigurationImportReportsLocalCommitWhenConfirmationFails(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	ctx := context.Background()
	if err := runtime.store.SetMetadataJournalAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) {
		return 1, nil
	}, func(context.Context, uint64) error {
		return errors.New("replica confirmation fixture")
	}); err != nil {
		t.Fatal(err)
	}
	input := configurationImportInput{Bundle: configurationBundle{
		Version:     1,
		Projects:    []configurationProject{{ID: "project_fixture", projectInput: projectInput{Name: "Disabled project"}}},
		GatewayKeys: []configurationKey{{ID: "key_fixture", ProjectID: "project_fixture", Name: "Fixture key", Enabled: true, Scopes: []domain.GatewayScope{domain.GatewayScopeInference}}},
	}}
	request := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/configuration/import"+path, "", body)
	}
	var preview configurationImportPreview
	if response := request("/preview", input); response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.PlanID == "" {
		t.Fatalf("preview status=%d body=%s", response.Code, response.Body)
	}
	apply := configurationApplyInput{
		configurationImportInput: input, PlanID: preview.PlanID, stepUpMaterial: stepUpMaterial{CurrentPassword: stepUpTestPassword},
	}
	response := request("", apply)
	var outcome struct {
		Status        string                   `json:"status"`
		OperationID   string                   `json:"operation_id"`
		KeysToReissue []configurationIssuedKey `json:"keys_to_reissue"`
	}
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &outcome) != nil ||
		outcome.Status != "confirmation_unavailable" || outcome.OperationID == "" || outcome.OperationID != response.Header().Get("Halro-Operation-Id") ||
		len(outcome.KeysToReissue) != 1 || outcome.KeysToReissue[0].ID != "key_fixture" ||
		outcome.KeysToReissue[0].Key != "" || strings.Contains(response.Body.String(), `"gateway_keys":[`) {
		t.Fatalf("wrong HA outcome: status=%d body=%s", response.Code, response.Body)
	}
	if key, err := runtime.store.GetGatewayKey(ctx, "key_fixture"); err != nil || key.ProjectID != "project_fixture" {
		t.Fatalf("local commit missing: %v", err)
	}
	if replay := request("", apply); replay.Code != http.StatusConflict {
		t.Fatalf("confirmation loss permitted a second import: %d", replay.Code)
	}
}

func TestConfigurationExportOmitsRetiredDependents(t *testing.T) {
	source, bootstrap, session := activationTestRuntime(t)
	deleteRequest := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+bootstrap.ProjectID, session, stepUp())
	deleteRequest.Header.Set("If-Match", revisionETag(1))
	removed := httptest.NewRecorder()
	source.adminRouter().ServeHTTP(removed, deleteRequest)
	if removed.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", removed.Code, removed.Body)
	}
	configuration, err := source.store.ReadPortableConfiguration(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Retiring providers/deployments may also leave their disabled dependents.
	deleted := time.Now().UTC()
	configuration.Providers = append(configuration.Providers, domain.ProviderInstance{ID: "retired_provider", DeletedAt: &deleted})
	configuration.Deployments = append(configuration.Deployments, domain.Deployment{ID: "dangling_deployment", ProviderID: "retired_provider"})
	configuration.Routes = append(configuration.Routes, domain.Route{ID: "dangling_route", DeploymentID: "dangling_deployment"})
	bundle := exportConfiguration(configuration)
	if len(bundle.Projects) != 0 || len(bundle.GatewayKeys) != 0 || len(bundle.Deployments) != 1 || len(bundle.Routes) != 1 ||
		!slices.Contains(bundle.Omissions, configurationNotice{Resource: "gateway_keys", ID: bootstrap.KeyID, Code: "retired_project"}) ||
		!slices.Contains(bundle.Omissions, configurationNotice{Resource: "routes", ID: "dangling_route", Code: "retired_deployment"}) {
		t.Fatalf("export did not close references: %+v", bundle.Omissions)
	}
}

func TestConfigurationImportDerivesMantleDeploymentRegionFromTarget(t *testing.T) {
	profile := domain.ProfileBedrockMantleOpenAIChat
	providerID := "prv_mantle_fixture"
	capabilities := domain.ProviderCapabilities{Chat: true}
	instance := domain.ProviderInstance{
		ID: providerID, Type: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockMantle,
		BaseURL: "https://bedrock-mantle.us-west-2.api.aws", ProfileID: profile,
		Bindings: []domain.ProviderProfileBinding{{
			ID: domain.DefaultProviderProfileBindingID(providerID, profile), ProviderID: providerID,
			ProfileID: profile, AccessSurface: domain.SurfaceBedrockMantle, Enabled: true,
			Capabilities: capabilities, CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		}},
	}
	item := configurationDeployment{
		ID: "dep_mantle_fixture", Name: "Mantle", ProviderID: providerID, ProviderModel: "review-model",
		Region: "us-east-1", ProfileID: profile, Capabilities: capabilities,
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	deployment, err := (&Runtime{}).prepareConfigurationDeployment(request, instance, item, time.Now().UTC())
	if err != nil || deployment.Region != "us-west-2" {
		t.Fatalf("target deployment region=%q err=%v", deployment.Region, err)
	}
}
