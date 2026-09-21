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
) (*inferenceIdempotency, error) {
	key, sent := requestmeta.IdempotencyKey(ctx)
	if !sent {
		return &inferenceIdempotency{}, nil
	}
	if s.resources == nil {
		// Idempotency is a durable promise. An instance with nowhere to write
		// it must refuse the header rather than accept it and keep none of it.
		return nil, gatewayError("idempotency_unavailable", "this instance cannot record idempotency keys", 503, nil)
	}
	if err := idempotency.ValidateKey(key); err != nil {
		return nil, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
	}
	if len(targets) == 0 {
		return nil, gatewayError("internal_error", "idempotency requires a resolved route", 500, nil)
	}
	fingerprint, err := inferenceFingerprint(publicModel, payload)
	if err != nil {
		return nil, gatewayError("internal_error", "unable to fingerprint the request", 500, err)
	}
	keyHash := sha256.Sum256([]byte(key))
	existing, verdict, err := s.classifyIdempotency(
		ctx, principal.Project.ID, domain.ResourceInferenceCall, keyHash, fingerprint,
	)
	if err != nil {
		return nil, err
	}
	switch verdict {
	case idempotencyCompleted:
		// The answer went to the first caller and was not kept. Saying that is
		// the honest refusal; inventing a replay would need a store this
		// deliberately does not have.
		return nil, gatewayError(
			"idempotency_completed",
			"this idempotency key has already been used; its answer was delivered once and is not stored",
			409, nil,
		)
	case idempotencyInProgress:
		return nil, gatewayError(
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
			return nil, gatewayError("internal_error", "unable to create idempotency record ID", 500, err)
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
		// Another request took the same key between the classification and the
		// write. Both cannot proceed, and the one that lost says so.
		return nil, gatewayError("idempotency_in_progress", "this idempotency key was just taken by another request", 409, err)
	}
	// In flight before anything is dispatched: from here on an interruption
	// means the upstream may have served it, so the key must not be reclaimed
	// by a retry. That is the whole protection — the conservative direction is
	// refusing a caller who would otherwise have been billed twice.
	inflight, err := s.markInFlight(ctx, stored)
	if err != nil {
		return nil, err
	}
	return &inferenceIdempotency{service: s, record: inflight, held: true}, nil
}

// settle closes the record once the request has an outcome, whatever it was.
//
// Both outcomes spend the key, and they spend it differently. A success is
// completed: the caller got their answer, and a repeat is told the key is used.
// A failure is unknown, which is the existing vocabulary for "the upstream may
// have served this and nobody can say" — the classifier already reads it as
// still in progress, and refusing a retry is the conservative direction when
// the alternative is billing the caller twice for a call one of them cannot
// see. A record is never deleted here: a key that has been used stays used
// until it expires.
func (claim *inferenceIdempotency) settle(ctx context.Context, err error) {
	if claim == nil || !claim.held {
		return
	}
	claim.held = false
	record := claim.record
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
