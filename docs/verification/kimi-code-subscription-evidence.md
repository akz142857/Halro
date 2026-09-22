# Kimi Code: measured on a real subscription (2026-09-22)

This is the evidence that lifted the withhold on `kimi.code.openai.chat.v1` and
`kimi.code.anthropic.messages.v1`. It is the 阶段 A fixture set that
[the Code subscription plan](../prd/code-subscription-adaptation-plan.zh-CN.md)
§11 left open, driven with an operator's own Kimi Code subscription key against
`https://api.kimi.com/coding`. No pay-as-you-go Kimi result is credited here, and
nothing in the ordinary test suite makes these calls.

Every request carried `User-Agent: Halro/<version>` — the identity
`withHalroUserAgent` forces on this product — and no request borrowed a
supported client's identity.

## The two gates

**Identity.** Accepted. Model enumeration and generation both answer 200 under
Halro's own User-Agent. The product does not appear to enforce the header at all:
the same calls answer 200 under curl's default identity. So the fixture records
that Halro's truthful identity is admitted, which is what the gate asked, and not
a mechanism that would notice if that changed.

**Reasoning.** Every model this product serves publishes
`supports_thinking_type: "only"` and reasons on a request that asks for nothing.
Both faces can be told to stop, and the two switches are different members:

| Face | Request | Result |
|---|---|---|
| Chat | nothing said | `reasoning_content` present, reasoning tokens billed, 90 prompt tokens |
| Chat | `reasoning_effort: "none"` | no `reasoning_content`, no reasoning tokens, 22 prompt tokens |
| Chat | `reasoning: {"effort":"none"}` | 200 **and it reasoned anyway** — accepted and ignored |
| Messages | nothing said | a `thinking` block with a signature |
| Messages | `thinking: {"type":"disabled"}` | a text block alone, streaming and non-streaming |

Measured on `k3`, `k3-256k` and `kimi-for-coding` alike. The nested
`reasoning.effort` spelling is the trap: the metered Kimi face refuses it
outright, this one takes it and does nothing, which is the worse of the two
answers. `RenderKimiCodeChatRequest` therefore always writes the flat
`reasoning_effort`, and writes `"none"` rather than omitting the member.

`kimi-for-coding-highspeed` could not be driven: the plan under test answers
`401 Your current subscription does not have access to
kimi-for-coding-highspeed`. Its catalogue entry is marked as reasoning unasked on
the Chat face for that reason.

## Enumeration

`GET /coding/v1/models` answers on both faces — Bearer and `x-api-key` alike —
with an OpenAI-shaped list carrying richer per-model metadata than the wire
format requires:

| id | display_name | context_length | think_efforts |
|---|---|---|---|
| `kimi-for-coding` | K2.8 Preview | 1048576 | low/high/max, default max |
| `kimi-for-coding-highspeed` | K2.7 Code Highspeed | 262144 | — |
| `k3-256k` | K3-256k | 262144 | low/high/max, default high |
| `k3` | K3 | 262144 | low/high/max, default high |

`GET /coding/v1/models/k3` answers `404 resource_not_found_error`, which is why
the adapter keeps `DisableTargetDescribe`.

Two consequences for the catalogue. The seed's identifiers were right and its
context windows were not: it claimed 256K for `kimi-for-coding`, which this plan
reports at 1M, and left `k3` unbounded, which this plan reports at 256K. The
bound is plan-dependent, so the builtin entries now claim none and enumeration
answers availability.

## The accepted member list

The Chat face shares OpenAI's wire shape and not its member list. Measured with
`reasoning_effort: "none"` so that reasoning never consumed the budget:

| Member | Result |
|---|---|
| `temperature` | 400 `invalid temperature: only 0.6 is allowed for this model` |
| `top_p` | 400 `invalid top_p: only 0.95 is allowed for this model` |
| `frequency_penalty`, `presence_penalty` | 400, each naming 0 as the only value |
| `n: 2`, `logprobs` | 400 invalid value |
| `stop` with 6 sequences | 400 `maximum length 5` |
| `stop` with a 40-byte sequence | 400 `must not be longer than 32` |
| `tool_choice` naming a function, with a depth | 400 `tool_choice 'specified' is incompatible with thinking enabled` |
| `tool_choice: "required"`, with a depth | 200 with a tool call |
| `seed`, `user`, `parallel_tool_calls: false` | 200, with nothing establishing that any was read |
| `max_completion_tokens` | 200 |
| `response_format` json_object and json_schema | 200; the schema request came back matching the schema |
| an unknown member | 200, ignored |
| `reasoning_effort` at `minimal`/`medium`/`xhigh`/nonsense | 200, and it reasoned |

That last row is why the ladder is a bound Halro applies rather than one the
upstream enforces: a caller asking for `medium` would otherwise be served, and
billed for, the product's default depth under their own name for it.

The three accepted-but-unestablished members are declared as losses rather than
forwarded. The two JSON halves are measured working and still refused, because
the profile does not declare either capability — declaring them is a deliberate
change to what this product offers, and this section is the evidence for it.

On the Messages face the same members behave differently again: `temperature`
and `top_k` answer 200 with nothing showing they were read, where the Chat face
refuses them. `stop_sequences` is honoured — a request naming `THREE` came back
cut at it — but the cut answer reports `stop_reason: "end_turn"` with a null
`stop_sequence` rather than naming the sequence.

## Errors

| Condition | Status | Body |
|---|---|---|
| invalid key | 401 | `invalid_authentication_error` |
| valid key, model not in the plan | 401 | `invalid_authentication_error` (Chat) / `authentication_error` (Messages) |
| unknown model name | **200**, answered, with the requested name echoed back |

The first two rows are why §8 of the adaptation plan refuses to attribute a bare
Kimi 401 to a bad credential, and the fixtures now prove it rather than citing
the documentation: one structured type covers both a dead key and a model the
plan does not include, and only the prose message tells them apart.

The third row is its own hazard. `halro-no-such-model` answered 200 with a
completion and echoed the requested identifier, so a response echo cannot confirm
which model served a request on this product, and a mistyped Deployment model
would be answered rather than refused. Halro's own catalogue and the enumeration
route are the only bound.

## Not covered

- `kimi-for-coding-highspeed` on either face.
- Any account other than the one plan measured, so nothing here establishes a
  mainland/international boundary. The surface stays `RegionNone`.
- Vision, video, files and the structured-output capability bits.
- Quota exhaustion and the 402 entitlement path, which the plan reaches through
  `FailureReasonEntitlementVerificationUnavailable` and which a healthy
  subscription cannot produce on demand.
