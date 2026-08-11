package app

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	referenceconfigs "github.com/akz142857/Halro/configs"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/domain"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/timezone"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func (r *Runtime) adminDashboard(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	now := time.Now()
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
	dashboard := r.usage.Dashboard(now, usage.Period{Start: period.Start, End: period.End})
	governance, labels, err := r.dashboardGovernance(request, now, period)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"usage":               dashboard,
		"first_value_reached": r.usage.Metrics().RequestsSuccess > 0,
		"governance":          governance,
		"resource_labels":     labels,
		"accounting_status":   r.status.Load(),
		"wal":                 r.ledger.Stats(),
		"write_path":          r.writePathSummary(),
		"alerts":              r.alerts.Stats(),
		"time_context":        timing,
	})
}

// writePathSummary is the durable write path reduced to the handful of means that
// explain this process's throughput ceilings. Counters answer "how much"; these
// answer "why is that the limit", and they are derived once on the server so the
// CLI and the console cannot disagree about them.
//
// It exists because a single-binary product should be able to answer "what is
// this instance doing right now" without an operator standing up Prometheus
// first. The same numbers are available as metrics for anyone who has.
type writePathSummary struct {
	// Mean cost of one Ledger durability barrier. Every ceiling here is bounded
	// by it, and it spans orders of magnitude between filesystems.
	WALSyncSeconds float64 `json:"wal_sync_seconds"`
	// Mean records per barrier. Near 1.0 under load means appends are not
	// coalescing — a concurrency problem, not a disk problem.
	WALBatchSize float64 `json:"wal_batch_size"`
	// Mean wait for, and hold of, the per-project accounting lock. The hold is
	// the per-project serialization budget: one project cannot exceed
	// 1/hold accounting events per second no matter how many requests it offers.
	ProjectLockWaitSeconds float64 `json:"project_lock_wait_seconds"`
	ProjectLockHeldSeconds float64 `json:"project_lock_held_seconds"`
	// Ceiling implied by the hold above, in accounting events per second for a
	// single project. Reported as observed rather than promised: it is a
	// measurement of this instance's recent behaviour, not a rating.
	ProjectEventsPerSecond float64 `json:"project_events_per_second"`
	// The same ceiling expressed in requests, which is the unit an operator
	// thinks in. One request lifecycle is five accounting events — request
	// accepted, reservation, started, settled, finalized — so this is
	// ProjectEventsPerSecond over that. Approximate by construction: a request
	// that retries or falls back spends more than five, so the real ceiling for
	// such traffic is lower than this reports.
	ProjectRequestsPerSecond float64 `json:"project_requests_per_second"`
	// Mean batched metadata calls per write transaction, the bbolt counterpart
	// of WALBatchSize.
	MetadataBatchSize    float64 `json:"metadata_batch_size"`
	MetadataWriteSeconds float64 `json:"metadata_write_seconds"`
}

// accountingEventsPerRequest is the五-event lifecycle ADR 0018 describes, and is
// a measured fact rather than an estimate: one Gateway request appends exactly
// five Ledger records and takes exactly five project-lock acquisitions.
const accountingEventsPerRequest = 5

func (r *Runtime) writePathSummary() writePathSummary {
	wal := r.ledger.Stats()
	lock := r.accounting.ProjectLockStats()
	metadata := r.store.MetadataWriteStats()
	summary := writePathSummary{
		WALSyncSeconds:         perOperationSeconds(wal.SyncDuration, wal.Syncs),
		WALBatchSize:           ratio(float64(wal.Records), float64(wal.Batches)),
		ProjectLockWaitSeconds: perOperationSeconds(lock.WaitDuration, lock.Acquisitions),
		ProjectLockHeldSeconds: perOperationSeconds(lock.HeldDuration, lock.Acquisitions),
		MetadataBatchSize:      ratio(float64(metadata.BatchCalls), float64(metadata.BatchTransactions)),
		MetadataWriteSeconds:   perOperationSeconds(metadata.PageWriteDuration, uint64(max(metadata.PageWrites, 0))),
	}
	summary.ProjectEventsPerSecond = ratio(1, summary.ProjectLockHeldSeconds)
	summary.ProjectRequestsPerSecond = ratio(summary.ProjectEventsPerSecond, accountingEventsPerRequest)
	return summary
}

func perOperationSeconds(total time.Duration, operations uint64) float64 {
	if operations == 0 {
		return 0
	}
	return total.Seconds() / float64(operations)
}

// ratio keeps an idle instance reporting 0 rather than NaN, which would
// serialize as invalid JSON and take the whole payload down with it.
func ratio(numerator, denominator float64) float64 {
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

type dashboardGovernance struct {
	PolicyRejections policyRejectionSummary   `json:"policy_rejections"`
	Budget           pressureSummary          `json:"budget"`
	Capacity         pressureSummary          `json:"capacity"`
	Pricing          pricingGovernanceSummary `json:"pricing"`
}

type pricingGovernanceSummary struct {
	Quarantined int `json:"quarantined"`
	Unknown     int `json:"unknown"`
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
		"write_path":        r.writePathSummary(),
		"audit":             auditSummary,
		"alerts":            r.alerts.Stats(),
		"usage_watermark":   r.usage.Watermark(),
		"time_context":      timing,
		// The commit protocol makes "durable" and "in force" two different
		// questions, so the answer to the second one has to be visible
		// somewhere an operator can read it. A stale runtime is refusing data
		// plane traffic; the pending count is the audit backlog behind it.
		"activation": r.activation.status(),
	}
	if pending, err := r.store.PendingAdminAuditIntentCount(request.Context()); err == nil {
		payload["pending_admin_audit"] = pending
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
// YAML and a field-level reference for Settings > System configuration. Values
// come from the normalized in-memory Config; operator names, descriptions, and
// ordering come from the annotated example embedded at build time.
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
		"entries":      describeSystemConfig(referenceconfigs.ExampleYAML, rendered),
		"time_context": timing,
	})
}

type systemConfigEntry struct {
	Path          string `json:"path"`
	TitleZH       string `json:"title_zh"`
	TitleEN       string `json:"title_en"`
	DescriptionZH string `json:"description_zh"`
	DescriptionEN string `json:"description_en"`
	Value         string `json:"value"`
	Kind          string `json:"kind"`
}

func describeSystemConfig(reference, effective []byte) []systemConfigEntry {
	var referenceDoc, effectiveDoc yaml.Node
	if yaml.Unmarshal(reference, &referenceDoc) != nil || yaml.Unmarshal(effective, &effectiveDoc) != nil || len(referenceDoc.Content) == 0 || len(effectiveDoc.Content) == 0 {
		return nil
	}
	effectiveValues := make(map[string]*yaml.Node)
	collectConfigLeaves(effectiveDoc.Content[0], "", func(path string, node *yaml.Node) { effectiveValues[path] = node })
	entries := make([]systemConfigEntry, 0, len(effectiveValues))
	seen := make(map[string]bool, len(effectiveValues))
	collectConfigLeaves(referenceDoc.Content[0], "", func(path string, node *yaml.Node) {
		value, ok := effectiveValues[path]
		if !ok {
			return
		}
		metadata := configCommentMetadata(node.HeadComment)
		entries = append(entries, systemConfigEntry{
			Path: path, TitleZH: metadata["title.zh-CN"], TitleEN: metadata["title.en-US"],
			DescriptionZH: metadata["description.zh-CN"], DescriptionEN: metadata["description.en-US"],
			Value: configNodeValue(value), Kind: configNodeKind(value),
		})
		seen[path] = true
	})
	// Fail visibly instead of hiding a valid Config field when its reference
	// annotation is temporarily missing. CI tests keep this fallback exceptional.
	var extra []string
	for path := range effectiveValues {
		if !seen[path] {
			extra = append(extra, path)
		}
	}
	sort.Strings(extra)
	for _, path := range extra {
		value := effectiveValues[path]
		entries = append(entries, systemConfigEntry{Path: path, Value: configNodeValue(value), Kind: configNodeKind(value)})
	}
	return entries
}

func collectConfigLeaves(node *yaml.Node, prefix string, visit func(string, *yaml.Node)) {
	if node.Kind != yaml.MappingNode {
		visit(prefix, node)
		return
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + path
		}
		if value.Kind == yaml.MappingNode {
			collectConfigLeaves(value, path, visit)
		} else {
			// Comments written above a key belong to the key node in yaml.v3.
			if value.HeadComment == "" {
				value.HeadComment = key.HeadComment
			}
			visit(path, value)
		}
	}
}

func configCommentMetadata(comment string) map[string]string {
	metadata := make(map[string]string)
	for _, line := range strings.Split(comment, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if !strings.HasPrefix(line, "@") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "@"), " ")
		if ok {
			metadata[key] = strings.TrimSpace(value)
		}
	}
	return metadata
}

func configNodeValue(node *yaml.Node) string {
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	rendered, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Sprint(node.Value)
	}
	return strings.TrimSpace(string(rendered))
}

func configNodeKind(node *yaml.Node) string {
	if node.Kind == yaml.SequenceNode || node.Kind == yaml.MappingNode {
		return "collection"
	}
	if node.Tag == "!!bool" {
		return "boolean"
	}
	if node.Tag == "!!int" || node.Tag == "!!float" {
		return "number"
	}
	return "text"
}

func (r *Runtime) syncUsageAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if err := r.usageCollector.CatchUp(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage analytics unavailable"})
		return false
	}
	return true
}
