# Repository implementation evidence

Status: Verified locally on 2026-08-03
Owner: Application
Production authority: `admission-checklist.md`

This maps repository-scoped requirements to authoritative evidence. It does
not mark target-environment Phase D gates as passed.

| Requirement | Implementation evidence | Verification |
|---|---|---|
| Metrics contract and label policy | `metrics-contract.md`, `metrics-reference.md`, `metrics_contract_test.go` | parsed exposition, unique HELP/TYPE, allowlisted labels, documentation inventory |
| Versioned credential lifecycle | `internal/metricsauth` and Metrics CLI | overlap, expiry, immediate revoke, hot reload, restore non-revival |
| Credential audit integrity | `.audit` hash chain and `verify-audit` | deletion, rewrite, truncation, reorder and version-reuse tests |
| Concurrent administration | per-credential OS file lock | concurrent race test proves unique epochs and valid chain |
| Mutual workload identity | dedicated Metrics TLS config | real handshake accepts trusted client and rejects missing identity |
| Histogram idempotency | Usage checkpoint schema v3 | checkpoint/replay equality over request and attempt buckets |
| Runtime/build metrics | exporter contract | parser test protects family/type/labels and forbidden labels |
| Provider capacity | registry concurrency limits | capacity recording rule and firing fixture |
| Rules | repository Prometheus rules | pinned `promtool` validates 8 recording and 16 alert rules plus fixtures |
| Dashboard provisioning | four fixed-UID dashboards | pinned Grafana 12.1.0 started empty; API returned all four dashboards |
| Local topology | Compose and validation script | loopback profile, no published management ports, secret scan, container hardening |
| Independent probe artifact | separate probe Compose profile and heartbeat/state-transition script | HTTPS-only targets, dedicated curl-config credential, shell/Compose validation |
| Supply chain | digest-pinned images and CI SBOM job | CI emits SPDX JSON for all three monitoring images |

## Local verification commands

```text
go test ./...
go test -race ./internal/metricsauth ./internal/app ./internal/usage ./internal/provider
go vet ./...
go build ./cmd/heimdall
./deploy/observability/validate.sh
docker compose -f deploy/observability/compose.example.yaml config --quiet
```

## Not repository-verifiable

Target PKI rotation, organization SSO/RBAC, independent dead-man notification,
immutable external audit anchoring, encrypted backup restore, production-sized
24-hour soak, approved RPO/RTO, and four-party sign-off remain No-Go until their
evidence IDs are recorded in `admission-checklist.md`.
