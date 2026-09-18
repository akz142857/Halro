# Application role — E1 evidence survey for G2 (真实 Provider primitive 矩阵) + G5 application-side load model

- Plan executed: `docs/verification/production-validation-plan.zh-CN.md` (G2 at `:120`–`:136`, G5 at `:165`–`:184`)
- Candidate SHA surveyed: `f09ed2d768bf2cd335478223beb4d57af326013b` (working tree carries only uncommitted `docs/` edits)
- Survey date: 2026-09-18
- Method: static read of the repository only. **No binary was built, no test was executed, no provider was called.** Every row below is therefore itself E1 about the repository's *contents*; where it asserts what a test does, that assertion is read from source, not from a run. This is called out because `CLAUDE.md` ("Verify, never assume") treats an unrun test as a hypothesis.

---

## 0. What "officially declares" means here — and why the question is not yet well-posed

The plan's G2 pass criterion is "每项正式声明的能力都有 E4 正反例" and "未执行真实验证的 profile 必须从本次生产声明中移除". That requires a single, unambiguous list of what is declared. **There are four lists in the tree and they do not agree.**

| List | Location | Contents |
|---|---|---|
| The authoritative code table | `internal/domain/provider_table.go:172`–`:573` | 34 profile rows, 10 of them `Withheld` → **24 offered** |
| The public README claim | `README.md:280`–`:290` | 9 vendor rows. **No BigModel/Z.AI row at all; no MiniMax Subscription row at all** |
| The repo instructions | `CLAUDE.md` "What this is" | same 9 vendors as README — BigModel absent |
| The machine-readable contract | `docs/compatibility/endpoint-manifests.json` | publishes `profile_coverage` for **withheld** profiles as well (see §1.3) |

So four *offered, write-path-reachable* profiles — `bigmodel.cn.chat-embeddings.v1` (`internal/domain/provider_table.go:236`), `bigmodel.global.chat.v1` (`:244`), `bigmodel.cn.coding.chat.v1` (`:262`), `bigmodel.global.coding.chat.v1` (`:275`) — plus the two offered MiniMax Subscription profiles (`:544`, `:559`) are served by the binary and absent from the product's own public declaration. **G2 cannot be signed off until the Application owner fixes which list is "正式声明".** This report enumerates from `provider_table.go`, because that is what the write path actually enforces.

Second definitional gap: the tree also carries a **GA / Beta** split that the plan does not model. `docs/verification/provider-real-matrix.md:80` declares Gemini, Bedrock, MiniMax and BigModel "Beta/experimental and therefore do not satisfy or block the GA matrix"; `:357` adds Kimi. There is **no GA/Beta field in `provider_table.go`** — `grep -n "Beta" internal/domain/provider_table.go` returns only three comment lines (`:283`, `:291`, `:631`). So "Beta" is a documentation-only attribute with no machine representation, and the write path treats a Beta profile exactly like a GA one.

---

## 1. Declared profile → wire shape → declared primitives → withheld

Primitives are read from two places and both are cited:
- **operations** (chat / chat-stream / messages / messages-stream / embeddings / media …) from `internal/provider/profile_bindings.go:73`–`:167`;
- **capabilities** (streaming, tools, …) from the `Defaults`/`Ceiling` sets in `internal/domain/provider_table.go`.

`Tools` in the table means the connection-level capability bit, not that any endpoint-level tool-call round trip is proven.

### 1.1 Offered profiles (Withheld = no) — 24 rows

| # | Profile ID | Row | Wire shape / surface | Bound operations (`profile_bindings.go`) | Declared capability bits (`Defaults`) | Withheld |
|---|---|---|---|---|---|---|
| 1 | `openai.chat-embeddings.v1` | `provider_table.go:174` | OpenAI Chat Completions | chat, chat-stream, embeddings — `profile_bindings.go:74` | chat, streaming, embeddings, tools, vision, fetched_image, json_object, structured_outputs, developer_role, reasoning, stream_usage — `provider_table.go:124` | no |
| 2 | `openai.responses.v1` | `:185` | OpenAI Responses (unary only) | chat **only** — `profile_bindings.go:77` | chat, tools, vision, fetched_image, json_object, structured_outputs, developer_role; ceiling adds provider_executed_tools — `:159`, `:190` | no |
| 3 | `anthropic.messages.2023-06-01` | `:193` | Anthropic Messages | chat, chat-stream, messages, messages-stream, files, batches — `profile_bindings.go:82` | chat, streaming, tools, vision, fetched_image, structured_outputs, reasoning, stream_usage, files, batches — `:141` | no |
| 4 | `azure-openai.chat-embeddings.v1` | `:205` | Azure OpenAI | chat, chat-stream, embeddings — `profile_bindings.go:86` | same set as OpenAI chat — `:208` | no |
| 5 | `deepseek.chat.v1` | `:217` | OpenAI-shaped Chat | chat, chat-stream — `profile_bindings.go:89` | chat, streaming, tools, json_object, reasoning, stream_usage; ceiling adds vision+fetched_image — `:578`, `:222` | no |
| 6 | `openai-compatible.chat-embeddings.v1` | `:230` | OpenAI-compatible | chat, chat-stream, embeddings — `profile_bindings.go:103` | chat, streaming, embeddings only — `:618` | no |
| 7 | `bigmodel.cn.chat-embeddings.v1` | `:236` | BigModel Chat (mainland) | chat, chat-stream, embeddings — `profile_bindings.go:91` | chat, streaming, embeddings, tools, json_object, stream_usage; ceiling adds vision/fetched_image/reasoning — `:582`, `:241` | no |
| 8 | `bigmodel.global.chat.v1` | `:244` | BigModel Chat (Z.AI) | chat, chat-stream — `profile_bindings.go:94` | chat, streaming, tools, json_object, stream_usage; same ceiling widening — `:586`, `:249` | no |
| 9 | `bigmodel.cn.coding.chat.v1` | `:262` | GLM Coding Plan (mainland) | chat, chat-stream — `profile_bindings.go:99` | chat, streaming, tools, json_object, stream_usage, reasoning — `:597` | no |
| 10 | `bigmodel.global.coding.chat.v1` | `:275` | GLM Coding Plan (Z.AI) | chat, chat-stream — `profile_bindings.go:101` | chat, streaming, tools, reasoning — `:606` | no |
| 11 | `gemini.generate-content.text.v1beta` | `:284` | Gemini generateContent | chat, chat-stream, embeddings — `profile_bindings.go:106` | chat, streaming, embeddings, developer_role — `:619` | no |
| 12 | `openai.media-resources.v1` | `:315` | OpenAI media/resources | moderations, images, transcriptions, speech, files, batches — `profile_bindings.go:113` | moderations, images, transcriptions, speech, files, batches — `:163` | no |
| 13 | `bedrock.mantle.chat.v1` | `:358` | Mantle `/v1` (OpenAI shape) | chat, chat-stream — `profile_bindings.go:129` | chat, streaming, tools, vision, json_object, structured_outputs, developer_role, reasoning, stream_usage — `:633` | no |
| 14 | `bedrock.mantle.openai.chat.v1` | `:367` | Mantle `/openai/v1` | chat, chat-stream — `profile_bindings.go:131` | same as #13 — `:633` | no |
| 15 | `bedrock.mantle.responses.v1` | `:379` | Mantle Responses `/v1` | chat, chat-stream — `profile_bindings.go:133` | chat, streaming, tools, vision, json_object, structured_outputs, developer_role, stream_usage (no reasoning) — `:638` | no |
| 16 | `bedrock.mantle.openai.responses.v1` | `:388` | Mantle Responses `/openai/v1` | chat, chat-stream — `profile_bindings.go:135` | same as #15 — `:638` | no |
| 17 | `bedrock.mantle.anthropic.messages.v1` | `:397` | Mantle `/anthropic/v1` | chat, chat-stream, messages, messages-stream — `profile_bindings.go:137` | chat, streaming, tools, vision, reasoning, stream_usage — `:643` | no |
| 18 | `minimax.anthropic.messages.v1` | `:419` | MiniMax Anthropic face | chat, chat-stream, messages, messages-stream — `profile_bindings.go:139` | chat, streaming, tools, vision, fetched_image, reasoning, stream_usage — `:680` | no |
| 19 | `minimax.chat.v1` | `:426` | MiniMax Chat | chat, chat-stream — `profile_bindings.go:141` | chat, streaming, tools, vision, fetched_image, json_object, reasoning, stream_usage — `:694` | no |
| 20 | `minimax.responses.v1` | `:433` | MiniMax Responses (unary only) | chat **only** — `profile_bindings.go:145` | chat, tools, vision, fetched_image — `:729` | no |
| 21 | `kimi.chat.v1` | `:458` | Kimi Chat | chat, chat-stream — `profile_bindings.go:147` | chat, streaming, tools, vision, json_object, structured_outputs, reasoning, stream_usage — `:760` | no |
| 22 | `kimi.anthropic.messages.v1` | `:465` | Kimi Anthropic face | chat, chat-stream, messages, messages-stream — `profile_bindings.go:149` | chat, streaming, tools, vision, structured_outputs, reasoning, stream_usage — `:777` | no |
| 23 | `minimax.cn.subscription.openai.chat.v1` | `:544` | MiniMax Subscription (mainland) | chat, chat-stream — `profile_bindings.go:159` | chat, streaming, tools, reasoning, stream_usage — `:615` | no |
| 24 | `minimax.global.subscription.openai.chat.v1` | `:559` | MiniMax Subscription (global) | chat, chat-stream — `profile_bindings.go:163` | same as #23 — `:615` | no |

### 1.2 Withheld profiles (NOT part of the production claim) — 10 rows

| Profile ID | Row | `Withheld:` line | Bound operations |
|---|---|---|---|
| `bedrock.runtime.converse.text.v1` | `provider_table.go:298` | `:302` | chat, chat-stream — `profile_bindings.go:109` |
| `bedrock.runtime.invoke.titan-embed-text-v2.v1` | `:306` | `:311` | embeddings — `profile_bindings.go:111` |
| `bedrock.runtime.invoke.titan-image-v2.v1` | `:323` | `:328` | images — `profile_bindings.go:120` |
| `bedrock.agent-runtime.rerank.cohere-v3-5.v1` | `:332` | `:337` | rerank — `profile_bindings.go:122` |
| `bedrock.runtime.async.nova-reel-v1.v1` | `:341` | `:346` | async_invoke — `profile_bindings.go:124` |
| `kimi.responses.v1` | `:520` | `:524` | chat — `profile_bindings.go:153` |
| `kimi.code.openai.chat.v1` | `:528` | `:532` | chat, chat-stream — `profile_bindings.go:155` |
| `kimi.code.anthropic.messages.v1` | `:536` | `:540` | chat, chat-stream, messages, messages-stream — `profile_bindings.go:157` |
| `minimax.cn.subscription.anthropic.messages.v1` | `:551` | `:555` | chat/stream/messages/messages-stream — `profile_bindings.go:161` |
| `minimax.global.subscription.anthropic.messages.v1` | `:566` | `:570` | same — `profile_bindings.go:165` |

The withholding gate is `domain.IsWithheldProfile` (`internal/domain/provider_profile.go:353`–`:361`), read by the offering filter at `internal/domain/provider_offering.go:633` and asserted by `TestNoProviderTypeDefaultsToAWithheldProfile` (`internal/domain/registration_guard_test.go:46`).

### 1.3 A contradiction between the two declared-lists that G2 has to resolve

`docs/compatibility/endpoint-manifests.json` is named by `README.md:270`–`:271` as "the authoritative machine-readable contract". It publishes `profile_coverage` rows for profiles the write path refuses:

- `kimi.responses.v1` appears as **`"compatible"`** on `openai.chat-completions.v1` (`endpoint-manifests.json:3`), `openai.responses.create.v1` (`:700`) and `anthropic.messages.2023-06-01` (`:1266`) — while `provider_table.go:524` withholds it.
- `bedrock.runtime.converse.text.v1` likewise `"compatible"` on those three endpoints while withheld at `provider_table.go:302`.
- `kimi.code.openai.chat.v1`, `kimi.code.anthropic.messages.v1`, both MiniMax Subscription Anthropic profiles, `bedrock.runtime.invoke.titan-embed-text-v2.v1` (on `openai.embeddings.v1`, `:596`), `…titan-image-v2.v1` (on `openai.images.generations.v1`, `:2539`), `…rerank.cohere-v3-5.v1` (on `halro.rerank.v1`, `:3078`) and `…async.nova-reel-v1.v1` (on `halro.async.*`, `:3122`) all appear as `"experimental"` coverage while withheld.

The only withholding filter in the manifest builder is scoped to the **native Anthropic protocol path**: `internal/compatibility/manifest.go:469` (`if manifest.Protocol == "anthropic" && domain.IsWithheldProfile(alias) { continue }`), with the stated reason at `:465`–`:468` ("its field rules remain reviewable"). That is a deliberate design choice, not an oversight — but for G2 it means **an auditor reading the authoritative machine-readable contract will count profiles that no deployment can be created against**. There is no test reconciling manifest coverage against `Withheld` (searched `internal/compatibility/manifest_test.go`, `profile_registration_test.go`, `platform_checklist_test.go` — `TestEveryChatProfileAppearsInAnEndpointManifest` at `internal/compatibility/profile_registration_test.go:52` enforces the *opposite* direction only).

---

## 2. Evidence class per declared profile + primitive

Legend: **E1** = document/design only. **E2** = automated unit/contract/fake-server test in-repo. **E4** = a real-provider run recorded in `docs/verification/provider-real-matrix.md`.

### 2.0 The headline finding

**Not one offered profile has a recorded E4 run of Halro's own request path in `provider-real-matrix.md`.** The document's opening paragraphs (`docs/verification/provider-real-matrix.md:1`–`:82`) describe the *harness* — what the runner "covers", which env prefixes it needs, how to invoke it. That is E1 by the plan's own definition (`production-validation-plan.zh-CN.md:29`: "E1 | 文档、设计和静态检查"). Searching the document for a recorded GA run returns nothing: there is no result table, no `-commit` binding, no evidence-file digest for `openai`, `anthropic`, `azure_openai`, `openai_compatible`, or `gemini` anywhere in its 1039 lines (`grep -n -i "azure" docs/verification/provider-real-matrix.md` returns only `:7`, `:51`, `:55`, `:56`, `:540`; `grep -n -i "gemini"` returns only `:80`, `:357`, `:536`, `:537`).

This is consistent with `docs/verification/release-assessment.md:10`, which states that "the retired 1.0.0 gates (24h soak, **full real-account Provider matrix**, signed-tag governance) stay retired" for the v0.x line — and `:15`–`:18` explicitly says that retirement "is not evidence that a deployment is production-ready".

### 2.1 Per-profile evidence table

| Profile | chat | streaming | embeddings | responses | messages | tool calls | Notes |
|---|---|---|---|---|---|---|---|
| `openai.chat-embeddings.v1` | **E2** `internal/provider/openai/real_smoke_test.go:98` is the *harness*, unrun → E1; hermetic E2 at `internal/provider/openai/adapter_test.go:198` | **E2** harness `real_smoke_test.go:111`; hermetic adapter tests | **E2** harness `real_smoke_test.go:131` (only when `HALRO_SMOKE_EMBEDDING_MODEL` set) | n/a | n/a | **E1 only** — the GA smoke sends no tool; `grep tool_calls internal/provider/openai/real_smoke_test.go` finds none | no recorded E4 |
| `openai.responses.v1` | **E2** `internal/provider/openai/responses_profile_test.go` | n/a (binds no stream primitive, `profile_bindings.go:77`) | n/a | **E2** | n/a | **E1** | **No smoke of any kind exercises this profile**: `real_smoke_test.go:44`–`:48` accepts only `openai`, `azure_openai`, `deepseek`, `openai_compatible`, and the adapter it builds is the Chat one |
| `anthropic.messages.2023-06-01` | **E2** harness `internal/provider/anthropic/real_smoke_test.go:73`–`:76` (native + portable), unrun | **E2** `:74`, `:76` | n/a | n/a | **E2** `:73` | **E1** — no tool-call subtest in the smoke | count_tokens `:77`, catalog `:78`, probe `:79` also E2-harness |
| `azure-openai.chat-embeddings.v1` | **E2** shares `real_smoke_test.go:33` | **E2** | **E2** | n/a | n/a | **E1** | no Azure-specific negative test at all (§3) |
| `deepseek.chat.v1` | **E4 (partial, adaptation only)** — `provider-real-matrix.md:498`–`:526`, run **2026-08-20 on `13d55ff`**, against `https://api.deepseek.com` with `deepseek-v4-flash`. Quoted at `:502`–`:504`: *"**This is adaptation evidence, not GA matrix evidence**: it carries no `-commit` binding and produced no archived evidence file, so the release gate above is still open for this profile."* | **E2** | n/a | n/a | n/a | **E1** | Established: `thinking` spelling, `none` really disables, cache tiers sum (`:508`–`:517`). Explicitly NOT established: whether `max_tokens` counts the thinking chain (`:519`–`:526`) |
| `openai-compatible.chat-embeddings.v1` | **E2** | **E2** | **E2** | n/a | n/a | **E1** | no recorded E4 |
| `bigmodel.cn.chat-embeddings.v1` | **E1/E2** — `provider-real-matrix.md:175`–`:208` heading reads *"implementation complete, real-account cells not run (2026-09-08)"*; `:190`–`:191`: *"No real key was available for this implementation, so neither regional cell is recorded as passing."* | E2 | E2 | n/a | n/a | E1 | harness `internal/provider/openai/bigmodel_real_smoke_test.go` (`:192`) |
| `bigmodel.global.chat.v1` | **E1** — same section, same "not run" verdict | E2 | n/a | n/a | n/a | E1 | `:166`–`:170` records that only an *unauthenticated* 401 probe of the global `/models` route was made |
| `bigmodel.cn.coding.chat.v1` | **E4 (partial, adaptation only)** — `provider-real-matrix.md:84`–`:173`, *"measured on a real mainland subscription (2026-09-09)"*. No commit SHA is recorded for the run, and no evidence-file digest | **E4** SSE shape measured `:136`–`:143` | **not declared**, deliberately: `:160`–`:163` | n/a | n/a | **E4** — *"Tool calls work and carry the standard shape"* `:145`–`:147` | Also measured: `json_object` and `json_schema` both honoured `:148`–`:150`; `thinking:{"type":"disabled"}` ignored `:152`–`:156`. Not probed: vision, structured outputs `:164` |
| `bigmodel.global.coding.chat.v1` | **E1** — `:166`–`:170`: *"No Z.AI Coding Plan key was available… Its authenticated response shape, model mapping and provider errors remain unmeasured and are not reported here as verified evidence."* | E1 | n/a | n/a | n/a | E1 | |
| `gemini.generate-content.text.v1beta` | **E2** harness `internal/provider/gemini/real_smoke_test.go:27`, unrun | **E2** `:69` | **E2** `:87` (opt-in) | n/a | n/a | **E1** (no tools capability declared; `provider_table.go:619`) | **No Gemini section exists in `provider-real-matrix.md`** — the only three hits are the Beta disclaimer `:80`, a passing mention `:357`, and catalog-metadata rules `:536`–`:537`. **Zero real-account evidence of any kind.** |
| `openai.media-resources.v1` | n/a | n/a | n/a | n/a | n/a | n/a | Six operations (moderations/images/transcriptions/speech/files/batches). Harness `internal/provider/openai/media_smoke_test.go:46` described at `provider-real-matrix.md:36`–`:46`; **no recorded run** |
| `bedrock.mantle.chat.v1` / `…openai.chat.v1` | **service-level E4, Halro-path E1** — `provider-real-matrix.md:669`–`:683`: curl probes over 50 models on 2026-08-21. Explicitly bounded at `:677`–`:682`: *"It is **not** evidence that Halro's own request path reaches that service end to end — the matrix runner still has no Mantle coverage, and `tests/provider-matrix` has still never been run against this endpoint."* | **E1** — `:989`–`:990`: *"No streaming Responses call, no tool call, and no multi-turn conversation was probed. Only single-turn chat and single-turn Responses were."* | n/a | see below | n/a | **E1** — explicitly not probed `:989` | `:994`: *"The harness exists; no run has happened."* |
| `bedrock.mantle.responses.v1` / `…openai.responses.v1` | **service-level E4** (single-turn Responses, `:990`) | **E1** (no streaming Responses call, `:989`) | n/a | as left | n/a | **E1** | |
| `bedrock.mantle.anthropic.messages.v1` | **service-level E4** + a narrow 2026-08-29 A/B on the workspace header (`:932`–`:959`) | **E1** | n/a | n/a | **service-level E4** | **E1** | `:967`–`:974`: *"These were `curl` requests against the service. They say the header name is right; they do not say Halro sends it."* Positive project attribution is explicitly **not established** (`:963`–`:966`) |
| `minimax.anthropic.messages.v1` | **E4 (adaptation only)** — `provider-real-matrix.md:235`–`:302`, international account, 2026-08-31. `:248`–`:250`: *"**This is adaptation evidence, not GA matrix evidence**: MiniMax is Beta, so it never gates a release, and the runs below carried no `-commit` binding and produced no archived evidence file."* | **E4** — a real defect was found and fixed here (`:274`–`:281`) | not declared | n/a | **E4** | **E1** — tools were not among the measured claims (`:257`–`:265`) | Mainland region explicitly **not measured** (`:287`–`:299`) |
| `minimax.chat.v1` | **E4 (adaptation only)** same run | **E4** | not declared | n/a | n/a | **E1** | `json_object` moved into the declaration on this measurement (`:263`, `:684`–`:690`) |
| `minimax.responses.v1` | **E4 (adaptation only)** — `:304`–`:351`, 2026-09-02, five calls. This is a **negative** finding: M2.x models return a `reasoning` output item the decoder refuses, so the seven M2 identifiers were removed from this profile's catalogue (`:336`–`:339`) | n/a (binds no stream primitive) | not declared | as left | n/a | **E1** | Honest limits recorded at `:341`–`:345` |
| `kimi.chat.v1` | **E4 (adaptation only)** — `:353`–`:496`, mainland account 2026-09-01, international host also measured (`:381`–`:386`). `:356`–`:359`: *"**This is adaptation evidence, not GA matrix evidence**… there is no row in the runner yet."* | **E4** (`:446`–`:448` stream usage with and without `stream_options`) | not declared | n/a | n/a | **E4** — `tool_choice` measured over six probes (`:388`–`:416`) | **There is no Kimi file under `internal/provider/*/…real_smoke_test.go`** — `grep -rn "func TestReal" internal/` returns no Kimi entry, so this profile has no repeatable harness at all |
| `kimi.anthropic.messages.v1` | **E4 (adaptation only)** same run; the profile was *restored* on this evidence (`:364`–`:367`) | **E4** | not declared | n/a | **E4** | **E4** (`:396`–`:407`) | Its 503 is explicitly **unmeasured** and cannot be arranged (`:420`–`:425`, `:460`–`:462`) |
| `minimax.cn.subscription.openai.chat.v1` | **E1 only** — `provider-real-matrix.md:210`–`:233`, heading *"implementation evidence (2026-09-10)"*; `:212`–`:214`: *"No dedicated subscription credential was supplied for this change, so no billable real-provider call was run and no existing pay-as-you-go result is credited to either subscription product."* | E2 | n/a | n/a | n/a | E1 | Fake-transport contract tests only (`:223`–`:226`) |
| `minimax.global.subscription.openai.chat.v1` | **E1 only** — same | E2 | n/a | n/a | n/a | E1 | |

### 2.2 What the GA matrix runner would actually exercise if it were run

`tests/provider-matrix/main.go:83`–`:92` defines five GA rows: `openai`, `anthropic`, `azure_openai`, `deepseek`, `openai_compatible`. Beta rows at `:54`–`:81`: `bedrock_mantle`, `minimax`, `minimax_anthropic`.

Consequences, all read from source:
- **No runner row exists for** `openai.responses.v1`, `openai.media-resources.v1`, `gemini.generate-content.text.v1beta`, any BigModel profile, any Kimi profile, or either MiniMax Subscription profile. Nine of the 24 offered profiles are not reachable by the GA/Beta runner at all.
- The GA smoke body (`internal/provider/openai/real_smoke_test.go:86`–`:149`) issues exactly: one unary chat, one streaming chat, one optional embedding, plus DeepSeek's two extra assertions (`:144`–`:146`) and optional capability detection (`:147`–`:149`). **No tool call, and no negative request of any kind.**
- The Anthropic smoke (`internal/provider/anthropic/real_smoke_test.go:73`–`:82`) runs seven happy-path subtests. No tool call, no negative request.
- Beta results never move the gate: `tests/provider-matrix/main.go:119`–`:120` and `provider-real-matrix.md:1018`–`:1021`.

---

## 3. Negative-path coverage per declared primitive

The plan requires, per profile (`production-validation-plan.zh-CN.md:129`): 无效或已撤销凭证、权限不足、未知模型、限流、超时和上游 5xx.

### 3.1 The one class with essentially no coverage anywhere: **timeout**

No test in `internal/provider/**`, `internal/gatewayapi`, `internal/openaiapi`, `internal/anthropicapi`, `internal/compatibility` or `internal/circuit` drives a provider adapter against a hanging or slow upstream under a client/context deadline. What exists is *synthetic classification* of an already-constructed `context.DeadlineExceeded` value, with no adapter in the loop:
- `internal/provider/unsent_test.go:15` `TestTransportClassSeparatesTheCallerFromTheNetwork`
- `internal/provider/unsent_test.go:35` `TestUnsentSeparatesSetupFailuresFromPossiblyExecutedOnes`
- `internal/provider/transport_failure_test.go:12` `TestClassifyTransportFailureUsesClosedRetrySet`

The nearest adapter-level test is `internal/provider/openai/adapter_test.go:645` `TestChatPropagatesCancellation`, which covers `context.Canceled`, **not** a deadline. Every "timeout" cell marked covered below is an HTTP **408/504 status code** mapping, which is a different thing from a real client-side timeout.

### 3.2 Coverage matrix (automated tests only; `S` = covered only through the shared classifier, no vendor-specific test)

| Vendor / route | invalid/revoked cred | insufficient permission | unknown model | rate limit | timeout | upstream 5xx |
|---|---|---|---|---|---|---|
| openai | `internal/provider/openai/adapter_test.go:543` `TestFakeProviderHTTPErrorMatrix` | `:543` | `:580` `TestUpstreamRefusalMessageMatchesStatus`; `:821` `TestBadRequestRefusalKindComesFromTheUpstreamCode` | `:483` `TestHTTPErrorClassification`; `:543` | **408 status only** (`:543`) — **no real-timeout test** | `:543`; `:821` |
| azure-openai | S | S | S | S | S (408 only) | S — **no Azure-specific negative test of any class** |
| deepseek | S | S | S | S | S (408 only) | S — **no DeepSeek-specific negative test of any class** |
| openai-compatible | S | S | S | S | S (408 only) | S — **no compatible-specific negative test of any class** |
| anthropic | `internal/provider/anthropic/adapter_test.go:309` `TestAnthropicBadRequestRefusalIsUnattributed`; `internal/provider/anthropic/probe_test.go:71` `TestProbeWithoutDeploymentModelClassifiesUpstreamRefusal` | **NONE** | **NONE** (Anthropic answers 400 unattributed by design — documented at `adapter_test.go:309` — but no test drives a nonexistent model id) | `internal/provider/anthropic/adapter_test.go:139` `TestAnthropicHTTPErrorClassification` | **NONE** | via `internal/provider/anthropic/kimi_error_tolerance_test.go:31` (shared `decodeHTTPError`) |
| gemini | `internal/provider/gemini/adapter_test.go:166` `TestErrorsAreClassifiedWithoutReturningProviderBody` | **NONE** | **NONE** | `:204` `TestGeminiRefusalKeepsIdentifiersAndDropsTheSentence` | **NONE** (408 status at `:123`) | 500 only (`:123`) — **no 502/503** |
| bedrock-mantle (chat) | `internal/provider/openai/bedrock_mantle_wire_test.go:48`, `:83` | same | `internal/provider/bedrockmantle/adapter_test.go:356`, `:557` | `bedrockmantle/adapter_test.go:258`, `:356` | **408/504 status only** | `bedrockmantle/adapter_test.go:356`, `:557` |
| bedrock-mantle (messages) | `internal/provider/anthropic/bedrock_mantle_wire_test.go:116`, `:159` | `:116`, `:187` `TestBedrockMantleMessagesTreatsAnUnusableProjectAsAuthenticationFailure` | via `:116` matrix | `:116` | **status only** | `:116` |
| bedrock-mantle (responses) | `internal/provider/bedrockmantle/adapter_test.go:356`, `:461` | `:356`, `:437` | `:356`, `:557` | `:258`, `:356`, `:557` | **status only** | `:356`, `:557` |
| minimax | `internal/provider/openai/minimax_test.go:41`, `:81`; `internal/provider/anthropic/minimax_catalog_test.go:253` | **NONE** | **billable real-account only** — `internal/provider/openai/minimax_real_smoke_test.go:129`; **no hermetic test** | `minimax_test.go:41`, `:81`; `minimax_stream_test.go:103` | `minimax_test.go:41` (`base_resp` code 1001) | `minimax_test.go:41` |
| kimi | `internal/provider/anthropic/kimi_error_tolerance_test.go:31` | **NONE** | **NONE** | `:31` | **NONE** (504 classified as 5xx, not timeout) | `:31` |
| bigmodel-cn | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** | only a 200-wrapped `network_error` (`internal/provider/openai/bigmodel_test.go:187`, `:202`) — **no HTTP 5xx test** |
| bigmodel-global | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** | same as above |
| minimax subscription (both) | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** |
| openai media-resources | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** | **NONE** |
| openai.responses.v1 | S | S | S | S | S | S |

Gateway-layer negatives exist but answer a different question — they cover Halro's own Gateway Key and policy, not a provider credential: `internal/gatewayapi/guard_test.go:51`, `:212` (401 on unknown gateway key), `internal/gatewayapi/handler_test.go:513` (gateway-issued 429), `internal/gatewayapi/sourcelimit_test.go:46`/`:83`/`:123`, `internal/gatewayapi/handler_test.go:303` (`TestAnthropicOverloadedErrorPreservesProtocolStatus`, a synthetic `provider.Error`).

### 3.3 Real-account negative coverage

Exactly **one** real-provider smoke exercises any of the six classes: `internal/provider/openai/minimax_real_smoke_test.go:129` (`TestRealMiniMaxSmoke`, func at `:35`) drives an unknown model. Every other smoke is happy-path only: `internal/provider/openai/real_smoke_test.go:33`, `internal/provider/anthropic/real_smoke_test.go:41`, `internal/provider/gemini/real_smoke_test.go:27`, `internal/provider/bedrock/real_smoke_test.go:25`, `internal/provider/bedrockmantle/real_smoke_test.go:43` and `:170`, `internal/provider/openai/bigmodel_real_smoke_test.go:23`, `internal/provider/anthropic/minimax_real_smoke_test.go:28`, `internal/provider/openai/media_smoke_test.go:46`.

**Net G2 position on negatives: 0 of 24 offered profiles has E4 negative-path evidence for all six classes. 0 has it for even four of six.**

---

## 4. The two G2 invariants

Both invariants the plan names at `production-validation-plan.zh-CN.md:131`–`:132` **are enforced in code and are covered by automated tests at E2**. Neither has E4 evidence, because no real-provider run exists at all (§2.0). Citations below were spot-checked against the source, not taken on report.

### 4(a) 首字节或外部副作用发生后不切换 Provider、不重复执行副作用

Enforced by **three** distinct mechanisms, deliberately separate:

**(i) Bytes visible to the client — `emitted`.** The only cross-provider fallback loop for streaming chat is in `generateStream`:
- `internal/gateway/service.go:2302` — `emitted := false`, declared **outside** both loops, so it is per-request, not per-attempt.
- `internal/gateway/service.go:2303`–`:2304` — the nested loops: `for targetIndex, target := range targets` (cross-provider) inside which `for targetTry := 0; ...` (same-target retry).
- `internal/gateway/service.go:2371`, `:2390` — `emitted = true` set immediately after the caller's `emit(safeChunk)` returns (event callback, and the redactor `Flush()` tail).
- `internal/gateway/service.go:2417` — inner guard: `if emitted || !retryable(providerErr) { … return terminalProviderError(providerErr) }`.
- `internal/gateway/service.go:2434` — outer guard: `if totalAttempts >= s.maxAttempts || emitted || ctx.Err() != nil { break }` — this is the line that refuses the cross-provider switch.

**(ii) The upstream may already have run — `provider.Error.Ambiguous`.** This is the gate for non-streaming and for everything that is not "bytes on the wire":
- `internal/provider/provider.go:194` — the field.
- `internal/gateway/service.go:2612`–`:2623` — `func retryable(err error) bool`, with `if classified.Ambiguous { return false }` at `:2617`. Consulted before every retry *and* every fallback in every loop.
- `internal/provider/provider.go:338`–`:355` — `Unsent(err)`, the positive "nothing left the box" determination (SafeTransport pre-send refusal, DNS failure, failed dial).
- `internal/provider/provider.go:100`–`:103` — `ClassifyTransportFailure` sets `Ambiguous: !Unsent(err)`; comment at `:97`–`:99`: once CONNECT succeeded, TLS/read/write failures are conservatively ambiguous and non-retryable.

The two are joined by converting a post-payload wire/translation failure into an `Ambiguous` provider error so the generic gate stops it: `internal/gateway/service.go:1446`–`:1451` (Responses stream), `:1548`/`:1562`/`:1577`–`:1580` (Messages stream), `:1841`/`:1868`–`:1871` (native Messages stream, via `nativeStreamGate.emitted` declared `:2049`, set `:2161`), `:2004` (native outbound redaction failure).

**(iii) An upstream object was created — a durable state machine, not an in-memory flag.** `internal/gateway/inference_resources_store.go:52`–`:55` (`creationReserved` / `creationUnknown` / `creationCompleted`), classified at `:72`–`:96`, enforced at `:326`–`:327`, `:811`–`:812`, `:1204`–`:1205`: a replay of the same idempotency key after an ambiguous upstream create answers HTTP 409 `idempotency_in_progress` rather than calling any provider again. `creationUnknown` is stamped at `:388`, `:397`, `:846`, `:1233` and deliberately **not** for a local-only failure (`:383`–`:387`). Phase-2 resource operations are additionally forbidden to have more than one candidate: `internal/gateway/inference_resources.go:22`–`:24` → `ambiguous_resource_route`, 409.

Native Anthropic mode has **no fallback path at all** — `internal/gateway/service.go:1521`–`:1523`, and `prepareNativeMessages` (`:1907`) returns a single target.

HTTP-layer twin (bytes actually handed to `net/http`): `internal/gatewayapi/handler.go:244`–`:255` (`started`/`start()`), checked at `:272`; a post-payload failure closes the stream with an SSE `error` event and **no** `[DONE]` (`:283`–`:296`). Same pattern at `:131`–`:142`/`:166`, `:580`–`:590`/`:603`, `:628`–`:638`/`:655`.

Non-streaming is covered by (ii), not by an `emitted` flag: `executeGenerate` (`internal/gateway/service.go:1271`–`:1385`) returns from inside the loop on success (`:1368`), and a *post-success* render/redaction failure explicitly neither retries nor falls back (`:1324`–`:1372`, rationale at `:1323`–`:1331`). Same shape in the embeddings loop (`:2487`–`:2555`).

**Tests (E2):**

| Test | Location |
|---|---|
| `TestChatStreamNeverFallsBackAfterPayload` — the direct assertion | `internal/gateway/service_test.go:1767` |
| `TestChatStreamFallsBackOnlyBeforeFirstPayload` — negative control | `internal/gateway/service_test.go:1729` |
| `TestAmbiguousProviderFailureIsEstimatedAndSettled` — non-streaming, asserts `primary=1 fallback=0` | `internal/gateway/service_test.go:1185` |
| `TestChatDoesNotFallbackForNonRetryableProviderError` | `internal/gateway/service_test.go:708` |
| `TestChatDoesNotFallbackForProviderAuthenticationFailure` | `internal/gateway/service_test.go:2020` |
| `TestChatRetriesThenFallsBackInPriorityOrder` — control: fallback *is* allowed pre-byte | `internal/gateway/service_test.go:678` |
| `TestAcceptedMalformedResponseIsNotRetriedAndSettlesConservatively` | `internal/gateway/accepted_response_test.go:24` |
| `TestResponsesFinalStreamEventFailureIsNotRecordedAsSuccess` (`adapter.calls == 1`) | `internal/gateway/outbound_failure_outcome_test.go:188` |
| `TestMessagesFinalStreamEventFailureIsNotRecordedAsSuccess` (`adapter.calls == 1`) | `internal/gateway/outbound_failure_outcome_test.go:215` |
| `TestUnreachableProviderFailsOverToTheNextTarget` | `internal/gateway/unsent_attempt_test.go:31` |
| `TestAmbiguousStreamFailureStillSettlesConservatively` | `internal/gateway/unsent_attempt_test.go:108` |
| `TestInferenceResourcesUnknownFileCreationBlocksRetry` — the external-object case | `internal/gateway/inference_resources_service_test.go:361` |
| `TestInferenceResourcesFileCleanupFailureKeepsRetryState` | `internal/gateway/inference_resources_service_test.go:467` |
| `TestStreamingPostPayloadErrorHasNoDoneSentinel` — HTTP layer | `internal/gatewayapi/handler_test.go:587` |
| `TestStreamingPrePayloadErrorRemainsJSON` — HTTP-layer control | `internal/gatewayapi/handler_test.go:568` |

**Verdict: enforced, tested at E2 with a negative control on both sides of the boundary, and "external side effect" *is* modelled distinctly from "bytes written" (three concepts, three mechanisms). No E4.**

Residual gap for G2: every one of those tests drives a fake adapter. The plan's G2 requires the property to hold against a real provider (`production-validation-plan.zh-CN.md:131`), and no such run exists.

### 4(b) 未被 Provider 实际枚举或确认的目标不伪装成 `provider_metadata` 能力

Enumeration and capability confirmation are **two different enums**, which is exactly the distinction the plan (`:124`) demands be recorded separately:

| Enum | Location | Means |
|---|---|---|
| `domain.MetadataSource` → `MetadataSourceProvider` | `internal/domain/invocation_target.go:30`–`:51` (const at `:34`) | the upstream **enumerated** this target |
| `domain.ClaimSource` → `ClaimSourceProviderMetadata` | `internal/domain/invocation_target.go:134`–`:142` (const at `:138`) | this **capability claim** came from provider metadata |
| `modelcatalog.Source` → `SourceProviderMetadata` | `internal/modelcatalog/catalog.go:29`–`:51` (const at `:39`) | the catalog-merge twin |

`MetadataSourceProvider` is assigned only where an adapter reads a real upstream enumeration response: `internal/provider/gemini/adapter.go:187`, `:234`; `internal/provider/bedrock/models.go:129`; `internal/provider/anthropic/adapter.go:235`. Everything Halro constructs itself is `MetadataSourceNone` or `MetadataSourceModelCatalog`: `internal/app/admin_deployments.go:949`, `internal/app/admin_invocation_targets.go:213`, `:955`, `:512`, `internal/provider/model_catalog.go:69`.

**The gate is one line, at the sole call site of `ProviderMetadataMapper`** (interface at `internal/provider/provider.go:447`–`:449`):

```go
// internal/app/admin_invocation_targets.go:630
if mapper := mappers[binding.ID]; mapper != nil && bindingTarget.MetadataSource == domain.MetadataSourceProvider {
```

verified in source; the 25-line rationale above it (`internal/app/admin_invocation_targets.go:606`–`:629`) records the two bugs it fixed, including the quiet one: *"the Anthropic mapper claims chat and streaming from the endpoint rather than from a field, so a model nobody has ever served resolved as a working chat deployment on evidence labelled provider_metadata."* Both consumers funnel through it — listing (`internal/app/admin_invocation_targets.go:582`) and deployment save (`internal/app/admin_deployments.go:1003`).

Defence in depth:
- `internal/app/admin_invocation_targets.go:636`–`:643` — a mapper claim failing `Validate()` or scope match marks the binding conflicting (fail-closed).
- `internal/domain/invocation_target.go:186`–`:189` — a `provider_metadata` or `verified_probe` claim **must** carry an expiry, so a claim built from a target with no `FetchedAt` cannot validate.
- `internal/domain/invocation_target.go:183`–`:185` + `internal/domain/models.go:992`–`:1001` — `provider_metadata` caps at `EvidenceDeclared`, never `verified`.
- `internal/modelcatalog/catalog.go:63`–`:68` — `Source.PreselectsCapabilities()` is **false** for `SourceProviderMetadata`: upstream metadata never pre-ticks a capability box.
- `internal/modelcatalog/catalog.go:446`–`:461` — `rank()` gives it the lowest non-zero rank.
- `internal/provider/anthropic/adapter.go:326`–`:328` — the mapper deliberately claims nothing for tool use.
- `internal/provider/capability_detection.go:593`–`:601` (call site `:478`) — `applySubstitutionGuard` discards probe evidence from a model other than the one requested (the `verified_probe` analogue; the finding that produced it is recorded at `provider-real-matrix.md:110`–`:114`).

**Tests (E2):**

| Test | Location |
|---|---|
| `TestAMapperSeesOnlyTargetsAnUpstreamEnumerated` — the primary one; table over Bedrock-Mantle-Anthropic, direct Anthropic, MiniMax-Anthropic, using the **real** adapter | `internal/app/model_catalog_offers_test.go:274` |
| `TestAnEnumeratedTargetStillEarnsItsProviderClaims` — negative control, gate does not over-block | `internal/app/model_catalog_offers_test.go:302` |
| `TestACatalogCoveredModelStillResolvesWhenTyped` | `internal/app/model_catalog_offers_test.go:318` |
| `TestModelCatalogOffersTravelAsOffersNotFindings` — a catalog offer is labelled `model_catalog`, not `provider_metadata` | `internal/app/model_catalog_offers_test.go:51` |
| `TestAnOfferResolvesForTheDeploymentSaveToo` — guards the second consumer | `internal/app/model_catalog_offers_test.go:148` |
| `TestNewTargetIsAvailableWithoutInventingCapabilities` — enumeration ≠ capability | `internal/app/admin_invocation_targets_test.go:96` |
| `TestProviderMetadataCreatesOnlyAllowlistedClaimsAndConflictsFailClosed` | `internal/app/admin_invocation_targets_test.go:278` |
| `TestSignedCatalogAndProviderMetadataConflictFailsClosed` | `internal/app/admin_invocation_targets_test.go:294` |
| `TestMergeCannotExceedProfileCeiling` — assertion at `:344`–`:345` | `internal/modelcatalog/catalog_test.go:327` |
| `TestMergeSilenceIsNotDenial` | `internal/modelcatalog/catalog_test.go:313` |
| `TestTheSubstitutionGuardIsScopedToUpstreamsThatEchoTheIdentifier` | `internal/provider/capability_substitution_test.go:58` |
| Bedrock Mantle asserts it deliberately does **not** implement `ProviderMetadataMapper` | `internal/provider/bedrockmantle/adapter_test.go:239` |
| Frontend never renders a raw evidence-source token | `web/src/pages/DeploymentsPage.test.tsx:336` |

The separation is also written down normatively at `docs/contracts/provider-capabilities.md:55`–`:62` ("Discovery is existence, not capability" / "Provider metadata is allowlisted by its Adapter"), `:120`–`:121`, and `AGENTS.md:113`–`:132`.

**Verdict: enforced at a single choke point, tested at E2 with a negative control, and the enumerate/confirm distinction is explicit in the type system. No E4.**

**One unguarded default found, and it is worth an owner decision before G2 signs off.** `internal/app/admin_deployments.go:1029` initialises `source := modelcatalog.SourceProviderMetadata` as the *default* label for the stored deployment entry, upgrading it only when a `builtin_catalog` or `signed_catalog` claim exists (`:1030`–`:1039`). That value becomes the persisted `ModelCapabilitySnapshot.Source` at `internal/app/admin_deployments.go:621`. Two later branches override it — capability detection (`:634`–`:636`) and operator declaration (`:637`–`:639`, which sets `SourceOperatorDeclared` with the comment *"An operator declaration is its own source"*). The residual case is a resolved variant whose claims are neither builtin nor signed and which took neither override: it is persisted as `provider_metadata`. **I found no test asserting that this default cannot mislabel.** Whether that case is actually reachable at runtime is **UNVERIFIED** — establishing it needs a running instance and a constructed variant, which this read-only survey could not do. It is recorded as an open question, not as a defect.

---

## 5. G5 application-side load model — component → harness

The plan names nine components (`production-validation-plan.zh-CN.md:167`–`:169`): 同步请求、慢 Provider、长流式响应、大响应体/失败捕获、多 Project、公平性、预算与 token guard、管理面大数据量、备份与压缩等后台任务.

| Component | Harness that exists | Verdict |
|---|---|---|
| **同步请求** (synchronous requests) | `tests/soak/main.go:199` `driveLoad` | **Partial.** It is a **single goroutine issuing one blocking request per tick** (`tests/soak/main.go:125`–`:128` starts exactly one; `:205`–`:234` is a serial ticker loop), default `-request-interval 10s` (`:92`). There is no concurrency knob. `docs/verification/soak-testing.md:27` puts the ceiling at "at most 8,640 small requests" over 24 h — ≈0.1 RPS. This is a leak detector, **not** a load model. |
| **慢 Provider** (slow provider) | **NONE** | No harness injects upstream latency. `tests/soak` talks to whatever is deployed; `tests/stress` uses a hand-written in-process `streamService` (`tests/stress/stream_test.go:26`–`:55`) that blocks on a release channel — that is a *held* stream, not a latency profile, and it is not a provider. |
| **长流式响应** (long streaming) | `tests/stress/stream_test.go:61` `TestThousandConcurrentSSEConnectionsCleanup` | **Partial.** 1,000 concurrent SSE connections held open with non-reading clients (`:112`–`:114`), asserting goroutine/FD cleanup. Gated behind `HALRO_STRESS=1` (`:62`). It uses a fake in-process service, exercises no provider path, and measures cleanup rather than sustained throughput or per-stream memory. |
| **大响应体 / failure capture** | **NONE at load.** Correctness bounds are E2-tested: `internal/failurecapture/failurecapture_test.go:120` (oversize truncation), `:155` (byte ceiling before async ownership), `:184` (daily ceiling), `:215` (ceiling survives restart), `:270` (retention window) | **No harness.** Nothing generates large response bodies under load or measures failure-capture growth against a budget. |
| **多 Project** (multi-project) | `internal/budget` `BenchmarkRequestLifecycle` (`internal/budget/lifecycle_throughput_bench_test.go:31`), listed as workload 1 in `docs/verification/standalone-capacity-baseline.md:33` | **Benchmark only, not a load harness.** It varies worker count × project count in-process (`standalone-capacity-baseline.md:51`–`:55`); it never goes through the HTTP surface. `tests/soak` uses **one** gateway key (`tests/soak/main.go:100`). |
| **公平性** (fairness) | **NONE** | `grep -rn "func Test.*[Ff]air" internal/` returns **zero results**. The closest is `internal/limiter/benchmark_test.go:40` `BenchmarkProjectAdmissionContended`, which measures contended admission throughput, not whether one noisy project starves another. **No fairness assertion exists anywhere in the tree.** |
| **预算与 token guard** | `internal/tokenguard/benchmark_test.go:10` `BenchmarkAdmit`, `:43` `BenchmarkAcquireReleaseContended`; `internal/limiter/benchmark_test.go:12` `BenchmarkProjectAdmission`, `:40` | **Benchmarks only.** `standalone-capacity-baseline.md:14` names this workload 2 ("Admission-only RPM/TPM/concurrency and Token Guard cost") and `:28`–`:29` states only **workloads 1 and 5** have committed entry points — so workload 2 has none. |
| **管理面大数据量** (admin plane at scale) | `internal/store/bolt/pricing_throughput_bench_test.go:38` `BenchmarkMetadataWriteTransaction`, `:79` `BenchmarkDeploymentPricePinCeiling`, `:160` `BenchmarkMetadataBatchDelay` — workload 5 in `standalone-capacity-baseline.md:35`–`:36` | **Benchmark only.** These are bbolt write-contention benchmarks. Nothing drives the Admin HTTP surface, list/paginate endpoints, or the React console against a large dataset. |
| **后台任务** (backup, compaction) | **NONE** | `grep -rn "^func Benchmark" internal/` (18 hits, all listed) contains no backup, no `usage compact`, no Parquet-partition and no audit-seal benchmark. Ledger replay has `internal/ledger/log_test.go:642` `BenchmarkReplayLargeWAL`, which is startup replay (workload 6), not a *concurrent background task under load*. |

### 5.1 Harnesses under `tests/` and what each is actually for

| Harness | Scope | G5 component covered |
|---|---|---|
| `tests/soak/main.go` | Long-duration serial single-key chat load against a **running instance**, sampling RSS / goroutines / FDs / WAL queue depth / analytics queue (`:38`–`:50`), with pass thresholds at `:52`–`:58` and evaluation at `:352` | 同步请求 (at ≈0.1 RPS), and resource-drift detection. Nothing else. |
| `tests/stress/stream_test.go` | In-process 1,000-stream SSE cleanup, `HALRO_STRESS=1` | 长流式响应 (cleanup half only) |
| `tests/provider-matrix/main.go` | Real-provider correctness evidence runner | **Not a load harness.** Contributes to G2, nothing to G5. |
| `tests/compatibility/{go,node,python,server}` | Official-SDK protocol conformance against an in-process fake service (`tests/compatibility/server/main.go:26` `type service struct`) | **Not a load harness.** No provider, no concurrency assertion. |

### 5.2 G5 pre-filled SLO status

`production-validation-plan.zh-CN.md:81`–`:91` requires every service target to be filled **before first measurement** ("禁止看到结果后调整阈值"). Every one of the nine rows currently reads `TBD`, including the three the Application role owns jointly: 同步请求吞吐与 p50/p95/p99 (`:83`), 流式首字节与完整响应 p95/p99 (`:84`), 允许错误率、超时率和限流率 (`:85`). **G5 cannot legitimately start.** The only pre-declared numeric thresholds that exist anywhere are the soak harness's own leak limits (`tests/soak/main.go:52`–`:58`), which are not SLOs.

---

## 6. Gaps found, most severe first

1. **No E4 evidence exists for any offered profile's own request path.** Every real-account section in `provider-real-matrix.md` is either (a) explicitly labelled adaptation-not-GA evidence with no `-commit` binding and no archived evidence file (DeepSeek `:502`–`:504`, MiniMax `:248`–`:250`, Kimi `:356`–`:359`), (b) a `curl` probe of the *service* rather than of Halro (Bedrock Mantle `:677`–`:682`, `:967`–`:974`), or (c) explicitly "not run" (BigModel `:190`–`:191`, subscription products `:212`–`:214`, Mantle smoke `:994`). Under the plan's own pass criterion (`production-validation-plan.zh-CN.md:136`) — "未执行真实验证的 profile 必须从本次生产声明中移除" — **all 24 offered profiles would have to be removed from the production claim today.**

2. **Timeout has no adapter-level automated coverage at all, for any vendor.** Only synthetic classification of a pre-built `context.DeadlineExceeded` (`internal/provider/unsent_test.go:15`, `internal/provider/transport_failure_test.go:12`) and HTTP 408/504 status mapping. G2 requires 超时 per profile; the repo cannot currently answer it even at E2.

3. **Four offered profiles are served but not publicly declared.** BigModel CN/global general and CN/global coding (`internal/domain/provider_table.go:236`, `:244`, `:262`, `:275`) appear in no `README.md` Providers row (`:280`–`:290`) and in no `CLAUDE.md` provider list. The two MiniMax Subscription OpenAI profiles (`:544`, `:559`) are likewise absent from README. G2's ledger cannot be closed against a list that does not exist.

4. **Gemini has zero real-account evidence of any kind.** No section in `provider-real-matrix.md`, no runner row in `tests/provider-matrix/main.go:54`–`:92`, and a harness (`internal/provider/gemini/real_smoke_test.go:27`) that has never been recorded as run — while `README.md:289` presents it as a shipped Beta provider and the manifest marks it `"compatible"` on chat, responses, messages and embeddings.

5. **BigModel (both regions, general profiles) has no negative-path test of any class** — no 401, 403, unknown-model, 429, timeout or HTTP 5xx test exists; only two 200-wrapped `finish_reason` tests (`internal/provider/openai/bigmodel_test.go:187`, `:202`).

6. **Kimi has no real-provider harness at all.** `grep -rn "func TestReal" internal/` produces no Kimi entry, and `provider-real-matrix.md:358`–`:359` confirms *"there is no row in the runner yet"*. Its 2026-09-01 findings are therefore unrepeatable at a new candidate SHA — exactly what §3 of the plan (不可变验证单元) forbids relying on.

7. **`endpoint-manifests.json` — the document `README.md:270`–`:271` calls authoritative — publishes coverage for 10 withheld profiles**, `kimi.responses.v1` and `bedrock.runtime.converse.text.v1` at status `"compatible"`. The only withholding filter is scoped to the native Anthropic protocol (`internal/compatibility/manifest.go:469`), and no test reconciles manifest coverage against `Withheld`.

8. **`openai.responses.v1` and `openai.media-resources.v1` are unreachable by any smoke.** `internal/provider/openai/real_smoke_test.go:44`–`:48` accepts four profile names, none of them Responses; the media smoke (`media_smoke_test.go:46`) has no recorded run. Both are offered profiles carrying README claims (`README.md:282`).

9. **G5 has no harness for five of its nine named components**: 慢 Provider, 大响应体/failure capture, 公平性, 后台任务, and (as an HTTP-level load) 管理面大数据量. **Fairness has no test of any kind anywhere in the tree** (`grep -rn "func Test.*[Ff]air" internal/` → zero hits), which is the one component where a benchmark cannot substitute, because the property is relational.

10. **`tests/soak` is misclassified as a load harness.** It is single-goroutine, single-key, single-model, non-streaming, ≈0.1 RPS (`tests/soak/main.go:125`–`:128`, `:205`–`:234`, `:92`; `docs/verification/soak-testing.md:27`). It satisfies the plan's step 3 ("在目标持续负载下执行 24 小时浸泡") in duration only, not in load shape. Building the G5 load model means writing a new harness, not configuring this one.

11. **Every G5 SLO cell is `TBD`** (`production-validation-plan.zh-CN.md:81`–`:91`), and the plan forbids setting thresholds after seeing results (`:78`–`:79`). G5 is blocked at its precondition.

12. **No profile has a recorded tool-call round trip through Halro against a real account, except BigModel Coding** (`provider-real-matrix.md:145`–`:147`) **and Kimi** (`:396`–`:407`) — and both of those are adaptation evidence, not gate evidence. Neither GA smoke sends a tool (`internal/provider/openai/real_smoke_test.go:86`–`:149`; `internal/provider/anthropic/real_smoke_test.go:73`–`:82`).

13. **One unguarded default on the capability-source label**, with no test: `internal/app/admin_deployments.go:1029` defaults a persisted deployment snapshot's `Source` to `provider_metadata` (reaching `ModelCapabilitySnapshot.Source` at `:621`) unless a builtin/signed claim, capability detection or an operator declaration overrides it. Reachability UNVERIFIED (§4b). Lowest severity of the list, but it sits on exactly the invariant G2 asks about.

14. **Both G2 invariants pass at E2 — this is the one part of G2 in good shape.** No-switch-after-first-byte and no-unconfirmed-`provider_metadata` are each enforced at a single choke point with negative-control tests (§4). Recorded here so the gate does not spend E4 budget re-establishing what E2 already pins; what E4 owes is the *real-provider* half, not the logic.

---

## 7. UNVERIFIED items

- **Whether any test named in §3 actually passes at `f09ed2d`.** No test was executed (a full gate is running in another process). Every E2 claim is read from source.
- **Whether the DeepSeek 2026-08-20 result at `13d55ff` still holds at `f09ed2d`.** `provider-real-matrix.md:500` names the run commit; the plan's §3 requires re-running on the frozen candidate. Not checked — that needs a billable call.
- **Whether `provider-real-matrix.md` describes runs whose raw evidence exists outside the repo.** The document records no evidence IDs, digests or URIs in the format `production-validation-plan.zh-CN.md:230`–`:251` prescribes. Absence in-repo is not proof of absence in a controlled evidence store; the Application owner has to confirm. Recorded here as UNVERIFIED rather than as "no evidence exists".
- **Whether the `openai.media-resources.v1` batch/file operations behave as documented.** `provider-real-matrix.md:36`–`:46` describes the smoke; no run is recorded and none was performed.
- **Whether the `provider_metadata` default at `internal/app/admin_deployments.go:1029` is reachable with a variant carrying only `operator_declared` or `verified_probe` claims.** Determining this needs a running instance and a constructed variant; read-only static reading cannot settle it. Recorded as an open question, not as a defect (§4b).
- **Whether the plan's "正式声明" list is intended to be `provider_table.go`'s offered rows, README's nine vendors, or the endpoint manifest.** This is an owner decision, not a fact discoverable in the tree (§0). Every count in this report is against `provider_table.go`.
