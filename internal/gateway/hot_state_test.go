package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// A request refused because the accounting state is at its limit is a 503
// telling the caller when to retry, and reaches no provider.
func TestHotStateLimitRefusesARequestWith503(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	maxTokens := int64(10)
	request := openaiapi.ChatCompletionRequest{
		Model: "chat", MaxCompletionTokens: &maxTokens,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
	}
	if _, err := f.service.Chat(context.Background(), f.plaintext, request); err != nil {
		t.Fatal(err)
	}
	limited, err := budget.NewWithOptions(f.log, f.state, mustResolver(t, "UTC"), budget.Options{MaxHotStateBytes: f.state.HotStateBytes(0)})
	if err != nil {
		t.Fatal(err)
	}
	f.service.accounting = limited
	_, err = f.service.Chat(context.Background(), f.plaintext, request)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "accounting_capacity_exhausted" || gatewayErr.HTTPStatus != 503 ||
		gatewayErr.RetryAfter != AccountingCapacityRetryAfter {
		t.Fatalf("error=%#v", err)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("provider calls=%d", f.adapter.calls)
	}
	if stats := limited.HotStateStats(); stats.RequestRejections != 1 {
		t.Fatalf("stats %#v", stats)
	}
}
