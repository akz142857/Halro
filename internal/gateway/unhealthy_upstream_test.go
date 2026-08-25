package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

// Candidate resolution drops probe-unhealthy targets before the operation
// filter, so an alias whose every deployment is unhealthy used to fall into
// the "operation unsupported" branch and answer 400 — blaming the request for
// an upstream state. It is the same condition an open circuit reports, and it
// gets the same shape now.
func TestChatReportsUnhealthyUpstreamAsUnavailableNotUnsupported(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	f.registry.SetDeploymentProbe("dep_target_1", provider.DeploymentProbe{Healthy: false})

	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "provider_unavailable" || gatewayErr.HTTPStatus != 503 {
		t.Fatalf("unexpected error: %#v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("provider was called %d times", f.adapter.calls)
	}

	// A genuinely unsupported operation on the same unhealthy alias still says
	// so: health must not upgrade a capability refusal into a retryable 503.
	if supported := f.registry.SupportsOperation("chat", provider.OperationRerank, ""); supported {
		t.Fatal("fixture target unexpectedly claims rerank")
	}

	// Recovery is symmetric: a healthy probe restores ordinary resolution.
	f.registry.SetDeploymentProbe("dep_target_1", provider.DeploymentProbe{Healthy: true})
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("healthy target refused: %v", err)
	}
}

func TestSupportsOperationIgnoresProbeHealth(t *testing.T) {
	registry := provider.NewRegistry()
	adapter := &fakeAdapter{}
	if err := registry.Register(provider.Target{
		ID: "target_h", DeploymentID: "dep_h", PublicModel: "alias", ProviderModel: "m",
		Adapter: adapter, Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	}); err != nil {
		t.Fatal(err)
	}
	registry.SetDeploymentProbe("dep_h", provider.DeploymentProbe{Healthy: false})
	if candidates := registry.ResolveCandidatesFor("alias", provider.OperationChat); len(candidates) != 0 {
		t.Fatalf("unhealthy target still resolved: %#v", candidates)
	}
	if !registry.SupportsOperation("alias", provider.OperationChat, "") {
		t.Fatal("supported operation was hidden by probe health")
	}
	if registry.SupportsOperation("alias", provider.OperationEmbeddings, "") {
		t.Fatal("unsupported operation was reported as supported")
	}
	if registry.SupportsOperation("absent", provider.OperationChat, "") {
		t.Fatal("unknown alias was reported as supported")
	}
}
