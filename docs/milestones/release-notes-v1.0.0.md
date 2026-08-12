# Halro v1.0.0 release notes

Status: release-ready document; publish with the final `v1.0.0` tag only after
the RC, real-Provider, and 24-hour soak gates pass.

Halro v1.0.0 is the first release of a single-binary, self-hosted LLM
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
- Deployment prices are immutable, effective-dated versions. Every Provider
  attempt durably pins its own price snapshot before upstream I/O, so later
  price changes cannot rewrite historical spend. Unknown cost is never
  displayed as zero.
- Pricing automation and LLM extraction create expiring Proposals only.
  Provenance, model/region match, warnings and a digest are reviewed in Admin;
  recent re-authentication is required before adoption creates a new audited
  Price Version. Proposals never participate in Gateway price selection.
- The embedded React Admin console covers Dashboard, Projects/Keys,
  Credentials, Providers, Deployments, Routes, Usage, Policies, Alerts, Audit,
  and Settings without a Node.js runtime dependency.
- The Admin console uses a governed semantic-token design system and provides
  server-persisted per-admin Light and Dark appearances. Dark remains the
  unauthenticated default; switching is immediate, revision-safe, and does not
  use browser persistence.
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
- `halro key rotate --new-key-file ...` performs offline per-Credential COW
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
`docs/verification/performance-baseline.md` for commands, workload shapes, and caveats.

## Installation and upgrade

1. Download the release assets. `checksums.txt` covers every published
   artifact, so download all of them, or keep `--ignore-missing` below when
   verifying only your platform:

   ```bash
   gh release download v1.0.0 --repo akz142857/Halro
   ```
2. Verify the signed `checksums.txt` first, then use it to verify every release
   blob, and finally verify each blob's Sigstore bundle. Requires cosign v2.2 or
   newer; `gh attestation verify` requires an authenticated `gh`:

   ```bash
   COSIGN_IDENTITY='https://github.com/akz142857/Halro/.github/workflows/release.yml@refs/tags/v1.0.0'
   COSIGN_ISSUER='https://token.actions.githubusercontent.com'
   cosign verify-blob \
     --certificate-identity "$COSIGN_IDENTITY" \
     --certificate-oidc-issuer "$COSIGN_ISSUER" \
     --bundle checksums.txt.sigstore.json checksums.txt
   sha256sum --check --ignore-missing checksums.txt  # macOS: shasum -a 256 --check --ignore-missing checksums.txt
   for artifact in halro-*.tar.gz halro*.spdx.json; do
     gh attestation verify "$artifact" --repo akz142857/Halro
   done
   for artifact in halro-* halro.spdx.json; do
     case "$artifact" in *.sigstore.json) continue ;; esac
     [ -f "$artifact.sigstore.json" ] || continue
     cosign verify-blob \
       --certificate-identity "$COSIGN_IDENTITY" \
       --certificate-oidc-issuer "$COSIGN_ISSUER" \
       --bundle "$artifact.sigstore.json" "$artifact"
   done
   ```
3. Copy and validate `configs/config.example.yaml`.
4. Run `halro init`, bootstrap the local Admin and first Provider/Project,
   then start `halro serve`.


An optional non-root distroless container is attached as
`halro-container.tar.gz`; verify it like every other release blob, then load
it with `gzip -dc halro-container.tar.gz | docker load`.

For upgrades, stop Halro, create and verify an encrypted backup, preserve the
current binary/config/Master Key, run `config check`, and start the new binary.
Do not downgrade a migrated data directory in place. Follow
`docs/guides/operator-guide.md`, `docs/guides/backup-restore.md`, and `docs/guides/releasing.md`.

### Upgrading from v1.0.0-rc.1

`v1.0.0-rc.1` was tagged in the public source repository but was never
published as a GitHub Release. It is a hard compatibility boundary, not an
upgrade source: the Heimdall-to-Halro rename changed the backup encryption
domain and Vault HKDF domain. An rc.1 data directory, backup archive, and
configuration must not be reused with 1.0.0; create a fresh 1.0.0 instance and
re-enter reviewed configuration and credentials.

The resulting errors are misleading: a correct old backup key may report
`backup authentication failed`, and a correct old Master Key may report
`master key does not authenticate the metadata store`. At this boundary those
messages mean version incompatibility, not necessarily a mistyped key.

Do not probe an rc.1 data directory with the 1.0.0 binary. Startup migrates
bbolt from schema 19 to schema 27 and commits that migration before the Vault
key check fails; after one attempted 1.0.0 start, the rc.1 binary cannot reopen
the directory either. Preserve an offline copy before any investigation and
rebuild rather than attempting an in-place upgrade or downgrade.

## Intentional v1 limits

v1 is single-node and does not include Redis/PostgreSQL, multi-node clustering,
SSO/OAuth, organization-level multitenancy, Kubernetes Operator, prompt/RAG
management, Agent tracing/evaluation, workflow orchestration, plugin systems,
MCP Server, or multi-region synchronization. Production pprof/core-dump/crash
upload endpoints are not exposed. Windows is not a release target because the
data-directory lock uses Unix `flock` semantics.

## Known limitations

1. **Single writer and data directory.** Exactly one Halro process may own a
   data directory. Docker and Kubernetes use one replica and `Recreate`; shared
   storage and multiple writers are unsupported.
2. **No high availability.** An instance failure is an outage. Cluster/HA work
   is proposed for 1.1.0 (`docs/todo/halro-ha-architecture.zh-CN.md`) and is not
   a hidden mode of this binary. Recovery from an instance failure is entirely
   an operator responsibility.
3. **Writable parent layout.** Halro creates publication state beside
   `storage.data_dir`. Mount a persistent writable parent and use its child as
   `data_dir`; never use the volume mount point itself.
4. **rc.1 is not upgrade-compatible.** Use the hard-cutover procedure above.
   Attempting one 1.0.0 start can make the directory unreadable by rc.1 before
   the later key check fails.
5. **Admin create idempotency is mandatory.** Provider, Deployment, Route,
   Project, and Gateway Key creates require `Idempotency-Key` (≤256 bytes; the
   data-plane contract's range is 1–128, so the two limits differ). A missing
   header returns 400 `idempotency_key_required`; a replay returns 409
   `<resource>_idempotency_replay` naming the existing ID, and does not replay
   the old response.
6. **Migrations 25/26 reset detection caches.** This only affects development
   instances that ran unpublished schema-24/25 builds; run detection again.
7. **Gemini and AWS Bedrock are Beta.** The Admin API may store declarations
   beyond a Beta profile's defaults, but compile-time profile/adapter gates
   prevent those declarations from becoming data-plane capabilities: an
   out-of-ceiling request is refused with 400 `unsupported_feature` before any
   Provider I/O, with no budget reserved and no upstream connection opened.
   Where the Console shows more than a profile's defaults, data-plane behaviour
   is authoritative.
8. **Dynamic signed catalog is inactive in the current release build.** No
   production trust roots are compiled, so verification fails closed to the
   bundled catalog; `trust_root_count: 0` is expected and updates default off.
9. **Unknown request fields are rejected.** Chat and Embeddings return 400 for
   any field absent from their manifests, including the official OpenAI
   parameters `frequency_penalty`, `presence_penalty`, `logit_bias`,
   `logprobs`, `store`, `metadata`, and `service_tier`. Check the field set in
   `docs/compatibility/endpoint-manifests.json` before migrating from direct
   OpenAI access.
10. **Control-plane Provider calls are outside project accounting.** Capability
    detection, Provider tests, and health probes can incur upstream charges but
    do not enter the Ledger, project budget, or Usage totals. The spend is
    bounded (at most 12 calls per detection, each ≤2048 bytes in and 16 output
    tokens) and every call is recorded durably and counted in Prometheus, but
    the operator's real upstream invoice will be slightly higher than Halro's
    ledger total.
11. **Ambiguous upstream outcomes settle conservatively.** If Halro cannot prove
    that no request bytes reached the Provider, it may settle the reserved
    maximum rather than blind-retry or refund. Failures that provably never
    reached the Provider — refused connection, DNS failure, upstream 5xx — are
    settled at zero and release the full reservation; only the ambiguous middle
    is charged.
12. **Shutdown is bounded and operator-configurable.** The default
    `server.shutdown_timeout` is two minutes and cannot be shorter than
    `gateway.route_total_timeout`. Service-manager termination grace must be
    longer than Halro's budget. Attempts still active when the budget expires
    are forcibly closed, conservatively settled when ambiguous, and counted by
    durable `halro_shutdown_truncated_attempts_total`.
13. **Stale activation refuses the whole data plane.** A durable Admin change
    that has not reached topology, authentication, redaction, or Token Guard
    snapshots returns 503 for every Project. Halro retries all four domains
    every five seconds; the Console, system status, readiness, the unlabelled
    `halro_activation_stale`/`_seconds` gauges, critical alert, and
    `docs/runbooks/configuration-stale.md` expose the condition. The blast
    radius remains instance-wide by design to avoid known-stale authorization.
    OpenAI routes return code `configuration_stale`; Anthropic Messages returns
    `overloaded_error`; both use HTTP 503 with `Retry-After: 5`.
14. **Capability monitoring requires the bundled Prometheus rules.** The
    release ships alerts for signed-catalog degradation, capability drift, and
    high detection failure rate, but deployments that scrape `/metrics`
    without loading those rules will not receive these signals.
15. **The dead-man monitor is a separate deployment.** The default Core compose
    file contains only Prometheus and Alertmanager; without the external probe
    there is no independent witness. It does not prove notification delivery.
16. **Old backups can restore revoked Gateway Keys.** After restoring an
    archive from before an incident, repeat the reviewed revocation list before
    returning the instance to service.
17. **Pre-rotation backups require their historical Master Key generation.** A
    backup created before Master Key rotation cannot be restored with only the
    new key; retain a mapping from archive to retired key generation.
18. **KMS Key Slot mode is release-blocked.** Do not use the three M11 runbooks
    as production acceptance until the real AWS matrix, independent recovery
    drill, and four-party sign-off are archived for this exact commit. File mode
    is the verified default.
19. **Capacity figures are reference results, not guarantees.** All figures were
    measured on macOS/APFS with `F_FULLFSYNC` and are floors, not ceilings —
    Linux/NVMe is materially higher. Accounting sustained 1,223 lifecycles/s at
    64 concurrency and scales with concurrency rather than project count; the
    Admin write path sustained about 31 mutations/s, of which the topology
    commit protocol costs 25.3%; a 2,588-mutation run grew RSS by 1.4 MiB with
    no goroutine leak. Argon2id uses about 64 MiB per operation and is bounded
    to two concurrent operations process-wide, so 64 concurrent logins cost
    about 256 MiB of peak heap instead of roughly 4 GiB; extra login/step-up
    work queues, and deployments still need memory headroom for the runtime.
    Details in `docs/verification/performance-baseline.md`. The exact-tag
    24-hour soak artifact is not yet archived.
20. **There is no official container registry.** Releases attach
    `halro-container.tar.gz`; operators load it, push it to their own registry,
    and replace the explicit placeholder digest in Kubernetes manifests.

The final release remains contingent on the exact-tag 24-hour soak, complete
real-account matrix for every GA Provider profile, and two successful signed RC
cycles. Published RC or final assets must never be overwritten.
