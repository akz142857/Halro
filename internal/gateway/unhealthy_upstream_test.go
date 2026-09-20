package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

// Candidate resolution drops ineligible targets before the operation filter, so
// an alias whose every deployment is refused used to fall into the "operation
// unsupported" branch and answer 400 — blaming the request for an upstream
// state. It is the same condition a suspension reports, and it gets the same
// shape now.
func TestChatReportsUnhealthyUpstreamAsUnavailableNotUnsupported(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	now := time.Now()
	f.gate.ObserveProbe("dep_target_1", routegate.DeploymentProbe{Healthy: false, ObservedAt: now}, now)

	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	// The refusal is typed now: an operator reading "temporarily out of service"
	// looks for an outage, and that is the right place to look for this one.
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "all_candidates_suspended" ||
		gatewayErr.HTTPStatus != 503 || gatewayErr.RetryAfter <= 0 {
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
	f.gate.ObserveProbe("dep_target_1", routegate.DeploymentProbe{Healthy: true, ObservedAt: now}, now)
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("healthy target refused: %v", err)
	}
}

func TestSupportsOperationIgnoresEligibility(t *testing.T) {
	registry := provider.NewRegistry()
	adapter := &fakeAdapter{}
	if err := registry.Register(provider.Target{
		ID: "target_h", DeploymentID: "dep_h", PublicModel: "alias", ProviderModel: "m",
		Adapter: adapter, Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	}); err != nil {
		t.Fatal(err)
	}
	gate := routegate.New(routegate.Config{AvailabilityThreshold: 1})
	registry.SetEligibility(gate)
	probeAt := time.Now()
	gate.ObserveProbe("dep_h", routegate.DeploymentProbe{Healthy: false, ObservedAt: probeAt}, probeAt)
	if candidates := registry.ResolveCandidatesFor("alias", provider.OperationChat); len(candidates) != 0 {
		t.Fatalf("unhealthy target still resolved: %#v", candidates)
	}
	if !registry.SupportsOperation("alias", provider.OperationChat, "") {
		t.Fatal("supported operation was hidden by a suspension")
	}
	if registry.SupportsOperation("alias", provider.OperationEmbeddings, "") {
		t.Fatal("unsupported operation was reported as supported")
	}
	if registry.SupportsOperation("absent", provider.OperationChat, "") {
		t.Fatal("unknown alias was reported as supported")
	}
}
