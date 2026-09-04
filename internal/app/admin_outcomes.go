package app

import (
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/id"
	"github.com/go-chi/chi/v5"
)

type outcomeDefinitionInput struct {
	Name          string                 `json:"name"`
	DataType      domain.OutcomeDataType `json:"data_type"`
	AllowedValues []string               `json:"allowed_values"`
	SuccessValues []string               `json:"success_values"`
	Unit          string                 `json:"unit,omitempty"`
	Description   string                 `json:"description,omitempty"`
	Enabled       *bool                  `json:"enabled,omitempty"`
}

func (r *Runtime) listAdminOutcomeDefinitions(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{"cursor": {}, "limit": {}}) {
		return
	}
	items, err := r.store.ListOutcomeDefinitions(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminStoreError(writer)
		return
	}
	writeGovernanceAdminPage(writer, request, items, func(item domain.OutcomeDefinition) string {
		return fmt.Sprintf("%s:%020d", item.ID, item.Version)
	})
}

func (r *Runtime) createAdminOutcomeDefinition(writer http.ResponseWriter, request *http.Request) {
	expectedProject, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input outcomeDefinitionInput
	if err := decodeAdminJSONLimit(request, &input, governanceBodyLimit); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	project, err := r.store.GetProject(request.Context(), chi.URLParam(request, "id"))
	if err != nil || project.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if project.Revision != expectedProject {
		adminPreconditionFailed(writer)
		return
	}
	definitionID, err := id.New("odef")
	if err != nil {
		adminStoreError(writer)
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	intent, err := r.newAdminAuditIntent(request, "outcome_definition.create", "outcome_definition", definitionID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	definition := domain.OutcomeDefinition{ID: definitionID, ProjectID: project.ID, Name: input.Name, Version: 1,
		DataType: input.DataType, AllowedValues: input.AllowedValues, SuccessValues: input.SuccessValues,
		Unit: input.Unit, Description: input.Description, Enabled: enabled, CreatedAt: time.Now().UTC(), CreatedBy: intent.ActorID}
	definition, err = r.store.PutOutcomeDefinition(request.Context(), definition, expectedProject, 0, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(definition.Revision))
	writeJSON(writer, http.StatusCreated, definition)
}

func (r *Runtime) createAdminOutcomeDefinitionVersion(writer http.ResponseWriter, request *http.Request) {
	expectedDefinition, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input outcomeDefinitionInput
	if err := decodeAdminJSONLimit(request, &input, governanceBodyLimit); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	projectID, definitionID := chi.URLParam(request, "id"), chi.URLParam(request, "definitionID")
	project, err := r.store.GetProject(request.Context(), projectID)
	if err != nil || project.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	current, err := r.store.GetOutcomeDefinition(request.Context(), projectID, definitionID, 0)
	if err != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expectedDefinition {
		adminPreconditionFailed(writer)
		return
	}
	if input.Name != "" && input.Name != current.Name {
		adminBadRequest(writer, "outcome definition name is immutable")
		return
	}
	enabled := current.Enabled
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	intent, err := r.newAdminAuditIntent(request, "outcome_definition.version", "outcome_definition", definitionID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	definition := domain.OutcomeDefinition{ID: current.ID, ProjectID: current.ProjectID, Name: current.Name, Version: current.Version + 1,
		DataType: input.DataType, AllowedValues: input.AllowedValues, SuccessValues: input.SuccessValues,
		Unit: input.Unit, Description: input.Description, Enabled: enabled, CreatedAt: time.Now().UTC(), CreatedBy: intent.ActorID}
	definition, err = r.store.PutOutcomeDefinition(request.Context(), definition, project.Revision, expectedDefinition, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(definition.Revision))
	writeJSON(writer, http.StatusCreated, definition)
}

func (r *Runtime) listAdminOutcomes(writer http.ResponseWriter, request *http.Request) {
	allowed := map[string]struct{}{"cursor": {}, "limit": {}, "project_id": {}, "work_unit_id": {}, "definition_id": {}}
	if !allowQueryKeys(writer, request, allowed) {
		return
	}
	query := request.URL.Query()
	items := r.governance.manager.Outcomes(strings.TrimSpace(query.Get("project_id")), strings.TrimSpace(query.Get("work_unit_id")), strings.TrimSpace(query.Get("definition_id")))
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeGovernanceAdminPage(writer, request, items, func(item domain.Outcome) string { return item.ID })
}

type governanceSummary struct {
	Basis                   string         `json:"basis"`
	CohortStart             string         `json:"cohort_start"`
	CohortEnd               string         `json:"cohort_end"`
	DefinitionID            string         `json:"definition_id"`
	DefinitionVersion       uint64         `json:"definition_version"`
	GeneratedAt             time.Time      `json:"generated_at"`
	AccountingWatermark     any            `json:"accounting_watermark"`
	GovernanceWatermark     map[string]any `json:"governance_watermark"`
	EligibleUnits           int64          `json:"eligible_units"`
	MaturedUnits            int64          `json:"matured_units"`
	EvaluatedUnits          int64          `json:"evaluated_units"`
	SuccessfulUnits         int64          `json:"successful_units"`
	OutcomeCoverage         *float64       `json:"outcome_coverage"`
	SuccessRate             *float64       `json:"success_rate"`
	KnownCostMicrosUSD      int64          `json:"known_cost_micros_usd"`
	InProgressCostMicrosUSD int64          `json:"in_progress_cost_micros_usd"`
	EstimatedCostMicrosUSD  int64          `json:"estimated_cost_micros_usd"`
	UnknownAttempts         int64          `json:"unknown_attempts"`
	CostCompleteness        string         `json:"cost_completeness"`
	CostPerSuccessMicrosUSD *int64         `json:"cost_per_success_micros_usd"`
}

func (r *Runtime) adminGovernanceSummary(writer http.ResponseWriter, request *http.Request) {
	allowed := map[string]struct{}{"project_id": {}, "definition_id": {}, "definition_version": {}, "cohort_start": {}, "cohort_end": {}}
	if !allowQueryKeys(writer, request, allowed) {
		return
	}
	query := request.URL.Query()
	projectID, definitionID := strings.TrimSpace(query.Get("project_id")), strings.TrimSpace(query.Get("definition_id"))
	version, err := strconv.ParseUint(query.Get("definition_version"), 10, 64)
	start, startErr := time.Parse("2006-01-02", query.Get("cohort_start"))
	end, endErr := time.Parse("2006-01-02", query.Get("cohort_end"))
	if projectID == "" || !domain.ValidOutcomeDefinitionID(definitionID) || err != nil || version == 0 || startErr != nil || endErr != nil || end.Before(start) || end.Sub(start) > 89*24*time.Hour {
		adminBadRequest(writer, "summary requires a valid project, definition version, and cohort of at most 90 days")
		return
	}
	definition, err := r.store.GetOutcomeDefinition(request.Context(), projectID, definitionID, version)
	if err != nil {
		adminNotFound(writer)
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	workUnits := r.state.WorkUnits(projectID)
	if len(workUnits) > 100_000 {
		adminBadRequest(writer, "cohort exceeds 100000 Work Units; use governance export")
		return
	}
	pendingRuns := map[string]struct{}{}
	for _, lease := range r.state.PendingLeases() {
		pendingRuns[lease.Reservation.RunID] = struct{}{}
	}
	runWorkUnit := map[string]string{}
	for _, run := range r.state.Runs(projectID, "") {
		runWorkUnit[run.ID] = run.WorkUnitID
	}
	costs, estimates, unknown := map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, attempt := range r.state.SettledAttempts() {
		wu := attempt.Settlement.WorkUnitID
		if wu == "" {
			continue
		}
		if attempt.CostKnown {
			costs[wu] = satAdd(costs[wu], attempt.CostMicrosUSD)
			if attempt.Settlement.CostEstimated {
				estimates[wu] = satAdd(estimates[wu], attempt.CostMicrosUSD)
			}
		} else {
			unknown[wu]++
		}
	}
	result := governanceSummary{Basis: "work_unit_cohort", CohortStart: query.Get("cohort_start"), CohortEnd: query.Get("cohort_end"), DefinitionID: definitionID, DefinitionVersion: version,
		GeneratedAt: time.Now().UTC(), AccountingWatermark: r.state.Watermark(), CostCompleteness: "complete"}
	if r.governance.log != nil {
		summary := r.governance.log.Summary()
		result.GovernanceWatermark = map[string]any{"sequence": summary.Records, "offset": summary.Bytes}
	} else {
		result.GovernanceWatermark = map[string]any{"sequence": 0, "offset": 0}
	}
	for _, workUnit := range workUnits {
		if time.Now().After(deadline) {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "summary deadline exceeded; use governance export"})
			return
		}
		if workUnit.PeriodID < result.CohortStart || workUnit.PeriodID > result.CohortEnd {
			continue
		}
		eligible := false
		for _, ref := range workUnit.OutcomeDefinitions {
			if ref.ID == definitionID && ref.Version == version {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		result.EligibleUnits++
		matured := workUnit.Status == domain.WorkUnitClosed
		for runID, wuID := range runWorkUnit {
			if wuID == workUnit.ID {
				if _, inflight := pendingRuns[runID]; inflight {
					matured = false
				}
			}
		}
		if !matured {
			result.InProgressCostMicrosUSD = satAdd(result.InProgressCostMicrosUSD, costs[workUnit.ID])
			continue
		}
		result.MaturedUnits++
		result.KnownCostMicrosUSD = satAdd(result.KnownCostMicrosUSD, costs[workUnit.ID])
		result.EstimatedCostMicrosUSD = satAdd(result.EstimatedCostMicrosUSD, estimates[workUnit.ID])
		result.UnknownAttempts = satAdd(result.UnknownAttempts, unknown[workUnit.ID])
		if outcome, ok := r.governance.manager.State().Current(projectID, workUnit.ID, definitionID, version); ok {
			result.EvaluatedUnits++
			if definition.Successful(outcome.Value) {
				result.SuccessfulUnits++
			}
		}
	}
	if result.UnknownAttempts > 0 {
		result.CostCompleteness = "partial"
	}
	if result.EligibleUnits > 0 {
		value := float64(result.EvaluatedUnits) / float64(result.EligibleUnits)
		result.OutcomeCoverage = &value
	}
	if result.EvaluatedUnits > 0 {
		value := float64(result.SuccessfulUnits) / float64(result.EvaluatedUnits)
		result.SuccessRate = &value
	}
	if result.SuccessfulUnits > 0 {
		value := result.KnownCostMicrosUSD / result.SuccessfulUnits
		result.CostPerSuccessMicrosUSD = &value
	}
	writeJSON(writer, http.StatusOK, result)
}

func (r *Runtime) createAdminGovernanceExport(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	exportID, err := id.New("gex")
	if err != nil {
		adminStoreError(writer)
		return
	}
	directory := filepath.Join(r.config.GovernanceExportPath(), exportID)
	definitions, err := r.store.ListOutcomeDefinitions(request.Context(), "")
	if err != nil {
		adminStoreError(writer)
		return
	}
	journalSummary := map[string]int64{"sequence": 0, "offset": 0}
	if r.governance.log != nil {
		current := r.governance.log.Summary()
		journalSummary["sequence"], journalSummary["offset"] = int64(current.Records), current.Bytes
	}
	manifest, err := governance.WriteExport(request.Context(), directory, governance.ExportInput{
		WorkUnits: r.state.WorkUnits(""), Runs: r.state.Runs("", ""), Outcomes: r.governance.manager.Outcomes("", "", ""), Definitions: definitions,
		AccountingWatermark: r.state.Watermark(), GovernanceSequence: uint64(journalSummary["sequence"]), GovernanceOffset: journalSummary["offset"],
	})
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "governance export failed"})
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	_ = r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "governance.export", "governance_export", exportID, "success", "", map[string]any{"directory": exportID})
	writeJSON(writer, http.StatusCreated, map[string]any{"id": exportID, "directory": directory, "manifest": manifest})
}

func satAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}
