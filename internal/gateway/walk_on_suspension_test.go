package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// The question this file exists for.
//
// Whether the loop tries another upstream used to be answered by Retryable
// alone, which answers something else: may this same call be re-issued, a
// billing judgement the adapter owns. A 401 and a 402 are not retryable in that
// sense and never will be, so a request that hit one died there — the gateway
// had two more healthy providers configured and used neither.
//
// It was bound to Retryable for a reason. With nothing remembering the refusal,
// walking on meant every later request paid the same failed round trip to the
// same dead upstream, forever, silently. A suspension collapses that to once per
// window, which is what lets the binding come off.
func TestAStatedRefusalFallsOverOnceTheGateRemembersIt(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		reason provider.FailureReason
	}{
		// 401 needs no vendor knowledge: the classifier reaches
		// invalid_credential from the status alone.
		{name: "credential refused", status: 401},
		// 402 does. An adapter that knows what its own vendor means by it says
		// so, and only then can the gate act; the test below covers what happens
		// while nobody knows.
		{
			name:   "quota exhausted, where the adapter knows that is what 402 means",
			status: 402, reason: provider.FailureReasonSubscriptionQuotaExhausted,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, 10_000_000)
			defer f.close()
			f.adapter.err = &provider.Error{
				Class: provider.ErrorBadRequest, StatusCode: testCase.status,
				FailureReason: testCase.reason, Message: "refused",
			}
			fallback := &fakeAdapter{response: openaiapi.ChatCompletionResponse{
				ID: "chatcmpl_fallback", Object: "chat.completion", Model: "provider-model",
				Choices: []openaiapi.Choice{{Index: 0}},
				Usage:   &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			}}
			if err := f.registry.Register(provider.Target{
				ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
				ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
				CredentialID: "cred_other", CredentialRevision: 1,
				Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
				InputMicrosPerMillion:  1_000_000,
				OutputMicrosPerMillion: 2_000_000,
			}); err != nil {
				t.Fatal(err)
			}

			response, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
			if err != nil {
				t.Fatalf("a stated refusal ended the request instead of falling over: %v", err)
			}
			if response.ID != "chatcmpl_fallback" {
				t.Fatalf("answered by %q, want the fallback", response.ID)
			}
			if f.adapter.calls != 1 || fallback.calls != 1 {
				t.Fatalf("primary_calls=%d fallback_calls=%d", f.adapter.calls, fallback.calls)
			}
			if len(f.gate.Snapshot(time.Now())) == 0 {
				t.Fatal("the refusal was walked past but nothing was suspended, which is the tax this rule exists to avoid")
			}
		})
	}
}

// Ambiguous still stops the walk, and that is not a detail. A 5xx can be raised
// part way through a generation, so the upstream may already be billing for work
// it never returned; sending the same request somewhere else would pay for it
// twice and settling the first as free would hide the charge.
func TestAnAmbiguousFailureDoesNotWalkOnEvenWhenItSuspends(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, StatusCode: 500, Ambiguous: true, Message: "mid-generation",
	}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
		Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("an ambiguous failure was expected to end the request")
	}
	if fallback.calls != 0 {
		t.Fatalf("an ambiguous failure was duplicated onto another upstream: %d calls", fallback.calls)
	}
}

// The limit of what this can do today, stated rather than papered over.
//
// A 402 from an upstream whose adapter has not been taught what it means carries
// no canonical reason, so the gate reads it as one availability failure among
// others and the request ends there, as before. Guessing that every 402 means
// "out of quota" would be exactly the unverified vendor assumption this design
// refuses to make: one vendor's 402 is a spent balance, another's could be a
// per-request tier limit, and the repository holds evidence for one of them.
// Teaching the adapters is #319's job; this mechanism is what makes that
// teaching worth doing.
func TestAnUnclassified402DoesNotYetFallOver(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 402, Message: "payment required",
	}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
		Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("expected the request to end")
	}
	if fallback.calls != 0 {
		t.Fatalf("an unclassified 402 was walked past: %d fallback calls", fallback.calls)
	}
}

// A refusal nothing remembers must not be walked past, or the request pays a
// round trip per candidate and the next request does it all again.
func TestAnUnrememberedRefusalStillEndsTheRequest(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	// A 400 the upstream wrote about the request itself: not retryable, and
	// nothing about it says an upstream is unusable, so the gate suspends
	// nothing and the walk must stop.
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 400, Message: "your request is malformed",
	}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
		Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("a plain bad request was expected to end the request")
	}
	if fallback.calls != 0 {
		t.Fatalf("a refusal nothing remembers was walked past: %d fallback calls", fallback.calls)
	}
}

// Streaming was the path where this mattered most and the last one to get it.
//
// A caller that asked for a stream and was refused before a single byte came
// back is in exactly the position a unary caller is in: nothing has been
// delivered, nothing downstream has been committed to, and the next upstream can
// serve the request whole. The loop used to answer that with Retryable alone —
// so the one shape almost every SDK sends by default was also the one shape a
// dead credential could not fall over from.
func TestAStatedRefusalFallsOverOnAStreamThatHasSaidNothingYet(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	// No chunks: the refusal arrives instead of a first token, which is what
	// makes this a fallback rather than a truncation.
	f.adapter.streamChunks = nil
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 401,
		FailureReason: provider.FailureReasonInvalidCredential, Message: "refused",
	}
	fallback := &fakeAdapter{
		streamChunks: []openaiapi.ChatCompletionResponse{{
			ID: "chunk_fallback", Object: "chat.completion.chunk", Model: "provider-model",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
				Role: "assistant", Content: openaiapi.TextContent("served by the fallback"),
			}}},
		}},
		streamUsage: &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
		CredentialID: "cred_other", CredentialRevision: 1,
		Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	request := chatRequest()
	request.Stream = true
	var delivered []openaiapi.ChatCompletionResponse
	err := f.service.ChatStream(context.Background(), f.plaintext, request, func(chunk openaiapi.ChatCompletionResponse) error {
		delivered = append(delivered, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("a stated refusal ended the stream instead of falling over: %v", err)
	}
	if len(delivered) == 0 || delivered[0].ID != "chunk_fallback" {
		t.Fatalf("the stream was not served by the fallback: %#v", delivered)
	}
	if f.adapter.calls != 1 || fallback.calls != 1 {
		t.Fatalf("primary_calls=%d fallback_calls=%d", f.adapter.calls, fallback.calls)
	}
	if len(f.gate.Snapshot(time.Now())) == 0 {
		t.Fatal("the refusal was walked past but nothing was suspended, which is the tax this rule exists to avoid")
	}
}

// The line the walk rule does not cross, and the reason `emitted` is tested
// before anything else is asked.
//
// Once chunks have reached the client, the answer has started being told. A
// second upstream would tell a different one from the middle, and no framing
// makes that coherent — so a refusal this late ends the request even though the
// gate remembers it and even though a fallback is sitting right there. What the
// caller keeps is the truncated stream it already has.
func TestAStreamThatHasAlreadySpokenNeverWalksOn(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 401,
		FailureReason: provider.FailureReasonInvalidCredential, Message: "refused mid-stream",
	}
	fallback := &fakeAdapter{
		streamChunks: []openaiapi.ChatCompletionResponse{{
			ID: "chunk_fallback", Object: "chat.completion.chunk", Model: "provider-model",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
				Role: "assistant", Content: openaiapi.TextContent("a second answer"),
			}}},
		}},
		streamUsage: &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: fallback, Priority: 1,
		CredentialID: "cred_other", CredentialRevision: 1,
		Capabilities:           provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	request := chatRequest()
	request.Stream = true
	var delivered []openaiapi.ChatCompletionResponse
	err := f.service.ChatStream(context.Background(), f.plaintext, request, func(chunk openaiapi.ChatCompletionResponse) error {
		delivered = append(delivered, chunk)
		return nil
	})
	if err == nil {
		t.Fatal("a refusal after the first byte was expected to end the request")
	}
	if fallback.calls != 0 {
		t.Fatalf("a stream that had already spoken was continued by another upstream: %d calls", fallback.calls)
	}
	if len(delivered) == 0 {
		t.Fatal("the chunks the primary did deliver were lost, which is not what the caller saw")
	}
}
