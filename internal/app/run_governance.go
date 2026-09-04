package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/idempotency"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/go-chi/chi/v5"
)

const governanceBodyLimit = int64(16 << 10)

func withRunAttribution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if runID := strings.TrimSpace(request.Header.Get("X-Halro-Run-ID")); runID != "" {
			request = request.WithContext(requestmeta.WithRunID(request.Context(), runID))
		}
		next.ServeHTTP(writer, request)
	})
}

type createWorkUnitInput struct {
	OutcomeDefinitionIDs []string `json:"outcome_definition_ids,omitempty"`
}

type createRunInput struct {
	WorkUnitID      string `json:"work_unit_id"`
	BudgetMicrosUSD int64  `json:"budget_micros_usd,omitempty"`
	TTLSeconds      int64  `json:"ttl_seconds,omitempty"`
}

type closeRunInput struct {
	Reason string `json:"reason,omitempty"`
}

type closeWorkUnitInput struct{}

type reportOutcomeInput struct {
	DefinitionID        string    `json:"definition_id"`
	Value               string    `json:"value"`
	ObservedAt          time.Time `json:"observed_at"`
	EvidenceRef         string    `json:"evidence_ref,omitempty"`
	EvidenceSHA256      string    `json:"evidence_sha256,omitempty"`
	SupersedesOutcomeID string    `json:"supersedes_outcome_id,omitempty"`
}

func (r *Runtime) withGovernanceRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID, err := id.New("req")
		if err != nil {
			writeGovernanceError(writer, http.StatusInternalServerError, "internal_error", "unable to create request ID")
			return
		}
		writer.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(writer, request.WithContext(requestmeta.WithRequestID(request.Context(), requestID)))
	})
}

func (r *Runtime) governancePrincipal(writer http.ResponseWriter, request *http.Request, scope domain.GatewayScope) (auth.AuthResult, bool) {
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	scheme, plaintext, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(plaintext) == "" {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeGovernanceError(writer, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		return auth.AuthResult{}, false
	}
	principal, err := r.gatewayService.AuthorizeGovernance(request.Context(), strings.TrimSpace(plaintext), scope)
	if err != nil {
		writeGovernanceGatewayError(writer, err)
		return auth.AuthResult{}, false
	}
	write := scope != domain.GatewayScopeGovernanceRead
	keyLimit, projectLimit := governanceReadKeyRPM, governanceReadProjectRPM
	if write {
		keyLimit, projectLimit = governanceWriteKeyRPM, governanceWriteProjectRPM
	}
	allowed, retryAfter := r.governance.rate.allow(r.clockNow(), principal.Key.ID, principal.Project.ID, write, keyLimit, projectLimit)
	if !allowed {
		writer.Header().Set("Retry-After", retryAfter)
		writeGovernanceError(writer, http.StatusTooManyRequests, "governance_rate_limited", "governance request rate exceeded")
		return auth.AuthResult{}, false
	}
	return principal, true
}

func governanceIntent(request *http.Request, operation string, canonical any) (budget.GovernanceIntent, error) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if err := idempotency.ValidateKey(key); err != nil {
		return budget.GovernanceIntent{}, err
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return budget.GovernanceIntent{}, err
	}
	keyDigest := sha256.Sum256([]byte(key))
	fingerprint := sha256.Sum256(append([]byte(operation+"\x00"), payload...))
	return budget.GovernanceIntent{
		Operation:          operation,
		IdempotencyKeyHash: "sha256:" + hex.EncodeToString(keyDigest[:]),
		RequestFingerprint: "sha256:" + hex.EncodeToString(fingerprint[:]),
	}, nil
}

func decodeGovernanceJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeGovernanceError(writer, http.StatusUnsupportedMediaType, "unsupported_content_type", "Content-Type must be application/json")
		return false
	}
	if err := decodeAdminJSONLimit(request, value, governanceBodyLimit); err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_request", "invalid request body")
		return false
	}
	return true
}

func (r *Runtime) createWorkUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeWorkUnitCreate)
	if !ok {
		return
	}
	var input createWorkUnitInput
	if !decodeGovernanceJSON(writer, request, &input) {
		return
	}
	if len(input.OutcomeDefinitionIDs) > domain.MaxDefinitionsPerWorkUnit {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome_definitions", "too many outcome definitions")
		return
	}
	seen := map[string]struct{}{}
	definitions := make([]domain.OutcomeDefinitionRef, 0, len(input.OutcomeDefinitionIDs))
	for _, definitionID := range input.OutcomeDefinitionIDs {
		if !domain.ValidOutcomeDefinitionID(definitionID) {
			writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome_definitions", "outcome definition is invalid")
			return
		}
		if _, exists := seen[definitionID]; exists {
			writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome_definitions", "outcome definitions must be unique")
			return
		}
		seen[definitionID] = struct{}{}
		definition, err := r.store.GetOutcomeDefinition(request.Context(), principal.Project.ID, definitionID, 0)
		if err != nil || !definition.Enabled {
			writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome_definitions", "outcome definition is not enabled")
			return
		}
		definitions = append(definitions, domain.OutcomeDefinitionRef{ID: definition.ID, Version: definition.Version})
	}
	intent, err := governanceIntent(request, "work_unit.create", input)
	if err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	workUnit, replay, err := r.accounting.CreateWorkUnitWithDefinitions(request.Context(), principal.Project.ID, principal.Key.ID, principal.Project.RunGovernance.MaxOpenWorkUnits, definitions, intent)
	if err != nil {
		writeGovernanceOperationError(writer, err, "work_unit")
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, workUnit)
}

func (r *Runtime) getWorkUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeGovernanceRead)
	if !ok {
		return
	}
	workUnitID := chi.URLParam(request, "workUnitID")
	if !domain.ValidWorkUnitID(workUnitID) {
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
		return
	}
	accounting, err := r.state.GovernanceSnapshot(request.Context(), ledger.GovernanceSnapshotOptions{
		ProjectID: principal.Project.ID, WorkUnitID: workUnitID, IncludeRuns: true,
	})
	if err != nil {
		writeGovernanceError(writer, http.StatusServiceUnavailable, "run_governance_unavailable", "run governance state is unavailable")
		return
	}
	if len(accounting.WorkUnits) != 1 {
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
		return
	}
	workUnit := accounting.WorkUnits[0]
	runs := accounting.Runs
	for index := range runs {
		runs[index].Status = domain.EffectiveRunStatus(runs[index], r.clockNow())
	}
	outcomeSnapshot, err := r.governance.manager.ReadOutcomeSnapshot(request.Context(), principal.Project.ID, workUnit.ID, "")
	if err != nil {
		writeOutcomeError(writer, err)
		return
	}
	outcomes := outcomeSnapshot.Outcomes
	for index := range outcomes {
		outcomes[index].Provisional = workUnit.Status == domain.WorkUnitOpen || accounting.InflightWorkUnit[workUnit.ID]
	}
	writeJSON(writer, http.StatusOK, map[string]any{"work_unit": workUnit, "runs": runs, "outcomes": outcomes})
}

func (r *Runtime) reportOutcome(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeOutcomeWrite)
	if !ok {
		return
	}
	workUnitID := chi.URLParam(request, "workUnitID")
	if !domain.ValidWorkUnitID(workUnitID) {
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
		return
	}
	var input reportOutcomeInput
	if !decodeGovernanceJSON(writer, request, &input) {
		return
	}
	intent, err := governanceIntent(request, "outcome.report", struct {
		WorkUnitID string             `json:"work_unit_id"`
		Outcome    reportOutcomeInput `json:"outcome"`
	}{WorkUnitID: workUnitID, Outcome: input})
	if err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	outcome, replay, err := r.governance.manager.Report(request.Context(), governance.ReportInput{
		ProjectID: principal.Project.ID, WorkUnitID: workUnitID, DefinitionID: input.DefinitionID,
		Value: input.Value, ReporterKeyID: principal.Key.ID, EvidenceRef: input.EvidenceRef,
		EvidenceSHA256: input.EvidenceSHA256, ObservedAt: input.ObservedAt,
		SupersedesOutcomeID: input.SupersedesOutcomeID,
		Intent:              governance.Intent{IdempotencyKeyHash: intent.IdempotencyKeyHash, RequestFingerprint: intent.RequestFingerprint},
	})
	if err != nil {
		writeOutcomeError(writer, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, outcome)
}

func writeOutcomeError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, governance.ErrNotFound):
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit or definition was not found")
	case errors.Is(err, governance.ErrDefinitionDenied):
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome", "definition was not declared by the work unit")
	case errors.Is(err, governance.ErrRevisionConflict):
		writeGovernanceError(writer, http.StatusConflict, "outcome_revision_conflict", "supersedes_outcome_id is not the current outcome")
	case errors.Is(err, governance.ErrIdempotency):
		writeGovernanceError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency-Key conflicts with another request")
	case errors.Is(err, governance.ErrRevisionLimit), errors.Is(err, governance.ErrWriteWindow):
		writeGovernanceError(writer, http.StatusConflict, "outcome_write_closed", err.Error())
	case errors.Is(err, governance.ErrUnavailable):
		writeGovernanceError(writer, http.StatusServiceUnavailable, "governance_unavailable", "outcome governance is unavailable")
	default:
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_outcome", err.Error())
	}
}

func (r *Runtime) closeWorkUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeWorkUnitCreate)
	if !ok {
		return
	}
	workUnitID := chi.URLParam(request, "workUnitID")
	if !domain.ValidWorkUnitID(workUnitID) {
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
		return
	}
	var input closeWorkUnitInput
	if !decodeGovernanceJSON(writer, request, &input) {
		return
	}
	intent, err := governanceIntent(request, "work_unit.close", struct {
		WorkUnitID string `json:"work_unit_id"`
	}{workUnitID})
	if err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	workUnit, replay, err := r.accounting.CloseWorkUnit(request.Context(), principal.Project.ID, principal.Key.ID, workUnitID, intent)
	if err != nil {
		writeGovernanceOperationError(writer, err, "work_unit")
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, workUnit)
}

func (r *Runtime) createRun(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeRunCreate)
	if !ok {
		return
	}
	var input createRunInput
	if !decodeGovernanceJSON(writer, request, &input) {
		return
	}
	if !domain.ValidWorkUnitID(input.WorkUnitID) {
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
		return
	}
	if input.BudgetMicrosUSD == 0 {
		input.BudgetMicrosUSD = principal.Project.RunGovernance.DefaultRunBudgetMicrosUSD
	}
	if input.TTLSeconds == 0 {
		input.TTLSeconds = principal.Project.RunGovernance.DefaultRunTTLSeconds
	}
	if input.BudgetMicrosUSD <= 0 || input.BudgetMicrosUSD > principal.Project.RunGovernance.MaxRunBudgetMicrosUSD ||
		input.TTLSeconds <= 0 || input.TTLSeconds > principal.Project.RunGovernance.MaxRunTTLSeconds {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_run", "run budget or TTL is outside the project limits")
		return
	}
	intent, err := governanceIntent(request, "run.create", input)
	if err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	run, replay, err := r.accounting.CreateRun(request.Context(), principal.Project.ID, principal.Key.ID, input.WorkUnitID,
		input.BudgetMicrosUSD, time.Duration(input.TTLSeconds)*time.Second, principal.Project.RunGovernance.MaxActiveRuns, intent)
	if err != nil {
		writeGovernanceOperationError(writer, err, "run")
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, run)
}

func (r *Runtime) getRun(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeGovernanceRead)
	if !ok {
		return
	}
	runID := chi.URLParam(request, "runID")
	if !domain.ValidRunID(runID) {
		writeGovernanceError(writer, http.StatusNotFound, "run_not_found", "run was not found")
		return
	}
	run, found := r.accounting.Run(principal.Project.ID, runID)
	if !found {
		writeGovernanceError(writer, http.StatusNotFound, "run_not_found", "run was not found")
		return
	}
	run.Status = domain.EffectiveRunStatus(run, r.clockNow())
	writeJSON(writer, http.StatusOK, run)
}

func (r *Runtime) closeRun(writer http.ResponseWriter, request *http.Request) {
	principal, ok := r.governancePrincipal(writer, request, domain.GatewayScopeRunCreate)
	if !ok {
		return
	}
	runID := chi.URLParam(request, "runID")
	if !domain.ValidRunID(runID) {
		writeGovernanceError(writer, http.StatusNotFound, "run_not_found", "run was not found")
		return
	}
	var input closeRunInput
	if !decodeGovernanceJSON(writer, request, &input) {
		return
	}
	if input.Reason == "" {
		input.Reason = "completed"
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" || len(input.Reason) > 64 {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_close_reason", "close reason must contain 1 to 64 characters")
		return
	}
	run, found := r.accounting.Run(principal.Project.ID, runID)
	if !found {
		writeGovernanceError(writer, http.StatusNotFound, "run_not_found", "run was not found")
		return
	}
	intent, err := governanceIntent(request, "run.close", struct {
		RunID  string `json:"run_id"`
		Reason string `json:"reason"`
	}{runID, input.Reason})
	if err != nil {
		writeGovernanceError(writer, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	closed, replay, err := r.accounting.CloseRun(request.Context(), principal.Project.ID, principal.Key.ID, run.WorkUnitID, run.ID, input.Reason, intent)
	if err != nil {
		writeGovernanceOperationError(writer, err, "run")
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, closed)
}

func writeGovernanceGatewayError(writer http.ResponseWriter, err error) {
	var gatewayErr *gatewaycore.Error
	if errors.As(err, &gatewayErr) {
		writeGovernanceError(writer, gatewayErr.HTTPStatus, gatewayErr.Code, gatewayErr.Message)
		return
	}
	writeGovernanceError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
}

func writeGovernanceOperationError(writer http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, budget.ErrIdempotencyConflict):
		writeGovernanceError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency-Key conflicts with another request")
	case errors.Is(err, budget.ErrResourceLimit):
		writeGovernanceError(writer, http.StatusTooManyRequests, "governance_resource_limit", "run governance resource limit exceeded")
	case errors.Is(err, budget.ErrWorkUnitClosed):
		writeGovernanceError(writer, http.StatusConflict, "work_unit_closed", "work unit is closed or was not found")
	case errors.Is(err, budget.ErrWorkUnitNotFound):
		writeGovernanceError(writer, http.StatusNotFound, "work_unit_not_found", "work unit was not found")
	case errors.Is(err, budget.ErrRunNotFound):
		writeGovernanceError(writer, http.StatusNotFound, "run_not_found", "run was not found")
	default:
		writeGovernanceError(writer, http.StatusServiceUnavailable, "run_governance_unavailable", resource+" state is unavailable")
	}
}

func writeGovernanceError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
