# Alerting contract

Status: Accepted for Phase A1
Owner: SRE

Prometheus rule files under `deploy/observability/prometheus/` are the only
alert evaluation source. Grafana displays alert state but does not load an
equivalent second rule set.

Every alert includes `severity`, `service`, `owner`, and `category`, plus a
stable summary and authenticated runbook URL. Notifications allowlist those
fields and controlled target labels only.

## Groups

- Availability: expected target absent/down and dead-man failure.
- Accounting: WAL append errors, Ledger queue pressure, analytics lag.
- Delivery: alert failures and drops.
- Providers: deployment unhealthy, multiple deployments unhealthy, fallback
  saturation, and capacity pressure.
- Traffic: sustained error rate with a minimum request count.
- Platform: Prometheus rule/config failures and TSDB disk pressure,
  Alertmanager notification failures, and Grafana target health.

Aggregated critical provider alerts inhibit child deployment warnings with the
same environment and cluster. Maintenance silences require owner, reason, and
expiry. `promtool test rules` fixtures cover firing, pending, recovery, low
traffic, reset, absent target, not-yet-probed deployments, and cascade cases.

An external monitor, outside the Prometheus/Alertmanager failure domain and
using a distinct notification path, must observe the `Watchdog` signal. The
production admission drill stops Prometheus and Alertmanager independently and
makes the TSDB read-only/full.
