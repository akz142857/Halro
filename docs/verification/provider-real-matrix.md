# Real-account Provider matrix

The final release requires a passing real-account smoke for every GA Provider
profile on the exact RC commit. Unit, golden, fake-server, SDK compatibility,
and capability-contract tests remain mandatory but do not replace this gate.

The matrix runner covers OpenAI, Anthropic, Azure OpenAI, DeepSeek, and one
explicitly reviewed OpenAI-compatible endpoint. It verifies non-stream chat,
semantic SSE, embeddings wherever the profile declares embeddings, and the same
bounded fixed-protocol capability-detection plan used by the Admin control
plane.

Anthropic is the one GA profile with two execution modes, so its smoke proves
both: native Messages and its stream forwarded verbatim, the portable OpenAI
shape re-authored through the canonical model, `count_tokens`, and the model
catalog that a credential-only connection test falls back to. A pass on one
mode is not evidence for the other; they share an adapter and nothing else.

DeepSeek's smoke carries two extra assertions, because it is the one profile
whose southbound body differs from the adapter it shares. Both were adapted from
DeepSeek's published documentation against a fake upstream, and neither can be
established that way:

- `thinking` is the spelling of the reasoning switch. A request carrying it is
  sent at the `low` rung; a refusal is how a wrong spelling surfaces, since
  DeepSeek does not accept the top-level `reasoning_effort` the OpenAI wire has.
- a repeated prefix comes back with `prompt_cache_hit_tokens` non-zero and the
  two tiers summing to `prompt_tokens`. A hit reported as zero is settled at the
  miss rate, which DeepSeek prices at thirty times the hit rate, so this
  is an accounting assertion rather than a protocol one.

Both add billable calls to a DeepSeek run and run unconditionally on that
profile — an inconclusive DeepSeek cell is what the adaptation work was trying
to close.

`openai.media-resources.v1` has a smoke of its own,
`TestRealMediaResourcesSmoke`, outside this runner. It is separate because its
operations cost differently and leave different traces: an image is priced like
a generation, and files and batches put objects on the account. Each operation
is opted into by naming its model, so an operator buys only the evidence they
want, and the file upload is deleted unconditionally at the end. Run it with
`HALRO_SMOKE_PROFILE=openai_media`; a batch record survives in a terminal state
because OpenAI has no delete for batches.
Capability detection is enabled only in this opt-in child process, may incur
additional charges, and must verify `chat` for the configured model without
exceeding eight calls. Configure dedicated,
budget-limited credentials through environment variables using these prefixes:

- `HALRO_MATRIX_OPENAI_...`
- `HALRO_MATRIX_ANTHROPIC_...`
- `HALRO_MATRIX_AZURE_OPENAI_...`
- `HALRO_MATRIX_DEEPSEEK_...`
- `HALRO_MATRIX_OPENAI_COMPATIBLE_...`

Each prefix requires `BASE_URL`, `API_KEY`, and `MODEL`. OpenAI, Azure, and the
reviewed compatible endpoint also require `EMBEDDING_MODEL`; Azure additionally
requires `API_VERSION`. Anthropic requires only the three common values — the
profile declares no embeddings, and its extra surfaces take no configuration. Then run:

```bash
go run ./tests/provider-matrix \
  -commit '<exact-lowercase-40-character-RC-commit>' \
  -output provider-matrix.json
```

Credentials are transferred to only the selected child test through its
environment, removed from captured output, and never included in the evidence
file. The runner fails closed for missing profiles, failed requests, or per-
profile timeout. Archive the `0600` JSON evidence with RC artifacts. Review it
before sharing because third-party toolchains can change their diagnostics.

The safe evidence contains bounded call/supported counts and
`stable_negative=not_configured` when no reviewed, stable unsupported model is
available. It never contains the model ID, fixed probe input, generated output,
Provider error body, request ID, endpoint, or credential. A transient error is
never accepted as `unsupported`; the fake-server contract tests cover those
negative classifications even when a real account has no stable negative
model.

Gemini and Bedrock are Beta and therefore do not satisfy or block the GA matrix,
but each has a separate opt-in real smoke test under its adapter package. Run
those when the corresponding Beta is included in an RC deployment.

## DeepSeek: the two extra assertions passed on a real account (2026-08-20)

Run on `13d55ff` against `https://api.deepseek.com` with `deepseek-v4-flash`,
through `TestRealProviderSmoke` directly rather than through
`tests/provider-matrix`. **This is adaptation evidence, not GA matrix evidence**:
it carries no `-commit` binding and produced no archived evidence file, so the
release gate above is still open for this profile.

What it established, none of which a fake upstream could:

- `thinking` is the spelling. The request was accepted at the `low` rung and came
  back with `reasoning_content` present and 39 reasoning tokens billed. A wrong
  spelling would have surfaced as a refusal.
- `none` really turns it off: 0 reasoning tokens, asserted rather than logged. The
  documented default is thinking on, so a caller who declined reasoning and was
  billed for it anyway would have been silent.
- The cache counters are spelled as documented and the tiers sum:
  `prompt_tokens` 1208 = `prompt_cache_hit_tokens` 1152 + `prompt_cache_miss_tokens`
  56 on the repeated prefix. A hit read as zero settles at the miss rate, which
  DeepSeek prices at thirty times the hit rate.

Still not established, and outside what this smoke asks: whether DeepSeek's
`max_tokens` counts the thinking chain when thinking is on. The thinking probe
sends `max_tokens` 256 and only asserts the call succeeds; it never measures
whether the answer was truncated by the chain's share. Halro declares
`max_completion_tokens` in that case, which is the conservative reading — sending
the total as the answer budget would quietly shrink the caller's request — so
nothing depends on the answer today, but the declaration is a choice rather than
a measurement.

## Invocation-target resolution RC checks

For an RC that includes the cross-provider selection flow, archive a second
Admin control-plane checklist alongside the matrix evidence:

- OpenAI and DeepSeek list account-visible targets by name; a newly visible,
  uncatalogued ID remains `unknown` and never inherits capabilities by name or
  owner.
- Anthropic and Gemini retain only the reviewed structured metadata fields.
  Unknown fields are inert; Gemini generation methods establish operations only.
- Bedrock continues to filter inactive and non-invocable summaries, retains
  input/output modalities, and keeps region/ARN semantics exact.
- Azure works with only its data-plane credential through manual Deployment
  name plus explicit canonical-model mapping; absence of ARM identity is not a
  failure.
- The Admin selector renders one visible name column, keyboard selection, a
  read-only summary for one variant, an explicit radio choice for many, and a
  Provider-settings exit for zero variants.
- Saving a stale variant returns `resolution_changed`, loads the latest
  resolution, and keeps Save disabled until the operator confirms again.

These checks require real Provider accounts on the exact RC commit. Their
absence remains an external release gate, not a reason to substitute fixture
evidence or mark the account behavior as verified.

### Local fixture browser RC evidence (2026-08-10)

The repository-local browser pass used the production Admin bundle with fixture
Provider responses and verified:

- an unknown target exposes exactly the explicit verification and Advanced
  onboarding exits before any detection request is sent;
- the selector and result regions expose their combobox/listbox, option, status,
  fieldset/legend, and button/link semantics to the accessibility tree;
- the selector remains keyboard operable; and
- at a 390 × 844 viewport the deployment modal and selector produce no
  horizontal page overflow.

This is local fixture evidence for the Phase 3 Admin UX gate only. It does not
establish account-visible catalogs, permissions, model availability, billing,
or invocation behavior for any real Provider. The real-account checks above
remain pending on the exact RC commit and continue to block release where the
corresponding profile is in scope.

## Dynamic signed catalog local security evidence (2026-08-10)

The Phase 4 repository-local gate passed with:

```bash
go test ./...
go vet ./...
go test -race ./internal/modelcatalog ./internal/app \
  -run 'Test(Manager|SignedSnapshot|BundledSnapshot|ParseTrustRoots|ProductionTrustRoots|SignedCatalog|Invocation|CapabilitySnapshot|ResolutionMetric|RuntimeWrites)'
npm test --prefix web
npm run build --prefix web
```

The focused suite covers disabled-mode zero network, SafeTransport private/DNS
rejection, environment-proxy bypass, redirect refusal, compressed and decoded
limits, compression ratio, strict fields, tampered signatures, schema and
capability-dictionary bounds, expiry, revision pinning, rollback and sequence
reuse, exact revocation, old/new key overlap, mode-0600 atomic last-known-good,
degradation/recovery, and continued use of existing immutable Deployment
snapshots when the signed catalog is unavailable. The production Admin bundle
also passed its artifact secret scan.

This evidence does not claim that GitHub environment protection, two distinct
reviewers, a production KMS/HSM key, repository public-root variable, or a live
published catalog has been configured. Those are explicit production
activation gates in `docs/runbooks/model-catalog-publishing.md`; dynamic updates
must remain disabled until they are evidenced for the target repository and
release.

## AWS Bedrock Runtime: the connection probe was unpassable (2026-08-15)

A real account in `us-east-2` refused every Bedrock Runtime connection test with
**HTTP 404 and no `x-amzn-errortype`**. Every modelled Bedrock error names itself
in that header, so a bare 404 carrying only a request id is the service frontend
refusing a method its route does not have — the probe was a `HEAD` on
`/model/{id}/converse`, and Converse routes `POST`.

Nothing in the tree contradicted it: the Bedrock probe had no test of any kind
and no real-account evidence at any commit, and the `400`/`405` statuses it
accepted were a guess about how a POST-only route would answer a HEAD.

The probe now asks the operation with its own method and a body the operation
must reject (`{}`): a validation refusal proves the host resolved, the signature
verified, the model exists on the account and the credential may call the
operation, while nothing in an empty object can be inferred on or metered. `404`
is deliberately not accepted — a model the account cannot invoke has to fail the
test that gates enabling a deployment.

What is pinned against a fake server: the method, path, body and that the body is
covered by the signature; `400` reachable; `404` and `403` failing, the latter as
an authentication class.

The same account then answered the new form with **HTTP 403
`InvalidSignatureException`** — which is what made the next finding reachable at
all. The HEAD probe had never carried a body, and no other request from this
adapter had ever reached a real account, so the signer's defects had nothing to
report them.

## AWS Bedrock: the SigV4 signature was wrong for every signed request (2026-08-15)

Three defects in the hand-rolled signer, all invisible to every test that existed:

1. the canonical request omitted the **empty line** SigV4 requires between the
   canonical headers and the signed-header list, so AWS derived a different
   string to sign for *every* request;
2. the canonical URI used the once-encoded path. SigV4 canonicalizes the path
   for every service except S3 by **encoding it a second time**, and a Bedrock
   model id carries a colon (`anthropic.claude-sonnet-4-5-…-v1:0`), an
   inference-profile ARN several — so the two sides disagreed on essentially
   every real model;
3. the canonical host carried the scheme's **default port**. Halro normalizes
   every stored Provider endpoint to an explicit `:443`, so the adapter signed
   `host:443` where an AWS SDK signs `host` — and the SDK also rewrites the
   request's own `Host` header to the authority it signed, which this signer did
   not. This is the one that survived the first two fixes: the account went on
   answering InvalidSignatureException until the port came off both the
   canonical string and the header.

`content-length` is now signed whenever a request carries a body, matching what
every AWS SDK signer does. Omitting it was legal — AWS verifies against the
SignedHeaders list — but a set that matches the SDK's is a set that can be
compared against it.

The tests that existed asserted the *shape* of the Authorization header: its
prefix, that the secret never appears inside it. A wrong signature satisfies all
of that. `signer_vector_test.go` now checks the produced header against
`aws-sdk-go-v2`'s own SigV4 signer — an independent implementation, already a
dependency for KMS, used as a test oracle and never in the request path — over a
GET with no body, a POST with a JSON body, a POST to a colon-bearing
inference-profile path, and a POST to an endpoint carrying the `:443` that
Halro's own normalization produces. It also asserts that the host sent is the
host signed. Backing out any of the three defects fails it.

This repaired the request path, not only the probe: Chat, Converse streaming,
Titan invoke, rerank and async-invoke all signed through the same function and
had all never reached a real account.

## AWS Bedrock Mantle: measured against a real account (2026-08-21)

The Mantle profiles had **no real-account evidence at any commit** until
2026-08-21. Every path, capability and credential claim came from AWS
documentation and this repository's own fake servers. On 2026-08-21 the surface
was probed directly with `curl` against a real Bedrock Mantle endpoint, over all
50 models the account lists.

Read the scope precisely. What follows is evidence about **the service**: which
routes exist, which models answer on them, and which request members are
accepted. It is **not** evidence that Halro's own request path reaches that
service end to end — the matrix runner still has no Mantle coverage, and
`tests/provider-matrix` has still never been run against this endpoint. The
section below on running the smoke remains outstanding work.

### Three routes on one host, and the model picks one

Mantle serves `/v1`, `/openai/v1` and `/anthropic/v1` from a single origin. The
first two speak the same OpenAI wire shape, so the wire shape cannot select
between them, and a model reaches exactly one:

| route | models | refusal when addressed wrongly |
| --- | --- | --- |
| `/v1` | 38 | ``model `x` isn't supported on this route`` |
| `/openai/v1` | 11 | ``model `x` isn't supported on this route`` |
| `/anthropic/v1` | 1 | `The model 'x' does not support the '/v1/responses' API` |

The two refusal wordings are the service's, verbatim, and they come from
different validators. Neither is silent: a request sent to the wrong route is
refused, never served by a fallback.

**The route cannot be derived from the model identifier.** This is the finding
that shapes the design, and it has four independent counterexamples:

| model | identifier suggests | actually answers on |
| --- | --- | --- |
| `openai.gpt-oss-20b`, `openai.gpt-oss-120b` | `/openai/v1` | `/v1` |
| `openai.gpt-5.6-sol` and every other `openai.gpt-5.x` | `/openai/v1` | `/openai/v1` |
| `google.gemma-3-*` (4 models) | — | `/v1` |
| `google.gemma-4-*` (3 models) | same vendor as gemma-3 | `/openai/v1` |
| `xai.grok-4.3` | nothing puts it with the OpenAI models | `/openai/v1` |

The model list does not carry the route either: `GET /v1/models/{id}` answers
with `id`, `object`, `status`, `owned_by`, `created` and `data_retention`, and
nothing about which route serves it. So the route is fixed by the Provider
Profile, which is why there are five Mantle profiles rather than three.

#### The catalogue is account-wide, not route-scoped

Measured 2026-08-25 against the same account, us-east-2:

| request | status |
| --- | --- |
| `GET /v1/models` | 200 |
| `GET /openai/v1/models` | 404 |
| `GET /v1/models/openai.gpt-5.5` | 200 |
| `GET /openai/v1/models/openai.gpt-5.5` | 404 |

The last row is the sharp one: `openai.gpt-5.5` is served on `/openai/v1`, and
that route still has no entry for it to read. Discovery exists at `/v1` only.

This says what Halro's discovery cannot do. A profile pinned to `/openai/v1`
enumerates the account's whole model list — including the 38 models that route
will refuse — because no route-scoped list exists to enumerate instead, and the
route cannot be derived from the identifier (above). Halro addresses `/v1` for
the model list and the connection test whatever route the profile pins for
inference. Two consequences worth stating rather than discovering:

- A connection test that passes proves the model exists on the account and the
  credential reaches it. It does not prove this route serves that model.
- Nothing here is silent. A model addressed on the wrong route is refused with
  ``model `x` isn't supported on this route``, never served by a fallback.

Making discovery follow the pinned prefix — an earlier attempt to keep
discovery and inference on one URL — is what the 404 above rules out: it broke
the model list and the connection test on both `/openai/v1` profiles while
inference on that route kept working.

### What each model answers, measured

Streaming was reachable on **49 of 49** chat models, `text/event-stream` in every
case. Stateless Responses was reachable on **13 of 49**.

The sharpest capability counterexample sits inside one vendor's own family:
`openai.gpt-oss-20b` and `-120b` serve Responses, while `openai.gpt-oss-safeguard-20b`
and `-120b` — same vendor, same route, adjacent names — do not.

| model | route | Responses |
| --- | --- | --- |
| `anthropic.claude-haiku-4-5` | `/anthropic/v1` | no |
| `google.gemma-4-26b-a4b` | `/openai/v1` | yes |
| `google.gemma-4-31b` | `/openai/v1` | yes |
| `google.gemma-4-e2b` | `/openai/v1` | yes |
| `openai.gpt-5.4` | `/openai/v1` | yes |
| `openai.gpt-5.4-2026-03-05` | `/openai/v1` | yes |
| `openai.gpt-5.5` | `/openai/v1` | yes |
| `openai.gpt-5.5-2026-04-23` | `/openai/v1` | yes |
| `openai.gpt-5.6-luna` | `/openai/v1` | yes |
| `openai.gpt-5.6-sol` | `/openai/v1` | yes |
| `openai.gpt-5.6-terra` | `/openai/v1` | yes |
| `xai.grok-4.3` | `/openai/v1` | yes |
| `deepseek.v3.1` | `/v1` | no |
| `deepseek.v3.2` | `/v1` | no |
| `google.gemma-3-12b-it` | `/v1` | no |
| `google.gemma-3-27b-it` | `/v1` | no |
| `google.gemma-3-4b-it` | `/v1` | no |
| `minimax.minimax-m2` | `/v1` | no |
| `minimax.minimax-m2.1` | `/v1` | no |
| `minimax.minimax-m2.5` | `/v1` | no |
| `mistral.devstral-2-123b` | `/v1` | no |
| `mistral.magistral-small-2509` | `/v1` | no |
| `mistral.ministral-3-14b-instruct` | `/v1` | no |
| `mistral.ministral-3-3b-instruct` | `/v1` | no |
| `mistral.ministral-3-8b-instruct` | `/v1` | no |
| `mistral.mistral-large-3-675b-instruct` | `/v1` | no |
| `mistral.voxtral-mini-3b-2507` | `/v1` | no |
| `mistral.voxtral-small-24b-2507` | `/v1` | no |
| `moonshotai.kimi-k2-thinking` | `/v1` | no |
| `moonshotai.kimi-k2.5` | `/v1` | no |
| `nvidia.nemotron-nano-12b-v2` | `/v1` | no |
| `nvidia.nemotron-nano-3-30b` | `/v1` | no |
| `nvidia.nemotron-nano-9b-v2` | `/v1` | no |
| `nvidia.nemotron-super-3-120b` | `/v1` | no |
| `openai.gpt-oss-120b` | `/v1` | yes |
| `openai.gpt-oss-20b` | `/v1` | yes |
| `openai.gpt-oss-safeguard-120b` | `/v1` | no |
| `openai.gpt-oss-safeguard-20b` | `/v1` | no |
| `qwen.qwen3-235b-a22b-2507` | `/v1` | no |
| `qwen.qwen3-32b` | `/v1` | no |
| `qwen.qwen3-coder-30b-a3b-instruct` | `/v1` | no |
| `qwen.qwen3-coder-480b-a35b-instruct` | `/v1` | no |
| `qwen.qwen3-coder-next` | `/v1` | no |
| `qwen.qwen3-next-80b-a3b-instruct` | `/v1` | no |
| `qwen.qwen3-vl-235b-a22b-instruct` | `/v1` | no |
| `writer.palmyra-vision-7b` | `/v1` | no |
| `zai.glm-4.6` | `/v1` | no |
| `zai.glm-4.7` | `/v1` | no |
| `zai.glm-4.7-flash` | `/v1` | no |
| `zai.glm-5` | `/v1` | no |

The account listed 50 identifiers on 2026-08-25, one more than the 49 chat
models measured above plus nothing new: the extra is `zai.glm-4.6`, and the
three the console's model page does not show a card for are `zai.glm-4.6`,
`openai.gpt-5.4-2026-03-05` and `openai.gpt-5.5-2026-04-23`. The two dated
snapshots resolve to their base model's card; `zai.glm-4.6` resolves to none,
so no context window is recorded for it anywhere.

This table is what seeds `bedrockMantleModels()` in
`internal/modelcatalog/builtin.go`: the route decides which chat profile a model
appears under, and only a measured `yes` in the Responses column puts it under a
Responses profile as well. The windows and maximum output beside them come from
the account's own model list, read 2026-08-25. Nothing else about these models is
claimed there — tools, JSON mode, vision, developer role and reasoning are all
inside the Mantle ceiling and none was exercised per model, so they stay for
capability detection to establish against a real account.

### Request members

- `max_completion_tokens` was accepted by **all 49** chat models on their own
  route. `max_tokens` was not: `openai.gpt-5.6-*`, `google.gemma-4-*` and
  `xai.grok-4.3` refuse it with `Unsupported parameter: 'max_tokens' is not
  supported with this model`. The two are therefore not interchangeable, and
  `max_completion_tokens` is the one to send everywhere.
- `store:false` was sent on `/openai/v1/responses` and echoed back as
  `"store":false`. The pin holds. Left unset, the service defaults to
  `"store":true`, so sending it explicitly is doing real work.

### Corrections to what was previously pinned

- **The Anthropic Messages route accepts either credential header.** Both a
  bearer `Authorization` and an `x-api-key` returned 200 on
  `/anthropic/v1/messages`. The previous text described `x-api-key` as what the
  route requires; it is what Halro sends, and clearing the other headers remains
  the safer behaviour, but the exclusivity was never true of the service.
- **Model metadata is not a cheap capability oracle.** The previous cost-shape
  note said the OpenAI-shaped profiles probe by reading one model's metadata.
  That read succeeds, but it carries neither the route nor any capability, so it
  cannot answer the question a probe is asking. Establishing what a Mantle model
  supports costs a real inference call.

### Not yet established

- `data_retention` on the model object offers `allowed_modes`
  `["default", "provider_data_share"]` with `mode: "default"` and
  `source: "model_default"`. What `provider_data_share` means for data
  handling, and whether Halro should ever select it, is unreviewed.
- Mantle returns response members with no OpenAI counterpart — `billing.payer`,
  `output[].phase`, `reasoning.context`, `prompt_cache_retention`,
  `service_tier` — and `usage` carries `cache_write_tokens`, `cached_tokens`
  and `reasoning_tokens` details. Halro re-authors responses through the
  canonical model, so none of these reach a client today. Whether the usage
  details should reach accounting is a separate question and is **not** answered
  by this run.
- No streaming Responses call, no tool call, and no multi-turn conversation was
  probed. Only single-turn chat and single-turn Responses were.

### Running the Mantle smoke when it is authorised

The harness exists; no run has happened. Probing the service with `curl`, as the
section above records, is not the same as exercising Halro's own request path,
and this remains the way to do the latter. It is opt-in twice over: the matrix
runner ignores Beta profiles unless asked, and the smoke itself skips unless
every variable is set.

```bash
export HALRO_MATRIX_BEDROCK_MANTLE_BASE_URL="https://bedrock-mantle.<region>.api.aws"
export HALRO_MATRIX_BEDROCK_MANTLE_API_KEY="<dedicated, budget-limited Bedrock API key>"
export HALRO_MATRIX_BEDROCK_MANTLE_MODEL="<exact upstream model id, on the route the profile names>"
export HALRO_MATRIX_BEDROCK_MANTLE_MANTLE_PROFILE="chat"   # or openai-chat, responses, openai-responses, messages
# optional; omit to exercise the account default project
export HALRO_MATRIX_BEDROCK_MANTLE_BEDROCK_PROJECT_ID="proj_..."

go run ./tests/provider-matrix \
  -commit '<exact-lowercase-40-character-commit>' \
  -include-beta \
  -output provider-matrix.json
```

Each run exercises one wire profile with a non-stream and a streaming call, and
requires both to report usage — a run Halro could not account for is not
evidence that Halro can serve the profile. Three wire profiles is three runs.

`-include-beta` never moves the GA release gate: Beta results carry
`tier: "beta"` and `passed` counts GA results only. Without the flag the Beta
rows are still emitted with `status: "not_run"`, so silence cannot be read as
coverage.

### What a Mantle evidence row may contain

Each row records the cell it covers — `region`, `wire_profile`,
`authentication`, `project_mode` — plus a `target_digest`. The digest is
`sha256(region, wire profile, authentication, project mode, exact model)`: it
lets a later reader confirm two runs used the same target, or match a claimed
target against a custody record they hold locally, without this shared file
naming an account's model entitlements. The exact model, the key, the account
ID, the project ID, request and response bodies, and provider request IDs never
enter it.

A run proves one cell. It says nothing about the other two wire profiles,
another model, another region, or the other project mode, and nothing about
capabilities the run did not exercise. A transient failure, a rate limit or a
5xx must never be recorded as `unsupported`. Until at least one row exists, the
release notes carry Mantle as a known limitation and any statement that Mantle
"works" is a statement about fixtures.
