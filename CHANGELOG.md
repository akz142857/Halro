# Changelog

All notable user-visible changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and releases use
semantic versioning.

## [Unreleased]

### Added

- **TLS material is replaced on `SIGHUP` rather than on restart.** The signal
  reloads a closed list: the certificates and their keys, the Metrics
  certificate together with its client CA, `logging.level`, and the log file
  handle. Everything else still needs a restart, because a reload replaces
  material and never semantics — who is trusted as a proxy, which origin the
  console is, and where the data directory lives are not things that should
  change without one. Items apply independently and a failure keeps that item's
  previous value, so a bad replacement certificate leaves the old one serving
  instead of taking the instance down. In-flight connections are not
  interrupted. `SIGHUP` is never delivered on Windows; that platform still
  restarts to change a certificate.

- **Several certificates, selected by the name the client asks for.** Gateway
  and Admin can use different hostnames without one certificate having to cover
  both. Connections that arrive without SNI are answered by the first entry, and
  a name nothing declares is answered by it too, so the client's own name check
  reports which name was missing rather than an opaque handshake failure. Two
  entries claiming one name are refused at load.

- **The log file follows the rotate-then-signal convention.** `logrotate` renames
  the file and sends `SIGHUP`; Halro reopens the path. `logging.level` can be
  changed the same way, and every change is recorded in the log at a severity
  both the old and the new setting admit.

- **New observability for all of the above.**
  `halro_tls_certificate_expiry_seconds`, `halro_reload_total`, and
  `halro_reload_last_success_timestamp_seconds`, plus alert rules for a
  certificate expiring within 30 days, one that has expired, and a failing
  reload. `halro doctor` gains `tls_certificates` and `metrics_tls_certificate`,
  which run before the data lock is taken and therefore answer while the
  instance is serving. `/admin/api/v1/system/config` renders the values actually
  in force rather than re-reading the file, and reports per item when each was
  last applied.

### Changed

- **`tls.cert_file` and `tls.key_file` are replaced by a `tls.certificates`
  list.** The old keys are removed rather than deprecated.

### Operator impact

- **`config.yaml` must be edited; the data directory is untouched.** Unknown
  YAML fields are refused, so an unedited file stops the process at load with
  `field cert_file not found in type config.TLS`. One list entry carrying the two
  paths reproduces the previous behaviour exactly:

  ```yaml
  tls:
    enabled: true
    certificates:
      - cert_file: /etc/halro/tls/fullchain.pem
        key_file: /etc/halro/tls/privkey.pem
  ```

- **Rotating the Metrics client CA takes two signals, in order.** Concatenate the
  old and new CA and `SIGHUP`; move each scraper to its new client certificate;
  reduce the file to the new CA and `SIGHUP` again. Doing the last step first
  refuses every scraper that has not moved yet.

- **Changing a certificate's *path* is still a restart.** `SIGHUP` reloads the
  bytes behind the configured paths, not the paths themselves, so adding an
  entry to `tls.certificates` needs one.

## [0.2.0] - 2026-08-19

### Added

- A price version can bill by time of day. It carries an optional schedule of
  disjoint daily windows in the provider's own IANA zone; its four rate terms
  become the rates outside every window, so a price with no schedule bills
  exactly as it did before. The rung is chosen at reservation and never
  re-read, so an attempt spanning a boundary settles against the snapshot its
  reservation pinned. `POST /prices/preview` takes an `at` instant and returns
  every rung priced. An unresolvable zone bills the per-term maximum across the
  rungs rather than refusing or guessing an hour, and `halro doctor` reports it
  as `pricing_schedule_zone`. See ADR 0023.

- The cached span of a prompt is priced at its own rate. Price versions,
  proposals and snapshots carry `cached_input_micros_per_million`, and cost is
  `(input - cached) x input_rate + cached x cached_rate + output x output_rate
  + fixed_request_cost`. An attempt with a cache tier is no longer marked
  estimated, because the number is now exact. See ADR 0022.

- A Bedrock deployment chooses its own region. Endpoint templates live in the
  profile table with a region placeholder instead of being hardcoded in the
  browser, so an operator outside `us-east-1` no longer retypes the URL for
  every Bedrock connection.

- The Admin API serves the provider capability matrix, including the
  connection-level capability sets and the full dependency graph, so a caller
  that is not this console can produce a connection this console can.

- Both time zone fields in the console — the provider zone on a price schedule
  and the accounting zone in instance settings — offer a searchable list of
  IANA names with each zone's current UTC offset. It is a suggestion list over
  a free text input, not a closed menu: the browser's database and the server's
  are not the same build, so a typed name the server accepts is warned about
  rather than blocked, and an engine that cannot enumerate degrades to the
  plain field this replaces.

- Container images are multi-architecture (`linux/amd64` and `linux/arm64`) and
  are published to `ghcr.io` by the release workflow, for both `halro` and
  `halro-deadman`. Release creation is idempotent, so a rerun on the same tag
  no longer fails.

### Changed

- A provider connection is created from one flat capability set. The `bindings`
  array is gone from the request and the server decides which profile serves
  each capability: to the anchor profile whenever it can serve it, otherwise to
  the one peer that can. Several peers and no anchor is refused by name rather
  than resolved by table order, and a capability no profile can serve is
  refused rather than dropped. Token limits stay per binding, because they
  belong to the profile — a request can only narrow a profile that already
  declares one.

- DeepSeek is served by its own adaptation instead of the OpenAI-compatible
  passthrough it shared. Its cache split arrives as `prompt_cache_hit_tokens`
  and `prompt_cache_miss_tokens`, so every hit used to decode as zero and
  settle at the miss rate — thirty times the published price for that span. The
  accepted field list is narrowed to what the upstream takes (no `n`, `seed`,
  `parallel_tool_calls`, top-level `reasoning_effort` or `response_format`
  schema mode; the end-user reference is `user_id`), so a caller no longer gets
  `200` for a request that never happened as written. Reasoning maps to
  `thinking.reasoning_effort`, and an unasked request sends
  `thinking.type=disabled` rather than inheriting the upstream default.

- `max_completion_tokens` is carried to DeepSeek as `max_tokens` when thinking
  is off, where the two bound the same tokens, and still refused when thinking
  is on or the request already carries `max_tokens`. Declaring the loss
  unconditionally had taken DeepSeek out of the candidate set for every
  `/v1/responses` request that budgeted its output.

- Gemini, Bedrock Converse, Mantle Responses and Mantle Anthropic now declare in
  the published endpoint manifests that they route a portable Messages request
  away when it carries `output_config.effort` or `.format`. The routing
  behaviour is unchanged; the manifest was silent about it.

- A request the caller abandoned mid-flight is classified as canceled rather
  than as a connection failure. A cancel is never retryable and is not an
  availability failure, so a client that hangs up early no longer penalises a
  healthy upstream's circuit breaker or reads as an upstream network problem.
  An attempt already sent stays ambiguous, so its conservative accounting is
  unchanged.

- A project's `allowed_models` list is validated for shape on write. Duplicate,
  empty and whitespace-padded entries are refused, and the list is bounded —
  the Gateway scans it on every request, so an Admin write should not be able to
  make every request arbitrarily more expensive.

- Selecting the price version in force no longer re-audits the whole timeline on
  every call. The audit is unchanged and none of its fail-closed behaviour is
  weakened; its result is held per deployment and dropped by all four write
  paths. A 60-version timeline costs 243 ns and no allocations where it cost
  386 µs and 1,895 allocations, and a failed audit is deliberately not cached.

- Round-robin candidate resolution takes a single read-locked pass instead of
  resolving twice and serialising behind the write lock. Eight candidates cost
  4.4 µs / 41 allocations where they cost 9.4 µs / 82. Rotation order is
  unchanged.

### Fixed

- The console's form action bar no longer squeezes its own buttons: a long
  explanation beside them used to crush a two-character cancel onto two lines,
  and the cancel button now carries a resting border rather than growing one
  only on hover.

- The time-of-day price form is laid out for the width it has. The schedule
  toggle renders as a checkbox rather than a 44px box spanning the row, each
  window is a card instead of a seven-column table that could only scroll
  sideways, the price modal takes the 900px the deployment form takes, and the
  missing-timezone error is reported on the field it names.

- A project's alias card counts the strategy of enabled routes only. One
  retired row carrying the other strategy blanked the label, beside a count
  that had already ignored that row.

### Operator impact

- **Storage schema 30 upgrades in place; no re-initialisation.** It backfills
  the new cached-input rate on existing price versions with their own input
  rate — which is what they were charging — rather than defaulting it to zero,
  which would retroactively make cached tokens free. Proposals are re-digested
  so the backfill does not invalidate them.

- **Time-of-day pricing needs no migration.** Both new fields are absent when
  unset, so existing price versions decode as round-the-clock and existing
  snapshots re-encode byte-identically, digest included.

- **Admin provider create and update no longer accept `bindings`.** Send the
  flat capability set and let the server split it. The token limit inputs are
  gone from the connection form; a model's own limits are declared on the
  deployment.

- **The DeepSeek profile's catalog models changed.** `deepseek-chat` and
  `deepseek-reasoner` no longer appear in DeepSeek's documentation and are
  replaced by `deepseek-v4-flash` and `deepseek-v4-pro`, not kept alongside
  them. A deployment still naming a retired model reads as not covered by the
  catalog and asks its operator to declare or re-detect its capabilities; no
  data-directory re-initialisation is involved. The two names remain in the
  generic OpenAI-compatible profile's list, which is a different profile.

- **Bedrock model discovery still needs the control-plane host allowed**, and a
  Bedrock deployment now carries its own region. Existing connections keep the
  endpoint they were saved with.

## [0.1.0] - 2026-08-16

### Added

- A deployment now records which capabilities an operator switched off, apart
  from the ones nothing ever established. The two used to be the same absence,
  and they call for opposite treatment: a capability that was declined is
  reported as declined and is not offered again after every catalog change,
  while one that was never established still is.

- A capability snapshot now carries the evidence its source established, capped
  at what that source is allowed to claim. Storage refuses a snapshot whose
  evidence exceeds its source or describes something the snapshot does not
  establish.

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

- Giving one deployment several internal bindings is refused by name rather than
  incidentally, and the refusal says what to do instead: a deployment carries one
  model's own capabilities through one internal binding, and serving several
  capabilities under one public model is done with one deployment per binding and
  a route pointing at each. The design that proposed `operation_bindings` is
  withdrawn rather than deferred — composition already happens at the route
  layer, where the router selects a candidate per core operation.

- A model catalog entry claiming a capability its provider profile cannot carry
  is now refused at build time instead of being silently trimmed. A trimmed entry
  validated cleanly and left the console showing a model missing a capability
  somebody had written down, with nothing saying why.

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

- A Gateway Key no longer carries `last_used_at`. Nothing ever wrote it, so the
  field was absent from every response and the console showed every key as never
  used — an appearance indistinguishable from a key that genuinely had not been
  used. It is removed rather than implemented: recording it means a metadata
  write on the Gateway request path for a value nothing enforces, and the
  question it looked like it answered — what a key actually did — is answered by
  the audit trail and Usage data. No stored record ever contained it and it was
  never serialised, so nothing an API client could observe changes. The MFA
  authenticator's own `last_used_at` is a different field and is unaffected.

### Operator impact

- **Storage schemas 24 and 27 upgrade in place.** Schema 24 adds the durable
  model-capability detection buckets; schema 27 adds durable Admin audit
  intents so a committed mutation cannot lose its audit event across a crash.
  Neither change requires re-initializing a supported data directory.

- **Storage schemas 25 and 26 reset capability-detection cache state.** These
  are two reset migrations, not three. They affect only development instances
  that ran unpublished builds between schemas 24 and 26; published/schema ≤23
  instances had no detection cache to discard. Re-run capability detection if
  such an intermediate instance is upgraded.

- **Admin creates now require `Idempotency-Key`.** Provider, Deployment, Route,
  Project, and Gateway Key create requests without the header return `400
  idempotency_key_required`; a retry after a committed create returns `409
  <resource>_idempotency_replay` and the existing ID. See
  `docs/contracts/idempotency-contract.md` for the Admin/data-plane difference.

- **`security.allow_private_provider_endpoints` now takes effect.** Enabling it
  permits explicitly configured RFC1918/CGN Provider endpoints while loopback,
  link-local, cloud metadata, reserved ranges, redirects, environment proxies,
  and DNS-rebinding remain blocked. Leave it disabled unless private Provider
  routing is an intentional network boundary.

- **Re-initialising the data directory is required** for an instance holding any
  deployment created before capability snapshots existed. Storage schema 20
  refuses such a directory at start-up and names the deployment count. There is
  no backfill: the only value that could be written for an existing deployment
  is the provider ceiling, which is the guess the snapshot exists to replace,
  and writing it would make a guess indistinguishable from an established fact
  from then on. This was previously only mentioned in passing under the schema
  21 and 22 entries; it is its own reason to re-initialise.

- **Storage schema 23 upgrades in place; no re-initialisation.** It fills in the
  two fields above on existing deployments, computed by the same functions the
  write path uses, so a record brought forward is what re-saving it would
  produce. This is a backfill rather than a refusal — unlike schema 20, 21 and
  22 — because both values are derivable from fields already in the record,
  where those were not. Run `halro doctor` after starting once: before the first
  start it reports the schema mismatch, which is how every schema bump reads.

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

- Graceful shutdown uses a configurable budget that cannot be shorter than the
  Gateway route timeout. Provider attempts remaining when that budget expires
  are durably counted before their connections are forcibly closed.
- Release assets now include a separate binary-input SPDX SBOM and GitHub build
  provenance attestations. Halro 1.0.0 continues to ship container archives
  without claiming an official registry image.
- The optional signed model catalog remains intentionally inactive in 1.0.0
  release builds without production trust roots; bundled catalog resolution is
  the fail-closed default.
- The Light primary hue adjustment is explicitly deferred until after 1.0.0;
  the current AA-compliant color remains, while typography token use is gated.

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

[Unreleased]: https://github.com/akz142857/Halro/compare/v0.2.0...main
[0.2.0]: https://github.com/akz142857/Halro/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/akz142857/Halro/compare/v1.0.0-rc.1...v0.1.0
[1.0.0-rc.1]: https://github.com/akz142857/Halro/releases/tag/v1.0.0-rc.1
