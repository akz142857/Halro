package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// finalizedOutcome reads back what the ledger was told about a request. The
// ledger is the accounting authority, so a test that asserts on the caller's
// error alone cannot see the half of this bug that mattered: the record.
func finalizedOutcome(t *testing.T, f fixture) string {
	t.Helper()
	outcome := ""
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventRequestFinalized {
			outcome = record.Event.Outcome
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outcome == "" {
		t.Fatal("no RequestFinalized event was written")
	}
	return outcome
}

// An answer the caller's wire cannot carry used to be discovered by the facade,
// after finish and finalize had already closed the request out as a success. The
// caller got a 502 for a request the ledger recorded as having succeeded, and
// nothing later reconciled the two: run.finalize is idempotent, so no path could
// correct the outcome after the fact.
//
// The upstream call really happened, so neither a refund nor a retry is owed
// here. What is owed is that the record and the answer agree.
func TestUnrenderableAnswerIsNotRecordedAsSuccess(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	renderFailure := errors.New("wire form cannot carry this content kind")
	_, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error { return renderFailure },
	)
	if err == nil {
		t.Fatal("a render that failed answered the caller successfully")
	}
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error is not a gateway error: %v", err)
	}
	if gatewayErr.HTTPStatus != 502 {
		t.Fatalf("status = %d, want 502", gatewayErr.HTTPStatus)
	}
	if !errors.Is(err, renderFailure) {
		t.Fatalf("the render's own error was dropped: %v", err)
	}
	if outcome := finalizedOutcome(t, f); outcome != "provider_error" {
		t.Fatalf("ledger outcome = %q, want provider_error — the record disagrees with the answer", outcome)
	}
}

// The successful path has to keep recording success, or the test above would
// pass against a service that failed everything.
func TestRenderedAnswerIsStillRecordedAsSuccess(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	rendered := false
	if _, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error { rendered = true; return nil },
	); err != nil {
		t.Fatal(err)
	}
	if !rendered {
		t.Fatal("generate returned without rendering the answer")
	}
	if outcome := finalizedOutcome(t, f); outcome != "success" {
		t.Fatalf("ledger outcome = %q, want success", outcome)
	}
}

// The render runs before the ledger is closed, not after. Ordering is the whole
// fix: a render called after finalize can still fail, and by then the outcome is
// written and cannot be corrected.
func TestRenderRunsBeforeTheRequestIsFinalized(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	outcomeAtRenderTime := "written before the render ran"
	if _, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error {
			outcomeAtRenderTime = ""
			if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
				if record.Event.Kind == ledger.EventRequestFinalized {
					outcomeAtRenderTime = record.Event.Outcome
				}
				return nil
			}); err != nil {
				return err
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if outcomeAtRenderTime != "" {
		t.Fatalf("the request was already finalized as %q when the render ran", outcomeAtRenderTime)
	}
}

// chatCanonical is the semantic form of the fixture's ordinary chat request, so
// these tests enter the hot path the same way a facade does.
func chatCanonical(t *testing.T) semantic.GenerateRequest {
	t.Helper()
	request := chatRequest()
	request.Model = "chat"
	canonical, err := openaiwire.DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// Messages renders twice on its way back — into the Chat wire and out of it into
// an Anthropic message — and both used to run after the request had been
// finalized. A tool call whose arguments are not valid JSON is refused by the
// Anthropic renderer, so the caller saw a 502 for a request the ledger had
// already closed as a success.
//
// Messages still reaches chat completions the same way it always did; what
// changed is that its renderings happen inside the request.
func TestMessagesRenderFailureIsNotRecordedAsSuccess(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	f.adapter.response.Choices = []openaiapi.Choice{{Message: &openaiapi.Message{
		Role: "assistant",
		ToolCalls: []openaiapi.ToolCall{{
			ID: "call_1", Type: "function",
			Function: openaiapi.ToolCallFunction{Name: "lookup", Arguments: "not json"},
		}},
	}}}
	_, err := f.service.Messages(context.Background(), f.plaintext, anthropicapi.MessageRequest{
		Model: "chat", MaxTokens: 16,
		Messages: []anthropicapi.MessageParam{{Role: "user", Content: anthropicapi.ContentBlocks{{Type: "text", Text: "hello"}}}},
	})
	if err == nil {
		t.Fatal("an answer the Anthropic wire cannot carry was returned successfully")
	}
	if outcome := finalizedOutcome(t, f); outcome == "success" {
		t.Fatal("the ledger recorded success for a request the caller saw fail")
	}
}
