package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
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
		domain.GatewayScopeOutcomeWrite,
	}
	if _, err := runtime.store.PutGatewayKey(context.Background(), key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.auth.Refresh(context.Background(), runtime.store); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeHTTPFreezesDefinitionAndReplaysIdempotently(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_acceptance", ProjectID: bootstrap.ProjectID, Name: "accepted", Version: 1,
		DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	definition, err = runtime.store.PutOutcomeDefinition(context.Background(), definition, project.Revision, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := runtime.gatewayRouter()
	created := httptest.NewRecorder()
	router.ServeHTTP(created, governanceRequest(http.MethodPost, "/halro/v1/work-units", bootstrap.GatewayKey, "outcome-wu", `{"outcome_definition_ids":["`+definition.ID+`"]}`))
	var workUnit domain.WorkUnit
	if err := json.Unmarshal(created.Body.Bytes(), &workUnit); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create Work Unit status=%d body=%s err=%v", created.Code, created.Body.String(), err)
	}
	body := `{"definition_id":"` + definition.ID + `","value":"accepted","observed_at":"` + now.Format(time.RFC3339Nano) + `","evidence_ref":"acceptance_42"}`
	reported := httptest.NewRecorder()
	router.ServeHTTP(reported, governanceRequest(http.MethodPost, "/halro/v1/work-units/"+workUnit.ID+"/outcomes", bootstrap.GatewayKey, "outcome-one", body))
	var outcome domain.Outcome
	if err := json.Unmarshal(reported.Body.Bytes(), &outcome); err != nil || reported.Code != http.StatusCreated || !outcome.Provisional || outcome.DefinitionVersion != 1 {
		t.Fatalf("report status=%d body=%s err=%v", reported.Code, reported.Body.String(), err)
	}
	replayed := httptest.NewRecorder()
	router.ServeHTTP(replayed, governanceRequest(http.MethodPost, "/halro/v1/work-units/"+workUnit.ID+"/outcomes", bootstrap.GatewayKey, "outcome-one", body))
	var again domain.Outcome
	if err := json.Unmarshal(replayed.Body.Bytes(), &again); err != nil || replayed.Code != http.StatusOK || again.ID != outcome.ID {
		t.Fatalf("replay status=%d body=%s err=%v", replayed.Code, replayed.Body.String(), err)
	}
	unsafe := httptest.NewRecorder()
	router.ServeHTTP(unsafe, governanceRequest(http.MethodPost, "/halro/v1/work-units/"+workUnit.ID+"/outcomes", bootstrap.GatewayKey, "outcome-secret", strings.Replace(body, "acceptance_42", "https://example.test/token=secret", 1)))
	if unsafe.Code != http.StatusBadRequest || !strings.Contains(unsafe.Body.String(), "invalid_outcome") {
		t.Fatalf("unsafe evidence status=%d body=%s", unsafe.Code, unsafe.Body.String())
	}
	closed := httptest.NewRecorder()
	router.ServeHTTP(closed, governanceRequest(http.MethodPost, "/halro/v1/work-units/"+workUnit.ID+"/close", bootstrap.GatewayKey, "outcome-close", `{}`))
	if closed.Code != http.StatusCreated {
		t.Fatalf("close status=%d body=%s", closed.Code, closed.Body.String())
	}
	cookie, _ := loginAdminForTest(t, runtime)
	query := "/admin/api/v1/governance/summary?project_id=" + bootstrap.ProjectID + "&definition_id=" + definition.ID + "&definition_version=1&cohort_start=" + workUnit.PeriodID + "&cohort_end=" + workUnit.PeriodID
	summaryRequest := adminRequest(t, http.MethodGet, query, nil)
	summaryRequest.AddCookie(cookie)
	summaryResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(summaryResponse, summaryRequest)
	if summaryResponse.Code != http.StatusOK || !strings.Contains(summaryResponse.Body.String(), `"eligible_units":1`) || !strings.Contains(summaryResponse.Body.String(), `"successful_units":1`) || !strings.Contains(summaryResponse.Body.String(), `"success_rate":1`) {
		t.Fatalf("summary status=%d body=%s", summaryResponse.Code, summaryResponse.Body.String())
	}
}

func TestGovernanceCorruptionDoesNotBlockRuntimeOrAccounting(t *testing.T) {
	runtime, _, reopen := openBootstrappedRuntime(t)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	path := runtime.config.GovernancePath()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 {
		// Create a syntactically non-empty journal that cannot pass the HGOV header check.
		payload = []byte("broken-governance-frame-that-is-long-enough-to-be-non-partial")
	} else {
		payload[0] ^= 1
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := reopen()
	defer recovered.Close()
	if ready, _ := recovered.governance.manager.Ready(); ready {
		t.Fatal("corrupt Governance Journal reported ready")
	}
	event := ledger.Event{EventID: "evt_after_governance_failure", Kind: ledger.EventRequestAccepted, RequestID: "req_after_governance_failure", ProjectID: "project_test", PeriodID: "2026-09-04", OccurredAt: time.Now().UTC()}
	if _, err := recovered.ledger.Append(context.Background(), event); err != nil {
		t.Fatalf("Accounting append was poisoned by Governance failure: %v", err)
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

func TestAdminOutcomeDefinitionVersionsAreImmutable(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, revision uint64, body any) *httptest.ResponseRecorder {
		request := adminRequest(t, http.MethodPost, path, body)
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("If-Match", revisionETag(revision))
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}
	created := post("/admin/api/v1/projects/"+bootstrap.ProjectID+"/outcome-definitions", project.Revision, map[string]any{"name": "ticket_resolved", "data_type": "CATEGORICAL", "allowed_values": []string{"accepted", "rejected"}, "success_values": []string{"accepted"}})
	var first domain.OutcomeDefinition
	if err := json.Unmarshal(created.Body.Bytes(), &first); err != nil || created.Code != http.StatusCreated || first.Version != 1 {
		t.Fatalf("create definition status=%d body=%s err=%v", created.Code, created.Body.String(), err)
	}
	secondResponse := post("/admin/api/v1/projects/"+bootstrap.ProjectID+"/outcome-definitions/"+first.ID+"/versions", first.Revision, map[string]any{"name": "ticket_resolved", "data_type": "CATEGORICAL", "allowed_values": []string{"accepted", "rejected", "unknown"}, "success_values": []string{"accepted"}, "enabled": true})
	var second domain.OutcomeDefinition
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &second); err != nil || secondResponse.Code != http.StatusCreated || second.Version != 2 || second.ID != first.ID {
		t.Fatalf("version status=%d body=%s err=%v", secondResponse.Code, secondResponse.Body.String(), err)
	}
	storedFirst, err := runtime.store.GetOutcomeDefinition(context.Background(), bootstrap.ProjectID, first.ID, 1)
	if err != nil || len(storedFirst.AllowedValues) != 2 {
		t.Fatalf("v1 was changed: %#v err=%v", storedFirst, err)
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
