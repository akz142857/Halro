# Metrics reference

The dedicated Metrics listener exposes Prometheus text format at `/metrics`.

Derivative Usage lag is reported separately from the durable Ledger queue:

- `heimdall_usage_analytics_queue_depth` — records awaiting aggregation;
- `heimdall_usage_analytics_dropped_total` — notifications dropped from the
  bounded derivative queue (recoverable from Ledger);
- `heimdall_usage_analytics_lagging` — `1` until watermark catch-up succeeds.
It binds to loopback by default. When `metrics.require_auth` is true, obtain
the deterministic, purpose-separated bearer token locally:

```text
heimdall metrics token --config ./config.yaml
```

Configure Prometheus with `Authorization: Bearer <token>`. The token is derived
from the master key under a metrics-only HKDF domain, is never stored in YAML,
and is compared by SHA-256 in constant time. Treat it as a Secret.

Implemented metrics:

| Metric | Type | Labels |
|---|---|---|
| `heimdall_requests_total` | counter | `status` |
| `heimdall_attempts_total` | counter | `status` |
| `heimdall_tokens_total` | counter | `direction` |
| `heimdall_cost_usd_total` | counter | none |
| `heimdall_request_duration_seconds` | summary sum/count | none |
| `heimdall_attempt_duration_seconds` | summary sum/count | none |
| `heimdall_active_requests` | gauge | none |
| `heimdall_process_goroutines` | gauge | none |
| `heimdall_fallbacks_total` | counter | none |
| `heimdall_usage_queue_depth` | gauge | none |
| `heimdall_usage_queue_capacity` | gauge | none |
| `heimdall_wal_append_errors_total` | counter | none |
| `heimdall_alert_delivery_total` | counter | `status` |
| `heimdall_alert_queue_depth` | gauge | none |
| `heimdall_provider_up` | gauge | `provider_type` |

User-controlled Project, Key, Route, model, request ID, source IP, and raw error
values are deliberately excluded from labels. `provider_up` currently means
that an adapter loaded successfully and remains eligible for passive routing;
active health-probe semantics will be added separately.
