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

Each AWS account has a default project, and a request that omits the header is
associated with it. At the time of this amendment Halro sent neither header, so
only that default project was reachable; the amendment below supersedes that
with an optional provider-level project.

**A deployment's region must match its provider endpoint's region.** A Bedrock
project is region-scoped — the region is in its ARN — so a declared region that
disagrees with the endpoint keys the catalog and the capability evidence on a
region no request can reach. The Admin API refuses that combination on Mantle
rather than letting the explicit value win.

The wire contract in the Decision section — three paths, two credential header
shapes, `store:false` — is pinned by contract tests against fake servers. None
of it has been verified against a real Bedrock Mantle account; see
`docs/verification/provider-real-matrix.md`.

## Amendment, 2026-08-12: a Bedrock Project is a Provider-level property

Addressing a non-default Bedrock Project is now supported through an optional
`ProviderInstance.BedrockProjectID`.

**Why the provider and not the binding.** A Profile Binding is uniquely keyed by
(provider, profile) and a provider may not repeat a profile, so a binding cannot
carry a dimension an operator needs several of at once. Two projects are two
Provider instances, which may share one Credential — they keep their own
concurrency limit, circuit state, capability evidence and accounting target,
which is the isolation the Projects feature is for on the AWS side too.

**Why not the deployment.** Deployment-level projects would make the project a
per-request routing dimension: it would have to cross the semantic operation and
the adapter call interface, and the meaning of a project against Route, Halro's
own Project, budgets, cost attribution and fallback isolation would all have to
be defined. Nothing asks for that yet.

**Rendering.** Empty sends no header and AWS associates the request with the
account default. A value renders as `OpenAI-Project` on the two OpenAI-shaped
profiles and as `anthropic-workspace` on the Messages profile. One helper owns
both header names and clears every project-selecting header it knows —
including `anthropic-workspace-id`, which it exists to delete and never to send
— before setting the right one. It is deliberately not part of the credential
authorizer and deliberately not a free-form header map: a map would let a stored
value name `Authorization`, and the authorizer's header clearing exists to make
that impossible.

**Validation.** `proj_` followed by alphanumerics, bounded in length. The
literal `default` is the account default's own ID and normalises to empty, so
"the default project" has one stored spelling. A `wrkspc_` value is refused by
name because it belongs to Claude Platform on AWS, and pasting one product's
identifier into the other's field is the likeliest mistake here. The field is
refused outside the Mantle surface, where it would be stored and never sent.
There is no online existence check: AWS's own default policy for long-term API
keys allows only get/list on projects, so a save-time lookup would make Provider
creation depend on an upstream call whose permission varies by key type. A
missing or archived project surfaces at request time, as a 403 that — like every
other authentication failure — is not retried and not failed over.

**No migration.** The field is absent from every record written before it
existed, and absent means the account default, which is exactly what those
providers did. Nothing stored changes meaning, so there is no schema version to
bump and no data directory to rebuild.
