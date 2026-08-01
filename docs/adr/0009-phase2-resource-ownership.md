# ADR 0009: Phase 2 resource ownership

Status: Accepted (2026-08-01)

## Decision

Files, batches, and asynchronous invocations are Gateway resources. Every record
is durably bound to its project, provider, deployment, immutable profile,
region, upstream resource identifier, and expiry. External identifiers are
opaque Heimdall identifiers; callers never supply an upstream identifier.

Creation is pinned after the first accepted provider operation and cannot fall
back. Repeated creates require a project-scoped idempotency key. Reads, cancel,
and delete resolve the recorded owner and fail closed if that owner is absent or
changed. Terminal records are retained until their TTL and then reaped. File
content is stored only in the configured private object directory; metadata is
stored in bbolt. Bedrock asynchronous input/output uses explicit `s3://` URIs;
Heimdall does not copy media through logs or metadata storage.

The `bedrock-agent-runtime` access surface is admitted only for the versioned
Cohere Rerank 3.5 primitive. It does not admit Agents, Knowledge Bases, Flows,
or arbitrary native payloads.
