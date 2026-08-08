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

## Model capability catalog

A profile ceiling states what a wire protocol can carry, not what one model
behind it does. `internal/modelcatalog` holds the second claim: that a specific
model, reached through a specific profile and region, supports a specific set of
operations.

Effective capabilities are an intersection, and every layer may only narrow:

```text
profile ceiling  ∩  model catalog entry  ∩  operator-retained subset
```

Rules the catalog holds to:

- **A `/models` response is existence, not capability.** `provider.ModelLister`
  returns identifiers and no capability claims; nothing in the catalog is
  derived from one.
- **Unknown means zero.** A model with no entry resolves to `unknown` with no
  capabilities, never to the profile ceiling. The operator declares what it does
  before it can serve.
- **Exact model matching.** A prefix or family rule must never promote an
  unknown future model to known capabilities. An entry with no region applies to
  every region, which is itself a claim that the capability does not vary by
  region.
- **Disagreement fails closed.** When sources disagree about a capability it is
  switched off and the model is marked `conflicting`. Silence is not denial: a
  source that says nothing about a capability does not veto another's evidence,
  which is why claims carry asserted-supported and asserted-unsupported
  separately.
- **Only the builtin catalog pre-selects.** Provider metadata is external input;
  it may inform a claim but never arrives pre-checked, and no source but an
  explicit probe may claim `verified` evidence.
- **Nothing widens a profile.** Every merge is clamped to the ceiling, so
  upstream metadata cannot loosen the deliberately pinned Gemini, Bedrock, or
  Bedrock Mantle Beta limits.

Sources are `builtin_catalog`, `provider_metadata`, `verified_probe`,
`operator_declared`, and `unsupported`; statuses are `known`, `partial`,
`unknown`, and `conflicting`.

### Carrier and release cadence

Catalog entries are Go source, reviewed like code and shipped with the binary,
on the same release path as the profile ceilings they must stay under. There is
no second embedded data format and no runtime catalog fetch.

The consequence is deliberate: a model Halro has not shipped an entry for is
`unknown` until either a release adds one or the operator declares it. The
manual model-ID path therefore stays available permanently — it is the escape
hatch for anything the catalog does not yet cover.

An entry is added only against evidence. The shipped seed covers the four
profiles that already pin exactly one model each — Titan Text Embeddings V2,
Titan Image Generator V2, Cohere Rerank 3.5, and Nova Reel — where "profile
implies model" is enforced in the adapter, so the model's capabilities are the
profile's. Chat model line-ups are not seeded from names.

Digests: each entry carries a per-model revision, and that is what conflict
detection compares. The catalog-wide digest is diagnostic only, because it
rotates whenever any unrelated model appears and would turn every concurrent
edit into a spurious conflict.

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
| AWS Bedrock Invoke Titan Text Embeddings V2 | Beta | no | no | one string; float; 256/512/1024 | no | SigV4, encrypted static JSON |
| OpenAI Media/Resources | Beta | no | no | no | Moderations, Images, Audio, Files, Batches | Bearer key |
| AWS Bedrock Titan Image V2 | Beta | no | no | no | strict image generation | SigV4, encrypted static JSON |
| AWS Bedrock Agent Runtime Cohere Rerank 3.5 | Beta | no | no | no | strict rerank | SigV4, encrypted static JSON |
| AWS Bedrock Nova Reel Async | Beta | no | no | no | async video with explicit S3 output | SigV4, encrypted static JSON |
| AWS Bedrock Mantle OpenAI Chat | Beta | yes | yes | no | tools/vision/JSON/reasoning | Bedrock API key as Bearer |
| AWS Bedrock Mantle Responses | Beta | stateless | text SSE | no | tools/vision/JSON; no reasoning output | Bedrock API key as Bearer |
| AWS Bedrock Mantle Anthropic Messages | Beta | yes | yes | no | tools/vision/reasoning | Bedrock API key as `x-api-key` |

The Gemini Beta adapter translates text messages, system instructions,
generation limits, stop sequences, finish reasons, usage metadata, SSE chunks,
and string/string-array embeddings. It deliberately rejects multimodal content,
tool messages/calls, JSON mode, and base64 embedding output until their semantic
contracts and redaction behavior have dedicated tests. API keys are sent only
in the header, never in query strings.

The Bedrock Converse adapter translates system/developer, user, and assistant
text messages; normalizes output text, finish reasons, and token usage; and
validates AWS EventStream prelude and message CRCs before emitting semantic
chunks. It rejects tool, vision, JSON-mode, embedding, unknown-event, and
truncated-stream inputs instead of silently downgrading them. The separate Titan
Text Embeddings V2 InvokeModel profile accepts one string and validates the
versioned native schema, dimensions, float vector and token usage. It rejects
batch fan-out and arbitrary JSON. Static access key, secret, optional session
token, and region are encrypted as one audience-bound credential. The region
must match the endpoint hostname. Runtime adapters neither read environment
credentials nor contact IMDS.

Mantle is a separate access surface. Only regional
`bedrock-mantle.<region>.api.aws` origins are accepted. Each Provider instance
selects one immutable wire profile; Runtime credentials cannot be attached to
Mantle. The Responses profile participates only in Halro's stateless tier
and always sends `store:false`. Native Anthropic routing preserves validated
thinking signatures and raw event order while remaining pinned to the selected
Mantle Anthropic profile.
