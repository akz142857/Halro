package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/idempotency"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
)

// inferenceIdempotencyTTL is how long a key is remembered.
//
// It bounds what a retry can reach rather than what a caller can read: nothing
// here is readable. A day is longer than any client's retry budget and short
// enough that a key an operator reuses next week is not refused by a record
// nobody remembers making.
const inferenceIdempotencyTTL = 24 * time.Hour

// inferenceIdempotency is one synchronous request's claim on its key, or the
// zero value for a caller who sent none.
type inferenceIdempotency struct {
	service          *Service
	record           domain.ProviderResource
	held             bool
	retryableRefused bool
}

const inferenceRetryPending = "retry_pending"

// inferenceClaimKey carries the live claim from admission to the one point that
// can say the upstream was reached.
//
// It travels in the context for the same reason the key itself does: the
// alternative is a parameter on executeGenerate, which the deferred worker also
// calls, and a nil passed there would be a path that silently spends nobody's
// key. A worker context holds no claim, so the mark is a no-op there by
// construction rather than by remembering to pass nil.
type inferenceClaimKey struct{}

func withInferenceClaim(ctx context.Context, claim *inferenceIdempotency) context.Context {
	if claim == nil || !claim.held {
		return ctx
	}
	return context.WithValue(ctx, inferenceClaimKey{}, claim)
}

// noteInferenceDispatch records that this request is about to reach the
// upstream, so an interruption from here on is remembered as ambiguous rather
// than as a reservation a retry may take over.
//
// Called immediately before the Provider call and nowhere else: everything
// earlier — capability filtering, redaction, token limits, the Project budget,
// the per-attempt reservation — refuses the request without the upstream ever
// hearing of it, and a key those refusals had spent would answer the caller's
// legitimate retry with a conflict for a day.
//
// Failing to record it fails the request. Dispatching without the mark would
// leave a crash indistinguishable from a reservation, which is the one
// ambiguity this state exists to remove.
func noteInferenceDispatch(ctx context.Context) error {
	claim, ok := ctx.Value(inferenceClaimKey{}).(*inferenceIdempotency)
	if !ok || !claim.held || claim.record.CreationStatus == creationInFlight {
		return nil
	}
	inflight, err := claim.service.markInFlight(ctx, claim.record)
	if err != nil {
		return err
	}
	claim.record = inflight
	return nil
}

// admitInferenceIdempotency resolves the caller's Idempotency-Key against what
// the key has already been used for, and reserves it when the request may
// proceed.
//
// The guarantee is at-most-once upstream execution, and only that. A completed
// key is refused rather than replayed, because replaying would mean storing the
// answer, and this gateway keeps caller content in exactly two places — both
// because the caller asked it to. A generation nobody asked to be stored is not
// going to become the third.
//
// A caller who sends no key gets the behaviour they had before: nothing is
// written, nothing is checked, and the request costs no durable state.
func (s *Service) admitInferenceIdempotency(
	ctx context.Context,
	principal auth.AuthResult,
	targets []provider.Target,
	publicModel string,
	payload any,
) (context.Context, *inferenceIdempotency, error) {
	key, sent := requestmeta.IdempotencyKey(ctx)
	if !sent {
		return ctx, &inferenceIdempotency{}, nil
	}
	if s.resources == nil {
		// Idempotency is a durable promise. An instance with nowhere to write
		// it must refuse the header rather than accept it and keep none of it.
		return ctx, nil, gatewayError("idempotency_unavailable", "this instance cannot record idempotency keys", 503, nil)
	}
	if err := idempotency.ValidateKey(key); err != nil {
		return ctx, nil, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
	}
	if len(targets) == 0 {
		return ctx, nil, gatewayError("internal_error", "idempotency requires a resolved route", 500, nil)
	}
	fingerprint, err := inferenceFingerprint(publicModel, payload)
	if err != nil {
		return ctx, nil, gatewayError("internal_error", "unable to fingerprint the request", 500, err)
	}
	keyHash := sha256.Sum256([]byte(key))
	existing, verdict, err := s.classifyIdempotency(
		ctx, principal.Project.ID, domain.ResourceInferenceCall, keyHash, fingerprint,
	)
	if err != nil {
		return ctx, nil, err
	}
	if repeat := repeatedKeyError(verdict); repeat != nil {
		return ctx, nil, repeat
	}
	recordID := existing.ID
	expectedRevision := existing.Revision
	if verdict != idempotencyReclaim {
		recordID, err = id.New("idm")
		if err != nil {
			return ctx, nil, gatewayError("internal_error", "unable to create idempotency record ID", 500, err)
		}
		expectedRevision = 0
	}
	now := s.now()
	target := targets[0]
	record := domain.ProviderResource{
		ID: recordID, Kind: domain.ResourceInferenceCall, ProjectID: principal.Project.ID,
		ProviderID: target.ProviderID, DeploymentID: target.DeploymentID,
		PublicModel: publicModel, ProfileID: target.ProfileID, Region: target.Region,
		KeyID: principal.Key.ID, IdempotencyKeyHash: keyHash, RequestFingerprint: fingerprint,
		CreationStatus: creationReserved, ReservedBy: s.instanceID, Status: "pending",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(inferenceIdempotencyTTL),
	}
	// A reclaim keeps the record's identity but never its old expiry: the key
	// has to outlive the request it is about to protect. Inheriting a deadline
	// seconds away would free the key while the reclaimed call is still
	// running, and the caller's timeout retry would then execute a second time.
	if verdict == idempotencyReclaim && existing.ExpiresAt.After(now) {
		record.CreatedAt = existing.CreatedAt
		if existing.CreationStatus == creationRetryableRefused || existing.Status == inferenceRetryPending {
			record.Status = inferenceRetryPending
		}
	}
	stored, err := s.resources.PutProviderResource(ctx, record, expectedRevision)
	if err != nil {
		return ctx, nil, s.reservationRefusal(ctx, principal.Project.ID, keyHash, fingerprint, err)
	}
	// Reserved, not in flight: nothing has been dispatched yet, and the
	// difference is what lets a request this gateway refuses on its own — a
	// budget, a token limit — give the key back instead of burning it. A
	// concurrent duplicate is still refused, because a reservation this
	// instance holds reads as in progress, and a reservation a dead process
	// held reads as reclaimable.
	claim := &inferenceIdempotency{service: s, record: stored, held: true}
	return withInferenceClaim(ctx, claim), claim, nil
}

// inferenceRepeatRefusal answers a keyed request that is being refused before
// its key was reserved, when the key already says more than the refusal does.
//
// It reserves nothing and refuses nothing on its own account: a fresh key, a
// reclaimable one, an invalid one, or a store that cannot answer all leave the
// original refusal standing, which never reached the upstream either way.
func (s *Service) inferenceRepeatRefusal(
	ctx context.Context, principal auth.AuthResult, publicModel string, payload any,
) error {
	key, sent := requestmeta.IdempotencyKey(ctx)
	if !sent || s.resources == nil || idempotency.ValidateKey(key) != nil {
		return nil
	}
	// The lookup comes before the fingerprint. A fast refusal — a suspended
	// route answering a 429 storm — mostly carries keys with no live record, and
	// those must not pay a marshal of the whole request to learn nothing.
	keyHash := sha256.Sum256([]byte(key))
	existing, found, err := s.resources.ProviderResourceByIdempotency(ctx, principal.Project.ID, domain.ResourceInferenceCall, keyHash)
	if err != nil || !found || !existing.ExpiresAt.After(s.now()) {
		return nil
	}
	fingerprint, err := inferenceFingerprint(publicModel, payload)
	if err != nil {
		return nil
	}
	verdict, err := s.classifyExisting(domain.ResourceInferenceCall, existing, fingerprint)
	var conflict *Error
	switch {
	case errors.As(err, &conflict) && conflict.Code == "idempotency_conflict":
		return err
	case err != nil:
		return nil
	default:
		return repeatedKeyError(verdict)
	}
}

// repeatedKeyError is the answer to a key whose earlier use decides this
// request, or nil when it does not.
func repeatedKeyError(verdict idempotencyVerdict) error {
	switch verdict {
	case idempotencyCompleted:
		// The answer went to the first caller and was not kept. Saying that is
		// the honest refusal; inventing a replay would need a store this
		// deliberately does not have.
		return gatewayError(
			"idempotency_completed",
			"this idempotency key has already been used; its answer was delivered once and is not stored",
			409, nil,
		)
	case idempotencyInProgress:
		return gatewayError(
			"idempotency_in_progress",
			"a request with this idempotency key is still running",
			409, nil,
		)
	case idempotencyOutcomeUnknown:
		return outcomeUnknownError()
	}
	return nil
}

// outcomeUnknownError is the answer for a key whose request may have been
// served and whose outcome is lost. It is told apart from in progress because
// the two call for opposite responses: a running request is worth waiting for,
// and this one will read the same for the rest of the key's lifetime. Whether
// to give up or to retry under a new key — accepting that the upstream may
// already have done the work — is the caller's decision, not the gateway's.
func outcomeUnknownError() *Error {
	return gatewayError(
		"idempotency_outcome_unknown",
		"a request with this idempotency key may have reached the provider and its outcome is unknown; retrying the key will not change this",
		409, nil,
	)
}

// reservationRefusal says which of the two very different things went wrong
// when the reservation could not be written.
//
// A failed reclaim can leave the original record intact. Its presence alone
// does not prove another request took the key. Classify the latest state just
// as admission does so a competing claim gets the appropriate repeat error.
func (s *Service) reservationRefusal(ctx context.Context, projectID string, keyHash, fingerprint [32]byte, cause error) error {
	_, verdict, lookupErr := s.classifyIdempotency(ctx, projectID, domain.ResourceInferenceCall, keyHash, fingerprint)
	var conflict *Error
	if errors.As(lookupErr, &conflict) && conflict.Code == "idempotency_conflict" {
		return lookupErr
	}
	if lookupErr == nil {
		if repeat := repeatedKeyError(verdict); repeat != nil {
			return repeat
		}
	}
	return gatewayError("resource_store_unavailable", "the idempotency key could not be recorded", 503, cause)
}

// settleInto settles the claim on the way out of a request and lets a refusal
// that could not be recorded replace the request's own result: the caller must
// hear that the key is still protected rather than that it may retry.
func (claim *inferenceIdempotency) settleInto(ctx context.Context, result *error) {
	if err := claim.settle(ctx, *result); err != nil {
		*result = err
	}
}

// settle closes the record once the request has an outcome, whatever it was.
//
// A success completes the key. Failures leave it unknown unless every dispatch
// was explicitly refused and its accounting closed. That refusal preserves the
// fingerprint while allowing a same-body retry to claim the key by revision.
func (claim *inferenceIdempotency) settle(ctx context.Context, err error) error {
	if claim == nil || !claim.held {
		return nil
	}
	claim.held = false
	record := claim.record
	restoreRefusal := record.CreationStatus == creationReserved && record.Status == inferenceRetryPending
	if record.CreationStatus == creationReserved && !restoreRefusal {
		if delErr := claim.service.resources.DeleteProviderResource(ctx, record.ProjectID, record.ID); delErr != nil {
			// The reservation stays, which refuses this key until it expires
			// rather than for good. Conservative in the direction that costs a
			// caller a retry window instead of a duplicate bill.
			claim.service.logger.Error("an unused idempotency reservation could not be released",
				"resource_id", record.ID, "error", delErr)
		}
		return nil
	}
	record.CreationStatus = creationCompleted
	record.Status = "completed"
	if err != nil {
		record.CreationStatus = creationUnknown
		record.Status = "failed"
	}
	if claim.retryableRefused || restoreRefusal {
		record.CreationStatus = creationRetryableRefused
		record.Status = "failed"
	}
	record.UpdatedAt = claim.service.now()
	if _, putErr := claim.service.resources.PutProviderResource(ctx, record, record.Revision); putErr != nil {
		// The record stays in flight, which refuses a retry rather than
		// admitting one. Losing the write in the other direction would be the
		// second billed call this exists to prevent.
		claim.service.logger.Error("an idempotency record could not be closed",
			"resource_id", record.ID, "error", putErr)
		if record.CreationStatus == creationRetryableRefused {
			return gatewayError("resource_store_unavailable", "the retryable refusal could not be recorded", 503, putErr)
		}
	}
	return nil
}

// inferenceFingerprint identifies the request a key was used for, so the same
// key with a different body is refused instead of being served as a repeat.
//
// It is a digest and nothing else reaches the record: the request is hashed,
// never stored.
func inferenceFingerprint(publicModel string, payload any) ([32]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte(publicModel+"\x00"), encoded...)), nil
}
