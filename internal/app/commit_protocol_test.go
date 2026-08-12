package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
)

// A mutation that is durable but not in force used to be indistinguishable from
// one that is both, because the only channel was the HTTP status. The operation
// ID is what the caller looks the change up by, and the activation header is
// what says whether it is live yet.
func TestAdminMutationReportsItsOperationAndActivationState(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority, "strategy": string(route.Strategy), "enabled": route.Enabled,
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, body)
	request.Header.Set("If-Match", revisionETag(route.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("route update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	operation := recorder.Header().Get("Halro-Operation-Id")
	if operation == "" {
		t.Fatal("a committed mutation did not name the operation it can be looked up by")
	}
	if activation := recorder.Header().Get("Halro-Activation"); activation != "current" {
		t.Fatalf("activation header=%q", activation)
	}
	// Delivered, so nothing is left pending and the audit log carries the very
	// event ID the caller was handed.
	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	audit := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/audit")
	if !bytes.Contains(audit.Body.Bytes(), []byte(operation)) {
		t.Fatalf("the audit log does not carry operation %q: %s", operation, audit.Body.String())
	}
}

func TestAMutationCommittedWhileStaleSaysSoOnTheResponse(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	runtime.activation.markStale(activationDomainAuth, "injected auth activation failure", time.Now().UTC())
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority + 1, "strategy": string(route.Strategy), "enabled": route.Enabled,
	})
	request.Header.Set("If-Match", revisionETag(route.Revision))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Halro-Activation") != "stale" || response.Header().Get("Halro-Operation-Id") == "" {
		t.Fatalf("committed stale mutation status=%d activation=%q operation=%q body=%s", response.Code,
			response.Header().Get("Halro-Activation"), response.Header().Get("Halro-Operation-Id"), response.Body.String())
	}
	if !activationDomainIsStale(runtime.activation.status(), activationDomainAuth) {
		t.Fatal("an unrelated topology activation cleared auth staleness")
	}
}

func TestProviderDeploymentAndProjectCreateRequireAKeyAndRefuseReplay(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	provider, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, replayCode string
		body                   map[string]any
	}{
		{name: "provider", path: "/admin/api/v1/providers", replayCode: "provider_idempotency_replay", body: map[string]any{
			"name": "Idempotent provider", "type": provider.Type, "base_url": provider.BaseURL,
			"credential_id": provider.CredentialID, "max_concurrency": int64(1), "enabled": false,
		}},
		{name: "deployment", path: "/admin/api/v1/deployments", replayCode: "deployment_idempotency_replay", body: map[string]any{
			"name": "Idempotent deployment", "provider_id": provider.ID, "provider_model": "gpt-4o",
			"max_concurrency": int64(1), "enabled": false,
		}},
		{name: "project", path: "/admin/api/v1/projects", replayCode: "project_idempotency_replay", body: map[string]any{
			"name": "Idempotent project", "enabled": false, "allowed_routes": []string{},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			send := func(key string) *httptest.ResponseRecorder {
				request := adminMutationRequest(t, http.MethodPost, test.path, session, test.body)
				if key == "" {
					request.Header.Del("Idempotency-Key")
				} else {
					request.Header.Set("Idempotency-Key", key)
				}
				response := httptest.NewRecorder()
				runtime.adminRouter().ServeHTTP(response, request)
				return response
			}
			missing := send("")
			if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "idempotency_key_required") {
				t.Fatalf("missing key status=%d body=%s", missing.Code, missing.Body.String())
			}
			key := "a4-t5-" + test.name
			created := send(key)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
			}
			replay := send(key)
			if replay.Code != http.StatusConflict || !strings.Contains(replay.Body.String(), test.replayCode) {
				t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
			}
		})
	}
}

func TestAnIntentLeftByACrashIsDeliveredOnTheNextStart(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Audit recovery", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	session := loginTestAdmin(t, runtime, "admin", stepUpTestPassword)
	request := requestWithAuthenticatedAdminContext(t, runtime,
		adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+bootstrap.RouteID, session, nil), session)
	intent, err := runtime.newAdminAuditIntent(request, "route.update", "route", bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority++
	if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision, intent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("startup did not recover pending intent: %v", err)
	}
	defer reopened.Close()
	if pending, err := reopened.store.PendingAdminAuditIntentCount(context.Background()); err != nil || pending != 0 {
		t.Fatalf("pending intents after restart=%d err=%v", pending, err)
	}
	found := false
	if _, err := reopened.audit.Replay(func(record audit.Record) error {
		found = found || record.Event.EventID == intent.EventID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("recovered audit event %q is missing", intent.EventID)
	}
}

func TestAnUndeliverableIntentRefusesToStart(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	request := requestWithAuthenticatedAdminContext(t, runtime,
		adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+bootstrap.RouteID, session, nil), session)
	intent, err := runtime.newAdminAuditIntent(request, "route.update", "route", bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority++
	if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision, intent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.deliverAdminAuditIntent(context.Background(), *intent); err != nil {
		t.Fatal(err)
	}
	route, err = runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority++
	conflictingIntent := *intent
	conflictingIntent.Action = "route.delete"
	if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision, &conflictingIntent); err != nil {
		t.Fatal(err)
	}
	cfg := runtime.config
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		reopened.Close()
		t.Fatal("runtime started with an undeliverable durable audit intent")
	} else if !strings.Contains(err.Error(), "recover pending admin audit") {
		t.Fatalf("startup error=%v", err)
	}
}

func TestCommittedAdminAuditDeliveryIgnoresClientCancellation(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, nil)
	request = requestWithAuthenticatedAdminContext(t, runtime, request, session)
	intent, err := runtime.newAdminAuditIntent(request, "route.update", "route", route.ID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority++
	if _, err = runtime.store.PutRoute(context.Background(), route, route.Revision, intent); err != nil {
		t.Fatal(err)
	}

	cancelledContext, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(cancelledContext)
	runtime.completeAdminMutation(httptest.NewRecorder(), request, *intent)

	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil || pending != 0 {
		t.Fatalf("client cancellation stranded %d audit intents: %v", pending, err)
	}
	auditResponse := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/audit")
	if !bytes.Contains(auditResponse.Body.Bytes(), []byte(intent.EventID)) {
		t.Fatalf("audit event %q was not delivered: %s", intent.EventID, auditResponse.Body.String())
	}
}

func TestAdminAuditRecoveryRunsWithoutActivationStaleness(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, nil)
	request = requestWithAuthenticatedAdminContext(t, runtime, request, session)
	intent, err := runtime.newAdminAuditIntent(request, "route.update", "route", route.ID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority++
	if _, err = runtime.store.PutRoute(context.Background(), route, route.Revision, intent); err != nil {
		t.Fatal(err)
	}
	if runtime.activation.status().Stale {
		t.Fatal("test requires a current runtime")
	}
	if err := runtime.recoverAdminAuditIntents(); err != nil {
		t.Fatal(err)
	}
	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil || pending != 0 {
		t.Fatalf("runtime-owned retry left %d intents: %v", pending, err)
	}
}

func requestWithAuthenticatedAdminContext(t *testing.T, runtime *Runtime, request *http.Request, login loggedInAdmin) *http.Request {
	t.Helper()
	session, err := runtime.adminSessions.Authenticate(request.Context(), login.cookie.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(request.Context(), adminContextKey{}, adminRequestContext{
		session: session, token: login.cookie.Value, role: "administrator",
	})
	return request.WithContext(ctx)
}

// The fail-open direction F-01 and F-03 both point at: a revocation is durable,
// activation did not happen, and the old snapshot would go on authorizing. The
// data plane must refuse rather than serve from a snapshot known to be behind.
func TestStaleActivationRefusesDataPlaneTraffic(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	recorder := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("the runtime refused before anything was marked stale: %s", recorder.Body.String())
	}

	runtime.activation.markStale(activationDomainTopology, "routing registry: store unavailable", time.Now().UTC())
	for _, test := range []struct {
		name, path, wantType, wantCode string
	}{
		{name: "openai", path: "/v1/chat/completions", wantType: "service_unavailable", wantCode: "configuration_stale"},
		{name: "anthropic", path: "/v1/messages", wantType: "overloaded_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder = httptest.NewRecorder()
			runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, nil))
			if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("stale response status=%d retry-after=%q cache=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Header().Get("Cache-Control"), recorder.Body.String())
			}
			var refusal struct {
				Type  string `json:"type"`
				Error struct {
					Type, Code, Message string
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil || refusal.Error.Type != test.wantType || refusal.Error.Code != test.wantCode {
				t.Fatalf("protocol refusal type=%q code=%q body=%s err=%v", refusal.Error.Type, refusal.Error.Code, recorder.Body.String(), err)
			}
			if test.name == "anthropic" && (refusal.Type != "error" || !bytes.Contains([]byte(refusal.Error.Message), []byte("configuration_stale"))) {
				t.Fatalf("Anthropic refusal is not actionable: %#v", refusal)
			}
		})
	}

	// Readiness has to agree, or an orchestrator keeps routing traffic to an
	// instance that is refusing all of it.
	ready := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stale runtime reported itself ready: status=%d body=%s", ready.Code, ready.Body.String())
	}

	runtime.activation.markCurrent(activationDomainTopology)
	recovered := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recovered, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recovered.Code == http.StatusServiceUnavailable {
		t.Fatalf("the runtime kept refusing after catching up: %s", recovered.Body.String())
	}
}

func TestASuccessfulActivationDoesNotClearAnotherSubsystemsStaleness(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	runtime.activation.markStale(activationDomainRedaction, "injected redaction failure", time.Now().UTC())

	runtime.adminTopologyMu.Lock()
	err := runtime.activateTopology()
	runtime.adminTopologyMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	status := runtime.activation.status()
	if !status.Stale || !activationDomainIsStale(status, activationDomainRedaction) {
		t.Fatalf("topology activation cleared redaction staleness: %#v", status)
	}
	recorder := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime resumed while redaction was stale: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRecoveryReplaysEveryActivationDomain(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	now := time.Now().UTC()
	for _, domain := range activationDomains {
		runtime.activation.markStale(domain, "injected failure", now)
	}

	runtime.recoverActivationDomains()
	if status := runtime.activation.status(); status.Stale {
		t.Fatalf("recovery left a domain stale: %#v", status)
	}
}

func TestActivationStalenessIsExportedForOperators(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	runtime.activation.markStale(activationDomainAuth, "injected auth failure", time.Now().Add(-3*time.Second).UTC())

	body := renderMetricsForTest(t, runtime)
	if !bytes.Contains([]byte(body), []byte("halro_activation_stale 1")) {
		t.Fatalf("stale gauge was not asserted:\n%s", grepSeries(body, "halro_activation_stale"))
	}
	var staleSeconds float64
	if _, err := fmt.Sscanf(grepSeries(body, "halro_activation_stale_seconds"), "halro_activation_stale_seconds %f", &staleSeconds); err != nil {
		t.Fatalf("parse stale seconds: %v", err)
	}
	if staleSeconds < 2 {
		t.Fatalf("stale duration=%f, want at least two seconds", staleSeconds)
	}
}

func TestFailedPolicyActivationRefusesDataPlaneTraffic(t *testing.T) {
	for _, test := range []struct {
		name     string
		domain   activationDomain
		activate func(*Runtime)
	}{
		{name: "redaction", domain: activationDomainRedaction, activate: (*Runtime).activateRedactionPolicies},
		{name: "token_guard", domain: activationDomainTokenGuard, activate: (*Runtime).activateTokenGuardPolicies},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _, _ := activationTestRuntime(t)
			activationContext, cancel := context.WithCancel(context.Background())
			cancel()
			runtime.backgroundCtx = activationContext
			test.activate(runtime)
			status := runtime.activation.status()
			if !status.Stale || !activationDomainIsStale(status, test.domain) {
				t.Fatalf("failed %s activation did not mark its domain stale: %#v", test.name, status)
			}
			recorder := httptest.NewRecorder()
			runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("failed %s activation did not refuse traffic: status=%d body=%s", test.name, recorder.Code, recorder.Body.String())
			}
		})
	}
}

// The deliberate other direction, and the one that is easy to "fix" into an
// outage. Alert delivery decides nothing about whether a request is
// authorized, redacted or admitted, so a webhook that cannot be rebuilt must
// not refuse the data plane. Without this test, a later change that routes
// every activation failure through markStale would look like a tightening and
// would in fact take the whole gateway down for a broken notification path.
func TestFailedAlertActivationDoesNotRefuseTraffic(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	activationContext, cancel := context.WithCancel(context.Background())
	cancel()
	runtime.backgroundCtx = activationContext

	runtime.activateAlertEndpoints()

	status := runtime.activation.status()
	if status.Stale {
		t.Fatalf("a failed alert activation refused the data plane: %#v", status)
	}
	recorder := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("traffic was refused after an alert activation failure: body=%s", recorder.Body.String())
	}
}

func activationDomainIsStale(status activationStatus, domain activationDomain) bool {
	for _, item := range status.Domains {
		if item.Domain == domain {
			return item.Stale
		}
	}
	return false
}

// A create mints its own ID, so a retry after a lost response used to produce a
// second record: two routes on one public model become two routing candidates,
// and the operator sees a duplicate they never asked for. The idempotency key
// is how the caller says "this is the same create", and deriving the ID from it
// turns the retry into a collision instead.
func TestRetriedCreateDoesNotProduceASecondRecord(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	body := map[string]any{
		"public_model": "retry-alias", "deployment_id": bootstrap.DeploymentID,
		"priority": 0, "strategy": "ordered", "enabled": false,
	}
	send := func(key string) *httptest.ResponseRecorder {
		request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/routes", session, body)
		request.Header.Set("Idempotency-Key", key)
		recorder := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(recorder, request)
		return recorder
	}

	first := send("operator-retry-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	retry := send("operator-retry-1")
	if retry.Code != http.StatusConflict {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	var replay struct {
		Code string `json:"code"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &replay); err != nil || replay.Code != "route_idempotency_replay" || replay.ID == "" {
		t.Fatalf("retry does not name the existing record: %s err=%v", retry.Body.String(), err)
	}

	// A deliberate second create is a different key, and must still work.
	second := send("operator-retry-2")
	if second.Code != http.StatusCreated {
		t.Fatalf("second deliberate create status=%d body=%s", second.Code, second.Body.String())
	}

	routes, err := runtime.store.ListRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	aliases := 0
	for _, route := range routes {
		if route.PublicModel == "retry-alias" {
			aliases++
		}
	}
	if aliases != 2 {
		t.Fatalf("routes on the retried alias=%d, want 2 (one per distinct key)", aliases)
	}

	missing := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/routes", session, body)
	missing.Header.Del("Idempotency-Key")
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, missing)
	if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte("idempotency_key_required")) {
		t.Fatalf("create without a key status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// The policy resources were left on the old ordering when the commit protocol
// landed for the topology chain. A Token Guard policy decides what traffic is
// admitted, so a change of it that is durable without a record — or that is
// recorded as having been applied when activation never ran — is the same
// defect, on a resource where it matters as much.
func TestPolicyMutationsFollowTheCommitProtocol(t *testing.T) {
	runtime, _, session := activationTestRuntime(t)
	request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/token-guard-policies", session, map[string]any{
		"name": "guard", "enabled": true, "action": "alert", "request_tokens": 1000,
	})
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("token guard policy create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	operation := recorder.Header().Get("Halro-Operation-Id")
	if operation == "" {
		t.Fatal("a committed policy change did not name its operation")
	}
	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	audit := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/audit")
	if !bytes.Contains(audit.Body.Bytes(), []byte(operation)) {
		t.Fatalf("the audit log does not carry operation %q: %s", operation, audit.Body.String())
	}
}
