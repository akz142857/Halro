package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
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

func TestDefinitiveRateLimitKeepsTheFingerprintAndAllowsOneRetry(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("temporary-refusal")
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: 7 * time.Second}
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != http.StatusTooManyRequests || refusal.RetryAfter != 7*time.Second {
		t.Fatalf("initial error = %v, want seven-second 429", err)
	}
	initial := f.store.all()
	if len(initial) != 1 || initial[0].CreationStatus != creationRetryableRefused {
		t.Fatal("explicit refusals must leave one reusable reservation")
	}
	f.adapter.err = nil
	now = now.Add(3 * time.Second)
	_, err = f.service.Chat(ctx, f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.HTTPStatus != http.StatusTooManyRequests || refusal.RetryAfter != 4*time.Second || f.adapter.calls != 1 {
		t.Fatalf("retry before recovery = %v, want remaining four seconds and no dispatch", err)
	}
	now = now.Add(4 * time.Second)
	other := chatRequest()
	other.Messages[0].Content = json.RawMessage(`"different request"`)
	_, err = f.service.Chat(ctx, f.plaintext, other)
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_conflict" || f.adapter.calls != 1 {
		t.Fatalf("different body = %v, want conflict before dispatch", err)
	}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatalf("same key after recovery: %v", err)
	}
	_, err = f.service.Chat(ctx, f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_completed" || f.adapter.calls != 2 {
		t.Fatalf("repeat after success = %v, want completed without dispatch", err)
	}
	completed := f.store.all()
	if len(completed) != 1 || completed[0].ID != initial[0].ID || !completed[0].CreatedAt.Equal(initial[0].CreatedAt) {
		t.Fatal("reclaim changed the durable claim identity")
	}
	if want := now.Add(inferenceIdempotencyTTL); !completed[0].ExpiresAt.Equal(want) {
		t.Fatalf("reclaimed expiry = %v, want a full lifetime from the retry (%v)", completed[0].ExpiresAt, want)
	}
}

func TestRateLimitOutcomeWriteFailureKeepsTheKeyInFlight(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("temporary-refusal-write-failed")
	f.store.failOutcomeWrite = true
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true}
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "resource_store_unavailable" {
		t.Fatalf("failed write = %v, want a 503 storage error", err)
	}
	// Same durable state a crash would leave after sending and before recording
	// the refusal. Another process has no execution evidence to release the key.
	f.service.instanceID = "inst_restarted"
	now = now.Add(time.Minute)
	f.adapter.err = nil
	_, err = f.service.Chat(ctx, f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_outcome_unknown" || f.adapter.calls != 1 {
		t.Fatalf("retry after restart = %v, want outcome unknown without a dispatch", err)
	}
}

func TestRateLimitedKeyHasOneConcurrentReclaimWinner(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("rate-limited-concurrent-retry")
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true}
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("initial result = %v, want 429", err)
	}
	now = now.Add(time.Minute)
	principal, targets, err := f.service.resolveRequest(ctx, f.plaintext, "chat", provider.OperationChat, "chat unavailable")
	if err != nil {
		t.Fatal(err)
	}
	request, err := openaiwire.DecodeGenerate(chatRequest())
	if err != nil {
		t.Fatal(err)
	}
	const concurrent = 8
	starting := make(chan struct{})
	results := make(chan error, concurrent)
	for range concurrent {
		go func() {
			<-starting
			_, _, err := f.service.admitInferenceIdempotency(ctx, principal, targets, "chat", request)
			results <- err
		}()
	}
	close(starting)
	winners := 0
	for range concurrent {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.As(err, &refusal) && refusal.Code == "idempotency_in_progress":
		default:
			t.Errorf("competing claim = %v, want in-progress", err)
		}
	}
	if winners != 1 {
		t.Fatalf("reclaim winners = %d, want one", winners)
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
	// Two injections, because a crash is both of them at once: the in-flight
	// write never lands, and nothing afterwards gets to tidy up. Failing only
	// the first would leave a request that releases its own unused reservation,
	// which is the other fix and has its own test.
	f.store.failInFlightWrite = true
	f.store.failReservationDelete = true
	ctx := keyed("crashed-before-dispatch")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the injected write failure did not surface")
	}
	if f.adapter.calls != 0 {
		t.Fatalf("the upstream was called before the reservation was in flight: %d", f.adapter.calls)
	}
	if !slices.ContainsFunc(f.store.all(), func(record domain.ProviderResource) bool {
		return record.Kind == domain.ResourceInferenceCall
	}) {
		t.Fatal("no reservation survived, so this test would pass without reclaiming anything")
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

// A request this gateway refuses on its own never reached the upstream, so it
// has nothing to protect: the key goes back, and the caller's retry gets the
// same reason again rather than a conflict about a call nobody made.
func TestARefusalBeforeDispatchGivesTheKeyBack(t *testing.T) {
	f := newDeferredFixtureWith(t, func(project *domain.Project) { project.MaxInputTokens = 1 })
	ctx := keyed("refused-before-dispatch")

	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "token_limit_exceeded" {
		t.Fatalf("err = %v, want 400 token_limit_exceeded", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("a refused request reached the upstream: %d calls", f.adapter.calls)
	}
	for _, record := range f.store.all() {
		if record.Kind == domain.ResourceInferenceCall {
			t.Fatalf("an undispatched request kept its key: %+v", record)
		}
	}
	// The retry is answered by the limit it broke, not by the key it reused.
	_, err = f.service.Chat(ctx, f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.Code != "token_limit_exceeded" {
		t.Fatalf("retry err = %v, want the same refusal rather than a conflict", err)
	}
}

// The same at the innermost stage there is: the request got as far as the write
// that records the dispatch, and failed on it. Nothing was sent, so the key is
// still the caller's — the only reason it would be kept is a call that was
// never made.
func TestAFailureAtTheDispatchMarkGivesTheKeyBack(t *testing.T) {
	f := newDeferredFixture(t)
	f.store.failInFlightWrite = true
	ctx := keyed("failed-to-record-the-dispatch")

	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the injected write failure did not surface")
	}
	if f.adapter.calls != 0 {
		t.Fatalf("the upstream was called without the dispatch being recorded: %d calls", f.adapter.calls)
	}
	for _, record := range f.store.all() {
		if record.Kind == domain.ResourceInferenceCall {
			t.Fatalf("an undispatched request kept its key: %+v", record)
		}
	}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatalf("the released key was still refused: %v", err)
	}
}

// The contract says a record expires in 24 hours and that expiry permits a new
// execution. This is that sentence.
func TestAnExpiredKeyIsUsableAgain(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("expires-and-comes-back")

	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the key was not spent while it was still live")
	}
	now = now.Add(inferenceIdempotencyTTL + time.Minute)
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatalf("an expired key was still refused: %v", err)
	}
	if f.adapter.calls != 2 {
		t.Fatalf("calls = %d, want 2: the expired key must reach the upstream again", f.adapter.calls)
	}
}

// A key nobody remembers issuing is free for whatever it is next used for, so
// expiry is decided ahead of the fingerprint rather than after it.
func TestAnExpiredKeyMayBeUsedForADifferentRequest(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("expires-then-reused")

	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(inferenceIdempotencyTTL + time.Minute)
	other := chatRequest()
	other.Messages[0].Content = json.RawMessage(`"a different question entirely"`)
	if _, err := f.service.Chat(ctx, f.plaintext, other); err != nil {
		t.Fatalf("an expired key was refused for a new request: %v", err)
	}
}

// A failed call spends its key, and the record that spends it has to leave when
// the key does. Every other resource kind withholds a record in this state
// because something may still exist upstream; this one owns nothing, and
// withholding it would refuse the key for the life of the install.
func TestAFailedKeyedRecordIsReleasedByTheReaper(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("failed-then-reaped")
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Ambiguous: true, StatusCode: 500, Message: "upstream failed"}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	f.adapter.err = nil

	var spent domain.ProviderResource
	for _, record := range f.store.all() {
		if record.Kind == domain.ResourceInferenceCall {
			spent = record
		}
	}
	if spent.ID == "" {
		t.Fatal("the failed call did not record its key")
	}
	if spent.CreationStatus != creationUnknown {
		t.Fatalf("CreationStatus = %q, want the ambiguous outcome", spent.CreationStatus)
	}
	if !spent.ExpiryReapable() {
		t.Fatal("a failed call's record is not reapable, so its key is spent for good")
	}
	now = now.Add(inferenceIdempotencyTTL + time.Minute)
	if err := f.service.CleanupExpiredProviderResource(context.Background(), spent); err != nil {
		t.Fatalf("the reaper refused the expired record: %v", err)
	}
	if _, err := f.store.ProviderResource(context.Background(), spent.ProjectID, spent.ID); err == nil {
		t.Fatal("the expired record survived the reaper")
	}
}

// The one question this machinery answers is "has this key been used", and a
// store that cannot answer it has not answered "no". Reading the failure as a
// free key would admit the duplicate billed call the header was sent to stop.
func TestAKeyThatCannotBeCheckedIsRefusedRatherThanAdmitted(t *testing.T) {
	f := newDeferredFixture(t)
	f.store.failIdempotencyLookup = true
	_, err := f.service.Chat(keyed("store-cannot-say"), f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 503 {
		t.Fatalf("err = %v, want 503: an unreadable store must not be read as a free key", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("an unverified key reached the upstream: %d calls", f.adapter.calls)
	}
}

// A store that cannot write and a key another request holds are two different
// operational facts, and an operator reading a 409 storm needs to know which
// one they have.
func TestAReservationOutageIsNotReportedAsARace(t *testing.T) {
	f := newDeferredFixture(t)
	f.store.failReservationWrite = true
	_, err := f.service.Chat(keyed("store-cannot-write"), f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 503 || refusal.Code != "resource_store_unavailable" {
		t.Fatalf("err = %v, want 503 resource_store_unavailable rather than a conflict", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("an unrecorded key reached the upstream: %d calls", f.adapter.calls)
	}
}

// A reclaimed key must outlive the call it is about to protect. Inheriting the
// first use's expiry would free the key seconds after the retry succeeded, and
// the caller's timeout retry would then run upstream a second time.
func TestAReclaimNearExpiryKeepsTheKeyForTheWholeCall(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	ctx := keyed("reclaim-near-expiry")
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: time.Second}
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the rate limit did not reach the caller")
	}
	f.adapter.err = nil
	now = now.Add(inferenceIdempotencyTTL - 5*time.Second)
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatalf("retry before expiry: %v", err)
	}
	now = now.Add(10 * time.Second)
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_completed" || f.adapter.calls != 2 {
		t.Fatalf("repeat just past the first use's expiry = %v (calls=%d), want completed without a dispatch", err, f.adapter.calls)
	}
}

// A completed key is answered as one even when the request would otherwise be
// refused before its key is reserved. A 429 would invite a backoff loop on a key
// that is never going to run again.
func TestACompletedKeyIsAnsweredAsOneWhileItsRouteIsSuspended(t *testing.T) {
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	done := keyed("completed-then-suspended")
	if _, err := f.service.Chat(done, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: 30 * time.Second}
	if _, err := f.service.Chat(keyed("suspends-the-route"), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the rate limit did not reach the caller")
	}
	f.adapter.err = nil
	_, err := f.service.Chat(done, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_completed" || f.adapter.calls != 2 {
		t.Fatalf("completed key on a suspended route = %v, want 409 idempotency_completed", err)
	}
	// A key with no history still hears about the suspension.
	_, err = f.service.Chat(keyed("fresh-while-suspended"), f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.Code != "provider_rate_limit" || f.adapter.calls != 2 {
		t.Fatalf("fresh key on a suspended route = %v, want 429 provider_rate_limit", err)
	}
}

// The two refusals call for opposite responses, so they must read differently:
// a request this process is still running is worth waiting for, and one whose
// outcome was lost reads the same for the rest of the key's lifetime.
func TestARunningKeyAndALostOutcomeAreToldApart(t *testing.T) {
	f := newDeferredFixture(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	f.adapter.started, f.adapter.release = started, release
	ctx := keyed("running-then-lost")
	done := make(chan error, 1)
	go func() {
		_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
		done <- err
	}()
	<-started
	f.adapter.started = nil
	_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "idempotency_in_progress" {
		t.Fatalf("repeat while running = %v, want idempotency_in_progress", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	lost := keyed("ambiguous-failure")
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Ambiguous: true, StatusCode: 500, Message: "upstream failed"}
	if _, err := f.service.Chat(lost, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	f.adapter.err = nil
	_, err = f.service.Chat(lost, f.plaintext, chatRequest())
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 409 || refusal.Code != "idempotency_outcome_unknown" {
		t.Fatalf("repeat after an ambiguous failure = %v, want 409 idempotency_outcome_unknown", err)
	}
}
