package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
)

// The admin mutation protocol
//
// A mutation used to be three independent outcomes — store commit, runtime
// activation, audit append — and the HTTP status could only report the last
// failure. A change could be durable while its audit record was never written,
// and the caller could not tell whether the failed request had taken effect.
//
// The store commit is now the single commit point. The audit record is written
// into the same transaction as the mutation, so it exists as soon as the
// mutation does; delivery to the audit log happens afterwards and is retried by
// a drain if it fails. What the operator sees is therefore either "the mutation
// did not happen" or "the mutation happened and its record is in the log, or on
// its way there" — never "it happened and nothing recorded it".
//
// The intent is built before the mutation because its event ID has to be
// durable with the record it describes. Building it afterwards would reopen the
// window this closes.

// newAdminAuditIntent builds the durable audit record for an admin mutation.
//
// It reads the actor from the admin session rather than from the request body:
// the audit trail must name who the server authenticated, not who the request
// claims to be.
func (r *Runtime) newAdminAuditIntent(request *http.Request, action, targetType, targetID string) (*domain.AdminAuditIntent, error) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	eventID, err := id.New("aud")
	if err != nil {
		return nil, err
	}
	intent := &domain.AdminAuditIntent{
		EventID:       eventID,
		OccurredAt:    time.Now().UTC(),
		ActorType:     "admin_user",
		ActorID:       admin.session.Username,
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		CorrelationID: strings.TrimSpace(request.Header.Get("X-Request-ID")),
	}
	if err := intent.Validate(); err != nil {
		return nil, err
	}
	return intent, nil
}

// completeAdminMutation finishes a mutation whose store commit already
// succeeded.
//
// The operation ID is the audit event ID, which is durable with the mutation:
// it is what an operator whose connection dropped can look the change up by,
// instead of guessing from a revision whether a failed-looking request took
// effect. The activation header says whether the change is in force yet — the
// commit protocol makes those two different questions, so the answer has to be
// on the response rather than inferred from its status.
//
// A delivery failure does not fail the response. The record is already durable;
// the drain will deliver it.
func (r *Runtime) completeAdminMutation(writer http.ResponseWriter, request *http.Request, intent domain.AdminAuditIntent) {
	writer.Header().Set("Halro-Operation-Id", intent.EventID)
	if r.activation.status().Stale {
		writer.Header().Set("Halro-Activation", "stale")
	} else {
		writer.Header().Set("Halro-Activation", "current")
	}
	// The commit has happened. Delivery belongs to the runtime now: a caller
	// disconnecting after the commit must not cancel the durable record's trip
	// to the audit log.
	deliveryContext, cancel := r.activationContext()
	defer cancel()
	if err := r.deliverAdminAuditIntent(deliveryContext, intent); err != nil {
		r.logger.Error("admin audit record is durable but not yet delivered",
			"event_id", intent.EventID, "error", err)
	}
}

// deliverAdminAuditIntent appends a durable intent to the audit log and retires
// it. The intent is removed only after the append and the checkpoint are both
// durable, so an interrupted delivery is retried rather than lost.
func (r *Runtime) deliverAdminAuditIntent(ctx context.Context, intent domain.AdminAuditIntent) error {
	metadata := make(map[string]any, len(intent.Metadata))
	for key, value := range intent.Metadata {
		metadata[key] = value
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	if _, err := r.audit.Append(ctx, audit.Event{
		EventID: intent.EventID, OccurredAt: intent.OccurredAt, ActorType: intent.ActorType,
		ActorID: intent.ActorID, Action: intent.Action, TargetType: intent.TargetType,
		TargetID: intent.TargetID, Outcome: "success", CorrelationID: intent.CorrelationID,
		Metadata: metadata,
	}); err != nil {
		return err
	}
	if err := checkpointAudit(r.store, r.audit.Summary()); err != nil {
		return err
	}
	return r.store.DeleteAdminAuditIntent(ctx, intent.EventID)
}

// drainAdminAuditIntents delivers every admin mutation whose record is durable
// but not yet in the audit log.
//
// It runs at startup, where it answers what a crash between the commit and the
// append left behind, and after a delivery failure. Delivery is ordered by
// occurrence so the log reproduces the order the mutations happened in.
func (r *Runtime) drainAdminAuditIntents(ctx context.Context) error {
	intents, err := r.store.ListPendingAdminAuditIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := r.deliverAdminAuditIntent(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

// recoverAdminAuditIntents is the bounded, runtime-owned retry used after
// startup. A delivery error does not make activation stale, so this check must
// run independently of snapshot recovery.
func (r *Runtime) recoverAdminAuditIntents() error {
	pending, err := r.store.PendingAdminAuditIntentCount(r.backgroundCtx)
	if err != nil || pending == 0 {
		return err
	}
	ctx, cancel := r.activationContext()
	defer cancel()
	return r.drainAdminAuditIntents(ctx)
}
