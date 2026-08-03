# Implementation status

Last updated: 2026-08-03

This file records implementation evidence for the current source tree. Release
scope is governed by reviewed release notes and release gates.

## Completed foundation

| Area | Evidence |
|---|---|
| Go module and build | `go.mod`, `cmd/heimdall`, successful `go build -trimpath` |
| Strict configuration | `internal/config`, unknown-field and listener-policy tests |
| Safe listener defaults | public plaintext Gateway rejection; Admin cannot use the Gateway override |
| Runtime lifecycle | three listener skeleton, readiness/liveness, signal drain and resource close |
| Exclusive data ownership | `internal/store/lock`, second-process rejection test |
| Metadata | copy-on-write bbolt schema migrations through v10, migration history, 41 mutation/commit-boundary rollback injections with safe retry, and revision conflict tests |
| Master key | provider-neutral `Unlocker` boundary across Runtime/init/bootstrap/Admin/Audit/Metrics/doctor/backup/restore/rotate; strict final `storage.master_key` File/Key-Slot schema with zero-I/O config check; File backend atomic create/replace, `0600`, no-follow load and wrong-key startup check; versioned provider-neutral Key Slot descriptor with Primary/Recovery purposes, pending/active/retiring/revoked state machine, descriptor/Slot CAS, idempotency, last-usable guard, production-ready invariant, secret-safe Audit transition evidence, real Vault Key Check activation boundary, COW rollback and revoked-material compaction evidence; persistent versioned keyring; offline COW rotation with per-Credential re-encryption, stable protected Audit key, Admin-session invalidation, compaction, idempotent crash bridge, repeated rotation, and nine kill-point recoveries |
| Credential vault | HKDF-SHA256 + AES-256-GCM + audience-bound AAD |
| Gateway key | 256-bit random key, one-way SHA-256 representation |
| Auth hot path | immutable in-memory key/project snapshot |
| Ledger | framed WAL, checksum, partial-tail recovery, bounded group commit with per-batch fsync, writer-locked committed-prefix snapshots |
| Accounting correctness | one atomic Attempt settlement contains reservation delta, cost, and tokens |
| Replay | monotonic sequence, stable Event ID idempotency, pending reservation recovery |
| Accounting health | Healthy/Degraded/Unavailable/RecoveryRequired status and readiness behavior |
| Safe logs | structured attribute and value redaction baseline |
| SafeTransport | host allowlist, HTTPS policy, all-answer IP validation, pinned dial, no env proxy, no redirects |
| OpenAI-compatible API | strict chat-completions and embeddings contracts, stable errors, body limits |
| Stateless Responses Phase 1A | strict `POST /v1/responses` JSON and typed text SSE facade; state/resource fields fenced before Provider I/O; canonical Generate/ProviderPrimitive reuse; deterministic event lifecycle; machine-readable per-profile manifest; official Go/Node/Python SDK coverage |
| Anthropic Messages Phase 1B | strict `POST /v1/messages` JSON and native SSE facade; required Anthropic version header and errors; portable canonical routing or native profile pinning; Tool Use/Tool Result and signed Thinking round-trip; OpenAI/Anthropic/Gemini tool-choice matrix; official Go/Node/Python Anthropic SDK coverage |
| SSE streaming | bounded SSE parser, semantic chunks, `[DONE]`, usage settlement, first-payload error boundary |
| Resilient routing | ordered/round-robin candidates, bounded retry with backoff/jitter, fallback before payload |
| Shared Gateway orchestration | common authentication/routing, admission/accounting lifecycle, attempt acquisition/settlement, and terminal error mapping across Chat, streaming Chat, and Embeddings |
| Circuit breaker | per-target Closed/Open/HalfOpen state, passive failures, single/limited probes |
| Multi-attempt accounting | one Request lifecycle with independently reserved and settled Provider attempts |
| GA Provider adapters | OpenAI, Azure OpenAI, DeepSeek, and generic OpenAI-compatible chat/stream/embedding profiles with cancellation and capability enforcement |
| Capability contract | immutable Provider/Deployment declarations for chat, stream, embeddings, tools, vision, JSON, developer role, reasoning, stream usage and token limits; request-derived preflight filters incompatible fallbacks and fails before upstream I/O |
| Provider profile Phase 0A | registered versioned immutable manifests bind Provider type to Access Surface, Operation Registry and Credential Scheme; scheme-specific authorizers are constructed outside adapters; capability evidence is persisted as verified/declared/legacy/unsupported, exposed in Admin, available to evidence-aware route resolution, and migrated atomically with old records kept conservatively legacy through `LegacyAdapterBridge`; normal writes are strict and Provider/Deployment capability and evidence relations are transactionally enforced |
| Real Provider evidence | fail-closed exact-commit GA matrix runner with per-profile credential isolation, output scrubbing, chat/stream/embedding contracts, and 0600 JSON evidence; execution still requires external accounts |
| Gemini Beta adapter | native text `generateContent`, SSE, float embeddings, usage normalization, secret-safe errors, and opt-in real smoke test |
| Bedrock Beta adapter | fixed `bedrock.runtime.converse.text.v1` text-only Converse/ConverseStream profile; strict encrypted credential JSON, explicit-session SigV4, region binding, AWS EventStream CRC/order/truncation checks, conservative stop-reason mapping, Provider request ID/code/Retry-After extraction, secret-safe errors, Admin hot-load integration, and opt-in real smoke test |
| Bedrock Mantle Phase 1C | isolated `bedrock-mantle` surface with OpenAI Chat, stateless Responses and Anthropic Messages profiles; regional origin pinning, encrypted Bedrock API-key scheme, Bearer/x-api-key wire auth, `store:false`, native Thinking signature round-trip, Admin profile selection, and raw JSON/SSE fixtures |
| Bedrock Invoke Phase 2A | versioned `bedrock.runtime.invoke.titan-embed-text-v2.v1` model-family schema over `/v1/embeddings`; exact Titan V2 model pin, SigV4 Runtime isolation, single-string/float/256-512-1024 contract, normalized native requests, strict vector/usage validation, and pre-I/O rejection without arbitrary JSON or batch fan-out |
| Phase 2 media/resources | OpenAI Moderations, Images, Audio Transcription/Speech, Files and Batches; Bedrock Titan Image V2, Cohere Rerank 3.5 on isolated Agent Runtime, and Nova Reel Async; bbolt project ownership, opaque IDs, idempotency/unknown-outcome protection, owner-pinned reads, local private file objects, fixed-request pricing, redaction, TTL reaping, and fail-closed unsupported Bedrock cancellation |
| Runtime provider loading | audience-bound decrypt, HTTPS/host policy, SafeTransport, route snapshot |
| Offline bootstrap | atomic Provider/Route/Project/Key creation, secret via stdin, Gateway key shown once |
| Internal key lifecycle | offline one-time key issuance and revisioned disable with snapshot reload tests |
| Gateway authorization | Bearer key authentication plus project and allowed-route enforcement |
| Budget admission | fixed-point price estimate and concurrent daily reservation before provider calls |
| Request settlement | durable accepted/reserved/started/settled/finalized path with cleanup context |
| Unknown outcomes | conservative estimated accounting for ambiguous calls and missing usage |
| In-memory policy | atomic per-project RPM, TPM, and concurrency admission; TPM reconciles the one-time request estimate against summed Provider attempt usage, caps refunds, carries overage debt, and preserves hot-updated limits |
| Source policy | trusted-proxy chain parsing, Project CIDR authorization, keyed source-IP hashing |
| Token Guard | 10-second rolling buckets, fixed thresholds, deduplicated events, temporary block/recovery, revisioned Admin CRUD/test, reference protection and hot reload |
| EWMA detect-only | experimental RPM/TPM/average-token/cost-rate baselines with warmup, sample floor, absolute floors, bounded evaluation windows, cooldown, anomaly-window poisoning freeze, policy-revision reset, versioned bbolt checkpoint recovery, corruption fallback to fixed limits, Admin UI, and a hard invariant that EWMA cannot block |
| Alert delivery | encrypted header secret, Generic JSON, SafeTransport, bounded queue, retry/jitter, dedup, secret-safe Admin CRUD/test and atomic credential cleanup |
| Alert audit | HMAC audit records for generated/submission/delivery states using only stable IDs and enums; payload/Details/URL/Header/Secret excluded; concurrent submit/close race-safe |
| Redaction policies | built-in PII/Secret, RE2 regex and dictionary compiler; finite-width analysis; recursive JSON inbound/outbound actions; per-choice/content/tool rolling stream tails with hard limits; Admin CRUD/test; Project binding and hot reload |
| Secret canary gate | real Runtime request scans logs, encrypted metadata, WAL, Usage checkpoint, Parquet, Audit, Metrics, Admin HTML/API and heap pprof; value-blind panic recovery; production browser artifact scan rejects canaries, source maps and persistence APIs |
| Redaction quality gate | versioned positive/false-positive corpus, Luhn and China ID checksum/date validation, rolling-vs-buffered equivalence fuzzing, compiler fuzzing and allocation/throughput benchmarks |
| Usage analytics | bounded non-blocking derivative queue; overflow marks lag without blocking Ledger, checkpoint/Parquet boundaries catch up from authoritative watermark; live aggregate and bbolt checkpoint |
| Historical usage | atomic daily Parquet partitions, checksum manifest, compact/verify/prune commands, explicit Ledger reconciliation report with missing/duplicate/extra Event ID rejection |
| WAL resilience/performance | every-byte truncation recovery; 10,000 random crash cuts; 126 checkpoint boundaries before/after commit; deterministic ENOSPC/partial-write EIO/fsync EIO fail-closed tests; 100k-record replay benchmark; concurrent group commit; measured 10 GiB verify+State replay bound of 68.578 s on the reference host |
| Streaming stress | 1,000 real TCP SSE clients pause reads after the first event; measured heap/RSS/CPU/FD and baseline cleanup; ten same-process rounds/10,000 connections show exact goroutine/FD return and no monotonic heap growth; retained pprof is runtime/thread/stack initialization only |
| Soak automation | exact-commit 24h harness records secret-free JSONL RSS/goroutine/FD/WAL/analytics/request evidence and enforces explicit limits; short runs are unambiguously `smoke_only` |
| Audit integrity | HMAC hash chain, bbolt head checkpoint, lifecycle events, offline verify |
| Backup | offline epoch-consistent bbolt/fixed-WAL/Usage/Audit snapshot, chunked AES-GCM archive, atomic publication and verification; 100 concurrent append/restore exact-watermark gate |
| Prometheus/Alertmanager Core | versioned independently revocable Metrics credentials, dedicated mTLS listener, bounded scrape rendering, Ledger-replay-safe classic latency histograms, build/runtime/capacity metrics, low-cardinality contract tests, runtime smoke asset, formal external dead-man artifact, recording/alert rules, rule fixtures, admission contract and operations runbook |
| Admin authentication | offline bootstrap, Argon2id password, hash-only sessions, Secure/HttpOnly/SameSite cookie, Origin + CSRF enforcement; standard TOTP with multiple independently revocable encrypted authenticators, two-stage login challenges, replay protection, one-time recovery codes, optional/required policy, and offline MFA reset |
| Admin read API | Dashboard, Usage request detail, system status, secret-safe resources, Audit cursor pagination |
| Project and key Admin API | CSRF-protected lifecycle, `If-Match` revisions, one-time key response, immediate auth snapshot refresh |
| Provider Admin API | audience-bound credential encryption/rotation, full Provider create/edit/enable/disable/delete/test lifecycle, capability upper bounds, Deployment capability subsets and atomic runtime route replacement |
| Admin contract completion | revisioned Credential deletion with atomic Provider/Webhook reference protection; Project Token Guard unblock; Route connection test; collection and per-resource Alert test endpoints |
| Admin mutation integrity | dependency-scoped mutation ordering, dependency guards, tombstones, HMAC Audit events, `no-store` and strict CSP headers |
| Concurrent mutation and admission paths | Admin writes use dependency-scoped consistency domains and group-committed audit batches; limiter state is Project-local, Token Guard state is subject-local, and durable Budget operations use Project locks with globally ordered Ledger application |
| Embedded Admin console | React/TypeScript/Vite SPA embedded with `go:embed`; login/logout, in-session password and CSRF/session rotation, Dashboard, Projects/Keys, Credentials/Providers, Deployments/Routes, Usage, merged Redaction/Token Guard Policies, Alerts/Audit Operations and status |
| Admin localization | complete semantic-key `zh-CN`/`en-US` resources with parity test, single-language navigation, no-refresh switching, public bootstrap locale, separately saved revisioned instance default and per-admin server-side preference (no browser persistence), document language updates, localized display enums, and safe localized API errors |
| Operations CLI | byte-verified read-only `doctor` using a non-rewriting existing lock plus read-only bbolt/WAL paths, offline audited Admin password reset/session invalidation, top-level restore alias, config/usage/audit/backup/key lifecycle commands |
| First-run experience | `heimdall start` safe config generation, fail-closed idempotent system initialization, transactional first-Admin Web setup, transient public-listener setup token, and incremental `make start` build path |
| Frozen API contract | route-registration regression covers every v1 Admin endpoint plus Chat, Embeddings, health and Metrics so a documented route cannot silently disappear |
| Frontend security | in-memory CSRF, no browser persistence for secrets, one-time Key acknowledgement, destructive confirmations, no source maps/CDN/service worker |
| Frontend quality gates | typed API client, TanStack Query/Table, lazy uPlot chart, Vitest component/API tests, 500 KiB gzip initial-bundle gate |
| Contracts | ADRs, threat model, Gateway correctness, OpenAI compatibility, provider capabilities |
| Container delivery | 15.0 MB static non-root distroless image; UID/GID 65532, loopback-only built-in readiness check, CI metadata assertions, and signed/checksummed release tarball path |
| CI/release gate | test, Race, Vet, vulnerability scan, web artifact scan, SDK compatibility, SSE stress, container and release builds; signed/checksummed assets are generated before a separate `v1-release` evidence approval, and only GitHub-verified annotated tags can publish |
| Distributed Phase 0 foundations | Standalone/HA/Cluster ADR, Project ownership and state matrix, deterministic versioned mutation envelope, epoch fencing primitive, durable project-scoped idempotency lifecycle, and reproducible capacity-baseline contract; no consensus or multi-writer mode |

## Verified commands

```text
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o bin/heimdall ./cmd/heimdall
./bin/heimdall version
./bin/heimdall config check --config ./configs/config.example.yaml
cd web && npm run typecheck && npm test -- --run && npm run build
docker build -t heimdall:v1.0.0-dev .
docker run --rm --entrypoint /usr/local/bin/heimdall heimdall:v1.0.0-dev version
```

## Next critical path

1. Run and archive the 24-hour soak on the exact RC commit.
2. Create signed `v1.0.0-rc.1` and verify all GitHub release assets, checksums, SPDX SBOMs, and Sigstore bundles on supported architectures.
3. Resolve RC findings, then repeat the release gate for `v1.0.0-rc.2`.
4. Publish `v1.0.0` only after both RC gates and release notes are reviewed.
