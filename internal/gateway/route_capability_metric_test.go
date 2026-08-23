package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// A route that cannot serve what was asked is a policy outcome, and every other
// policy outcome is counted where an operator already looks — the dashboard's
// rejection summary and the metrics endpoint. This one was not counted at all.
//
// It is a counter rather than a per-request record on purpose. The refusal
// happens before the first ledger write, so there is no usage row to carry the
// reason, and creating one would mean writing a request that never started:
// it would change what the ledger means by "request" and give anything holding
// a valid key a cheap way to make it write.
func TestARouteThatCannotServeIsCounted(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.registry = provider.NewRegistry()
	blind := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_blind", DeploymentID: "dep_blind", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: blind, Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before := service.RejectionMetrics().RouteCapability; before != 0 {
		t.Fatalf("the counter started at %d", before)
	}

	withImage := chatRequest()
	withImage.Messages = []openaiapi.Message{{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]`,
	)}}
	if _, err := service.Chat(context.Background(), f.plaintext, withImage); err == nil {
		t.Fatal("a target with no vision served an image")
	}
	if counted := service.RejectionMetrics().RouteCapability; counted != 1 {
		t.Fatalf("route capability rejections = %d, want 1", counted)
	}

	// A request the route can serve does not touch it.
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("a servable request was refused: %v", err)
	}
	if counted := service.RejectionMetrics().RouteCapability; counted != 1 {
		t.Fatalf("a served request moved the counter to %d", counted)
	}
}
