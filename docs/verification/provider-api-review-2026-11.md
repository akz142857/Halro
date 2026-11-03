# Provider API review — 2026-11-03

This review covers all 11 provider types and 36 registered profiles, including
withheld runtime and subscription profiles. It compares the existing adapters
with official documentation and published schemas. It does not establish account
entitlements, network reachability, or successful inference on every provider.
All changes belong to PR #482.

Model enumeration remains authoritative for account availability. Bundled entries
supply reviewed capabilities for matching identifiers; adding an entry does not
make a model available to an account. Unknown identifiers remain unknown.

## Changes included

| Provider | Contract checked | Result in this PR |
| --- | --- | --- |
| OpenAI | Chat, Responses, media and GPT-6 model contracts | Native Responses streaming, reasoning replay across tool turns, strict tools, effort/sampling validation, usage and media updates. See the [OpenAI runbook](../runbooks/openai-api-compatibility.md). |
| Anthropic | Messages, token counting, native and subscription boundaries | Accept `2026-10-21` as well as `2023-06-01`. Validate the new date's removed parameters and role/content restrictions before admission. Preserve the caller's native version; keep the existing portable upstream default. Support native top-level automatic cache control and refuse it in portable mode. |
| DeepSeek | Chat and model discovery | Send effort at the top level, alongside `thinking.type`, rather than inside `thinking`. Refuse temperature/top-p controls that the reviewed model and reasoning/tool mode would ignore or clamp. Add capability evidence for `deepseek-flash`, without changing account inventory. |
| MiniMax | Metered Chat, Messages, Responses; separate subscription contracts | Add exact Chat behavior for `MiniMax-M3.1-Flash`: preserve always-on reasoning and the low/medium/high/xhigh/max ladder; split reasoning from the answer; refuse `none`, `minimal`, and an answer-only output limit. Keep M3's existing switch behavior. No M3.1 capability entry is added to the other faces or subscription products. |
| Kimi | Chat model discriminator, native Messages caching | Add the two K3.1 Pro IDs with low/high/max effort and forced reasoning. Declare model-specific tool and output-budget incompatibilities before admission. Preserve the measured K3 off switch. Native Messages accepts top-level `cache_control` with default/5m/1h TTL, preserves its bytes, and still refuses ignored nested breakpoints. |
| BigModel / Z.AI | Regional standard and Coding Chat | Carry `max` effort on GLM-5.2/5.3; request incremental tool streaming on the exact 5.3 IDs; send `clear_thinking=false` when replaying their reasoning history. Other model contracts remain unchanged. |
| Gemini | Text Generate Content, streaming, embeddings | Send explicit `store=false` on both generation paths. For reviewed modern text IDs, reject removed temperature/top-p/candidate/prefill controls before provider I/O. Add 3.8 Flash and 4 Argon text capability entries. Existing 2.5 behavior is retained before the announced migration date. |
| Azure OpenAI | Existing deployment-path Chat/Embeddings | The legacy profile remains supported. The additional v1/Responses surface is an explicit remaining gap below. An arbitrary deployment name is not treated as evidence of the underlying OpenAI model. |
| Amazon Bedrock | Registered Runtime, Agent Runtime and Mantle profiles | Keep the existing route, signing and project boundaries. The new Anthropic date is restricted to reviewed Claude 4.7+ model IDs; older/unreviewed models fail before I/O. Existing Mantle Responses reasoning and new Runtime Responses remain gaps below. |
| Volcengine Ark | Existing Chat profile and published new interfaces | No newly broken Chat behavior established; the existing contract was reviewed on 2026-11-02. Additional interfaces remain below. |
| Generic OpenAI-compatible | Configured dialect boundary | No universal vendor contract exists. No blanket OpenAI model rules, new capabilities or endpoints are inferred for unrelated upstreams. |

## Explicit remaining gaps

These are additional interfaces or evidence requirements, not claims that the
existing supported API was removed. This PR does not register them as working.

- **Azure v1 and Responses:** require a separate explicit profile, versionless
  `/openai/v1` addressing, deployment/body mapping, credential and discovery
  fixtures, and reasoning/tool/SSE acceptance. The legacy dated profile must not
  be silently redirected.
- **Bedrock Responses:** existing Mantle decoding still cannot carry reasoning
  items. Runtime `/openai/v1/responses` additionally needs its own endpoint,
  authorization, discovery and model support contract; it is not a hostname
  replacement for Mantle. Do not route reasoning workloads to those profiles.
- **DeepSeek native Responses/Messages and richer model metadata:** dedicated
  upstream profiles are absent. The Chat model list still enumerates IDs, but its
  shared decoder does not retain all new limits, modalities and effort metadata.
  A real `/models` response must be captured before writing that decoder; no
  DeepSeek account response was obtained in this review. Documentation examples
  are not presented as account captures.
- **MiniMax M3.1 on other faces:** native Messages can forward its provider
  payload, but the portable signed-thinking projection and Responses reasoning
  decoder do not establish a complete M3.1 contract. Those capability entries
  stay absent. Subscription access is not inferred from metered API docs.
- **Kimi cache pricing:** TTL directives are preserved, but the ledger does not
  model separate 5-minute and 1-hour cache-write price tiers. Operators must not
  treat estimated local costs as the provider's invoice. Chat/Responses
  `prompt_cache_options` is not added to the portable request schema.
- **Gemini Interactions/tools:** the existing profile is text-only. New thought
  signatures, function-call IDs, server state and Interactions are not implemented
  by labeling Generate Content as a new endpoint.
- **Ark Responses/Messages:** official docs and the first-party SDK expose these
  interfaces, but full current contract retrieval did not complete here. No
  decoder or capability claim was built from search snippets alone.

## Migration notices

Google announces automatic routing of `gemini-2.5-pro` to `gemini-3.8-flash` and
`gemini-2.5-flash` to `gemini-3.5-flash-lite` on 2026-12-01, including different
pricing and request contracts. Choose an explicit new deployment and validate it
before that date. Halro does not silently rewrite a deployment or its prices.

Kimi announces changed access for new K2-series users from 2026-10-31 and a planned
end-of-December retirement. Existing accounts must use refreshed upstream
inventory and current provider notices; the bundled catalog is not entitlement
evidence. DeepSeek's old Flash identifiers temporarily redirect to V4.1 Flash;
prefer the current `deepseek-flash` identifier when provisioning a new deployment.

## Evidence and validation

Regression tests cover wire-level effort placement; forced-thinking defaults and
off-switch refusals; GLM tool history; explicit Gemini storage opt-out on unary
and SSE requests; new-date Anthropic generation/counting validation; exact native
version headers; and cache TTL byte preservation and ignored-marker rejection.
All use local fixtures. No additional paid provider calls were made for this
cross-provider review.

The earlier local OpenAI acceptance used four explicitly authorized requests on
the configured Responses deployment: tools, continuation, SSE and encrypted
reasoning replay all returned 200. Those results establish that deployment only,
not the other providers above. The local container still runs that earlier build;
the additional cross-provider changes in this PR have not been deployed there.

## Official sources

- Anthropic: [API date migration](https://platform.claude.com/docs/en/api/migration-2026-10-21), [release notes](https://platform.claude.com/docs/en/release-notes/overview), [automatic caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching).
- DeepSeek: [Chat schema](https://api-docs.deepseek.com/api/create-chat-completion/), [thinking and sampling](https://api-docs.deepseek.com/guides/thinking_mode/), [Models schema](https://api-docs.deepseek.com/api/list-models/), [changes](https://api-docs.deepseek.com/updates/), [Responses schema](https://api-docs.deepseek.com/api/create-response/).
- MiniMax: [OpenAI Chat](https://platform.minimax.io/docs/api-reference/text-chat-openai), [Anthropic Messages](https://platform.minimax.io/docs/api-reference/text-chat-anthropic).
- Kimi: [K3.1 guide](https://platform.kimi.ai/docs/guide/kimi-k3-1-pro-quickstart), [context caching](https://platform.kimi.ai/docs/guide/context-caching), [models and lifecycle](https://platform.kimi.com/docs/models).
- BigModel / Z.AI: [GLM migration](https://docs.z.ai/guides/overview/migrate-to-glm-new), [thinking](https://docs.z.ai/guides/capabilities/thinking-mode).
- Gemini: [release notes](https://aistudio.google.com/docs/changelog), [3.8 migration](https://aistudio.google.com/docs/whats-new-gemini-3.8), [Interactions](https://ai.google.dev/gemini-api/docs/interactions-overview).
- Azure: [API lifecycle](https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle), [reasoning models](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/reasoning).
- Bedrock: [Responses API](https://docs.aws.amazon.com/bedrock/latest/userguide/inference-responses-api.html), [OpenAI's Bedrock guide](https://developers.openai.com/api/docs/guides/amazon-bedrock).
- Ark: [Responses API](https://docs.volcengine.com/docs/ark/create-model-responses-api?lang=en), [agent invocation](https://docs.volcengine.com/docs/ark/agent-model-invocation-practice?lang=en), [official SDK response types](https://pkg.go.dev/github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses).
