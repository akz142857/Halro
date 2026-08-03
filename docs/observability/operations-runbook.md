# Observability operations runbook

Status: Phase A1 baseline
Owner: SRE

## First response

1. Confirm the expected target exists and inspect Prometheus `up` and scrape
   error without printing authorization headers.
2. Check Heimdall `/health/live` and `/health/ready` through their approved
   paths.
3. Check WAL errors and Ledger queue before treating Usage/Grafana values as
   authoritative.
4. Compare request, attempt, fallback, deployment health, and capacity pressure
   to distinguish upstream degradation from gateway saturation.
5. Check Prometheus rule evaluation, Alertmanager notification, TSDB disk, and
   the independent dead-man monitor.

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

Restore Prometheus/Grafana data only from encrypted, authorized backups. Restore
Metrics credential lifecycle state from a snapshot at or after the latest
revocation watermark; otherwise keep credentials revoked and rotate anew.

## Required drills

- Heimdall and expected-target loss;
- WAL append error and analytics lag;
- multiple deployment failure and fallback saturation;
- Prometheus/Alertmanager stop and TSDB full/read-only;
- credential and certificate rotation/revocation;
- unauthorized local process, SSRF, and audit tampering.

Record target-environment outcomes in `admission-checklist.md`. A repository
test result is supporting evidence only and cannot close a production gate.
