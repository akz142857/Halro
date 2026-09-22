# Claude subscription credentials: measured against the real API (2026-09-22)

[Issue #351](https://github.com/akz142857/Halro/issues/351) closed on a reading of
Anthropic's terms. This is the other half: what the upstream actually *does* with
a Claude subscription credential, measured rather than inferred, because two
sentences in the operator guide and in `refuseClaudeSubscriptionToken` asserted a
401 that nothing here had ever seen.

Driven with the operator's own Claude Code sign-in — the access token out of this
machine's Claude Code credential store — against `https://api.anthropic.com`.
Two requests, one model, `max_tokens: 1`. Nothing in the ordinary test suite
makes these calls, and no fixture carries the token.

```
POST /v1/messages
{"model":"claude-haiku-4-5-20251001","max_tokens":1,
 "messages":[{"role":"user","content":"hi"}]}
```

## The two results

| Credential presented as | Status | Answer |
|---|---|---|
| `x-api-key: sk-ant-oat01-…` | **401** | `{"type":"authentication_error","message":"API key is invalid."}` |
| `Authorization: Bearer sk-ant-oat01-…` | **200** | a served completion, `input_tokens: 8`, `service_tier: "standard"` |

Both from plain curl, from outside Claude Code, with no Claude Code headers and
no client identity of any kind beyond curl's own.

## What each one means

**The 401 is what an operator would actually hit, and it names the wrong
problem.** `ProfileAnthropicMessages` sends the secret as `x-api-key` and strips
`Authorization` (`staticHeader("x-api-key", "", "Authorization")`,
`internal/app/provider_adapters.go:128`), so a subscription token saved into that
field produces exactly this row and cannot produce the other one. "API key is invalid." sends the reader to
check for a typo, rotate the key, or suspect Halro — none of which is the
problem, and none of which can fix it. That is the operator-experience argument
for refusing the paste at save time, and it is now measured rather than assumed.

**The 200 is the finding that matters, and it is the opposite of what secondary
reporting claims.** Reports of server-side enforcement — a refusal reading "this
credential is only authorized for use with Claude Code" — did not reproduce.
Presented as a Bearer token, this subscription credential is served by the
Messages API today, from an arbitrary client.

So the boundary is **contractual and not enforced on this path**. Three
consequences, and the first is the important one:

1. **Halro's refusal cannot be conditioned on the upstream saying no**, because
   the upstream says yes. A guard that waited for a 401 would never fire on the
   shape that works. `refuseClaudeSubscriptionToken` is therefore Halro's own
   fail-closed decision, taken on the terms, and it stays that way even if
   Anthropic never enforces.
2. **Nothing upstream protects the operator.** Without the save-time refusal, an
   operator who reached for Bearer would find it working and would be in breach
   of the Consumer Terms with no signal at all — which is the risk that lands on
   their Claude account, not on Halro's.
3. **"It works" is not a reopening condition.** #351 reopens on a first-party
   delegated-access contract from Anthropic, not on a measurement. This file is
   evidence about mechanism; it says nothing about permission, and it must not be
   read as a route.

## What this does not establish

One account, one token, one model, one moment. It is not a claim that the Bearer
path is stable, that other plans behave alike, or that enforcement will not
arrive — Anthropic's page says it "reserves the right to take measures to enforce
these restrictions and may do so without prior notice." Re-measuring is one
request; do that rather than trusting this table a year from now.

The token shapes read first-hand from the same credential store, and matched by
`refuseClaudeSubscriptionToken`: access `sk-ant-oat01-…`, refresh
`sk-ant-ort01-…`, 108 characters each.
