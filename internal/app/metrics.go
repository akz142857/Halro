package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
	"github.com/akz142857/Heimdall/internal/timezone"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/akz142857/Heimdall/internal/vault"
)

func MetricsToken(cfg config.Config) ([]byte, error) {
	if cfg.Metrics.CredentialFile != "" {
		return nil, errors.New("versioned metrics credentials are enabled; use metrics rotate")
	}
	masterKey, err := unlockMasterKey(context.Background(), cfg)
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
	pendingLeases, oldestPendingAge := r.state.PendingLeaseStats(time.Now())
	recoveryStats := r.accounting.RecoveryStats()
	pricingQuarantines, _ := r.store.PricingQuarantineCount(context.Background())
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
	// Deliberately unlabelled: the interesting dimension here is the source
	// address, and that is exactly the label that would make this series
	// unbounded — and would publish caller addresses through the metrics port.
	metricHeader(output, "heimdall_source_rate_limited_total", "counter",
		"Gateway requests shed by the per-source rate limiter.")
	fmt.Fprintf(output, "heimdall_source_rate_limited_total %d\n", r.sourceLimiter.Rejected())
	metricHeader(output, "heimdall_source_rate_limit_overflow_total", "counter",
		"Gateway requests charged to the shared budget because distinct sources outgrew the tracking ceiling.")
	fmt.Fprintf(output, "heimdall_source_rate_limit_overflow_total %d\n", r.sourceLimiter.Overflows())
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
	// Alert on disagreement across the fleet: nodes resolving different rules
	// place the same instant in different accounting periods.
	if database, err := timezone.Describe(r.config.Usage.Timezone); err == nil {
		metricHeader(output, "heimdall_tzdata_info", "gauge", "IANA time zone database this node resolves accounting periods against.")
		fmt.Fprintf(output, "heimdall_tzdata_info{source=%s,version=%s,fingerprint=%s} 1\n",
			strconv.Quote(database.Source), strconv.Quote(database.Version), strconv.Quote(database.Fingerprint))
	}
	// Every node in a fleet must agree on both, or the same instant lands in
	// different accounting periods on different nodes.
	settings := r.periods.Settings()
	metricHeader(output, "heimdall_accounting_timezone_version", "gauge", "Generation of the accounting timezone setting periods are derived from.")
	fmt.Fprintf(output, "heimdall_accounting_timezone_version %d\n", settings.TimezoneVersion)
	if period, err := r.periods.PeriodAt(time.Now()); err == nil {
		metricHeader(output, "heimdall_accounting_period_end_seconds", "gauge", "Unix time at which the accounting period in progress ends.")
		fmt.Fprintf(output, "heimdall_accounting_period_end_seconds %d\n", period.End.Unix())
	}
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
	metricHeader(output, "heimdall_accounting_pending_leases", "gauge", "Durable Accounting Leases awaiting recovery or settlement.")
	fmt.Fprintf(output, "heimdall_accounting_pending_leases %d\n", pendingLeases)
	metricHeader(output, "heimdall_accounting_oldest_pending_lease_age_seconds", "gauge", "Age of the oldest pending Accounting Lease.")
	fmt.Fprintf(output, "heimdall_accounting_oldest_pending_lease_age_seconds %.6f\n", oldestPendingAge.Seconds())
	metricHeader(output, "heimdall_accounting_recovery_total", "counter", "Accounting Lease startup recovery outcomes.")
	fmt.Fprintf(output, "heimdall_accounting_recovery_total{status=\"released_not_started\"} %d\n", recoveryStats.ReleasedNotStarted)
	fmt.Fprintf(output, "heimdall_accounting_recovery_total{status=\"conservatively_settled\"} %d\n", recoveryStats.ConservativelySettled)
	fmt.Fprintf(output, "heimdall_accounting_recovery_total{status=\"failed\"} %d\n", recoveryStats.Failures)
	metricHeader(output, "heimdall_pricing_quarantined_deployments", "gauge", "Deployments blocked by pricing clock or restore quarantine.")
	fmt.Fprintf(output, "heimdall_pricing_quarantined_deployments %d\n", pricingQuarantines)
	metricHeader(output, "heimdall_pricing_unknown_attempts_total", "counter", "Provider attempts settled without known price evidence.")
	fmt.Fprintf(output, "heimdall_pricing_unknown_attempts_total %d\n", usageMetrics.UnknownAttempts)
	metricHeader(output, "heimdall_cost_adjustments_total", "counter", "Append-only cost adjustment events by direction.")
	fmt.Fprintf(output, "heimdall_cost_adjustments_total{direction=\"positive\"} %d\n", usageMetrics.AdjustmentPositiveCount)
	fmt.Fprintf(output, "heimdall_cost_adjustments_total{direction=\"negative\"} %d\n", usageMetrics.AdjustmentNegativeCount)
	metricHeader(output, "heimdall_cost_adjustment_micros_usd_total", "counter", "Absolute cost adjustment value by direction in micro-USD.")
	fmt.Fprintf(output, "heimdall_cost_adjustment_micros_usd_total{direction=\"positive\"} %d\n", usageMetrics.AdjustmentPositiveMicrosUSD)
	fmt.Fprintf(output, "heimdall_cost_adjustment_micros_usd_total{direction=\"negative\"} %d\n", usageMetrics.AdjustmentNegativeMicrosUSD)
	pendingAdjustmentIntents, _ := r.store.ListPendingCostAdjustmentIntents(context.Background())
	metricHeader(output, "heimdall_pricing_recovery_pending_intents", "gauge", "Durable pricing and cost adjustment intents awaiting delivery.")
	fmt.Fprintf(output, "heimdall_pricing_recovery_pending_intents %d\n", len(pendingAdjustmentIntents))
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
	r.writeKMSMetrics(output)
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

func (r *Runtime) writeKMSMetrics(output *bufio.Writer) {
	snapshot := snapshotKMSMetrics()
	callKeys := make([]kmsCallMetricKey, 0, len(snapshot.Calls))
	for key := range snapshot.Calls {
		callKeys = append(callKeys, key)
	}
	sort.Slice(callKeys, func(left, right int) bool {
		if callKeys[left].Operation != callKeys[right].Operation {
			return callKeys[left].Operation < callKeys[right].Operation
		}
		if callKeys[left].Status != callKeys[right].Status {
			return callKeys[left].Status < callKeys[right].Status
		}
		return callKeys[left].ErrorClass < callKeys[right].ErrorClass
	})
	metricHeader(output, "heimdall_kms_calls_total", "counter", "Cloud KMS provider calls by bounded operation and outcome.")
	metricHeader(output, "heimdall_kms_call_duration_seconds", "summary", "Cloud KMS provider call duration by bounded operation and outcome.")
	for _, key := range callKeys {
		labels := fmt.Sprintf("operation=%s,status=%s,error_class=%s", strconv.Quote(string(key.Operation)), strconv.Quote(key.Status), strconv.Quote(key.ErrorClass))
		fmt.Fprintf(output, "heimdall_kms_calls_total{%s} %d\n", labels, snapshot.Calls[key])
		fmt.Fprintf(output, "heimdall_kms_call_duration_seconds_sum{%s} %s\n", labels, nanosecondsSeconds(snapshot.DurationNanos[key]))
		fmt.Fprintf(output, "heimdall_kms_call_duration_seconds_count{%s} %d\n", labels, snapshot.Calls[key])
	}
	unlocks := make([]kmsUnlockMetricKey, 0, len(snapshot.Unlocks))
	for key := range snapshot.Unlocks {
		unlocks = append(unlocks, key)
	}
	sort.Slice(unlocks, func(left, right int) bool {
		if unlocks[left].Purpose != unlocks[right].Purpose {
			return unlocks[left].Purpose < unlocks[right].Purpose
		}
		if unlocks[left].Status != unlocks[right].Status {
			return unlocks[left].Status < unlocks[right].Status
		}
		return unlocks[left].ErrorClass < unlocks[right].ErrorClass
	})
	metricHeader(output, "heimdall_kms_unlock_total", "counter", "Final Key Slot unlock outcomes; Recovery is always an explicit operator path.")
	for _, key := range unlocks {
		fmt.Fprintf(output, "heimdall_kms_unlock_total{purpose=%s,status=%s,error_class=%s} %d\n",
			strconv.Quote(string(key.Purpose)), strconv.Quote(key.Status), strconv.Quote(key.ErrorClass), snapshot.Unlocks[key])
	}
	metricHeader(output, "heimdall_kms_automatic_fallback_total", "counter", "Automatic Primary-to-Recovery fallbacks; invariantly zero because fallback is forbidden.")
	fmt.Fprintln(output, "heimdall_kms_automatic_fallback_total 0")
	metricHeader(output, "heimdall_kms_recovery_last_used_timestamp_seconds", "gauge", "UTC timestamp of the latest audited Recovery Slot use; zero means none recorded.")
	lastRecoveryUse := int64(0)
	if !r.kmsRecoveryLastUsed.IsZero() {
		lastRecoveryUse = r.kmsRecoveryLastUsed.Unix()
	}
	fmt.Fprintf(output, "heimdall_kms_recovery_last_used_timestamp_seconds %d\n", lastRecoveryUse)
	if r.config.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return
	}
	metricHeader(output, "heimdall_kms_descriptor_valid", "gauge", "Whether the persisted Key Slot descriptor is structurally valid.")
	metricHeader(output, "heimdall_kms_recovery_ready", "gauge", "Whether the configured Recovery Slot is active and independently verified.")
	metricHeader(output, "heimdall_kms_pending_rotation_slots", "gauge", "Number of pending Key Slots in the current descriptor generation.")
	metricHeader(output, "heimdall_kms_slot_state", "gauge", "Configured Key Slot state by bounded purpose and state.")
	metricHeader(output, "heimdall_kms_slot_verified_timestamp_seconds", "gauge", "UTC verification timestamp for configured active Key Slots.")
	descriptor, err := r.store.KeySlotDescriptor(context.Background())
	if err != nil || descriptor.Validate() != nil {
		fmt.Fprintln(output, "heimdall_kms_descriptor_valid 0")
		fmt.Fprintln(output, "heimdall_kms_recovery_ready 0")
		fmt.Fprintln(output, "heimdall_kms_pending_rotation_slots 0")
		return
	}
	fmt.Fprintln(output, "heimdall_kms_descriptor_valid 1")
	pending := 0
	recoveryReady := false
	for _, slot := range descriptor.Slots {
		if slot.State == masterkey.KeySlotPending {
			pending++
		}
		expectedID := r.config.Storage.MasterKey.PrimarySlot
		if slot.Purpose == masterkey.KeySlotRecovery {
			expectedID = r.config.Storage.MasterKey.RecoverySlot
		}
		if slot.ID != expectedID || (slot.Purpose != masterkey.KeySlotPrimary && slot.Purpose != masterkey.KeySlotRecovery) {
			continue
		}
		fmt.Fprintf(output, "heimdall_kms_slot_state{purpose=%s,state=%s} 1\n", strconv.Quote(string(slot.Purpose)), strconv.Quote(string(slot.State)))
		if slot.VerifiedAt != nil {
			fmt.Fprintf(output, "heimdall_kms_slot_verified_timestamp_seconds{purpose=%s} %d\n", strconv.Quote(string(slot.Purpose)), slot.VerifiedAt.Unix())
		}
		if slot.Purpose == masterkey.KeySlotRecovery && slot.State == masterkey.KeySlotActive && slot.VerifiedAt != nil {
			recoveryReady = true
		}
	}
	fmt.Fprintf(output, "heimdall_kms_pending_rotation_slots %d\n", pending)
	if recoveryReady {
		fmt.Fprintln(output, "heimdall_kms_recovery_ready 1")
	} else {
		fmt.Fprintln(output, "heimdall_kms_recovery_ready 0")
	}
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

func nanosecondsSeconds(value uint64) string {
	return strconv.FormatFloat(float64(value)/float64(time.Second), 'f', 9, 64)
}
