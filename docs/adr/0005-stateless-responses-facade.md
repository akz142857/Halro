# ADR 0005: Stateless OpenAI Responses facade

Status: Accepted for Phase 1A

Date: 2026-08-01

## Context

The OpenAI Responses API combines a typed item/event protocol with optional
provider-owned resources: stored Responses, Conversations, background work,
retrieval, cancellation, deletion, input-item listing, and webhooks. Heimdall
does not yet have a cross-provider ownership model for those resources.

The Phase 0 semantic `generate` operation and versioned ProviderPrimitive
registry already preserve authentication, routing, budget reservation,
accounting, retry fencing, redaction, model aliases, and provider capability
preflight for short-lived inference. Phase 1A needs to add the Responses wire
protocol without duplicating that authority or pretending that provider state
is portable.

## Decision

Heimdall publishes only `POST /v1/responses` as the versioned northbound
profile `openai.responses.stateless.v1`.

The endpoint is a stateless protocol facade:

1. Decode a strict Responses request and reject unknown fields.
2. Reject stateful, resource-owning, or unproven features before Provider I/O.
3. Convert only the declared portable subset to the canonical semantic
   `generate` operation.
4. Execute that request through the selected deployment's existing,
   versioned generation ProviderPrimitive.
5. Render a Responses object or typed Responses SSE events from the canonical
   result/events.

The `resp_*` and output-item IDs returned by this tier are ephemeral Gateway
correlation identifiers. They are not provider resource IDs, cannot be used
with a retrieval API, and are not stored by Heimdall.

## Resource ownership

- Omitted `store` means `false` in Heimdall; `store: true` is rejected.
- `previous_response_id`, `conversation`, `background`, prompt resources,
  metadata persistence, retrieval, deletion, cancellation, input-item listing,
  compaction/context management, and webhooks are unavailable.
- Provider-owned Responses IDs are neither exposed as durable resources nor
  accepted as routing input.
- A future stored tier requires a new ADR defining provider/deployment/profile/
  region binding, lifecycle, encryption, deletion, and failover behavior.

## Portable subset

Phase 1A accepts:

- string input;
- message items containing text and supported input-image parts;
- portable `function_call` and string `function_call_output` input items;
- top-level instructions;
- temperature, top-p, and maximum output tokens;
- non-streaming function definitions and tool choice when the selected profile
  proves support;
- text, JSON object, and JSON Schema output formats when the selected profile
  proves support;
- non-streaming text and function-call output items;
- streaming output text.

Phase 1A rejects hosted tools, MCP tools, strict function tools, input files,
references to stored items, reasoning/encrypted-thinking items, reasoning
output, streaming refusals, and streaming function calls. These are
compatibility limits, not silently ignored fields.

For redaction safety, response metadata does not echo the original
instructions, tool definitions/tool choice, or structured schema bodies. Those
values have not passed through the translated Chat request's outbound
redaction path, so Phase 1A returns conservative null/empty/default metadata
instead of re-exposing unredacted request configuration. Generated output
items still pass through the existing outbound redaction authority.

## Provider routing

The northbound Responses profile is orthogonal to the provider profile. A
portable request is converted to semantic `generate` and resolved through the
existing Chat/Generate ProviderPrimitive binding for OpenAI, Azure OpenAI,
DeepSeek, OpenAI-compatible, Gemini, or Bedrock profiles. Field-level profile
coverage and capability evidence still filter candidates before an attempt.

Phase 1A deliberately does not add a provider-native stored Responses
primitive. Such a primitive may be added later under a new profile without
changing the canonical operation or weakening this stateless contract.

## Streaming contract

Text streams use deterministic, monotonically sequenced events:

1. `response.created`
2. `response.in_progress`
3. `response.output_item.added`
4. `response.content_part.added`
5. zero or more `response.output_text.delta`
6. `response.output_text.done`
7. `response.content_part.done`
8. `response.output_item.done`
9. `response.completed` or `response.incomplete`

There is no Chat Completions `[DONE]` sentinel. Before the first emitted event,
normal bounded fallback remains possible. After the first event, the attempt is
ambiguous and no cross-provider retry is allowed. A terminal transport failure
uses a typed `error` event when the HTTP response has already started.

## Accounting and policy

Responses does not create a parallel request authority. Translation delegates
to the existing generation hot path, so authentication, source policy, token
guard, limits, budget reservation, per-attempt accounting, conservative unknown
settlement, redaction, cancellation, retry, and model alias rules remain
unchanged.

## Compatibility evidence

The reviewed machine-readable contract is
[`docs/compatibility/endpoint-manifests.json`](../compatibility/endpoint-manifests.json).
Unit and golden tests cover strict decoding, canonical mapping, state rejection,
result rendering, event order, and pre-provider fencing. The compatibility
server exercises JSON and SSE through the pinned official OpenAI Go, Node.js,
and Python SDKs.

## Consequences

- Applications can adopt the Responses object/event model without giving
  Heimdall responsibility for durable provider resources.
- Existing provider integrations remain reusable through the semantic and
  primitive layers.
- Some valid OpenAI requests receive stable 4xx responses because their
  semantics are outside the declared tier.
- Native reasoning, native hosted tools, stored Responses, and streaming tool
  calls require later evidence and versioned contract expansion.
