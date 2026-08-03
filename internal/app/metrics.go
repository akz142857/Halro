package app

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/akz142857/Heimdall/internal/vault"
)

func MetricsToken(cfg config.Config) ([]byte, error) {
	if cfg.Metrics.CredentialFile != "" {
		return nil, errors.New("versioned metrics credentials are enabled; use metrics rotate")
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKeyFile)
	if err != nil {
		return nil, err
	}
	defer clear(masterKey)
	return vault.DeriveMetricsBearerToken(masterKey)
}

func (r *Runtime) authorizeMetrics(request *http.Request) bool {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" {
		return false
	}
	if r.config.Metrics.CredentialFile != "" {
		authorized, err := r.metricsAuthorizer.Authorize(token, time.Now())
		if err != nil {
			r.logger.Error("metrics credential authorization failed", "error", err)
			return false
		}
		return authorized
	}
	hash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(hash[:], r.metricsTokenHash[:]) == 1
}

func (r *Runtime) writeMetrics(writer http.ResponseWriter) error {
	usageMetrics := r.usage.Metrics()
	ledgerStats := r.ledger.Stats()
	collectorStats := r.usageCollector.Stats()
	alertStats := r.alerts.Stats()
	rejections := r.gatewayService.RejectionMetrics()
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	output := bufio.NewWriter(writer)

	metricHeader(output, "heimdall_requests_total", "counter", "Completed Gateway requests.")
	fmt.Fprintf(output, "heimdall_requests_total{status=\"success\"} %d\n", usageMetrics.RequestsSuccess)
	fmt.Fprintf(output, "heimdall_requests_total{status=\"error\"} %d\n", usageMetrics.RequestsError)
	metricHeader(output, "heimdall_attempts_total", "counter", "Completed Provider attempts.")
	fmt.Fprintf(output, "heimdall_attempts_total{status=\"success\"} %d\n", usageMetrics.AttemptsSuccess)
	fmt.Fprintf(output, "heimdall_attempts_total{status=\"error\"} %d\n", usageMetrics.AttemptsError)
	metricHeader(output, "heimdall_tokens_total", "counter", "Provider tokens accounted by direction.")
	fmt.Fprintf(output, "heimdall_tokens_total{direction=\"input\"} %d\n", usageMetrics.InputTokens)
	fmt.Fprintf(output, "heimdall_tokens_total{direction=\"output\"} %d\n", usageMetrics.OutputTokens)
	metricHeader(output, "heimdall_cost_usd_total", "counter", "Provider cost accounted in USD.")
	fmt.Fprintf(output, "heimdall_cost_usd_total %s\n", microsUSD(usageMetrics.CostMicrosUSD))
	metricHeader(output, "heimdall_request_duration_seconds", "summary", "Completed Gateway request duration.")
	requestCount := usageMetrics.RequestsSuccess + usageMetrics.RequestsError
	fmt.Fprintf(output, "heimdall_request_duration_seconds_sum %s\n",
		millisecondsSeconds(usageMetrics.RequestLatencyMillis))
	fmt.Fprintf(output, "heimdall_request_duration_seconds_count %d\n", requestCount)
	metricHeader(output, "heimdall_attempt_duration_seconds", "summary", "Completed Provider attempt duration.")
	attemptCount := usageMetrics.AttemptsSuccess + usageMetrics.AttemptsError
	fmt.Fprintf(output, "heimdall_attempt_duration_seconds_sum %s\n",
		millisecondsSeconds(usageMetrics.AttemptLatencyMillis))
	fmt.Fprintf(output, "heimdall_attempt_duration_seconds_count %d\n", attemptCount)
	writeLatencyHistogram(output, "heimdall_request_latency_seconds",
		"Completed Gateway request latency distribution.", usageMetrics.RequestLatencyBuckets,
		usageMetrics.RequestLatencyMillis, requestCount)
	writeLatencyHistogram(output, "heimdall_attempt_latency_seconds",
		"Completed Provider attempt latency distribution.", usageMetrics.AttemptLatencyBuckets,
		usageMetrics.AttemptLatencyMillis, attemptCount)
	metricHeader(output, "heimdall_active_requests", "gauge", "Requests accepted but not finalized.")
	fmt.Fprintf(output, "heimdall_active_requests %d\n", usageMetrics.ActiveRequests)
	metricHeader(output, "heimdall_process_goroutines", "gauge", "Current goroutines in the Heimdall process.")
	fmt.Fprintf(output, "heimdall_process_goroutines %d\n", runtime.NumGoroutine())
	metricHeader(output, "go_goroutines", "gauge", "Number of goroutines that currently exist.")
	fmt.Fprintf(output, "go_goroutines %d\n", runtime.NumGoroutine())
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	metricHeader(output, "go_memstats_heap_alloc_bytes", "gauge", "Bytes of allocated heap objects.")
	fmt.Fprintf(output, "go_memstats_heap_alloc_bytes %d\n", memory.HeapAlloc)
	metricHeader(output, "go_memstats_gc_cycles_total", "counter", "Completed garbage collection cycles.")
	fmt.Fprintf(output, "go_memstats_gc_cycles_total %d\n", memory.NumGC)
	metricHeader(output, "process_start_time_seconds", "gauge", "Start time of the process since unix epoch in seconds.")
	fmt.Fprintf(output, "process_start_time_seconds %d\n", r.startedAt.Unix())
	build := buildinfo.Current()
	metricHeader(output, "heimdall_build_info", "gauge", "Heimdall build information.")
	fmt.Fprintf(output, "heimdall_build_info{version=%s,commit=%s} 1\n",
		strconv.Quote(build.Version), strconv.Quote(build.Commit))
	metricHeader(output, "heimdall_metrics_auth_failures_total", "counter", "Rejected Metrics authentication attempts.")
	fmt.Fprintf(output, "heimdall_metrics_auth_failures_total %d\n", r.metricsAuthFailed.Load())
	metricHeader(output, "heimdall_metrics_scrape_rejected_total", "counter", "Metrics scrapes rejected by the concurrency bound.")
	fmt.Fprintf(output, "heimdall_metrics_scrape_rejected_total %d\n", r.metricsBusy.Load())
	metricHeader(output, "heimdall_metrics_render_errors_total", "counter", "Metrics responses that failed while flushing to the client.")
	fmt.Fprintf(output, "heimdall_metrics_render_errors_total %d\n", r.metricsRenderErrs.Load())
	metricHeader(output, "heimdall_fallbacks_total", "counter", "Provider fallback transitions.")
	fmt.Fprintf(output, "heimdall_fallbacks_total %d\n", usageMetrics.Fallbacks)
	metricHeader(output, "heimdall_usage_queue_depth", "gauge", "Current durable Ledger append queue depth.")
	fmt.Fprintf(output, "heimdall_usage_queue_depth %d\n", ledgerStats.QueueDepth)
	metricHeader(output, "heimdall_usage_queue_capacity", "gauge", "Durable Ledger append queue capacity.")
	fmt.Fprintf(output, "heimdall_usage_queue_capacity %d\n", ledgerStats.QueueCapacity)
	metricHeader(output, "heimdall_wal_append_errors_total", "counter", "Ledger records failed during write or fsync.")
	fmt.Fprintf(output, "heimdall_wal_append_errors_total %d\n", ledgerStats.Errors)
	metricHeader(output, "heimdall_usage_analytics_queue_depth", "gauge", "Queued derivative Usage records awaiting aggregation.")
	fmt.Fprintf(output, "heimdall_usage_analytics_queue_depth %d\n", collectorStats.QueueDepth)
	metricHeader(output, "heimdall_usage_analytics_dropped_total", "counter", "Derivative Usage notifications dropped and recoverable from Ledger.")
	fmt.Fprintf(output, "heimdall_usage_analytics_dropped_total %d\n", collectorStats.Dropped)
	metricHeader(output, "heimdall_usage_analytics_lagging", "gauge", "Whether derivative Usage requires Ledger catch-up.")
	if collectorStats.Lagging {
		fmt.Fprintln(output, "heimdall_usage_analytics_lagging 1")
	} else {
		fmt.Fprintln(output, "heimdall_usage_analytics_lagging 0")
	}
	metricHeader(output, "heimdall_alert_delivery_total", "counter", "Alert delivery outcomes.")
	fmt.Fprintf(output, "heimdall_alert_delivery_total{status=\"delivered\"} %d\n", alertStats.Delivered)
	fmt.Fprintf(output, "heimdall_alert_delivery_total{status=\"failed\"} %d\n", alertStats.Failed)
	fmt.Fprintf(output, "heimdall_alert_delivery_total{status=\"dropped\"} %d\n", alertStats.Dropped)
	metricHeader(output, "heimdall_alert_queue_depth", "gauge", "Queued alert events.")
	fmt.Fprintf(output, "heimdall_alert_queue_depth %d\n", alertStats.Queued)
	metricHeader(output, "heimdall_token_guard_events_dropped_total", "counter", "Token Guard security events dropped before alert dispatch.")
	fmt.Fprintf(output, "heimdall_token_guard_events_dropped_total %d\n", r.tokenGuard.DroppedEvents())
	metricHeader(output, "heimdall_provider_up", "gauge", "Provider adapter loaded and available for passive routing.")
	for _, providerType := range r.providers.ProviderTypes() {
		fmt.Fprintf(output, "heimdall_provider_up{provider_type=%s} 1\n",
			strconv.Quote(providerType))
	}
	metricHeader(output, "heimdall_policy_rejections_total", "counter", "Gateway policy rejections by reason.")
	for _, item := range []struct {
		reason string
		value  uint64
	}{
		{"rpm", rejections.RPM},
		{"tpm", rejections.TPM},
		{"project_concurrency", rejections.ProjectConcurrency},
		{"provider_concurrency", rejections.ProviderConcurrency},
		{"deployment_concurrency", rejections.DeploymentConcurrency},
		{"budget", rejections.Budget},
		{"token_guard", rejections.TokenGuard},
	} {
		fmt.Fprintf(output, "heimdall_policy_rejections_total{reason=%s} %d\n",
			strconv.Quote(item.reason), item.value)
	}
	metricHeader(output, "heimdall_provider_active_requests", "gauge", "Current in-flight requests by Provider instance.")
	activeProviders := r.gatewayService.ActiveProviderRequests()
	providerIDs := r.providers.ProviderIDs()
	knownProviderIDs := make(map[string]struct{}, len(providerIDs))
	for _, providerID := range providerIDs {
		knownProviderIDs[providerID] = struct{}{}
	}
	for providerID := range activeProviders {
		if _, known := knownProviderIDs[providerID]; !known {
			providerIDs = append(providerIDs, providerID)
		}
	}
	slices.Sort(providerIDs)
	for _, providerID := range providerIDs {
		fmt.Fprintf(output, "heimdall_provider_active_requests{provider_id=%s} %d\n",
			strconv.Quote(providerID), activeProviders[providerID])
	}
	metricHeader(output, "heimdall_provider_concurrency_limit", "gauge", "Configured Provider concurrency limit; absent or zero means unlimited.")
	providerLimits := r.providers.ProviderConcurrencyLimits()
	for _, providerID := range providerIDs {
		limit := providerLimits[providerID]
		if limit > 0 {
			fmt.Fprintf(output, "heimdall_provider_concurrency_limit{provider_id=%s} %d\n",
				strconv.Quote(providerID), limit)
		}
	}
	metricHeader(output, "heimdall_deployment_active_requests", "gauge", "Current in-flight requests by Deployment.")
	activeDeployments := r.gatewayService.ActiveDeploymentRequests()
	deploymentIDs := r.providers.DeploymentIDs()
	knownDeploymentIDs := make(map[string]struct{}, len(deploymentIDs))
	for _, deploymentID := range deploymentIDs {
		knownDeploymentIDs[deploymentID] = struct{}{}
	}
	metricHeader(output, "heimdall_deployment_concurrency_limit", "gauge", "Configured Deployment concurrency limit; absent or zero means unlimited.")
	deploymentLimits := r.providers.DeploymentConcurrencyLimits()
	for _, deploymentID := range deploymentIDs {
		limit := deploymentLimits[deploymentID]
		if limit > 0 {
			fmt.Fprintf(output, "heimdall_deployment_concurrency_limit{deployment_id=%s} %d\n",
				strconv.Quote(deploymentID), limit)
		}
	}
	for deploymentID := range activeDeployments {
		if _, known := knownDeploymentIDs[deploymentID]; !known {
			deploymentIDs = append(deploymentIDs, deploymentID)
		}
	}
	slices.Sort(deploymentIDs)
	for _, deploymentID := range deploymentIDs {
		fmt.Fprintf(output, "heimdall_deployment_active_requests{deployment_id=%s} %d\n",
			strconv.Quote(deploymentID), activeDeployments[deploymentID])
	}
	metricHeader(output, "heimdall_deployment_up", "gauge", "Latest active-probe health by Deployment; absent means not yet probed.")
	for deploymentID, healthy := range r.providers.DeploymentHealth() {
		value := 0
		if healthy {
			value = 1
		}
		fmt.Fprintf(output, "heimdall_deployment_up{deployment_id=%s} %d\n",
			strconv.Quote(deploymentID), value)
	}
	return output.Flush()
}

func metricHeader(output *bufio.Writer, name, metricType, help string) {
	fmt.Fprintf(output, "# HELP %s %s\n", name, help)
	fmt.Fprintf(output, "# TYPE %s %s\n", name, metricType)
}

func writeLatencyHistogram(
	output *bufio.Writer,
	name string,
	help string,
	buckets [12]uint64,
	sumMillis uint64,
	count uint64,
) {
	metricHeader(output, name, "histogram", help)
	var cumulative uint64
	for index, bucketCount := range buckets {
		cumulative += bucketCount
		fmt.Fprintf(output, "%s_bucket{le=%s} %d\n", name,
			strconv.Quote(millisecondsSeconds(usage.LatencyBucketsMillis[index])), cumulative)
	}
	fmt.Fprintf(output, "%s_bucket{le=\"+Inf\"} %d\n", name, count)
	fmt.Fprintf(output, "%s_sum %s\n", name, millisecondsSeconds(sumMillis))
	fmt.Fprintf(output, "%s_count %d\n", name, count)
}

func microsUSD(value uint64) string {
	return fmt.Sprintf("%d.%06d", value/1_000_000, value%1_000_000)
}

func millisecondsSeconds(value uint64) string {
	whole := value / 1000
	fraction := strings.TrimRight(fmt.Sprintf("%03d", value%1000), "0")
	if fraction == "" {
		return strconv.FormatUint(whole, 10)
	}
	return fmt.Sprintf("%d.%s", whole, fraction)
}
