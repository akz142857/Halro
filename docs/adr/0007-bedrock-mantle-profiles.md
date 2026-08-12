# ADR 0007: AWS Bedrock Mantle profile isolation

- Status: Accepted for Phase 1C
- Date: 2026-08-01
- Issues: #37, #38, #39

## Context

Amazon Bedrock exposes `bedrock-runtime` and `bedrock-mantle` as different
inference surfaces. Mantle serves OpenAI-compatible Chat Completions and
Responses APIs plus the Anthropic Messages API. It has a different hostname,
authentication audience, project/state model, and quota pool from Runtime.
Treating Mantle as another URL for the existing Converse adapter would merge
security and capacity claims that AWS keeps separate.

## Decision

Phase 1C registers one Mantle access surface and three immutable profiles:

- `bedrock.mantle.openai.chat.v1` uses `/v1/chat/completions`;
- `bedrock.mantle.openai.responses.v1` uses `/v1/responses`;
- `bedrock.mantle.anthropic.messages.v1` uses
  `/anthropic/v1/messages`.

All three profiles use a Mantle-specific Bedrock API-key credential scheme.
The OpenAI-compatible profiles render the key as an Authorization Bearer
credential. The Anthropic profile renders the same credential kind as
`x-api-key`. Credentials are encrypted against the exact Mantle endpoint
audience and cannot be attached to a Runtime provider.

Only region-bound HTTPS hosts matching
`bedrock-mantle.<region>.api.aws` are accepted. Phase 1C does not accept custom
hosts, redirects, ambient AWS credentials, or Runtime endpoints. SigV4 support
requires a later profile revision with the Mantle signing name and authority
rules independently reviewed.

The Responses profile participates in Halro's existing stateless Responses
tier. Halro always sends `store:false` and does not expose Mantle response
resource IDs, retrieval, deletion, background execution, Projects, or
`previous_response_id`. This avoids accidentally importing AWS's default
30-day stored-response ownership into the current gateway contract.

Each Provider instance selects exactly one profile. A credential may be reused
by multiple Mantle Provider instances only when their encrypted audience,
access surface, and credential scheme are identical. Runtime and Mantle keep
separate Provider IDs, concurrency limits, circuit state, capability evidence,
and accounting targets.

## Consequences

Applications gain direct Mantle wire compatibility without routing Mantle
traffic through Converse. The explicit profiles make protocol and model
compatibility visible per deployment. Operators who need all three wire APIs
create separate Provider instances and can bind them to the same approved
Mantle credential.

Phase 1C does not implement Mantle Models as a public Gateway endpoint,
stateful Responses, Workspaces/Projects, hosted tools, `count_tokens`, API-key
generation, or quota discovery. Those require separate ownership and operator
contracts.

## Amendment, 2026-08-12: what "immutable profile" and "no Projects" mean

Two things this ADR asserted were not true of the code, and one term it used
turned out to name a different AWS product. Recorded here rather than rewritten
above, so the correction is visible.

**The profiles were not immutable in practice.** The list of profiles whose
capability set the build fixes lived only in the Admin handler, and the three
Mantle profiles were absent from it. A stored binding could therefore claim
capabilities the profile does not serve, and the loader passed the binding's
capabilities straight to the adapter. The list is now
`domain.IsImmutableCapabilityProfile`, checked in `ProviderProfileBinding`
validation — which every write into the store crosses — and again by the
registry loader, which withholds an over-ceiling record from routing instead of
refusing the load.

**Workspaces and Projects are one Bedrock resource, and the header names differ
per protocol.** AWS documents Workspaces (Anthropic-compatible) and Projects
(OpenAI-compatible) as the same underlying project resource, selected by
`anthropic-workspace` on `/anthropic/v1/messages` and by `OpenAI-Project` on
the OpenAI-shaped paths. `anthropic-workspace-id` belongs to Claude Platform on
AWS — a different service on `aws-external-anthropic.<region>.api.aws` — and
must never be sent to Mantle.

Halro sends neither project header. Each AWS account has a default project and
a request that omits the header is associated with it, so the profiles are
usable, and reachable only for that default project. That is a product limit
stated in the release notes, not an oversight: addressing a non-default project
needs a durable, typed provider-level field and is deferred to its own change.

**A deployment's region must match its provider endpoint's region.** A Bedrock
project is region-scoped — the region is in its ARN — so a declared region that
disagrees with the endpoint keys the catalog and the capability evidence on a
region no request can reach. The Admin API refuses that combination on Mantle
rather than letting the explicit value win.

The wire contract in the Decision section — three paths, two credential header
shapes, `store:false` — is pinned by contract tests against fake servers, and
those tests also assert that no project or workspace header is sent. None of it
has been verified against a real Bedrock Mantle account; see
`docs/verification/provider-real-matrix.md`.
