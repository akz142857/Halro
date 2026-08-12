# OpenAI compatibility contract

## v1 endpoints

- `POST /v1/chat/completions`
- `POST /v1/embeddings`
- `POST /v1/responses` (`openai.responses.stateless.v1`)

## Required request matrix

- text and structured message content;
- tools, tool choice, parallel tool calls;
- response format;
- stop, seed, n;
- temperature and top-p;
- max tokens and max completion tokens;
- stream options with usage.

Unknown or unsupported parameters are rejected unless a deployment explicitly declares a transform. Silent dropping is forbidden.

## Required response matrix

- id, object, created, model;
- choices and message/delta;
- finish reason;
- tool calls and argument fragments;
- usage;
- generated or safely propagated request ID.

## SSE

- `data:` framing;
- empty deltas;
- role/content/reasoning/tool semantic channels;
- tool argument fragments;
- usage-only terminal chunk;
- one `[DONE]` on normal completion;
- defined malformed-event and disconnect handling.

Compatibility is tested with the Python, Node, and Go OpenAI SDKs. A Halro stream error extension is not represented as a standard OpenAI guarantee.

When a durable Admin change has not reached every running authorization and
routing snapshot, all OpenAI-family endpoints fail closed with HTTP `503`, an
OpenAI error envelope whose code is `configuration_stale`, and
`Retry-After: 5`. The runtime retries activation every five seconds; this is a
temporary gateway state, not evidence that a Provider received the request.

## Stateless Responses tier

The Responses endpoint has its own typed item and event contract; it is not a
Chat Completions response with renamed fields. Phase 1A supports strict Create
and text SSE only. Omitted `store` is treated as false. `store: true`,
`previous_response_id`, Conversations, background mode, prompt resources,
webhooks, retrieve/delete/cancel/input-items operations, hosted tools, strict
function tools, reasoning output, and streaming function calls are rejected
before Provider I/O.

For redaction safety, Phase 1A does not echo original instructions, tool
definitions/tool choice, or structured schema bodies in response metadata;
those response fields use conservative null/empty/default values.

Portable input messages, instructions, scalar generation controls, supported
function calls, and supported text formats are mapped to semantic `generate`
and executed through the selected deployment's versioned generation primitive.
The exact per-provider field coverage is authoritative in
[`endpoint-manifests.json`](../compatibility/endpoint-manifests.json).

Responses SSE uses named `response.*` events with monotonic sequence numbers
and ends at `response.completed` or `response.incomplete`; it never emits the
Chat Completions `[DONE]` sentinel. See
[ADR 0005](../adr/0005-stateless-responses-facade.md) for ownership, event, and
termination decisions.
