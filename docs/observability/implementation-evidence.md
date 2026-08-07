# Repository implementation evidence

Status: Verified locally on 2026-08-03
Owner: Application
Production authority: `admission-checklist.md`

This maps repository-scoped requirements to authoritative evidence. It does
not mark target-environment Phase D gates as passed.

| Requirement | Implementation evidence | Verification |
|---|---|---|
| Metrics contract and label policy | `metrics-contract.md`, `metrics-reference.md`, `metrics_contract_test.go` | parsed exposition, unique HELP/TYPE, allowlisted labels, documentation inventory |
| Versioned credential lifecycle | `internal/bearercred` and Metrics CLI | overlap, expiry, immediate revoke, hot reload, restore non-revival |
| Credential audit integrity | `.audit` hash chain and `verify-audit` | deletion, rewrite, truncation, reorder and version-reuse tests |
| Concurrent administration | per-credential OS file lock | concurrent race test proves unique epochs and valid chain |
| Mutual workload identity | dedicated Metrics TLS config | real handshake accepts trusted client and rejects missing identity |
| Histogram idempotency | Usage checkpoint schema v3 | checkpoint/replay equality over request and attempt buckets |
| Runtime/build metrics | exporter contract | parser test protects family/type/labels and forbidden labels |
| Provider capacity | registry concurrency limits | capacity recording rule and firing fixture |
| Core rules | repository Prometheus rules | pinned `promtool` validates 8 recording and 16 alert rules plus semantic fixtures |
| Local topology | Prometheus/Alertmanager Compose plus Linux and macOS Secret mounts | service inventory is exactly Prometheus and Alertmanager; no management ports are published |
| Core runtime smoke asset | `deploy/observability/smoke.sh` | starts authenticated mock Metrics/webhook endpoints and verifies targets, rules, `Watchdog` firing delivery and Alertmanager firing/resolved state lifecycle; real Contact Point delivery remains Phase D evidence |
| Independent probe artifact | `cmd/heimdall-deadman`, `internal/deadman`, hardened systemd unit, schema and example configuration | behavior tests cover authenticated Prometheus and Alertmanager down/up transitions, durable heartbeat/retry state, recovery, freshness and invalid configuration |
| Supply chain | digest-pinned images and CI SBOM job | CI emits SPDX JSON for Prometheus, Alertmanager and the dead-man image |

## Local verification commands

```text
go test ./...
go test -race ./internal/bearercred ./internal/app ./internal/usage ./internal/provider
go test -race ./internal/deadman
go vet ./...
go build ./cmd/heimdall ./cmd/heimdall-deadman
./deploy/observability/validate.sh
./deploy/observability/smoke.sh
docker compose -f deploy/observability/compose.example.yaml config --quiet
go run ./cmd/heimdall-deadman -config deploy/observability/external-probe/config.example.yaml -check-config
```

## Not repository-verifiable

Target PKI rotation, Core management identity/RBAC, real Contact Points,
dead-man failure-domain independence and missing-heartbeat delivery, immutable
external audit anchoring, encrypted backup restore, production-sized 24-hour
soak, approved RPO/RTO and four-party sign-off remain Core No-Go until their
evidence IDs are recorded in `admission-checklist.md`.
