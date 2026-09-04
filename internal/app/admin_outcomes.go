package app

import (
	"context"
	"errors"
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
	"github.com/akz142857/Halro/internal/ledger"
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
	projectID, workUnitID, definitionID := strings.TrimSpace(query.Get("project_id")), strings.TrimSpace(query.Get("work_unit_id")), strings.TrimSpace(query.Get("definition_id"))
	outcomeSnapshot, err := r.governance.manager.ReadOutcomeSnapshot(request.Context(), projectID, workUnitID, definitionID)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outcome governance is unavailable", "code": "governance_unavailable"})
		return
	}
	accounting, err := r.state.GovernanceSnapshot(request.Context(), ledger.GovernanceSnapshotOptions{ProjectID: projectID, WorkUnitID: workUnitID})
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	workUnits := make(map[string]domain.WorkUnit, len(accounting.WorkUnits))
	for _, item := range accounting.WorkUnits {
		workUnits[item.ID] = item
	}
	items := outcomeSnapshot.Outcomes
	for index := range items {
		workUnit, ok := workUnits[items[index].WorkUnitID]
		items[index].Provisional = !ok || workUnit.Status == domain.WorkUnitOpen || accounting.InflightWorkUnit[workUnit.ID]
	}
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
	OutcomeCompleteness     string         `json:"outcome_completeness"`
	OutcomeReason           string         `json:"outcome_reason,omitempty"`
	CostPerSuccessReason    string         `json:"cost_per_success_reason,omitempty"`
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
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	outcomes, err := r.governance.manager.ReadOutcomeSnapshot(ctx, projectID, "", definitionID)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outcome governance is unavailable", "code": "governance_unavailable"})
		return
	}
	definition, err := r.store.GetOutcomeDefinition(ctx, projectID, definitionID, version)
	if err != nil {
		if ctx.Err() != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "summary deadline exceeded", "code": "governance_summary_unavailable"})
		} else {
			adminNotFound(writer)
		}
		return
	}
	accounting, err := r.state.GovernanceSnapshot(ctx, ledger.GovernanceSnapshotOptions{
		ProjectID: projectID, DefinitionID: definitionID, DefinitionVersion: version,
		CohortStart: query.Get("cohort_start"), CohortEnd: query.Get("cohort_end"),
		MaxWorkUnits: 100_000, IncludeAttempts: true,
	})
	if errors.Is(err, ledger.ErrGovernanceCohortLimit) {
		adminBadRequest(writer, "cohort exceeds 100000 eligible Work Units; use governance export")
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "summary deadline exceeded or accounting view is unavailable", "code": "governance_summary_unavailable"})
		return
	}
	costs, estimates, unknown := map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, attempt := range accounting.SettledAttempts {
		wu := attempt.Settlement.WorkUnitID
		if attempt.CostKnown {
			costs[wu], err = checkedSummaryAdd(costs[wu], attempt.CostMicrosUSD)
			if err != nil {
				writeSummaryOverflow(writer)
				return
			}
			if attempt.Settlement.CostEstimated {
				estimates[wu], err = checkedSummaryAdd(estimates[wu], attempt.CostMicrosUSD)
				if err != nil {
					writeSummaryOverflow(writer)
					return
				}
			}
		} else {
			unknown[wu], err = checkedSummaryAdd(unknown[wu], 1)
			if err != nil {
				writeSummaryOverflow(writer)
				return
			}
		}
	}
	result := governanceSummary{Basis: "work_unit_cohort", CohortStart: query.Get("cohort_start"), CohortEnd: query.Get("cohort_end"), DefinitionID: definitionID, DefinitionVersion: version,
		GeneratedAt: time.Now().UTC(), AccountingWatermark: accounting.Watermark,
		GovernanceWatermark: map[string]any{"sequence": outcomes.Sequence, "offset": outcomes.Offset},
		CostCompleteness:    "complete", OutcomeCompleteness: "complete"}
	current := make(map[string]domain.Outcome)
	for _, outcome := range outcomes.Outcomes {
		if outcome.DefinitionVersion != version {
			continue
		}
		if previous, ok := current[outcome.WorkUnitID]; !ok || outcome.GovernanceSequence > previous.GovernanceSequence {
			current[outcome.WorkUnitID] = outcome
		}
	}
	for _, workUnit := range accounting.WorkUnits {
		if err := ctx.Err(); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "summary deadline exceeded; use governance export", "code": "governance_summary_unavailable"})
			return
		}
		result.EligibleUnits++
		matured := workUnit.Status == domain.WorkUnitClosed && !accounting.InflightWorkUnit[workUnit.ID]
		if !matured {
			result.InProgressCostMicrosUSD, err = checkedSummaryAdd(result.InProgressCostMicrosUSD, costs[workUnit.ID])
			if err != nil {
				writeSummaryOverflow(writer)
				return
			}
			continue
		}
		result.MaturedUnits++
		result.KnownCostMicrosUSD, err = checkedSummaryAdd(result.KnownCostMicrosUSD, costs[workUnit.ID])
		if err == nil {
			result.EstimatedCostMicrosUSD, err = checkedSummaryAdd(result.EstimatedCostMicrosUSD, estimates[workUnit.ID])
		}
		if err == nil {
			result.UnknownAttempts, err = checkedSummaryAdd(result.UnknownAttempts, unknown[workUnit.ID])
		}
		if err != nil {
			writeSummaryOverflow(writer)
			return
		}
		if outcome, ok := current[workUnit.ID]; ok {
			result.EvaluatedUnits++
			if definition.Successful(outcome.Value) {
				result.SuccessfulUnits++
			}
		}
	}
	if result.UnknownAttempts > 0 {
		result.CostCompleteness = "partial"
		result.CostPerSuccessReason = "unknown_costs_excluded"
	}
	if result.EligibleUnits > 0 {
		value := float64(result.EvaluatedUnits) / float64(result.EligibleUnits)
		result.OutcomeCoverage = &value
	}
	if result.EvaluatedUnits < result.EligibleUnits {
		result.OutcomeCompleteness = "partial"
		result.OutcomeReason = "missing_or_in_progress_outcomes"
	}
	if result.EligibleUnits == 0 {
		result.OutcomeCompleteness = "unknown"
		result.OutcomeReason = "no_eligible_units"
	}
	if result.EvaluatedUnits > 0 {
		value := float64(result.SuccessfulUnits) / float64(result.EvaluatedUnits)
		result.SuccessRate = &value
	}
	if result.SuccessfulUnits > 0 {
		value := result.KnownCostMicrosUSD / result.SuccessfulUnits
		result.CostPerSuccessMicrosUSD = &value
	} else {
		result.CostPerSuccessReason = "no_successful_units"
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
	outcomes, err := r.governance.manager.ReadOutcomeSnapshot(request.Context(), "", "", "")
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outcome governance is unavailable", "code": "governance_unavailable"})
		return
	}
	accounting, err := r.state.GovernanceSnapshot(request.Context(), ledger.GovernanceSnapshotOptions{IncludeRuns: true})
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	definitions, err := r.store.ListOutcomeDefinitions(request.Context(), "")
	if err != nil {
		adminStoreError(writer)
		return
	}
	workUnits := make(map[string]domain.WorkUnit, len(accounting.WorkUnits))
	for _, item := range accounting.WorkUnits {
		workUnits[item.ID] = item
	}
	for index := range outcomes.Outcomes {
		workUnit, ok := workUnits[outcomes.Outcomes[index].WorkUnitID]
		if !ok {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "governance export references an absent Work Unit", "code": "governance_export_inconsistent"})
			return
		}
		outcomes.Outcomes[index].Provisional = workUnit.Status == domain.WorkUnitOpen || accounting.InflightWorkUnit[workUnit.ID]
	}
	directory := filepath.Join(r.config.GovernanceExportPath(), exportID)
	manifest, err := governance.WriteExport(request.Context(), directory, governance.ExportInput{
		WorkUnits: accounting.WorkUnits, Runs: accounting.Runs, Outcomes: outcomes.Outcomes, Definitions: definitions,
		AccountingWatermark: accounting.Watermark, GovernanceSequence: outcomes.Sequence, GovernanceOffset: outcomes.Offset,
	})
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "governance export failed"})
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	_ = r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "governance.export", "governance_export", exportID, "success", "", map[string]any{"directory": exportID})
	writeJSON(writer, http.StatusCreated, map[string]any{"id": exportID, "directory": directory, "manifest": manifest})
}

func checkedSummaryAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("governance summary amount overflow")
	}
	return left + right, nil
}

func writeSummaryOverflow(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
		"error": "governance summary amount overflow; use export for reconciliation",
		"code":  "governance_summary_overflow",
	})
}
