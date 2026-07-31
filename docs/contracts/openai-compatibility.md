# OpenAI compatibility contract

## v1 endpoints

- `POST /v1/chat/completions`
- `POST /v1/embeddings`

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

Compatibility is tested with the Python, Node, and Go OpenAI SDKs. A Heimdall stream error extension is not represented as a standard OpenAI guarantee.
