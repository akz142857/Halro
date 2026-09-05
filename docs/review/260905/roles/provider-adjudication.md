# Independent adjudication: PROV-01

**Verdict: CONFIRMED, with scope clarification. Severity: P1. Confidence: high.**

Baseline independently checked: `381743f6613607dc256828f4776b52af8bdd232c`, 2026-09-05. This adjudication examines an authenticated, valid unary request whose configured upstream answers HTTP 200 with an unusable body. It is **not an external-attack scenario**, and does not depend on an attacker controlling the upstream.

The concrete confirmed defect is that early OpenAI Chat response read/size/JSON/required-envelope failures are classified as `ErrorMalformed, Ambiguous=false`. The real generation bridge preserves that classification. The gateway both repeats the outbound request and commits zero cost for the failed attempts. Independent tests reproduced this **with metered pricing, an immutable price snapshot, a committed price pin and a positive reservation before each outbound call**. Thus the original result is not merely an artifact of omitting the production pricing path.

Retain P1 for incorrect uncertainty handling in a normal configured generation path: possible duplicate upstream execution and failure to conservatively account for possibly executed work. Do not describe the result as proven real-provider billing, unlimited retry, unauthorized execution, ledger corruption, or loss of all attempt records. The confirmed calls are controlled HTTP-transport exchanges; actual upstream execution and charges remain unknown, which is precisely the condition the ambiguity flag must preserve.

## Original fixture assessment

The provider report was read with explicit authorization. Its fixture uses `openai.New`, `BuiltinProfile(ProfileOpenAIChatEmbeddings)`, `NewLegacyAdapterBridge`, an enabled/authenticated project fixture and `Service.Chat`. This is a valid way to reach the production Chat decoder:

- `internal/provider/openai/adapter.go:57-64` makes `New` an OpenAI `NewWithOptions` constructor with the usual bearer scheme and Chat capabilities.
- `internal/app/provider_adapters.go:82-85,247-254` selects the same adapter for the production OpenAI Chat profile, with `Responses=false` and an explicit static bearer authorizer.
- `internal/app/providers.go:536-545` wraps production adapters in the same legacy bridge.
- `internal/provider/profile_bindings.go:74-76` maps the profile's Chat operation to `PrimitiveOpenAIChatCompletions`; `internal/provider/primitive.go:170-181` then invokes the adapter's Chat method.
- Its explicit `Capabilities{Chat:true}` is necessary and legitimate. The earlier fixture failure without an operation capability does not disprove reachability of a normally configured Chat deployment.

The original fixture **does not** exercise HTTP ingress, persisted provider/deployment creation, URL/DNS/TLS enforcement, the production price-pin store, or real upstream execution. Its direct `Registry.Register` bypasses app assembly validation; its static target prices use the non-snapshot accounting branch. Those are material evidence limits, but neither is an effective response-time defense. The independent fixture below strengthens the missing pricing and routing assertions rather than claiming to have run the entire binary.

## Full defense chain and adjudication

| Boundary | Current source and defense | Why it does / does not stop this case |
|---|---|---|
| Northbound ingress | `internal/gatewayapi/handler.go:753-797`: request ID/source handling, bearer extraction, bounded strict Chat decoding, route timeout, then `service.Chat` | A valid small authenticated unary request passes. No later handler code revises the provider error or accounting result. HTTP ingress was source-traced, not independently socket-tested. |
| Project authorization | `internal/gateway/service.go:278-319`: key authentication, allowed alias, source CIDR, policy snapshot coverage, eligible target resolution | These defend unauthorized/unservable requests. The reproduction uses a real synthetic Gateway Key under an enabled project authorizing `chat`; no authorization bypass is used. |
| Production route assembly | `internal/app/providers.go:475-545,560-720`: endpoint/audience, enabled binding, manifest/type/surface/scheme, capability ceilings, deployment state/drift, price readability, adapter registration | A valid enabled OpenAI binding/deployment with Chat evidence and a current price can pass all gates. These gates run before a response exists; they do not authenticate or repair the content of a later successful HTTP response. |
| Request admission | `service.go:1104-1148,338-380`: semantic/profile/primitive and token filtering, inbound redaction, Token Guard, project limiter, request accounting | Independent fixture passes these using a plain text request and unrestrictive policy. Existing policy limits can prevent a particular call, but are not a general defense after admission. |
| Per-attempt admission | `service.go:470-618`: circuit lease, concurrency lease, current price and pin, cost recheck, budget reservation, pin commit, `MarkStarted` | Independently verified positive reservation (50 micro-USD), committed pin and metered snapshot at the moment each outbound request enters the transport. `MarkStarted` does not force every later zero-token error to become ambiguous. |
| HTTP authorization/acceptance | `internal/provider/openai/adapter.go:487-513`: create POST, apply authorizer, set JSON headers and X-Request-ID, `client.Do`; non-2xx handled separately | Independent transport asserts actual URL `/v1/chat/completions`, bearer header, provider model, unary body and stable nonempty X-Request-ID. It then returns HTTP 200. No authoritative refusal or evidence of nonexecution exists in the malformed body. |
| Transport versus body failure | `adapter.go:500-507` correctly sets transport ambiguity from `!provider.Unsent(err)`; `:514-521` instead returns bare malformed errors for body read/size failure | The correct transport defense is bypassed when `Do` succeeds with headers and body processing fails afterward. An HTTP 200 with a body returning `io.ErrUnexpectedEOF` reaches the latter branch. |
| Chat decoding | `adapter.go:411-415`: JSON unmarshal and required ID/object/choices checks return `ErrorMalformed` without ambiguity | This is the primary defect, not an unsupported-profile selection or caller-input rejection. Both truncated JSON and `{}` reproduce it. |
| Canonical bridge | `internal/provider/primitive.go:175-181`: adapter errors return unchanged; only later semantic conversion failures set `Ambiguous:true` | The later protection cannot see a response already discarded by the adapter. Do not claim every malformed semantic response has this defect: normalization failures after successful adapter decoding already have the correct ambiguity flag. |
| Retry decision | `service.go:2478-2489`: ambiguity blocks retry; otherwise `ErrorMalformed` is retried even if `Retryable` is false. Loop at `:1161-1200,1261-1279` | Both failed attempts log `Retryable=false`; the class-based OR still retries. Default target attempts=2 and total attempts=3 (`service.go:833-846`, `config/default.yaml:157,199`, passed by `app/runtime.go:460-464`). One eligible target therefore makes two calls. Multiple-target fallback is source-reachable but was not independently exercised. |
| Settlement classification | `service.go:2874-2916`: only `provider.Error.Ambiguous` establishes uncertainty; a non-ambiguous error returns `Outcome=provider_error` with zero cost/tokens | The early error provides no valid usage and is treated as definitive nonservice. The counterfactual ambiguity-only control instead takes the estimate branch. |
| Settlement completion | `service.go:666-702`: enrich/log/capture, release concurrency, settle, report breaker. `budget/manager.go:587-594,606-636` recognizes zero-cost/non-success/zero-token settlements as unserved | **Important attempted refutation:** immutable pricing does not reject or charge this result. `unservedSettlement` deliberately bypasses the known-price formula check, including a fixed fee. This defense is appropriate for actual unserved failures; the bad ambiguity classification incorrectly qualifies this accepted response. The independent fixture includes a 7-micro-USD fixed fee to prove the distinction. |
| Failure bounds/finalization | `service.go:783-788` reports malformed availability failures to the circuit; the run finalizes provider_error and releases leases | The circuit, attempts, deadlines, rate limits and Token Guard can bound later work. Default circuit threshold is five failures, so it does not block the demonstrated second call on a fresh circuit. Attempts are settled/finalized, not left for unknown-orphan recovery to repair. No crash/recovery test is claimed. |

The repeated X-Request-ID is correlation material; this code does not send an upstream idempotency contract or establish that OpenAI deduplicates by that header. Treating it as an execution guarantee would be unsupported. The reproduction does not require a second user request or northbound idempotency retry: both outbound POSTs occur within one authorized `Service.Chat` call.

## Independent reproduction

Own test source: `/private/tmp/halro-provider-adjudication-260905/adjudication_test.go`.
Overlay: `/private/tmp/halro-provider-adjudication-260905/overlay.json`, virtually adding `internal/gateway/review_adjudication_overlay_test.go`. No repository test or production code was modified.

The new fixture differs from the original:

1. Uses `NewWithOptions` with the production-style static bearer authorizer, `ProviderType=openai`, Chat capability and explicit real profile/surface/binding fields.
2. Asserts `target.Generation(OperationChat).ProviderPrimitive()` is the production OpenAI Chat primitive.
3. Supplies the existing narrow `fakePricePinStore` fixture with a valid metered price version: input 1,000,000 micro-USD/million tokens, output 2,000,000, fixed fee 7. The gateway and budget manager construct and validate real immutable snapshots; only the metadata pin storage is synthetic.
4. At every outbound transport call, checks positive reserved balance, committed pin and metered lease snapshot. This covers the production `PricePinStore` branch, rather than only direct target-price projection.
5. Checks outbound method, URL, bearer credential, provider-model rewrite, nonstream body, and stable nonempty correlation ID. `https://api.openai.com` is only a URL assertion: a custom in-memory RoundTripper intercepts every request, so **no network or real-provider call occurs**.
6. An observing adapter wrapper calls the actual OpenAI decoder and records its error unchanged for every defect case. Only the expressly labeled counterfactual control clones the error and changes `Ambiguous` to true. This is diagnostic evidence, not an applied fix.

| Case | Real adapter error before control override | Outbound calls | Final committed / reserved micro-USD |
|---|---|---:|---:|
| HTTP 200 truncated JSON | malformed_response, ambiguous=false, retryable=false | 2 | 0 / 0 |
| HTTP 200 `{}` missing required fields | same | 2 | 0 / 0 |
| HTTP 200 body read returns `io.ErrUnexpectedEOF` | same | 2 | 0 / 0 |
| HTTP 200 body exceeds 16 MiB | same | 2 | 0 / 0 |
| HTTP 400 definitive refusal control | bad_request, ambiguous=false, retryable=false | 1 | 0 / 0 |
| HTTP 200 truncated JSON, ambiguity-only counterfactual | original malformed_response, then test sets ambiguous=true | 1 | 50 / 0 |

Each outbound call observed a reservation of **50 micro-USD** with its committed metered price pin. All calls ended with a gateway error. The 400 case's raw transport log also says “accepted call” because that fixture log is printed before returning its chosen status; it means transport entry, **not** an assertion that HTTP 400 is an upstream acceptance.

Command, from `/Users/ziy/Code/ClayCosmos/Halro`:

```sh
go test -count=1 -overlay=/private/tmp/halro-provider-adjudication-260905/overlay.json ./internal/gateway -run '^TestAdjudicationPROV01$' -v > /private/tmp/halro-provider-adjudication-260905/run2.log 2>&1
```

- Toolchain: `go version go1.26.6 darwin/arm64` (version command exit 0).
- First attempt, same selector redirected to `run1.log`: **exit 1**, temporary test compilation error because reviewer treated `Registry.Resolve`'s bool as an error. Harness mistake, not product failure or sandbox restriction. Corrected only temporary source.
- Final command: **exit 0**; six subtests pass, test body 0.64 s, package 2.127 s. The four defect cases assert the faulty baseline behavior, not desirable regression behavior.
- No cache/loopback denial occurred in this adjudication; no escalation was needed. Parent's prior environment failures are not attributed to this result.
- No full gates, sockets, billable providers, browser, SDK run, production data or Git mutation. `git diff --name-only -- internal` returned no changed paths.

## Scope/severity decision and repair acceptance

**CONFIRMED:** production-profile unary OpenAI Chat after HTTP 200, covering malformed JSON, absent required envelope fields, read failure and response-size rejection. The original lower-level fixture is representative of this response path, and the stronger independent metered-price fixture preserves the result.

**Source-supported siblings, not independently confirmed end to end:** OpenAI Responses JSON parsing at `adapter.go:472-473` and shared `postJSON` failures; embedding body/JSON/envelope failures at `:577-589`; OpenAI-family profiles that use those branches. Their flags warrant the same repair audit. Do not extend the test result to every Azure/MiniMax/Kimi/Mantle operation without route-specific validation. MiniMax has a separate `base_resp` classifier; Mantle Responses streaming and its pre-first-event behavior remain **UNVERIFIED here**. PROV-02 is outside this adjudication.

**Not part of the finding:** local request encoding/validation failures, authoritative HTTP refusals, discovery/probes that do not generate billable output, and semantic normalization errors already marked ambiguous. The comment in ADR 0005 allowing bounded fallback before the first emitted stream event does not establish that a unary successful HTTP response with unreadable content was unexecuted. The existing conservative ambiguity rule is explicit in `service.go:2874-2883` and reaffirmed by ADR 0022's accounting discussion.

P1 is retained because the normal post-acceptance failure path violates both INV-03 and INV-06 and defeats intended conservative accounting within default retry limits. Severity does not rely on hostile responses or a demonstrated charge from a real account. It is not P0: the attempts are bounded and recorded, effective admission/circuit controls remain, and this test establishes neither irreversible ledger corruption nor an unbounded charge. If all attempts are capped at one, repeat execution is mitigated but the zero-settlement error remains; stopping affected traffic avoids further exposure. No operational changes were made.

Proposed fix acceptance, not implemented:

- Mark post-acceptance unusable unary inference results ambiguous at their producer; keep genuinely unsent/definitively refused and non-billable operations distinct.
- Convert the four defect reproductions into permanent real-adapter + metered gateway regression tests expecting one call, released reservation and conservative estimated cost. Retain the definitive-refusal control expecting one call and zero cost.
- Exercise a second eligible fallback target and verify ambiguity prevents both same-target retry and fallback; add per-profile Responses/embeddings coverage.
- Verify attempt events retain explicit uncertainty/estimated cost and useful failure evidence. A zero settled balance does not mean the request was never recorded; repair descriptions should say the **possible cost is incorrectly treated as zero**, not that all execution evidence disappears.

Independent adjudication complete for the stated scope. Owner remains Provider adapter maintainer with gateway/accounting regression ownership. Recommendation: treat PROV-01 as an unresolved P1 before release; this is a scoped finding adjudication, not a whole-repository release verdict.
