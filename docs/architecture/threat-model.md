# Halro v1 threat model

## Assets

- master key;
- provider credentials;
- Gateway keys;
- Admin password, encrypted TOTP seeds, recovery-code hashes, login challenges, and sessions;
- TLS private key;
- webhook/backup secrets;
- usage, cost, source-IP derivatives, and audit data;
- Run/Work Unit identifiers and lifecycle, Outcome definitions, authenticated
  Outcome declarations, reporter identity, and evidence digests/references;
- prompts and responses in transient memory.

## Trust boundaries

```text
Internal client
  → Gateway listener
  → policy/redaction/accounting
  → SafeTransport
  → external provider

Admin browser
  → TLS/Admin listener
  → Admin session + CSRF
  → metadata/vault

Process
  → bbolt + Accounting Ledger + Governance Journal + derived exports

HA member
  → mTLS + per-member SPKI pin + Master Key challenge-response
  → peer ordering journal and replicated authoritative stores
```

The host root account is trusted for v1. In HA this expands to every member's
root account, every PVC/storage administrator, and every RBAC or secret-manager
principal that can read a member certificate or Master Key material. Audit
chaining detects offline record mutation but is not non-repudiation against a
root attacker.

Ledger frame integrity (ADR 0016) draws the same boundary. The MAC and hash chain
stop someone who can write to `ledger.wal` but does not hold the Master Key:
offline edits, a truncated suffix, a file swapped in from elsewhere. They do not
stop the holder of `master.key` in `mode: file`, who can derive the chain key and
recompute anything they rewrite — the audited party and the auditor are the same
principal there. `mode: key_slots` narrows this, since the key is not on disk and
KMS use leaves its own record. The external anchor (ADR 0015) is what makes a
rewrite detectable by someone other than the rewriter.

## Primary threats and controls

| Threat | Primary controls |
|---|---|
| Provider credential theft | AEAD + audience binding, no reveal, safe logs |
| Gateway key theft | one-time display, SHA-256 storage, TLS, no browser persistence |
| SSRF/credential exfiltration | SafeTransport resolves and validates every target address, then sends a literal-IP CONNECT authority or performs a pinned direct dial; no environment proxy; host allowlist |
| Outbound proxy observes or redirects Provider traffic | step-up protected Admin-managed proxy registry with Vault-encrypted authentication and atomic hot activation; independently validated proxy addresses; Provider TLS still verifies the original hostname; no same-Provider direct fallback; mixed direct/proxy Routes are surfaced as a trust-boundary crossing |
| Public plaintext Admin | loopback default; never allowed by insecure Gateway override |
| Budget overspend | per-attempt durable reservation and atomic settlement |
| Crash undercount | one Ledger WAL and conservative orphan reconciliation |
| Historical price drift or forged zero cost | immutable per-Attempt PriceSnapshot, fixed-point settlement, explicit free/unknown states |
| LLM or automation silently changes production price | isolated expiring Proposal records, source digest and match evidence, recent re-authentication and audited human adoption |
| Old backup reactivates a scheduled price | restore-time pricing quarantine and explicit audited confirmation before traffic |
| Sensitive prompt/output leakage | inbound, outbound, and mandatory telemetry redaction modes |
| Streaming boundary bypass | strict or compiler-bounded enforcement; detect-only is not a prevention claim |
| Disk failure | accounting state machine, readiness false, stop new provider calls |
| Backup disclosure | authenticated encrypted backup by default |
| Browser secret recovery | no-store, strict CSP, no service worker, no local storage |
| Admin password disclosure | optional or required TOTP; no full Admin Session is issued until the second factor succeeds |
| TOTP/recovery replay | per-authenticator atomic time-step watermark; recovery codes are hash-only and atomically consumed |
| Run ID enumeration or cross-project attachment | server-generated bounded IDs; Project equality; `run:attach` scope; existence-hiding errors |
| Concurrent Project/Run over-admission | one Project critical section checks both committed + reserved + pending totals; checked integer arithmetic; reservation durable before Provider I/O |
| Creating Runs to bypass a Run cap or exhaust storage | Project budget remains authoritative; per-Key/Project write limits; 1,000 active Run and 1,000 open Work Unit hard limits |
| Run close/expiry races with inference | close/create/admission serialize on the Project lock; an already admitted Attempt still settles conservatively |
| Forged or misleading business success | Outcome is labelled as an authenticated external declaration; immutable Definition versions; reporter key and revision history retained |
| Outcome revision race or idempotency index loss | current-head compare under a short writer critical section; append-only revision; index rebuilt from the authenticated journal |
| Evidence reference carries secrets, payloads, or SSRF input | 128-character non-URL reference; reject controls/newlines/secret-like values; optional SHA-256 only; never fetch the reference |
| Governance corruption stops model traffic | independent Journal, writer, apply state, checkpoint, and readiness; ordinary inference never reads Governance state |
| Cross-log inconsistency hidden as a complete report | explicit partial/unknown states and separate accounting/governance watermarks; no invented global order |
| High-cardinality metrics or unbounded Admin query | identifiers forbidden as Prometheus labels; low-cardinality cohort rollups; cursor pagination and 200-item page limit |
| Duplicate cost in business exports | Attempt cost exists only in Usage export; normalized governance datasets reference IDs and manifest reconciliation checks counts/ranges |
| Split-brain confirmation | One local writer per directory plus durable term/promise state; a Primary confirms authority-sensitive writes only after a Replica fsync; startup adjudication and promotion fail closed on conflicting identity, incarnation, term, or prefix |
| Compromised HA member | Treat as whole-cluster compromise: the member holds replicated ciphertext plus access to the Master Key and can leave as a decrypting Standalone; externally fence it, revoke its SPKI or rotate the cluster CA, rotate Provider credentials, and retire its PVC |
| Replication credential theft | mTLS, node allowlist, per-peer SPKI pins, Master Key challenge-response, bounded/deadlined streams, and no plaintext fallback; use the cluster CA rotation runbook for revocation |
| Replication as an exfiltration path | Frames carry existing encrypted or authenticated bytes only; low-cardinality metrics and logs exclude payload, credential, object, full key fingerprint, and Project identifiers; seed is separately authorized by a target-bound, cluster-key-MACed manifest |
| Seed bypasses Backup Key | Seed requires an authenticated cluster member and optional operator approval, uses private staging, verifies every digest, and emits requested/served Audit events; possession of a cluster identity plus Master Key is therefore a full-data export authority |
| Retained or stolen PVC rejoins | Cluster incarnation and authenticated state reject stale members; scale-down PVCs must be destroyed or isolated; whole-cluster restore creates a new incarnation |

## Default assumptions

- One process owns each data directory. In HA, durable term and promises bound
  which member may confirm authoritative writes; they do not turn shared
  storage into a supported topology.
- Public access uses TLS.
- Admin and Metrics are loopback or protected behind a precisely trusted TLS proxy.
- Prompt/response bodies are not persisted by default.
- Unknown provider price is denied for budget-protected projects.
- Outcome reporters and evaluators are external principals; Halro authenticates
  who declared a value but does not attest that the business judgment is true.
- The Accounting Ledger is the only budget authority. Governance state cannot
  release budget, alter Attempt history, or trigger a Provider call.
- An approved CONNECT proxy is trusted to connect the literal IP and port Halro
  requests. It can observe that address, port, and TLS SNI. Halro does not add a
  proxy CA or disable Provider certificate validation, but this is not certificate
  pinning: a CA already trusted by the host may still issue for the Provider name.
- Provider-level fail-closed does not make a Route proxy-only. Another candidate
  under the same public model may use direct egress and receive retryable fallback.

## Required security tests

- default listener and insecure-override matrix;
- DNS rebinding, mixed A/AAAA, redirect, literal-IP CONNECT, proxy DNS rebinding,
  mapped-IP, header-limit, buffered-tunnel and metadata tests;
- credential audience tampering;
- stream redaction byte-boundary and Unicode fuzzing;
- secret canary scanning across logs, errors, heap diagnostics, WAL, bbolt, Parquet, and browser artifacts;
- WAL corruption, disk-full, backup tampering, and restore path tests.
- price-boundary, WAL v1/v2 reader-gate, proposal non-activation, and restored scheduled-price quarantine tests.
- Project/Run admission races at 1/8/64 workers, integer overflow, close/expiry
  barriers, append/apply kill points, and zero Provider calls on uncertain state.
- Governance wrong-key, truncation, tampering, revision-gap, idempotency rebuild,
  checkpoint-ahead, incremental/full-replay equality, and failure-isolation tests.
- Outcome field boundary/fuzz tests and secret canary scans across Journal,
  bbolt, logs, exports, backup, errors, and browser artifacts.
- Cross-project ID/scope matrix, control-plane rate/body/cardinality limits, and
  bounded cohort-query scan tests.
- HA term/promise and startup-adjudication matrix; stale-incarnation rejection;
  frame/MAC/SPKI/challenge tampering; split-brain partition and promotion;
  seed interruption and staging permissions; replicated-byte secret canaries;
  CA dual-trust rotation plus retired-certificate rejection.
