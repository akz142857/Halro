package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
)

// The resource plane redacts file names, textual uploads and batch results,
// and the redaction engine answers a policy lookup miss with "no policy" — the
// fail-open direction. resolveRequest refuses a request whose Project names a
// policy the live snapshot does not hold; the resource plane has to give the
// same answer, or a stale snapshot turns its redaction off silently.
func TestResourcePlaneRefusesWhenPolicySnapshotMissesProjectPolicy(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("media", adapter), nil)
	defer f.close()

	// Point the project at a redaction policy the engine does not hold, the
	// state a stale snapshot or a regressed Admin guard would produce.
	project := f.project
	project.RedactionPolicyID = "pol_not_loaded"
	key := domain.GatewayKey{
		ID: "key_policy_gate", ProjectID: project.ID, Name: "gate",
		HashVersion: 1, KeyHash: auth.HashGatewayKey(f.plaintext), Enabled: true,
	}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{project},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := f.service.resourcePrincipal(context.Background(), f.plaintext)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "configuration_stale" || gatewayErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected configuration_stale 503, got %v", err)
	}

	// With the reference cleared the same principal is admitted again.
	project.RedactionPolicyID = ""
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{project},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.resourcePrincipal(context.Background(), f.plaintext); err != nil {
		t.Fatalf("clean project was refused: %v", err)
	}
}
