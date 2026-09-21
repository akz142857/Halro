package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

// suspensionRuntime is the gate, a real metadata store, and nothing else. The
// listing reads both — the gate for what is suspended, the store for which of
// those a clear can act on — so a fixture with only one of them would be
// asserting half the answer.
func suspensionRuntime(t *testing.T, gate *routegate.Gate, now time.Time) *Runtime {
	t.Helper()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// A real audit log, because the clear's whole shape is that the removal and
	// the record commit together: a fixture that could not append would leave
	// the assertion testing only half of it.
	auditLog, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auditLog.Close() })
	return &Runtime{
		routes: gate, store: store, audit: auditLog,
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The one surface that names identity.
//
// The caller-facing 503 names nothing and the metrics carry enumerations only,
// both deliberately — so an operator answering HalroCredentialUnusable has
// exactly one place to learn which credential, and it has to actually say.
func TestRouteSuspensionsNameTheScopeAnOperatorHasToFix(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	gate.Observe(provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 7,
	}, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401, Code: "invalid_api_key",
	}, now)
	runtime := suspensionRuntime(t, gate, now)

	recorder := httptest.NewRecorder()
	runtime.listAdminRouteSuspensions(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items []routeSuspensionView `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items=%+v", payload.Items)
	}
	item := payload.Items[0]
	if item.ScopeKind != "credential" || item.ScopeKey != "cred_live" {
		t.Fatalf("scope=%s/%s, want credential/cred_live", item.ScopeKind, item.ScopeKey)
	}
	if !item.Indefinite || item.Until != "" {
		t.Fatalf("a dead credential was given an end time: %+v", item)
	}
	// The revision is what the operator is comparing against when they wonder
	// why the suspension is still there after saving a new secret.
	if item.CredentialRevision != 7 {
		t.Fatalf("credential_revision=%d, want 7", item.CredentialRevision)
	}
	if item.Reason != "invalid_credential" || item.Status != 401 || item.Code != "invalid_api_key" {
		t.Fatalf("evidence lost: %+v", item)
	}
}

// The credential-and-model scope joins its halves with a NUL so they cannot
// collide inside the gate. That byte has no business in a JSON body an operator
// reads, or in anything that pastes it into a search box.
func TestRouteSuspensionKeysAreLegibleOutsideTheGate(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	gate.Observe(provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 1,
	}, routegate.Observation{
		Reason: provider.FailureReasonSubscriptionQuotaExhausted, Status: 402,
	}, now)
	runtime := suspensionRuntime(t, gate, now)

	recorder := httptest.NewRecorder()
	runtime.listAdminRouteSuspensions(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if strings.ContainsRune(body, 0) {
		t.Fatalf("a NUL reached the response body: %q", body)
	}
	if !strings.Contains(body, "cred_live/gpt-test") {
		t.Fatalf("the credential-and-model scope did not render legibly: %s", body)
	}
	if !strings.Contains(body, `"until"`) {
		t.Fatalf("a suspension that ends on a clock did not say when: %s", body)
	}
}

// The clear acts on the durable half, and the listing has to say which rows
// those are — an operator who cannot tell the difference reads a 409 as a bug.
func TestTheListingSaysWhichSuspensionsAreClearable(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	runtime := suspensionRuntime(t, gate, now)
	gate.UsePersistence(runtime.store, nil)
	credential := provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 7,
	}
	// One long refusal, which is stored, and one availability window, which is
	// not: thirty seconds is gone before an operator could reach it.
	gate.Observe(credential, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401,
	}, now)
	gate.ObserveProbe("dep_other", routegate.DeploymentProbe{Healthy: false, ObservedAt: now}, now)

	recorder := httptest.NewRecorder()
	runtime.listAdminRouteSuspensions(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	var payload struct {
		Items []routeSuspensionView `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %+v, want the credential and the deployment", payload.Items)
	}
	for _, item := range payload.Items {
		switch item.ScopeKind {
		case "credential":
			if !item.Clearable {
				t.Fatalf("the stored credential suspension is not marked clearable: %+v", item)
			}
			kind, key, ok := domain.DecodeRouteScopeID(item.ScopeID)
			if !ok || kind != "credential" || key != "cred_live" {
				t.Fatalf("scope_id %q does not decode back to its scope", item.ScopeID)
			}
		case "deployment":
			if item.Clearable {
				t.Fatalf("an availability window was offered as clearable: %+v", item)
			}
		default:
			t.Fatalf("unexpected scope kind %q", item.ScopeKind)
		}
	}
}

// Clearing removes the stored row, clears the live gate, and leaves an audit
// record — the three halves that make this an administrative action rather than
// a knob.
func TestClearingARouteSuspensionRemovesTheRowAndRecordsIt(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	runtime := suspensionRuntime(t, gate, now)
	gate.UsePersistence(runtime.store, nil)
	target := provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 7,
	}
	gate.Observe(target, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401,
	}, now)
	scopeID := domain.EncodeRouteScopeID("credential", "cred_live")

	recorder := httptest.NewRecorder()
	runtime.clearAdminRouteSuspension(recorder, adminClearRequest(t, scopeID))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if rows, _ := runtime.store.ListRouteSuspensions(context.Background()); len(rows) != 0 {
		t.Fatalf("stored rows after the clear = %+v", rows)
	}
	if len(gate.Snapshot(now)) != 0 {
		t.Fatal("the live gate is still holding a suspension the operator cleared")
	}
	// The record is in the trusted chain, and the intent that carried it there
	// has been retired: a pending row left behind would mean delivery failed.
	recorded := false
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		if record.Event.Action == "route_suspension.clear" && record.Event.TargetID == scopeID {
			recorded = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("clearing a suspension left no audit record")
	}
	intents, err := runtime.store.ListPendingAdminAuditIntents(context.Background())
	if err != nil || len(intents) != 0 {
		t.Fatalf("pending intents = %+v err=%v, want none after delivery", intents, err)
	}
	// And the target is admissible again, which is the point of the escape
	// hatch: the gate re-suspends only if the upstream refuses again.
	if admitted := gate.Filter([]provider.Target{target}, now); len(admitted) != 1 {
		t.Fatal("the cleared target is still filtered out")
	}
}

// A live suspension nobody stored is visible and not clearable, and saying so
// is better than a 404 an operator reads as "the listing is lying".
func TestClearingASuspensionThatIsNotDurableSaysSo(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	runtime := suspensionRuntime(t, gate, now)
	gate.UsePersistence(runtime.store, nil)
	gate.ObserveProbe("dep_1", routegate.DeploymentProbe{Healthy: false, ObservedAt: now}, now)

	recorder := httptest.NewRecorder()
	runtime.clearAdminRouteSuspension(recorder, adminClearRequest(t, domain.EncodeRouteScopeID("deployment", "dep_1")))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "route_suspension_not_clearable") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestClearingAnUnknownScopeIsNotFound(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	runtime := suspensionRuntime(t, gate, now)

	for _, scopeID := range []string{domain.EncodeRouteScopeID("credential", "cred_absent"), "not-a-handle!!"} {
		recorder := httptest.NewRecorder()
		runtime.clearAdminRouteSuspension(recorder, adminClearRequest(t, scopeID))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("scope %q: status=%d body=%s, want 404", scopeID, recorder.Code, recorder.Body.String())
		}
	}
}

// adminClearRequest is the DELETE the router would deliver: the path parameter
// chi extracts, and the authenticated admin the audit record names.
func adminClearRequest(t *testing.T, scopeID string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/route-suspensions/"+scopeID, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("scopeID", scopeID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, adminContextKey{}, adminRequestContext{
		session: domain.AdminSession{Username: "admin"}, role: "administrator",
	})
	return request.WithContext(ctx)
}
