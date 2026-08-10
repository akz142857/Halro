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

Effective capabilities are resolved per exact invocation target and binding,
and every layer may only narrow:

```text
binding/profile ceiling  ∩  active capability claims  ∩  operator-retained subset
```

Rules the catalog holds to:

- **Discovery is existence, not capability.** `provider.InvocationTargetLister`
  returns normalized `InvocationTargetDescriptor` values. A target remains
  `unknown` unless an independent exact claim exists. The Admin endpoint is
  `/providers/{id}/invocation-targets`; the pre-1.0 `/models` endpoint has been
  removed.
- **Provider metadata is allowlisted by its Adapter.** Only reviewed structured
  fields may become `provider_metadata` claims. Names, owners, families, and
  unknown fields are never evidence.
- **Unknown means zero until evidence exists.** A model with no entry resolves
  to `unknown` with no capabilities, never to the profile ceiling. An explicit
  safe capability detection may create `verified_probe` evidence; the advanced
  fallback remains an operator declaration.
- **Exact model matching.** A prefix or family rule must never promote an
  unknown future model to known capabilities. An entry with no region applies to
  every region, which is itself a claim that the capability does not vary by
  region.
- **Disagreement fails closed.** When sources disagree about a capability it is
  switched off and the model is marked `conflicting`. Silence is not denial: a
  source that says nothing about a capability does not veto another's evidence,
  which is why claims carry asserted-supported and asserted-unsupported
  separately.
- **Only resolved variants pre-select.** A variant may use exact builtin claims,
  Adapter-allowlisted Provider metadata, or a successful explicit probe. Every
  result is clamped to one Binding and contradictory claims emit no variant.
- **Nothing widens a profile.** Every merge is clamped to the ceiling, so
  upstream metadata cannot loosen the deliberately pinned Gemini, Bedrock, or
  Bedrock Mantle Beta limits.

Claim sources are `builtin_catalog`, `provider_metadata`, `signed_catalog`,
`verified_probe`, and `operator_declared`; statuses are
`supported`, `unsupported`, `unknown`, and `conflicting`. Deployment snapshots
retain the catalog status (`known`, `partial`, `unknown`, `conflicting`) for
compatibility with stored evidence.

Each Claim is scoped to provider ID, target kind and ID, Binding, Profile, and
location semantics. Provider metadata expires with the target-cache lifetime;
expiry only affects future resolution. A Deployment save binds the selected
variant revision and stores its Claim revisions in an immutable snapshot. A
stale save receives `409 resolution_changed` with mismatch names and the latest
resolution.

### Carrier and release cadence

Builtin entries are rendered into the same versioned snapshot schema used by
the optional signed catalog and remain available offline. In 1.1.0 an
administrator may explicitly enable background refresh from the compiled
catalog endpoint. The reader verifies an Ed25519 signature against release-
embedded public roots, an exact schema and capability-dictionary version,
monotonic sequence, revision, expiry, entry count, and download/decompression
limits before atomically replacing its last-known-good snapshot. The request
path never downloads a catalog.

Signed entries may add or revoke only exact provider/profile/target-kind/model/
region identities and capability IDs already understood by the binary. They cannot add request
protocols, authentication, hosts, or SSRF policy and never imply that the
current Provider credential can enumerate or invoke a target. Failure, future
schema, rollback, or loss of the update service preserves an unexpired last-
known-good or bundled catalog and reports `catalog_unavailable`. An expired
last-known-good remains visible for provenance and sequence protection but
cannot establish a new variant or save. No failure rewrites stored Deployment snapshots. See ADR 0020 and the catalog
publishing runbook.

The consequence is deliberate: a model with no exact builtin, signed, metadata,
or verified-probe claim is `unknown`. The manual model-ID path stays available
permanently as the escape hatch for targets that cannot be detected.

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

Mantle is a separate access surface — for which of the two an operator should
pick, and what neither supports, see
[选择 AWS 接入面](../guides/aws-surface-selection.md). Only regional
`bedrock-mantle.<region>.api.aws` origins are accepted. Each Provider instance
selects one immutable wire profile; Runtime credentials cannot be attached to
Mantle. The Responses profile participates only in Halro's stateless tier
and always sends `store:false`. Native Anthropic routing preserves validated
thinking signatures and raw event order while remaining pinned to the selected
Mantle Anthropic profile.
