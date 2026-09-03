# OpenAI compatibility contract

## v1 endpoints

- `POST /v1/chat/completions`
- `POST /v1/embeddings`
- `POST /v1/responses` (`openai.responses.deferrable.v1`)
- `GET /v1/responses/{id}`, `POST /v1/responses/{id}/cancel`, `DELETE /v1/responses/{id}`
  — the deferred tier only: they address a submission made with `background: true`
  and never a synchronous response, which is not stored (ADR 0024)

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
webhooks, retrieve/delete/cancel/input-items operations, strict function tools,
reasoning output, and streaming tools are rejected before Provider I/O.

`tools: [{"type": "web_search"}]` is the one hosted tool accepted. It names a
tool the upstream runs itself, so it is routed against the
`provider_executed_tools` capability and reaches only a connection whose
operator turned that on — enabling it accepts that the provider originates
network calls that never pass through SafeTransport. It is served by the
`openai.responses.v1` provider profile and by no other, because no other
profile's wire form can carry it. `code_interpreter` and `file_search` are
rejected: both are provider-side state, and a gateway whose consistency
boundary is one process owning one data directory has nowhere to hold a handle
to somebody else's.

A search comes back as a `web_search_call` output item, with the query the
model wrote, and as `url_citation` annotations on the answer text. Both are
carried; a profile that cannot represent either refuses the result rather than
returning the answer with its sources removed.

For redaction safety, Phase 1A does not echo original instructions, tool
definitions/tool choice, or structured schema bodies in response metadata;
those response fields use conservative null/empty/default values.

Portable input messages, instructions, scalar generation controls, supported
function calls, and supported text formats are mapped to semantic `generate`
and executed through the selected deployment's versioned generation primitive.
That mapping is direct: the endpoint decodes its own wire form into the
semantic request and enters the same hot path Chat Completions enters, rather
than writing itself as a Chat Completions request first. What a Responses
request may contain is therefore bounded by the semantic model and by the
selected profile's declared coverage, not by what the Chat Completions wire can
express.

The exact per-provider field coverage is authoritative in
[`endpoint-manifests.json`](../compatibility/endpoint-manifests.json).

Responses SSE uses named `response.*` events with monotonic sequence numbers
and ends at `response.completed` or `response.incomplete`; it never emits the
Chat Completions `[DONE]` sentinel. See
[ADR 0005](../adr/0005-stateless-responses-facade.md) for ownership, event, and
termination decisions.
