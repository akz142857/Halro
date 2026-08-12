# v1 Security Review

Date: 2026-08-11

Review target: the v1.0.0 remediation tree. Final approval must bind this review,
the test results, soak artifact, and signed release evidence to the exact release
commit.

Scope: Gateway/Admin HTTP surfaces, local persistence, Provider/Webhook egress,
authentication, secrets, usage accounting, streaming redaction, dependency
exposure, release CI, and the incremental systems listed below. This is a
source-and-test review of the single-node build, not a third-party penetration
test or target-environment admission result.

## Outcome

No new P0 security defect was identified in the incremental review. The most
important post-review correction is the activation commit protocol: a durable
configuration change that has not reached any live authorization or routing
snapshot now makes the whole data plane fail closed, exposes per-domain status,
and is retried by a runtime-owned loop.

Dependency scanning and license inventory are current for the reviewed tree.
The earlier reachable chi Host-header issue remains remediated; the root module
now pins `github.com/go-chi/chi/v5@v5.3.1`. CI reruns Go and npm advisory scans
on every push and pull request.

## Controls verified

| Boundary | Evidence | Result |
|---|---|---|
| Admin authentication | Process-wide bounded Argon2id work, signed server-side session, fixation rotation, absolute/idle expiry tests | Pass |
| Admin TOTP MFA | Encrypted independent seeds, short-lived hash-only pre-auth challenges, atomic replay protection and recovery codes | Pass |
| Admin mutations | Same-origin + CSRF + exact administrator role, optimistic revisions, mandatory create idempotency | Pass |
| Credential-spending reads | Invocation-target refresh/describe require administrator role and same-origin validation | Pass |
| Gateway keys | One-time plaintext, hash-only persistence, fail-closed revocation snapshot activation | Pass |
| Provider/Webhook secrets | AES-GCM + HKDF audience binding; API, errors, telemetry and heap canaries exclude protected values | Pass |
| Egress/SSRF | Shared SafeTransport, redirect/DNS revalidation, metadata denial and explicit private-network opt-in | Pass; typed-unsent classification remains P3 |
| Usage abuse | Durable reservations, RPM/TPM/concurrency/budget limits, Token Guard, race and stress tests | Pass |
| PII/secrets | Bounded streaming redaction, reject semantics, persistence/API/heap/browser artifact canaries | Pass |
| Durability | Ledger/Audit chains and checkpoints, encrypted backup verification, COW Master Key and Key Slot publication | Pass |
| Browser | CSP/security headers, `no-store`, no persistent browser secrets, modal/landmark baseline | Pass |
| Dependencies | `govulncheck`, npm audits, 12-direct-Go/11-runtime-UI license review and notice drift gate | Pass after remediation |

## Incremental threat review

### KMS, Master Key and host custody

`internal/kms`, `internal/masterkey`, `internal/hostsecurity`, and the Key Slot
lifecycle were reviewed together. Key references must match an exact
purpose/region/account/ARN allowlist; KMS responses must echo that ARN and the
approved algorithm; encryption context binds the protected payload to the
Halro instance and slot purpose. Primary and Recovery are independent verified
slots, Recovery use is explicit and audited, and no automatic fallback exists.
Lifecycle operations use revisioned COW state plus durable audit intents and
recover idempotently at injected publication boundaries. Provider material is
redacted from status, logs, metrics, errors, backups, and heap canaries. File
mode remains free of cloud SDK initialization and calls.

Host hardening rejects unsafe dump/debug conditions where the platform exposes
them, while filesystem confidentiality still depends on correct permissions,
backup custody, and retaining every Master Key generation needed by an archive.
Real AWS authorization, CloudTrail correlation, recovery drills, and host policy
remain target-environment gates; repository tests cannot close them.

### AWS default credential chain / IMDS decision

The previous review said that ambient AWS credential support required a new
threat review. That review is now recorded here for
`internal/kms/awskms.New`, which calls `LoadDefaultConfig` only in Key Slot mode.

- The SDK result is wrapped before use. Only `WebIdentityCredentials`,
  `CredentialsEndpointProvider` (ECS task role / EKS Pod Identity), and
  `EC2RoleProvider` are accepted. Environment and shared-file static
  credentials fail closed instead of being used.
- Region, account, KMS key ARN, algorithm, and optional HTTPS endpoint are
  operator-configured allowlists. KMS requests cannot substitute a different
  key or omit the encryption context.
- SDK retries are fixed to one; startup/call deadlines are bounded; unknown
  outcomes are not treated as proof that retry is safe. Public errors expose a
  typed class and provider request ID, not credentials or upstream bodies.
- Approving `EC2RoleProvider` deliberately permits the AWS SDK metadata path on
  EC2. Production EC2 must enforce its approved IMDSv2 and network/hop-limit
  policy; this repository does not claim to prove host metadata configuration.
  ECS/EKS credential endpoint URIs and identity policy are likewise deployment
  evidence, not user-supplied Admin inputs.

Decision: the default chain is accepted for the three workload-identity sources
above, only in Key Slot mode and under the exact KMS allowlist. Adding static
sources, another metadata source, or allowing Admin input to control credential
endpoint selection requires another review.

### Bearer credentials and signed model catalog

Versioned Metrics and audit-anchor bearer credentials are file-backed,
hash/audit protected, rotatable with bounded overlap, and independently scoped.
The signed model catalog verifies pinned trust roots, signatures, expiry,
sequence/rollback checkpoints, decompression limits, entry limits and immutable
activation. Failure retains bundled/last-known-good evidence and never widens a
Deployment. The 1.0.0 release build intentionally has no production trust root;
`trust_root_count: 0` and an inactive dynamic catalog are documented limitations,
not a silent security claim.

### Bedrock Mantle and capability detection

Bedrock Runtime SigV4 and Bedrock Mantle API-key surfaces use distinct profile,
credential and audience axes. Mantle cannot borrow a Runtime credential or
silently switch protocol; OpenAI and Anthropic native envelopes are pinned to
reviewed profiles.

Capability detection is an explicit administrator mutation with same-origin,
CSRF, role, concurrency, rate, timeout and provider-call bounds. Provider I/O is
durably reserved before execution; cancellation cannot erase an ambiguous
charge; results bind provider/credential/binding revisions and expire. Only
verified supported results can establish capability evidence. Detection calls
are control-plane spend and intentionally do not enter project Ledger/budget or
Usage totals; the Console now says so.

### Topology commit and activation

The store transaction is the single Admin commit point and contains the durable
audit intent. Audit delivery and live activation use runtime-owned bounded
contexts, so a client disconnect cannot cancel post-commit security work.
Topology, authentication, redaction and Token Guard staleness are tracked per
domain; one successful activation cannot clear another domain. Any stale domain
fails readiness and refuses all data-plane traffic with protocol-correct 503
responses until a loop replays all four domains. Audit delivery has its own
backlog retry even when activation is current.

## Evidence and verification status

The 2026-07-31 review ran the complete Go test/race/vet, vulnerability, npm,
redaction fuzz and official SDK suites for its then-current tree. The 2026-08-11
incremental review inspected the new boundaries above and ran their directly
affected tests, including race tests for post-commit audit recovery. The final
remediation handoff must run the repository's complete current Go, frontend,
observability, release-workflow and embedded-bundle gates once; only that final
result can be attached to the release commit.

## Residual risks and release requirements

- Single-node operation has no failover; crash recovery and a 24-hour exact-
  commit soak artifact remain required.
- Real Provider/KMS calls can consume quota or money. No billable release matrix
  is inferred from fakes or protocol stubs.
- Filesystem confidentiality depends on host permissions and Master Key custody.
  Old backups remain bound to their recorded key fingerprint.
- The inactive 1.0.0 signed-catalog trust root limits delivery scope but fails
  closed; enabling it requires signed publication and rollback evidence.
- The narrow SafeTransport typed-unsent/error-class distinction remains a P3
  correctness hardening item; ambiguous upstream outcomes remain fail closed.
- Repository validation does not prove protected GitHub release approval,
  retained artifacts, production registry availability, 24-hour soak, real AWS
  recovery, or independent dead-man delivery.

## Release decision

Core controls are suitable for an exact-commit release-candidate gate. Final
v1.0.0 release remains blocked until all of the following are archived for that
same commit:

1. the current crash/recovery matrix and 24-hour soak artifact;
2. packaging/install evidence for every advertised artifact surface;
3. a protected `v1-release` GitHub Environment with required reviewers and its
   environment-scoped signing secret;
4. a complete signed/checksummed release whose public verification procedure
   succeeds and whose workflow/artifact identifiers are retained.

These replace the obsolete, undefined name “M10 recovery”; none is considered
closed by editing this document.
