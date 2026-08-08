package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// The console reads the review state and the preflight over HTTP, so both are
// checked through the router rather than against the helpers alone.
func TestAdminDeploymentExposesReviewStateAndPreflightsNarrowing(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
		ProjectName: "Capabilities", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	read := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments/"+bootstrap.DeploymentID)
	if read.Code != http.StatusOK {
		t.Fatalf("get deployment status=%d body=%s", read.Code, read.Body.String())
	}
	var view struct {
		Capabilities     domain.ProviderCapabilities `json:"capabilities"`
		CapabilityReview struct {
			State  string `json:"state"`
			Source string `json:"source"`
		} `json:"capability_review"`
		// The persisted field is gone; a stored review state must not reappear
		// beside the derived one.
		Stored *string `json:"capability_review_state"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CapabilityReview.State == "" {
		t.Fatalf("deployment read carried no review state: %s", read.Body.String())
	}
	if view.Stored != nil {
		t.Fatalf("a stored capability_review_state is still serialized: %s", read.Body.String())
	}
	if view.CapabilityReview.Source != "operator_declared" {
		t.Fatalf("bootstrap snapshot source=%q, expected the operator declaration", view.CapabilityReview.Source)
	}
	if !view.Capabilities.Chat {
		t.Fatalf("bootstrap deployment has no chat capability: %+v", view.Capabilities)
	}

	// Bootstrap wires exactly one route onto this deployment, so dropping chat
	// leaves the "chat" public model with no candidate at all.
	narrowed := view.Capabilities
	narrowed.Chat = false
	narrowed.Streaming = false // streaming depends on chat
	preflight := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/capabilities/preflight", "",
		map[string]any{"capabilities": narrowed})
	if preflight.Code != http.StatusOK {
		t.Fatalf("preflight status=%d body=%s", preflight.Code, preflight.Body.String())
	}
	var result capabilityPreflightResponse
	if err := json.Unmarshal(preflight.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Blocking {
		t.Fatalf("removing the only chat candidate was not reported as blocking: %s", preflight.Body.String())
	}
	if len(result.Routes) == 0 {
		t.Fatalf("no affected route reported: %s", preflight.Body.String())
	}
	found := false
	for _, impact := range result.Routes {
		if impact.RouteID == bootstrap.RouteID && impact.Capability == "chat" && impact.SoleCandidate {
			found = true
		}
	}
	if !found {
		t.Fatalf("the bootstrap route was not reported as losing chat: %+v", result.Routes)
	}

	// A preflight is a read: it must not have changed the deployment.
	after := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments/"+bootstrap.DeploymentID)
	var unchanged struct {
		Capabilities domain.ProviderCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Capabilities != view.Capabilities {
		t.Fatalf("the preflight applied the narrowing it was asked to predict: %+v -> %+v",
			view.Capabilities, unchanged.Capabilities)
	}
}
