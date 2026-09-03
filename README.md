# Halro

[![CI](https://github.com/akz142857/Halro/actions/workflows/ci.yml/badge.svg)](https://github.com/akz142857/Halro/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/akz142857/Halro)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Halro is a single-binary, security-first LLM gateway. It gives applications
one governed API for multiple model providers while keeping upstream
credentials, budgets, routing, redaction, audit, and usage accounting inside a
locally owned control boundary.

- One Go binary with an embedded Chinese/English Admin console
- No external database, cache, CDN, or browser-side secret storage
- OpenAI-compatible Chat, Embeddings, and Stateless Responses
- Anthropic-compatible Messages with portable and native routing
- Versioned, capability-aware Provider Profiles and fail-closed transport
- Durable accounting, audit integrity, anomaly containment, and redaction
- Authenticated Prometheus metrics, Alertmanager rules, and an independent
  dead-man monitor

> [!IMPORTANT]
> Chat, Embeddings, Stateless Responses, and Anthropic Messages have published
> compatibility contracts. Phase 2 media/resource endpoints are
> **Experimental**. Realtime WebSocket/WebRTC, stateful Responses, and
> `/v1/models` are not implemented.

## Quick start

### Requirements

- Go 1.26.6 or later
- Node.js/npm only when rebuilding the embedded Admin console

Start a new loopback-only local instance:

```bash
make start
```

On first run, Halro creates `config.yaml`, initializes encrypted local
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
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

For SDK examples and the complete Admin workflow, read the
[User Guide](docs/guides/user-guide.md), also available in
[简体中文](docs/guides/user-guide.zh-CN.md). For deployment, upgrades, backup,
recovery, and hardening, use the [Operator Guide](docs/guides/operator-guide.md).

## Run with Docker

Every tag publishes multi-architecture images (`linux/amd64`, `linux/arm64`):

```bash
docker pull ghcr.io/akz142857/halro:v0.6.0
docker pull ghcr.io/akz142857/halro-deadman:v0.6.0   # the independent watchdog, deployed separately
```

`latest` follows the newest tag; pin by digest for anything you deploy:

```text
ghcr.io/akz142857/halro@sha256:aaa3bfa22ea0bb032bddccacd2ed2f4fda978165db14d5ce3198601f51e29f91
```

The runtime is distroless with no shell, runs as uid 65532, and its
`ENTRYPOINT` is the `halro` binary. The default command is
`serve --config /etc/halro/config.yaml`, and `serve` never initializes storage,
so the first run is an explicit `init`.

Configuration is never merged with built-in defaults — a partial file is
rejected key by key — so start from the complete annotated
[`configs/config.example.yaml`](configs/config.example.yaml), or from the
`config.yaml` that a local `make start` writes, and change the three values
whose defaults are wrong for a container:

```yaml
server:
  # Container loopback would refuse every request arriving through -p.
  gateway_listen: "0.0.0.0:8080"
storage:
  # A child of the mount; the mount point itself must not be the data directory.
  data_dir: "/var/lib/halro/data"
  master_key:
    file: "/run/secrets/halro-master.key"
```

Initialize once, with `/run/secrets` writable so `init` can create the
file-mode Master Key at `0600`:

```bash
mkdir -p ./halro-secrets
sudo chown 65532:65532 ./halro-secrets && chmod 700 ./halro-secrets
docker volume create halro-data

docker run --rm --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
  -v "$PWD/halro-secrets:/run/secrets" \
  -v halro-data:/var/lib/halro \
  ghcr.io/akz142857/halro:v0.6.0 init --config /etc/halro/config.yaml
```

Then serve, publishing only the Gateway on host loopback:

```bash
docker run -d --name halro --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
  -v "$PWD/halro-secrets:/run/secrets:ro" \
  -v halro-data:/var/lib/halro \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/akz142857/halro:v0.6.0 serve --config /etc/halro/config.yaml \
    -allow-insecure-public-listen
```

`-allow-insecure-public-listen` covers Gateway plaintext only and exists
because of the `0.0.0.0` bind above — neither alone is enough, and together
they describe a host-local development boundary, not a public one. Admin and
Metrics stay on container loopback in this shape; publish them only after
enabling TLS and mounting the certificate, key, and Metrics client CA.

Halro has no built-in certificate issuance: `tls.certificates` names keypairs
it loads, and obtaining or renewing them belongs to whatever already does that
here — certbot, an internal CA, or a reverse proxy such as Caddy that terminates
TLS in front of a loopback-bound Halro. Replacing the files and sending `SIGHUP`
swaps the certificate without a restart. Both shapes, and which settings each
one requires, are in
[TLS and inbound exposure](docs/guides/operator-guide.md#tls-and-inbound-exposure).

Two container facts that bite:

- **One replica, always.** Halro is single-writer over one data directory.
  Mount the persistent parent at `/var/lib/halro`, and give Kubernetes
  `replicas: 1` with a `Recreate` strategy — never a rolling update.
- **`healthy` is not reachability.** `HEALTHCHECK` calls a readiness URL from
  inside the container, so it proves the process is ready, not that a published
  port, certificate name, firewall, or reverse proxy works. Probe the external
  address separately, and set `HALRO_HEALTH_URL` to an HTTPS name the mounted
  certificate covers once TLS is on.

Registry images carry no cosign signature or registry attestation. The signed
and attested artifacts are the release archives, so when you need that
provenance chain verify `halro-container-<arch>.tar.gz` as described in
[Verify release downloads](#verify-release-downloads) and load it directly
instead of pulling:

```bash
gzip -dc halro-container-amd64.tar.gz | docker load
```

Full container guidance — TLS, key custody, the backup and restore layout, and
the Kubernetes manifest — is in the
[Operator Guide](docs/guides/operator-guide.md#optional-container-image).

## Data durability and encrypted backup

Container or Pod replacement is safe only when the complete
`storage.data_dir` remains on persistent storage. Halro is currently a
single-writer service: Docker/Kubernetes deployments must run one instance,
and Kubernetes must use `replicas: 1` with a `Recreate` strategy.

File-mode `master.key` is not stored in the encrypted backup. Keep it outside
the data directory and back it up independently from both the `.hmbk` archive
and its dedicated 32-byte backup key. Never restore only `halro.db` or mix
the database, Ledger, Audit, Usage, and Provider-object files from different
snapshots.

Backups are deliberately offline: stop Halro, create the archive, verify it,
and regularly perform an isolated restore drill. For containers, mount the
persistent parent directory and configure `storage.data_dir` as its child so
restore can atomically rename the data directory on the same filesystem.

With Halro stopped, the repository helper creates and immediately verifies
an encrypted backup:

```bash
make backup
```

It defaults to `config.yaml`, `backups/`, and the git-ignored `backup.key`.
Production operators should provide independent locations explicitly:

```bash
make backup \
  CONFIG=/etc/halro/config.yaml \
  BACKUP_DIR=/secure-backups \
  BACKUP_KEY_FILE=/secure-secrets/halro-backup.key
```

The helper generates the Backup Key with mode `0600` when it is absent, runs
offline `doctor`, creates the archive, and runs `backup verify`. Move or escrow
the key independently; deleting it makes every archive encrypted with it
unrecoverable.

See [Encrypted backup and restore](docs/guides/backup-restore.md) for Docker/Kubernetes
layouts, upgrade sequencing, key custody, retention, and recovery commands.

## Verify release downloads

Verify the signed checksum manifest before trusting it, use that manifest to
check all downloaded bytes, and then verify every artifact bundle against the
exact release workflow identity:

Requires cosign v2.2 or newer (`--bundle` reads the new Sigstore bundle format).

```bash
# Download everything: checksums.txt lists every published artifact, and both
# the checksum check and the loop below expect the files to be present.
gh release download v0.6.0 --repo akz142857/Halro   # or download all assets by hand

COSIGN_IDENTITY='https://github.com/akz142857/Halro/.github/workflows/release.yml@refs/tags/v0.6.0'
COSIGN_ISSUER='https://token.actions.githubusercontent.com'
cosign verify-blob \
  --certificate-identity "$COSIGN_IDENTITY" \
  --certificate-oidc-issuer "$COSIGN_ISSUER" \
  --bundle checksums.txt.sigstore.json checksums.txt
# --ignore-missing lets you verify only the platform you downloaded; drop it to
# require every listed file. Without either, a partial download exits non-zero.
sha256sum --check --ignore-missing checksums.txt  # macOS: shasum -a 256 --check --ignore-missing checksums.txt
for artifact in halro-* halro.spdx.json; do
  case "$artifact" in *.sigstore.json) continue ;; esac
  [ -f "$artifact.sigstore.json" ] || continue
  cosign verify-blob \
    --certificate-identity "$COSIGN_IDENTITY" \
    --certificate-oidc-issuer "$COSIGN_ISSUER" \
    --bundle "$artifact.sigstore.json" "$artifact"
done
```

Do not verify the blobs against an unsigned `checksums.txt`, and do not replace
the exact tag identity with a branch identity — substitute the tag you actually
downloaded. Release candidates use their own exact `refs/tags/vX.Y.Z-rc.N`
identity.

## API status

| API | Status | Notes |
|---|---|---|
| `POST /v1/chat/completions` | Compatible | JSON and SSE; OpenAI Go/Node/Python SDK matrix |
| `POST /v1/embeddings` | Compatible | Per-Profile coverage applies: OpenAI, Azure OpenAI, Gemini Beta, and the OpenAI-compatible Profile |
| `POST /v1/responses` | Compatible subset | Stateless Create and text SSE, plus `background: true` deferred submission; `store:true` and stateful fields are rejected |
| `GET`/`POST .../cancel`/`DELETE /v1/responses/{id}` | Experimental | Deferred retrieval, cancel, and delete for `background: true`; per-Project opt-in, answers sealed and retained at most 24 h ([ADR 0024](docs/adr/0024-deferred-response-tier.md)) |
| `POST /v1/messages` | Compatible | Anthropic JSON/SSE; portable or exact native Profile routing |
| `POST /v1/messages/count_tokens` | Compatible | Anthropic token counting over the same Profile routing |
| Moderations, Images, Audio | Experimental | Strict Phase 2 endpoint and Provider Profile contracts |
| Files and Batches | Experimental | Project-scoped opaque IDs, idempotency, ownership, private local objects |
| `/v1/rerank` and Async Invoke | Not served in this build | Halro extensions whose only backing Profiles — Cohere Rerank 3.5 on Bedrock Agent Runtime, Nova Reel Async on Bedrock Runtime — are withheld, so no Deployment can be created for either |
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
| Anthropic | Native Messages JSON/SSE, `count_tokens`, tools, and signed Thinking round-trip; no embeddings |
| Azure OpenAI | Deployment-scoped Chat/stream/embeddings with an explicitly pinned API version |
| DeepSeek | OpenAI-compatible Chat/stream with reasoning and cache-tier usage; no embeddings |
| OpenAI-compatible | Chat/stream/embeddings with explicitly declared optional capabilities |
| MiniMax | One host and one key across three Profiles: Chat, Stateless Responses (no streaming), and Anthropic Messages |
| Kimi | One key across the connection group: Chat (the only face reaching all published models) and Anthropic Messages; its Responses Profile is withheld |
| Gemini Beta | Native text generation/SSE and float embeddings translation |
| AWS Bedrock Mantle Beta | Isolated OpenAI Chat, Stateless Responses, and Anthropic Messages Profiles |

Bedrock is offered through Mantle alone. The five Bedrock Runtime and Agent
Runtime Profiles — Converse, Titan Text Embeddings V2, Titan Image V2, Cohere
Rerank 3.5, and Nova Reel Async — are **withheld** in this build: the
implementation is present, but the served Profile matrix omits them and every
write path refuses them, so no Deployment can be created against one. An install
that already holds such a connection still starts and can delete it.

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
- Prompts and response bodies never reach a log, a metric, or an audit record.
  Two opt-in stores are the only places caller-written content is kept at rest,
  and both change what the data directory contains, so enabling either is a
  decision rather than a default: `gateway.failure_capture` retains the request
  and upstream answer of a *failed* call, and the per-Project deferred tier
  (`background: true`) retains the answer of a *successful* one. Both are sealed
  under the Master Key, bound to their request and Project, bounded in size and
  count, and swept on expiry. A captured failure payload is readable only
  through an audited admin action; a deferred answer is readable only by the
  Project that submitted it, within its retention window.

See the [Threat Model](docs/architecture/threat-model.md),
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

Halro currently runs as a standalone, single-writer system backed by bbolt
metadata, an authoritative Ledger WAL, private local objects, and Parquet usage
partitions. HA/Cluster and Realtime material in the architecture documents is
future, gated design—not a statement of current runtime support.

## Observability

Halro exposes an authenticated Prometheus-format Metrics endpoint. The
repository includes a versioned single-host Core reference deployment made up
of Prometheus and Alertmanager, together with recording rules, alert rules,
rule tests, configuration validation, and an isolated runtime smoke test.
Prometheus is the metrics and alert authority.

The `halro-deadman` binary is deployed in a separate failure domain. It
checks Halro, Prometheus, and Alertmanager readiness, verifies Prometheus
sample freshness, and sends durable heartbeat and down/up events to an
independent receiver.

Validate the repository-provided configuration before deployment:

```bash
make observability-check
```

Secret preparation, Linux and macOS Compose commands, the independent dead-man
contract, and production boundaries are documented in the
[Prometheus/Alertmanager deployment guide](deploy/observability/README.md).

## Documentation

### Use and operations

- [User Guide](docs/guides/user-guide.md) · [中文使用手册](docs/guides/user-guide.zh-CN.md)
- [Operator Guide](docs/guides/operator-guide.md)
- [Backup and restore](docs/guides/backup-restore.md)
- [异步提交与延迟取回](docs/guides/deferred-responses.zh-CN.md) — `background: true`
- [Choosing an AWS access surface](docs/guides/aws-surface-selection.md)
- [Metrics reference](docs/contracts/metrics-reference.md)
- [Prometheus/Alertmanager deployment](deploy/observability/README.md)
- [Observability operations runbook](docs/observability/operations-runbook.md)
- [Webhook payloads](docs/contracts/webhook-payloads.md)

### Contracts and implementation evidence

- [Implementation status](docs/milestones/implementation-status.md)
- [Endpoint compatibility manifests](docs/compatibility/README.md)
- [OpenAI compatibility contract](docs/contracts/openai-compatibility.md)
- [Provider capability contract](docs/contracts/provider-capabilities.md)
- [Gateway idempotency contract](docs/contracts/idempotency-contract.md)
- [Crash recovery matrix](docs/verification/crash-recovery-matrix.md)
- [Provider real-test matrix](docs/verification/provider-real-matrix.md)

### Architecture and governance

- [多协议 LLM API、Provider 与 Realtime 架构设计](docs/architecture/api-provider-realtime-architecture.zh-CN.md)
- [Distributed evolution and state ownership](docs/architecture/distributed-state-ownership.md)
- [Architecture Decision Records](docs/adr/)
- [Threat Model](docs/architecture/threat-model.md)
- [Audit integrity](docs/contracts/audit-integrity.md)

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
go build -trimpath -o bin/halro ./cmd/halro
```

Real Provider smoke tests are opt-in and may be billable. They require isolated
test credentials, explicit environment flags, and hard account budgets; see
the [Provider real-test matrix](docs/verification/provider-real-matrix.md).

Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. Release
process and evidence gates are documented in [docs/guides/releasing.md](docs/guides/releasing.md).

## Community and license

Use GitHub Issues for bug reports and feature proposals. Security
vulnerabilities must follow [SECURITY.md](SECURITY.md) and must not be posted in
a public issue. Community expectations are in
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), and support guidance is in
[SUPPORT.md](SUPPORT.md).

Halro is licensed under the [Apache License 2.0](LICENSE). Third-party
attributions are documented in [NOTICE](NOTICE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
