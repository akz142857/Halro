# Observability operations runbook

Status: Phase A1 baseline
Owner: SRE

## First response

1. Confirm the expected target exists and inspect Prometheus `up` and scrape
   error without printing authorization headers.
2. Check Heimdall `/health/live` and `/health/ready` through their approved
   paths.
3. Check WAL errors and Ledger queue before treating Usage values as
   authoritative.
4. Compare request, attempt, fallback, deployment health, and capacity pressure
   to distinguish upstream degradation from gateway saturation.
5. Check Prometheus rule evaluation, Alertmanager notification, TSDB disk, and
   the independent dead-man monitor.

Use the authenticated, audited Prometheus query path to inspect `up`, the
`heimdall:*` recording rules and the source `heimdall_*` series. Do not enable
a public Prometheus UI or unrestricted API. Useful checks include:

- target health: `up{job="heimdall"}`;
- request/error traffic: `heimdall:requests:rate5m` and
  `heimdall:requests_errors:ratio5m`;
- tail latency: `histogram_quantile(0.95, sum by (le, environment, region,
  cluster) (rate(heimdall_request_latency_seconds_bucket[5m])))`;
- accounting queues: `heimdall:ledger_queue:ratio` and
  `heimdall_usage_analytics_lagging`;
- provider pressure: `heimdall:deployments_unhealthy:count` and
  `heimdall:deployment_capacity:ratio`.

## Credential rotation

1. Generate a new active Metrics credential with a bounded overlap.
2. Atomically replace Prometheus's `0400/0440` credential file and reload.
3. Require `up == 1` for two scrape intervals.
4. Revoke the retiring credential.
5. Run `heimdall metrics verify-audit --config <path>` and send its returned
   sequence/hash chain head to the independent immutable audit platform.
6. Prove the old token receives 401 and record the secret-free audit evidence
   and independently anchored chain hash.

Never put the token in a command argument, environment variable, ticket,
dashboard, screenshot, or shell history.

## Metrics certificate rotation

1. Issue the replacement server certificate and Prometheus client certificate
   from the approved CA, retaining the old trust path for the overlap window.
2. Atomically replace the mounted files with mode `0400/0440`.
3. Perform a controlled Heimdall restart; Metrics TLS material is intentionally
   loaded at listener startup, so replacing files alone is not a reload.
4. Require two successful mTLS scrapes, then remove the old client identity and
   trust anchor and restart again if the CA bundle changed.
5. Prove missing, expired, and wrong-CA clients fail and archive the chain head,
   restart, scrape, and rollback evidence.

## Recovery

Restore Prometheus, Alertmanager configuration/state and Metrics identity state
only from encrypted, authorized backups. Restore credential lifecycle state
from a snapshot at or after the latest revocation watermark; otherwise keep
credentials revoked and rotate anew. If TSDB is formally classified as a
disposable cache, rebuild it according to the approved RPO instead of silently
claiming historical recovery.

## Independent dead-man drill

1. Confirm the external receiver is receiving `Watchdog` and the synthetic
   probe heartbeat. Record their last-seen timestamps without exposing webhook
   credentials.
2. Stop Prometheus. Require both missing `Watchdog` and the direct Prometheus
   probe to alarm within the approved detection objective; restore Prometheus
   and require both signals to recover.
3. Stop Alertmanager while Prometheus remains healthy. Require missing
   `Watchdog` and the direct Alertmanager probe to alarm; restore Alertmanager
   and require recovery.
4. Stop the external probe. Require the receiver's probe-heartbeat alarm, then
   restore the probe and require recovery.
5. Demonstrate that Core host/storage loss and Core notification credentials
   cannot suppress or authorize the independent path. Archive timestamps,
   payload hashes, delivery receipts and audit IDs in immutable evidence.

Never deploy the external probe beside Core as production evidence. It must use
authenticated HTTPS checks and a dedicated receiver credential; it must not
reuse Alertmanager's operational or Watchdog webhook identity.

## Local Core runtime smoke

Run `./deploy/observability/smoke.sh` on a development host where loopback ports
9090, 9091, 9093 and 19094 are unused. The script uses a unique Compose project,
temporary local-only credentials, an authenticated mock Metrics target and a
mock webhook. It verifies Core target health, loaded rules, the continuous
`Watchdog` and an Alertmanager API firing/resolved lifecycle. Cleanup removes only the
unique smoke project and temporary files.

If a development service already owns one of those ports, do not stop it just
for this check. Set `HEIMDALL_SMOKE_METRICS_PORT`,
`HEIMDALL_SMOKE_PROMETHEUS_PORT`, `HEIMDALL_SMOKE_ALERTMANAGER_PORT`, and
`HEIMDALL_SMOKE_WEBHOOK_PORT` to four distinct unused ports in `1024..65535`.
The smoke creates runtime-only config copies with those addresses; shipped Core
configuration and the existing service are not modified.

This is repository/runtime regression evidence only. It does not exercise
target PKI, the real Contact Point, an independent dead-man receiver, immutable
audit, backup/restore, production capacity or four-party sign-off, and therefore
cannot change any Phase D checklist row from `BLOCKED`.

## Required drills

- Heimdall and expected-target loss;
- WAL append error and analytics lag;
- multiple deployment failure and fallback saturation;
- Prometheus/Alertmanager stop and TSDB full/read-only;
- external probe stop and Watchdog/probe-heartbeat loss;
- credential and certificate rotation/revocation;
- unauthorized local process, SSRF, and audit tampering.

Record target-environment outcomes in `admission-checklist.md`. A repository
test result is supporting evidence only and cannot close a production gate.
