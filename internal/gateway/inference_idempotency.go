package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	service *Service
	record  domain.ProviderResource
	held    bool
}

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
	switch verdict {
	case idempotencyCompleted:
		// The answer went to the first caller and was not kept. Saying that is
		// the honest refusal; inventing a replay would need a store this
		// deliberately does not have.
		return ctx, nil, gatewayError(
			"idempotency_completed",
			"this idempotency key has already been used; its answer was delivered once and is not stored",
			409, nil,
		)
	case idempotencyInProgress:
		return ctx, nil, gatewayError(
			"idempotency_in_progress",
			"a request with this idempotency key is still running or its outcome is unknown",
			409, nil,
		)
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
	stored, err := s.resources.PutProviderResource(ctx, record, expectedRevision)
	if err != nil {
		return ctx, nil, s.reservationRefusal(ctx, principal.Project.ID, keyHash, err)
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

// reservationRefusal says which of the two very different things went wrong
// when the reservation could not be written.
//
// A lost race and an unavailable store both surface as one error from the
// store, and answering both as "just taken by another request" tells an
// operator staring at a 409 storm that their callers are colliding when the
// truth may be that bbolt cannot write. The observed state decides it: if the
// key is held now, it was a race; if nothing holds it, the write failed for its
// own reasons and the caller may retry.
func (s *Service) reservationRefusal(ctx context.Context, projectID string, keyHash [32]byte, cause error) error {
	_, found, lookupErr := s.resources.ProviderResourceByIdempotency(ctx, projectID, domain.ResourceInferenceCall, keyHash)
	if lookupErr == nil && found {
		return gatewayError("idempotency_in_progress", "this idempotency key was just taken by another request", 409, cause)
	}
	return gatewayError("resource_store_unavailable", "the idempotency key could not be recorded", 503, cause)
}

// settle closes the record once the request has an outcome, whatever it was.
//
// A request that reached the upstream spends its key either way, and spends it
// differently. A success is completed: the caller got their answer, and a
// repeat is told the key is used. A failure is unknown, which is the existing
// vocabulary for "the upstream may have served this and nobody can say" — the
// classifier reads it as still in progress, and refusing a retry is the
// conservative direction when the alternative is billing the caller twice for a
// call one of them cannot see. Either way the record stays until it expires.
//
// A request that never reached the upstream is the other case, and it gives the
// key back. Nothing was billed and nothing is ambiguous, so the record goes
// rather than answering the caller's own retry with a conflict.
func (claim *inferenceIdempotency) settle(ctx context.Context, err error) {
	if claim == nil || !claim.held {
		return
	}
	claim.held = false
	record := claim.record
	if record.CreationStatus == creationReserved {
		if delErr := claim.service.resources.DeleteProviderResource(ctx, record.ProjectID, record.ID); delErr != nil {
			// The reservation stays, which refuses this key until it expires
			// rather than for good. Conservative in the direction that costs a
			// caller a retry window instead of a duplicate bill.
			claim.service.logger.Error("an unused idempotency reservation could not be released",
				"resource_id", record.ID, "error", delErr)
		}
		return
	}
	record.CreationStatus = creationCompleted
	record.Status = "completed"
	if err != nil {
		record.CreationStatus = creationUnknown
		record.Status = "failed"
	}
	record.UpdatedAt = claim.service.now()
	if _, putErr := claim.service.resources.PutProviderResource(ctx, record, record.Revision); putErr != nil {
		// The record stays in flight, which refuses a retry rather than
		// admitting one. Losing the write in the other direction would be the
		// second billed call this exists to prevent.
		claim.service.logger.Error("an idempotency record could not be closed",
			"resource_id", record.ID, "error", putErr)
	}
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
