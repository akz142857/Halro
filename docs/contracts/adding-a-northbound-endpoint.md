# Adding a northbound endpoint

Normative checklist for adding an API face Halro *serves*, as
`adding-a-platform.md` is for an upstream Halro *calls*. Every step names the
file it belongs in and the guard that fails if it is skipped, so this document is
a map rather than a memory aid.

The two documents are siblings and the distinction matters, because the same
word means opposite things in them. A **provider profile** is a face Halro
speaks *to*; a **northbound profile** is a face Halro speaks *for*. This file is
about the second.

## Read this first: what actually goes wrong here

Adding a platform fails by forgetting a registration. Adding a northbound
endpoint fails a different way, and the failure has happened twice:

- **DeepSeek, 2026-08-18.** `/v1/responses` rejects the `reasoning` request
  field outright, so a caller cannot ask for reasoning there. DeepSeek's own
  default is to reason anyway. The reasoning came back, the endpoint's renderer
  could not represent it, and the request ended as `malformed_response` with
  `ambiguous: true` — the caller paid for reasoning they had no way to request
  and received nothing. See `docs/prd/deepseek-adaptation-plan.zh-CN.md` §9.
- **Kimi, 2026-09-01.** The same chain, on an operator's screen, on both
  `/v1/responses` and `/v1/messages`: `502 provider response cannot be rendered
  safely`, then `503 no healthy deployment is available` once enough of them had
  marked the deployment unhealthy. See
  `docs/prd/kimi-adaptation-plan.zh-CN.md` §12–§13.

The shape both times:

> **What an endpoint can represent is a property of the endpoint, and the
> provider-side renderer cannot see which endpoint the caller used.**

A northbound endpoint that refuses to *carry* something must not be paired with
an upstream that *produces* it unasked. Neither half is wrong on its own. The
pairing is, and nothing in the request says so — which is why both incidents
were found in production rather than in the suite.

Two corollaries worth stating before the steps:

1. **Enumerate what your endpoint cannot render, and check it against every
   provider profile's defaults, not against its accepted request fields.** The
   DeepSeek and Kimi failures were both about output the caller never requested.
2. **Prefer making the upstream not produce it over refusing the route.** The
   Kimi fix went the wrong way round first — it took Kimi off two endpoints,
   which broke the promise the gateway exists to keep, that an application does
   not change when the operator repoints an alias. The right fix was the house
   rule the DeepSeek renderer already followed: unspecified means off.

## Four classes of work, and they differ by an order of magnitude

Before the checklist, decide which of these you are doing. Most requests to
"support X" are not the third kind, and the first kind needs no new endpoint at
all.

**A. A feature of an endpoint Halro already serves.** Streaming on
`/v1/responses` is already served; file inputs and context compaction are
declared rejected by ADR 0005 and would be *widened into* the existing portable
subset. No new northbound profile: this is a change to one endpoint's accepted
fields, its field rules, its manifest, and — the expensive half — a per-provider
answer to "can this face represent it at all". That per-provider half is the same
work `adding-a-platform.md` describes, once per platform.

**B. A stateful or resource-owning surface.** Conversations, background mode,
stored responses, webhooks. ADR 0005 is explicit that these are not deferred
implementation but a missing design:

> Halro does not yet have a cross-provider ownership model for those resources.
> A future stored tier requires a new ADR defining provider/deployment/profile/
> region binding, lifecycle, encryption, deletion, and failover behavior.

The difficulty is not the code. A conversation lives on one upstream, and a
public model alias exists precisely so an operator can repoint it; stateful
resources make failover and repointing incorrect rather than merely awkward.
ADR 0009 and ADR 0021 built the `ProviderResource` model for files, batches and
async invocations, which is why those endpoints exist and why they are still
`experimental` — that is the path a new stateful surface follows, and it starts
with an ADR.

Webhooks carry one more: they are the *upstream* originating requests. All
outbound traffic goes through `safetransport` — HTTPS-only, host allowlists,
DNS/IP validation, no redirects — and a webhook is traffic that does not. That
is the same class of decision as the `provider_executed_tools` capability, which
CLAUDE.md describes as changing "who else gets to make requests". Contract
review, not a table edit.

**C. A new transport.** WebSocket mode is not a feature; it is a second way of
carrying a conversation. Halro's whole streaming contract — ADR 0005's
deterministic event sequence, "after the first emitted event the attempt is
ambiguous and no cross-provider retry is allowed", `internal/sse`'s bounded
parsing — is written for HTTP plus SSE. A new transport means a new northbound
profile, a new connection lifecycle, and new retry and accounting semantics for
a request that outlives a single HTTP exchange.

**D. A read answered from Halro's own configuration.** `GET /v1/models` is the
one of these: it consults no provider, writes no ledger event, and its answer is
the caller's Project and the live route table. It is much cheaper than B or C —
there is no semantic mapping, no renderer, no provider coverage, and the two
incidents above cannot happen to it, because nothing upstream produces anything
for it to fail to represent.

Two things about it are not cheap, and they are where the work goes.

The first is that such an endpoint is still an API face with a published
contract, a frozen route and a compatibility manifest; skipping steps 3 and 5
does not mean skipping 1, 4, 6 and 7.

The second is the hard part, and it is a documentation problem rather than a
code one. The wire shape belongs to the API being imitated, and it carries
promises Halro's domain does not keep. An OpenAI model object implies a thing
with a stable identity and stable capabilities; a Halro `id` is a public alias,
which is a pointer an operator may repoint between two calls. Non-standard
fields are not the way out — an SDK-contracted object with an extra member is a
member somebody's client chokes on, which is why the deferred tier put its
polling cadence in a header. So the gap between what the shape implies and what
the value is has to be discharged in `DocumentedDeviations`, exhaustively.
Under-stating it there is this class's version of the DeepSeek and Kimi
failures: a promise the endpoint cannot keep, made where nobody looks for it.

Only B, C and D are "add a northbound endpoint". A is "widen one".

## The steps

### 1. The northbound profile

`internal/compatibility/profile.go` — a `NorthboundProfileID` constant, plus a
row in `BuiltinNorthboundProfile` carrying its protocol, revision, and the exact
`METHOD /path` strings it serves.

The method list is not decoration. `EndpointCompatibilityManifest.Validate`
refuses a manifest whose `METHOD path` is not in it, and refuses one whose
protocol or revision disagrees. Guarded by `TestManifestRejectsNorthboundProfileDrift`.

One profile may carry several methods — `ProfileAnthropicMessages` carries
`/v1/messages` and `/v1/messages/count_tokens`, and
`ProfileOpenAIMediaResources` carries eleven. Group by "one wire contract the
caller integrates against", not by "one HTTP route".

### 2. The wire types

The protocol's own package — `internal/openaiapi`, `internal/anthropicapi`, or a
new one. A request type, a strict decoder that rejects unknown fields, and a
`Validate` that refuses what the endpoint does not model.

**Refuse rather than ignore.** ADR 0005's wording is the rule: these are
"compatibility limits, not silently ignored fields". A member accepted and
dropped is a request the caller paid for and did not get.

No mechanical guard holds this file to the manifest in step 4 — see "Steps with
no mechanical guard".

### 3. The semantic mapping

`internal/compatibility/<protocol>/` — decode the wire request into
`semantic.GenerateRequest` (or whichever operation), and render the semantic
result back out. Streaming endpoints need the event renderer too.

Set `Source: semantic.Source{ProfileID: string(ProfileX), ProfileRevision: N}`.
That field is how a provider field rule can key on which northbound endpoint the
request came from, and a mapping that leaves it empty makes any such rule
invisible.

**This is where the two incidents happened.** The renderer decides what the
endpoint can carry back, and it is the only place that knows. Before writing it,
enumerate the `semantic.Content` kinds it cannot represent, and then ask, for
each provider profile in step 5, whether that upstream can produce one *without
being asked to*. Reasoning is the one that has caught this codebase twice.

### 4. The endpoint manifest

`internal/compatibility/manifest.go` — one `EndpointCompatibilityManifest`, with
`RequestFields`, `ResponseFields`, `StreamEvents`, `StateSemantics`,
`DocumentedDeviations`, `Evidence`, `SDKMatrix`, `ProviderProfiles` and one
`ProfileCoverage` row per profile.

`Validate` is unusually strict here, and each rule is worth knowing:

- A `compatible` status requires documented deviations *and* an SDK matrix *and*
  `EvidenceSDKBlackBox`. Claiming SDK evidence without naming the SDKs, or
  naming them without claiming it, is refused — the two ways of saying the same
  thing may not drift.
- Every declared provider profile needs a coverage row, and every coverage row
  needs a declared profile.
- A coverage row may only name a field in `RequestFields`. This is why
  under-declaring `RequestFields` silently hides a real refusal: the Kimi work
  found `reasoning` missing from the Responses endpoint's list while
  `responses.go` had been reading it all along.
- `RequestFields` must be non-empty, with one named exception: a class D
  collection read that takes no parameters at all — `semantic.OperationDiscovery`
  — has none to declare. The exemption is written against that one operation
  rather than derived from "does not reach a provider", because the wider
  predicate would also take the rule off Run Governance, which accepts request
  bodies and must keep owing a declaration for them.
- Provider profiles and their coverage rows are required of every endpoint that
  reaches an upstream — `semantic.Operation.ProviderBacked()`, which is false for
  Run Governance and for discovery. An endpoint answered from Halro's own state
  declares neither, and `ProviderBacked` is the right predicate here precisely
  because it is the wrong one above.

Guarded by `TestBuiltinEndpointManifestsAreValidImmutableAndGolden`,
`TestEveryEndpointDeclaresItsEvidence`,
`TestEndpointEvidenceCannotDriftFromTheSDKMatrix`, and — for the two rules above,
which were once rewritten and shipped with nothing watching them —
`TestManifestRejectsAnEndpointThatDeclaresNeitherRequestFieldsNorAReasonToHaveNone`. The golden file is
`docs/compatibility/endpoint-manifests.json`; regenerate with
`HALRO_UPDATE_GOLDEN=1` and **read the diff** — it is the published contract.

### 5. Provider coverage, in both directions

**Class D skips this step entirely**: an endpoint that reaches no upstream has no
profile to cover, declares none, and `Validate` requires none of it.

Every provider profile that may serve this endpoint needs a coverage row, and
every member it cannot carry needs a declaration in
`internal/compatibility/provider_fields.go`.

Three guards, and they disagree with each other on purpose:

- `TestTheManifestDeclaresEverythingTheRulesRefuse` drives a synthetic probe
  battery through every rule and requires declared and refused to match
  **exactly**, in both directions, for fields the endpoint models.
- `TestPortableMessagesCoverageDeclaresEveryOutputConfigLoss` and
  `TestResponsesCoverageDeclaresEveryOutputBudgetAndFormatLoss` drive **real**
  request bodies through the actual decode chain and check the same thing.

Where the two disagree, the rule is wrong rather than the tests: a synthetic
probe can reach a value the real endpoint never produces. Kimi hit this twice —
each time the resolution was to move the fact to the layer that owns it (a
capability rather than a field rule), not to weaken a test.

Two files support this and are easy to miss:

- `coverageProbes(northbound)` builds the battery. **A new value your endpoint
  accepts needs a probe**, or a rule about it is invisible in both directions at
  once. The battery had no sampling probes until Kimi refused `temperature`, and
  no northbound identity until a rule keyed on one.
- `endpointSpelling(endpointID, field)` maps the chat-shaped name a rule returns
  to your endpoint's own name for it — `response_format` becomes `text.format`
  on Responses and `output_config.format` on Messages. Without an entry the
  guard cannot connect a refusal to its declaration.

### 6. The route

`internal/app/runtime.go` — register the handler inside the guarded group for
its protocol, so it inherits the stale-snapshot refusal, the limiter and the
guard middleware. **Do not register it outside that group**: those middlewares
are what turn an unauthenticated caller away before its body is read.

Guarded by `TestEveryGatewayRouteIsADeclaredNorthboundMethod` and
`TestEveryGatewayRouteSitsInsideItsGuardedGroup`, both in
`internal/app/gateway_contract_test.go`. The second is what holds this
paragraph's warning: it counts the middlewares on each northbound route, so a
route registered beside a guarded group instead of inside one fails by name.

### 7. The handler

`internal/gateway/service.go` — decode, delegate to the existing generation hot
path, render.

Do not build a second request authority. ADR 0005's wording is the rule for
every facade after it: translation delegates, so "authentication, source policy,
token guard, limits, budget reservation, per-attempt accounting, conservative
unknown settlement, redaction, cancellation, retry, and model alias rules remain
unchanged". A facade that reimplements any of those has forked an invariant.

### 8. Native mode, if the endpoint has one

Only for an endpoint whose caller bytes are forwarded verbatim. See
`adding-a-platform.md` step 8 for the provider half; the northbound half is a
schema in `internal/compatibility/<protocol>/native.go` and a gate in
`internal/gateway/service.go`.

### 9. The console, if the endpoint should be offered

`web/src/pages/DeveloperPage.tsx` carries the code-sample protocol selector. It
is **not** the list of endpoints Halro serves — it covers three of the
twenty-three routes the gateway carries, and
`/v1/messages` is not among them. Adding an endpoint does not require adding it
here, and adding it here does not make it served.

## Steps with no mechanical guard

One left, and two that were written after this document named them. They are
kept here rather than deleted because what a guard does *not* cover is the part
worth reading.

**The wire type against the manifest (steps 2 and 4) — still uncovered.**
`RequestFields` is hand-written. A member the decoder accepts but the manifest
omits cannot be declared unsupported by any profile, so a real refusal is
silently dropped by `Validate`'s "unknown request field" filter rather than
reported. Found by accident during the Kimi work, on `reasoning`.

**The route table (step 6) — guarded.** `TestEveryGatewayRouteIsADeclaredNorthboundMethod`
in `internal/app/gateway_contract_test.go` walks the gateway router against the
northbound profiles in both directions: a route with no profile is a face the
published contract does not describe, a declared method with no route is a
manifest promising a 404. `TestEveryGatewayRouteSitsInsideItsGuardedGroup`
covers step 6's warning as well, by counting middlewares — a northbound route
registered on the bare router carries the two the router itself has, and one
inside a guarded group carries five.

Writing it needed `compatibility.AllNorthboundProfiles`. The table had been a
map built inside `BuiltinNorthboundProfile`, which is to say a list nothing could
walk — the same shape the provider profile table was fixed out of.

**Whether the renderer can carry what providers produce (step 3) — guarded, and
enforced at routing time.**
`TestNoEndpointIsServedByATargetThatReasonsUnasked` in
`internal/app/northbound_reasoning_contract_test.go` pairs the two halves this
document says nothing paired.

The endpoint's half is derived by execution rather than declared: the real
northbound renderer is handed a result carrying a reasoning part and one
carrying only text, and the endpoint is intolerant when it refuses the first and
accepts the second. The control matters — without it the guard reports the Chat
face, the one face that does carry reasoning, as unable to.

The provider's half is `modelcatalog.Entry.ReasonsUnasked`. It is deliberately
**not** in `ProviderCapabilities`: every member of that struct answers "may this
be turned on", is offered to an operator as a checkbox, and is bounded by a
connection ceiling that has to contain it. "What arrives whether or not anyone
asked" is none of those, and adding it there would invert the containment.

**It reaches the router.** `compatibility.ReasoningAnswerSurvives` pairs the two
halves, `filterUnrenderableReasoning` in the gateway drops a marked target from
an endpoint that cannot carry its answer, and the refusal names
`target_reasons_unasked` so an operator is not left bisecting a request that
asked for nothing unusual. It happens before the budget reservation, so where a
route has another deployment the request is simply served by it, and where it
does not the caller is told rather than charged.

The fact does **not** go through the deployment capability snapshot. It is read
from the catalogue when the registry is built, because it is neither a capability
nor an operator declaration: the snapshot exists to pin what an operator agreed
to, and a fact that can only ever remove a route should not wait for every
deployment to be re-saved before it applies. No durable schema changed, and a
model the catalogue does not cover answers false — routing away everything
unknown would refuse every operator-declared deployment.

The chain is three links and each has its own test, which is not ceremony: the
two ends were covered first and backing the middle one out changed nothing that
failed. `TestTheRegistryReadsReasonsUnaskedFromTheCatalogue` is that link.

A withheld profile is skipped, which is what attaches a withholding to its
reason. `kimi.responses.v1` is withheld because that face reasons on every model
and its ladder has no off value; un-withholding it without finding an off switch
fails this guard rather than reaching an operator.

## After the steps

Run `HALRO_UPDATE_GOLDEN=1 go test ./internal/compatibility/ -run
TestBuiltinEndpointManifestsAreValidImmutableAndGolden` and read the diff in
`docs/compatibility/endpoint-manifests.json`. That file is what an integrator
reads to decide whether their client will work, so the diff is the review.

Then answer, in writing, the question both incidents came from: **for every
provider profile this endpoint accepts, what does that upstream return without
being asked, and can this endpoint render it?**
