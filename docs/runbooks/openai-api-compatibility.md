# OpenAI API compatibility review — 2026-11-03

This review covers the OpenAI endpoints Halro already exposes. Findings are
based on the official references linked below and local transport/contract tests.
No real-provider inference was run. A declaration is not live verification.

## GPT-6 tools and reasoning

| Exact upstream model | Chat Completions function tools | Reasoning effort |
| --- | --- | --- |
| `gpt-6-astra`, `gpt-6.1-sol` | Responses required | low / medium / high / xhigh / max |
| `gpt-6.1-terra` | Responses required | none / low / medium / high / xhigh / max |
| `gpt-6-sol`, `gpt-6-luna` | Only with explicit `none`; otherwise use Responses | none / low / medium / high / xhigh / max |

These are exact reviewed identifiers, not a prefix rule. Unknown models remain
unknown and are enumerated from the upstream model list; catalog entries supply
capability evidence, never an account's model inventory.

Official sources: [GPT-6 migration parameters](https://developers.openai.com/api/docs/guides/latest-model#gpt-6-astra-update-api-and-model-parameters),
[GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol),
[GPT-6.1 Terra](https://developers.openai.com/api/docs/models/gpt-6.1-terra),
[GPT-6 Sol](https://developers.openai.com/api/docs/models/gpt-6-sol),
[GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna).

### Configure the upstream surface

1. In Providers, create an OpenAI connection and choose **Responses** under
   **Capability implementation** (`openai.responses.v1`). Reuse the existing
   encrypted credential. Chat Completions remains the default for existing
   connections; the console now exposes both explicit upstream choices.
2. Create a replacement deployment on that connection, validate it, and point
   the public model route at it. Deployment target identities are immutable.
   Keep the public alias (for example `gpt-sol`) so applications retain their model name.
3. Declare or inspect the model's actual capabilities. The Responses profile now
   carries streaming, stream usage, function tools, and reasoning. Its web-search
   capability remains an explicit operator opt-in and is supported in unary calls.
4. Applications can keep calling Halro's `/v1/chat/completions`; the selected
   Responses connection sends `/v1/responses` upstream, including for streaming.

Changing the inbound URL alone does not change the selected upstream profile.
Halro does not silently redirect a Chat-profile connection to an endpoint the
operator did not select. Updating the source checkout also does not update an
already-running container.

For the reviewed GPT-6 identifiers, invalid effort values, tools on an incompatible
Chat surface, and sampling parameters while reasoning is enabled are refused
before provider I/O. Explicit `none` is never overwritten or silently dropped.
Use Chat's `max_completion_tokens` or Responses' `max_output_tokens` for the total
answer-plus-reasoning bound. The deprecated `max_tokens` is refused for these
models rather than reinterpreted as a different token quantity.

### Multi-turn tool requests

Halro requests `reasoning.encrypted_content` with upstream `store=false`. The
original reasoning IDs, encrypted bytes and order relative to tool calls are
carried through an opaque replay envelope. Readable summaries stay in the normal
redaction path and are never hidden as a second plaintext copy inside that
envelope; upstream replay uses an empty summary array with the original ciphertext. Chat clients must preserve
`message.encrypted_content` (including its streaming delta) when returning the
assistant tool-call message alongside tool results. A plaintext reasoning summary
alone cannot be fabricated into a native upstream reasoning item. Native Responses clients can
return the complete output items. An ID-only stored-item reference is still
unsupported. Opaque state is not forwarded to other provider profiles.

Chat tools default to non-strict validation. Their conversion explicitly sends
`strict:false` unless the caller supplied another value. Native Responses keeps
its own default and carries explicit `strict:true` or `strict:false`. Profiles
that cannot preserve strict enforcement reject it before provider I/O.

Streaming uses the real Responses SSE protocol. Text, parallel function-call
argument fragments, reasoning summaries, opaque replay state and final usage
survive. When reasoning precedes a function call, Halro holds that call's
arguments until the terminal snapshot supplies the complete opaque replay state,
then emits the reasoning state before the tool fragments. This also preserves
replay when no reasoning summary was requested, at the cost of delaying those
tool fragments until upstream completion. Calls without preceding reasoning
continue to stream immediately. Chat tool indices are contiguous from zero,
independent of Responses output-item indices, and the assistant role is emitted
once per turn. Terminal output is checked against streamed deltas. EOF without a
completed/incomplete event is an ambiguous failure; partial output is not replayed
through another provider as if nothing had executed. Reasoning-token exhaustion
is returned as an incomplete/length result, not a successful empty answer.

Official sources: [Reasoning and stateless replay](https://developers.openai.com/api/docs/guides/reasoning),
[Function calling](https://developers.openai.com/api/docs/guides/function-calling),
[Responses streaming](https://developers.openai.com/api/docs/guides/streaming-responses).

## Other changes incorporated

During local upgrade acceptance, deleted deployments' historical clock
quarantines were found to block global readiness. Readiness and the blocked
deployment gauge now exclude explicit tombstones. Their high-water marks and
quarantine reasons remain intact; restoring a deployment restores its gate.
Disabled or missing records do not bypass quarantine.

| Area | Finding and change |
| --- | --- |
| Effort ladder | Accept `max` throughout the OpenAI wire representation. Other providers still use their reviewed effort subsets. GPT-6 probes use a supported depth rather than `minimal`. |
| Usage | Preserve cached input, cache writes and reasoning tokens in unary and streamed Responses, and in Chat conversion. These are token subsets, never additions to prompt/output totals. |
| Catalog | Add the five reviewed GPT-6 identifiers on their appropriate surfaces. Remove retired OpenAI `gpt-4.1-nano`, `o4-mini`, `gpt-image-1`, and `dall-e-3` from capability seeding. Other vendors' catalogs are unaffected. No model alias is automatically substituted. |
| Images | Support `output_format`, `background`, and `output_compression`; seed `gpt-image-2` and the Sunburst/Flare 2.5 aliases. GPT Image always returns base64. Retired image models and legacy `response_format`/`style` controls are refused before provider I/O. Other image profiles reject OpenAI-only controls. |
| File transcription | Seed `gpt-transcribe`; accept repeated multipart `languages[]` and `keywords[]`. A single legacy `language` hint is rendered as one `languages[]` value for this exact model. Mixed language spellings and invalid keywords are refused. Hints participate in inbound redaction. |
| Speech | Carry `instructions` for models that accept it, including redaction. Legacy `tts-1`/`tts-1-hd` reject it instead of silently ignoring it. |
| Interface contract | Update provider/northbound revisions, generated endpoint manifests, and the console's provider-profile fixture to match the implementation. |

Official sources: [Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching),
[Image generation API](https://developers.openai.com/api/reference/resources/images/methods/generate),
[File transcription](https://developers.openai.com/api/docs/guides/speech-to-text),
[Speech](https://developers.openai.com/api/docs/guides/text-to-speech),
[Deprecations](https://developers.openai.com/api/docs/deprecations).

## Boundaries found in the review

- Cache-write tokens are now recorded, but Halro's current deployment price model
  has no separate write-tier price. A request reporting writes is visibly marked
  `CostEstimated`; its premium cannot be claimed as an exact cost. The same price
  model does not automatically encode OpenAI's processing-tier, regional or
  long-context surcharges. Configure appropriate deployment prices and retain
  the estimate status where applicable.
- `service_tier`, prompt-cache options/retention, verbosity, reasoning `mode`/`context`,
  async tools, configuration-update items, compaction, WebSocket steering and
  multi-agent orchestration are not part of this portable contract. Unsupported
  fields/items continue to be rejected explicitly. They require their own state,
  routing or pricing design; this change does not advertise them as implemented.
- Provider-executed streaming tools, file search, code interpreter, image tools
  inside Responses and Realtime audio are not added. Image generation remains
  on `/v1/images/generations`; file transcription remains on `/v1/audio/transcriptions`.
- Official deprecations schedule Speech API models (`tts-1`, `tts-1-hd`,
  `gpt-4o-mini-tts`) for **2027-01-06**. Their replacement uses Realtime; replacing
  a model name on `/v1/audio/speech` cannot perform that migration. Existing
  transcription models shut down on **2027-02-26**; use the new file-transcription
  model for that existing workflow. No existing deployment is silently migrated.

Embeddings, moderation, files and batch resources were checked against the
existing request/transport contracts and deprecation list. This review found no
additional required wire change within Halro's supported subsets. It does not
establish account access, model availability, or correctness of a real upstream
response; those require a separately authorized live acceptance test.
