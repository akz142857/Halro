# Provider capability contract

Each immutable Deployment snapshot declares:

- chat;
- embeddings;
- streaming;
- tools;
- vision;
- JSON response;
- developer role;
- reasoning;
- usage in stream;
- maximum context and output tokens.

The Adapter may discover capabilities during configuration validation, but the request path reads only the validated immutable Deployment snapshot.

Unsupported capabilities produce a stable request error. Provider implementations may not silently ignore fields.

The Gateway derives requirements from the actual request before any Provider
call: tools/tool messages, multimodal image parts, JSON response formats,
developer messages, reasoning fields, and streamed usage. It removes
incompatible fallback candidates while preserving route order. If no candidate
remains it returns `unsupported_feature` without calling an upstream. Estimated
input plus requested output must also fit both token limits; otherwise it
returns `token_limit_exceeded` before reservation or upstream I/O.

Provider capability declarations are an upper bound. A Deployment may narrow
boolean capabilities and token limits but cannot expand them. `0` means that no
limit was declared at that layer; an inherited non-zero Provider limit cannot
be erased by a Deployment.

## Shipped profiles

| Profile | Stage | Chat | Stream | Embeddings | Tools/Vision/JSON | Authentication |
|---|---|---:|---:|---:|---:|---|
| OpenAI | GA | yes | yes | yes | yes | Bearer key |
| Anthropic Messages | GA | yes | yes | no | tools/vision/reasoning | `x-api-key` |
| Azure OpenAI | GA | yes | yes | yes | yes | `api-key` |
| DeepSeek | GA | yes | yes | no by default | tools/JSON | Bearer key |
| OpenAI-compatible | GA | yes | yes | yes | opt-in | Bearer key |
| Gemini generateContent | Beta | text | yes | float | not declared | `x-goog-api-key` |
| AWS Bedrock Converse | Beta | text | yes | no | not declared | SigV4, encrypted static JSON |

The Gemini Beta adapter translates text messages, system instructions,
generation limits, stop sequences, finish reasons, usage metadata, SSE chunks,
and string/string-array embeddings. It deliberately rejects multimodal content,
tool messages/calls, JSON mode, and base64 embedding output until their semantic
contracts and redaction behavior have dedicated tests. API keys are sent only
in the header, never in query strings.

The Bedrock Beta adapter translates system/developer, user, and assistant text
messages to Converse; normalizes output text, finish reasons, and token usage;
and validates AWS EventStream prelude and message CRCs before emitting semantic
chunks. It rejects tool, vision, JSON-mode, embedding, unknown-event, and
truncated-stream inputs instead of silently downgrading them. Static access key,
secret, optional session token, and region are encrypted as one audience-bound
credential. The region must match the endpoint hostname. The adapter neither
reads environment credentials nor contacts IMDS.
