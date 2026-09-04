package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/vault"
)

type deferredFixture struct {
	fixture
	store     *inferenceResourcesMemoryStore
	objectDir string
}

func newDeferredFixture(t *testing.T) deferredFixture {
	return newDeferredFixtureWith(t, nil)
}

func newDeferredFixtureWith(t *testing.T, shape func(*domain.Project)) deferredFixture {
	t.Helper()
	store := newInferenceResourcesMemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	sealer, err := vault.New(bytes.Repeat([]byte{0x5c}, vault.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sealer.Close)
	f := newFixtureShaped(t, 1_000_000, ledger.Options{}, nil,
		func(project *domain.Project) {
			project.DeferredResponses = true
			project.MaxOutputTokens = 0
			if shape != nil {
				shape(project)
			}
		},
		func(options *ServiceOptions) {
			options.Resources = store
			options.ResourceObjectDir = objectDir
			options.ResourceObjectSealer = sealer
		})
	t.Cleanup(f.close)
	return deferredFixture{fixture: f, store: store, objectDir: objectDir}
}

func (f deferredFixture) submit(t *testing.T, idempotencyKey string) openaiapi.Response {
	t.Helper()
	response, err := f.service.SubmitDeferredResponse(context.Background(), f.plaintext, idempotencyKey,
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// runOnce drains the queue synchronously, so a test asserts on a finished state
// rather than on a race with a dispatcher goroutine.
func (f deferredFixture) runOnce(t *testing.T) {
	t.Helper()
	pending, err := f.store.PendingDeferredResponses(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range pending {
		if record.Status == domain.DeferredQueued {
			f.service.runDeferredResponse(context.Background(), record)
		}
	}
}

func (f deferredFixture) record(t *testing.T, id string) domain.ProviderResource {
	t.Helper()
	record, err := f.store.ProviderResource(context.Background(), f.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestDeferredResponseCarriesRunAttributionFromSubmissionToSettlement(t *testing.T) {
	f := newDeferredFixtureWith(t, func(project *domain.Project) {
		project.RunGovernance = domain.RunGovernanceConfig{
			Enabled: true, DefaultRunBudgetMicrosUSD: 1_000_000, MaxRunBudgetMicrosUSD: 2_000_000,
			DefaultRunTTLSeconds: 3600, MaxRunTTLSeconds: 86400,
			MaxActiveRuns: 10, MaxOpenWorkUnits: 10,
		}
	})
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeRunAttach}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}
	intent := func(operation, keyHash string) budget.GovernanceIntent {
		return budget.GovernanceIntent{
			Operation:          operation,
			IdempotencyKeyHash: "sha256:" + keyHash,
			RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
	}
	workUnit, _, err := f.accounting.CreateWorkUnit(context.Background(), f.project.ID, f.key.ID, 10,
		intent("work_unit.create", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := f.accounting.CreateRun(context.Background(), f.project.ID, f.key.ID, workUnit.ID, 1_000_000, time.Hour, 10,
		intent("run.create", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestmeta.WithRunID(context.Background(), run.ID)
	response, err := f.service.SubmitDeferredResponse(ctx, f.plaintext, "deferred-run",
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true})
	if err != nil {
		t.Fatal(err)
	}
	record := f.record(t, response.ID)
	if record.WorkUnitID != workUnit.ID || record.RunID != run.ID {
		t.Fatalf("stored attribution=%s/%s", record.WorkUnitID, record.RunID)
	}
	f.runOnce(t)
	settled, ok := f.accounting.Run(f.project.ID, run.ID)
	if !ok || settled.CommittedMicrosUSD <= 0 || settled.ReservedMicrosUSD != 0 {
		t.Fatalf("settled Run=%#v found=%t", settled, ok)
	}
}

// The submission returns before any upstream is touched, and it writes no
// ledger events. Reserving budget here would let a request that waits ten
// minutes hold ten minutes of the Project's daily allowance, and would give
// crash recovery an unsettled lease that was never sent to reason about.
func TestSubmissionReachesNoProviderAndNoLedger(t *testing.T) {
	f := newDeferredFixture(t)
	calls := f.adapter.calls
	period := time.Now().UTC().Format("2006-01-02")
	before := f.state.Balance(f.project.ID, period, testTimezoneVersion)

	response := f.submit(t, "submit-key")

	if response.Status != domain.DeferredQueued || !response.Background {
		t.Fatalf("submission answered %#v", response)
	}
	if !strings.HasPrefix(response.ID, "resp_") {
		t.Fatalf("submission id is not a response id: %q", response.ID)
	}
	if f.adapter.calls != calls {
		t.Fatalf("a submission called an upstream: %d calls", f.adapter.calls-calls)
	}
	after := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if after != before {
		t.Fatalf("a submission moved the ledger: before=%#v after=%#v", before, after)
	}
	record := f.record(t, response.ID)
	if record.Status != domain.DeferredQueued || record.InputObjectPath == "" {
		t.Fatalf("record=%#v", record)
	}
	if record.ProviderID != "" && record.DeploymentID != "dep_target_1" {
		t.Fatalf("the route was not pinned at submission: %#v", record)
	}
}

// The synchronous path has never required an Idempotency-Key, and a deferred
// submission is the same generation. Requiring one would be a 400 the caller
// pays for a header they had no reason to send. Found by running the real
// binary: every submission without the header was refused.
func TestSubmissionDoesNotRequireAnIdempotencyKey(t *testing.T) {
	f := newDeferredFixture(t)
	response, err := f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "",
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true})
	if err != nil {
		t.Fatalf("a submission with no idempotency key was refused: %v", err)
	}
	if response.Status != domain.DeferredQueued {
		t.Fatalf("response=%#v", response)
	}
	f.runOnce(t)
	if record := f.record(t, response.ID); record.Status != domain.DeferredCompleted {
		t.Fatalf("record=%#v", record)
	}
	// A malformed key is still refused; the header is optional, not ignored.
	_, err = f.service.SubmitDeferredResponse(context.Background(), f.plaintext, strings.Repeat("k", 200),
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_idempotency_key" {
		t.Fatalf("a malformed idempotency key was accepted: %v", err)
	}
}

// The request the caller wrote is on disk between submission and execution, and
// it is not readable there. It is the same class of material failure capture has
// always sealed.
func TestStoredRequestIsSealedAndErasedOnceTheUpstreamHasAnswered(t *testing.T) {
	f := newDeferredFixture(t)
	response, err := f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "seal-key",
		openaiapi.ResponseRequest{
			Model: "chat", Input: json.RawMessage(`"canary-4b71-prompt-must-not-be-readable"`), Background: true,
		})
	if err != nil {
		t.Fatal(err)
	}
	record := f.record(t, response.ID)
	raw, err := os.ReadFile(filepath.Join(f.objectDir, record.InputObjectPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("canary-4b71-prompt-must-not-be-readable")) {
		t.Fatal("the stored request is readable in the object directory")
	}
	if !vault.SealedEnvelope(raw) {
		t.Fatal("the stored request carries no seal")
	}

	f.runOnce(t)

	record = f.record(t, response.ID)
	if record.Status != domain.DeferredCompleted {
		t.Fatalf("record=%#v", record)
	}
	if record.InputObjectPath != "" {
		t.Fatalf("the record still names the stored request: %q", record.InputObjectPath)
	}
	// Once the upstream has answered, the caller's prompt has no remaining
	// purpose here. Keeping it would be holding a copy of their traffic.
	entries, err := os.ReadDir(f.objectDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "."+objectRoleInput) {
			t.Fatalf("the stored request survived the answer: %s", entry.Name())
		}
	}
}

// Retrieval is the assertion this whole tier turns on: a caller polling every
// two seconds must not write eighteen hundred groups of WAL frames an hour for
// work that already happened.
func TestRetrievalWritesNoLedgerEventsAndCallsNoUpstream(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "poll-key")
	f.runOnce(t)

	period := time.Now().UTC().Format("2006-01-02")
	before := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	calls := f.adapter.calls

	for range 5 {
		response, retryAfter, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != "completed" || response.ID != submitted.ID || !response.Background {
			t.Fatalf("poll answered %#v", response)
		}
		if retryAfter != 0 {
			t.Fatalf("a terminal answer asked the caller to poll again in %s", retryAfter)
		}
	}
	if after := f.state.Balance(f.project.ID, period, testTimezoneVersion); after != before {
		t.Fatalf("polling moved the ledger: before=%#v after=%#v", before, after)
	}
	if f.adapter.calls != calls {
		t.Fatalf("polling reached an upstream %d times", f.adapter.calls-calls)
	}
}

// A queued poll carries the cadence in a header, not in the Response body: that
// object has an SDK contract and a non-standard field in it is a field somebody's
// client chokes on.
func TestQueuedPollSuggestsWhenToAskAgain(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "retry-after-key")
	response, retryAfter, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != domain.DeferredQueued {
		t.Fatalf("response=%#v", response)
	}
	if retryAfter < deferredMinRetryAfter || retryAfter > deferredMaxRetryAfter {
		t.Fatalf("retry-after=%s is outside the bounds the tier promises", retryAfter)
	}
}

// A deferred request's accounting must be indistinguishable from the same
// request made synchronously — that is the reason execution runs the same code
// rather than a parallel copy of it.
func TestExecutionChargesTheSameAsASynchronousRequest(t *testing.T) {
	period := time.Now().UTC().Format("2006-01-02")

	sync := newDeferredFixture(t)
	if _, err := sync.service.Responses(context.Background(), sync.plaintext,
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`)}); err != nil {
		t.Fatal(err)
	}
	expected := sync.state.Balance(sync.project.ID, period, testTimezoneVersion)

	deferred := newDeferredFixture(t)
	deferred.submit(t, "parity-key")
	deferred.runOnce(t)
	actual := deferred.state.Balance(deferred.project.ID, period, testTimezoneVersion)

	if actual.InputTokens != expected.InputTokens || actual.OutputTokens != expected.OutputTokens ||
		actual.CommittedMicrosUSD != expected.CommittedMicrosUSD || actual.ReservedMicrosUSD != expected.ReservedMicrosUSD {
		t.Fatalf("deferred=%#v synchronous=%#v", actual, expected)
	}
}

// Submission charges RPM, because it is a request; if it did not, background
// would be a way around a Project's per-minute ceiling.
func TestSubmissionConsumesAnRPMSlot(t *testing.T) {
	store := newInferenceResourcesMemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	sealer, err := vault.New(bytes.Repeat([]byte{0x5c}, vault.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sealer.Close)
	f := newFixtureShaped(t, 1_000_000, ledger.Options{}, nil,
		func(project *domain.Project) {
			project.DeferredResponses = true
			project.MaxOutputTokens = 0
			project.RPM = 1
		},
		func(options *ServiceOptions) {
			options.Resources = store
			options.ResourceObjectDir = objectDir
			options.ResourceObjectSealer = sealer
		})
	t.Cleanup(f.close)
	request := openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true}
	if _, err := f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "rpm-a", request); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "rpm-b", request)
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 429 {
		t.Fatalf("a second submission inside the same minute was not rate limited: %v", err)
	}
}

// The queue is bounded, and it refuses in the caller's face. An unbounded queue
// is fail-open exactly when it matters: the upstream slows, the queue grows in
// silence, and every entry is a promise of an answer.
func TestFullQueueRefusesTheSubmission(t *testing.T) {
	store := newInferenceResourcesMemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	sealer, err := vault.New(bytes.Repeat([]byte{0x5c}, vault.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sealer.Close)
	f := newFixtureShaped(t, 1_000_000, ledger.Options{}, nil,
		func(project *domain.Project) {
			project.DeferredResponses = true
			project.MaxOutputTokens = 0
			project.MaxDeferredQueue = 2
		},
		func(options *ServiceOptions) {
			options.Resources = store
			options.ResourceObjectDir = objectDir
			options.ResourceObjectSealer = sealer
		})
	t.Cleanup(f.close)
	request := openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true}
	for _, key := range []string{"queue-a", "queue-b"} {
		if _, err := f.service.SubmitDeferredResponse(context.Background(), f.plaintext, key, request); err != nil {
			t.Fatalf("submission %s: %v", key, err)
		}
	}
	_, err = f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "queue-c", request)
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 429 || failure.RetryAfter == 0 {
		t.Fatalf("a full queue did not refuse with a retry hint: %v", err)
	}
}

// The contract callers have to code against: a request in flight when the
// process dies comes back failed, and says it may have been billed. A deferred
// response has no upstream handle to ask — it was a plain synchronous call, and
// the socket died with the process.
func TestInterruptedRequestIsFailedRatherThanResumed(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "crash-key")
	record := f.record(t, submitted.ID)
	record.Status = domain.DeferredInProgress
	record.StartedAt = time.Now().UTC()
	record.ReservedBy = "inst_a_process_that_is_gone"
	if _, err := f.store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}

	f.service.deferred.recover(context.Background())

	recovered := f.record(t, submitted.ID)
	if recovered.Status != domain.DeferredFailed {
		t.Fatalf("an interrupted request was not failed: %#v", recovered)
	}
	if !strings.Contains(recovered.ErrorMessage, "billed") {
		t.Fatalf("the failure does not warn that it may have cost money: %q", recovered.ErrorMessage)
	}
	if recovered.InputObjectPath != "" {
		t.Fatalf("the stored request survived the failure: %q", recovered.InputObjectPath)
	}
}

// A queued record never reached an upstream, so re-running it duplicates
// nothing. This is the half of recovery that is safe, and it must not be
// confused with the half that is not.
func TestQueuedRequestIsReclaimedAfterARestart(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "reclaim-key")
	record := f.record(t, submitted.ID)
	record.ReservedBy = "inst_a_process_that_is_gone"
	if _, err := f.store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}

	f.service.deferred.recover(context.Background())

	recovered := f.record(t, submitted.ID)
	if recovered.Status != domain.DeferredQueued || recovered.ReservedBy != f.service.instanceID {
		t.Fatalf("a queued request was not reclaimed: %#v", recovered)
	}
	f.runOnce(t)
	if final := f.record(t, submitted.ID); final.Status != domain.DeferredCompleted {
		t.Fatalf("the reclaimed request did not run: %#v", final)
	}
}

// A Project at its concurrency or TPM ceiling is the exact pressure a queue
// exists to absorb. Failing the request there would answer "no" to the one
// condition the deferred tier was built for, and would throw away a request
// that never reached an upstream — beginRequestRun refuses before the
// reservation, so nothing has been decided and nothing has been paid.
func TestALimitAtDequeueReturnsTheRequestToTheQueue(t *testing.T) {
	f := newDeferredFixtureWith(t, func(project *domain.Project) { project.MaxConcurrency = 1 })
	submitted := f.submit(t, "")
	calls := f.adapter.calls

	held, err := f.service.limiter.AcquireExecutionSlot(f.project, 1, f.service.now())
	if err != nil {
		t.Fatal(err)
	}
	record := f.record(t, submitted.ID)
	if requeued := f.service.runDeferredResponse(context.Background(), record); !requeued {
		t.Fatal("a refusal at the concurrency ceiling did not report itself as a requeue")
	}
	blocked := f.record(t, submitted.ID)
	if blocked.Status != domain.DeferredQueued {
		t.Fatalf("the request was not returned to the queue: status=%q code=%q", blocked.Status, blocked.ErrorCode)
	}
	if blocked.InputObjectPath == "" {
		t.Fatal("the stored request was erased for a request that was never sent")
	}
	if !blocked.StartedAt.IsZero() {
		t.Fatal("the record still claims it started")
	}
	if f.adapter.calls != calls {
		t.Fatal("a request refused admission reached an upstream anyway")
	}

	// Once the slot frees, the same record runs to completion.
	held.Release()
	f.runOnce(t)
	if final := f.record(t, submitted.ID); final.Status != domain.DeferredCompleted {
		t.Fatalf("the requeued request did not run once the ceiling cleared: %#v", final)
	}
}

// A caller who cancelled is owed "cancelled". A caller whose request happened to
// be running when the process began shutting down cancelled nothing, and both
// arrive at the worker as one cancelled context.
func TestShutdownDoesNotTellTheCallerTheyCancelled(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "")
	record := f.record(t, submitted.ID)

	shutdown, stop := context.WithCancel(context.Background())
	f.service.deferred.runCtx = shutdown
	stop()
	f.service.runDeferredResponse(shutdown, record)

	after := f.record(t, submitted.ID)
	if after.Status != domain.DeferredFailed {
		t.Fatalf("a shutdown reported %q, want failed", after.Status)
	}
	if after.ErrorCode != "deferred_response_interrupted" {
		t.Fatalf("error code %q blames the caller for a shutdown", after.ErrorCode)
	}
	if !strings.Contains(after.ErrorMessage, "billed") {
		t.Fatalf("the failure does not warn that it may have cost money: %q", after.ErrorMessage)
	}
}

func TestCancelIsDeterminateWhileQueued(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "cancel-key")
	calls := f.adapter.calls

	response, err := f.service.CancelDeferredResponse(context.Background(), f.plaintext, submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != domain.DeferredCancelled {
		t.Fatalf("response=%#v", response)
	}
	f.runOnce(t)
	if f.adapter.calls != calls {
		t.Fatal("a cancelled submission reached an upstream anyway")
	}
	if record := f.record(t, submitted.ID); record.InputObjectPath != "" {
		t.Fatalf("the stored request survived cancellation: %q", record.InputObjectPath)
	}
}

// Delete removes both objects and the record. An object left behind is a file
// nothing can name or reap.
func TestDeleteRemovesTheRecordAndItsObjects(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "delete-key")
	f.runOnce(t)

	deleted, err := f.service.DeleteDeferredResponse(context.Background(), f.plaintext, submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Deleted || deleted.ID != submitted.ID || deleted.Object != "response.deleted" {
		t.Fatalf("deleted=%#v", deleted)
	}
	if _, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID); err == nil {
		t.Fatal("a deleted response was still served")
	}
	entries, err := os.ReadDir(f.objectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d object(s) outlived the record that named them", len(entries))
	}
}

// A running request cannot be deleted out from under its worker: the record is
// what the worker will write its outcome to.
func TestDeleteRefusesWhileTheRequestIsStillOwed(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "delete-inflight-key")
	_, err := f.service.DeleteDeferredResponse(context.Background(), f.plaintext, submitted.ID)
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 409 {
		t.Fatalf("deleting queued work answered %v", err)
	}
}

// A caller may collect the same answer twice — an HTTP 200 leaving Halro is not
// proof it arrived — and stops being able to once the cool-off has passed.
func TestAnswerSurvivesUntilTheCoolOffEnds(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "cooloff-key")
	f.runOnce(t)

	for range 2 {
		if _, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID); err != nil {
			t.Fatalf("a repeat retrieval inside the cool-off failed: %v", err)
		}
	}
	record := f.record(t, submitted.ID)
	if record.RetrievedAt.IsZero() {
		t.Fatal("the first retrieval did not start the cool-off")
	}
	if want := record.RetrievedAt.Add(deferredCoolOff); !record.ExpiresAt.Equal(want) {
		t.Fatalf("cool-off ends at %s, want %s", record.ExpiresAt, want)
	}

	record.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if _, err := f.store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID)
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 404 {
		t.Fatalf("a response past its cool-off answered %v", err)
	}
}

// Revoking a key has to stop the work it authorised, not only the work it has
// not yet submitted.
func TestWorkStopsWhenTheSubmittingKeyIsRevoked(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "revoke-key")
	calls := f.adapter.calls

	revoked := f.key
	revoked.Enabled = false
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{revoked}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}

	f.runOnce(t)

	if f.adapter.calls != calls {
		t.Fatal("work authorised by a revoked key reached an upstream")
	}
	record := f.record(t, submitted.ID)
	if record.Status != domain.DeferredFailed || record.ErrorCode != "invalid_api_key" {
		t.Fatalf("record=%#v", record)
	}
}

// A project that turns the feature off does not get a queue that keeps draining.
func TestDeferredSubmissionIsRefusedWhenTheProjectHasNotEnabledIt(t *testing.T) {
	store := newInferenceResourcesMemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	sealer, err := vault.New(bytes.Repeat([]byte{0x5c}, vault.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sealer.Close)
	f := newFixtureShaped(t, 1_000_000, ledger.Options{}, nil, nil,
		func(options *ServiceOptions) {
			options.Resources = store
			options.ResourceObjectDir = objectDir
			options.ResourceObjectSealer = sealer
		})
	t.Cleanup(f.close)
	_, err = f.service.SubmitDeferredResponse(context.Background(), f.plaintext, "disabled-key",
		openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Background: true})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unsupported_feature" {
		t.Fatalf("a project that never enabled deferred responses got %v", err)
	}
}

// Two submissions with no idempotency key are two submissions. Hashing the
// empty string would give them one shared key hash, and that field is a lookup
// index: one call away from answering "these are the same request".
func TestKeylessSubmissionsDoNotShareAnIdempotencyHash(t *testing.T) {
	f := newDeferredFixture(t)
	first := f.submit(t, "")
	second := f.submit(t, "")
	if first.ID == second.ID {
		t.Fatal("two keyless submissions collapsed into one record")
	}
	var zero [32]byte
	for _, id := range []string{first.ID, second.ID} {
		if hash := f.record(t, id).IdempotencyKeyHash; hash != zero {
			t.Fatalf("%s carries an idempotency hash for a key that was never sent: %x", id, hash[:4])
		}
	}
	// A key that was sent still deduplicates.
	keyed := f.submit(t, "same-key")
	again := f.submit(t, "same-key")
	if keyed.ID != again.ID {
		t.Fatalf("a repeated idempotency key created a second record: %s then %s", keyed.ID, again.ID)
	}
}

// A synchronous attempt is bounded by route_total_timeout. SafeTransport caps
// the connect and the response header, so a body that arrives one byte at a
// time is bounded by nothing — and on the deferred path it would hold one of a
// small number of workers while it did.
func TestADeferredAttemptIsBoundedLikeASynchronousOne(t *testing.T) {
	f := newDeferredFixture(t)
	f.service.deferredExecutionTimeout = 20 * time.Millisecond
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	f.adapter.release = released
	submitted := f.submit(t, "")
	f.runOnce(t)

	record := f.record(t, submitted.ID)
	if record.Status != domain.DeferredFailed {
		t.Fatalf("an attempt past its deadline reported %q", record.Status)
	}
	if record.ErrorCode != "deferred_response_timeout" {
		t.Fatalf("error code %q does not name the deadline", record.ErrorCode)
	}
	if !strings.Contains(record.ErrorMessage, "billed") {
		t.Fatalf("the failure does not warn that it may have cost money: %q", record.ErrorMessage)
	}
}

// The record claims to name the ledger request that paid for it. It has to
// actually do so, on failure as much as on success — a failed deferred request
// is the one an operator most needs to look up.
func TestTheRecordNamesTheLedgerRequestThatPaidForIt(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "")
	f.runOnce(t)
	completed := f.record(t, submitted.ID)
	if completed.Status != domain.DeferredCompleted {
		t.Fatalf("record=%#v", completed)
	}
	if !strings.HasPrefix(completed.AttemptID, "req_") {
		t.Fatalf("a completed record does not name its ledger request: %q", completed.AttemptID)
	}

	f.adapter.err = errors.New("upstream refused")
	failedSubmit := f.submit(t, "")
	f.runOnce(t)
	failed := f.record(t, failedSubmit.ID)
	if failed.Status != domain.DeferredFailed {
		t.Fatalf("record=%#v", failed)
	}
	if !strings.HasPrefix(failed.AttemptID, "req_") {
		t.Fatalf("a failed record does not name its ledger request: %q", failed.AttemptID)
	}
	if failed.AttemptID == completed.AttemptID {
		t.Fatal("two requests share one ledger identifier")
	}
}

// The TTL is the promise, and the reaper is how the record eventually leaves
// disk — not how the promise is kept. A submission nobody ever collected used
// to stay readable past its 24 hours until the hourly reaper happened to run,
// so how long a caller's answer lived depended on where in that hour it expired.
// ADR 0024 gives the deferred tier the same 24 hours as failure capture on the
// grounds that it is the same class of material, and failure capture expires
// record by record against the clock.
func TestAnUncollectedAnswerStopsBeingReadableAtItsTTLNotAtTheNextSweep(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "uncollected-key")
	f.runOnce(t)

	record := f.record(t, submitted.ID)
	if !record.RetrievedAt.IsZero() {
		t.Fatal("the fixture collected the answer; this case is about one nobody collected")
	}
	record.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if _, err := f.store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, submitted.ID)
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 404 {
		t.Fatalf("an expired answer nobody had collected answered %v", err)
	}
}
