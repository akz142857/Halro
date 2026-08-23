# Image input across provider platforms

Normative for what Halro accepts as an image and where it will route one. Every
row was read from the provider's own documentation on the date given; the
declarations in `internal/compatibility/provider_fields.go` and the published
manifests in `docs/compatibility/endpoint-manifests.json` are expected to agree
with it, and a disagreement is a bug in one of the two.

An image reaches the gateway as one of two things, and the difference decides
almost every row below:

- **inline** — the bytes travel in the request. OpenAI and the compatible
  surfaces spell this as a `data:` URL; Anthropic spells it as a
  `{"type":"base64","media_type":…,"data":…}` source. Halro's portable model
  carries both as a data URL, so the two spellings convert without loss.
- **fetched** — the request names an address and the *provider* retrieves it.

Halro never retrieves the address itself. Doing so would make the gateway fetch
a caller-supplied URL, which is the request forgery `safetransport`'s host
allowlists exist to prevent, so a platform that does not fetch is declared
unable to serve the field rather than served by proxy.

## Matrix

| Profile | Vision declared | Inline (base64 / data URL) | Fetched (http URL) | Read from |
| --- | --- | --- | --- | --- |
| `openai.chat-embeddings.v1` | yes | yes | yes | OpenAI images-vision guide, 2026-08 |
| `azure-openai.chat-embeddings.v1` | yes | yes | yes | same wire form; the pinned `api-version` governs availability |
| `anthropic.messages.2023-06-01` | yes | yes | yes | Anthropic vision guide, 2026-08 |
| `bedrock.mantle.chat.v1` | yes | yes | **no** | Bedrock takes image bytes and does not fetch |
| `bedrock.mantle.openai.chat.v1` | yes | yes | **no** | as above |
| `bedrock.mantle.responses.v1` | yes | yes | **no** | as above |
| `bedrock.mantle.openai.responses.v1` | yes | yes | **no** | as above |
| `bedrock.mantle.anthropic.messages.v1` | yes | yes | **no** | "On Amazon Bedrock and Google Cloud, only base64-encoded sources are currently available" — Anthropic vision guide |
| `openai-compatible.chat-embeddings.v1` | **no** | — | — | the endpoint behind it is unknown, so nothing is claimed for it |
| `deepseek.chat.v1` | ceiling only | yes | yes | one model; see "DeepSeek" below |
| `gemini.generate-content.text.v1beta` | **no** | — | — | Beta profile, deliberately pinned to text |
| `bedrock.runtime.converse.text.v1` | **no** | — | — | Beta profile, deliberately pinned to text |

A profile that does not declare vision is not a claim that the provider cannot
see: it is a claim that *this* profile does not carry an image. Routing removes
such a target before any provider call, so an image request simply never
arrives there.

## Members that do not survive the crossing

- `detail` (OpenAI's fidelity hint) has no counterpart in an Anthropic image
  source, whose members are exactly `type` plus `media_type`/`data`, `url`, or
  `file_id`. It is declared unsupported for the Anthropic-shaped profiles at any
  value other than `auto` — `auto` is what omitting the member already means, so
  it costs the caller nothing and must not route them away.
- A `file_id` source names an object one provider's Files API holds. There is no
  portable identifier for it, so portable mode refuses it rather than forwarding
  an id another provider has never seen. Native mode passes it through untouched.

## Size, before the provider's own limit applies

`server.max_request_bytes` (10 MiB by default) bounds the whole body, and a
project may declare a smaller ceiling of its own. base64 inflates a file by
about 1.37×, so the instance default admits an image of roughly 7 MB before any
provider limit is reached. Published provider limits for comparison: OpenAI
512 MB total payload and 1500 images per request; Anthropic 10 MB per image
direct and 5 MB on Bedrock and Google Cloud; DeepSeek 32 MiB per image with a
48 MiB body.

Halro's pre-flight token estimate charges each image
`semantic.ImageInputTokenCeiling` rather than measuring its encoded length —
see the constant's own documentation for why, and note that the project input
limit, the deployment context window, the TPM lease and the budget reservation
are all measured against that estimate.

## DeepSeek

DeepSeek serves images on `deepseek-v4-flash-vision-exp` and answers one with a
400 on every other model. It accepts a data URL, an https URL up to 8192
characters, and a Files API reference; the wire form is OpenAI's, and
`RenderDeepSeekChatRequest` forwards `messages` unchanged, so both an inline and
a fetched image cross intact.

The profile carries vision at its **ceiling only**. Its defaults do not, so a
new DeepSeek connection claims nothing about images, and the per-model claim
lives in the model catalogue, where `deepseek-v4-flash-vision-exp` carries
vision and the two text models do not. An operator whose deployment points at
the vision model can opt in; one pointing at a text model cannot pick vision up
by association.

This is the second ceiling-over-defaults opt-in in the profile table, after
provider-executed tools on Anthropic Messages, and `TestOnlyNamedProfilesHaveAWiderCeiling`
holds the list to exactly those two: any other profile whose ceiling drifts past
its defaults fails, and provider-executed tools may still appear in no ceiling
but Anthropic's.
