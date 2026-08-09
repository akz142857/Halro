# Changelog

All notable user-visible changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and releases use
semantic versioning.

## [Unreleased]

### Added

- The built-in model catalog now covers OpenAI and DeepSeek models by exact
  identifier, in addition to the four Bedrock profiles that pin their model.
  A covered model arrives with its operations pre-checked and its context and
  output limits filled in; a model not covered still requires an explicit
  operator declaration and gets nothing by default. Entries carry `declared`
  evidence — the catalog is reviewed claims, not measurements.

- Bedrock model discovery. The Converse profile reads the regional Bedrock
  control plane, and the four profiles that accept exactly one model answer from
  that pin without any call. This is what makes a catalog-covered model
  selectable in the console rather than only reachable by typing its identifier.

### Changed

- A deployment may now exceed what the catalog establishes for a model with an
  explicit `mode=operator_declared`, and is then recorded with the operator as
  the capability source rather than the catalog. Without the word, the catalog
  still stands. This exists because a catalog entry that under-claims would
  otherwise be a wall no operator knowledge could get past.

- Creating a deployment no longer asks which internal capability interface to
  use. The console asks for a provider, then a model, and the server resolves
  the interface from the model and the selected capabilities. The interface
  remains visible and overridable under advanced details, where it is the only
  way to locate the internal adapter when diagnosing a deployment.

- The capabilities offered when creating a deployment are now what the model
  catalog establishes for that model, not what the interface can carry. The
  interface ceiling still applies to a model the catalog does not cover, because
  there the operator is the one making the claim.

- The console's model list is now the aggregate across every enabled interface
  of a provider rather than one interface's list. The server already returned
  the aggregate; the console was filtering it back down to a single interface.

- A profile that accepts exactly one model is no longer a candidate for a
  different one. That pin used to be checked after the interface had been
  chosen, so automatic selection could settle on an interface the very next
  check rejected, and the refusal named a profile the operator never picked.

- A route now names a deployment and nothing else. The shape that carried a
  provider and model directly is removed: such a route reached an upstream
  without the deployment's versioned price, health probe, capability snapshot
  or concurrency limit behind it, and none of those can be inferred from it.

- The `legacy` capability evidence tier is removed. It meant "this came from a
  record written before capability evidence was durable metadata", which is not
  evidence, and keeping it beside `declared` and `verified` gave the design a
  third tier that asserted nothing.

### Operator impact

- **Bedrock model discovery needs a second host allowed.** The control plane
  Halro derives from your runtime endpoint — `bedrock.<region>.amazonaws.com`
  for `bedrock-runtime.<region>.amazonaws.com` — must be in the Provider's
  allowed hosts, and the credential needs `bedrock:ListFoundationModels`. When
  either is missing the binding is reported degraded and the console falls back
  to entering the model ID by hand; nothing else about the deployment changes.
  PrivateLink and Agent Runtime endpoints have no control plane to derive and
  always fall back.

- **Existing deployments are unaffected by the new catalog entries.** A stored
  capability snapshot is never rewritten by a catalog that grew: a deployment
  whose model is now covered reports the new capabilities as available for
  review, and enabling one remains a deliberate act that makes its test stale.

- **Re-initialising the data directory is also required** for an instance
  holding any route without a `deployment_id`. Storage schema 22 refuses such a
  directory at start-up and names the route count. The synthesis that used to
  manufacture a deployment for these routes during migration is gone: what it
  produced no longer satisfies the capability snapshot every deployment has
  carried since schema 20, and it was invisible to that migration's own guard,
  so a schema-2 directory could reach the current schema carrying a deployment
  that fails validation.

- **Re-initialising the data directory is required** for an instance whose
  providers or deployments still carry `legacy` capability evidence — that is
  any instance migrated through schema 6 or earlier. Storage schema 21 refuses
  such a directory at start-up and names the record count. There is no rewrite:
  promoting the value to `declared` would assert a declaration nobody made, and
  demoting it to `unsupported` would turn capabilities off under a running
  deployment. Run `make reset CONFIRM=RESET` and recreate the topology, or stay
  on the previous build. A directory created at schema 20 or later is
  unaffected and upgrades in place.

## [1.0.0-rc.1] - 2026-08-07

First release candidate. Everything below is the initial v1 surface; the entries
kept under `Changed` and `Fixed` record where that surface departs from what a
pre-tag deployment ran, because those are the differences an early operator has
to act on.

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

[Unreleased]: https://github.com/akz142857/Halro/compare/v1.0.0-rc.1...main
[1.0.0-rc.1]: https://github.com/akz142857/Halro/releases/tag/v1.0.0-rc.1
