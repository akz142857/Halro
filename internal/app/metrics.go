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

	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/timezone"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
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

func (r *Runtime) writeMetrics(ctx context.Context, writer http.ResponseWriter) error {
	usageMetrics := r.usage.Metrics()
	ledgerStats := r.ledger.Stats()
	collectorStats := r.usageCollector.Stats()
	alertStats := r.alerts.Stats()
	rejections := r.gatewayService.RejectionMetrics()
	pendingLeases, oldestPendingAge := r.state.PendingLeaseStats(time.Now())
	recoveryStats := r.accounting.RecoveryStats()
	projectLockStats := r.accounting.ProjectLockStats()
	metadataWriteStats := r.store.MetadataWriteStats()
	pricingQuarantines, _ := r.store.PricingQuarantineCount(ctx)
	// Read before the status line is written, and treated like the capability
	// gauges below: a failed read omits its own series rather than failing the
	// whole exposition. Returning an error here would abort the render after
	// net/http has already implied 200, so the scrape would succeed with an
	// empty body — `up` stays 1 while every series vanishes, including
	// halro_activation_stale. That silences the configuration-stale alert in
	// exactly the failure it exists to catch, because an unreadable metadata
	// store is one of the ways a snapshot goes stale.
	shutdownTruncatedAttempts, shutdownCounterErr := r.store.ShutdownTruncatedAttempts()
	if shutdownCounterErr != nil {
		r.logger.Warn("shutdown-truncated Provider attempt counter unavailable",
			"error", shutdownCounterErr)
	}
	activation := r.activation.status()
	// Current state, so it is read rather than tracked: a count that drifts
	// from the records it describes is worse than one that costs a read.
	//
	// A failed read must not be rendered as zeros. These gauges exist so that
	// `drifted == 0` is assertable, and a store error that silently produces
	// exactly that reading turns the alert into a check on whether the read
	// worked. Omitting the series makes the scrape stale instead, which is what
	// an absent answer actually is.
	storedDeployments, deploymentsErr := r.store.ListDeployments(ctx)
	capabilityGauges, capabilityGaugesReadable := summariseDeploymentCapabilities(storedDeployments), deploymentsErr == nil
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	output := bufio.NewWriter(writer)

	metricHeader(output, "halro_requests_total", "counter", "Completed Gateway requests.")
	fmt.Fprintf(output, "halro_requests_total{status=\"success\"} %d\n", usageMetrics.RequestsSuccess)
	fmt.Fprintf(output, "halro_requests_total{status=\"error\"} %d\n", usageMetrics.RequestsError)
	metricHeader(output, "halro_attempts_total", "counter", "Completed Provider attempts.")
	fmt.Fprintf(output, "halro_attempts_total{status=\"success\"} %d\n", usageMetrics.AttemptsSuccess)
	fmt.Fprintf(output, "halro_attempts_total{status=\"error\"} %d\n", usageMetrics.AttemptsError)
	metricHeader(output, "halro_tokens_total", "counter", "Provider tokens accounted by direction.")
	fmt.Fprintf(output, "halro_tokens_total{direction=\"input\"} %d\n", usageMetrics.InputTokens)
	fmt.Fprintf(output, "halro_tokens_total{direction=\"output\"} %d\n", usageMetrics.OutputTokens)
	metricHeader(output, "halro_cost_usd_total", "counter", "Provider cost accounted in USD.")
	fmt.Fprintf(output, "halro_cost_usd_total %s\n", microsUSD(usageMetrics.CostMicrosUSD))
	metricHeader(output, "halro_request_duration_seconds", "summary", "Completed Gateway request duration.")
	requestCount := usageMetrics.RequestsSuccess + usageMetrics.RequestsError
	fmt.Fprintf(output, "halro_request_duration_seconds_sum %s\n",
		millisecondsSeconds(usageMetrics.RequestLatencyMillis))
	fmt.Fprintf(output, "halro_request_duration_seconds_count %d\n", requestCount)
	metricHeader(output, "halro_attempt_duration_seconds", "summary", "Completed Provider attempt duration.")
	attemptCount := usageMetrics.AttemptsSuccess + usageMetrics.AttemptsError
	fmt.Fprintf(output, "halro_attempt_duration_seconds_sum %s\n",
		millisecondsSeconds(usageMetrics.AttemptLatencyMillis))
	fmt.Fprintf(output, "halro_attempt_duration_seconds_count %d\n", attemptCount)
	writeLatencyHistogram(output, "halro_request_latency_seconds",
		"Completed Gateway request latency distribution.", usageMetrics.RequestLatencyBuckets,
		usageMetrics.RequestLatencyMillis, requestCount)
	writeLatencyHistogram(output, "halro_attempt_latency_seconds",
		"Completed Provider attempt latency distribution.", usageMetrics.AttemptLatencyBuckets,
		usageMetrics.AttemptLatencyMillis, attemptCount)
	writeCapabilityMetrics(output, r.capabilityMetrics.snapshot(), capabilityGauges, capabilityGaugesReadable)
	metricHeader(output, "halro_activation_stale", "gauge", "Whether any live configuration snapshot is known to be behind the durable store.")
	if activation.Stale {
		fmt.Fprintln(output, "halro_activation_stale 1")
	} else {
		fmt.Fprintln(output, "halro_activation_stale 0")
	}
	staleSeconds := 0.0
	if activation.Stale && !activation.StaleSince.IsZero() {
		staleSeconds = time.Since(activation.StaleSince).Seconds()
		if staleSeconds < 0 {
			staleSeconds = 0
		}
	}
	metricHeader(output, "halro_activation_stale_seconds", "gauge", "Seconds since the oldest live configuration snapshot became stale, or 0 when current.")
	fmt.Fprintf(output, "halro_activation_stale_seconds %.6f\n", staleSeconds)
	metricHeader(output, "halro_active_requests", "gauge", "Requests accepted but not finalized.")
	fmt.Fprintf(output, "halro_active_requests %d\n", usageMetrics.ActiveRequests)
	// The console window's working set and its lower edge. Together they answer
	// the question the window exists for: is the in-memory history bounded, or
	// is it still growing? A resident count that keeps climbing while the floor
	// stays put means the trim is not running — which is what a stalled export
	// looks like from the outside, since the trim is bounded by it.
	// The deferred tier, which is otherwise invisible between its two visible
	// ends. An expiry writes no ledger event and its record is reaped within the
	// hour, so a submission that waited out its TTL leaves nothing to look at;
	// an interruption is the restart case, which may already have been billed.
	// Both are counters an alert can watch instead of a log an operator has to
	// think to read.
	deferred := r.gatewayService.DeferredMetrics()
	metricHeader(output, "halro_deferred_responses_running", "gauge",
		"Deferred submissions this process is executing right now.")
	fmt.Fprintf(output, "halro_deferred_responses_running %d\n", deferred.Running)
	metricHeader(output, "halro_deferred_responses_submitted_total", "counter",
		"Deferred submissions accepted since start.")
	fmt.Fprintf(output, "halro_deferred_responses_submitted_total %d\n", deferred.Submitted)
	metricHeader(output, "halro_deferred_responses_expired_total", "counter",
		"Deferred submissions that reached their TTL without being executed.")
	fmt.Fprintf(output, "halro_deferred_responses_expired_total %d\n", deferred.Expired)
	metricHeader(output, "halro_deferred_responses_interrupted_total", "counter",
		"Deferred submissions failed at startup because the process died while they were running.")
	fmt.Fprintf(output, "halro_deferred_responses_interrupted_total %d\n", deferred.Interrupted)
	// The capture day's budget. Past the ceiling, real failures stop being
	// captured and the process log says so exactly once — which is the wrong
	// shape for something an operator needs to notice while it is happening.
	capture := r.failureCapture.Saturation()
	metricHeader(output, "halro_failure_captures_today", "gauge",
		"Failure payloads captured so far in the current day.")
	fmt.Fprintf(output, "halro_failure_captures_today %d\n", capture.Captured)
	metricHeader(output, "halro_failure_capture_day_limit", "gauge",
		"Failure payloads the current day may hold; 0 when capture is disabled.")
	fmt.Fprintf(output, "halro_failure_capture_day_limit %d\n", capture.DayLimit)
	metricHeader(output, "halro_failure_capture_saturated", "gauge",
		"1 when the current day's capture budget has been reached and failures are no longer captured.")
	fmt.Fprintf(output, "halro_failure_capture_saturated %d\n", boolMetric(capture.Saturated))
	windowed := r.usage.Windowed()
	metricHeader(output, "halro_usage_window_attempts", "gauge",
		"Attempts resident in the usage aggregate the console reads.")
	fmt.Fprintf(output, "halro_usage_window_attempts %d\n", windowed.Attempts)
	metricHeader(output, "halro_usage_window_requests", "gauge",
		"Request summaries resident in the usage aggregate the console reads.")
	fmt.Fprintf(output, "halro_usage_window_requests %d\n", windowed.Summaries)
	metricHeader(output, "halro_usage_window_floor_sequence", "gauge",
		"Lowest ledger sequence the usage aggregate still holds; 0 when it has never been trimmed.")
	fmt.Fprintf(output, "halro_usage_window_floor_sequence %d\n", windowed.Floor)
	metricHeader(output, "halro_usage_window_trimmed_total", "counter",
		"Attempts removed from the usage aggregate by the console window.")
	fmt.Fprintf(output, "halro_usage_window_trimmed_total %d\n", windowed.TrimmedAttempts)
	// What the stored checkpoint costs, which is a different question from what
	// the window holds. The bytes gauge is the whole checkpoint; the last-write
	// gauge is what one tick actually rewrote. The gap between them is the
	// point of the segmented format, and the two converging would mean it has
	// stopped working — a tick rewriting the window again.
	checkpointed := r.usage.Checkpointed()
	metricHeader(output, "halro_usage_checkpoint_segments", "gauge",
		"Segments holding the stored usage checkpoint.")
	fmt.Fprintf(output, "halro_usage_checkpoint_segments %d\n", checkpointed.Segments)
	metricHeader(output, "halro_usage_checkpoint_bytes", "gauge",
		"Bytes the stored usage checkpoint occupies across all its segments.")
	fmt.Fprintf(output, "halro_usage_checkpoint_bytes %d\n", checkpointed.Bytes)
	metricHeader(output, "halro_usage_checkpoint_open_segment_bytes", "gauge",
		"Bytes in the one segment a checkpoint tick rewrites.")
	fmt.Fprintf(output, "halro_usage_checkpoint_open_segment_bytes %d\n", checkpointed.OpenSegmentBytes)
	// The WAL's own shape. Active bytes is what sealing bounds; sealed bytes is
	// what it moved out of the way and is still keeping. Read together they say
	// whether the growth an operator is watching is in the file being written
	// or in the archive behind it — two very different problems, and before
	// sealing there was no way to tell them apart because there was only one
	// number.
	sealed := r.ledger.Segments()
	var sealedBytes, sealedStored int64
	for _, segment := range sealed {
		sealedBytes += segment.Length
		sealedStored += segment.StoredLength
	}
	metricHeader(output, "halro_ledger_active_bytes", "gauge",
		"Bytes in the ledger generation currently being appended to.")
	fmt.Fprintf(output, "halro_ledger_active_bytes %d\n", r.ledger.ActiveBytes())
	metricHeader(output, "halro_ledger_sealed_generations", "gauge",
		"Sealed ledger generations this data directory holds.")
	fmt.Fprintf(output, "halro_ledger_sealed_generations %d\n", len(sealed))
	metricHeader(output, "halro_ledger_sealed_bytes", "gauge",
		"Frame bytes held in sealed ledger generations, before compression.")
	fmt.Fprintf(output, "halro_ledger_sealed_bytes %d\n", sealedBytes)
	metricHeader(output, "halro_ledger_sealed_stored_bytes", "gauge",
		"Disk bytes the sealed ledger generations occupy, after compression.")
	fmt.Fprintf(output, "halro_ledger_sealed_stored_bytes %d\n", sealedStored)
	// Deliberately unlabelled: the interesting dimension here is the source
	// address, and that is exactly the label that would make this series
	// unbounded — and would publish caller addresses through the metrics port.
	metricHeader(output, "halro_source_rate_limited_total", "counter",
		"Gateway requests shed by the per-source rate limiter.")
	fmt.Fprintf(output, "halro_source_rate_limited_total %d\n", r.sourceLimiter.Rejected())
	metricHeader(output, "halro_source_rate_limit_overflow_total", "counter",
		"Gateway requests charged to the shared budget because distinct sources outgrew the tracking ceiling.")
	fmt.Fprintf(output, "halro_source_rate_limit_overflow_total %d\n", r.sourceLimiter.Overflows())
	metricHeader(output, "halro_process_goroutines", "gauge", "Current goroutines in the Halro process.")
	fmt.Fprintf(output, "halro_process_goroutines %d\n", runtime.NumGoroutine())
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
	metricHeader(output, "halro_build_info", "gauge", "Halro build information.")
	fmt.Fprintf(output, "halro_build_info{version=%s,commit=%s} 1\n",
		strconv.Quote(build.Version), strconv.Quote(build.Commit))
	// Alert on disagreement across the fleet: nodes resolving different rules
	// place the same instant in different accounting periods.
	if database, err := timezone.Describe(r.config.Usage.Timezone); err == nil {
		metricHeader(output, "halro_tzdata_info", "gauge", "IANA time zone database this node resolves accounting periods against.")
		fmt.Fprintf(output, "halro_tzdata_info{source=%s,version=%s,fingerprint=%s} 1\n",
			strconv.Quote(database.Source), strconv.Quote(database.Version), strconv.Quote(database.Fingerprint))
	}
	// Every node in a fleet must agree on both, or the same instant lands in
	// different accounting periods on different nodes.
	settings := r.periods.Settings()
	metricHeader(output, "halro_accounting_timezone_version", "gauge", "Generation of the accounting timezone setting periods are derived from.")
	fmt.Fprintf(output, "halro_accounting_timezone_version %d\n", settings.TimezoneVersion)
	if period, err := r.periods.PeriodAt(time.Now()); err == nil {
		metricHeader(output, "halro_accounting_period_end_seconds", "gauge", "Unix time at which the accounting period in progress ends.")
		fmt.Fprintf(output, "halro_accounting_period_end_seconds %d\n", period.End.Unix())
	}
	metricHeader(output, "halro_metrics_auth_failures_total", "counter", "Rejected Metrics authentication attempts.")
	fmt.Fprintf(output, "halro_metrics_auth_failures_total %d\n", r.metricsAuthFailed.Load())
	metricHeader(output, "halro_metrics_scrape_rejected_total", "counter", "Metrics scrapes rejected by the concurrency bound.")
	fmt.Fprintf(output, "halro_metrics_scrape_rejected_total %d\n", r.metricsBusy.Load())
	metricHeader(output, "halro_metrics_render_errors_total", "counter", "Metrics responses that failed while flushing to the client.")
	fmt.Fprintf(output, "halro_metrics_render_errors_total %d\n", r.metricsRenderErrs.Load())
	metricHeader(output, "halro_shutdown_truncated_attempts_total", "counter", "Provider attempts still active when a graceful-shutdown budget expired and forced connection close began.")
	if shutdownCounterErr == nil {
		fmt.Fprintf(output, "halro_shutdown_truncated_attempts_total %d\n", shutdownTruncatedAttempts)
	}
	if r.config.Audit.Anchor.Enabled {
		// Emission is fail-open, so silence is what a broken witness looks
		// like. Alert on staleness against Audit.Anchor.Interval rather than
		// on the failure counter alone: an anchor that stopped being emitted
		// at all increments nothing.
		metricHeader(output, "halro_audit_anchor_last_emit_timestamp_seconds", "gauge", "Unix time of the most recent audit anchor emission, or 0 if none has been emitted.")
		fmt.Fprintf(output, "halro_audit_anchor_last_emit_timestamp_seconds %d\n", r.anchorLastEmitUnix.Load())
		metricHeader(output, "halro_audit_anchor_interval_seconds", "gauge", "Configured maximum interval between audit anchor emissions.")
		fmt.Fprintf(output, "halro_audit_anchor_interval_seconds %.6f\n", r.config.Audit.Anchor.Interval.Value().Seconds())
		metricHeader(output, "halro_audit_anchor_emit_failures_total", "counter", "Audit anchor emissions that failed and were dropped.")
		fmt.Fprintf(output, "halro_audit_anchor_emit_failures_total %d\n", r.anchorEmitFailures.Load())
		metricHeader(output, "halro_audit_anchor_auth_failures_total", "counter", "Rejected audit anchor authentication attempts.")
		fmt.Fprintf(output, "halro_audit_anchor_auth_failures_total %d\n", r.anchorAuthFailed.Load())
	}
	metricHeader(output, "halro_fallbacks_total", "counter", "Provider fallback transitions.")
	fmt.Fprintf(output, "halro_fallbacks_total %d\n", usageMetrics.Fallbacks)
	metricHeader(output, "halro_usage_queue_depth", "gauge", "Current durable Ledger append queue depth.")
	fmt.Fprintf(output, "halro_usage_queue_depth %d\n", ledgerStats.QueueDepth)
	metricHeader(output, "halro_usage_queue_capacity", "gauge", "Durable Ledger append queue capacity.")
	fmt.Fprintf(output, "halro_usage_queue_capacity %d\n", ledgerStats.QueueCapacity)
	metricHeader(output, "halro_accounting_pending_leases", "gauge", "Durable Accounting Leases awaiting recovery or settlement.")
	fmt.Fprintf(output, "halro_accounting_pending_leases %d\n", pendingLeases)
	metricHeader(output, "halro_accounting_oldest_pending_lease_age_seconds", "gauge", "Age of the oldest pending Accounting Lease.")
	fmt.Fprintf(output, "halro_accounting_oldest_pending_lease_age_seconds %.6f\n", oldestPendingAge.Seconds())
	metricHeader(output, "halro_accounting_recovery_total", "counter", "Accounting Lease startup recovery outcomes.")
	fmt.Fprintf(output, "halro_accounting_recovery_total{status=\"released_not_started\"} %d\n", recoveryStats.ReleasedNotStarted)
	fmt.Fprintf(output, "halro_accounting_recovery_total{status=\"conservatively_settled\"} %d\n", recoveryStats.ConservativelySettled)
	fmt.Fprintf(output, "halro_accounting_recovery_total{status=\"failed\"} %d\n", recoveryStats.Failures)
	metricHeader(output, "halro_pricing_quarantined_deployments", "gauge", "Deployments blocked by pricing clock or restore quarantine.")
	fmt.Fprintf(output, "halro_pricing_quarantined_deployments %d\n", pricingQuarantines)
	metricHeader(output, "halro_pricing_unknown_attempts_total", "counter", "Provider attempts settled without known price evidence.")
	fmt.Fprintf(output, "halro_pricing_unknown_attempts_total %d\n", usageMetrics.UnknownAttempts)
	pendingPricingIntents, _ := r.store.ListPendingPricingAuditIntents(context.Background())
	metricHeader(output, "halro_pricing_recovery_pending_intents", "gauge", "Durable pricing intents awaiting delivery.")
	fmt.Fprintf(output, "halro_pricing_recovery_pending_intents %d\n", len(pendingPricingIntents))
	metricHeader(output, "halro_wal_append_errors_total", "counter", "Ledger records failed during write or fsync.")
	fmt.Fprintf(output, "halro_wal_append_errors_total %d\n", ledgerStats.Errors)
	// Records divided by batches is the mean group-commit size. It is the signal
	// that separates "this host's fsync is the ceiling" from "too few concurrent
	// appenders to coalesce" — identical symptoms, opposite remedies.
	metricHeader(output, "halro_wal_append_records_total", "counter", "Ledger records durably appended.")
	fmt.Fprintf(output, "halro_wal_append_records_total %d\n", ledgerStats.Records)
	metricHeader(output, "halro_wal_append_batches_total", "counter", "Ledger group-commit batches durably appended.")
	fmt.Fprintf(output, "halro_wal_append_batches_total %d\n", ledgerStats.Batches)
	// The durability barrier every accounting ceiling in this process is bounded
	// by. Its cost differs by orders of magnitude between filesystems, so no
	// capacity figure measured elsewhere transfers without it.
	metricHeader(output, "halro_wal_sync_seconds", "summary", "Cumulative time spent in Ledger durability barriers.")
	fmt.Fprintf(output, "halro_wal_sync_seconds_sum %.6f\n", ledgerStats.SyncDuration.Seconds())
	fmt.Fprintf(output, "halro_wal_sync_seconds_count %d\n", ledgerStats.Syncs)
	// Per-project accounting serialization: the write path holds this lock across
	// the durable append, which is the per-project request-rate ceiling (ADR
	// 0018). Aggregate on purpose — project count is unbounded, so it cannot be a
	// label.
	metricHeader(output, "halro_accounting_project_lock_acquisitions_total", "counter", "Per-project accounting lock acquisitions.")
	fmt.Fprintf(output, "halro_accounting_project_lock_acquisitions_total %d\n", projectLockStats.Acquisitions)
	metricHeader(output, "halro_accounting_project_lock_wait_seconds", "summary", "Cumulative time waiting for the per-project accounting lock.")
	fmt.Fprintf(output, "halro_accounting_project_lock_wait_seconds_sum %.6f\n", projectLockStats.WaitDuration.Seconds())
	fmt.Fprintf(output, "halro_accounting_project_lock_wait_seconds_count %d\n", projectLockStats.Acquisitions)
	metricHeader(output, "halro_accounting_project_lock_held_seconds", "summary", "Cumulative time holding the per-project accounting lock.")
	fmt.Fprintf(output, "halro_accounting_project_lock_held_seconds_sum %.6f\n", projectLockStats.HeldDuration.Seconds())
	fmt.Fprintf(output, "halro_accounting_project_lock_held_seconds_count %d\n", projectLockStats.Acquisitions)
	// Metadata store durable writes. Calls divided by transactions is the
	// coalescing factor for the batched price pin writes.
	metricHeader(output, "halro_metadata_batch_calls_total", "counter", "Batched metadata write calls.")
	fmt.Fprintf(output, "halro_metadata_batch_calls_total %d\n", metadataWriteStats.BatchCalls)
	metricHeader(output, "halro_metadata_batch_transactions_total", "counter", "Write transactions those batched calls coalesced into.")
	fmt.Fprintf(output, "halro_metadata_batch_transactions_total %d\n", metadataWriteStats.BatchTransactions)
	metricHeader(output, "halro_metadata_page_writes_total", "counter", "Metadata store page writes.")
	fmt.Fprintf(output, "halro_metadata_page_writes_total %d\n", metadataWriteStats.PageWrites)
	metricHeader(output, "halro_metadata_page_write_seconds_total", "counter", "Cumulative metadata store page write time.")
	fmt.Fprintf(output, "halro_metadata_page_write_seconds_total %.6f\n", metadataWriteStats.PageWriteDuration.Seconds())
	metricHeader(output, "halro_metadata_free_pages", "gauge", "Metadata store freelist pages available for reuse.")
	fmt.Fprintf(output, "halro_metadata_free_pages %d\n", metadataWriteStats.FreePages)
	metricHeader(output, "halro_metadata_pending_pages", "gauge", "Metadata store pages pending release to the freelist.")
	fmt.Fprintf(output, "halro_metadata_pending_pages %d\n", metadataWriteStats.PendingPages)
	metricHeader(output, "halro_usage_analytics_queue_depth", "gauge", "Queued derivative Usage records awaiting aggregation.")
	fmt.Fprintf(output, "halro_usage_analytics_queue_depth %d\n", collectorStats.QueueDepth)
	metricHeader(output, "halro_usage_analytics_dropped_total", "counter", "Derivative Usage notifications dropped and recoverable from Ledger.")
	fmt.Fprintf(output, "halro_usage_analytics_dropped_total %d\n", collectorStats.Dropped)
	metricHeader(output, "halro_usage_analytics_lagging", "gauge", "Whether derivative Usage requires Ledger catch-up.")
	if collectorStats.Lagging {
		fmt.Fprintln(output, "halro_usage_analytics_lagging 1")
	} else {
		fmt.Fprintln(output, "halro_usage_analytics_lagging 0")
	}
	metricHeader(output, "halro_alert_delivery_total", "counter", "Alert delivery outcomes.")
	fmt.Fprintf(output, "halro_alert_delivery_total{status=\"delivered\"} %d\n", alertStats.Delivered)
	fmt.Fprintf(output, "halro_alert_delivery_total{status=\"failed\"} %d\n", alertStats.Failed)
	fmt.Fprintf(output, "halro_alert_delivery_total{status=\"dropped\"} %d\n", alertStats.Dropped)
	metricHeader(output, "halro_alert_queue_depth", "gauge", "Queued alert events.")
	fmt.Fprintf(output, "halro_alert_queue_depth %d\n", alertStats.Queued)
	metricHeader(output, "halro_token_guard_events_dropped_total", "counter", "Token Guard security events dropped before alert dispatch.")
	fmt.Fprintf(output, "halro_token_guard_events_dropped_total %d\n", r.tokenGuard.DroppedEvents())
	r.writeKMSMetrics(output)
	metricHeader(output, "halro_provider_up", "gauge", "Provider adapter loaded and available for passive routing.")
	for _, providerType := range r.providers.ProviderTypes() {
		fmt.Fprintf(output, "halro_provider_up{provider_type=%s} 1\n",
			strconv.Quote(providerType))
	}
	metricHeader(output, "halro_policy_rejections_total", "counter", "Gateway policy rejections by reason.")
	for _, item := range []struct {
		reason string
		value  uint64
	}{
		{"route_capability", rejections.RouteCapability},
		{"rpm", rejections.RPM},
		{"tpm", rejections.TPM},
		{"project_concurrency", rejections.ProjectConcurrency},
		{"provider_concurrency", rejections.ProviderConcurrency},
		{"deployment_concurrency", rejections.DeploymentConcurrency},
		{"budget", rejections.Budget},
		{"token_guard", rejections.TokenGuard},
	} {
		fmt.Fprintf(output, "halro_policy_rejections_total{reason=%s} %d\n",
			strconv.Quote(item.reason), item.value)
	}
	metricHeader(output, "halro_provider_active_requests", "gauge", "Current in-flight requests by Provider instance.")
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
		fmt.Fprintf(output, "halro_provider_active_requests{provider_id=%s} %d\n",
			strconv.Quote(providerID), activeProviders[providerID])
	}
	metricHeader(output, "halro_provider_concurrency_limit", "gauge", "Configured Provider concurrency limit; absent or zero means unlimited.")
	providerLimits := r.providers.ProviderConcurrencyLimits()
	for _, providerID := range providerIDs {
		limit := providerLimits[providerID]
		if limit > 0 {
			fmt.Fprintf(output, "halro_provider_concurrency_limit{provider_id=%s} %d\n",
				strconv.Quote(providerID), limit)
		}
	}
	metricHeader(output, "halro_deployment_active_requests", "gauge", "Current in-flight requests by Deployment.")
	activeDeployments := r.gatewayService.ActiveDeploymentRequests()
	deploymentIDs := r.providers.DeploymentIDs()
	knownDeploymentIDs := make(map[string]struct{}, len(deploymentIDs))
	for _, deploymentID := range deploymentIDs {
		knownDeploymentIDs[deploymentID] = struct{}{}
	}
	metricHeader(output, "halro_deployment_concurrency_limit", "gauge", "Configured Deployment concurrency limit; absent or zero means unlimited.")
	deploymentLimits := r.providers.DeploymentConcurrencyLimits()
	for _, deploymentID := range deploymentIDs {
		limit := deploymentLimits[deploymentID]
		if limit > 0 {
			fmt.Fprintf(output, "halro_deployment_concurrency_limit{deployment_id=%s} %d\n",
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
		fmt.Fprintf(output, "halro_deployment_active_requests{deployment_id=%s} %d\n",
			strconv.Quote(deploymentID), activeDeployments[deploymentID])
	}
	metricHeader(output, "halro_deployment_up", "gauge", "Latest active-probe health by Deployment; absent means not yet probed.")
	for deploymentID, probe := range r.providers.DeploymentProbes() {
		value := 0
		if probe.Healthy {
			value = 1
		}
		fmt.Fprintf(output, "halro_deployment_up{deployment_id=%s} %d\n",
			strconv.Quote(deploymentID), value)
	}
	r.writeReloadMetrics(output)
	return output.Flush()
}

// writeReloadMetrics exposes certificate lifetimes and what the last SIGHUP
// did. Both series carry only label values that come from configuration or from
// the certificates themselves: a label taken from a handshake's ServerName
// would be chosen by whoever dialled the port, which is exactly the unbounded
// cardinality this exposition must not have.
func (r *Runtime) writeReloadMetrics(output *bufio.Writer) {
	metricHeader(output, "halro_tls_certificate_expiry_seconds", "gauge",
		"Unix time at which a served TLS certificate stops being valid.")
	if r.reload.serving != nil {
		writeCertificateExpiry(output, "serving", r.reload.serving.describe())
	}
	if r.reload.metricsTLS != nil {
		writeCertificateExpiry(output, "metrics", r.reload.metricsTLS.describe())
	}

	reloads := r.reload.state.snapshot()
	metricHeader(output, "halro_reload_total", "counter", "SIGHUP reload outcomes by item.")
	for _, item := range reloadItems {
		state := reloads[item]
		// status rather than result, to match halro_requests_total and every
		// other outcome series in this exposition.
		fmt.Fprintf(output, "halro_reload_total{item=%s,status=\"success\"} %d\n", strconv.Quote(item), state.success)
		fmt.Fprintf(output, "halro_reload_total{item=%s,status=\"error\"} %d\n", strconv.Quote(item), state.failure)
	}
	metricHeader(output, "halro_reload_last_success_timestamp_seconds", "gauge",
		"Unix time of the last successful reload of an item; absent until one has succeeded.")
	for _, item := range reloadItems {
		if state := reloads[item]; state.lastSuccessUnix > 0 {
			fmt.Fprintf(output, "halro_reload_last_success_timestamp_seconds{item=%s} %d\n",
				strconv.Quote(item), state.lastSuccessUnix)
		}
	}
}

func writeCertificateExpiry(output *bufio.Writer, scope string, descriptions []certificateDescription) {
	for _, description := range descriptions {
		fmt.Fprintf(output, "halro_tls_certificate_expiry_seconds{scope=%s,name=%s} %d\n",
			strconv.Quote(scope), strconv.Quote(description.Name), description.NotAfter.Unix())
	}
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
	metricHeader(output, "halro_kms_calls_total", "counter", "Cloud KMS provider calls by bounded operation and outcome.")
	metricHeader(output, "halro_kms_call_duration_seconds", "summary", "Cloud KMS provider call duration by bounded operation and outcome.")
	for _, key := range callKeys {
		labels := fmt.Sprintf("operation=%s,status=%s,error_class=%s", strconv.Quote(string(key.Operation)), strconv.Quote(key.Status), strconv.Quote(key.ErrorClass))
		fmt.Fprintf(output, "halro_kms_calls_total{%s} %d\n", labels, snapshot.Calls[key])
		fmt.Fprintf(output, "halro_kms_call_duration_seconds_sum{%s} %s\n", labels, nanosecondsSeconds(snapshot.DurationNanos[key]))
		fmt.Fprintf(output, "halro_kms_call_duration_seconds_count{%s} %d\n", labels, snapshot.Calls[key])
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
	metricHeader(output, "halro_kms_unlock_total", "counter", "Final Key Slot unlock outcomes; Recovery is always an explicit operator path.")
	for _, key := range unlocks {
		fmt.Fprintf(output, "halro_kms_unlock_total{purpose=%s,status=%s,error_class=%s} %d\n",
			strconv.Quote(string(key.Purpose)), strconv.Quote(key.Status), strconv.Quote(key.ErrorClass), snapshot.Unlocks[key])
	}
	metricHeader(output, "halro_kms_automatic_fallback_total", "counter", "Automatic Primary-to-Recovery fallbacks; invariantly zero because fallback is forbidden.")
	fmt.Fprintln(output, "halro_kms_automatic_fallback_total 0")
	metricHeader(output, "halro_kms_recovery_last_used_timestamp_seconds", "gauge", "UTC timestamp of the latest audited Recovery Slot use; zero means none recorded.")
	lastRecoveryUse := int64(0)
	if !r.kmsRecoveryLastUsed.IsZero() {
		lastRecoveryUse = r.kmsRecoveryLastUsed.Unix()
	}
	fmt.Fprintf(output, "halro_kms_recovery_last_used_timestamp_seconds %d\n", lastRecoveryUse)
	if r.config.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return
	}
	metricHeader(output, "halro_kms_descriptor_valid", "gauge", "Whether the persisted Key Slot descriptor is structurally valid.")
	metricHeader(output, "halro_kms_recovery_ready", "gauge", "Whether the configured Recovery Slot is active and independently verified.")
	metricHeader(output, "halro_kms_pending_rotation_slots", "gauge", "Number of pending Key Slots in the current descriptor generation.")
	metricHeader(output, "halro_kms_slot_state", "gauge", "Configured Key Slot state by bounded purpose and state.")
	metricHeader(output, "halro_kms_slot_verified_timestamp_seconds", "gauge", "UTC verification timestamp for configured active Key Slots.")
	descriptor, err := r.store.KeySlotDescriptor(context.Background())
	if err != nil || descriptor.Validate() != nil {
		fmt.Fprintln(output, "halro_kms_descriptor_valid 0")
		fmt.Fprintln(output, "halro_kms_recovery_ready 0")
		fmt.Fprintln(output, "halro_kms_pending_rotation_slots 0")
		return
	}
	fmt.Fprintln(output, "halro_kms_descriptor_valid 1")
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
		fmt.Fprintf(output, "halro_kms_slot_state{purpose=%s,state=%s} 1\n", strconv.Quote(string(slot.Purpose)), strconv.Quote(string(slot.State)))
		if slot.VerifiedAt != nil {
			fmt.Fprintf(output, "halro_kms_slot_verified_timestamp_seconds{purpose=%s} %d\n", strconv.Quote(string(slot.Purpose)), slot.VerifiedAt.Unix())
		}
		if slot.Purpose == masterkey.KeySlotRecovery && slot.State == masterkey.KeySlotActive && slot.VerifiedAt != nil {
			recoveryReady = true
		}
	}
	fmt.Fprintf(output, "halro_kms_pending_rotation_slots %d\n", pending)
	if recoveryReady {
		fmt.Fprintln(output, "halro_kms_recovery_ready 1")
	} else {
		fmt.Fprintln(output, "halro_kms_recovery_ready 0")
	}
}

// boolMetric renders a condition the way Prometheus expects one: a gauge that is
// 1 or 0, so an alert can be written against it without a recording rule.
func boolMetric(condition bool) int {
	if condition {
		return 1
	}
	return 0
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

// writeCapabilityMetrics renders the §13 model-capability observability series.
//
// Every label here is a bounded enum. Model IDs, provider IDs and deployment
// IDs are deliberately absent: they are unbounded in principle and identify the
// specific object, which belongs in the audit trail rather than on a metrics
// port that is scraped and retained indefinitely.
func writeCapabilityMetrics(output *bufio.Writer, snapshot capabilityMetricsSnapshot,
	gauges deploymentCapabilityGauges, readable bool) {
	metricHeader(output, "halro_model_catalog_refresh_total", "counter",
		"Provider model catalog reads by capability profile and outcome.")
	for _, sample := range snapshot.CatalogRefresh {
		fmt.Fprintf(output, "halro_model_catalog_refresh_total{provider_type=%s,profile=%s,status=%s} %d\n",
			strconv.Quote(sample.Key.ProviderType), strconv.Quote(sample.Key.Profile),
			strconv.Quote(sample.Key.Status), sample.Count)
	}
	metricHeader(output, "halro_model_catalog_degraded_total", "counter",
		"Aggregate model catalog reads that lost a binding and returned partial results.")
	for _, sample := range snapshot.CatalogDegraded {
		fmt.Fprintf(output, "halro_model_catalog_degraded_total{provider_type=%s,error_class=%s} %d\n",
			strconv.Quote(sample.Key.ProviderType), strconv.Quote(sample.Key.ErrorClass), sample.Count)
	}
	metricHeader(output, "halro_signed_model_catalog_refresh_total", "counter",
		"Signed model catalog background refreshes by bounded outcome and error class.")
	signedKeys := make([]signedCatalogMetricKey, 0, len(snapshot.SignedCatalog))
	for key := range snapshot.SignedCatalog {
		signedKeys = append(signedKeys, key)
	}
	sort.Slice(signedKeys, func(i, j int) bool {
		return signedKeys[i].Status+signedKeys[i].ErrorClass < signedKeys[j].Status+signedKeys[j].ErrorClass
	})
	for _, key := range signedKeys {
		fmt.Fprintf(output, "halro_signed_model_catalog_refresh_total{status=%s,error_class=%s} %d\n", strconv.Quote(key.Status), strconv.Quote(key.ErrorClass), snapshot.SignedCatalog[key])
	}
	metricHeader(output, "halro_signed_model_catalog_degraded", "gauge", "Whether signed model catalog refresh is degraded while Halro continues with its last-known-good or bundled catalog.")
	degraded := 0
	if snapshot.SignedCatalogState.State == modelcatalog.CatalogStateDegraded || snapshot.SignedCatalogState.State == modelcatalog.CatalogStateUnavailable {
		degraded = 1
	}
	fmt.Fprintf(output, "halro_signed_model_catalog_degraded %d\n", degraded)
	metricHeader(output, "halro_signed_model_catalog_degraded_since_seconds", "gauge", "Unix time at which signed model catalog degradation began, or zero when healthy.")
	degradedSince := int64(0)
	if snapshot.SignedCatalogState.DegradedSince != nil {
		degradedSince = snapshot.SignedCatalogState.DegradedSince.Unix()
	}
	fmt.Fprintf(output, "halro_signed_model_catalog_degraded_since_seconds %d\n", degradedSince)
	metricHeader(output, "halro_capability_drift_total", "counter",
		"Deployments withheld from routing because their capability snapshot no longer matches.")
	// Both reasons are always emitted: `== 0` is the condition worth alerting
	// on, and a series that only appears once it is non-zero cannot express it.
	for _, reason := range []string{driftReasonCatalog, driftReasonProfile} {
		fmt.Fprintf(output, "halro_capability_drift_total{reason=%s} %d\n",
			strconv.Quote(reason), snapshot.Drift[reason])
	}
	metricHeader(output, "halro_model_revision_conflicts_total", "counter",
		"Deployment writes refused because the model's catalog revision moved.")
	fmt.Fprintf(output, "halro_model_revision_conflicts_total %d\n", snapshot.RevisionConflicts)
	resolutionKeys := make([]resolutionMetricKey, 0, len(snapshot.Resolutions))
	for key := range snapshot.Resolutions {
		resolutionKeys = append(resolutionKeys, key)
	}
	sort.Slice(resolutionKeys, func(i, j int) bool {
		a, b := resolutionKeys[i], resolutionKeys[j]
		return a.ProviderType+a.TargetKind+a.Status+a.Source < b.ProviderType+b.TargetKind+b.Status+b.Source
	})
	metricHeader(output, "halro_invocation_target_resolution_total", "counter",
		"Invocation target resolution results by bounded provider type, target kind, status, and evidence source.")
	for _, key := range resolutionKeys {
		fmt.Fprintf(output, "halro_invocation_target_resolution_total{provider_type=%s,target_kind=%s,status=%s,source=%s} %d\n",
			strconv.Quote(key.ProviderType), strconv.Quote(key.TargetKind), strconv.Quote(key.Status), strconv.Quote(key.Source), snapshot.Resolutions[key])
	}
	metricHeader(output, "halro_deployment_test_total", "counter",
		"Deployment validation tests by outcome.")
	for _, status := range []string{"success", "failure"} {
		fmt.Fprintf(output, "halro_deployment_test_total{status=%s} %d\n",
			strconv.Quote(status), snapshot.DeploymentTests[status])
	}
	detectionKeys := make([]detectionMetricKey, 0, len(snapshot.Detections))
	for key := range snapshot.Detections {
		detectionKeys = append(detectionKeys, key)
	}
	sort.Slice(detectionKeys, func(i, j int) bool {
		a, b := detectionKeys[i], detectionKeys[j]
		return a.ProviderType+a.Status+a.Source < b.ProviderType+b.Status+b.Source
	})
	metricHeader(output, "halro_model_capability_detection_total", "counter", "Model capability detections by bounded provider type, terminal status, and source.")
	for _, key := range detectionKeys {
		fmt.Fprintf(output, "halro_model_capability_detection_total{provider_type=%s,status=%s,source=%s} %d\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Status), strconv.Quote(key.Source), snapshot.Detections[key])
	}
	probeKeys := make([]probeMetricKey, 0, len(snapshot.Probes))
	for key := range snapshot.Probes {
		probeKeys = append(probeKeys, key)
	}
	sort.Slice(probeKeys, func(i, j int) bool {
		a, b := probeKeys[i], probeKeys[j]
		return a.ProviderType+a.Capability+a.Status < b.ProviderType+b.Capability+b.Status
	})
	metricHeader(output, "halro_model_capability_probe_total", "counter", "Capability probe classifications by bounded provider type and capability.")
	for _, key := range probeKeys {
		fmt.Fprintf(output, "halro_model_capability_probe_total{provider_type=%s,capability=%s,status=%s} %d\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Capability), strconv.Quote(key.Status), snapshot.Probes[key])
	}
	providerTypes := make([]string, 0, len(snapshot.DetectionInflight))
	for key := range snapshot.DetectionInflight {
		providerTypes = append(providerTypes, key)
	}
	sort.Strings(providerTypes)
	metricHeader(output, "halro_model_capability_detection_inflight", "gauge", "Currently running model capability detection jobs by provider type.")
	for _, providerType := range providerTypes {
		fmt.Fprintf(output, "halro_model_capability_detection_inflight{provider_type=%s} %d\n", strconv.Quote(providerType), snapshot.DetectionInflight[providerType])
	}
	cacheStatuses := make([]string, 0, len(snapshot.DetectionCache))
	for key := range snapshot.DetectionCache {
		cacheStatuses = append(cacheStatuses, key)
	}
	sort.Strings(cacheStatuses)
	metricHeader(output, "halro_model_capability_detection_cache_total", "counter", "Capability detection cache lookups by outcome.")
	for _, status := range cacheStatuses {
		fmt.Fprintf(output, "halro_model_capability_detection_cache_total{status=%s} %d\n", strconv.Quote(status), snapshot.DetectionCache[status])
	}
	callProviders := make([]string, 0, len(snapshot.DetectionCalls))
	for key := range snapshot.DetectionCalls {
		callProviders = append(callProviders, key)
	}
	sort.Strings(callProviders)
	metricHeader(output, "halro_model_capability_detection_provider_calls_total", "counter", "Provider calls made by control-plane capability detection.")
	for _, providerType := range callProviders {
		fmt.Fprintf(output, "halro_model_capability_detection_provider_calls_total{provider_type=%s} %d\n", strconv.Quote(providerType), snapshot.DetectionCalls[providerType])
	}
	metricHeader(output, "halro_model_capability_detection_duration_seconds", "histogram", "Duration of model capability detections by provider type and terminal status.")
	for _, key := range detectionKeys {
		histogram, ok := snapshot.DetectionDuration[key]
		if !ok {
			continue
		}
		for index, bound := range detectionDurationBounds {
			fmt.Fprintf(output, "halro_model_capability_detection_duration_seconds_bucket{provider_type=%s,status=%s,source=%s,le=%s} %d\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Status), strconv.Quote(key.Source), strconv.Quote(strconv.FormatFloat(bound, 'f', -1, 64)), histogram.Buckets[index])
		}
		fmt.Fprintf(output, "halro_model_capability_detection_duration_seconds_bucket{provider_type=%s,status=%s,source=%s,le=%s} %d\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Status), strconv.Quote(key.Source), strconv.Quote("+Inf"), histogram.Count)
		fmt.Fprintf(output, "halro_model_capability_detection_duration_seconds_sum{provider_type=%s,status=%s,source=%s} %g\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Status), strconv.Quote(key.Source), histogram.Sum)
		fmt.Fprintf(output, "halro_model_capability_detection_duration_seconds_count{provider_type=%s,status=%s,source=%s} %d\n", strconv.Quote(key.ProviderType), strconv.Quote(key.Status), strconv.Quote(key.Source), histogram.Count)
	}
	// Both gauges describe stored records, so when the store could not be read
	// there is no value to publish. They are omitted rather than zeroed: a
	// scrape gap is what "we do not know" looks like in Prometheus, and it
	// leaves `drifted == 0` meaning what it says.
	if !readable {
		return
	}
	metricHeader(output, "halro_deployment_capability_status", "gauge",
		"Deployments by the status of the model capability snapshot they hold.")
	for _, status := range capabilityStatuses {
		fmt.Fprintf(output, "halro_deployment_capability_status{state=%s} %d\n",
			strconv.Quote(status), gauges.ByStatus[status])
	}
	// Any status outside the fixed four lands here rather than being dropped,
	// so the states sum to the deployment count. Silently discarding one would
	// make the gauges quietly disagree with the records they summarise —
	// exactly the drift computing them at render time was meant to avoid.
	if gauges.Unrecognised > 0 {
		fmt.Fprintf(output, "halro_deployment_capability_status{state=%s} %d\n",
			strconv.Quote("unrecognised"), gauges.Unrecognised)
	}
	metricHeader(output, "halro_operator_declared_deployments", "gauge",
		"Deployments whose capabilities an administrator declared rather than the catalog establishing them.")
	fmt.Fprintf(output, "halro_operator_declared_deployments %d\n", gauges.OperatorDeclared)
}
