# ADR 0009: Phase 2 resource ownership

Status: Accepted (2026-08-01)

> Naming note (2026-08-07): "Phase 2" was a delivery phase, not a description of
> anything, and the Go identifiers that carried it are now named after what they
> serve — `InferenceResources`, matching the northbound profile
> `halro.inference-resources.v1`, with the OpenAI provider profile named for
> its own value `openai.media-resources.v1`. This file keeps its name so
> existing links resolve. The one thing that must never be renamed is the bbolt
> migration `phase2_capability_evidence` and its two step names: those are
> recorded history in every instance that has run them.

## Decision

Files, batches, and asynchronous invocations are Gateway resources. Every record
is durably bound to its project, provider, deployment, immutable profile,
region, upstream resource identifier, and expiry. External identifiers are
opaque Halro identifiers; callers never supply an upstream identifier.

Creation is pinned after the first accepted provider operation and cannot fall
back. Repeated creates require a project-scoped idempotency key. Reads, cancel,
and delete resolve the recorded owner and fail closed if that owner is absent or
changed. Terminal records are retained until their TTL and then reaped. File
content is stored only in the configured private object directory; metadata is
stored in bbolt. Bedrock asynchronous input/output uses explicit `s3://` URIs;
Halro does not copy media through logs or metadata storage.

The `bedrock-agent-runtime` access surface is admitted only for the versioned
Cohere Rerank 3.5 primitive. It does not admit Agents, Knowledge Bases, Flows,
or arbitrary native payloads.
