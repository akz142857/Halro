# Metrics reference

The dedicated Metrics listener exposes Prometheus text format at `/metrics`.
It binds to loopback by default and enforces a bounded concurrent scrape count
and write deadline.

## Authentication

Development and upgrade-compatible loopback installations may use the legacy,
purpose-separated token derived from the Master Key:

```text
halro metrics token --config ./config.yaml
```

Production uses `metrics.credential_file`. Initialize or rotate it offline
from shell history-safe automation and redirect stdout directly into the
Prometheus secret store:

```text
halro metrics rotate --config ./config.yaml --overlap 10m
halro metrics list --config ./config.yaml
halro metrics revoke --config ./config.yaml --version 1
halro metrics verify-audit --config ./config.yaml
```

The credential file stores SHA-256 hashes, version/epoch, lifecycle state, and
timestamps, never plaintext tokens. Rotation permits one active token and
bounded retiring tokens; revocation is independent of the Master Key and is
enforced on the next request without restarting Halro. A separate append-only
revocation ledger prevents an older credential-file restore from reauthorizing
a revoked version. Credential, revocation, audit, and lock files are mode
`0600`; the first three are backed up as security state. Rotation and
revocation are serialized by an OS file lock and append secret-free hash-chained audit
events; `verify-audit` detects local deletion, rewriting, truncation, reordering,
and missing lifecycle events before the latest chain hash is anchored in an
independent production audit platform.

Configure Prometheus with an `Authorization: Bearer` credential file. Do not put
the token in YAML, environment variables, process arguments, logs, screenshots,
or artifacts.

Non-loopback Metrics requires a versioned credential file and dedicated mutual
TLS under `metrics.tls`, including a client CA. A private network alone is not
an authentication boundary.

## Exported metrics

| Metric | Type | Labels |
|---|---|---|
| `halro_requests_total` | counter | `status` |
| `halro_attempts_total` | counter | `status` |
| `halro_tokens_total` | counter | `direction` |
| `halro_cost_usd_total` | counter | none |
| `halro_request_duration_seconds` | summary sum/count | none |
| `halro_attempt_duration_seconds` | summary sum/count | none |
| `halro_request_latency_seconds` | classic histogram | `le` |
| `halro_attempt_latency_seconds` | classic histogram | `le` |
| `halro_active_requests` | gauge | none |
| `halro_source_rate_limited_total` | counter | none |
| `halro_source_rate_limit_overflow_total` | counter | none |
| `halro_fallbacks_total` | counter | none |
| `halro_usage_queue_depth` | gauge | none |
| `halro_usage_queue_capacity` | gauge | none |
| `halro_accounting_pending_leases` | gauge | none |
| `halro_accounting_oldest_pending_lease_age_seconds` | gauge | none |
| `halro_accounting_recovery_total` | counter | `status` |
| `halro_pricing_quarantined_deployments` | gauge | none |
| `halro_pricing_unknown_attempts_total` | counter | none |
| `halro_pricing_recovery_pending_intents` | gauge | none |
| `halro_wal_append_errors_total` | counter | none |
| `halro_wal_append_records_total` | counter | none |
| `halro_wal_append_batches_total` | counter | none |
| `halro_wal_sync_seconds` | summary sum/count | none |
| `halro_accounting_project_lock_acquisitions_total` | counter | none |
| `halro_accounting_project_lock_wait_seconds` | summary sum/count | none |
| `halro_accounting_project_lock_held_seconds` | summary sum/count | none |
| `halro_metadata_batch_calls_total` | counter | none |
| `halro_metadata_batch_transactions_total` | counter | none |
| `halro_metadata_page_writes_total` | counter | none |
| `halro_metadata_page_write_seconds_total` | counter | none |
| `halro_metadata_free_pages` | gauge | none |
| `halro_metadata_pending_pages` | gauge | none |
| `halro_usage_analytics_queue_depth` | gauge | none |
| `halro_usage_analytics_dropped_total` | counter | none |
| `halro_usage_analytics_lagging` | gauge | none |
| `halro_alert_delivery_total` | counter | `status` |
| `halro_alert_queue_depth` | gauge | none |
| `halro_token_guard_events_dropped_total` | counter | none |
| `halro_kms_calls_total` | counter | `operation`, `status`, `error_class` |
| `halro_kms_call_duration_seconds` | summary sum/count | `operation`, `status`, `error_class` |
| `halro_kms_unlock_total` | counter | `purpose`, `status`, `error_class` |
| `halro_kms_automatic_fallback_total` | counter | none |
| `halro_kms_recovery_last_used_timestamp_seconds` | gauge | none |
| `halro_kms_descriptor_valid` | gauge | none |
| `halro_kms_recovery_ready` | gauge | none |
| `halro_kms_pending_rotation_slots` | gauge | none |
| `halro_kms_slot_state` | gauge | `purpose`, `state` |
| `halro_kms_slot_verified_timestamp_seconds` | gauge | `purpose` |
| `halro_provider_up` | gauge | `provider_type` |
| `halro_policy_rejections_total` | counter | `reason` |
| `halro_provider_active_requests` | gauge | `provider_id` |
| `halro_provider_concurrency_limit` | gauge | `provider_id` |
| `halro_deployment_active_requests` | gauge | `deployment_id` |
| `halro_deployment_concurrency_limit` | gauge | `deployment_id` |
| `halro_deployment_up` | gauge | `deployment_id` |
| `halro_build_info` | gauge | `version`, `commit` |
| `halro_tzdata_info` | gauge | `source`, `version`, `fingerprint` |
| `halro_accounting_timezone_version` | gauge | none |
| `halro_accounting_period_end_seconds` | gauge | none |
| `halro_model_catalog_refresh_total` | counter | `provider_type`, `profile`, `status` |
| `halro_model_catalog_degraded_total` | counter | `provider_type`, `error_class` |
| `halro_capability_drift_total` | counter | `reason` |
| `halro_model_revision_conflicts_total` | counter | none |
| `halro_deployment_test_total` | counter | `status` |
| `halro_model_capability_detection_total` | counter | `provider_type`, `status`, `source` |
| `halro_model_capability_probe_total` | counter | `provider_type`, `capability`, `status` |
| `halro_model_capability_detection_inflight` | gauge | `provider_type` |
| `halro_model_capability_detection_cache_total` | counter | `status` |
| `halro_model_capability_detection_provider_calls_total` | counter | `provider_type` |
| `halro_model_capability_detection_duration_seconds` | classic histogram | `provider_type`, `status`, `source`, `le` |
| `halro_deployment_capability_status` | gauge | `state` |
| `halro_operator_declared_deployments` | gauge | none |
| `halro_metrics_auth_failures_total` | counter | none |
| `halro_metrics_scrape_rejected_total` | counter | none |
| `halro_metrics_render_errors_total` | counter | none |
| `halro_process_goroutines` | gauge | none |
| `go_goroutines` | gauge | none |
| `go_memstats_heap_alloc_bytes` | gauge | none |
| `go_memstats_gc_cycles_total` | counter | none |
| `process_start_time_seconds` | gauge | none |

### Counter reset semantics

Two kinds of counter are exported here and they do not reset together.

`halro_requests_total`, `halro_attempts_total`, `halro_tokens_total` and
`halro_cost_usd_total` are read-model totals: startup replays the Ledger into
the Usage aggregate, so they carry the whole history of the data directory
across restarts.

`halro_wal_append_*`, `halro_wal_sync_seconds`,
`halro_accounting_project_lock_*` and `halro_metadata_*` count work this
*process* performed, so they start at zero on every restart — replay appends
nothing and takes no locks.

The pair is therefore expected to disagree after a restart: a freshly started
instance can report thousands of requests and zero WAL appends without anything
being wrong. `rate()` over either is unaffected; only an absolute comparison
between the two families is meaningless.

The model-capability detection families are process-local control-plane
counters. A possibly billable probe is durably reserved in the detection
record before provider I/O, while these metrics describe activity observed by
the current process. `source` is the bounded evidence source
(`builtin_catalog` or `verified_probe`); `capability` is one of the compiled
`ProviderCapabilities` field names. No model, provider instance, binding,
detection, administrator, request, or credential identifier is used as a
label. Duration buckets are 1, 5, 15, 30, 60, and 90 seconds plus `+Inf`.


`halro_deployment_capability_status` and `halro_operator_declared_deployments`
describe stored records rather than process counters, so they are read at scrape
time. When that read fails, both families are **omitted from the exposition
rather than reported as zero**. Alerts must treat a missing series as unknown —
`absent()` — and not as "no drifted deployments", which is the assertion these
gauges exist to support. Every other value of `state` is one of `known`,
`partial`, `unknown` or `conflicting`, all four always present; the additional
`unrecognised` series appears only if a stored record carries a status outside
those four, and it exists so such a record shows up in the totals instead of
being dropped from them.

Histogram buckets are 10, 25, 50, 100, 250, and 500 milliseconds, then 1,
2.5, 5, 10, 30, and 120 seconds plus `+Inf`. They are derived from Ledger
events and persisted in the Usage checkpoint, so replay and catch-up preserve
the exact distribution.

`halro_source_rate_limited_total` counts requests shed by the per-source
Gateway limiter. It carries no source label for the same reason a source IP is
excluded everywhere else here: that label is both unbounded and a disclosure of
caller addresses through the Metrics port. A rising
`halro_source_rate_limit_overflow_total` means distinct sources per minute
have outgrown `gateway.source_rate_limit.max_tracked_sources`, so callers past
the ceiling are sharing one budget and may be shed while inside their own —
raise the ceiling rather than the budget.

User-controlled Project, Key, Route, model, request ID, source IP, and raw error
values are deliberately excluded. Provider/Deployment IDs are bounded managed
identifiers. `halro_deployment_up` is absent until the first active probe;
absence must not be interpreted as healthy.
