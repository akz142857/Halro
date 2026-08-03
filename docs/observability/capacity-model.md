# Observability capacity model

Status: Baseline template; production values required in Phase D
Owner: SRE
Sign-off: Platform

## Series formula

```text
per_instance = fixed_application
             + provider_count * series_per_provider
             + deployment_count * series_per_deployment
             + histogram_bucket_count * histogram_label_combinations
             + go_process_series

per_environment = per_instance * instance_count
                + recording_rule_series
                + core_platform_series
```

Classic histogram cost includes every `_bucket` plus `_sum` and `_count`.
Label combinations use the full admitted Cartesian product.

## Storage formula

```text
required_bytes = measured_bytes_per_series_day
               * admitted_series
               * retention_days
               * 1.30
```

The local MVP uses seven-day retention and a bounded volume. Production must
replace every placeholder below using a representative soak:

The example alert uses an absolute 3.5 GiB warning threshold for a planned
5 GiB local volume because container volume capacity is not exported by
Prometheus itself. Production must either provision a real 5 GiB bound or
replace the expression with filesystem capacity metrics from the platform.

| Input | Baseline |
|---|---:|
| fixed application series | measure in A1 |
| maximum providers | declare in D |
| series per provider | measure in A1 |
| maximum deployments | declare in D |
| series per deployment | measure in A1 |
| classic histogram buckets | 12 + sum/count |
| instances | declare in D |
| retention days | declare in D |
| bytes/series/day | measure in soak |

Warn at 80% of admitted series. Reject an unreviewed cardinality increase at
100%. Disk alerts fire at 70% warning and 85% critical. Capacity lead time must
exceed on-call response plus storage expansion time.

## Core and dead-man budgets

The Core admission budget includes Heimdall scrape series, recording/alert rule
series, Prometheus and Alertmanager self-monitoring, rule-evaluation/query cost,
TSDB/WAL growth and retention headroom. The independent dead-man has its own
host, network, receiver retention and notification-rate budget; it is not
counted as spare capacity on the Core host.

Core and the independently deployed dead-man must each test their own worst
case. The dead-man result cannot replace the Core 24-hour soak.
