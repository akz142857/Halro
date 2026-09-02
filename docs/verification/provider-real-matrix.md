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

Gemini, Bedrock and MiniMax are Beta and therefore do not satisfy or block the
GA matrix, but each has a separate opt-in real smoke test under its adapter
package. Run those when the corresponding Beta is included in an RC deployment.

## MiniMax: measured on an international account (2026-08-31)

Two smokes, wired into the runner as the Beta rows `minimax` and
`minimax_anthropic`. They share the `HALRO_MATRIX_MINIMAX_` prefix because one
account serves all three wire shapes; they are separate rows because the
OpenAI-shaped two and the Anthropic one live in different adapter packages.

```bash
go run ./tests/provider-matrix -include-beta \
  -commit '<exact-lowercase-40-character-commit>' -output provider-matrix.json
```

`BASE_URL`, `API_KEY` and `MODEL` are required; `M2_MODEL` is optional and
defaults to `MiniMax-M2.7`. **This is adaptation evidence, not GA matrix
evidence**: MiniMax is Beta, so it never gates a release, and the runs below
carried no `-commit` binding and produced no archived evidence file.

### What was measured, and what it changed

Two rounds. The first closed four assumptions and found a defect; the second
closed four more, two of which contradicted what the documentation said.

| Claim | Result |
|---|---|
| Chat usage carries the input/output split | `prompt=170 completion=2 total=172` — it does |
| A real error arrives as a non-2xx | HTTP 400, classified `bad_request`, not retryable, not ambiguous |
| `Authorization: Bearer` works on `/anthropic/v1/messages` | Yes. This is what the three profiles sharing one connection group rests on |
| `stream_options.include_usage` is honoured | Yes — the final chunk carries the totals |
| `response_format: json_object` | **Served**, against three documentation sites that never mention the member |
| MiniMax-M2.7 accepts a disabled thinking switch | **Accepted.** The loudest predicted failure does not happen |
| Prompt caching | **Reported on both routes** (`cached_tokens: 128`), against documentation saying there is none |

Two of those changed the implementation rather than confirming it, and both had
been inferred from silence in the documentation. The Chat profile now declares
`json_object`; the schema mode stays undeclared because it was never sent, and
the same silence that was wrong about one half is no evidence about the other.
The Responses face stays undeclared for the same reason — it is a different
endpoint and was not the one measured.

**One defect was found and fixed.** The stream failed with `provider stream
ended before [DONE]`, and the diagnostic showed why: MiniMax ends a stream with
a `finish_reason` chunk, then a usage chunk, then closes — it never sends the
sentinel. For every other OpenAI-family upstream that is what a truncated
generation looks like, and the check is what stops a partial response settling
as a complete one, so the exemption is scoped to this provider **and** still
requires a `finish_reason` first. Before the fix a streaming caller paid for a
generation the upstream ran and received `malformed_response`.

**One operator-facing consequence**: MiniMax reports a cache-read tier, so
prices should be entered for it. Without a cache-read price those tokens settle
at the input rate, which is more than they cost.

### The mainland region is not measured

MiniMax splits by account region — `https://api.minimax.io` for international
accounts and `https://api.minimaxi.com` for mainland ones — with identical paths,
headers, bodies and error envelopes, and keys that are not interchangeable. Halro
serves both with one profile group and one base URL field, which is only correct
if they really are identical once a credential is attached.

Everything above ran on the international host. **The mainland round is
deliberately deferred and recorded as not measured, not as passing.** The
evidence file records which region a run covered, so the two cannot be confused
later. What remains unknown with a credential attached: whether the model lists
agree, the rate limits, and whether `base_resp` behaves the same way.

`docs/prd/minimax-adaptation-plan.zh-CN.md` §7 carries the assumptions that are
still open, each with the assertion that would close it.

### `/v1/responses` measured, and the M2 line does have Kimi's defect (2026-09-02)

Raised by the Kimi work rather than by MiniMax: `minimax.responses.v1` might have
the pairing that got `kimi.responses.v1` withheld, and neither MiniMax smoke had
ever driven that endpoint and read the *output* back through Halro's own mapper —
the same blind spot that let Kimi's row ship. Five calls on the international
host settled it, and the answer splits by model.

| request | `reasoning` output item | output tokens |
|---|---|---|
| `MiniMax-M3`, nothing asked | no | 2 |
| `MiniMax-M3` + `reasoning.effort=none` | no (echoed back as `"none"`) | 3 |
| `MiniMax-M3`, a multi-step word problem | **no** — the working is inside the text | 112 |
| `MiniMax-M2.1`, nothing asked | **yes** | 44 |
| `MiniMax-M2.1` + `reasoning.effort=none` | **yes**, unchanged | 52 |

The M2 rows are the third instance this month of one shape: a documented switch
**accepted, echoed back, and ignored**. MiniMax documents `none` on this face's
effort ladder; the upstream returns it in the response and reasons anyway. Kimi's
Responses face does the same with `thinking`.

The consequence is worse than Kimi's k2.7-code gap and was verified by running
the captured bodies through the real decoder, not by reading:

```
M3 nothing asked      decodes cleanly
M2.1 nothing asked    DecodeProviderResponse -> provider Responses output item is unsupported
M2.1 effort none      DecodeProviderResponse -> provider Responses output item is unsupported
```

That refusal is in the **provider-side** decoder, upstream of every northbound
renderer, so all three faces answer 502 with the call already paid — not just the
two that cannot render a reasoning part. The seven M2 identifiers are therefore
gone from the Responses profile in the model catalogue; they keep their Chat and
Anthropic entries, and `MiniMax-M3` keeps all three. An operator who wants one
anyway still has `operator_declared`.

Two honest limits on that. Only `MiniMax-M2.1` was driven; the other six are the
same line under MiniMax's own documented constraint that M2.x cannot be told to
stop thinking, and are removed on the fail-closed reading rather than on
evidence of their own. And M3's switch is `adaptive`, so two negative probes —
one of them built to provoke multi-step work — are evidence, not proof.

The guard this exposed a hole in is
`TestNoEndpointIsServedByATargetThatReasonsUnasked`. It asked only whether the
*endpoint* could render reasoning, and would have called `/v1/chat/completions`
safe here; that endpoint can render it and never gets the chance. It now asks the
provider profile's own decoder first.

## Kimi: measured on a mainland account (2026-09-01)

Kimi (Moonshot AI) was adapted from its published OpenAPI documents and then
measured against a real `platform.kimi.com` account. **This is adaptation
evidence, not GA matrix evidence**: Kimi is Beta on the same terms as Gemini,
Bedrock and MiniMax, so it never gates a release, and there is no row in the
runner yet.

Three findings changed code, and all three are recorded with their measurements
in `docs/prd/kimi-adaptation-plan.zh-CN.md` §10:

- The Anthropic Messages face is usable through the portable path. It had been
  dropped on the strength of Kimi's OpenAPI, which lists no `thinking` member;
  the member is accepted, and a request carrying `{"type":"disabled"}` comes back
  with no thinking block. The profile was restored.
- Kimi's single output bound counts reasoning: `max_completion_tokens: 48` on
  kimi-k3 spent 45 on reasoning and returned an empty answer. An answer-only
  bound is now routed away rather than renamed into it.
- The Anthropic face reports its thinking span under `output_tokens_details`,
  which the decoder did not read. Every Kimi reasoning span was recorded as zero.

**Any Kimi smoke has to pace itself.** A first pass firing 26 probes back to back
lost 20 of them to `rate_limit_reached_error`. The passes that produced the
evidence ran serially at one request every 22 seconds. The limit does send
`retry-after: 1` (and Kimi's own `x-retry-after`), which an earlier version of
this section got wrong: the mainland 429s all landed in the pass that captured no
response headers, and "not captured" was written up as "not sent".

**The international host is measured too** (2026-09-01, its own key — the two
platforms do not share credentials). `GET /v1/models` returns the same four model
ids with every capability field identical, and Chat usage, the pinned-temperature
refusal, the Anthropic face with thinking disabled, and Responses usage all match
the mainland shapes field for field. The "one contract, two hosts" conclusion is
now established at runtime and not only by comparing the two OpenAPI documents.

**tool_choice took three versions and six probes** (2026-09-01 and 2026-09-02),
and it is the clearest case here of documentation, an error message, and the
truth being three different things. Kimi's parameter reference says the K2.x line
does not support `required`, so the first version keyed on the model. The
upstream's own error says `tool_choice 'required' is incompatible with thinking
enabled`, so the second keyed on the reasoning switch. Both were wrong, in
opposite rows:

| model | thinking | tool_choice | result |
|---|---|---|---|
| kimi-k3 | on | `required` | 200, tool call **and** reasoning span together |
| kimi-k3 | on | named function | 400, `'specified' is incompatible with thinking enabled` |
| kimi-k2.6 | off | `required` | 200, tool call |
| kimi-k2.6 | on | `required` | 400, `'required' is incompatible with thinking enabled` |
| kimi-k2.6 | on | named function | 400, `'specified' is incompatible with thinking enabled` |

The two forms are two rules, which is the part nothing predicted. `required`
conflicts with reasoning on the K2.x line and not on kimi-k3; a **named function**
conflicts with reasoning on every model, kimi-k3 included. Same model, same depth,
one request answers 200 and the other 400.

That split is also what let half of it move before the reservation: a named
function is a property of the profile, so it is routed away by field rule, while
`required` stays a per-model refusal in the renderer because expressing it by
profile would cost kimi-k3 the request it serves.

One cell of the allowing side stays inference, and a low-risk one: a named
function with thinking **off** was not driven. The upstream's error names thinking
as the condition, and `required` with thinking off is measured at 200.

The Anthropic route's 429 was measured by deliberately tripping the organisation
limit, with the operator's consent: it answers in the **OpenAI** envelope, so that
endpoint uses Anthropic's shape for 400 and OpenAI's for 401 and 429. Its 503 is
still unmeasured and cannot be arranged; what stands in for it is
`TestAnthropicErrorDecodingToleratesShapesItHasNotSeen`, which pins Halro's own
tolerance — classify by status, never fail on an unparseable body, never carry
provider response bytes into the error — and is named so it is not mistaken for
evidence about Kimi.

What *was* established without a credential, and it is worth separating from the
guesses:

- Both hosts exist, both authenticate with a bearer token, and all three routes
  answer 401 with `{"error":{"message":"Incorrect API key provided","type":
  "incorrect_api_key_error"}}` — `GET /v1/models`, `POST /v1/chat/completions`
  and `POST /anthropic/v1/messages`, on `api.moonshot.cn` and `api.moonshot.ai`
  alike.
- The two regions serve one contract. The published `openapi.json` documents were
  compared structurally: identical path sets, identical schema name sets,
  identical request property sets on the chat, responses and messages request
  schemas, and one differing `servers[0].url`. Prices and currencies differ; the
  contract does not.
- The Anthropic-shaped route answers 401 in the *OpenAI* error shape, and its
  OpenAPI declares the Anthropic shape for 400 and 500. Error decoding on that
  face has to tolerate both.

The three assumptions that moved money are all closed. Chat's `prompt_tokens`
includes `cached_tokens` and Kimi also sends the standard
`prompt_tokens_details.cached_tokens`; a stream reports usage with and without
`stream_options`, and the option Halro already sends produces the standard
top-level shape; and `max_completion_tokens` counts reasoning, which is the
finding that changed the routing rule. That question was asked and answered. Kimi's Chat face accepts
`thinking:{"type":"disabled"}` on kimi-k3 and kimi-k2.6, and both then stop
reasoning — so kimi-k3's documentation and its own `/v1/models` metadata
(`supports_thinking_type: "only"`) are both wrong about it, the fifth place
Kimi's documentation did not survive contact. The renderer now follows the house
rule the DeepSeek and MiniMax renderers already follow — unspecified means off —
and the routing rules that had taken Kimi off two northbound endpoints are gone.
kimi-k2.7-code really cannot be switched off (`invalid thinking: only
type=enabled is allowed for this model`) and keeps two after-reservation
refusals of its own. See `docs/prd/kimi-adaptation-plan.zh-CN.md` §13.

One assumption remains open: what the
Anthropic face answers on 503. It cannot be arranged deliberately and is recorded
as unmeasured rather than inferred.

One assumption was closed the wrong way and is recorded here because the
measurement above is what makes the correction readable. Responses usage was
measured and the Responses *output* was not read back through Halro's own mapper:
Kimi's `/v1/responses` reasons by default and its effort ladder has no off value,
so every call returns a `reasoning` output item, and the canonical mapper refuses
one rather than dropping it — the verb three separate documents got wrong. That
made `kimi.responses.v1` fail every request after the upstream had been paid, and
the profile is withheld.

**Both off switches were then tried on that face (2026-09-02) and neither works.**
`reasoning.effort="none"` is refused — `reasoning.effort value "none" is not
supported` — and `thinking:{"type":"disabled"}`, the undocumented member that does
switch reasoning off on Kimi's *Messages* face, is accepted here and ignored: 200,
with the reasoning item still returned. One upstream, one key, one model, two
routes, opposite behaviour for the same member. Extrapolating from the other face
would have shipped a 200, a bill, and a caller who believed reasoning was off, so
the withholding is now a measured conclusion rather than a precaution. The control
run is worth recording on its own: a 64-token budget came back as 61 reasoning
tokens, one `reasoning` output item, and no message item at all.

Two smaller things fell out of the same run. Kimi's Responses face does **not**
reject unknown members — `thinking` is absent from its schema and still answered
200 — which closes the open question of whether the unconditional `store:false`
this renderer emits would have been a 400 on every call. And its `input_tokens`
**includes** the cached span (89 of 89 cached on the probe), the opposite of the
Messages face.

No northbound endpoint lost Kimi: the Chat and Anthropic profiles still serve all
three. See `docs/prd/kimi-adaptation-plan.zh-CN.md` §14.5.

`docs/prd/kimi-adaptation-plan.zh-CN.md` §10.2 carries the closed list item by
item, §10.3 the four places Kimi's own documentation was wrong, and §11 the
international round.

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

### The vision probe's own image was invalid, measured 2026-08-26

`openai.gpt-5.4` on `/openai/v1` reads images, and Halro's capability detection
recorded vision as unsupported for it. The refusal was real and it was ours:

| what was sent | answer |
| --- | --- |
| text only | 200 |
| the probe's compiled-in 1x1 PNG | 400 `validation_error` — `Invalid or unsupported image format` |
| a rebuilt 1x1 RGBA transparent PNG | 200 |
| 1x1 opaque RGB, 1x1 grayscale, 8x8, 64x64, 256x256 | 200 |

Neither the size nor the alpha channel decides it — a rebuilt image of the same
1x1 transparent shape is accepted. The compiled-in bytes were a corrupt PNG:
`IDAT` failed its chunk CRC and the zlib stream failed its Adler-32. Lenient
decoders accept it, which is why it survived; Mantle's does not.

Two things follow, and both are fixed rather than noted. The probe image is now
a byte-valid 8x8 PNG that `image/png` decodes, with a test that decodes it — the
old one cannot pass that test. And the console now prints the upstream status and
identifier beside a refused capability, because "this model does not support it"
was Halro's sentence for what was in fact the upstream rejecting Halro's own
payload, and an operator holding the model card had nothing to check it against.

The residual limit is worth stating: a refusal that names no parameter is
attributed to the field the probe added, which is the right field and not
necessarily the right conclusion. Nothing reads the sentence beside it, so an
upstream that refuses a probe's payload for its own reasons still reads as
"unsupported" — the identifiers on screen are what makes that visible.

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
claimed there — tools, both JSON halves, vision, developer role and reasoning
are all inside the Mantle ceiling and none was exercised per model, so they
stay for capability detection to establish against a real account.

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

## The Ledger write path, measured (2026-08-29)

The Settings card reports a request-rate ceiling for this instance. The numbers
behind it were arithmetic over counters until this date. They are now measured,
and the harness is committed rather than thrown away:

```
go test ./internal/ledger/ -run '^$' -bench ReportedCeiling -benchtime 2000x
```

`BenchmarkReportedCeilingTracksAchieved` drives `ledger.Log` under the shipped
configuration — `MaxBatch: 128` and a 2 ms `FlushInterval`, which the sibling
`BenchmarkConcurrentAppend` does not, since it runs the package defaults where
there is no linger. Real fsyncs on an APFS laptop volume; on darwin
`os.File.Sync` issues `F_FULLFSYNC`, so these are pessimistic bounds that do not
transfer to a Linux NVMe host.

| offered concurrency | records per flush | fsync | sustained |
| --- | --- | --- | --- |
| 1 | 1.00 | 4.40 ms | 29 req/s |
| 4 | 4.00 | 4.31 ms | 119 req/s |
| 16 | 16.00 | 4.51 ms | 460 req/s |
| 64 | 64.00 | 4.43 ms | 1,837 req/s |
| 128 | 128.00 | 4.40 ms | 5,256 req/s |
| 256 | 128.00 | 4.38 ms | 5,318 req/s |

Three things this settles.

**Group commit works, and the batch size is exactly the offered concurrency**
until it saturates at `MaxBatch`. Between one and 128 concurrent appenders the
same disk carries 175× the traffic. So a batch size of 1 is a statement about
arrival, not about the disk, and the ceiling it implies is a floor that rises —
which is what the card now says instead of naming a fault.

**A ceiling derived from the barrier alone is optimistic**, by 55% at
concurrency 1 and 14% at saturation — the benchmark reports exactly this as
`barrier-derived/achieved`, 1.53 and 1.15, beside `reported/achieved` at 1.00. The gap is the flush interval: at
concurrency 1 a batch occupies 6.7 ms, of which the fsync is 4.4 ms and the rest
is `collectBatch` lingering for an appender that never arrives. The Ledger now
measures the writer's own busy time (`AppendStats.BatchDuration`) and the card
divides records by that; reported and actual then agree within 1% at every
level above.

**The per-project lock is not the binding constraint here.** Its hold implies
six figures of requests per second while the Ledger sustains four, so on this
host the minimum is always the Ledger's. That is the case the card used to get
wrong by reporting the lock alone.

Scope: one host, one filesystem. The shape — batch size tracking concurrency,
the linger showing up as the gap between derived and actual — is the finding;
the absolute numbers are this laptop's. Nothing here involves a provider, so it
costs nothing to re-run elsewhere.

## AWS Bedrock Mantle: the workspace header, measured (2026-08-29)

A second, narrow measurement against a real account, in `us-east-2` against
`anthropic.claude-opus-4-6-v1`. It settles one question the 2026-08-21 run did
not ask: which header selects a Bedrock Project on `/anthropic/v1/messages`.

The measurement is an A/B on one request. The same body, the same credential,
and the same **nonexistent** project id, changing only the header name:

| header | project id | status |
| --- | --- | --- |
| `anthropic-workspace-id` | `proj_zzzzzzzzzzzzzzzzzzzz` | **404** |
| `anthropic-workspace` | `proj_zzzzzzzzzzzzzzzzzzzz` | **200** |

A nonexistent project is the discriminator, and it is what makes this cheap: a
name the service reads has to reject it, and a name the service does not read
cannot. The 200 is the whole finding — the request was served, against the
account default, carrying a project id that does not exist.

So `anthropic-workspace-id` is read and validated, and `anthropic-workspace` —
the name Halro sent until this date — is ignored. A connection that named a
Bedrock Project was billed to the account default without any error to say so.
The header name is also **not** what separates Bedrock from Claude Platform on
AWS: both spell it `anthropic-workspace-id`, and the separation is the host plus
the identifier prefix, `proj_` against `wrkspc_`.

An earlier request in the same session, with a real project id, answered 200
(`req_73rietrkqqo5kr5lj5wlkhzhubscpxpayzjlr5jjtmhhw3cpqcwq`).

### What this does not establish

- **Positive attribution.** That a valid project id causes usage to be recorded
  against that project — rather than merely to pass validation — is visible in
  CloudWatch `AWS/BedrockMantle`, whose `Inferences` and token metrics carry a
  `Project` dimension. That reading has not been taken.
- **Halro's own path.** These were `curl` requests against the service. They say
  the header name is right; they do not say Halro sends it. `tests/provider-matrix`
  still has no Mantle coverage, and the smoke in
  `internal/provider/bedrockmantle/real_smoke_test.go` cannot close this gap as
  written: it asserts that a request succeeds, and an ignored header succeeds
  too. Verifying Halro end to end means running the smoke with
  `HALRO_SMOKE_BEDROCK_PROJECT_ID` set and then reading the CloudWatch
  attribution — the assertion cannot be synchronous, because the metric is not.

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
