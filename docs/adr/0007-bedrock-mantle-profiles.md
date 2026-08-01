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

The Responses profile participates in Heimdall's existing stateless Responses
tier. Heimdall always sends `store:false` and does not expose Mantle response
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
