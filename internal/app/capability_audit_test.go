package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func auditActions(t *testing.T, runtime *Runtime, cookie *http.Cookie) []map[string]any {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	if response.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", response.Code, response.Body.String())
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func findAudit(records []map[string]any, action string) map[string]any {
	for _, record := range records {
		if record["action"] == action {
			return record
		}
	}
	return nil
}

// §11.3 requires the capability events to carry the deployment, provider,
// model, catalog revision and administrator identity — deployment.create does
// not, which is why these exist separately.
func TestCreatingADeploymentAuditsTheCapabilitySnapshotAndDeclaration(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	// Bootstrap's deployment is an operator declaration; create a second one
	// through the Admin API so the handler path is what is being audited.
	created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments", "", map[string]any{
			"name": "Declared", "provider_id": bootstrap.ProviderID,
			"provider_model": "gpt-declared", "target_kind": "model_id",
			"mode":         "operator_declared",
			"capabilities": map[string]any{"chat": true},
			"weight":       1,
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	records := auditActions(t, runtime, cookie)
	snapshot := findAudit(records, auditCapabilitySnapshotCreated)
	if snapshot == nil {
		t.Fatalf("no %s event: %v", auditCapabilitySnapshotCreated, records)
	}
	metadata, _ := snapshot["metadata"].(map[string]any)
	if metadata["provider_model"] != "gpt-declared" {
		t.Fatalf("metadata did not carry the model: %v", metadata)
	}
	if metadata["provider_id"] != bootstrap.ProviderID {
		t.Fatalf("metadata did not carry the provider: %v", metadata)
	}
	if metadata["model_revision"] == "" || metadata["model_revision"] == nil {
		t.Fatalf("metadata did not carry the catalog revision: %v", metadata)
	}
	if snapshot["actor_id"] != "admin" {
		t.Fatalf("event did not carry the administrator: %v", snapshot)
	}

	// An operator declaration gets its own record, per §8.3.
	if findAudit(records, auditOperatorCapabilitiesDeclared) == nil {
		t.Fatalf("no %s event: %v", auditOperatorCapabilitiesDeclared, records)
	}
}

// Editing a deployment's capabilities is a review; editing its concurrency is
// not, and recording both the same way would bury the events that matter.
func TestOnlyACapabilityChangeAuditsAReview(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	deployment, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	body := func(capabilities map[string]any, concurrency int) map[string]any {
		return map[string]any{
			"name": deployment.Name, "provider_id": deployment.ProviderID,
			"provider_model": deployment.ProviderModel, "target_kind": string(deployment.TargetKind),
			"capabilities": capabilities, "max_concurrency": concurrency,
			"weight": 1, "enabled": deployment.Enabled,
		}
	}
	enabled := map[string]any{}
	for _, name := range enabledCapabilityNames(deployment.Capabilities) {
		enabled[name] = true
	}

	// Concurrency only: no review event.
	unrelated := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/deployments/"+deployment.ID, revisionETag(deployment.Revision), body(enabled, 7))
	if unrelated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", unrelated.Code, unrelated.Body.String())
	}
	if record := findAudit(auditActions(t, runtime, cookie), auditCapabilitySnapshotReviewed); record != nil {
		t.Fatalf("a concurrency edit was audited as a capability review: %v", record)
	}

	// Now drop a capability: that is a review.
	updated, err := runtime.store.GetDeployment(context.Background(), deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A leaf capability: dropping "streaming" would violate the dependency
	// stream_usage has on it, which is a different rejection entirely.
	narrowed := map[string]any{}
	for name := range enabled {
		if name == "stream_usage" {
			continue
		}
		narrowed[name] = true
	}
	if _, ok := enabled["stream_usage"]; !ok {
		t.Fatal("bootstrap did not enable stream_usage, so this drops nothing")
	}
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/deployments/"+deployment.ID, revisionETag(updated.Revision), body(narrowed, 7))
	if response.Code != http.StatusOK {
		t.Fatalf("narrow status=%d body=%s", response.Code, response.Body.String())
	}
	record := findAudit(auditActions(t, runtime, cookie), auditCapabilitySnapshotReviewed)
	if record == nil {
		t.Fatal("dropping a capability was not audited as a review")
	}
	metadata, _ := record["metadata"].(map[string]any)
	disabled, _ := metadata["disabled"].([]any)
	if len(disabled) != 1 || disabled[0] != "stream_usage" {
		t.Fatalf("the review did not name what was turned off: %v", metadata)
	}
}

// §4.4: the reconciliation result must reach the audit trail, not only doctor.
func TestDriftDetectionIsAudited(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, _ := loginAdminForTest(t, runtime)
	driftDeployment(t, runtime, bootstrap.DeploymentID)

	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}

	record := findAudit(auditActions(t, runtime, cookie), auditCapabilityDriftDetected)
	if record == nil {
		t.Fatal("withholding a drifted deployment produced no audit event")
	}
	if record["target_id"] != bootstrap.DeploymentID {
		t.Fatalf("event targets %v, expected the drifted deployment", record["target_id"])
	}
	// No administrator asked for this; a catalog or a binary upgrade caused it.
	if record["actor_type"] != "system" {
		t.Fatalf("actor_type=%v", record["actor_type"])
	}
	metadata, _ := record["metadata"].(map[string]any)
	if metadata["route_id"] != bootstrap.RouteID {
		t.Fatalf("event did not name the withheld route: %v", metadata)
	}
	if metadata["capability_review_state"] != string(domain.CapabilityReviewDrifted) {
		t.Fatalf("state=%v", metadata["capability_review_state"])
	}
}

// The audit trail is durable and exportable, so a credential reaching it would
// be a leak that outlives the request. Nothing in the capability metadata is
// sourced from a credential, and this asserts it stays that way.
func TestCapabilityAuditCarriesNoSecret(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, _ := loginAdminForTest(t, runtime)
	driftDeployment(t, runtime, bootstrap.DeploymentID)
	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}

	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	for _, secret := range [][]byte{
		[]byte("provider-secret"), []byte("correct horse battery staple"), []byte("gw_"),
		[]byte("api.openai.com"), []byte("Authorization"),
	} {
		if bytes.Contains(response.Body.Bytes(), secret) {
			t.Fatalf("audit response carried %q", secret)
		}
	}
}
