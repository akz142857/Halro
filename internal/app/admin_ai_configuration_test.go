package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestAdminAIConfigurationPublishAlias(t *testing.T) {
	ctx := context.Background()
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	cookie, csrf := loginAdminForTest(t, runtime)
	deployment, err := runtime.store.GetDeployment(ctx, bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	project, err := runtime.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rollbackRoute := domain.Route{ID: "rte_rollback_ai", PublicModel: "rollback-alias", DeploymentID: deployment.ID,
		Enabled: true, Strategy: "ordered", CreatedAt: now, UpdatedAt: now}
	committed, err := runtime.store.PublishProjectAlias(ctx, rollbackRoute, project.ID, project.Revision, deployment.Revision, &domain.AdminAuditIntent{})
	if committed || err == nil {
		t.Fatal("route and project committed without an audit record")
	}
	if _, err := runtime.store.GetRoute(ctx, rollbackRoute.ID); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("aborted transaction left a route: %v", err)
	}
	if unchanged, err := runtime.store.GetProject(ctx, project.ID); err != nil || unchanged.Revision != project.Revision ||
		!slices.Equal(unchanged.AllowedModels, project.AllowedModels) {
		t.Fatalf("aborted transaction changed the project: %+v %v", unchanged, err)
	}
	other := project
	other.ID, other.Name, other.Revision = "prj_ai_other", "Other project canary", 0
	other.AllowedModels = []string{"chat", "retired-alias"}
	if _, err := runtime.store.PutProject(ctx, other, 0, nil); err != nil {
		t.Fatal(err)
	}
	otherPlaintext, otherKey, err := auth.GenerateGatewayKey(other.ID, "Discovery", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherKey.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeDiscovery}
	if _, err := runtime.store.PutGatewayKey(ctx, otherKey, 0, nil); err != nil {
		t.Fatal(err)
	}
	runtime.activateAuthSnapshot()

	skillPath := "/admin/api/v1/skill.md?" + url.Values{
		"deployment_id": {deployment.ID}, "project_id": {project.ID}, "locale": {"zh-CN"},
	}.Encode()
	anonymous := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, skillPath, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous export: %d", anonymous.Code)
	}
	skill := authenticatedAdminGet(t, runtime, cookie, skillPath)
	if skill.Code != http.StatusOK || skill.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(skill.Header().Get("Content-Disposition"), "SKILL.md") {
		t.Fatalf("skill export: %d %s", skill.Code, skill.Body)
	}
	for _, value := range []string{runtime.instanceID, deployment.ID, project.ID, aiPublishAlias, "实例快照"} {
		if !strings.Contains(skill.Body.String(), value) {
			t.Fatalf("skill missing %q", value)
		}
	}
	for _, value := range []string{bootstrap.GatewayKey, other.ID, other.Name, bootstrap.ProviderID, "provider-secret", "api.openai.com"} {
		if strings.Contains(skill.Body.String(), value) {
			t.Fatal("skill discloses unselected data")
		}
	}

	post := func(path string, input any) *httptest.ResponseRecorder {
		t.Helper()
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/configuration/ai/"+path, "", input)
	}
	plan := aiAliasPlan{
		Version: 1, InstanceID: runtime.instanceID, Action: aiPublishAlias, PublicModel: "support-chat",
		DeploymentID: deployment.ID, DeploymentRevision: deployment.Revision, ProjectID: project.ID, ProjectRevision: project.Revision,
	}
	invalid := plan
	invalid.InstanceID = "another-instance"
	if response := post("preview", invalid); response.Code != http.StatusBadRequest {
		t.Fatalf("other instance: %d %s", response.Code, response.Body)
	}
	invalid = plan
	invalid.PublicModel = "retired-alias"
	if response := post("preview", invalid); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ai_configuration_alias_in_use") {
		t.Fatalf("orphan authorization: %d %s", response.Code, response.Body)
	}
	invalid.PublicModel = "chat"
	if response := post("preview", invalid); response.Code != http.StatusConflict {
		t.Fatalf("existing route: %d %s", response.Code, response.Body)
	}

	response := post("preview", plan)
	var preview aiAliasPreview
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &preview) != nil ||
		preview.PreviewID == "" || preview.Deployment == nil || preview.Deployment.ID != deployment.ID ||
		preview.Project == nil || preview.Project.ID != project.ID {
		t.Fatalf("preview: %d %s", response.Code, response.Body)
	}
	assertDiscovery := func(key string, wantNew bool) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		runtime.gatewayRouter().ServeHTTP(response, request)
		var models openaiapi.ModelList
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &models) != nil {
			t.Fatalf("discovery: %d %s", response.Code, response.Body)
		}
		hasNew := slices.ContainsFunc(models.Data, func(model openaiapi.Model) bool { return model.ID == plan.PublicModel })
		hasOriginal := slices.ContainsFunc(models.Data, func(model openaiapi.Model) bool { return model.ID == "chat" })
		if hasNew != wantNew || !hasOriginal {
			t.Fatalf("discovery aliases: %+v", models.Data)
		}
	}
	assertDiscovery(bootstrap.GatewayKey, false)
	stored, err := runtime.store.GetProject(ctx, project.ID)
	if err != nil || stored.Revision != project.Revision || !slices.Equal(stored.AllowedModels, project.AllowedModels) {
		t.Fatalf("preview wrote project: %+v %v", stored, err)
	}
	input := aiAliasApply{Plan: plan, PreviewID: preview.PreviewID}
	tampered := input
	tampered.Plan.PublicModel = "different-alias"
	if response := post("apply", tampered); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ai_configuration_preview_required") {
		t.Fatalf("tampered plan: %d %s", response.Code, response.Body)
	}
	if response := post("apply", input); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "recent_reauth_required") {
		t.Fatalf("missing step-up: %d %s", response.Code, response.Body)
	}

	// A project revision changing after preview invalidates the earlier review.
	stored.Name = "Support"
	stored, err = runtime.store.PutProject(ctx, stored, stored.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response := post("apply", input); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ai_configuration_stale") {
		t.Fatalf("stale plan: %d %s", response.Code, response.Body)
	}
	plan.ProjectRevision = stored.Revision
	response = post("preview", plan)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.PreviewID == "" {
		t.Fatalf("fresh preview: %d %s", response.Code, response.Body)
	}
	input = aiAliasApply{Plan: plan, PreviewID: preview.PreviewID, stepUpMaterial: stepUpMaterial{CurrentPassword: "correct horse battery staple"}}
	response = post("apply", input)
	var result aiAliasResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		result.Verification != "verified" || result.OperationID == "" || response.Header().Get("Halro-Operation-Id") != result.OperationID {
		t.Fatalf("apply: %d %s", response.Code, response.Body)
	}
	assertDiscovery(bootstrap.GatewayKey, true)
	assertDiscovery(otherPlaintext, false)

	response = post("apply", input)
	var replay aiAliasResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &replay) != nil ||
		replay.Status != "already_applied" || replay.OperationID != result.OperationID || replay.RouteID != result.RouteID || replay.Verification != "verified" {
		t.Fatalf("replay: %d %s", response.Code, response.Body)
	}
	stored, err = runtime.store.GetProject(ctx, project.ID)
	if err != nil || stored.Revision != plan.ProjectRevision+1 || !slices.Equal(stored.AllowedModels, []string{"chat", "support-chat"}) {
		t.Fatalf("project state: %+v %v", stored, err)
	}
	unmodified, err := runtime.store.GetProject(ctx, other.ID)
	if err != nil || !slices.Equal(unmodified.AllowedModels, other.AllowedModels) || unmodified.Revision != 1 {
		t.Fatalf("other project: %+v %v", unmodified, err)
	}
	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit?limit=40")
	var auditPage struct{ Items []auditRecordView }
	if audit.Code != http.StatusOK || json.Unmarshal(audit.Body.Bytes(), &auditPage) != nil {
		t.Fatalf("audit: %d %s", audit.Code, audit.Body)
	}
	var records []auditRecordView
	for _, record := range auditPage.Items {
		if record.Action == "configuration.ai_apply" {
			records = append(records, record)
		}
	}
	if len(records) != 1 || records[0].EventID != result.OperationID || records[0].TargetID != result.RouteID ||
		records[0].Metadata["project_id"] != project.ID || records[0].Metadata["public_model"] != plan.PublicModel {
		t.Fatalf("audit must record the applied operation once: %+v", records)
	}

	// A later grant to another project invalidates the original isolation even
	// though the route and the intended grant still exist.
	unmodified.AllowedModels = append(unmodified.AllowedModels, plan.PublicModel)
	if _, err := runtime.store.PutProject(ctx, unmodified, unmodified.Revision, nil); err != nil {
		t.Fatal(err)
	}
	runtime.activateAuthSnapshot()
	assertDiscovery(otherPlaintext, true)
	response = post("preview", plan)
	var changed aiAliasPreview
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &changed) != nil ||
		changed.Result == nil || changed.Result.Verification != "configuration_changed" || changed.Result.OperationID != result.OperationID {
		t.Fatalf("recheck after scope change: %d %s", response.Code, response.Body)
	}
}
