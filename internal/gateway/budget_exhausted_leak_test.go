package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/usage"
)

// activeRequestsAfterReplay rebuilds the usage aggregate the way a restart
// does, so the assertion reflects what actually survives to disk rather than
// in-memory bookkeeping.
func activeRequestsAfterReplay(t *testing.T, log *ledger.Log) uint64 {
	t.Helper()
	aggregate := usage.NewAggregate()
	if _, err := log.Replay(ledger.Watermark{}, aggregate.Apply); err != nil {
		t.Fatal(err)
	}
	return aggregate.Metrics().ActiveRequests
}

// TestBudgetExhaustedRequestsDoNotStayActive covers the cheapest way to make
// the gateway leak. A request is written into the ledger as accepted before
// any budget check runs, so a rejection that returns without finalizing leaves
// an accepted-but-never-finalized request behind forever. The usage aggregate
// keys in-flight requests off exactly that pair, and it gets checkpointed into
// bbolt, so the leak survives restarts and grows with every rejected call —
// while halro_active_requests climbs and no request ever appears in Usage.
//
// The path costs an attacker nothing: it never reaches a provider, so it is
// not rate-limited by anything upstream, and a project whose budget is spent
// will keep producing it for the rest of the day.
func TestBudgetExhaustedRequestsDoNotStayActive(t *testing.T) {
	// One micro-USD of daily budget: enough to accept a request, never enough
	// to reserve an attempt against it.
	f := newFixture(t, 1)
	defer f.close()

	maxTokens := int64(10)
	request := openaiapi.ChatCompletionRequest{
		Model: "chat", MaxCompletionTokens: &maxTokens,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
	}
	const attempts = 8
	for range attempts {
		if _, err := f.service.Chat(context.Background(), f.plaintext, request); err == nil {
			t.Fatal("expected the request to be rejected once the budget is spent")
		}
	}
	if calls := f.adapter.calls; calls != 0 {
		t.Fatalf("budget-rejected requests must not reach the provider: calls=%d", calls)
	}
	if active := activeRequestsAfterReplay(t, f.log); active != 0 {
		t.Fatalf("budget-rejected requests left %d of %d in flight", active, attempts)
	}
}

// TestEmbeddingsBudgetRejectionDoesNotStayActive checks a second entry point,
// because the fix belongs to the request run rather than to any one handler:
// if closing the run is what finalizes, then every protocol path inherits it
// without its own return statement having to remember.
func TestEmbeddingsBudgetRejectionDoesNotStayActive(t *testing.T) {
	f := newFixture(t, 1)
	defer f.close()

	const attempts = 5
	for range attempts {
		_, err := f.service.Embeddings(context.Background(), f.plaintext, openaiapi.EmbeddingRequest{
			Model: "chat", Input: json.RawMessage(`["hello"]`),
		})
		if err == nil {
			t.Fatal("expected the request to be rejected once the budget is spent")
		}
	}
	if active := activeRequestsAfterReplay(t, f.log); active != 0 {
		t.Fatalf("budget-rejected embedding requests left %d of %d in flight", active, attempts)
	}
}
