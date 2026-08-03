package app

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/domain"
	gatewaycore "github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/go-chi/chi/v5"
)

func (r *Runtime) adminDashboard(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	now := time.Now()
	dashboard := r.usage.Dashboard(now, r.usageLocation)
	governance, labels, err := r.dashboardGovernance(request, now)
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
	})
}

type dashboardGovernance struct {
	PolicyRejections policyRejectionSummary `json:"policy_rejections"`
	Budget           pressureSummary        `json:"budget"`
	Capacity         pressureSummary        `json:"capacity"`
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

func (r *Runtime) dashboardGovernance(request *http.Request, now time.Time) (dashboardGovernance, map[string]string, error) {
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
		Budget:           budgetPressure(projects, r.state, now.In(r.usageLocation).Format("2006-01-02")),
		Capacity: capacityPressure(
			providers, deployments,
			r.gatewayService.ActiveProviderRequests(), r.providers.ProviderConcurrencyLimits(),
			r.gatewayService.ActiveDeploymentRequests(), r.providers.DeploymentConcurrencyLimits(),
		),
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
	Balance(string, string) ledger.Balance
}, periodID string) pressureSummary {
	result := pressureSummary{Items: make([]pressureItem, 0, len(projects))}
	for _, project := range projects {
		if !project.Enabled || project.DailyBudgetMicrosUSD <= 0 {
			continue
		}
		balance := state.Balance(project.ID, periodID)
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
		"cursor": {}, "limit": {}, "project_id": {}, "provider_id": {},
		"model": {}, "status": {}, "start": {}, "end": {},
	}
	for name := range request.URL.Query() {
		if _, exists := allowed[name]; !exists {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported usage filter"})
			return
		}
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
	page, err := r.usage.QueryAttempts(query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": page.Attempts, "next_cursor": page.NextCursor,
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

func (r *Runtime) adminSystemStatus(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	auditSummary := r.audit.Summary()
	writeJSON(writer, http.StatusOK, map[string]any{
		"build":             buildinfo.Current(),
		"accounting_status": r.status.Load(),
		"draining":          r.draining.Load(),
		"wal":               r.ledger.Stats(),
		"audit":             auditSummary,
		"alerts":            r.alerts.Stats(),
		"usage_watermark":   r.usage.Watermark(),
	})
}

func (r *Runtime) syncUsageAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if err := r.usageCollector.CatchUp(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage analytics unavailable"})
		return false
	}
	return true
}
