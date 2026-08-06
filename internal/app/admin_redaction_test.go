package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminRedactionPolicyLifecycleTestAndReferenceProtection(t *testing.T) {
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

	body := map[string]any{
		"name": "PII baseline", "enabled": true, "mode": "strict",
		"rules": []map[string]any{{
			"name": "Phone", "kind": "builtin", "builtin": "china_phone",
			"scopes": []string{"inbound", "outbound"}, "action": "mask",
			"enabled": true, "priority": 10,
		}},
	}
	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/redaction-policies", "", body)
	if created.Code != http.StatusCreated || created.Header().Get("ETag") != `"1"` {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var policy domain.RedactionPolicy
	if err := json.Unmarshal(created.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if !runtime.redactor.HasPolicy(policy.ID) ||
		len(policy.Rules) != 1 ||
		policy.Rules[0].ID == "" ||
		policy.Rules[0].ComputedMaxMatchBytes <= 0 {
		t.Fatalf("policy was not compiled and hot loaded: %#v", policy)
	}

	canary := "call me at 13800138000"
	tested := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/redaction-policies/"+policy.ID+"/test", "",
		map[string]any{"input": canary, "scope": "inbound"})
	if tested.Code != http.StatusOK || strings.Contains(tested.Body.String(), canary) ||
		strings.Contains(tested.Body.String(), "13800138000") {
		t.Fatalf("test leaked input or failed status=%d body=%s", tested.Code, tested.Body.String())
	}
	var result struct {
		MatchCount int `json:"match_count"`
	}
	if err := json.Unmarshal(tested.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.MatchCount != 1 {
		t.Fatalf("unexpected test result: %s", tested.Body.String())
	}

	now := time.Now().UTC()
	if _, err := runtime.store.PutProject(context.Background(), domain.Project{
		ID: "prj_redacted", Name: "Redacted", Enabled: true,
		RedactionPolicyID: policy.ID, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	listed := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodGet, "/admin/api/v1/redaction-policies", "", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var page struct {
		Items []redactionPolicyView `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].BoundProjects != 1 {
		t.Fatalf("unexpected policy bindings: %#v", page.Items)
	}
	blocked := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/redaction-policies/"+policy.ID, `"1"`, stepUp())
	if blocked.Code != http.StatusConflict {
		t.Fatalf("referenced delete status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	// Switching the project off does not release the reference: the ID stays on
	// the project, so deleting the policy now would leave a dangling reference
	// for whoever switches it back on.
	project, err := runtime.store.GetProject(context.Background(), "prj_redacted")
	if err != nil {
		t.Fatal(err)
	}
	project.Enabled = false
	project, err = runtime.store.PutProject(context.Background(), project, project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	stillBlocked := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/redaction-policies/"+policy.ID, `"1"`, stepUp())
	if stillBlocked.Code != http.StatusConflict {
		t.Fatalf("disabled project released the reference: status=%d body=%s",
			stillBlocked.Code, stillBlocked.Body.String())
	}

	project.RedactionPolicyID = ""
	if _, err := runtime.store.PutProject(context.Background(), project, project.Revision); err != nil {
		t.Fatal(err)
	}
	released := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodDelete, "/admin/api/v1/redaction-policies/"+policy.ID, `"1"`, stepUp())
	if released.Code != http.StatusNoContent {
		t.Fatalf("delete after the reference was removed: status=%d body=%s",
			released.Code, released.Body.String())
	}
}

func TestAdminRedactionPolicyRejectsUnsafeBoundedRuleBeforePersistence(t *testing.T) {
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

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/redaction-policies", "",
		map[string]any{
			"name": "Unsafe stream", "enabled": true, "mode": "bounded_stream",
			"rules": []map[string]any{{
				"name": "Email", "kind": "builtin", "builtin": "email",
				"scopes": []string{"outbound"}, "action": "replace",
				"replacement": "[EMAIL]", "enabled": true,
			}},
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsafe bounded policy status=%d body=%s", response.Code, response.Body.String())
	}
	items, err := runtime.store.ListRedactionPolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("unsafe policy was persisted: %#v", items)
	}
}
