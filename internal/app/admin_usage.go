package app

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/domain"
	gatewaycore "github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/timezone"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func (r *Runtime) adminDashboard(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	now := time.Now()
	basis := request.URL.Query().Get("reporting_basis")
	if basis == "" {
		basis = "service_period_restated"
	}
	if basis != "service_period_restated" && basis != "adjustment_posted" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid reporting_basis"})
		return
	}
	timing, ok := r.writeTimeContext(writer, now)
	if !ok {
		return
	}
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		adminStoreError(writer)
		return
	}
	// The same interval the response advertises in time_context, so the totals
	// and the window they claim to cover cannot disagree.
	dashboard := r.usage.DashboardForBasis(now, usage.Period{Start: period.Start, End: period.End}, basis)
	governance, labels, err := r.dashboardGovernance(request, now, period)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"usage":             dashboard,
		"governance":        governance,
		"resource_labels":   labels,
		"accounting_status": r.status.Load(),
		"wal":               r.ledger.Stats(),
		"alerts":            r.alerts.Stats(),
		"time_context":      timing,
	})
}

type dashboardGovernance struct {
	PolicyRejections policyRejectionSummary   `json:"policy_rejections"`
	Budget           pressureSummary          `json:"budget"`
	Capacity         pressureSummary          `json:"capacity"`
	Pricing          pricingGovernanceSummary `json:"pricing"`
}

type pricingGovernanceSummary struct {
	Quarantined          int `json:"quarantined"`
	Unknown              int `json:"unknown"`
	AdjustmentOverBudget int `json:"adjustment_over_budget"`
}

type policyRejectionSummary struct {
	RPM                   uint64 `json:"rpm"`
	TPM                   uint64 `json:"tpm"`
	ProjectConcurrency    uint64 `json:"project_concurrency"`
	ProviderConcurrency   uint64 `json:"provider_concurrency"`
	DeploymentConcurrency uint64 `json:"deployment_concurrency"`
	Budget                uint64 `json:"budget"`
	TokenGuard            uint64 `json:"token_guard"`
	Total                 uint64 `json:"total"`
}

type pressureSummary struct {
	AtRisk int            `json:"at_risk"`
	Items  []pressureItem `json:"items"`
}

type pressureItem struct {
	Scope              string  `json:"scope"`
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Current            int64   `json:"current"`
	Limit              int64   `json:"limit"`
	Utilization        float64 `json:"utilization"`
	CommittedMicrosUSD int64   `json:"committed_micros_usd,omitempty"`
	ReservedMicrosUSD  int64   `json:"reserved_micros_usd,omitempty"`
}

func (r *Runtime) dashboardGovernance(request *http.Request, now time.Time, period budget.Period) (dashboardGovernance, map[string]string, error) {
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return dashboardGovernance{}, nil, err
	}
	providers, err := r.store.ListProviders(request.Context())
	if err != nil {
		return dashboardGovernance{}, nil, err
	}
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		return dashboardGovernance{}, nil, err
	}
	labels := make(map[string]string, len(projects)+len(providers)+len(deployments))
	for _, project := range projects {
		labels[project.ID] = project.Name
	}
	for _, provider := range providers {
		labels[provider.ID] = provider.Name
	}
	for _, deployment := range deployments {
		labels[deployment.ID] = deployment.Name
	}
	rejections := r.gatewayService.RejectionMetrics()
	governance := dashboardGovernance{
		PolicyRejections: summarizeRejections(rejections),
		Budget:           budgetPressure(projects, r.state, period),
		Capacity: capacityPressure(
			providers, deployments,
			r.gatewayService.ActiveProviderRequests(), r.providers.ProviderConcurrencyLimits(),
			r.gatewayService.ActiveDeploymentRequests(), r.providers.DeploymentConcurrencyLimits(),
		),
	}
	governance.Pricing.Quarantined, err = r.store.PricingQuarantineCount(request.Context())
	if err != nil {
		return dashboardGovernance{}, nil, err
	}
	for _, deployment := range deployments {
		if !deployment.Enabled || deployment.DeletedAt != nil {
			continue
		}
		if _, priceErr := r.store.SelectDeploymentPriceVersion(request.Context(), deployment.ID, now.UTC()); priceErr != nil {
			governance.Pricing.Unknown++
		}
	}
	for _, project := range projects {
		balance := r.state.Balance(project.ID, period.ID, period.TimezoneVersion)
		if project.DailyBudgetMicrosUSD > 0 && balance.AdjustmentDeltaMicrosUSD != 0 && balance.CommittedMicrosUSD > project.DailyBudgetMicrosUSD {
			governance.Pricing.AdjustmentOverBudget++
		}
	}
	return governance, labels, nil
}

func summarizeRejections(source gatewaycore.RejectionMetrics) policyRejectionSummary {
	result := policyRejectionSummary{
		RPM: source.RPM, TPM: source.TPM, ProjectConcurrency: source.ProjectConcurrency,
		ProviderConcurrency:   source.ProviderConcurrency,
		DeploymentConcurrency: source.DeploymentConcurrency,
		Budget:                source.Budget, TokenGuard: source.TokenGuard,
	}
	result.Total = result.RPM + result.TPM + result.ProjectConcurrency +
		result.ProviderConcurrency + result.DeploymentConcurrency + result.Budget + result.TokenGuard
	return result
}

func budgetPressure(projects []domain.Project, state interface {
	Balance(string, string, uint64) ledger.Balance
}, period budget.Period) pressureSummary {
	result := pressureSummary{Items: make([]pressureItem, 0, len(projects))}
	for _, project := range projects {
		if !project.Enabled || project.DailyBudgetMicrosUSD <= 0 {
			continue
		}
		balance := state.Balance(project.ID, period.ID, period.TimezoneVersion)
		current := balance.CommittedMicrosUSD + balance.ReservedMicrosUSD
		utilization := float64(current) / float64(project.DailyBudgetMicrosUSD)
		if utilization >= .8 {
			result.AtRisk++
		}
		result.Items = append(result.Items, pressureItem{
			Scope: "project", ID: project.ID, Name: project.Name,
			Current: current, Limit: project.DailyBudgetMicrosUSD, Utilization: utilization,
			CommittedMicrosUSD: balance.CommittedMicrosUSD, ReservedMicrosUSD: balance.ReservedMicrosUSD,
		})
	}
	result.Items = topPressure(result.Items, 5)
	return result
}

func capacityPressure(
	providers []domain.ProviderInstance,
	deployments []domain.Deployment,
	activeProviders, providerLimits, activeDeployments, deploymentLimits map[string]int64,
) pressureSummary {
	result := pressureSummary{Items: make([]pressureItem, 0, len(providers)+len(deployments))}
	for _, provider := range providers {
		if limit := providerLimits[provider.ID]; provider.Enabled && limit > 0 {
			result.Items = append(result.Items, newCapacityPressure("provider", provider.ID, provider.Name, activeProviders[provider.ID], limit))
		}
	}
	for _, deployment := range deployments {
		if limit := deploymentLimits[deployment.ID]; deployment.Enabled && limit > 0 {
			result.Items = append(result.Items, newCapacityPressure("deployment", deployment.ID, deployment.Name, activeDeployments[deployment.ID], limit))
		}
	}
	for _, item := range result.Items {
		if item.Utilization >= .8 {
			result.AtRisk++
		}
	}
	result.Items = topPressure(result.Items, 5)
	return result
}

func newCapacityPressure(scope, id, name string, current, limit int64) pressureItem {
	return pressureItem{
		Scope: scope, ID: id, Name: name, Current: current, Limit: limit,
		Utilization: float64(current) / float64(limit),
	}
}

func topPressure(items []pressureItem, limit int) []pressureItem {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Utilization != items[j].Utilization {
			return items[i].Utilization > items[j].Utilization
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func (r *Runtime) adminUsage(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	allowed := map[string]struct{}{
		"cursor": {}, "limit": {}, "project_id": {}, "provider_id": {}, "request_id": {},
		"model": {}, "status": {}, "start": {}, "end": {},
	}
	for name := range request.URL.Query() {
		if _, exists := allowed[name]; !exists {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported usage filter"})
			return
		}
	}
	if len(request.URL.Query().Get("request_id")) > 128 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request ID"})
		return
	}
	cursor, err := usage.DecodeCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid page limit"})
			return
		}
	}
	query := usage.AttemptQuery{
		BeforeSequence: cursor, Limit: limit,
		ProjectID:      request.URL.Query().Get("project_id"),
		ProviderID:     request.URL.Query().Get("provider_id"),
		RequestID:      request.URL.Query().Get("request_id"),
		RequestedModel: request.URL.Query().Get("model"),
		Status:         request.URL.Query().Get("status"),
	}
	if raw := request.URL.Query().Get("start"); raw != "" {
		query.Start, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid start timestamp"})
			return
		}
	}
	if raw := request.URL.Query().Get("end"); raw != "" {
		query.End, err = time.Parse(time.RFC3339, raw)
		if err != nil || !query.Start.IsZero() && !query.End.After(query.Start) {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid end timestamp"})
			return
		}
	}
	timing, ok := r.writeTimeContext(writer, time.Now())
	if !ok {
		return
	}
	page, err := r.usage.QueryAttempts(query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": page.Attempts, "next_cursor": page.NextCursor, "time_context": timing,
	})
}

func (r *Runtime) adminUsageRequest(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	requestID := chi.URLParam(request, "requestID")
	if requestID == "" || len(requestID) > 128 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request ID"})
		return
	}
	detail, exists := r.usage.RequestDetail(requestID)
	if !exists {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "usage request not found"})
		return
	}
	writeJSON(writer, http.StatusOK, detail)
}

func (r *Runtime) adminUsageAttemptAdjustments(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	attemptID := chi.URLParam(request, "attemptID")
	if attemptID == "" || len(attemptID) > 128 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid attempt ID"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": r.usage.AttemptAdjustments(attemptID), "reporting_bases": []string{"service_period_restated", "adjustment_posted"}})
}

func (r *Runtime) adminSystemStatus(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	timing, ok := r.writeTimeContext(writer, time.Now())
	if !ok {
		return
	}
	auditSummary := r.audit.Summary()
	payload := map[string]any{
		"build":             buildinfo.Current(),
		"accounting_status": r.status.Load(),
		"draining":          r.draining.Load(),
		"wal":               r.ledger.Stats(),
		"audit":             auditSummary,
		"alerts":            r.alerts.Stats(),
		"usage_watermark":   r.usage.Watermark(),
		"time_context":      timing,
	}
	// Reported here so an operator can compare it against another node without
	// shell access: divergent rules place the same instant in different
	// accounting periods.
	if database, err := timezone.Describe(r.config.Usage.Timezone); err == nil {
		payload["tzdata"] = database
	}
	writeJSON(writer, http.StatusOK, payload)
}

// adminSystemConfig renders the effective, normalized config.yaml back as
// YAML for the Settings > Diagnostics preview. It re-marshals the in-memory
// Config rather than reading the file from disk: the struct is the schema
// boundary (config.Decode rejects unknown fields), so nothing can end up in
// this response that wasn't already validated into a known, non-secret field.
func (r *Runtime) adminSystemConfig(writer http.ResponseWriter, request *http.Request) {
	timing, ok := r.writeTimeContext(writer, time.Now())
	if !ok {
		return
	}
	rendered, err := yaml.Marshal(r.config)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "config render failed"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"yaml":         string(rendered),
		"time_context": timing,
	})
}

func (r *Runtime) syncUsageAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if err := r.usageCollector.CatchUp(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage analytics unavailable"})
		return false
	}
	return true
}
