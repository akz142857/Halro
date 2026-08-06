# Changelog

All notable user-visible changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and releases use
semantic versioning.

## [Unreleased]

### Added

- Single-binary OpenAI-compatible LLM Gateway with embedded Admin console.
- Encrypted Provider credential vault and hash-only internal Gateway keys.
- Project budget, RPM, TPM, concurrency, model, CIDR, Token Guard, and
  redaction controls.
- Durable local accounting, Parquet analytics, audit integrity, backup/restore,
  Prometheus metrics, alerts, and operational diagnostics.
- OpenAI, Azure OpenAI, DeepSeek, and generic compatible GA adapters; Gemini
  and Bedrock Beta adapters.
- The Overview leads a never-used instance through the six-step configuration
  chain, with the developer workbench as the step that proves it end to end.
- The configuration file written on first run is annotated: every setting says
  what it decides, instead of arriving as a bare list of values.
- `make version` reports the release identity a build will carry, and `make
  build` now stamps it into the binary rather than reporting `dev/unknown`.
- Startup warns when the developer workbench is enabled on an Admin listener
  bound to a routable address, where network controls applied to the Gateway
  listener do not cover it.
- The Admin console honours the read-only role: Settings gained an Admin
  accounts pane for creating and removing accounts (both step-up gated), and
  every write control a read-only account cannot use is disabled rather than
  left to fail with a 403.
- `gateway.source_rate_limit` bounds how many requests one source address may
  start per minute, applied before authentication — the per-project limits
  cannot bound the cost of deciding which project a request belongs to. Default
  600 per minute; `doctor` reports it when a configuration leaves it disabled.

### Changed

- **Breaking (Admin API, pre-1.0).** Deleting a project, gateway key,
  credential, provider, deployment, route, Token Guard policy, redaction policy
  or alert now requires step-up re-authentication: the caller resupplies their
  own current password, plus a TOTP code when they have MFA enrolled, in a JSON
  body on the DELETE. A request without that body is refused with 401
  `recent_reauth_required`. The console asks for it in the same dialog that
  states the consequence. Any other API client of these endpoints must be
  updated. This lands before the first tag deliberately — afterwards it would
  need a migration rather than a default.
- Step-up re-authentication is bounded and audited. Five failures per account
  per minute, after which further attempts are refused with 429
  `reauth_rate_limited`; successes do not consume the budget, so an operator
  cleaning up several resources is not locked out for doing nothing wrong.
  Previously an authenticated session could try passwords without limit or
  record.
- Closing a form with unsaved input asks first. Escape, the backdrop and Cancel
  all go through the same question, and keeping the form keeps every field.
- The interface declares CJK font families and no longer renders labels below
  12px, which were unreadable at 1x in Chinese.
- One focus ring across the console, and the Light theme's secondary and
  tertiary text are no longer in the wrong order.
- Deleting a redaction or Token Guard policy is refused while any project still
  references it, including a project that is switched off.
- A request from a project that is over its daily budget is refused on the first
  candidate deployment instead of re-pricing every one of them.
- Disabling or deleting a deployment that an enabled route references now says
  why it is unavailable.

### Fixed

- An Admin account created before the two-level roles existed had no role
  stored, which its own instance then rejected: saving a preference failed
  validation, and every administrator-gated write was refused as read-only.
  A schema migration backfills those records as administrators — the capability
  they already had — while validation stays strict, so an empty role is still
  refused everywhere at runtime. Only the empty role is backfilled; any other
  unrecognised value is left to keep failing loudly rather than being
  normalised into the highest privilege.

- Pin the Dashboard trend to the actual seven-day window when only one data
  point exists.
- Separate Provider-reported Token usage from conservative estimates recorded
  for ambiguous failed attempts.
- A stream interrupted before the provider reports usage is billed for what was
  delivered, not for the project's output ceiling.
- Non-streaming responses are bounded by a write deadline, so a client that
  stops reading no longer holds a connection and its goroutine indefinitely.
- The whole `X-Forwarded-For` chain is read. A client that sent its own header
  line could previously hide the address a trusted proxy appended.
- Server-sent events treat a bare carriage return as a line terminator on both
  encode and decode, so payload content cannot inject an event boundary.
- Outbound requests refuse carrier-grade NAT, reserved ranges and the IPv6
  tunnel prefixes that re-encode an arbitrary IPv4 address; cloud metadata
  addresses are refused regardless of the private-address setting.
- Alert delivery is bounded in fan-out, and shutdown no longer waits out an
  in-flight retry backoff.
- The dead-man probe no longer holds its state lock across the network, which
  had delayed the heartbeat it was probing to produce.
- A file, batch or async creation interrupted before the provider was called can
  be retried after a restart, instead of holding its idempotency key for days.

[Unreleased]: https://github.com/akz142857/Heimdall/commits/main
