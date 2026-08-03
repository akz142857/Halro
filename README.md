# Heimdall

[![CI](https://github.com/akz142857/Heimdall/actions/workflows/ci.yml/badge.svg)](https://github.com/akz142857/Heimdall/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/akz142857/Heimdall)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Heimdall is a single-binary, security-first LLM gateway. It gives applications
one governed API for multiple model providers while keeping upstream
credentials, budgets, routing, redaction, audit, and usage accounting inside a
locally owned control boundary.

- One Go binary with an embedded Chinese/English Admin console
- No external database, cache, CDN, or browser-side secret storage
- OpenAI-compatible Chat, Embeddings, and Stateless Responses
- Anthropic-compatible Messages with portable and native routing
- Versioned, capability-aware Provider Profiles and fail-closed transport
- Durable accounting, audit integrity, anomaly containment, and redaction

> [!IMPORTANT]
> Chat, Embeddings, Stateless Responses, and Anthropic Messages have published
> compatibility contracts. Phase 2 media/resource endpoints are
> **Experimental**. Realtime WebSocket/WebRTC, stateful Responses,
> `/v1/models`, and Anthropic `count_tokens` are not implemented.

## Quick start

### Requirements

- Go 1.26.5 or later
- Node.js/npm only when rebuilding the embedded Admin console

Start a new loopback-only local instance:

```bash
make start
```

On first run, Heimdall creates `config.yaml`, initializes encrypted local
storage, and prints the Admin URL. Open `/admin/setup` to create the first
administrator, then configure this path in the console:

```text
Credential → Provider → Deployment → Route → Project → Gateway Key
```

The Gateway Key is displayed once. Applications use that `gw_...` key and a
public model alias; they never receive the Provider credential or upstream
model identifier.

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $HEIMDALL_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

For SDK examples and the complete Admin workflow, read the
[中文使用手册](docs/user-guide.zh-CN.md). For deployment, upgrades, backup,
recovery, and hardening, use the [Operator Guide](docs/operator-guide.md).

## API status

| API | Status | Notes |
|---|---|---|
| `POST /v1/chat/completions` | Compatible | JSON and SSE; OpenAI Go/Node/Python SDK matrix |
| `POST /v1/embeddings` | Compatible | Per-Profile coverage applies; Titan Text Embeddings V2 remains Experimental |
| `POST /v1/responses` | Compatible subset | Stateless Create and text SSE; `store:true` and stateful fields are rejected |
| `POST /v1/messages` | Compatible | Anthropic JSON/SSE; portable or exact native Profile routing |
| Moderations, Images, Audio | Experimental | Strict Phase 2 endpoint and Provider Profile contracts |
| Files and Batches | Experimental | Project-scoped opaque IDs, idempotency, ownership, private local objects |
| `/v1/rerank` and Async Invoke | Experimental | Heimdall extensions backed by isolated Bedrock Profiles |
| Realtime WebSocket/WebRTC | Not implemented | Architecture only; no Realtime data plane is claimed |

The authoritative machine-readable contract is
[`docs/compatibility/endpoint-manifests.json`](docs/compatibility/endpoint-manifests.json).
Endpoint maturity does not promote every attached Provider Profile: each
`profile_coverage` entry carries its own status and field-level deviations.

## Providers

Every upstream is bound to an immutable, versioned Profile that fixes its
Access Surface, operations, credential scheme, and capability evidence.

| Provider | Current scope |
|---|---|
| OpenAI | Chat, streaming, embeddings, Stateless Responses, and Experimental Phase 2 media/resources |
| Anthropic | Native Messages JSON/SSE, tools, and signed Thinking round-trip; no embeddings or `count_tokens` |
| Azure OpenAI | Deployment-scoped Chat/stream/embeddings with an explicitly pinned API version |
| DeepSeek | OpenAI-compatible Chat/stream; embeddings disabled by default |
| OpenAI-compatible | Chat/stream/embeddings with explicitly declared optional capabilities |
| Gemini Beta | Native text generation/SSE and float embeddings translation |
| AWS Bedrock Beta | Converse, Titan Text Embeddings V2, Titan Image V2, Cohere Rerank 3.5, and Nova Reel Async through isolated Runtime/Agent Runtime Profiles |
| AWS Bedrock Mantle Beta | Isolated OpenAI Chat, Stateless Responses, and Anthropic Messages Profiles |

Provider capabilities and per-field coverage are checked before Provider I/O.
Unknown or unsupported fields are rejected instead of silently dropped.
Runtime, Agent Runtime, and Mantle credentials, audiences, quotas, concurrency,
and capability evidence are not merged.

## Security and correctness boundaries

- Provider credentials are encrypted with AES-256-GCM and audience-bound AAD.
- Gateway Keys are 256-bit random values stored only as SHA-256 representations.
- Outbound transport is HTTPS-only, allowlisted, DNS/IP validated, dial-pinned,
  redirect-free, and independent of environment proxies.
- Projects enforce allowed routes, budget reservations, RPM/TPM/concurrency,
  CIDR rules, redaction policy, and Token Guard policy before upstream access.
- Accounting uses a framed, checksummed WAL with conservative unknown-outcome
  settlement; retries never assume an ambiguous Provider request was absent.
- Files, Batches, and Async resources keep project-scoped owner mappings pinned
  to Provider, Deployment, Profile, and Region.
- Admin secrets and CSRF state are never persisted in browser storage; the
  production bundle is scanned for secret canaries and persistence APIs.

See the [Threat Model](docs/threat-model.md),
[Gateway correctness contract](docs/contracts/gateway-correctness.md), and
[Security Policy](SECURITY.md) for the complete boundary.

## Architecture

```text
Client / SDK
    │  Gateway Key + public model alias
    ▼
Protocol Facade ──► Semantic operation + governance requirements
    │
    ▼
Auth / policy / budget / redaction / accounting
    │
    ▼
Capability-aware router ──► versioned Provider Primitive
    │
    ▼
SafeTransport ──► OpenAI / Anthropic / Azure / DeepSeek / Gemini / Bedrock
```

Heimdall currently runs as a standalone, single-writer system backed by bbolt
metadata, an authoritative Ledger WAL, private local objects, and Parquet usage
partitions. HA/Cluster and Realtime material in the architecture documents is
future, gated design—not a statement of current runtime support.

## Documentation

### Use and operations

- [中文使用手册](docs/user-guide.zh-CN.md)
- [Operator Guide](docs/operator-guide.md)
- [Backup and restore](docs/backup-restore.md)
- [Metrics reference](docs/metrics-reference.md)
- [Prometheus/Grafana deployment](deploy/observability/README.md)
- [Observability operations runbook](docs/observability/operations-runbook.md)
- [Webhook payloads](docs/webhook-payloads.md)

### Contracts and implementation evidence

- [Implementation status](docs/implementation-status.md)
- [Endpoint compatibility manifests](docs/compatibility/README.md)
- [OpenAI compatibility contract](docs/contracts/openai-compatibility.md)
- [Provider capability contract](docs/contracts/provider-capabilities.md)
- [Gateway idempotency contract](docs/idempotency-contract.md)
- [Crash recovery matrix](docs/crash-recovery-matrix.md)
- [Provider real-test matrix](docs/provider-real-matrix.md)

### Architecture and governance

- [多协议 LLM API、Provider 与 Realtime 架构设计](docs/api-provider-realtime-architecture.zh-CN.md)
- [Distributed evolution and state ownership](docs/distributed-state-ownership.md)
- [Architecture Decision Records](docs/adr/)
- [Threat Model](docs/threat-model.md)
- [Audit integrity](docs/audit-integrity.md)

## Development

Run the local quality gates:

```bash
go test ./...
go test -race ./...
go vet ./...
```

The Admin console supports standard TOTP two-factor authentication with
multiple independently revocable authenticators and one-time recovery codes.
It works with Microsoft Authenticator, Google Authenticator, 1Password, and
other compatible apps without contacting those vendors.

Rebuild and verify the embedded Admin console:

```bash
cd web
npm ci --ignore-scripts
npm run typecheck
npm test -- --run
npm run build
cd ..
go build -trimpath -o bin/heimdall ./cmd/heimdall
```

Real Provider smoke tests are opt-in and may be billable. They require isolated
test credentials, explicit environment flags, and hard account budgets; see
the [Provider real-test matrix](docs/provider-real-matrix.md).

Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. Release
process and evidence gates are documented in [docs/releasing.md](docs/releasing.md).

## Community and license

Use GitHub Issues for bug reports and feature proposals. Security
vulnerabilities must follow [SECURITY.md](SECURITY.md) and must not be posted in
a public issue. Community expectations are in
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), and support guidance is in
[SUPPORT.md](SUPPORT.md).

Heimdall is licensed under the [Apache License 2.0](LICENSE). Third-party
attributions are documented in [NOTICE](NOTICE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
