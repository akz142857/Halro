# Metrics reference

The dedicated Metrics listener exposes Prometheus text format at `/metrics`.
It binds to loopback by default and enforces a bounded concurrent scrape count
and write deadline.

## Authentication

Development and upgrade-compatible loopback installations may use the legacy,
purpose-separated token derived from the Master Key:

```text
heimdall metrics token --config ./config.yaml
```

Production uses `metrics.credential_file`. Initialize or rotate it offline
from shell history-safe automation and redirect stdout directly into the
Prometheus secret store:

```text
heimdall metrics rotate --config ./config.yaml --overlap 10m
heimdall metrics list --config ./config.yaml
heimdall metrics revoke --config ./config.yaml --version 1
heimdall metrics verify-audit --config ./config.yaml
```

The credential file stores SHA-256 hashes, version/epoch, lifecycle state, and
timestamps, never plaintext tokens. Rotation permits one active token and
bounded retiring tokens; revocation is independent of the Master Key and is
enforced on the next request without restarting Heimdall. A separate append-only
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
| `heimdall_requests_total` | counter | `status` |
| `heimdall_attempts_total` | counter | `status` |
| `heimdall_tokens_total` | counter | `direction` |
| `heimdall_cost_usd_total` | counter | none |
| `heimdall_request_duration_seconds` | summary sum/count | none |
| `heimdall_attempt_duration_seconds` | summary sum/count | none |
| `heimdall_request_latency_seconds` | classic histogram | `le` |
| `heimdall_attempt_latency_seconds` | classic histogram | `le` |
| `heimdall_active_requests` | gauge | none |
| `heimdall_source_rate_limited_total` | counter | none |
| `heimdall_source_rate_limit_overflow_total` | counter | none |
| `heimdall_fallbacks_total` | counter | none |
| `heimdall_usage_queue_depth` | gauge | none |
| `heimdall_usage_queue_capacity` | gauge | none |
| `heimdall_accounting_pending_leases` | gauge | none |
| `heimdall_accounting_oldest_pending_lease_age_seconds` | gauge | none |
| `heimdall_accounting_recovery_total` | counter | `status` |
| `heimdall_pricing_quarantined_deployments` | gauge | none |
| `heimdall_pricing_unknown_attempts_total` | counter | none |
| `heimdall_pricing_recovery_pending_intents` | gauge | none |
| `heimdall_wal_append_errors_total` | counter | none |
| `heimdall_usage_analytics_queue_depth` | gauge | none |
| `heimdall_usage_analytics_dropped_total` | counter | none |
| `heimdall_usage_analytics_lagging` | gauge | none |
| `heimdall_alert_delivery_total` | counter | `status` |
| `heimdall_alert_queue_depth` | gauge | none |
| `heimdall_token_guard_events_dropped_total` | counter | none |
| `heimdall_kms_calls_total` | counter | `operation`, `status`, `error_class` |
| `heimdall_kms_call_duration_seconds` | summary sum/count | `operation`, `status`, `error_class` |
| `heimdall_kms_unlock_total` | counter | `purpose`, `status`, `error_class` |
| `heimdall_kms_automatic_fallback_total` | counter | none |
| `heimdall_kms_recovery_last_used_timestamp_seconds` | gauge | none |
| `heimdall_kms_descriptor_valid` | gauge | none |
| `heimdall_kms_recovery_ready` | gauge | none |
| `heimdall_kms_pending_rotation_slots` | gauge | none |
| `heimdall_kms_slot_state` | gauge | `purpose`, `state` |
| `heimdall_kms_slot_verified_timestamp_seconds` | gauge | `purpose` |
| `heimdall_provider_up` | gauge | `provider_type` |
| `heimdall_policy_rejections_total` | counter | `reason` |
| `heimdall_provider_active_requests` | gauge | `provider_id` |
| `heimdall_provider_concurrency_limit` | gauge | `provider_id` |
| `heimdall_deployment_active_requests` | gauge | `deployment_id` |
| `heimdall_deployment_concurrency_limit` | gauge | `deployment_id` |
| `heimdall_deployment_up` | gauge | `deployment_id` |
| `heimdall_build_info` | gauge | `version`, `commit` |
| `heimdall_tzdata_info` | gauge | `source`, `version`, `fingerprint` |
| `heimdall_accounting_timezone_version` | gauge | none |
| `heimdall_accounting_period_end_seconds` | gauge | none |
| `heimdall_metrics_auth_failures_total` | counter | none |
| `heimdall_metrics_scrape_rejected_total` | counter | none |
| `heimdall_metrics_render_errors_total` | counter | none |
| `heimdall_process_goroutines` | gauge | none |
| `go_goroutines` | gauge | none |
| `go_memstats_heap_alloc_bytes` | gauge | none |
| `go_memstats_gc_cycles_total` | counter | none |
| `process_start_time_seconds` | gauge | none |

Histogram buckets are 10, 25, 50, 100, 250, and 500 milliseconds, then 1,
2.5, 5, 10, 30, and 120 seconds plus `+Inf`. They are derived from Ledger
events and persisted in the Usage checkpoint, so replay and catch-up preserve
the exact distribution.

`heimdall_source_rate_limited_total` counts requests shed by the per-source
Gateway limiter. It carries no source label for the same reason a source IP is
excluded everywhere else here: that label is both unbounded and a disclosure of
caller addresses through the Metrics port. A rising
`heimdall_source_rate_limit_overflow_total` means distinct sources per minute
have outgrown `gateway.source_rate_limit.max_tracked_sources`, so callers past
the ceiling are sharing one budget and may be shed while inside their own —
raise the ceiling rather than the budget.

User-controlled Project, Key, Route, model, request ID, source IP, and raw error
values are deliberately excluded. Provider/Deployment IDs are bounded managed
identifiers. `heimdall_deployment_up` is absent until the first active probe;
absence must not be interpreted as healthy.
