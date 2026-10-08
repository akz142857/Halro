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
| `halro_resource_pressure_enabled` | gauge | none |
| `halro_resource_pressure_state` | gauge | `state` |
| `halro_resource_pressure_samples_total` | counter | none |
| `halro_resource_pressure_rejections_total` | counter | none |
| `halro_resource_pressure_signal_issue` | gauge | `reason` |
| `halro_resource_pressure_runtime_bytes` | gauge | none |
| `halro_resource_pressure_cgroup_bytes` | gauge | none |
| `halro_resource_pressure_disk_available_bytes` | gauge | none |
| `halro_resource_pressure_inodes_available` | gauge | none |
| `halro_replication_unconfirmed_bytes` | gauge | `kind` (`configured_*`, `used_*`) |
| `halro_replication_unconfirmed_entries` | gauge | `kind` (`used`, `configured_limit`) |
| `halro_replication_unconfirmed_recovery_debt_bytes` | gauge | none |
| `halro_replication_unconfirmed_recovery_over_limit` | gauge | none |
| `halro_replication_unconfirmed_admission_refusals_total` | counter | none |
| `halro_replication_unconfirmed_last_progress_timestamp_seconds` | gauge | none |
| `halro_resource_budget_enabled` | gauge | none |
| `halro_resource_budget_ram_bytes` | gauge | none |
| `halro_resource_budget_disk_bytes` | gauge | none |
| `halro_resource_budget_class_ram_bytes` | gauge | `class` |
| `halro_resource_budget_class_disk_bytes` | gauge | `class` |
| `halro_resource_budget_acquired_total` | counter | `class` |
| `halro_resource_budget_rejections_total` | counter | `class` |
| `halro_resource_budget_claims` | gauge | `pool` |
| `halro_resource_budget_owners` | gauge | none |
| `halro_resource_budget_pressure_state` | gauge | `state` |
| `halro_attempts_total` | counter | `status` |
| `halro_tokens_total` | counter | `direction` |
| `halro_cost_usd_total` | counter | none |
| `halro_request_duration_seconds` | summary sum/count | none |
| `halro_attempt_duration_seconds` | summary sum/count | none |
| `halro_request_latency_seconds` | classic histogram | `le` |
| `halro_attempt_latency_seconds` | classic histogram | `le` |
| `halro_stream_first_byte_seconds` | classic histogram | `operation`, `le` |
| `halro_admin_audit_intents_pending` | gauge | none |
| `halro_admin_audit_delivery_failures_total` | counter | none |
| `halro_activation_stale` | gauge | none |
| `halro_activation_stale_seconds` | gauge | none |
| `halro_active_requests` | gauge | none |
| `halro_deferred_responses_running` | gauge | none |
| `halro_deferred_responses_submitted_total` | counter | none |
| `halro_deferred_responses_expired_total` | counter | none |
| `halro_deferred_responses_interrupted_total` | counter | none |
| `halro_failure_captures_today` | gauge | none |
| `halro_failure_capture_day_limit` | gauge | none |
| `halro_failure_capture_saturated` | gauge | none |
| `halro_usage_window_attempts` | gauge | none |
| `halro_usage_window_requests` | gauge | none |
| `halro_usage_window_floor_sequence` | gauge | none |
| `halro_usage_window_trimmed_total` | counter | none |
| `halro_usage_checkpoint_segments` | gauge | none |
| `halro_usage_checkpoint_bytes` | gauge | none |
| `halro_usage_checkpoint_open_segment_bytes` | gauge | none |
| `halro_ledger_active_bytes` | gauge | none |
| `halro_ledger_sealed_generations` | gauge | none |
| `halro_ledger_sealed_bytes` | gauge | none |
| `halro_ledger_sealed_stored_bytes` | gauge | none |
| `halro_source_rate_limited_total` | counter | none |
| `halro_source_rate_limit_overflow_total` | counter | none |
| `halro_fallbacks_total` | counter | none |
| `halro_usage_queue_depth` | gauge | none |
| `halro_usage_queue_capacity` | gauge | none |
| `halro_accounting_pending_leases` | gauge | none |
| `halro_accounting_oldest_pending_lease_age_seconds` | gauge | none |
| `halro_accounting_pending_lease_normal_max_age_seconds` | gauge | none |
| `halro_accounting_recovery_total` | counter | `status` |
| `halro_pricing_quarantined_deployments` | gauge | none |
| `halro_pricing_unknown_attempts_total` | counter | none |
| `halro_pricing_recovery_pending_intents` | gauge | none |
| `halro_wal_append_errors_total` | counter | none |
| `halro_wal_append_records_total` | counter | none |
| `halro_wal_append_batches_total` | counter | none |
| `halro_wal_sync_seconds` | summary sum/count | none |
| `halro_accounting_project_lock_acquisitions_total` | counter | none |
| `halro_accounting_project_lock_live` | gauge | none |
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
| `halro_provider_failure_reason_total` | counter | `reason`, `provider_status` |
| `halro_provider_failure_reason_dropped_total` | counter | none |
| `halro_route_suspended` | gauge | `scope_kind`, `reason` |
| `halro_route_suspension_transitions_total` | counter | `reason` |
| `halro_route_probe_admitted_total` | counter | `reason`, `outcome` |
| `halro_provider_active_requests` | gauge | `provider_id` |
| `halro_provider_concurrency_limit` | gauge | `provider_id` |
| `halro_deployment_active_requests` | gauge | `deployment_id` |
| `halro_deployment_concurrency_limit` | gauge | `deployment_id` |
| `halro_deployment_up` | gauge | `deployment_id` |
| `halro_tls_certificate_expiry_seconds` | gauge | `scope`, `name` |
| `halro_reload_total` | counter | `item`, `status` |
| `halro_reload_last_success_timestamp_seconds` | gauge | `item` |
| `halro_build_info` | gauge | `version`, `commit` |
| `halro_tzdata_info` | gauge | `source`, `version`, `fingerprint` |
| `halro_accounting_timezone_version` | gauge | none |
| `halro_accounting_period_end_seconds` | gauge | none |
| `halro_model_catalog_refresh_total` | counter | `provider_type`, `profile`, `status` |
| `halro_model_catalog_degraded_total` | counter | `provider_type`, `error_class` |
| `halro_signed_model_catalog_refresh_total` | counter | `status`, `error_class` |
| `halro_signed_model_catalog_degraded` | gauge | none |
| `halro_signed_model_catalog_degraded_since_seconds` | gauge | none |
| `halro_capability_drift_total` | counter | `reason` |
| `halro_model_revision_conflicts_total` | counter | none |
| `halro_invocation_target_resolution_total` | counter | `provider_type`, `target_kind`, `status`, `source` |
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
| `halro_shutdown_truncated_attempts_total` | counter | none |
| `halro_audit_anchor_last_emit_timestamp_seconds` | gauge | none |
| `halro_audit_anchor_interval_seconds` | gauge | none |
| `halro_audit_anchor_emit_failures_total` | counter | none |
| `halro_audit_anchor_auth_failures_total` | counter | none |
| `halro_process_goroutines` | gauge | none |
| `go_goroutines` | gauge | none |
| `go_memstats_heap_alloc_bytes` | gauge | none |
| `go_memstats_gc_cycles_total` | counter | none |
| `process_start_time_seconds` | gauge | none |

The three `halro_ledger_sealed_*` gauges use a fixed-size committed manifest
summary. Scraping them does not copy or walk the segment directory. Generations
counts manifest entries, bytes sums uncompressed frame lengths, and stored bytes
sums the lengths of the files named by those entries. Stored bytes excludes
orphan/temporary files and filesystem allocation overhead; it is not a physical
disk, page-cache, RSS or retention-authorization measurement. Open reconstructs
the summary after manifest repair, and Roll/Compact update it together with the
in-memory committed directory. The resident directory itself remains unbounded.

### Counter reset semantics

Two kinds of counter are exported here and they do not reset together.

`halro_requests_total`, `halro_attempts_total`, `halro_tokens_total` and
`halro_cost_usd_total` are read-model totals: startup replays the Ledger into
the Usage aggregate, so they carry the whole history of the data directory
across restarts.

`halro_shutdown_truncated_attempts_total` is also durable. When
`server.shutdown_timeout` expires, Halro counts Provider attempts still active
at the instant forced connection close begins and commits that increment to the
metadata store before closing sockets. The next process therefore exposes the
terminal event that the previous process could no longer serve to Prometheus.

`halro_wal_append_*`, `halro_wal_sync_seconds`,
the project-lock acquisitions/wait/held families and `halro_metadata_*` count work this
*process* performed, so they start at zero on every restart — replay appends
nothing and takes no locks.

The pair is therefore expected to disagree after a restart: a freshly started
instance can report thousands of requests and zero WAL appends without anything
being wrong. `rate()` over either is unaffected; only an absolute comparison
between the two families is meaningless.

`halro_accounting_project_lock_live` is a current gauge of registry identities
retained by lock users/waiters, admitted money or Run-close obligations. Cold
projects and durable-only reservations do not retain entries. It has no project
label and requires no historical scan. Deleting an entry preserves its Ledger
facts. A lower count does not prove lower RAM: registry backing capacity is
released when entirely empty, and active obligation maps/allocator peaks and
shared physical resource charges are separate boundaries.

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

`halro_policy_rejections_total{reason="key_rate"}` counts requests shed by the
built-in per-Key ceiling — the bound on what one authenticated caller may ask
Halro to answer from its own state, which has no configuration surface. It rises
where the Project limiter cannot: discovery, deferred polling, and the resource
plane's lookups reach no provider, so `reason="rpm"` stays flat while this one
moves. A caller seeing it is looping something that costs nothing upstream.

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
absence must not be interpreted as healthy. The same probe result is reported to
the Admin console on the deployment record (`probe.state` is `healthy`,
`unhealthy` or `not_probed`), so an operator does not have to read metrics to
learn that a deployment has been taken out of the router's candidates. Only the
classified error travels with it — the upstream's own sentence about the request
stays inside the error, as it does everywhere else.

`halro_tls_certificate_expiry_seconds` reports when a served certificate stops
being valid. `scope` is `serving` (the Gateway and Admin listeners, which share
one certificate list) or `metrics`; a listener serving plaintext contributes no
series. `name` is the certificate's own first DNS name, so the label set is
bounded by the `tls.certificates` entries the operator wrote down. The name a
client offers during the handshake is never used as a label value.

`halro_reload_total` and `halro_reload_last_success_timestamp_seconds` describe
what `SIGHUP` did, per item: `tls`, `metrics_tls`, `log_level`, `log_file`. The
item list is compiled in, so every item is present from the first scrape.
Items are applied independently, and a failure keeps that item's previous value
— so a rising `status="error"` count means the process is still serving the
material it had before, not that it stopped. The timestamp gauge is absent for
an item until one reload of it has succeeded; an item that does not apply to the
deployment (no TLS, no log file) never records either outcome.

`halro_activation_stale` is always present. A value of `1` means at least one
live topology, authentication, redaction, or Token Guard snapshot is known to
be behind the durable store, so the data plane is intentionally refusing all
requests. `halro_activation_stale_seconds` measures from the oldest affected
domain and is `0` when every domain is current. These gauges have no labels;
use `/admin/api/v1/system/status` to identify the affected domains and reasons.

## Physical resource pressure admission

Optional `resource_pressure` calibration activates one bounded sampler per runtime.
No production threshold is inferred from the container limit. `enabled` reports the
active physical guard, not completion of shared logical resource budgets. New
Gateway inference, object/job creation, Work Unit and Run creation return HTTP 503
before their bodies are read while the guard is unknown or pressured. Existing
queries, cancellation, deletion, outcome reporting and close bypass new-work admission.
The OpenAI error code is `resource_pressure`; Anthropic uses `overloaded_error`.
Already admitted requests are not cancelled by this guard. Liveness remains available;
Standalone/Primary readiness includes pressure, while Replica readiness retains its
replication-candidate meaning. Replication and existing recovery are not gated.

`state` is one-hot over `unknown`, `healthy`, `pressured`. `signal_issue` has a fixed
reason vocabulary: `runtime_unavailable`, `cgroup_unavailable`, `disk_unavailable`,
`sample_cancelled`, `signal_invalid`, `sample_stale`, `clock_invalid`, `sample_repeated`,
`sample_gap`, `source_changed`, `runtime_high`, `cgroup_high`, `cgroup_headroom_low`,
`disk_low`, `inodes_low`, `cooling`, `monitor_inactive`. No paths or object IDs are labels.
State/issues/counts remain visible when physical measurements are missing. Runtime,
cgroup, available-space and inode gauges are omitted before the first sample,
after invalid/stale evidence or cancellation/stop, and for their unavailable signal.
A missing gauge is not zero usage or unlimited space. Go runtime bytes are
`total - released`, not RSS; runtime and cgroup scopes must not be summed.

Alert on `halro_resource_pressure_state{state="unknown"} == 1` or
`halro_resource_pressure_state{state="pressured"} == 1` for a calibrated sustained
interval; inspect fixed `signal_issue` reasons. This is a physical admission guard:
it does not reconcile historical disk charges, preallocate completion space, or
provide complete owner reconciliation or an ENOSPC guarantee. The optional
logical disk ceilings account only for owners that acquire permits; a
filesystem without inode capacity statistics remains unknown; supported dynamic
inode policy and target capacity qualification remain open.

Optional `resource_budget` requires that calibrated physical guard and adds a
single shared logical RAM and disk manager. Filesystem disk charges from enabled
owners remain until qualified physical cleanup. The Recovery reserve is isolated
from normal work; Completion credits are acquired atomically with active work and
remain charged through that work’s terminal operation. Every new HTTP work route acquires an Active
envelope before body reads and releases after the entire handler returns, including
sync finalization and downstream output. Deferred workers acquire a Background
envelope before reading stored input; refusal keeps already accepted work queued
and uses the existing dispatcher backoff. HTTP, deferred work and independent capture copies compete for the
same total, claim metadata and normal capacity excluding the recovery reserve.
Failure capture acquires its own Background envelope before JSON encoding.
The calibrated cost includes full encoding before truncation, plaintext and
sealed copies, not just the retained payload limit. Refusal drops only the
best-effort diagnostic. Packet, Store call and sealed writer share one claim
with explicitly retained owners; cancellation does not refund a still-running
writer. Queue cancellation clears all tail references before releasing their
owners. Runtime joins the concrete Store before releasing its directory lock.
With HA enabled, source-coordinator constructor streaming validation and peer
window reconstruction acquire a Recovery envelope before reading native bytes.
Each peer can retain one byte-blocked candidate; duplicate ACK/retry reuses it.
References are cleared before refund, after synchronous queue copying or session
failure/forget/stop. Global source/state failure clears both candidates and leases.
Unknown/pressured physical state preserves dedicated Recovery admission.
`replication_candidate_ram_bytes` requires explicit calibration; the wire-size
lower bound and capacity for two candidates plus one additional reconstruction
are not proof of a sufficient native decoding peak. Runtime session queues
independently reserve `replication_queue_ram_bytes` before channel/copy allocation,
covering full outbound capacity including the active writer and queue metadata.
Three live/retiring session slots cover two peers and one replacement/control
session. Closing clears unsent tails but retains fees/slots until socket Close completes and the actual
writer returns, even after a SendSync waiter has been released. Writing a record
frees byte capacity, not the session-wide reservation. Configuration now requires
Recovery capacity and eight claims for three reconstructions, three queue/read
owners, one NativeSource reader owner and one Governance cache owner.
Without shared accounting, ConnectionManager still bounds authenticated data
and control-only sessions to configured peers plus one. TLS internals, inbound
read/decode, handshakes, long-lived readers/IO, PendingFrom pages, preliminary
reconcile and live append encoding still require integration.

Default native Governance logs retain 4096 fixed identity/content fingerprints
and native positions, rather than complete Event payloads or string keys.
With shared budgeting enabled, `governance_log_ram_bytes` reserves one Recovery
lifetime claim before cache construction or file IO, including Standalone and
Replica. Its calibrated envelope has a 2 MiB lower bound; replay/decode and batch
encoding temporaries require separate fees. Native Close joins readers/writers,
closes the actual file and clears cache backing storage before refund. Physical
close errors retain the charge. Budgeted HA now requires eight dedicated claims
and RAM for `3*(candidate+queue+read)+reader+governance_log` plus their control
costs. Unknown/pressured state continues to permit dedicated Recovery.

Logical refusal uses OpenAI `resource_budget` or Anthropic `overloaded_error`, HTTP
503 and `Cache-Control: no-store`. Existing-work routes bypass this new admission.

The Primary unconfirmed families are emitted only when optional unconfirmed
budgets are configured. They expose fixed configured/used byte dimensions, the
bounded entry count, any recovered suffix debt, admission refusals and the time
of the latest confirmed-state publication in this process. These metrics carry no peer, request, or object
identity labels. The existing `halro_replication_confirmed_index` remains the
authoritative confirmed prefix value.

Budget `class` labels are fixed: `configuration`, `active`, `cache`, `io`,
`background`, `completion`, `recovery`, `control`; `pool` is `normal` or `recovery`.
RAM gauges report charged estimates, not actual allocation or reclaimed OS pages.
`pressure_state` includes the monitor owner's lifecycle and original lease age;
sampling cancellation immediately closes new normal claims even during blocked IO.
Unchanged healthy samples renew that lease. A decision that expires between
observation and application publishes unknown, resets cooldown and retries fresh
samples; it does not permanently stop the sampler. Counts include refused claim
attempts, so deferred retries may increment them repeatedly.

`halro_accounting_pending_leases` and
`halro_accounting_oldest_pending_lease_age_seconds` describe one accounting
owner's coherent pending cut. If that owner is retired, the read fails or the
scrape context is cancelled, both families are omitted; a failed observation
must not appear as zero pending work. Other metric families continue to render,
including the independently configured normal maximum lease age. A successful
empty pending view emits zero. The fresh disk projection candidate maintains
count and an ordered reservation-time index atomically with native Apply; this
does not grant trust to a reopened DB or mean the default Runtime has migrated.

This RAM integration covers HTTP, deferred work, capture and part of HA reconstruction. Fixed runtime
objects, historical maps/caches, pending enumeration, production recovery/finalization,
disk and confirmation owners still require integration and capacity calibration.
`enabled=1` does not mean the shared total covers every consumer or that durable
settlement has reserved space. Budget calibration changes require a restart.

### Selected Usage projection observations

| Metric | Type | Meaning |
| --- | --- | --- |
| `halro_usage_projection_selected` | gauge | 1 for the selected fresh disk owner, 0 for resident Aggregate. |
| `halro_usage_read_unavailable` | gauge | 1 if the selected observation failed; Usage-dependent counters/histograms and gauges are omitted, while other scrape families remain available. |
| `halro_usage_watermark_sequence` | gauge | Ledger sequence covered by the selected owner, emitted only after a successful read. |
| `halro_usage_projection_logical_bytes` | gauge | Committed DB page extent from the same transaction as the disk Usage counters; neither physical file allocation nor RSS. Emitted only for readable disk owners. |

The resident window/checkpoint families apply only to Aggregate. A disk owner
omits them instead of reporting fabricated zero counts or borrowing a retired
checkpoint. Usage reads do not reconstruct historical Snapshot data. Metrics
remain readable during business drain; terminal Close withdraws fresh scrapes
and joins admitted response writes before closing dependencies.

`halro_pricing_recovery_pending_intents` reads the schema-43 authoritative
pending counter maintained atomically with pricing intent facts. It does not
materialize the intent history. Missing/malformed counter state or unavailable
Metadata omits this series instead of reporting zero; other readable metric
families remain available. This point read is not a fresh history authentication.
