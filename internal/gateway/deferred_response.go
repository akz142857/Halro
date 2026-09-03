package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/idempotency"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// The deferred tier: one generation submitted on one connection and collected
// on another.
//
// The queue is the bbolt records, not a channel. A channel would be a second
// place the pending set is written down, and the one that does not survive a
// restart — so on every restart the two would disagree and the durable one
// would be the loser. The dispatcher reads the records, and the signal channel
// only wakes it sooner than the timer would.

const (
	// deferredMaxInputBytes and deferredMaxOutputBytes bound what one record may
	// put on disk, in the shape failure capture already uses: refuse at
	// submission rather than discover after the upstream has been paid.
	deferredMaxInputBytes  = 256 << 10
	deferredMaxOutputBytes = 1 << 20

	deferredDefaultTTL     = 24 * time.Hour
	deferredCoolOff        = 15 * time.Minute
	deferredRetryFloor     = 250 * time.Millisecond
	deferredRetryCeiling   = 5 * time.Second
	deferredMinRetryAfter  = time.Second
	deferredMaxRetryAfter  = 10 * time.Second
	deferredDefaultWorkers = 4

	// defaultDeferredExecutionTimeout matches the default route_total_timeout a
	// synchronous request runs under. An operator who raises one should raise
	// the other; leaving this unbounded would make "the deferred path is the
	// synchronous path" false in the one way that matters under load.
	defaultDeferredExecutionTimeout = 2 * time.Minute
)

// deferredEngine dispatches queued work. It holds no queue of its own: running
// tracks what is executing right now, which is what cancellation needs to reach
// and what stops a second worker picking up a record already in flight.
type deferredEngine struct {
	service *Service
	signal  chan struct{}
	slots   chan struct{}
	running sync.Map
	wait    sync.WaitGroup
	// runCtx is the context the whole engine lives under, so a worker can tell
	// the two cancellations apart. A caller cancelling one request and the
	// process shutting down both arrive as a cancelled worker context, and they
	// owe the caller different answers.
	runCtx context.Context
	// blocked says a worker handed its record back to the queue because the
	// Project was at a limit. It is what stops the dispatcher from spinning: the
	// record really is ready to run, and nothing but time will change that.
	blocked atomic.Bool
}

func newDeferredEngine(service *Service, workers int) *deferredEngine {
	if workers <= 0 {
		workers = deferredDefaultWorkers
	}
	return &deferredEngine{
		service: service,
		signal:  make(chan struct{}, 1),
		slots:   make(chan struct{}, workers),
	}
}

// nudge wakes the dispatcher without blocking. A signal already pending means
// the dispatcher has not yet read the records, so it will see this submission
// too; there is nothing a second signal would add.
func (e *deferredEngine) nudge() {
	select {
	case e.signal <- struct{}{}:
	default:
	}
}

// RunDeferredResponses drives the deferred tier until ctx ends. It returns only
// after every worker it started has finished, so a shutdown does not leave a
// record in in_progress that this process could still have settled.
func (s *Service) RunDeferredResponses(ctx context.Context) {
	engine := s.deferred
	if engine == nil {
		return
	}
	engine.runCtx = ctx
	engine.recover(ctx)
	backoff := deferredRetryFloor
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			engine.wait.Wait()
			return
		case <-engine.signal:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		// Work left behind means the instance is at its worker ceiling or a
		// Project is at its TPM or concurrency limit. Backing off keeps the
		// dispatcher from scanning bbolt in a tight loop for as long as that
		// lasts; a fresh submission still wakes it immediately through nudge.
		left := engine.dispatch(ctx)
		if engine.blocked.Swap(false) {
			left = true
		}
		if left {
			backoff = min(backoff*2, deferredRetryCeiling)
		} else {
			backoff = deferredRetryFloor
		}
		timer.Reset(backoff)
	}
}

// interleaveByProject takes one submission from each Project in turn, keeping
// each Project's own submissions in the order they arrived.
//
// The store returns the queue ordered by submission time across the whole
// instance, and the ceiling that bounds it is per Project while the workers
// draining it are process-wide. Serving that global order means one Project can
// fill its own queue — up to MaxDeferredQueueCeiling — and every other Project's
// submissions wait behind all of it. At the default ceiling and the default
// execution timeout that wait exceeds the 24-hour TTL, so those submissions do
// not merely run late: they expire without ever having been tried, and an
// expiry writes no ledger event, appears in no failed-request list, and has its
// record reaped an hour later. The operator's only signal is a complaint.
//
// Round-robin makes the ceiling mean what it says — a bound on one Project's
// backlog rather than on everyone's — without giving any Project a reservation
// it did not have before: a Project with nothing queued takes no turn.
func interleaveByProject(pending []domain.ProviderResource) []domain.ProviderResource {
	if len(pending) < 2 {
		return pending
	}
	order := make([]string, 0, 8)
	byProject := make(map[string][]domain.ProviderResource, 8)
	for _, record := range pending {
		if _, seen := byProject[record.ProjectID]; !seen {
			order = append(order, record.ProjectID)
		}
		byProject[record.ProjectID] = append(byProject[record.ProjectID], record)
	}
	if len(order) == 1 {
		return pending
	}
	// Projects take their turn in the order their oldest waiting submission
	// arrived, so the global ordering still decides who goes first; it just no
	// longer decides who goes at all.
	interleaved := make([]domain.ProviderResource, 0, len(pending))
	for len(interleaved) < len(pending) {
		for _, projectID := range order {
			queue := byProject[projectID]
			if len(queue) == 0 {
				continue
			}
			interleaved = append(interleaved, queue[0])
			byProject[projectID] = queue[1:]
		}
	}
	return interleaved
}

// dispatch starts what it can and reports whether anything was left waiting.
func (e *deferredEngine) dispatch(ctx context.Context) bool {
	pending, err := e.service.resources.PendingDeferredResponses(ctx, "")
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			e.service.logger.Error("deferred response queue could not be read", "error", err)
		}
		return false
	}
	for _, record := range interleaveByProject(pending) {
		if ctx.Err() != nil {
			return false
		}
		if _, busy := e.running.Load(record.ID); busy {
			continue
		}
		// A submission that outlived its TTL without running is failed rather
		// than left queued. ExpiryReapable refuses to reap anything that is not
		// terminal — correctly, since a queued record is still work Halro owes —
		// so nothing else would ever clear it.
		if !record.ExpiresAt.After(e.service.now()) {
			if err := e.service.finishDeferred(ctx, record, domain.DeferredFailed, "deferred_response_expired",
				"the request expired before it could be run", nil); err != nil {
				e.service.logger.Error("an expired deferred response could not be failed", "resource_id", record.ID, "error", err)
			}
			continue
		}
		if record.Status != domain.DeferredQueued {
			continue
		}
		select {
		case e.slots <- struct{}{}:
		default:
			return true
		}
		workerCtx, cancel := context.WithCancel(ctx)
		e.running.Store(record.ID, cancel)
		e.wait.Add(1)
		go func(record domain.ProviderResource) {
			defer e.wait.Done()
			defer func() {
				e.running.Delete(record.ID)
				cancel()
				<-e.slots
			}()
			if e.service.runDeferredResponse(workerCtx, record) {
				// Handed back to the queue because a limit was full. Waking the
				// dispatcher now would pick the same record up again
				// immediately and be refused again — a hot loop against bbolt
				// and the limiter for as long as the Project stays at its
				// ceiling. The timer retries instead, and backs off.
				e.blocked.Store(true)
				return
			}
			// A freed slot is only useful if something notices. Waking the
			// dispatcher here is what turns the worker count into a moving
			// pipeline rather than a batch that drains and then waits out the
			// timer.
			e.nudge()
		}(record)
	}
	return false
}

// recover decides what the previous process left behind.
//
// A queued record never reached an upstream, so it is simply re-enqueued. A
// record that reached in_progress is failed, and that is a contract rather than
// an implementation detail: unlike a batch, a deferred response has no upstream
// handle. It was a plain synchronous call, and when the process died the socket
// died with it — Halro cannot learn whether the work completed, whether it was
// billed, or what it said. ADR 0011 settles an outcome nobody can determine
// conservatively, and the only honest status to show is failed.
func (e *deferredEngine) recover(ctx context.Context) {
	pending, err := e.service.resources.PendingDeferredResponses(ctx, "")
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			e.service.logger.Error("deferred responses could not be recovered", "error", err)
		}
		return
	}
	failed, requeued := 0, 0
	for _, record := range pending {
		if record.ReservedBy == e.service.instanceID {
			continue
		}
		switch record.Status {
		case domain.DeferredQueued:
			record.ReservedBy = e.service.instanceID
			if _, err := e.service.saveDeferred(ctx, record); err != nil {
				e.service.logger.Error("a queued deferred response could not be reclaimed", "resource_id", record.ID, "error", err)
				continue
			}
			requeued++
		case domain.DeferredInProgress:
			if err := e.service.finishDeferred(ctx, record, domain.DeferredFailed,
				"deferred_response_interrupted",
				"the gateway restarted while this request was running; it may have been billed upstream and its answer cannot be retrieved",
				nil); err != nil {
				e.service.logger.Error("an interrupted deferred response could not be failed", "resource_id", record.ID, "error", err)
				continue
			}
			failed++
		}
	}
	if failed > 0 || requeued > 0 {
		e.service.logger.Warn("recovered deferred responses held by a previous process", "failed", failed, "requeued", requeued)
	}
	e.nudge()
}

// SubmitDeferredResponse accepts a generation the caller will collect later.
//
// It performs admission and nothing else: authentication, source policy, the
// alias check, one RPM slot, queue depth, and that the route resolves today.
// Deliberately absent is any ledger write. ADR 0011 requires a budget
// reservation to be durable before Provider I/O, not before queueing; reserving
// here would let a submission that waits ten minutes hold ten minutes of a
// Project's daily budget, and would force crash recovery to tell an unsettled
// lease that was queued from one that was sent — a state that exists for no
// other reason.
func (s *Service) SubmitDeferredResponse(
	ctx context.Context,
	plaintextKey, idempotencyKey string,
	request openaiapi.ResponseRequest,
) (openaiapi.Response, error) {
	if s.deferred == nil || s.resources == nil {
		return openaiapi.Response{}, gatewayError("unsupported_feature", "deferred responses are unavailable", 501, nil)
	}
	if !request.Background {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "background must be true", 400, nil)
	}
	// Optional, unlike the resource endpoints. Those mint an upstream object that
	// must never be created twice, so they require a key. A deferred submission
	// is the same generation the synchronous path takes without one, and
	// requiring a header here that /v1/responses has never required would be a
	// divergence the caller pays for with a 400.
	if idempotencyKey != "" {
		if err := idempotency.ValidateKey(idempotencyKey); err != nil {
			return openaiapi.Response{}, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
		}
	}
	principal, targets, err := s.resolveRequest(
		ctx, plaintextKey, request.Model, provider.OperationChat,
		"model route does not support chat completions",
	)
	if err != nil {
		return openaiapi.Response{}, err
	}
	if !principal.Project.DeferredResponses {
		// Off unless an operator turned it on, because turning it on changes
		// what this instance's data directory holds.
		return openaiapi.Response{}, gatewayError(
			"unsupported_feature", "deferred responses are not enabled for this project", 400, nil,
		)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	if len(payload) > deferredMaxInputBytes {
		return openaiapi.Response{}, gatewayError(
			"request_too_large", "request is too large to hold for deferred retrieval", 413, nil,
		)
	}
	// Resolving now and pinning the answer is the whole of D3: between here and
	// execution a deployment may be edited, disabled or deleted, and serving an
	// answer from a route the caller never asked for is the one outcome that
	// cannot be explained to them.
	canonical, err := openai.DecodeResponseGenerate(request)
	if err != nil {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	target, err := s.pinDeferredTarget(targets, canonical)
	if err != nil {
		return openaiapi.Response{}, err
	}
	// Left zero when the caller sent no key. Hashing the empty string would
	// stamp every keyless record with the same value, and that value is an index
	// — one lookup away from answering "these are all the same submission".
	var keyHash [32]byte
	fingerprint := sha256.Sum256(payload)
	if idempotencyKey != "" {
		keyHash = sha256.Sum256([]byte(idempotencyKey))
		existing, verdict, err := s.classifyIdempotency(ctx, principal.Project.ID, domain.ResourceDeferredResponse, keyHash, fingerprint)
		if err != nil {
			return openaiapi.Response{}, err
		}
		if verdict == idempotencyCompleted || verdict == idempotencyInProgress {
			return s.renderDeferred(existing)
		}
	}
	if err := s.admitDeferredQueue(ctx, principal.Project); err != nil {
		return openaiapi.Response{}, err
	}
	if err := s.limiter.AcquireRequestSlot(principal.Project, s.now()); err != nil {
		return openaiapi.Response{}, s.mapLimitError(err)
	}
	externalID, err := id.New("resp")
	if err != nil {
		return openaiapi.Response{}, gatewayError("internal_error", "unable to create response ID", 500, err)
	}
	now := s.now()
	record := domain.ProviderResource{
		ID: externalID, Kind: domain.ResourceDeferredResponse, ProjectID: principal.Project.ID,
		ProviderID: target.ProviderID, DeploymentID: target.DeploymentID, PublicModel: request.Model,
		ProfileID: target.ProfileID, Region: target.Region,
		KeyID: principal.Key.ID, IdempotencyKeyHash: keyHash, RequestFingerprint: fingerprint,
		CreationStatus: creationCompleted, ReservedBy: s.instanceID,
		Status: domain.DeferredQueued, SubmittedAt: now,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(deferredDefaultTTL),
	}
	// The request goes to disk before the record does. A sealed object nothing
	// names is swept once it is old enough that no write could still be holding
	// it; a record naming an object that was never written is a queued request
	// the worker can only fail.
	inputPath, err := s.writeResourceObject(record.ID, record.ProjectID, objectRoleInput, payload)
	if err != nil {
		return openaiapi.Response{}, gatewayError("resource_store_unavailable", "the request could not be stored", 503, err)
	}
	record.InputObjectPath = inputPath
	stored, err := s.resources.PutProviderResource(ctx, record, 0)
	if err != nil {
		_ = s.removeResourceObject(inputPath)
		return openaiapi.Response{}, gatewayError("resource_store_unavailable", "the submission could not be recorded", 503, err)
	}
	s.deferred.nudge()
	return s.renderDeferred(stored)
}

// pinDeferredTarget chooses the deployment the record will be bound to, using
// the same filters the synchronous path applies. Choosing here rather than at
// execution is what makes the pin meaningful: a request is accepted only if a
// route could serve it at the moment the caller asked.
func (s *Service) pinDeferredTarget(targets []provider.Target, canonical semantic.GenerateRequest) (provider.Target, error) {
	candidates := targets
	targets = filterSemanticCapabilities(targets, canonical.Requirements)
	targets = filterGenerateProfileCompatibility(targets, canonical)
	targets = filterUnrenderableReasoning(targets, canonical)
	targets = filterPrimitiveTargets(targets, provider.OperationChat)
	if len(targets) == 0 {
		return provider.Target{}, s.unservableError(
			"model route does not support the requested chat capabilities",
			unservableReasons(candidates, canonical, provider.OperationChat),
		)
	}
	return targets[0], nil
}

func (s *Service) admitDeferredQueue(ctx context.Context, project domain.Project) error {
	depth := project.MaxDeferredQueue
	if depth <= 0 {
		depth = domain.DefaultMaxDeferredQueue
	}
	pending, err := s.resources.PendingDeferredResponses(ctx, project.ID)
	if err != nil {
		return gatewayError("resource_store_unavailable", "the deferred queue could not be read", 503, err)
	}
	// Approximate under concurrency, deliberately. Two submissions racing can
	// both read a depth one below the ceiling and both be admitted, so the real
	// bound is the ceiling plus the number of simultaneous submitters — which
	// RPM already bounds. A lock here would buy exactness on a number that
	// exists to stop unbounded growth, not to be a quota.
	if int64(len(pending)) >= depth {
		// A bounded queue refuses in the caller's face at the moment the
		// pressure exists. An unbounded one grows silently while every entry is
		// a promise of an answer.
		return &Error{
			Code: "rate_limit_exceeded", Message: "the deferred response queue is full", HTTPStatus: 429,
			RetryAfter: deferredMinRetryAfter,
		}
	}
	return nil
}

// runDeferredResponse executes one queued submission.
//
// From beginRequestRun onwards this is the synchronous path, byte for byte: the
// same reservation, the same attempt ladder, the same settlement, the same
// redaction authority. That identity is deliberate — a deferred request's ledger
// events must be indistinguishable from the same request made synchronously,
// and the only way to guarantee that is to run the same code.
// It reports whether the record was handed back to the queue rather than
// finished, which happens when the Project is at a limit the queue exists to
// wait out.
func (s *Service) runDeferredResponse(ctx context.Context, record domain.ProviderResource) bool {
	principal, target, err := s.resolveDeferredRequest(record)
	if err != nil {
		s.abandonDeferred(ctx, record, err)
		return false
	}
	payload, err := s.readResourceInputObject(record)
	if err != nil {
		s.abandonDeferred(ctx, record, gatewayError(
			"deferred_response_unreadable", "the stored request could not be read", 500, err,
		))
		return false
	}
	var request openaiapi.ResponseRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		s.abandonDeferred(ctx, record, gatewayError(
			"deferred_response_unreadable", "the stored request could not be decoded", 500, err,
		))
		return false
	}
	canonical, err := openai.DecodeResponseGenerate(request)
	if err != nil {
		s.abandonDeferred(ctx, record, gatewayError(
			"invalid_request_error", "request cannot be represented safely", 400, err,
		))
		return false
	}
	// in_progress is written before the upstream is called, not after, and it is
	// what a restart reads as "may have been billed". Erring in this direction
	// is the point: a request that never left is reported as ambiguous, which
	// costs a caller a retry, where the reverse would report a call that did
	// leave as never having happened.
	record.Status = domain.DeferredInProgress
	record.StartedAt = s.now()
	record.ReservedBy = s.instanceID
	started, err := s.saveDeferred(ctx, record)
	if err != nil {
		s.logger.Error("a deferred response could not be marked in progress", "resource_id", record.ID, "error", err)
		return false
	}
	record = started

	// Bounded like a synchronous attempt. SafeTransport caps the connect and the
	// response header; a body arriving one byte at a time is capped by nothing,
	// and it would hold one of a small number of workers while it did.
	attemptCtx, releaseAttempt := context.WithTimeout(ctx, s.deferredExecutionTimeout)
	defer releaseAttempt()
	var rendered openaiapi.Response
	requestID := ""
	_, execErr := s.executeGenerate(attemptCtx, principal, []provider.Target{target}, record.PublicModel, canonical,
		func(generated semantic.GenerateResult) error {
			response, renderErr := openai.RenderResponseResult(generated, request)
			if renderErr != nil {
				return renderErr
			}
			rendered = response
			return nil
		}, admitExecutionOnly, &requestID)
	// Recorded on both outcomes. A failed deferred request is exactly the one an
	// operator has to be able to look up in the ledger, and until now this field
	// promised that link in a comment and never wrote it.
	record.AttemptID = requestID
	if execErr != nil {
		status, code, message := domain.DeferredFailed, "provider_error", "the request could not be completed"
		var failure *Error
		if errors.As(execErr, &failure) {
			code, message = failure.Code, failure.Message
		}
		// A Project at its TPM or concurrency ceiling is the condition the queue
		// exists for. Failing here would make the deferred tier answer "no" to
		// exactly the pressure it was built to absorb, and would throw away a
		// request that has not been sent anywhere. Nothing reached an upstream —
		// beginRequestRun refuses before the reservation — so the record goes
		// back to queued with its stored request intact.
		if deferredAdmissionRefusal(execErr) && ctx.Err() == nil {
			if err := s.requeueDeferred(ctx, record); err != nil {
				s.logger.Error("a deferred response could not be returned to the queue", "resource_id", record.ID, "error", err)
				return false
			}
			return true
		}
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// The attempt outlived its own deadline, which is neither a
			// cancellation nor a shutdown. It reached the upstream, so it may
			// have been paid for, and the attempt has already been settled.
			code = "deferred_response_timeout"
			message = "the request exceeded the time one attempt is allowed and may have been billed"
		} else if ctx.Err() != nil {
			// Two different cancellations arrive here as one. A caller who
			// cancelled is owed "cancelled"; a caller whose request was running
			// when the process began shutting down cancelled nothing, and
			// telling them they did would be a lie about their own traffic.
			// Either way the attempt has already been settled conservatively, and
			// either way the upstream may have been paid.
			if s.deferred != nil && s.deferred.runCtx != nil && s.deferred.runCtx.Err() != nil {
				code = "deferred_response_interrupted"
				message = "the gateway shut down while this request was running; it may have been billed upstream and its answer cannot be retrieved"
			} else {
				status, code = domain.DeferredCancelled, "deferred_response_cancelled"
				message = "the request was cancelled after it reached the upstream and may have been billed"
			}
		}
		if err := s.finishDeferred(ctx, record, status, code, message, nil); err != nil {
			s.logger.Error("a failed deferred response could not be recorded", "resource_id", record.ID, "error", err)
		}
		return false
	}
	// The identifier the caller was given at submission is the identifier the
	// answer carries. Anything else would hand them a second name for one thing.
	rendered.ID = record.ID
	rendered.Background = true
	answer, err := json.Marshal(rendered)
	if err != nil {
		if err := s.finishDeferred(ctx, record, domain.DeferredFailed, "deferred_response_unstorable",
			"the answer could not be stored", nil); err != nil {
			s.logger.Error("an unstorable deferred response could not be recorded", "resource_id", record.ID, "error", err)
		}
		return false
	}
	if len(answer) > deferredMaxOutputBytes {
		// The upstream has already answered and already been paid — the ceiling
		// cannot be checked before the work happens, only before it is stored.
		// So the message says so: this is not a request the caller can shrink
		// and retry for free.
		if err := s.finishDeferred(ctx, record, domain.DeferredFailed, "deferred_response_too_large",
			"the answer exceeds the size a deferred response may hold; the request reached the upstream and was billed", nil); err != nil {
			s.logger.Error("an oversized deferred response could not be recorded", "resource_id", record.ID, "error", err)
		}
		return false
	}
	if err := s.finishDeferred(ctx, record, domain.DeferredCompleted, "", "", answer); err != nil {
		s.logger.Error("a completed deferred response could not be recorded", "resource_id", record.ID, "error", err)
	}
	return false
}

// deferredAdmissionRefusal reports whether execution was refused by a limit that
// time alone clears. A budget refusal is deliberately not one of these: it does
// not clear until the accounting day does, which is longer than the record
// lives, so retrying it until the TTL would only delay the same answer.
func deferredAdmissionRefusal(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "concurrency_limit_exceeded", "token_rate_limit_exceeded", "rate_limit_exceeded":
		return true
	}
	return false
}

// requeueDeferred puts a record back where dispatch will find it, keeping the
// stored request: nothing has been sent, so nothing has been decided.
func (s *Service) requeueDeferred(ctx context.Context, record domain.ProviderResource) error {
	record.Status = domain.DeferredQueued
	record.StartedAt = time.Time{}
	_, err := s.saveDeferred(ctx, record)
	return err
}

// resolveDeferredRequest re-derives the principal and the pinned deployment
// without the caller's key.
//
// Re-authenticating is impossible — the plaintext key was never stored, and
// storing one to replay later would be worse than anything replaying it enables
// — but the identifier is enough to ask whether this instance still accepts that
// key. Revoking a key has to stop the work it authorised, not only the work not
// yet submitted, so this runs at execution rather than only at submission.
func (s *Service) resolveDeferredRequest(record domain.ProviderResource) (auth.AuthResult, provider.Target, error) {
	principal, err := s.auth.AuthorizeKeyID(record.KeyID, s.now())
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, gatewayError(
			"invalid_api_key", "the key that submitted this request is no longer valid", 401, err,
		)
	}
	if !principal.Project.DeferredResponses {
		return auth.AuthResult{}, provider.Target{}, gatewayError(
			"unsupported_feature", "deferred responses are no longer enabled for this project", 400, nil,
		)
	}
	if !slices.Contains(principal.Project.AllowedModels, record.PublicModel) {
		return auth.AuthResult{}, provider.Target{}, gatewayError(
			"model_not_allowed", "model is no longer allowed for this project", 403, nil,
		)
	}
	if err := s.assertPolicySnapshotsCoverProject(principal); err != nil {
		return auth.AuthResult{}, provider.Target{}, err
	}
	// The pin, honoured. ownedTarget answers with the one deployment the record
	// names, or refuses — which is the explainable failure D3 chose over serving
	// an answer from a route the caller never asked for.
	target, err := s.ownedTarget(record)
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, gatewayError(
			"deferred_response_route_unavailable",
			"the deployment selected when this request was submitted is no longer available", 409, err,
		)
	}
	return principal, target, nil
}

func (s *Service) abandonDeferred(ctx context.Context, record domain.ProviderResource, cause error) {
	code, message := "provider_error", "the request could not be completed"
	var failure *Error
	if errors.As(cause, &failure) {
		code, message = failure.Code, failure.Message
	}
	if err := s.finishDeferred(ctx, record, domain.DeferredFailed, code, message, nil); err != nil {
		s.logger.Error("an abandoned deferred response could not be recorded", "resource_id", record.ID, "error", err)
	}
}

// finishDeferred writes a terminal state and erases the stored request.
//
// The request is erased on every terminal path, not only the successful one.
// Once the upstream has answered — or will never be asked — the caller's prompt
// has no remaining purpose here, and keeping it would be holding a copy of their
// traffic rather than the bounded tail this project has always limited itself to.
func (s *Service) finishDeferred(
	ctx context.Context,
	record domain.ProviderResource,
	status, code, message string,
	answer []byte,
) error {
	if len(answer) > 0 {
		path, err := s.writeResourceObject(record.ID, record.ProjectID, objectRoleContent, answer)
		if err != nil {
			return err
		}
		record.ObjectPath = path
		record.ObjectBytes = int64(len(answer))
		record.ObjectContentType = "application/json"
	}
	input := record.InputObjectPath
	record.InputObjectPath = ""
	record.Status = status
	record.ErrorCode = code
	record.ErrorMessage = message
	record.CompletedAt = s.now()
	if _, err := s.saveDeferred(ctx, record); err != nil {
		return err
	}
	if input != "" {
		if err := s.removeResourceObject(input); err != nil {
			// The record no longer names it, so the hourly orphan sweep reclaims
			// it once it is old enough to be one. Failing the settlement over it
			// would be worse: the answer is already stored and the caller is
			// owed it.
			s.logger.Error("a stored deferred request could not be erased", "resource_id", record.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) saveDeferred(ctx context.Context, record domain.ProviderResource) (domain.ProviderResource, error) {
	record.UpdatedAt = s.now()
	return s.resources.PutProviderResource(ctx, record, record.Revision)
}

// DeferredResponse answers a poll.
//
// It writes no ledger events, opens no request run, starts no attempt, and calls
// no upstream. GetBatch runs its poll through the full accounting envelope and
// is right to: only the upstream knows a batch's status, so the poll is a
// request. Here the answer is already on local disk. Putting a poll loop through
// the accounting write path would fill the WAL and the usage partitions with
// events describing no work — at two seconds a poll, some eighteen hundred of
// them an hour.
func (s *Service) DeferredResponse(ctx context.Context, plaintextKey, resourceID string) (openaiapi.Response, time.Duration, error) {
	_, record, err := s.deferredOwner(ctx, plaintextKey, resourceID)
	if err != nil {
		return openaiapi.Response{}, 0, err
	}
	if !domain.DeferredTerminal(record.Status) {
		response, err := s.renderDeferred(record)
		return response, s.deferredRetryAfter(record), err
	}
	response, err := s.renderDeferred(record)
	if err != nil {
		return openaiapi.Response{}, 0, err
	}
	// The cool-off starts when the answer is first served, not when it is
	// reaped: an HTTP 200 leaving Halro is not proof the caller received it, so
	// a caller whose connection dropped mid-body gets to ask again.
	if record.RetrievedAt.IsZero() {
		record.RetrievedAt = s.now()
		// Bringing the TTL forward is what hands the cool-off to the reaper that
		// already runs, rather than adding a second cleanup loop that would have
		// to be reasoned about on its own.
		if coolOff := record.RetrievedAt.Add(deferredCoolOff); coolOff.Before(record.ExpiresAt) {
			record.ExpiresAt = coolOff
		}
		if _, err := s.saveDeferred(ctx, record); err != nil {
			s.logger.Error("the retrieval time of a deferred response could not be recorded", "resource_id", record.ID, "error", err)
		}
	}
	return response, 0, nil
}

// CancelDeferredResponse stops work the caller no longer wants.
//
// Cancelling a queued submission is determinate: nothing was sent, so the
// reservation that was never made needs no settling. Cancelling one already
// running is best-effort — the upstream call may already have happened — so the
// worker settles it conservatively under ADR 0011 and the record says plainly
// that it may have been billed.
func (s *Service) CancelDeferredResponse(ctx context.Context, plaintextKey, resourceID string) (openaiapi.Response, error) {
	_, record, err := s.deferredOwner(ctx, plaintextKey, resourceID)
	if err != nil {
		return openaiapi.Response{}, err
	}
	switch record.Status {
	case domain.DeferredQueued:
		if err := s.finishDeferred(ctx, record, domain.DeferredCancelled,
			"deferred_response_cancelled", "the request was cancelled before it reached the upstream", nil); err != nil {
			return openaiapi.Response{}, gatewayError("resource_store_unavailable", "the cancellation could not be recorded", 503, err)
		}
		record, err = s.resources.ProviderResource(ctx, record.ProjectID, record.ID)
		if err != nil {
			return openaiapi.Response{}, gatewayError("resource_store_unavailable", "the cancellation could not be read back", 503, err)
		}
		return s.renderDeferred(record)
	case domain.DeferredInProgress:
		if s.deferred != nil {
			if cancel, ok := s.deferred.running.Load(record.ID); ok {
				cancel.(context.CancelFunc)()
			}
		}
		// The worker owns the terminal write for a running request, because it
		// is the only party that knows how the attempt settled. The caller is
		// told the current state and polls for the rest.
		return s.renderDeferred(record)
	default:
		return openaiapi.Response{}, gatewayError(
			"deferred_response_not_cancellable", "the request has already finished", 409, nil,
		)
	}
}

func (s *Service) DeleteDeferredResponse(ctx context.Context, plaintextKey, resourceID string) (openaiapi.ResponseDeleted, error) {
	_, record, err := s.deferredOwner(ctx, plaintextKey, resourceID)
	if err != nil {
		return openaiapi.ResponseDeleted{}, err
	}
	if !domain.DeferredTerminal(record.Status) {
		return openaiapi.ResponseDeleted{}, gatewayError(
			"deferred_response_in_flight", "cancel the request before deleting it", 409, nil,
		)
	}
	if err := s.forgetDeferred(ctx, record); err != nil {
		return openaiapi.ResponseDeleted{}, gatewayError("resource_store_unavailable", "the response could not be deleted", 503, err)
	}
	return openaiapi.ResponseDeleted{ID: record.ID, Object: "response.deleted", Deleted: true}, nil
}

// forgetDeferred removes both objects and then the record. The objects go
// first: a record without its objects is a resource that fails a read, while an
// object without its record is a file nothing can name or reap.
func (s *Service) forgetDeferred(ctx context.Context, record domain.ProviderResource) error {
	for _, name := range []string{record.InputObjectPath, record.ObjectPath} {
		if name == "" {
			continue
		}
		if err := s.removeResourceObject(name); err != nil {
			return err
		}
	}
	return s.resources.DeleteProviderResource(ctx, record.ProjectID, record.ID)
}

func (s *Service) deferredOwner(ctx context.Context, plaintextKey, resourceID string) (auth.AuthResult, domain.ProviderResource, error) {
	principal, err := s.resourcePrincipal(ctx, plaintextKey)
	if err != nil {
		return auth.AuthResult{}, domain.ProviderResource{}, err
	}
	record, err := s.resources.ProviderResource(ctx, principal.Project.ID, resourceID)
	// A record past its expiry is gone as far as a caller is concerned, even in
	// the window before the hourly reaper removes it. Answering from it would
	// make the retention promise depend on when the reaper last ran.
	//
	// This used to apply to retrieved records alone, which left the case the
	// promise is actually about: a submission nobody ever collected reached its
	// TTL and stayed readable for up to another hour. Failure capture expires
	// record by record against the clock, and ADR 0024 gives the deferred tier
	// the same 24 hours on the stated grounds that it is the same class of
	// material — so it gets the same treatment rather than a weaker one.
	if err == nil && record.Kind == domain.ResourceDeferredResponse &&
		!record.ExpiresAt.After(s.now()) {
		return auth.AuthResult{}, domain.ProviderResource{}, gatewayError(
			"response_not_found", "response was not found", 404, nil,
		)
	}
	if err != nil || record.Kind != domain.ResourceDeferredResponse {
		// A record owned by another project and a record that never existed get
		// the same answer, so the identifier space cannot be probed.
		return auth.AuthResult{}, domain.ProviderResource{}, gatewayError(
			"response_not_found", "response was not found", 404, err,
		)
	}
	return principal, record, nil
}

// deferredRetryAfter grows with how long the caller has already waited. A
// request that has been queued for a second is worth asking about again
// shortly; one that has been running for a minute is not.
func (s *Service) deferredRetryAfter(record domain.ProviderResource) time.Duration {
	since := record.SubmittedAt
	if !record.StartedAt.IsZero() {
		since = record.StartedAt
	}
	waited := s.now().Sub(since)
	if waited < 0 {
		waited = 0
	}
	suggestion := deferredMinRetryAfter + waited/8
	return min(max(suggestion, deferredMinRetryAfter), deferredMaxRetryAfter)
}

// renderDeferred turns a record into the Response object the caller sees. A
// terminal success is the stored answer verbatim; everything else is built from
// the record, because there is no answer yet to render.
func (s *Service) renderDeferred(record domain.ProviderResource) (openaiapi.Response, error) {
	if record.Status == domain.DeferredCompleted && record.ObjectPath != "" {
		stored, err := s.readResourceObject(record)
		if err != nil {
			return openaiapi.Response{}, gatewayError(
				"resource_store_unavailable", "the stored answer could not be read", 503, err,
			)
		}
		var response openaiapi.Response
		if err := json.Unmarshal(stored, &response); err != nil {
			return openaiapi.Response{}, gatewayError(
				"resource_store_unavailable", "the stored answer could not be decoded", 503, err,
			)
		}
		return response, nil
	}
	response := openaiapi.Response{
		ID: record.ID, Object: "response", CreatedAt: record.SubmittedAt.Unix(),
		Status: record.Status, Background: true, Model: record.PublicModel,
		Output: []openaiapi.ResponseOutputItem{}, Tools: []openaiapi.ResponseTool{},
		Truncation: "disabled", Metadata: map[string]string{},
	}
	if !record.CompletedAt.IsZero() {
		completed := record.CompletedAt.Unix()
		response.CompletedAt = &completed
	}
	if record.ErrorCode != "" {
		response.Error = map[string]string{"code": record.ErrorCode, "message": record.ErrorMessage}
	}
	return response, nil
}
