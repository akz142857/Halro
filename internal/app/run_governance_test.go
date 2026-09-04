package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
)

func enableRunGovernanceForTest(t *testing.T, runtime *Runtime, projectID, keyID string) {
	t.Helper()
	project, err := runtime.store.GetProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	project.RunGovernance = domain.RunGovernanceConfig{
		Enabled: true, DefaultRunBudgetMicrosUSD: 1_000, MaxRunBudgetMicrosUSD: 10_000,
		DefaultRunTTLSeconds: 3600, MaxRunTTLSeconds: 86400,
		MaxActiveRuns: 10, MaxOpenWorkUnits: 10,
	}
	if _, err := runtime.store.PutProject(context.Background(), project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	key, err := runtime.store.GetGatewayKey(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	key.Scopes = []domain.GatewayScope{
		domain.GatewayScopeInference,
		domain.GatewayScopeWorkUnitCreate,
		domain.GatewayScopeRunCreate,
		domain.GatewayScopeRunAttach,
		domain.GatewayScopeGovernanceRead,
	}
	if _, err := runtime.store.PutGatewayKey(context.Background(), key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.auth.Refresh(context.Background(), runtime.store); err != nil {
		t.Fatal(err)
	}
}

func governanceRequest(method, path, key, idempotencyKey, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return request
}

func TestRunGovernanceHTTPScopesLifecycleAndIdempotency(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	router := runtime.gatewayRouter()

	disabled := httptest.NewRecorder()
	router.ServeHTTP(disabled, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "wu-disabled", `{}`))
	if disabled.Code != http.StatusForbidden || !strings.Contains(disabled.Body.String(), "run_governance_disabled") {
		t.Fatalf("disabled Project status=%d body=%s", disabled.Code, disabled.Body.String())
	}

	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	created := httptest.NewRecorder()
	router.ServeHTTP(created, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "wu-create", `{}`))
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" || created.Header().Get("X-Request-ID") == "" {
		t.Fatalf("create Work Unit status=%d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	var workUnit domain.WorkUnit
	if err := json.Unmarshal(created.Body.Bytes(), &workUnit); err != nil || !domain.ValidWorkUnitID(workUnit.ID) {
		t.Fatalf("Work Unit response=%#v err=%v", workUnit, err)
	}
	replayed := httptest.NewRecorder()
	router.ServeHTTP(replayed, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "wu-create", `{}`))
	var replayedWorkUnit domain.WorkUnit
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedWorkUnit); err != nil || replayed.Code != http.StatusOK || replayedWorkUnit.ID != workUnit.ID {
		t.Fatalf("Work Unit replay status=%d body=%s err=%v", replayed.Code, replayed.Body.String(), err)
	}

	createRunBody := `{"work_unit_id":"` + workUnit.ID + `","budget_micros_usd":2000,"ttl_seconds":3600}`
	createdRun := httptest.NewRecorder()
	router.ServeHTTP(createdRun, governanceRequest(http.MethodPost, "/halro/v1/runs", bootstrap.GatewayKey, "run-create", createRunBody))
	var run domain.Run
	if err := json.Unmarshal(createdRun.Body.Bytes(), &run); err != nil || createdRun.Code != http.StatusCreated || !domain.ValidRunID(run.ID) ||
		run.BudgetState != domain.RunBudgetAvailable || run.RemainingMicrosUSD != 2_000 {
		t.Fatalf("Run response status=%d body=%s err=%v", createdRun.Code, createdRun.Body.String(), err)
	}
	conflict := httptest.NewRecorder()
	router.ServeHTTP(conflict, governanceRequest(http.MethodPost, "/halro/v1/runs", bootstrap.GatewayKey, "run-create",
		`{"work_unit_id":"`+workUnit.ID+`","budget_micros_usd":3000,"ttl_seconds":3600}`))
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "idempotency_conflict") {
		t.Fatalf("Run idempotency conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	read := httptest.NewRecorder()
	router.ServeHTTP(read, governanceRequest(http.MethodGet, "/halro/v1/runs/"+run.ID, bootstrap.GatewayKey, "", ""))
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"work_unit_id":"`+workUnit.ID+`"`) {
		t.Fatalf("Run read status=%d body=%s", read.Code, read.Body.String())
	}

	key, err := runtime.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference}
	if _, err := runtime.store.PutGatewayKey(context.Background(), key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.auth.Refresh(context.Background(), runtime.store); err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	router.ServeHTTP(denied, governanceRequest(http.MethodGet, "/halro/v1/runs/"+run.ID, bootstrap.GatewayKey, "", ""))
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "gateway_key_scope_denied") {
		t.Fatalf("scope denial status=%d body=%s", denied.Code, denied.Body.String())
	}
}

func TestRunGovernanceHTTPRejectsBodyBeforeMutation(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	router := runtime.gatewayRouter()
	for name, request := range map[string]*http.Request{
		"missing idempotency": governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "", `{}`),
		"unknown field":       governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "unknown", `{"caller_id":"wku_external"}`),
		"wrong content type":  governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "content", `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "wrong content type" {
				request.Header.Set("Content-Type", "text/plain")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code < 400 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if got := len(runtime.accounting.WorkUnits(bootstrap.ProjectID)); got != 0 {
		t.Fatalf("invalid requests created %d Work Units", got)
	}
}

func TestAdminRunGovernanceListsAndDrillsIntoLedgerState(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	router := runtime.gatewayRouter()

	created := httptest.NewRecorder()
	router.ServeHTTP(created, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "admin-wu", `{}`))
	var workUnit domain.WorkUnit
	if err := json.Unmarshal(created.Body.Bytes(), &workUnit); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create Work Unit status=%d body=%s err=%v", created.Code, created.Body.String(), err)
	}
	createdRun := httptest.NewRecorder()
	router.ServeHTTP(createdRun, governanceRequest(http.MethodPost, "/halro/v1/runs", bootstrap.GatewayKey, "admin-run",
		`{"work_unit_id":"`+workUnit.ID+`","budget_micros_usd":2000,"ttl_seconds":3600}`))
	var run domain.Run
	if err := json.Unmarshal(createdRun.Body.Bytes(), &run); err != nil || createdRun.Code != http.StatusCreated {
		t.Fatalf("create Run status=%d body=%s err=%v", createdRun.Code, createdRun.Body.String(), err)
	}

	cookie, _ := loginAdminForTest(t, runtime)
	get := func(path string) *httptest.ResponseRecorder {
		request := adminRequest(t, http.MethodGet, path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}
	for name, path := range map[string]string{
		"work unit list":   "/admin/api/v1/run-governance/work-units?project_id=" + bootstrap.ProjectID,
		"work unit detail": "/admin/api/v1/run-governance/work-units/" + workUnit.ID,
		"run list":         "/admin/api/v1/run-governance/runs?work_unit_id=" + workUnit.ID,
		"run detail":       "/admin/api/v1/run-governance/runs/" + run.ID,
	} {
		response := get(path)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), workUnit.ID) {
			t.Fatalf("%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	if response := get("/admin/api/v1/run-governance/runs?unknown=true"); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRunGovernanceCloseNormalizesReasonAndDistinguishesMissingWorkUnit(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	router := runtime.gatewayRouter()

	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, governanceRequest(http.MethodPost, "/halro/v1/work-units/wku_missing/close", bootstrap.GatewayKey, "missing", `{}`))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "work_unit_not_found") {
		t.Fatalf("missing Work Unit status=%d body=%s", missing.Code, missing.Body.String())
	}

	created := httptest.NewRecorder()
	router.ServeHTTP(created, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "reason-wu", `{}`))
	var workUnit domain.WorkUnit
	if err := json.Unmarshal(created.Body.Bytes(), &workUnit); err != nil {
		t.Fatal(err)
	}
	createdRun := httptest.NewRecorder()
	router.ServeHTTP(createdRun, governanceRequest(http.MethodPost, "/halro/v1/runs", bootstrap.GatewayKey, "reason-run",
		`{"work_unit_id":"`+workUnit.ID+`","budget_micros_usd":2000,"ttl_seconds":3600}`))
	var run domain.Run
	if err := json.Unmarshal(createdRun.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	closed := httptest.NewRecorder()
	router.ServeHTTP(closed, governanceRequest(http.MethodPost, "/halro/v1/runs/"+run.ID+"/close", bootstrap.GatewayKey, "close-run", `{"reason":"  completed_by_agent  "}`))
	if closed.Code != http.StatusCreated || !strings.Contains(closed.Body.String(), `"close_reason":"completed_by_agent"`) {
		t.Fatalf("close Run status=%d body=%s", closed.Code, closed.Body.String())
	}
}

func TestLegacyAdminUpdatesPreserveRunGovernanceAndGatewayScopes(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	cookie, csrf := loginAdminForTest(t, runtime)

	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"name": project.Name + " updated", "enabled": project.Enabled, "allowed_models": project.AllowedModels,
		"rpm": project.RPM, "tpm": project.TPM, "max_concurrency": project.MaxConcurrency,
		"daily_budget_micros_usd": project.DailyBudgetMicrosUSD, "max_input_tokens": project.MaxInputTokens,
		"max_output_tokens": project.MaxOutputTokens, "max_request_bytes": project.MaxRequestBytes,
		"max_stream_duration_seconds": int64(project.MaxStreamDuration.Seconds()),
		"deferred_responses":          project.DeferredResponses, "max_deferred_queue": project.MaxDeferredQueue,
		"allowed_cidrs": []string{}, "redaction_policy_id": project.RedactionPolicyID,
		"token_guard_policy_id": project.TokenGuardPolicyID,
	}
	updated := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/projects/"+project.ID, `"`+strconv.FormatUint(project.Revision, 10)+`"`, body)
	if updated.Code != http.StatusOK {
		t.Fatalf("legacy Project update status=%d body=%s", updated.Code, updated.Body.String())
	}
	project, err = runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil || !project.RunGovernance.Enabled || project.RunGovernance.DefaultRunBudgetMicrosUSD != 1_000 {
		t.Fatalf("legacy Project update lost Run Governance: %#v err=%v", project.RunGovernance, err)
	}

	key, err := runtime.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	keyUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/projects/"+project.ID+"/keys/"+key.ID, `"`+strconv.FormatUint(key.Revision, 10)+`"`,
		map[string]any{"name": key.Name + " updated", "enabled": key.Enabled})
	if keyUpdate.Code != http.StatusOK || !strings.Contains(keyUpdate.Body.String(), `"run:attach"`) {
		t.Fatalf("legacy Key update status=%d body=%s", keyUpdate.Code, keyUpdate.Body.String())
	}
	key, err = runtime.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil || !domain.HasGatewayScope(key.Scopes, domain.GatewayScopeRunAttach) {
		t.Fatalf("legacy Key update lost scopes: %v err=%v", key.Scopes, err)
	}
}

func TestAdminCannotDisableRunGovernanceWhileARunIsActive(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	workUnit, _, err := runtime.accounting.CreateWorkUnit(context.Background(), bootstrap.ProjectID, bootstrap.KeyID, 10,
		budget.GovernanceIntent{Operation: "test.wu", IdempotencyKeyHash: "sha256:" + strings.Repeat("a", 64), RequestFingerprint: "sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.accounting.CreateRun(context.Background(), bootstrap.ProjectID, bootstrap.KeyID, workUnit.ID, 1_000, time.Hour, 10,
		budget.GovernanceIntent{Operation: "test.run", IdempotencyKeyHash: "sha256:" + strings.Repeat("c", 64), RequestFingerprint: "sha256:" + strings.Repeat("d", 64)}); err != nil {
		t.Fatal(err)
	}
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdminForTest(t, runtime)
	body := map[string]any{
		"name": project.Name, "enabled": project.Enabled, "allowed_models": project.AllowedModels,
		"rpm": project.RPM, "tpm": project.TPM, "max_concurrency": project.MaxConcurrency,
		"daily_budget_micros_usd": project.DailyBudgetMicrosUSD, "max_input_tokens": project.MaxInputTokens,
		"max_output_tokens": project.MaxOutputTokens, "max_request_bytes": project.MaxRequestBytes,
		"max_stream_duration_seconds": int64(project.MaxStreamDuration.Seconds()), "deferred_responses": project.DeferredResponses,
		"max_deferred_queue": project.MaxDeferredQueue, "allowed_cidrs": []string{}, "redaction_policy_id": project.RedactionPolicyID,
		"token_guard_policy_id": project.TokenGuardPolicyID, "run_governance": domain.RunGovernanceConfig{},
	}
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut, "/admin/api/v1/projects/"+project.ID,
		`"`+strconv.FormatUint(project.Revision, 10)+`"`, body)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "run_governance_active_runs") || !strings.Contains(response.Body.String(), `"active_runs":1`) {
		t.Fatalf("disable with active Run status=%d body=%s", response.Code, response.Body.String())
	}
}
