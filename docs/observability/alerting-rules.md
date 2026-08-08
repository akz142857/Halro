# Alerting contract

Status: Accepted
Owner: SRE

Prometheus rule files under `deploy/observability/prometheus/` are the only
alert evaluation authority.

Every alert includes `severity`, `service`, `owner`, and `category`, plus a
stable summary and authenticated runbook URL. Notifications allowlist those
fields and controlled target labels only.

## Core groups

- Availability: expected Halro target absent/down and the continuous
  `Watchdog` dead-man signal.
- Accounting: WAL append errors, Ledger queue pressure and analytics lag.
- Delivery: application alert failures and drops.
- Providers: deployment unhealthy, multiple deployments unhealthy, fallback
  saturation and capacity pressure.
- Traffic: sustained error rate with a minimum request count.
- Key custody: KMS unwrap errors, Recovery readiness/verification/use, Vault
  mismatch, and rotations left pending. Permanent cold-start failures are also
  covered by expected-target loss because an unavailable process cannot expose
  its own failure counter.
- Platform: Prometheus rule/config failures and TSDB disk pressure, plus
  Alertmanager notification failures.

Aggregated critical provider alerts inhibit child deployment warnings with the
same environment and cluster. Maintenance silences require owner, reason and
expiry. `promtool test rules` fixtures cover firing, pending, recovery, low
traffic, reset, absent target, not-yet-probed deployments and cascade cases.
KMS response procedures are in
[`docs/runbooks/m11-production-operations.md`](../runbooks/m11-production-operations.md).

## Formal dead-man contract

The production dead-man has two complementary signals outside the
Prometheus/Alertmanager failure domain:

1. Prometheus continuously evaluates `Watchdog`; Alertmanager routes it to a
   dedicated external receiver. The receiver alarms when the signal is absent
   beyond its approved grace window and reports recovery when it resumes.
2. An independently deployed synthetic probe directly checks authenticated
   Prometheus and Alertmanager readiness, emits down/up state transitions, and
   emits its own heartbeat. Its receiver alarms if the probe heartbeat stops.

The grace window must be longer than expected evaluation, grouping, delivery
and retry jitter but shorter than the approved detection objective. Probe and
receiver clocks must be monitored. The probe host/process, state, credentials,
network path, receiver and final Contact Point must not share Core's process,
storage, identity, only network path, or only notification channel.

Production admission stops and restores Prometheus and Alertmanager
separately, then stops the probe, and records each firing, delivery and recovery
through the independent path. A versioned probe artifact or a successful
repository test proves behavior, not failure-domain independence; the latter
requires target-environment evidence.
