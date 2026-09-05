# Provider / protocol independent review — 2026-09-05

Baseline: `381743f6613607dc256828f4776b52af8bdd232c` (HEAD verified). Reviewer: provider role. Review only; production source, tests, Git state and user data were not changed. Only this report is written inside the repository; reproduction code/logs live under `/private/tmp/halro-provider-review-260905`. No other current reviewers' reports were read. AGENTS.md and the approved review plan were read. No memories or real provider calls were used.

## Outcome and evidence boundary

- **PROV-01: P1, high confidence, locally confirmed:** malformed successful OpenAI-family unary responses can be retried and settled at zero cost. Actual gateway reproduction: **two outbound calls, committed=0, reserved=0**. Independent adversarial adjudication by a second reviewer is still required; this report does not claim that step is complete.
- **PROV-02: P2, high confidence, locally confirmed:** CR-only SSE events are buffered until LF/EOF, defeating incremental streaming and potentially reaching a false line-size limit.
- INV-05 has positive current source and focused fixture evidence. INV-03/06 has the confirmed PROV-01 violation. INV-08 has the PROV-02 boundary failure. INV-10 coverage distinguishes registered implementation from profiles offered for new configuration.
- Release recommendation from this scope: **do not release with PROV-01 unresolved**, subject to independent adjudication. This is not a whole-repository release verdict.
- No fresh upstream response capture, real credential validation, paid model execution, cloud deployment, full SDK execution, browser acceptance, crash campaign or soak was performed by this role. Parent owns full Go/frontend gates, SDK execution, real binary/browser and soak. Parent-reported frontend success and Go environmental rerun are coordination context, not independently verified evidence here.

## Findings

### PROV-01 — An accepted response that cannot be decoded is treated as safe to retry

- Type/status: confirmed BUG / locally CONFIRMED, pending independent P1 adjudication.
- Owner: provider adapter maintainer, with gateway/accounting regression coverage.
- Primary location: `internal/provider/openai/adapter.go:411-415`; additional occurrences at `:472-473`, `:515-521`, `:577-589`.
- Entry/reachable trigger: authenticated normal `POST /v1/chat/completions` routed to an enabled, capability-qualified OpenAI-family deployment; upstream accepts the POST and returns HTTP 200 with truncated JSON, unreadable body, oversized body, or missing required response fields. No unusual gateway configuration is required. A provider/edge failure after execution is enough; an untrusted compatible upstream can deliberately return it.
- Expected: no authoritative refusal exists after acceptance, so mark the execution uncertain, conservatively settle it, and do not automatically repeat it (INV-03/06).
- Actual: these branches construct `provider.Error{Class: ErrorMalformed}` without `Ambiguous: true`. `legacyGenerationPrimitive.Generate` at `internal/provider/primitive.go:175-177` returns that error unchanged. `internal/gateway/service.go:2478-2489` treats non-ambiguous malformed errors as retryable even when `Retryable` is false. `:2898-2902` settles such an error with zero committed cost. The ordinary generation loop at `:1194-1200` / `:1262` therefore settles and repeats an already accepted request.
- Reproduction: overlay `TestReviewMalformedUnaryRetries` constructs the real OpenAI adapter and profile bridge, authenticates through the existing gateway fixture, and returns HTTP 200 with `{"id":"generated-but-truncated"` from an in-memory RoundTripper. With default ServiceOptions the log records `calls=2 error=provider request failed committed=0 reserved=0`. The test asserts the observed defect, not the desired fixed behavior. All credentials and ledger data are synthetic and temporary.
- Impact: duplicate upstream execution/charges and an unrecorded possible upstream cost. This is not a claim that the local fake upstream billed money, nor that an attacker can exceed an arbitrary number of attempts: configured attempt ceilings still apply. The risk is repeated accepted execution within those ceilings, and repeated requests whose failed attempts consume no monetary balance.
- Existing defenses checked: authentication, capability filtering, admission and reservation are present; attempt count bounds repetition. Transport failures already use `!provider.Unsent(err)`. Semantic conversion errors after a successful adapter decode already set `Ambiguous: true` (`primitive.go:181` and OpenAI `adapter.go:477`). Those protections do not run on early read/JSON/required-field failures. The existing gateway `TestAmbiguousProviderFailureIsEstimatedAndSettled` uses an error already marked ambiguous and cannot catch this adapter-origin omission.
- Scope: direct reproduction covers unary Chat through the real bridge/gateway. The common OpenAI adapter also serves Azure, compatible, DeepSeek, MiniMax/Kimi Chat and Mantle Chat. Source shows equivalent missing ambiguity in unary Responses JSON decode and embeddings; these additional operations were not independently reproduced end-to-end here.
- Related candidate, **not separately confirmed**: Mantle Responses streaming uses `state.started` as ambiguity for malformed data/EOF before `response.created` (`internal/provider/bedrockmantle/adapter.go:277-294`). A successful HTTP status with malformed first event is also possibly executed, even before an event was observed. Include this in the fix audit and second-reviewer reproduction; do not infer that all first-event errors are authoritative refusals.
- Minimal repair proposal (not implemented): mark post-acceptance body/decoding failures ambiguous consistently; preserve distinctions for local preflight errors, non-billable discovery and authoritative refusals. Add a gateway regression using real adapter+fake transport asserting one call and conservative settlement for JSON truncation/read failure/oversize across Chat, Responses and embeddings. Do not “fix” by disabling all fallback.
- Severity/confidence: P1 / high for the reproduced route; same-root sibling coverage is source-based. Independent reviewer: pending parent assignment.

### PROV-02 — CR-only SSE framing waits for EOF instead of delivering a completed event

- Type/status: confirmed BUG / locally CONFIRMED.
- Owner: SSE/protocol maintainer.
- Location: `internal/sse/sse.go:91-117`, specifically `ReadSlice('\n')` at `:94` before splitting CR at `:117`.
- Entry/reachable trigger: any configured SSE provider emits `data: one\r\r` and flushes but keeps the stream open. OpenAI, Anthropic, Gemini and Mantle Responses all use this decoder. The package's own documented contract and regression test explicitly include bare CR line terminators.
- Expected: the blank CR-terminated line completes the event; `Next()` returns without waiting for a future LF or connection close.
- Actual: `fill()` waits for LF/EOF first. The pipe reproduction writes the complete event, observes no result for 150 ms, then closes the writer and receives the event immediately. The timeout is only the test observation window; source explains the unbounded wait until another delimiter/EOF or upstream cancellation. A long CR-only sequence can also hit `maxBytes` for the aggregate chunk even though its individual lines/events are small.
- Existing defenses: byte bounds limit memory; transport/request cancellation eventually releases the read; CRLF/LF work. `TestBareCarriageReturnIsALineTerminatorOnBothSides` passes because its decoder input eventually includes LF and is backed by a finite reader. It does not test delivery before EOF. Canonical stream validation cannot repair a frame the decoder has not delivered.
- Impact: delayed output, timeouts, or false oversized-line errors on this valid framing variant. No claim that a listed real provider currently uses CR-only framing; compatibility endpoints can do so and the low-level contract promises it.
- Reproduction: overlay `TestReviewCRStreamBlocksUntilEOF`, same command/log as PROV-01. No sockets or network.
- Minimal repair proposal (not implemented): recognize CR/LF incrementally, coalesce CRLF across read boundaries, apply limits to actual lines/events. Add an open-pipe early-delivery test, split-CRLF case, and many small CR-only events exceeding the aggregate limit while individually bounded.
- Severity/confidence: P2 / high. Independent adjudication not performed.

## Coverage and defenses

This is a targeted source/branch review, not a claim of line-by-line certification of every file in these large packages. Every assigned family is inventoried below; deep review centered on reachable transport, evidence and parser boundaries. Test sources were inspected selectively; existing tests are not fresh upstream captures.

| Scope | Reviewed paths / depth | Result / limitations |
|---|---|---|
| Provider dispatch/profile contracts | `provider/profile.go`, `profile_bindings.go`, `primitive.go`, `provider.go`; app adapter builders and gateway call sites for reachability | Explicit operation bindings; capability ceilings checked against operations. Deep trace of error propagation for PROV-01. Registry concurrency implementation was not independently race-tested. |
| OpenAI family | `provider/openai/adapter.go`, `minimax.go`, `inference_resources.go`; responses/profile, MiniMax, DeepSeek and Mantle-wire test sources | Unary/stream, URL/profile routing, HTTP errors, model catalog, resource transport sampled. PROV-01. No paid smoke. |
| Anthropic family | `provider/anthropic/adapter.go`, `batches.go`; catalog, native, usage and batch tests | Enumeration, metadata mapping, raw preservation, lifecycle and inline batch conversion reviewed. Request count/line bounds, duplicate custom IDs and fixed result URL construction exist. Resource persistence/authorization belongs to parent/other scopes. |
| Gemini | `provider/gemini/adapter.go`; enumeration and adapter test sources | Pagination bounded to 20 pages, reviewed generation-method claims, text-only mapping, usage and EOF reviewed. Gateway finalizer prevents truncated streams becoming success. No new Gemini transport reproduction. |
| Bedrock Runtime/Agent | `provider/bedrock/adapter.go`, `models.go`, `eventstream.go`, `inference_resources.go`, embedding/signer entry points and tests | Withheld profiles included. Frame length/CRC, lifecycle, model pins, modality/streaming claims and endpoint/signing boundaries reviewed selectively. Full SigV4 vector verification and AWS behavior delegated/unverified here. |
| Mantle | `provider/bedrockmantle/adapter.go`, OpenAI/Anthropic Mantle branches and app builders | Route prefix distinction, project header, catalog vs inference and stream lifecycle reviewed; focused route test passes. First-event ambiguity noted under PROV-01 as candidate. |
| Catalog | `modelcatalog/catalog.go`, `manager.go`, `snapshot.go`, `trust.go`; builtin entries/fixture expectations; `catalog/README.md`, unsigned example | Exact identity, conflict/ceiling semantics, signed schema/pin/trust/expiry/sequence checks and LKG activation reviewed. Source tree has an unsigned authoring example, not a production signed artifact. Runtime publication/trust-root provisioning not validated. Builtin factual model claims were sampled, not freshly verified upstream. |
| Domain capability/profile | `domain/provider_profile.go`, `provider_table.go`, `invocation_target.go`, provider connection definitions and detection definitions; `provider/capability_detection.go` | Source-tier ceiling, expiring provider/probe claims, immutable deployment snapshots, withheld vs registered distinction. No whole-domain ownership claim. |
| Protocol/semantic | `openaiapi/types.go`, `responses.go`, resource DTOs; `anthropicapi/types.go`, `stream.go`; `semantic/request.go`, `content.go`, `result.go`, `event.go` | Strict request decoders, native duplicate-member guard, usage subsets and canonical stream finalization. PROV-02; numerical extremes and arbitrary output schemas not exhaustively fuzzed. |
| Compatibility | `compatibility/profile.go`, `manifest.go`, `native.go`, provider-field and family constraints, OpenAI/Anthropic mappings; manifest and SDK test sources | Endpoint/profile matrix below. Strict unsupported/lossy fields and provider-tool capability separation exist. Not every value-dependent field combination independently executed. |
| External SDK harness | all three client scripts and `tests/compatibility/server/main.go` routes | Static review only; parent runs SDKs. Harness serves Chat, Responses, Messages, count_tokens and embeddings, using controlled adapter behavior rather than each real provider transport. |

INV-05 positive evidence: OpenAI-shaped descriptors use `MetadataSourceNone`; custom compatible model names leave canonical model mapping empty. Anthropic mapping is called only for binding-scoped descriptors marked `MetadataSourceProvider` at `internal/app/admin_invocation_targets.go:630`. A manually typed model cannot acquire endpoint-implied Anthropic chat/streaming claims through that call site. Gemini/Bedrock map actual metadata fields. `CapabilityClaim.Validate` requires provider/probe expiry and bounds source evidence; `ActiveAt` excludes expired claims from new resolutions. Existing deployment snapshots intentionally remain unchanged on expiry; that is documented behavior, not a discovered expiry bypass.

Historical source comments describe MiniMax's prior bundled-list error, Kimi Responses measurements and old Mantle routing. Current evidence verifies the code now selects the shared catalog decoder for MiniMax/Kimi Anthropic, withholds Kimi Responses from new writes, and uses the two Mantle operation prefixes. It does **not** re-establish the old accounts' model lists or current provider behavior. Historical 260903 progress entries were only consulted for relevant scope; their “fixed” labels were not accepted as verification.

## Endpoint coverage

The following inventory is extracted from the baseline checked-in manifest and compared with `internal/app/runtime.go:1522-1558` and `compatibility/profile.go:49-55`. It records implemented combinations; **a profile listed here may be withheld for new configuration**. All 23 manifest routes have matching registrations (parameter names differ harmlessly). Deferred resource routes are Halro-local state, not upstream Responses storage. Realtime/HA are not implemented endpoint claims and are not counted as defects.

Profile aliases for the table: C = the 19 registered generation profiles (all except media and the four specialized Bedrock resource/embedding profiles); E = OpenAI Chat, Azure Chat, OpenAI-compatible, Gemini, Titan Embed. W marks withheld profiles in the profile table below. Generation profiles support northbound portable Chat/Responses/Messages subject to declared field/value restrictions; a native Messages route is a separate, explicitly selected mode.

| Method/path | Manifest maturity | Profile coverage / SDK claim |
|---|---|---|
| POST `/v1/chat/completions` | compatible | C (includes withheld Converse and Kimi Responses); openai-go, openai-node, openai-python |
| POST `/v1/embeddings` | compatible | E (Titan Embed withheld); openai-go, openai-node, openai-python |
| POST `/v1/responses` | compatible | C (includes withheld Converse and Kimi Responses); openai-go, openai-node, openai-python |
| POST `/v1/messages` | compatible | C (includes withheld Converse and Kimi Responses); anthropic-go, anthropic-typescript, anthropic-python |
| POST `/v1/messages/count_tokens` | compatible | `anthropic.messages.2023-06-01`; anthropic-go, anthropic-typescript, anthropic-python |
| GET `/v1/responses/{id}` | experimental | C (includes withheld Converse and Kimi Responses); no SDK claim |
| POST `/v1/responses/{id}/cancel` | experimental | C (includes withheld Converse and Kimi Responses); no SDK claim |
| DELETE `/v1/responses/{id}` | experimental | C (includes withheld Converse and Kimi Responses); no SDK claim |
| POST `/v1/moderations` | experimental | `openai.media-resources.v1`; no SDK claim |
| POST `/v1/images/generations` | experimental | `openai.media-resources.v1`, `bedrock.runtime.invoke.titan-image-v2.v1`; no SDK claim |
| POST `/v1/audio/transcriptions` | experimental | `openai.media-resources.v1`; no SDK claim |
| POST `/v1/audio/speech` | experimental | `openai.media-resources.v1`; no SDK claim |
| POST `/v1/files` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| GET `/v1/files/{id}` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| GET `/v1/files/{id}/content` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| DELETE `/v1/files/{id}` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| POST `/v1/batches` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| GET `/v1/batches/{id}` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| POST `/v1/batches/{id}/cancel` | experimental | `openai.media-resources.v1`, `anthropic.messages.2023-06-01`; no SDK claim |
| POST `/v1/rerank` | experimental | `bedrock.agent-runtime.rerank.cohere-v3-5.v1`; no SDK claim |
| POST `/v1/async/invocations` | experimental | `bedrock.runtime.async.nova-reel-v1.v1`; no SDK claim |
| GET `/v1/async/invocations/{id}` | experimental | `bedrock.runtime.async.nova-reel-v1.v1`; no SDK claim |
| POST `/v1/async/invocations/{id}/cancel` | experimental | `bedrock.runtime.async.nova-reel-v1.v1`; no SDK claim |

Native Messages is available on direct Anthropic, Mantle Anthropic, MiniMax Anthropic and Kimi Anthropic. `count_tokens` is restricted to direct Anthropic by the manifest; do not infer support from an adapter having a method. Unary-only OpenAI/MiniMax Responses (and withheld Kimi Responses) do not gain streaming just because a northbound route accepts a stream parameter. Streaming tools/reasoning restrictions on northbound Responses remain documented limitations. Rerank/async routes are registered but their only provider profiles are withheld; async cancellation deliberately fails closed.

## Profile coverage, including withheld/experimental

| Profile ID | Southbound operation(s) | Enumeration / capability evidence | Availability in this build |
|---|---|---|---|
| `openai.chat-embeddings.v1` | `/v1/chat/completions` unary+SSE; embeddings | models API; no capabilities from identifiers | offered |
| `openai.responses.v1` | `/v1/responses` unary | models API; catalog/probe/declaration | offered; no stream binding |
| `anthropic.messages.2023-06-01` | Messages unary+SSE, token count, inline batches; local files | paginated Models; provider metadata behind provenance gate | offered; files/batches experimental |
| `azure-openai.chat-embeddings.v1` | deployment-scoped chat+SSE/embeddings, api-version | data-plane credential cannot enumerate management deployments; explicit deployment/canonical mapping | offered |
| `deepseek.chat.v1` | chat+SSE, narrowed fields | models API, catalog/probe | offered |
| `openai-compatible.chat-embeddings.v1` | configured base path chat+SSE/embeddings | models API; explicit canonical mapping, no name inference | offered |
| `gemini.generate-content.text.v1beta` | generateContent, streamGenerateContent, embedContent | paginated models and supportedGenerationMethods | offered Beta/text subset |
| `bedrock.runtime.converse.text.v1` | converse/converse-stream | control-plane foundation-models, reviewed metadata; allowed-host boundary | **withheld** |
| `bedrock.runtime.invoke.titan-embed-text-v2.v1` | pinned Titan invoke embedding | fixed profile model, not account entitlement measurement | **withheld**, experimental |
| `openai.media-resources.v1` | moderation/image/transcription/speech/files/batches | models identifiers; separate operation capabilities | offered, experimental |
| `bedrock.runtime.invoke.titan-image-v2.v1` | pinned Titan image invoke | fixed profile model | **withheld**, experimental |
| `bedrock.agent-runtime.rerank.cohere-v3-5.v1` | agent-runtime rerank | fixed profile model | **withheld**, experimental |
| `bedrock.runtime.async.nova-reel-v1.v1` | start/get async; cancel refused | fixed profile model; explicit S3 output | **withheld**, experimental |
| `bedrock.mantle.chat.v1` | `/v1/chat/completions` unary+SSE | `/v1/models`; catalog partitions model routes | offered |
| `bedrock.mantle.openai.chat.v1` | `/openai/v1/chat/completions` unary+SSE | same catalog; distinct route | offered |
| `bedrock.mantle.responses.v1` | `/v1/responses` unary+SSE | same catalog, no metadata-derived capabilities | offered, canonical subset |
| `bedrock.mantle.openai.responses.v1` | `/openai/v1/responses` unary+SSE | same catalog; distinct route | offered, canonical subset |
| `bedrock.mantle.anthropic.messages.v1` | `/anthropic/v1/messages` unary+SSE | adapter cannot enumerate; catalog offers/manual entry | offered |
| `minimax.anthropic.messages.v1` | `/anthropic/v1/messages` unary+SSE | `/v1/models` OpenAI decoder; no endpoint-implied capability claims | offered |
| `minimax.chat.v1` | chat+SSE, base_resp guard, EOF after finish accepted | `/v1/models`, catalog/probe | offered |
| `minimax.responses.v1` | Responses unary | `/v1/models`, catalog/probe | offered; unasked reasoning behavior remains unverified |
| `kimi.chat.v1` | chat+SSE, model-specific thinking switch | `/v1/models`, catalog/probe | offered |
| `kimi.anthropic.messages.v1` | `/anthropic/v1/messages` unary+SSE | `/v1/models` OpenAI decoder, no implied claims | offered |
| `kimi.responses.v1` | Responses unary implemented | models API implemented | **withheld**; historical unasked reasoning motivated withholding |

Withholding is enforced on new writes and Admin profile presentation (`admin_provider_profiles.go:108`, `admin_providers.go:1093,1305,1448`). Reads of old records remain possible deliberately. Consequently “registered” is not “offered”, and withholding is not proof that an old deployment can never execute. The compatibility manifest still lists withheld profiles without a withholding field: **contract/presentation gap, not a confirmed runtime bypass**. Proposal: expose deployment availability separately from wire maturity while retaining legacy contract descriptions.

## Tests mapped to invariants and command record

Environment: `go version go1.26.6 darwin/arm64`; repository working directory as above. `git status --short` showed only shared review-document changes (`docs/review/README.md`, untracked `docs/review/260905/`) when checked. No golden update mode was used. Tests use synthetic credentials, in-memory RoundTrippers and temporary fixture data. No paid smoke test name was selected.

| Evidence | INV mapping | Result |
|---|---|---|
| `TestReviewMalformedUnaryRetries` | INV-03, INV-06 | defect reproduced through real adapter/bridge/gateway; 2 calls, zero cost |
| `TestReviewCRStreamBlocksUntilEOF` | INV-08, INV-10 | defect reproduced using io.Pipe; event withheld before EOF |
| Unknown model, merge conflict, signed version/expiry/pin tests | INV-05 | pass; unknown remains without capability, conflicting support fails closed |
| Running catalog expiry and sequence checkpoint replay tests | INV-05, catalog portion of INV-09 | pass; expired catalog stops resolving, checkpoint survives LKG loss |
| MiniMax enumeration/direct Anthropic provenance fixtures | INV-05 | pass; enumeration and capability metadata remain separate |
| Responses profile/route and operation ceiling tests | INV-10 | pass; compiled operation bindings agree with ceilings and selected routes |
| Native duplicate-member, unknown/lossy field and native schema tests | INV-04/05 boundary contribution, INV-10 | pass; not a full project authorization test |
| Semantic stream validator identity/termination/tool limits | INV-06, INV-08 | pass; downstream compensation checked for early EOF/sentinel |

Commands below were run with output redirected directly to logs, and process exit codes obtained from the tool (not a pipeline). No cache/loopback environmental failure occurred in this role, so no escalation was needed. Parent's environmental failure is separate. Package elapsed times are in raw logs; wall-clock compilation time was not separately instrumented.

1. `go test -count=1 -overlay=/private/tmp/halro-provider-review-260905/overlay.json ./internal/gateway -run '^TestReview' -v` → initial exit **1**, fixture setup error: `target declares no operation capability`; CR test passed. This was a reproduction-harness error, not a product failure or environment failure. Added explicit `Capabilities{Chat:true}` **only in the temporary overlay**; identical command → exit **0**, package 1.075s. Final log: `/private/tmp/halro-provider-review-260905/repro.log`. Initial output is transcribed here; it was overwritten by final log.
2. `go test -count=1 ./internal/modelcatalog ./internal/provider/anthropic ./internal/provider/openai ./internal/provider/bedrockmantle ./internal/compatibility ./internal/semantic ./internal/sse -run '^(TestUnknownModelGetsNoCapabilities|TestMergeFailsClosedOnConflict|TestSignedSnapshotRejectsVersionExpiryPinAndUnknownCapability|TestSequenceCheckpointRefusesReplayAfterLastKnownGoodLoss|TestRunningManagerStopsResolvingFromCatalogAtExpiry|TestMiniMaxEnumeratesFromTheOpenAIShapedCatalogue|TestDirectAnthropicStillClaimsChatFromItsOwnCatalogue|TestMiniMaxProbeRefusesAJSONObjectThatIsNotAModelList|TestResponsesProfileResolvesToTheSemanticPrimitive|TestChatProfileAdapterRefusesTheResponsesSurface|TestResponsesAdapterAddressesTheRouteTheProfileFixed|TestResponsesAdapterValidatesStreamLifecycleAndUsage|TestBareCarriageReturnIsALineTerminatorOnBothSides|TestManifest.*|Test.*StreamValidator.*)$' -v` → exit **0**. Log: `/private/tmp/halro-provider-review-260905/focused.log`; package times 1.632–3.967s. This is a selected test set, not full package gates.
3. `go test -count=1 ./internal/provider ./internal/domain ./internal/anthropicapi ./internal/openaiapi ./internal/compatibility/anthropic ./internal/compatibility/openai -run '^(TestCeilingWithinProfileManifestOperations|TestManifestOperationsNotOfferedByCeiling|Test.*Duplicate.*|Test.*Native.*|Test.*Usage.*|Test.*Unknown.*)$' -v` → exit **0**. Log: `/private/tmp/halro-provider-review-260905/boundaries.log`. The regex incidentally selected a few domain pricing tests; it was not a whole-domain run. `compatibility/openai` selected **no tests**, so that package's cached/build status is not cited as test coverage.

No full Go/frontend/race suite, SDK command or real-provider smoke was executed by this role. No second full gate is requested here.

## Test gaps, proposals and limitations

- Add post-HTTP-acceptance error tests that instantiate real provider adapters inside gateway fixtures. Adapter-only error-class tests and gateway fake errors marked ambiguous each miss the composition in PROV-01.
- Exercise CR-only incremental streams before EOF; current finite-reader/fuzz corpus examples do not establish timely delivery. Further tests should bound line/event size independently and cover cancellation while waiting.
- Catalog envelope validation is inconsistent: OpenAI listing ignores the shared decoder's `listed` result (`openai/adapter.go:287`), and Mantle/Gemini/direct-Anthropic lists can interpret `{}` as an empty list. The shared decoder treats `data:null` as present. Probe methods also differ in how much a 200 body is validated. **Source-observed hardening/test gap:** add absent/null/wrong-envelope cases and decide whether a successful refresh may replace prior visible models. No end-to-end cache-loss finding is claimed here.
- Manifest compatibility does not prove an SDK×every-provider matrix. Current SDK clients target one controlled harness; Node/Python exercise Anthropic streams, Go only Anthropic unary; no client calls count_tokens despite its SDK claim. Deferred retrieve/cancel/delete, native betas, extended tool blocks and experimental resources lack coverage in these client scripts. Parent SDK success must be scoped accordingly.
- Profile metadata is generated from shared tables: invariants catch missing rows and operation capability mismatches, but cannot alone catch two valid primitives swapped between profiles. Route-specific transport fixtures remain necessary, including withheld profiles before promotion.
- Catalog signatures, expiry and rollback behavior were tested locally. Production signed artifact availability, release trust roots, actual account entitlements and real response shapes were not verified. Pinned Bedrock model offers should not be interpreted as measured account availability.
- MiniMax Responses unasked reasoning and exact current provider capabilities remain explicitly unverified; Kimi withholding is current code evidence, not a fresh replay of the historical measurements. No inference from one provider/route to another is accepted.
- Native request duplicate keys are rejected, whereas canonical OpenAI request decoding re-renders parsed data. Those are different trust boundaries; the absence of the same raw-preservation guard on OpenAI is not itself a bypass finding.
- Full numerical-overflow adversarial tests, fuzz campaigns, signing edge cases, large multipart/resource lifecycle and retained-object isolation were not completed in this role. They remain gaps or other owners' work, not implicit passes.
- INV-01/02/07 and full INV-04/09 accounting/storage/authorization proofs are outside this role. Only the relevant provider/catalog contributions are covered above.

## Reproduction source (temporary Go overlay)

This source was added virtually at `internal/gateway/review_provider_overlay_test.go`; the real test tree is unchanged. Run against the baseline with the overlay map shown after the source. The test intentionally passes when the reviewed bad behavior exists, so invert expectations for a permanent regression test.

```go
package gateway
import (
 "context"
 "io"
 "net/http"
 "net/url"
 "strings"
 "testing"
 "time"
 "github.com/akz142857/Halro/internal/domain"
 "github.com/akz142857/Halro/internal/provider"
 oa "github.com/akz142857/Halro/internal/provider/openai"
 "github.com/akz142857/Halro/internal/sse"
)
type reviewTransport func(*http.Request)(*http.Response,error)
func (f reviewTransport) RoundTrip(r *http.Request)(*http.Response,error){return f(r)}
func TestReviewMalformedUnaryRetries(t *testing.T){
 f:=newFixture(t,10000); defer f.close()
 calls:=0
 endpoint,_:=url.Parse("https://review.invalid")
 a,err:=oa.New(endpoint,[]byte("fake-review-key"),&http.Client{Transport:reviewTransport(func(r *http.Request)(*http.Response,error){calls++; return &http.Response{StatusCode:200,Header:http.Header{},Body:io.NopCloser(strings.NewReader(`{"id":"generated-but-truncated"`)),Request:r},nil})})
 if err!=nil{t.Fatal(err)}; defer a.Close()
 manifest,_:=provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
 bridge,err:=provider.NewLegacyAdapterBridge(a,manifest,nil); if err!=nil{t.Fatal(err)}
 reg:=provider.NewRegistry()
 if err=reg.Register(provider.Target{ID:"review-target",DeploymentID:"review-dep",PublicModel:"chat",ProviderModel:"provider-model",Adapter:bridge,Capabilities:provider.Capabilities{Chat:true},InputMicrosPerMillion:1000000,OutputMicrosPerMillion:2000000});err!=nil{t.Fatal(err)}
 svc,err:=NewServiceWithOptions(f.service.auth,reg,f.accounting,ServiceOptions{});if err!=nil{t.Fatal(err)}
 _,err=svc.Chat(context.Background(),f.plaintext,chatRequest())
 b:=f.state.Balance(f.project.ID,time.Now().UTC().Format("2006-01-02"),testTimezoneVersion)
 t.Logf("calls=%d error=%v committed=%d reserved=%d",calls,err,b.CommittedMicrosUSD,b.ReservedMicrosUSD)
 if calls<=1 || b.CommittedMicrosUSD!=0 || err==nil{t.Fatalf("candidate not reproduced")}
}
func TestReviewCRStreamBlocksUntilEOF(t *testing.T){
 r,w:=io.Pipe(); defer r.Close(); defer w.Close()
 result:=make(chan error,1)
 go func(){e,err:=sse.NewDecoder(r,1024).Next();if err==nil && string(e.Data)!="one"{t.Errorf("wrong payload %q",e.Data)};result<-err}()
 if _,err:=w.Write([]byte("data: one\r\r"));err!=nil{t.Fatal(err)}
 select{case err:=<-result:t.Fatalf("candidate not reproduced, decoder returned %v",err);case <-time.After(150*time.Millisecond):t.Log("complete CR-delimited event is withheld while upstream stays open")}
 w.Close()
 select{case err:=<-result:if err!=nil{t.Fatal(err)};t.Log("same event delivered after EOF");case <-time.After(time.Second):t.Fatal("decoder did not finish")}
}
```

```json
{"Replace": {"/Users/ziy/Code/ClayCosmos/Halro/internal/gateway/review_provider_overlay_test.go": "/private/tmp/halro-provider-review-260905/review_test.go"}}
```
