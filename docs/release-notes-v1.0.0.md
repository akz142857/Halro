# Heimdall v1.0.0 release notes

Status: release-ready document; publish with the final `v1.0.0` tag only after
the RC, real-Provider, and 24-hour soak gates pass.

Heimdall v1.0.0 is the first release of a single-binary, self-hosted LLM
Gateway focused on unified OpenAI-compatible access, secure Provider credential
custody, internal Gateway Key distribution, usage controls, anomaly protection,
and sensitive-data redaction. Runtime deployment requires one binary, one YAML
configuration, one explicitly selected Master Key custody mode, and one data
directory. File mode uses a private 32-byte key file; Key Slot mode uses AWS
KMS-wrapped Primary/Recovery descriptors and Workload Identity without a
plaintext Master Key file. Redis, PostgreSQL,
and other external services are not required.

## Highlights

- OpenAI-compatible `POST /v1/chat/completions` and `POST /v1/embeddings`,
  including bounded SSE streaming and official Python, Node, and Go SDK
  compatibility contracts.
- Public model aliases route to ordered or round-robin Deployments, with
  bounded retry, pre-payload fallback, and per-target circuit breakers.
- Encrypted Provider and webhook Credentials use AES-256-GCM, HKDF-separated
  keys, random nonces, and audience-bound AAD. Provider keys are never returned
  by Admin APIs.
- Internal `gw_...` keys are shown once and persisted only as SHA-256 hashes.
  Projects support allowed routes, RPM, TPM, concurrency, CIDR, daily fixed-point
  budgets, and Token Guard policies.
- Token Guard optionally maintains experimental EWMA baselines for RPM, TPM,
  average tokens/request, and cost rate. Relative anomalies are detect-only,
  freeze tainted windows, and can never trigger automatic blocking; fixed
  thresholds remain the enforcement path.
- Redaction supports built-in validated PII/secret detectors, RE2 rules,
  dictionaries, recursive JSON actions, and bounded cross-SSE-chunk tails.
- The append-only Ledger is the accounting authority. Usage checkpoint,
  Parquet export, reconciliation, budget reservation, retry/fallback attempts,
  and conservative unknown-outcome settlement are crash tested.
- The embedded React Admin console covers Dashboard, Projects/Keys,
  Credentials, Providers, Deployments, Routes, Usage, Policies, Alerts, Audit,
  and Settings without a Node.js runtime dependency.
- Prometheus metrics, generic webhook alerts, encrypted backup/restore, HMAC
  Audit chain verification, dependency/SBOM/signature release gates, and a
  complete operator guide are included.

## Provider support

| Provider profile | Status | v1 capability |
|---|---|---|
| OpenAI | GA | text chat, SSE stream, embeddings |
| Azure OpenAI | GA | deployment-scoped text chat, stream, embeddings; explicit API version |
| DeepSeek | GA | OpenAI-compatible text chat and stream; embeddings disabled by default |
| Reviewed OpenAI-compatible endpoint | GA profile | conservative chat/stream/embedding capability declaration |
| Gemini | Beta | text `generateContent`, SSE, float embeddings |
| AWS Bedrock | Beta | text Converse/ConverseStream, SigV4, static encrypted credential JSON |

Gemini and Bedrock reject undeclared tools, vision, JSON mode, or embedding
capabilities rather than silently degrading. Bedrock does not use ambient AWS
credentials, IMDS, or the default credential chain.

## Security and recovery

- Default listeners are loopback-only; public plaintext Gateway binding needs
  an explicit override, while Admin and Metrics cannot use it.
- SafeTransport rejects environment proxies, redirects, metadata/private IPs
  unless explicitly allowed, mixed DNS answers, and DNS-rebinding targets.
- Admin sessions use Argon2id password verification, hash-only server-side
  tokens, Secure/HttpOnly/SameSite cookies, origin checks, CSRF, and bounded
  login attempts.
- The administrator password minimum deliberately changes from 12 UTF-8 bytes
  to 8 Unicode code points. This lowers the minimum for ASCII-only passwords
  from 12 to 8 characters, raises it for short multibyte passwords such as
  CJK, and is a product-policy change rather than a behavior-preserving refactor.
  A substantially longer passphrase remains recommended.
- Admin accounts can bind multiple independent standard TOTP authenticators.
  Two-stage login, encrypted seeds, replay protection, one-time recovery codes,
  optional/required policy, and audited offline MFA reset are included.
- `heimdall key rotate --new-key-file ...` performs offline per-Credential COW
  re-encryption with a persistent versioned keyring, atomic Master Key
  publication, Admin-session invalidation, stable protected Audit HMAC key, and
  a compacted crash bridge. Rerun the same command and replacement key after an
  interruption.
- Encrypted backups authenticate every chunk and final record, pin an exact
  committed Ledger prefix, validate checksums/schema/Master-Key fingerprint,
  and restore through a same-filesystem atomic directory switch while retaining
  the previous directory.
- AWS KMS Key Slot mode includes explicit Primary/Recovery paths, Encryption
  Context, Vault Key Check, KMS-aware doctor/backup/restore, rewrap versus DEK
  rotate separation, crash-safe generations, low-cardinality Metrics and
  correlated Audit. It remains release-blocked until the M11 real-account
  matrix, independent recovery drill, and four-party sign-off are complete.

Deterministic recovery evidence includes every-byte WAL truncation, 10,000
random crash cuts, ENOSPC/partial-write/fsync EIO, 126 checkpoint boundaries,
39 bbolt migration boundaries, nine Master Key publication boundaries, and
100 concurrent Ledger writes overlapping a backup snapshot.

## Measured limits

Reference host: Apple M4 Pro, darwin/arm64, Go 1.24.

- 1,000 held SSE connections: about 36.1 KiB Heap and 77.0 KiB max-RSS delta
  per in-process client/server connection; FD and goroutine counts return to
  baseline.
- Ten same-process rounds (10,000 connections total) return from 5,003 to 3
  goroutines and 2,007 to 7 FDs every round, with no monotonic Heap growth.
- A 10,737,948,420-byte WAL with 10,245 near-1-MiB frames takes 29.638 seconds
  to open/verify and 38.939 seconds to replay into accounting State: the
  published reference bound is **68.578 seconds**, slightly above the original
  60-second target.

These values are regression evidence, not cross-host guarantees. See
`docs/performance-baseline.md` for commands, workload shapes, and caveats.

## Installation and upgrade

1. Download the binary for Linux or macOS on amd64/arm64.
2. Verify `checksums.txt`, the SPDX SBOM, and Sigstore bundles.
3. Copy and validate `configs/config.example.yaml`.
4. Run `heimdall init`, bootstrap the local Admin and first Provider/Project,
   then start `heimdall serve`.

An optional non-root distroless container is attached as
`heimdall-container.tar.gz`; verify it like every other release blob, then load
it with `gzip -dc heimdall-container.tar.gz | docker load`.

For upgrades, stop Heimdall, create and verify an encrypted backup, preserve the
current binary/config/Master Key, run `config check`, and start the new binary.
Do not downgrade a migrated data directory in place. Follow
`docs/operator-guide.md`, `docs/backup-restore.md`, and `docs/releasing.md`.

## Intentional v1 limits

v1 is single-node and does not include Redis/PostgreSQL, multi-node clustering,
SSO/OAuth, organization-level multitenancy, Kubernetes Operator, prompt/RAG
management, Agent tracing/evaluation, workflow orchestration, plugin systems,
MCP Server, or multi-region synchronization. Production pprof/core-dump/crash
upload endpoints are not exposed. Windows is not a release target because the
data-directory lock uses Unix `flock` semantics.

The final release remains contingent on the exact-tag 24-hour soak, complete
real-account matrix for every GA Provider profile, and two successful signed RC
cycles. Published RC or final assets must never be overwritten.
