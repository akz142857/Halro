# Codex subscription credentials: measured against the real endpoint (2026-09-23)

[Issue #350](https://github.com/akz142857/Halro/issues/350)'s Gate 0 asked three
questions of OpenAI's terms. Two are answered by reading them; the third —
whether the upstream requires a client to present as Codex — is a question about
mechanism, and reading the terms cannot answer it. Sibling of
[the Anthropic record](./anthropic-claude-subscription-evidence.md), and written
because the first draft of the Gate 0 finding asserted an answer from secondary
reporting.

Driven with the operator's own Codex sign-in, one request, run by the operator
rather than by Halro's tooling. Nothing in the ordinary test suite makes this
call.

## What the client normally sends

From OpenAI's own source (`openai/codex`, read the same day), so the probe could
be honest about what it was and was not doing:

| | |
|---|---|
| Endpoint | `https://chatgpt.com/backend-api/codex` |
| Auth | `Authorization: Bearer <access token>` plus `ChatGPT-Account-ID` (`codex-rs/model-provider/src/bearer_auth_provider.rs`) |
| Identity label | `originator`, default `codex_cli_rs` — and **overridable by design**, via `CODEX_INTERNAL_ORIGINATOR_OVERRIDE` or a supplied value (`codex-rs/login/src/auth/default_client.rs:42-68`) |
| Attestation | `x-oai-attestation`, inserted only when `generate_attestation_header_for()` returns `Some`, so requests go out without it |

## The probe

`POST https://chatgpt.com/backend-api/codex/responses`, carrying the
subscription's Bearer token and account id, with a deliberately **honest**
identity — `User-Agent: Halro/0.8.5`, `originator: halro`, nothing borrowed from
Codex — and a model name that does not exist, so that an accepted request would
cost nothing to answer.

```
status=400
{"detail":"The 'halro-probe-not-a-real-model' model is not supported when
using Codex with a ChatGPT account."}
```

## What it establishes

**Auth and client identity were both accepted.** The request was not refused at
the door; it reached model validation and died there, on the model. A 401 or 403
would have meant the opposite. So:

- A Codex subscription credential **is served to a third-party client that says
  truthfully what it is.** No impersonation, no borrowed User-Agent, no
  `codex_cli_rs`, no attestation header.
- The reported pattern of third-party tools rewriting requests into the Codex CLI
  shape "so OpenAI's auth check passes" is not a requirement of the upstream, at
  least on this path. It may be how some tool was built; it is not the gate.
- Therefore **question (c) is not a blocker for #350.** An earlier draft of that
  finding said the only known way to make a subscription work was to bypass a
  protective measure. That was secondary reporting, and it is wrong.

The error sentence is itself worth keeping: the upstream distinguishes "using
Codex with a ChatGPT account" from the platform API, so it knows which product is
paying and says so.

## What it does not establish

It says nothing about permission. **(b) is untouched and still decides the
issue**: the ChatGPT Terms of Use say you may not "make your account available to
anyone else", and Halro is multi-tenant by construction. This record only removes
a *technical* argument that was standing beside the contractual one and did not
belong there.

It also repeats the Anthropic record's lesson in a second vendor: the boundary is
contractual and the upstream does not enforce it. Nothing outside Halro stops an
operator who points a self-declared OpenAI-compatible connection at the Codex
endpoint with a subscription token — it would work, and it would breach the
terms. Halro offers no such product and states why in the operator guide; it does
not blockade the generic profile, which exists to reach endpoints Halro does not
enumerate.

One account, one request, one moment, and OpenAI may change any of it without
notice. Re-measuring costs one request.
