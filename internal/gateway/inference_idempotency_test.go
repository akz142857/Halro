package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
)

func keyed(key string) context.Context {
	return requestmeta.WithIdempotencyKey(context.Background(), key)
}

// The behaviour the key is for: a caller that retries blindly after a timeout
// must not be given a second billed call upstream.
func TestARepeatedKeyDoesNotReachTheUpstreamTwice(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("retry-after-timeout")

	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("the first call made %d upstream calls, want 1", f.adapter.calls)
	}

	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 || refusal.Code != "idempotency_completed" {
		t.Fatalf("err = %v, want 409 idempotency_completed", err)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("the repeat reached the upstream: %d calls", f.adapter.calls)
	}
}

// The answer is not stored, so the repeat is refused rather than replayed. That
// is the decision this change is built on, and the refusal is where it is
// visible — so it is asserted rather than left to the manifest.
func TestACompletedKeyIsRefusedRatherThanReplayed(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("no-replay")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_completed" {
		t.Fatalf("err = %v", err)
	}
	// Nothing about the first answer is echoed back.
	for _, record := range f.store.all() {
		if record.Kind != domain.ResourceInferenceCall {
			continue
		}
		if record.ObjectPath != "" || record.InputObjectPath != "" {
			t.Fatalf("an idempotency record acquired an object: %+v", record)
		}
	}
}

// A key reused for a different request is a caller bug, and answering it as a
// repeat would serve them the wrong conclusion about a request they did send.
func TestTheSameKeyWithADifferentRequestIsRefused(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("same-key-other-body")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	other := chatRequest()
	other.Messages[0].Content = json.RawMessage(`"a different question entirely"`)
	_, err := f.service.Chat(ctx, f.plaintext, other)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 || refusal.Code != "idempotency_conflict" {
		t.Fatalf("err = %v, want 409 idempotency_conflict", err)
	}
}

// A failure still reached the upstream, so the key is spent: a retry after one
// is the second call it exists to prevent.
func TestAFailedRequestStillSpendsItsKey(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("failed-but-sent")
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Ambiguous: true, StatusCode: 500, Message: "upstream failed"}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	f.adapter.err = nil
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 {
		t.Fatalf("err = %v, want the key to be spent", err)
	}
}

// Two different Projects may use the same external key, because a key is only
// ever meaningful inside the account that issued it.
func TestKeysAreScopedToTheProject(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("shared-external-key")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	second := f.project
	second.ID = "project_2"
	plaintext, key, err := auth.GenerateGatewayKey(second.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeDiscovery}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys:     []domain.GatewayKey{f.key, key},
		projects: []domain.Project{f.project, second},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Chat(ctx, plaintext, chatRequest()); err != nil {
		t.Fatalf("a second project was refused another project's key: %v", err)
	}
}

// A caller who sends no key keeps exactly the behaviour they had, and pays for
// no durable state.
func TestWithoutAKeyNothingIsWritten(t *testing.T) {
	f := newDeferredFixture(t)
	for range 3 {
		if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if f.adapter.calls != 3 {
		t.Fatalf("calls = %d, want 3: an unkeyed request must not be deduplicated", f.adapter.calls)
	}
	for _, record := range f.store.all() {
		if record.Kind == domain.ResourceInferenceCall {
			t.Fatalf("an unkeyed request wrote a durable record: %+v", record)
		}
	}
}

// The header's shape is enforced before anything durable is written, so a
// malformed key cannot reserve a record nobody can name again.
func TestAMalformedKeyIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	f := newDeferredFixture(t)
	_, err := f.service.Chat(keyed("has a space"), f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 400 || refusal.Code != "invalid_idempotency_key" {
		t.Fatalf("err = %v, want 400 invalid_idempotency_key", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("a malformed key still reached the upstream: %d calls", f.adapter.calls)
	}
	for _, record := range f.store.all() {
		if record.Kind == domain.ResourceInferenceCall {
			t.Fatal("a malformed key reserved a record")
		}
	}
}

// Embeddings carry the same promise as chat; a key is not a chat-only feature.
func TestEmbeddingsHonourTheSameKey(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("embedding-retry")
	embedding := openaiapi.EmbeddingRequest{Model: "chat", Input: json.RawMessage(`["hello"]`)}
	if _, err := f.service.Embeddings(ctx, f.plaintext, embedding); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Embeddings(ctx, f.plaintext, embedding)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 {
		t.Fatalf("err = %v, want the key to be spent", err)
	}
}

// The streaming refusal has to be an HTTP error, not an event inside a stream
// the caller has already been told is starting. Nothing may have been emitted.
func TestAStreamingRepeatIsRefusedBeforeTheStreamOpens(t *testing.T) {
	f := newDeferredFixture(t)
	ctx := keyed("streaming-retry")
	request := chatRequest()
	request.Stream = true

	if err := f.service.ChatStream(ctx, f.plaintext, request, func(openaiapi.ChatCompletionResponse) error { return nil }); err != nil {
		t.Fatal(err)
	}
	emitted := 0
	err := f.service.ChatStream(ctx, f.plaintext, request, func(openaiapi.ChatCompletionResponse) error {
		emitted++
		return nil
	})
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 {
		t.Fatalf("err = %v, want a 409 before the stream opens", err)
	}
	if emitted != 0 {
		t.Fatalf("the refused repeat emitted %d chunks; it must refuse before anything is written", emitted)
	}
}

// A key held by a reservation from a process that is gone never reached the
// upstream, so it is reclaimable — otherwise a crash between reserving and
// dispatching would freeze the key until it expired.
func TestAReservationFromADeadProcessIsReclaimed(t *testing.T) {
	f := newDeferredFixture(t)
	f.store.failInFlightWrite = true
	ctx := keyed("crashed-before-dispatch")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the injected write failure did not surface")
	}
	if f.adapter.calls != 0 {
		t.Fatalf("the upstream was called before the reservation was in flight: %d", f.adapter.calls)
	}
	// Another process's reservation: the data directory is exclusive, so a
	// reservation naming an instance that is not this one belongs to a process
	// that is gone.
	for _, record := range f.store.all() {
		if record.Kind != domain.ResourceInferenceCall {
			continue
		}
		record.ReservedBy = "inst_from_a_previous_life"
		if _, err := f.store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatalf("a reservation from a dead process was not reclaimed: %v", err)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("calls = %d, want the reclaimed key to reach the upstream once", f.adapter.calls)
	}
}
