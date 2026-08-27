# Changelog

All notable user-visible changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and releases use
semantic versioning.

## [Unreleased]

### Changed

- **Re-authentication is asked once per sitting, not once per action.** Deleting
  a Route, replacing a Provider credential, or editing a protection down to
  nothing still requires the account password (and a TOTP code where the account
  has an authenticator) — a stolen session alone must not be able to do those.
  What changed is that one proof now counts for
  `admin.reauth_elevation_window` (default 10m) instead of for exactly one
  request, so clearing out six Routes costs one prompt rather than six. The
  window is bound to the single session that proved itself, ends when that
  session signs out or its password changes, and `0s` restores the
  ask-every-time behaviour.

  Two things stay outside it, deliberately: the admin-account endpoints
  (changing a password, removing an authenticator, disabling MFA, creating or
  deleting an administrator) and minting a Gateway Key. Those are how a stolen
  session would turn itself into standing access, so they are asked every time.

  **Config change:** the key that used to be
  `admin.model_capability_detection.elevation_window` is now
  `admin.reauth_elevation_window` and governs every step-up, capability
  detection included. Config decoding is strict, so an instance still carrying
  the old key **refuses to start** — `field elevation_window not found in type
  config.ModelCapabilityDetection`, naming the line. Move the value up to
  `admin:` before upgrading.

  In the console the credential fields no longer appear until the server asks
  for them: the confirmation still states the consequence, and the fields open
  only when the window has closed.

- **`json_mode` became two capabilities: `json_object` and `structured_outputs`.**
  One switch could not describe any provider honestly — Anthropic enforces a
  schema and has no schema-less mode, DeepSeek has the schema-less mode and no
  schema, and on OpenAI the split is per model. A deployment that ticked
  `json_mode` on the strength of the half it had was routed the other half and
  refused upstream, after the budget reservation.

  **Upgrading turns both halves off for every existing deployment, and nothing
  says so at the time.** The old bit cannot say which half a target actually
  had, and off refuses a request where on forwards a doomed one — so migration
  32 clears both, marks the evidence unsupported, and empties the detection
  buckets. Until you re-tick the capabilities a deployment really has, **every
  request carrying `response_format: json_object` or `json_schema` is refused
  with 400 `unsupported_feature`.** Plan for that window.

  What you will and will not see: the startup log does not mention the
  migration; `halro doctor` reports `capability_drift` as "catalog capabilities
  available for review", and only for models the built-in catalogue covers — a
  self-declared model gets no signal at all. Re-running capability detection
  restores verified evidence at the cost of billable upstream probes.

- **The unary generation path is entered with a semantic request.** Every facade
  decodes its own wire form and calls one hot path; Responses no longer writes
  itself as a Chat Completions request first. Redaction and the token estimate
  moved onto the semantic request with it, so one traversal covers every
  endpoint.

- **An answer that cannot be delivered is no longer recorded as a success.**
  Rendering the provider's answer into the caller's wire form, and validating
  what redaction did to it, now happen before the request is finalized rather
  than after. Previously a request whose answer could not be rendered — or whose
  redaction policy rewrote it into something invalid — was settled and finalized
  as a success while the caller received a 502. The upstream call really
  happened and is still charged for; what changed is that the record and the
  answer now agree, and a policy that breaks its own output is reported as
  `redaction_policy_error` naming the policy, rather than as a provider error.

- **A replacement template may no longer name a capture group its pattern does
  not have.** `$1` on a rule with no capture groups expands to nothing, which
  deleted what it matched instead of masking it. Templates whose references are
  valid — including a bare `$1` that keeps only the captured part — are
  unchanged.

### Added

- **`openai.responses.v1`, a profile that addresses `/v1/responses` directly.**
  It carries OpenAI's provider-executed `web_search`, which is off by default
  and gated on the `provider_executed_tools` capability, because running it
  means the upstream originates network calls that never pass through Halro's
  own egress controls. `code_interpreter` and `file_search` are refused: they
  are provider-side state a single-process gateway has nowhere to hold. Searches
  come back as `web_search_call` items and `url_citation` annotations, and a
  profile that cannot carry either refuses the result rather than returning an
  answer with its sources removed.

- **A `json_schema` detection probe**, which sends a strict schema and checks the
  answer against it — a model told to return JSON returns parseable JSON whether
  or not it can enforce a schema. The probe budget went from 8 to 9 so no
  capability stopped being verified as a side effect.

### Fixed

- **Capability detection probes the surface the profile actually calls.**
  Detection spoke Chat and reached every adapter through the legacy bridge, so a
  detection on `openai.responses.v1` measured six capabilities against
  `/v1/chat/completions` and stored them as verified evidence for a surface the
  profile never calls. A key whose permissions covered only one of the two then
  passed detection green and failed the first real request, after the budget was
  reserved.

- **A model the catalogue already covers keeps its token limits when verified.**
  Verification previously recommended a context and output bound of zero.

- **Identification probes get the whole budget they can use.** When several
  interfaces are candidates, the pass that decides which one a model answers on
  runs only the probes that depend on nothing — but it divided the remaining
  time by the length of the whole plan, leaving each root probe a ninth of the
  budget on a nine-probe plan. A model that needs longer than that slice was
  reported as a timeout, and the console showed it as "temporarily unavailable".

- **Redaction covers the provider's own tool call and the sources beside it.**
  The search query the model wrote, and every citation URL and title the upstream
  returned, previously reached the caller with neither the Project policy nor the
  mandatory baseline having run, while every other string in the same message was
  rewritten — so the same secret could come back masked in the answer and verbatim
  in `action.query`. Citation offsets now collapse to zero length when the text
  they point into changed, rather than reporting a span that covers different
  words. `detail` on an image part and `status` on a provider tool call are
  covered by the mandatory baseline for the same reason.

## [0.3.0] - 2026-08-24

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

- **Each Bedrock Mantle route is its own Provider Profile.** Mantle serves `/v1`,
  `/openai/v1` and `/anthropic/v1` from one origin, and the first two speak the
  same OpenAI wire shape over disjoint model sets — so the wire shape cannot
  select between them and a request sent to the wrong one is refused upstream.
  Measured across the 50 models the region serves, 38 answer on `/v1` and 11 on
  `/openai/v1`, and the split does not follow the model identifier. The route is
  therefore fixed by the profile, and the console names the route rather than
  the wire shape when you pick one. A profile can now fix its operation path
  prefix; it is empty for every other provider, which keeps their URLs exactly
  as they were.

- **Fetching an image is a capability.** A Deployment states whether its
  interface may retrieve an image the request names by address, the Gateway
  refuses by that capability instead of failing downstream, and the developer
  workbench can send an image — a remote URL or a local file turned into a data
  URL in the page — without hand-writing the multimodal body. The request
  summary shows the body size against the instance limit, so an oversized image
  is refused before it costs a round trip.

- **Capability detection asks for step-up once per sitting.** A password and a
  TOTP code were required on every run, which an operator configuring six
  Deployments typed six times during first-time setup. One proof now covers
  `admin.model_capability_detection.elevation_window` (default 10m). The guard
  is unchanged in what it protects: the grant is bound to the session and its
  generation, never to the account, so a second session inherits nothing and
  changing a password invalidates it. An explicit `0s` asks every time.

- **`halro_route_capability_refusals_total`.** A route that cannot serve what was
  asked was counted nowhere, and it is the one refusal that reports a
  configuration answer rather than a pressure reading: the route is up, the key
  is good, and no deployment behind the public model declares what the request
  named. It now appears in the dashboard's rejection composition alongside every
  other policy outcome.

### Changed

- **`tls.cert_file` and `tls.key_file` are replaced by a `tls.certificates`
  list.** The old keys are removed rather than deprecated.

- **`gateway.stream_idle_timeout` is removed.** It was declared, defaulted,
  validated as positive, and documented as the inactivity period after which a
  streaming response is terminated — and nothing read it. No such timeout was
  ever enforced. What does bound a stream is `gateway.downstream_write_timeout`,
  re-armed on every emitted event, so a client that stops reading is cut off,
  and `gateway.stream_max_duration` with `gateway.route_total_timeout` for the
  stream and the request as wholes. An upstream that sends headers and then goes
  quiet is bounded only by those totals — it was never bounded by the removed
  key either. A setting that quietly does nothing is worse than an absent one,
  so it is gone rather than kept with a note.

- **A post-dispatch 5xx from OpenAI or Anthropic is ambiguous.** 500, 502 and 504
  were retryable and non-ambiguous, so the attempt was refunded in full *and*
  re-sent to the same and fallback targets while the origin may still have been
  generating — duplicating a completion the operator pays for while the ledger
  recorded nothing for each try. They are now neither retried nor settled as
  free, which is what the correctness contract says a dispatched request with no
  authoritative result is, and what the Bedrock and Gemini profiles already did.
  503 and Anthropic's 529 are unchanged: both are stated refusals to take the
  request on, so a fallback can serve them and the attempt owes nothing.

- **Outbound redaction refusing a native response answers 422.** Native
  `/v1/messages` and `count_tokens` returned a generic 502 `provider_error` for a
  policy decision, which read as a provider outage and invited retries of a
  request that must not be retried. They now return 422
  `sensitive_output_detected`, as the portable and native streaming paths already
  did.

### Fixed

- **An interrupted Replay could make the next Append overwrite durable ledger
  frames.** `writeBatch` trusted the OS file cursor to sit at the committed tail,
  but a Replay whose visit callback returns early — exactly what usage catch-up
  produces when its budget runs out — leaves the cursor mid-file. The next batch
  was written inside already-durable frames, with the write and the fsync both
  reporting success; the damage surfaced only at the next restart, when the chain
  scan refused the file. The writer now seeks to the offset the log itself
  tracks. Live-reproduced, and fixed before this release.

- **A revoked Gateway Key could be resurrected by a concurrent snapshot
  refresh.** Two refreshes run in production — the background activation-recovery
  loop and every admin key mutation — and the result was installed with a bare
  atomic store, so a read that started before a revocation committed and finished
  after it could land last. The operator watched the revocation succeed and the
  key kept authenticating. Refreshes are now serialized from the store read
  through the install; authentication itself stays lock-free.

- **Seven settlement and redaction defects found by a pre-release audit.** A
  served native Messages response refused by outbound redaction settled at zero
  and released its reservation, so a caller whose prompts elicit matching output
  could spend the operator's upstream budget with none of it reaching the ledger.
  An aborted tool-call stream settled at one output token, because the delivered
  byte count saw only assistant text and not tool arguments or reasoning. The
  Phase 2 endpoints — Moderations, Images, Speech, Transcription, Rerank —
  committed the full estimate whatever the provider returned, billing a refused
  dial like a served request. A JSON-escaped secret in streamed tool arguments
  passed every mandatory pattern and was reconstituted by the client's decoder,
  while the same response was redacted when requested non-streaming. Stream
  redaction delivered the tail of a message ahead of its head whenever the
  terminal chunk also carried text. And a price version with a fixed request fee
  made every deterministic failure unsettleable, stranding the reservation until
  a restart that then charged the full prepared estimate.

- **A definitively unsent stream no longer pays the fixed request fee.** A
  streaming attempt that failed before any byte reached the provider settled with
  zero tokens but still fell through to the fixed per-request fee, overcharging
  on every connect failure.

- **A fatal pre-provider error keeps its own status on the native path.** A Token
  Guard block or an accounting failure was re-mapped into a generic 502
  `provider_error`, so clients retried requests that must not be retried and
  operators debugged a provider that was never unhealthy.

- **`count_tokens` runs on a priced deployment.** It prepares no tokens, which
  derived a zero reservation the ledger refuses for a metered lease, and a zero
  settlement the frozen snapshot refuses under a fixed request fee — a 503 before
  the provider on the first shape and a stranded lease on the second.

- **A capability probe gets the whole remaining budget.** The per-probe timeout
  divided what was left by every probe the plan listed, which gave the chat probe
  that all the others wait on the smallest share — a seventh of 90s on the Mantle
  plan. A frontier reasoning model cannot answer one completion in 12.9s, so a
  model that works was reported as a timeout on every capability.

- **The console says why a connection test or a capability detection failed**,
  not only that it did; the deployment form draws a capability the connection has
  not enabled yet, together with the step that unblocks it; and the workbench
  response panel says it is waiting rather than reporting an empty body. Admin
  pages are fetched when their path is opened rather than all eleven at load.

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

- **Delete `gateway.stream_idle_timeout` from `config.yaml`** for the same
  reason — unknown fields are refused, so leaving it in stops the process.
  Nothing it claimed to do is lost, because it never did it, and no other
  setting needs adjusting to compensate.

- **Rotating the Metrics client CA takes two signals, in order.** Concatenate the
  old and new CA and `SIGHUP`; move each scraper to its new client certificate;
  reduce the file to the new CA and `SIGHUP` again. Doing the last step first
  refuses every scraper that has not moved yet.

- **Changing a certificate's *path* is still a restart.** `SIGHUP` reloads the
  bytes behind the configured paths, not the paths themselves, so adding an
  entry to `tls.certificates` needs one.

- **Storage schema 31 upgrades in place on the first start; no
  re-initialisation.** Verified by opening a data directory a v0.2.0 binary
  created. `halro doctor` does not migrate, though — run against a v0.2.0
  directory before that first start it reports `metadata: fail — metadata schema
  version 30 does not match required version 31`, which is the strict check
  doing its job rather than damage. Start once, then run `doctor`.

- **Fetching an image is off on every existing connection.** Migration 31 does
  not turn a new capability on for material it never verified. The Gateway names
  `fetched_image` in the refusal; enable it on the connection, then on the
  deployment.

- **Fallback no longer covers OpenAI or Anthropic 500, 502 and 504.** If your
  deployment relied on a standby target absorbing those, the traffic now fails to
  the caller instead, and the attempt is charged conservatively rather than
  refunded. 503 and 529 still fail over. This is the deliberate trade: retrying a
  request the upstream may already be billing duplicates the charge.

- **A price version with `fixed_request_usd` now charges it on `count_tokens`,**
  which is a request to the deployment like any other. Per-token prices still
  charge nothing for that endpoint.

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

[Unreleased]: https://github.com/akz142857/Halro/compare/v0.3.0...main
[0.3.0]: https://github.com/akz142857/Halro/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/akz142857/Halro/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/akz142857/Halro/compare/v1.0.0-rc.1...v0.1.0
[1.0.0-rc.1]: https://github.com/akz142857/Halro/releases/tag/v1.0.0-rc.1
