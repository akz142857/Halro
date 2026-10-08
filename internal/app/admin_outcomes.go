package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
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
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > governance.OutcomePageMaxRows {
			adminBadRequest(writer, "page limit must be between 1 and 200")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	page, err := r.governance.manager.ReadOutcomePage(ctx, governance.OutcomePageQuery{
		ProjectID: projectID, WorkUnitID: workUnitID, DefinitionID: definitionID, Cursor: query.Get("cursor"), Rows: limit, Bytes: governance.OutcomePageMaxBytes,
	})
	if errors.Is(err, governance.ErrOutcomeCursor) || errors.Is(err, governance.ErrOutcomePageLimit) {
		adminBadRequest(writer, "invalid, expired or oversized outcome page; restart listing with a valid page limit")
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outcome governance is unavailable", "code": "governance_unavailable"})
		return
	}
	refs := make([]ledger.WorkUnitReference, 0, len(page.Items))
	for _, item := range page.Items {
		refs = append(refs, ledger.WorkUnitReference{ProjectID: item.ProjectID, WorkUnitID: item.WorkUnitID})
	}
	accounting, err := r.accounting.ReadWorkUnitMaturityPage(ctx, refs)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	if len(accounting.Items) != len(page.Items) {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting maturity page is inconsistent", "code": "run_governance_unavailable"})
		return
	}
	for index := range page.Items {
		maturity := accounting.Items[index]
		if maturity.Reference != refs[index] {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting maturity page is inconsistent", "code": "run_governance_unavailable"})
			return
		}
		page.Items[index].Provisional = !maturity.Found || maturity.Status == domain.WorkUnitOpen || maturity.Inflight
	}
	if err := ctx.Err(); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outcome page deadline exceeded", "code": "run_governance_unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": page.Items, "next_cursor": page.NextCursor,
		"governance_watermark": map[string]any{"sequence": page.Head.Records, "offset": page.Head.Bytes}, "accounting_watermark": accounting.Watermark})
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
	fence, err := r.governance.manager.CaptureOutcomeFence(ctx)
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
	result := governanceSummary{Basis: "work_unit_cohort", CohortStart: query.Get("cohort_start"), CohortEnd: query.Get("cohort_end"), DefinitionID: definitionID, DefinitionVersion: version,
		GeneratedAt:         time.Now().UTC(),
		GovernanceWatermark: map[string]any{"sequence": fence.Head().Records, "offset": fence.Head().Bytes},
		CostCompleteness:    "complete", OutcomeCompleteness: "complete"}
	// Keep only the current finite batch of eligible, mature references. The
	// Governance fence and Ledger cohort cut belong to independent journals.
	refs := make([]governance.CurrentOutcomeReference, 0, governance.CurrentOutcomeBatchMaxRows)
	flush := func() error {
		if len(refs) == 0 {
			return nil
		}
		observations, err := r.readSummaryOutcomeBatch(ctx, fence, refs)
		if err != nil {
			return err
		}
		if len(observations) != len(refs) {
			return governance.ErrUnavailable
		}
		for i, outcome := range observations {
			if outcome.Reference != refs[i] {
				return governance.ErrUnavailable
			}
			if outcome.Found {
				result.EvaluatedUnits++
				if definition.Successful(outcome.Value) {
					result.SuccessfulUnits++
				}
			}
		}
		clear(refs)
		refs = refs[:0]
		return nil
	}
	head, err := r.accounting.VisitGovernanceCohort(ctx, ledger.GovernanceCohortQuery{
		ProjectID: projectID, DefinitionID: definitionID, DefinitionVersion: version,
		CohortStart: query.Get("cohort_start"), CohortEnd: query.Get("cohort_end"), MaxWorkUnits: 100_000,
	}, func(row ledger.GovernanceCohortRow) error {
		result.EligibleUnits++
		if row.Status != domain.WorkUnitClosed || row.Inflight {
			var err error
			result.InProgressCostMicrosUSD, err = checkedSummaryAdd(result.InProgressCostMicrosUSD, row.KnownCostMicrosUSD)
			if err != nil {
				return ledger.ErrGovernanceSummaryOverflow
			}
			return nil
		}
		result.MaturedUnits++
		var err error
		result.KnownCostMicrosUSD, err = checkedSummaryAdd(result.KnownCostMicrosUSD, row.KnownCostMicrosUSD)
		if err == nil {
			result.EstimatedCostMicrosUSD, err = checkedSummaryAdd(result.EstimatedCostMicrosUSD, row.EstimatedCostMicrosUSD)
		}
		if err == nil {
			result.UnknownAttempts, err = checkedSummaryAdd(result.UnknownAttempts, row.UnknownAttempts)
		}
		if err != nil {
			return ledger.ErrGovernanceSummaryOverflow
		}
		refs = append(refs, governance.CurrentOutcomeReference{ProjectID: projectID, WorkUnitID: row.WorkUnitID, DefinitionID: definitionID, DefinitionVersion: version})
		if len(refs) == governance.CurrentOutcomeBatchMaxRows {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if errors.Is(err, ledger.ErrGovernanceCohortLimit) {
		adminBadRequest(writer, "cohort exceeds 100000 eligible Work Units; use governance export")
		return
	}
	if errors.Is(err, ledger.ErrGovernanceSummaryOverflow) {
		writeSummaryOverflow(writer)
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "summary deadline exceeded or accounting view is unavailable", "code": "governance_summary_unavailable"})
		return
	}
	result.AccountingWatermark = head
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

// Splitting retains the same fact fence and deadline, and only retries a
// source/identity byte limit. Successful values from a failed read are never
// consumed. A legal single native revision chain fits the fixed source bound.
func (r *Runtime) readSummaryOutcomeBatch(ctx context.Context, fence governance.OutcomeFence, refs []governance.CurrentOutcomeReference) ([]governance.CurrentOutcomeObservation, error) {
	observations, err := r.governance.manager.ReadCurrentOutcomeBatch(ctx, fence, refs)
	if !errors.Is(err, governance.ErrOutcomePageLimit) || len(refs) < 2 {
		return observations, err
	}
	middle := len(refs) / 2
	left, err := r.readSummaryOutcomeBatch(ctx, fence, refs[:middle])
	if err != nil {
		return nil, err
	}
	right, err := r.readSummaryOutcomeBatch(ctx, fence, refs[middle:])
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func (r *Runtime) createAdminGovernanceExport(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	release, err := r.governance.exports.enter(request.Context())
	if err != nil {
		code := "governance_export_unavailable"
		if errors.Is(err, errGovernanceExportBusy) {
			code = "governance_export_busy"
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "governance export is unavailable", "code": code})
		return
	}
	defer release()
	exportID, err := id.New("gex")
	if err != nil {
		adminStoreError(writer)
		return
	}
	directory := filepath.Join(r.config.GovernanceExportPath(), exportID)
	manifest, err := r.governance.manager.WriteExportWithDefinitionOwner(request.Context(), directory, r.governanceDefinitionOwner)
	if errors.Is(err, ledger.ErrGovernanceExportBackend) {
		// Only the explicitly unsupported legacy accounting owner keeps the
		// transition path. Corrupt/closed/retired disk owners never fall back.
		manifest, err = r.writeLegacyGovernanceExport(request.Context(), directory)
	}
	if err != nil {
		code := "governance_export_unavailable"
		if errors.Is(err, governance.ErrUnavailable) {
			code = "governance_unavailable"
		}
		if errors.Is(err, governance.ErrExportInconsistent) {
			code = "governance_export_inconsistent"
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "governance export failed", "code": code})
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	_ = r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "governance.export", "governance_export", exportID, "success", "", map[string]any{"directory": exportID})
	writeJSON(writer, http.StatusCreated, map[string]any{"id": exportID, "directory": directory, "manifest": manifest})
}

// State still lacks immutable as-of lifecycle positions. Only accounting retains
// a history-sized snapshot/map here. Gov/Metadata use finite fixed sources, and
// both the selected accounting ticket and Runtime lease include publication.
func (r *Runtime) writeLegacyGovernanceExport(ctx context.Context, directory string) (governance.ExportManifest, error) {
	var manifest governance.ExportManifest
	err := r.governance.manager.WithOutcomeExport(ctx, func(facts governance.OutcomeExportView) error {
		return r.accounting.WithLegacyGovernanceSnapshot(ctx, func(ctx context.Context, accounting ledger.GovernanceSnapshot) error {
			workUnits := make(map[string]domain.WorkUnit, len(accounting.WorkUnits))
			for _, item := range accounting.WorkUnits {
				workUnits[item.ID] = item
			}
			return r.governanceDefinitionOwner(ctx, func(definitions governance.ExportDefinitionSource) error {
				input := governance.ExportStreamInput{AccountingWatermark: accounting.Watermark, GovernanceSequence: facts.Head().Records, GovernanceOffset: facts.Head().Bytes, Definitions: definitions.Read}
				input.WorkUnits = func(ctx context.Context, yield func(domain.WorkUnit) error) error {
					for _, row := range accounting.WorkUnits {
						if err := ctx.Err(); err != nil {
							return err
						}
						if err := yield(row); err != nil {
							return err
						}
					}
					return nil
				}
				input.Runs = func(ctx context.Context, yield func(domain.Run) error) error {
					for _, row := range accounting.Runs {
						if err := ctx.Err(); err != nil {
							return err
						}
						if err := yield(row); err != nil {
							return err
						}
					}
					return nil
				}
				input.Outcomes = func(ctx context.Context, yield func(domain.Outcome) error) error {
					for {
						page, err := facts.ReadPage(ctx, governance.OutcomePageMaxRows, governance.OutcomePageMaxBytes)
						if err != nil {
							return err
						}
						for _, row := range page.Items {
							unit, ok := workUnits[row.WorkUnitID]
							if !ok || unit.ProjectID != row.ProjectID {
								return governance.ErrExportInconsistent
							}
							row.Provisional = unit.Status == domain.WorkUnitOpen || accounting.InflightWorkUnit[unit.ID]
							if err := yield(row); err != nil {
								return err
							}
						}
						if page.Done {
							return nil
						}
					}
				}
				input.BeforePublish = func(ctx context.Context) error {
					if err := definitions.Complete(ctx); err != nil {
						return err
					}
					return facts.Complete(ctx)
				}
				var err error
				manifest, err = governance.WriteExportStream(ctx, directory, input)
				return err
			})
		})
	})
	if err != nil {
		return governance.ExportManifest{}, err
	}
	return manifest, nil
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
