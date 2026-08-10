# ADR 0020: Dynamic signed model catalog

- Status: **Implemented 2026-08-10**
- Date: 2026-08-10
- Tracking: `docs/prd/provider-model-selection-and-capability-resolution.zh-CN.md`
- Related: ADR 0019, `docs/contracts/provider-capabilities.md`

## Context

Exact model identities and reviewed capability facts change more frequently
than the Halro binary. Fetching live metadata in the Gateway request path would
make availability and routing depend on an external control plane, while
trusting an unsigned directory would allow that control plane to widen what a
Deployment may do.

## Decision

Halro carries an offline bundled snapshot and may, only when explicitly
enabled, refresh one complete signed snapshot in the background. Bundled and
remote entries use schema version 1 and capability dictionary version 1. A
remote envelope contains a canonical payload and one or more Ed25519
signatures. Release artifacts contain public trust roots only; Halro has no
signing command or private-key input.

The endpoint and hostname allowlist are compile-time constants. Downloads use
SafeTransport with HTTPS, no environment proxy, no redirects, DNS/IP
validation, and pinned dialing. Compressed and decoded bytes, expansion ratio,
and entries all have hard limits. The reader strictly rejects unknown JSON
fields, unreadable schemas, dictionary mismatch, future generation time,
expiry, revision mismatch, untrusted signatures, a fixed-revision mismatch,
sequence rollback, and sequence reuse with different content.

After all checks pass, Halro writes a mode-0600 last-known-good file with
fsync-and-rename semantics and swaps the in-memory catalog atomically. No
Gateway handler performs catalog I/O. A failed refresh keeps the prior verified
snapshot; if none exists, the bundled snapshot remains active. An expired but
otherwise verified last-known-good is retained only for visible provenance and
rollback protection; new resolution and save decisions use the bundled
catalog. Catalog absence is not treated as capability drift and does not stop
traffic based on an existing immutable Deployment snapshot.

Signed entries overlay or revoke exact
`(provider type, profile, target kind, model, region)` identities. They may use only known
capability IDs and cannot alter Provider protocols, authentication, allowed
hosts, credentials, routes, deployments, or Provider availability. Missing
facts remain unknown, contradictory binding-scoped claims fail closed, and a
catalog update never widens a saved Deployment.

## Publishing and trust

Canonicalization tools prepare bytes for an external KMS/HSM or offline
Ed25519 signer and assemble the returned signature; they never accept a private
key. The publication workflow accepts an immutable candidate commit, verifies
it with production public roots behind the `model-catalog-production`
protected environment, enforces a strictly increasing sequence, and opens a
CODEOWNERS pull request. A PR-head verification check and repository branch
protection supply the second independent approval. The operational
procedure and key-overlap retirement rule are defined in
`docs/runbooks/model-catalog-publishing.md`.

## Consequences

New exact model facts can ship without a Halro binary release, but only within
the protocols and capability dictionary already understood by that binary.
Offline and failed-update behavior stays deterministic. Operators gain explicit
current/degraded/unavailable state, bounded metrics, audits, manual refresh,
and optional revision pinning. Enabling production updates still requires
externally configured public roots, protected-environment reviewers, and
branch protection; source code cannot attest that those controls exist.
