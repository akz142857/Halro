package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type costAdjustmentInput struct {
	Mode                     ledger.AdjustmentMode `json:"mode"`
	ExplicitDeltaMicrosUSD   int64                 `json:"explicit_delta_micros_usd,omitempty"`
	CorrectionPrice          *createPriceInput     `json:"correction_price,omitempty"`
	ExpectedSequence         uint64                `json:"expected_sequence"`
	ExpectedNetCostMicrosUSD int64                 `json:"expected_net_cost_micros_usd"`
	ReasonCode               string                `json:"reason_code"`
	Reason                   string                `json:"reason"`
	EvidenceDigest           string                `json:"evidence_digest"`
	Confirm                  bool                  `json:"confirm,omitempty"`
	CurrentPassword          string                `json:"current_password,omitempty"`
	TOTPCode                 string                `json:"totp_code,omitempty"`
}

func (r *Runtime) previewAdminCostAdjustment(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	var input costAdjustmentInput
	if err := decodeAdminJSON(request, &input); err != nil {
		writeAdjustmentError(writer, errors.New("invalid adjustment request"))
		return
	}
	spec, err := r.adjustmentSpec(chi.URLParam(request, "attemptID"), admin.session.Username, "preview", input)
	clear([]byte(input.CurrentPassword))
	if err != nil {
		writeAdjustmentError(writer, err)
		return
	}
	settled, ok := r.state.SettledAttempt(spec.AttemptID)
	if !ok {
		writeAdjustmentError(writer, errors.New("settled attempt not found"))
		return
	}
	project, err := r.store.GetProject(request.Context(), settled.Settlement.ProjectID)
	if err != nil {
		writeAdjustmentError(writer, err)
		return
	}
	preview, err := r.accounting.PreviewAdjustment(spec, time.Now().In(r.usageLocation), project.DailyBudgetMicrosUSD)
	if err != nil {
		writeAdjustmentError(writer, err)
		return
	}
	preview.SoftLimitExceeded = adjustmentMagnitude(preview.Event.AdjustmentDeltaMicrosUSD) > r.config.Admin.AdjustmentSoftLimitMicrosUSD
	preview.HardLimitExceeded = adjustmentMagnitude(preview.Event.AdjustmentDeltaMicrosUSD) > r.config.Admin.AdjustmentHardLimitMicrosUSD
	writeJSON(writer, http.StatusOK, preview)
}

func (r *Runtime) createAdminCostAdjustment(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 256 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required", "code": "idempotency_key_required"})
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	var input costAdjustmentInput
	if err := decodeAdminJSON(request, &input); err != nil {
		writeAdjustmentError(writer, errors.New("invalid adjustment request"))
		return
	}
	if !input.Confirm {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "explicit confirmation is required", "code": "adjustment_confirmation_required"})
		return
	}
	keyDigest := adjustmentIdempotencyDigest(admin.session.Username, chi.URLParam(request, "attemptID"), idempotencyKey)
	password, totpCode := input.CurrentPassword, input.TOTPCode
	spec, err := r.adjustmentSpec(chi.URLParam(request, "attemptID"), admin.session.Username, keyDigest, input)
	clear([]byte(input.CurrentPassword))
	input.CurrentPassword, input.TOTPCode = "", ""
	if err != nil {
		writeAdjustmentError(writer, err)
		return
	}
	if prior, ok := r.state.AdjustmentByIdempotencyDigest(keyDigest); ok {
		if prior.RequestDigest != spec.RequestDigest {
			writeAdjustmentError(writer, errors.New("adjustment idempotency conflict"))
			return
		}
		// A response-loss retry must not consume the same one-time TOTP again.
		if !r.verifyPricingPassword(writer, request, admin.session.Username, password) {
			return
		}
		if err := r.drainCostAdjustmentIntents(request.Context()); err != nil {
			adminAuditError(writer)
			return
		}
		writer.Header().Set("Idempotent-Replayed", "true")
		writeJSON(writer, http.StatusCreated, map[string]any{"adjustment": prior.Event})
		return
	}
	if !r.verifyPricingReauthentication(writer, request, admin.session.Username, password, totpCode) {
		return
	}
	settled, ok := r.state.SettledAttempt(spec.AttemptID)
	if !ok {
		writeAdjustmentError(writer, errors.New("settled attempt not found"))
		return
	}
	project, err := r.store.GetProject(request.Context(), settled.Settlement.ProjectID)
	if err != nil {
		writeAdjustmentError(writer, err)
		return
	}
	auditID, err := id.New("aud")
	if err != nil {
		adminStoreError(writer)
		return
	}
	var intent boltstore.CostAdjustmentIntent
	preview, replayed, err := r.accounting.CommitAdjustmentWithIntent(request.Context(), spec, project.DailyBudgetMicrosUSD,
		r.config.Admin.AdjustmentDailyHardLimitMicrosUSD, func(event ledger.Event) error {
			if adjustmentMagnitude(event.AdjustmentDeltaMicrosUSD) > r.config.Admin.AdjustmentHardLimitMicrosUSD {
				return budget.ErrAdjustmentHardLimitExceeded
			}
			intent = boltstore.CostAdjustmentIntent{LedgerEvent: event, AuditEventID: auditID, ActorID: admin.session.Username,
				CorrelationID: request.Header.Get("X-Request-ID"), CreatedAt: time.Now().UTC()}
			return r.store.PutCostAdjustmentIntent(request.Context(), intent)
		})
	if err != nil {
		if errors.Is(err, budget.ErrAdjustmentHardLimitExceeded) {
			writeJSON(writer, http.StatusUnprocessableEntity, map[string]string{"error": "adjustment exceeds the online hard limit; use the controlled offline workflow", "code": "adjustment_hard_limit_exceeded"})
			return
		}
		writeAdjustmentError(writer, err)
		return
	}
	if replayed {
		if err := r.drainCostAdjustmentIntents(request.Context()); err != nil {
			adminAuditError(writer)
			return
		}
		writer.Header().Set("Idempotent-Replayed", "true")
		writeJSON(writer, http.StatusCreated, map[string]any{"adjustment": preview.Event})
		return
	}
	if err := r.deliverCostAdjustmentIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"adjustment": preview.Event, "budget_overage_micros_usd": preview.BudgetOverageMicrosUSD})
}

func (r *Runtime) adjustmentSpec(attemptID, actor, keyDigest string, input costAdjustmentInput) (budget.AdjustmentSpec, error) {
	input.ReasonCode, input.Reason, input.EvidenceDigest = strings.TrimSpace(input.ReasonCode), strings.TrimSpace(input.Reason), strings.ToLower(strings.TrimSpace(input.EvidenceDigest))
	input.CurrentPassword, input.TOTPCode = "", ""
	requestPayload, err := json.Marshal(input)
	if err != nil {
		return budget.AdjustmentSpec{}, err
	}
	digest := sha256.Sum256(requestPayload)
	if keyDigest == "preview" {
		keyDigest = "sha256:" + strings.Repeat("0", 64)
	}
	spec := budget.AdjustmentSpec{AttemptID: attemptID, Mode: input.Mode, ExplicitDeltaMicrosUSD: input.ExplicitDeltaMicrosUSD,
		ExpectedSequence: input.ExpectedSequence, ExpectedNetCostMicrosUSD: input.ExpectedNetCostMicrosUSD,
		IdempotencyKeyDigest: keyDigest, RequestDigest: "sha256:" + hex.EncodeToString(digest[:]), ReasonCode: input.ReasonCode,
		Reason: input.Reason, EvidenceDigest: input.EvidenceDigest, CreatedBy: actor}
	if input.Mode == ledger.AdjustmentModeReprice {
		if input.CorrectionPrice == nil {
			return budget.AdjustmentSpec{}, errors.New("correction price is required")
		}
		now := time.Now().UTC()
		price, err := priceVersionFromInput("correction_"+attemptID, "correction_"+attemptID, actor, now, *input.CorrectionPrice)
		if err != nil {
			return budget.AdjustmentSpec{}, err
		}
		price.Version, price.Revision = 1, 1
		snapshot, err := domain.NewVersionedPriceSnapshot(price, now)
		if err != nil {
			return budget.AdjustmentSpec{}, err
		}
		spec.CorrectionPriceSnapshot = &snapshot
	}
	return spec, nil
}

func adjustmentIdempotencyDigest(actor, attemptID, key string) string {
	hash := sha256.New()
	hash.Write([]byte("heimdall:cost-adjustment-idempotency:v1\x00"))
	hash.Write([]byte(actor))
	hash.Write([]byte{0})
	hash.Write([]byte(attemptID))
	hash.Write([]byte{0})
	hash.Write([]byte(key))
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func adjustmentMagnitude(delta int64) int64 {
	if delta == -1<<63 {
		return 1<<63 - 1
	}
	if delta < 0 {
		return -delta
	}
	return delta
}

func (r *Runtime) deliverCostAdjustmentIntent(ctx context.Context, intent boltstore.CostAdjustmentIntent) error {
	if _, err := r.audit.Append(ctx, audit.Event{EventID: intent.AuditEventID, OccurredAt: intent.CreatedAt, ActorType: "admin_user", ActorID: intent.ActorID,
		Action: "cost_adjustment.create", TargetType: "usage_attempt", TargetID: intent.LedgerEvent.AttemptID, Outcome: "success", CorrelationID: intent.CorrelationID}); err != nil {
		return err
	}
	if err := checkpointAudit(r.store, r.audit.Summary()); err != nil {
		return err
	}
	return r.store.MarkCostAdjustmentIntentDelivered(ctx, intent.LedgerEvent.EventID)
}

func (r *Runtime) drainCostAdjustmentIntents(ctx context.Context) error {
	intents, err := r.store.ListPendingCostAdjustmentIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if _, err := r.accounting.CommitPreparedAdjustment(ctx, intent.LedgerEvent); err != nil {
			// Old versions could persist a conflicting sequence before acquiring
			// the accounting lock. Quarantine that terminal intent so startup can
			// continue, while preserving an explicit failed Audit record.
			if strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "sequence") {
				if _, auditErr := r.audit.Append(ctx, audit.Event{EventID: intent.AuditEventID, OccurredAt: time.Now().UTC(), ActorType: "admin_user", ActorID: intent.ActorID,
					Action: "cost_adjustment.create", TargetType: "usage_attempt", TargetID: intent.LedgerEvent.AttemptID, Outcome: "failure", CorrelationID: intent.CorrelationID}); auditErr != nil {
					return auditErr
				}
				if err := checkpointAudit(r.store, r.audit.Summary()); err != nil {
					return err
				}
				if err := r.store.MarkCostAdjustmentIntentRejected(ctx, intent.LedgerEvent.EventID, err.Error()); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err := r.deliverCostAdjustmentIntent(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

func writeAdjustmentError(writer http.ResponseWriter, err error) {
	status, code := http.StatusUnprocessableEntity, "adjustment_evidence_required"
	switch {
	case errors.Is(err, boltstore.ErrNotFound):
		status, code = http.StatusNotFound, "usage_attempt_not_found"
	case errors.Is(err, boltstore.ErrIdempotencyConflict), strings.Contains(err.Error(), "conflict"), strings.Contains(err.Error(), "sequence"):
		status, code = http.StatusConflict, "adjustment_conflict"
	case strings.Contains(err.Error(), "not found"):
		status, code = http.StatusNotFound, "usage_attempt_not_found"
	case strings.Contains(err.Error(), "negative"), strings.Contains(err.Error(), "overflow"), strings.Contains(err.Error(), "invalid adjustment mode"):
		status, code = http.StatusBadRequest, "invalid_adjustment"
	}
	writeJSON(writer, status, map[string]string{"error": err.Error(), "code": code})
}
