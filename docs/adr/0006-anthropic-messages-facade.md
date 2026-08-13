# ADR 0006: Anthropic Messages facade execution modes

- Status: Accepted
- Date: 2026-08-01
- Issues: #32, #33, #34, #35

## Context

Halro needs an Anthropic-compatible `POST /v1/messages` facade without
pretending that every Anthropic field has an equivalent OpenAI, Gemini, or
Bedrock meaning. In particular, Anthropic tool selection and signed thinking
blocks have provider-specific constraints. Signed `thinking` and
`redacted_thinking` blocks in the latest assistant turn must be returned in
their original order and without changing their values.

## Decision

The facade supports two explicit modes selected by the
`Halro-Route-Mode` request header:

- `portable` is the default. The request is decoded into the approved
  Canonical `generate` subset. Only candidates whose versioned profile can
  represent every request requirement are eligible. Lossy or unknown fields
  are rejected before provider I/O.
- `native` retains the validated request in a versioned `NativeEnvelope` and
  derives only a bounded `GovernanceView`. Routing is pinned to an exact
  Anthropic Messages provider profile. Cross-provider fallback is disabled.

`anthropic-version` is required on every request. Phase 1B accepts
`2023-06-01`. `anthropic-beta` is rejected until each beta is registered in a
new profile revision. For official SDK compatibility, the gateway accepts its
own Gateway Key in `x-api-key` (or the existing Authorization Bearer form), but
never forwards that value as the upstream provider credential.

Portable mode initially supports text, URL image inputs, client function
tools, `tool_use`, `tool_result`, the top-level system prompt, stop sequences,
sampling values that the selected profile proves compatible, and the four
portable tool-choice meanings. Provider-native cache controls, hosted tools,
containers, citations, document/search blocks, and thinking configuration are
not silently dropped.

Native mode forwards only schema-validated and allowlisted fields and headers.
The exact raw request body is retained only for the in-flight call. Thinking
blocks are never normalized, redacted, logged, persisted, or converted across
profiles. If a policy would need to rewrite an opaque block, the request fails
closed.

The provider stream is validated as:

1. `message_start`
2. zero or more ordered `content_block_start`, matching delta events, and
   `content_block_stop`
3. one `message_delta`
4. `message_stop`

`ping` may appear between lifecycle events. A provider `error` event terminates
the stream. No retry or fallback is allowed after the first northbound event.

## Tool-choice compatibility

The semantic matrix is executable test data, not an assumed enum cast:

- OpenAI `required` corresponds to Anthropic `any` and Gemini `ANY` only when
  all declared functions remain eligible.
- OpenAI named function corresponds to Anthropic `tool` and Gemini `ANY` with
  one `allowed_function_names` entry.
- Anthropic `disable_parallel_tool_use` has cardinality implications that are
  recorded separately from the selection mode.
- Gemini `VALIDATED` has no lossless OpenAI or Anthropic equivalent and is not
  emitted by portable conversion.
- Strict schema guarantees are profile capabilities, not a property inferred
  from the presence of a JSON Schema.

## Consequences

Native mode provides provider fidelity at the cost of provider pinning and no
fallback. Portable mode retains gateway mobility but rejects features whose
semantics are not proven. Stored resources, media, Realtime, and HA/Cluster
remain outside Phase 1B.

`count_tokens` and provider-executed (hosted) tools were later added on the same
axis this ADR establishes. `count_tokens` is served natively against the direct
Anthropic profile only, and settled at zero cost while still taking a ledger
attempt, because it is a real provider call on the operator's credential.
Provider-executed tools are admitted by an explicit `provider_executed_tools`
capability rather than by the decoder: what makes them different is not their
shape but their egress — the upstream originates network calls that never pass
through SafeTransport.
