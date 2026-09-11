# R1：架构与领域模型评审

评审基线：`v0.7.1..222d08f84f61493fc9a273d351cc728528d6e30c`（`main`，2026-09-11）。本报告只读评审 Provider Offering、Surface、Region、Connection Group、稳定 ID、使用条款 revision、注册闸，以及 INV-03/04/05/06/07；没有调用真实 Provider，也没有修改产品代码。

## 结论

发布判定：**不建议按当前状态发布 v0.8.0**。确认 1 个 P1 功能/治理缺陷、1 个 P2 兼容性回归盲区；另有 1 个 P1 候选问题需要产品契约或真实响应证据裁决。

| 编号 | 分类 | 严重度 | 置信度 | 结论 |
| --- | --- | --- | --- | --- |
| R1-01 | 确认问题 | P1 | 高 | 使用条款确认只在新增/换绑时校验；条款 revision 升级后，既有受限连接仍会被装载并继续接流量 |
| R1-02 | 确认问题（测试/兼容性） | P2 | 高 | Profile/Surface/Offering 的“永久 ID”缺少覆盖全表的字面值兼容性闸；部分 ID 可随常量一起改名而测试仍通过 |
| R1-C01 | 候选问题 | P1 | 中 | MiniMax Global 一般 API 与 Subscription Access 同 host/path/header，错误产品声明无法机械拒绝，并可绕过条款确认 |

除以上问题外，INV-03、INV-04、注册闸和 INV-07 的代码链路在本次范围内没有发现确认缺陷；已有窄测试均通过。

## 全量身份映射

权威表位于 `internal/domain/provider_table.go:158-559`（Profile）和 `internal/domain/provider_offering.go:154-283`（Offering/Surface）。以下把 34 个 Profile 全部列出；同一格中的 Profile 共享完全相同的连接身份。`开放`表示当前 Admin 可创建，`保留`表示 `Withheld=true`。

| Provider Type | Profile ID | Connection Group | Surface / Credential Scheme | Offering / Region | 状态 |
| --- | --- | --- | --- | --- | --- |
| openai | `openai.chat-embeddings.v1`、`openai.responses.v1`、`openai.media-resources.v1` | `openai-api` | `openai-api` / `bearer.static` | `openai.api-platform` / none | 开放 |
| anthropic | `anthropic.messages.2023-06-01` | `anthropic-api` | `anthropic-api` / `anthropic.x-api-key` | `anthropic.console-api` / none | 开放 |
| azure_openai | `azure-openai.chat-embeddings.v1` | `azure-openai` | `azure-openai` / `azure.api-key` | `azure-openai.resource` / none | 开放 |
| deepseek | `deepseek.chat.v1` | `deepseek-api` | `deepseek-api` / `bearer.static` | `deepseek.api-platform` / none | 开放 |
| openai_compatible | `openai-compatible.chat-embeddings.v1` | `openai-compatible` | `openai-compatible` / `bearer.static` | `openai-compatible.self-declared` / none | 开放 |
| gemini | `gemini.generate-content.text.v1beta` | `gemini-text` | `gemini-generate-content` / `google.api-key` | `google.gemini-api` / none | 开放 |
| bigmodel | `bigmodel.cn.chat-embeddings.v1` | `bigmodel-cn-general` | `bigmodel-cn-general-api` / `bigmodel.api-key` | `bigmodel.general-api` / cn | 开放 |
| bigmodel | `bigmodel.global.chat.v1` | `bigmodel-global-general` | `bigmodel-global-general-api` / `bigmodel.api-key` | `bigmodel.general-api` / global | 开放 |
| bigmodel | `bigmodel.cn.coding.chat.v1` | `bigmodel-cn-coding` | `bigmodel-cn-coding-api` / `bigmodel.coding-plan-key` | `bigmodel.coding-plan` / cn | 开放 |
| bigmodel | `bigmodel.global.coding.chat.v1` | `bigmodel-global-coding` | `bigmodel-global-coding-api` / `bigmodel.coding-plan-key` | `bigmodel.coding-plan` / global | 开放 |
| bedrock | `bedrock.runtime.converse.text.v1`、`bedrock.runtime.invoke.titan-embed-text-v2.v1`、`bedrock.runtime.invoke.titan-image-v2.v1`、`bedrock.runtime.async.nova-reel-v1.v1` | `bedrock-runtime` | `bedrock-runtime` / `aws.sigv4.explicit-session` | `aws.bedrock-runtime` / none | 保留 |
| bedrock | `bedrock.agent-runtime.rerank.cohere-v3-5.v1` | `bedrock-agent-runtime` | `bedrock-agent-runtime` / `aws.sigv4.explicit-session` | `aws.bedrock-runtime` / none | 保留 |
| bedrock | `bedrock.mantle.chat.v1`、`bedrock.mantle.openai.chat.v1`、`bedrock.mantle.responses.v1`、`bedrock.mantle.openai.responses.v1`、`bedrock.mantle.anthropic.messages.v1` | `bedrock-mantle` | `bedrock-mantle` / `aws.bedrock.api-key` | `aws.bedrock-mantle` / none | 开放 |
| minimax | `minimax.anthropic.messages.v1`、`minimax.chat.v1`、`minimax.responses.v1` | `minimax-api` | `minimax-api` / `bearer.static` | `minimax.api-platform` / endpoint(global/cn) | 开放 |
| kimi | `kimi.chat.v1`、`kimi.anthropic.messages.v1` | `kimi-api` | `kimi-api` / `bearer.static` | `kimi.open-platform` / endpoint(global/cn) | 开放 |
| kimi | `kimi.responses.v1` | `kimi-api` | `kimi-api` / `bearer.static` | `kimi.open-platform` / endpoint(global/cn) | 保留 |
| kimi | `kimi.code.openai.chat.v1` | `kimi-code-openai` | `kimi-code` / `kimi.code-key` | `kimi.code` / none | 保留 |
| kimi | `kimi.code.anthropic.messages.v1` | `kimi-code-anthropic` | `kimi-code` / `kimi.code-key` | `kimi.code` / none | 保留 |
| minimax | `minimax.cn.subscription.openai.chat.v1` | `minimax-cn-subscription-openai` | `minimax-cn-subscription-access` / `minimax.subscription-key` | `minimax.subscription-access` / cn | 开放 |
| minimax | `minimax.cn.subscription.anthropic.messages.v1` | `minimax-cn-subscription-anthropic` | 同上 | 同上 | 保留 |
| minimax | `minimax.global.subscription.openai.chat.v1` | `minimax-global-subscription-openai` | `minimax-global-subscription-access` / `minimax.subscription-key` | `minimax.subscription-access` / global | 开放 |
| minimax | `minimax.global.subscription.anthropic.messages.v1` | `minimax-global-subscription-anthropic` | 同上 | 同上 | 保留 |

关键区域/产品边界：MiniMax 一般 API 通过 endpoint 区分 `api.minimax.io=global` 与 `api.minimaxi.com=cn`，Kimi 通过 `api.moonshot.ai=global` 与 `api.moonshot.cn=cn`；BigModel General/Coding 均为 fixed region，CN host 是 `open.bigmodel.cn`、Global host 是 `api.z.ai`；MiniMax Subscription 的 CN host 是 `api.minimax.cn`，Global host 是 `api.minimax.io`（`internal/domain/provider_offering.go:214-281`）。

## 端到端链路

1. **声明源**：Profile 表声明 Type、Connection Group、Surface、Scheme、endpoint、能力上限和 `Withheld`；Surface 表是 Offering 与法律/账户 Region 的唯一权威（`internal/domain/provider_table.go:158-559`，`internal/domain/provider_offering.go:197-283`）。
2. **Admin 投影**：控制面从 `AllProviderProfiles` 过滤保留 Profile，再从 Profile 反查产品身份，不维护第二套产品映射（`internal/app/admin_provider_profiles.go:167-229`、`243-283`）。
3. **凭证创建**：同一 Provider Type 有多个产品身份时必须显式选择 Surface+Scheme；已知跨产品 host 和 fixed-region 冲突被拒绝（`internal/app/admin_providers.go:1193-1205`、`1253-1283`）。ByEndpoint 凭证轮转不能改变或失去可验证账户 Region（`internal/app/admin_providers.go:1208-1229`）。
4. **连接创建**：连接必须与凭证的 Type、audience、Surface、Scheme 一致；保留 Profile 拒绝，能力只能分配给同一 Connection Group 内可服务且不歧义的 Profile（`internal/app/admin_providers.go:1422-1515`、`1579-1684`）。领域对象再次检查 binding 的 Scheme、Surface、Connection Group 和唯一性（`internal/domain/models.go:682-708`）。
5. **注册闸**：Profile operation manifest 来自同一 Profile 表，adapter builder 表必须精确覆盖所有开放 Profile；Registry 注册时再次使 Profile/Surface/Offering/AccountRegion 与 adapter manifest 一致（`internal/provider/profile_bindings.go:73-200`，`internal/app/provider_adapters.go:102-334`、`437-465`，`internal/provider/provider.go:546-594`）。
6. **路由与归因**：loader 从 deployment Surface 和 provider endpoint 派生 AccountRegion，Registry 再派生/校验 Offering，最终 Target 携带 Profile/Offering/AccountRegion（`internal/app/providers.go:697-725`，`internal/provider/provider.go:418-459`、`546-582`）。三者进入预算 attempt、账本、失败捕获和用量聚合；这些持久化边界也检查 Offering/Profile/Region 组合（`internal/budget/manager.go:917-1027`、`1122-1146`，`internal/ledger/event.go:216-228`，`internal/failurecapture/failurecapture.go:262-270`，`internal/usage/aggregate.go:49-51`）。

## 不变量判定

### INV-03：Offering Kind 仅作已验证归因

判定：**通过**。`ProviderOfferingKind` 的生产使用限于产品声明和 Admin 视图；没有进入 adapter 选择、鉴权、路径、重试、价格或预算逻辑（声明：`internal/domain/provider_offering.go:147-169`；展示：`internal/app/admin_provider_profiles.go:114`、`254`）。路由归因使用的是稳定 Offering ID，不按 `subscription`/`entitlement` 改变执行行为。

### INV-04：Profile 唯一映射且 Connection Group 不跨边界

判定：**通过**。Profile 只引用一个 Surface；Surface 唯一给出 Offering 与 Region 规则。组内 Type/Surface/Scheme 一致的全表测试在 `internal/domain/provider_table_test.go:21-50`，连接对象的运行时防御在 `internal/domain/models.go:682-708`。OpenAI/Anthropic 等同一密钥多协议能力可以同组；MiniMax Subscription 和 Kimi Code 的 OpenAI/Anthropic 协议选择被刻意拆组，避免一个 connection 混入协议替代项。

### INV-05：一般 API 与 Coding/订阅面不得串线

判定：**大部分通过；Global MiniMax 留有候选问题 R1-C01**。不同产品用独立 Surface 与 Scheme；选择缺失时多产品类型拒绝猜测（`internal/app/admin_providers.go:1253-1283`），连接与凭证不一致时给出结构化 `credential_surface_mismatch`（`internal/app/admin_providers.go:1490-1506`），已知跨产品/跨 Region host 也在控制面拒绝（`internal/app/admin_providers.go:1193-1205`）。

### INV-06：确认必须绑定 Offering+Region+PolicyRevision

判定：**当前写入通过，升级生命周期失败**。新建/换绑时会精确比较当前 revision，并将 Offering、Surface、Region、文档 URL、revision 和确认值写入与变更同事务的 durable audit intent（`internal/app/admin_providers.go:1291-1331`，`internal/app/admin_audit_intent.go:14-30`、`41-65`、`98-120`）。但没有可供装载时比较的当前有效确认状态，详见 R1-01。

### INV-07：存在性与能力证据分离

判定：**通过**。`/models` 只证明 ID 存在，未知模型能力默认为零（`internal/modelcatalog/catalog.go:1-10`）。不能枚举的 Profile 得到的是 `model_catalog` offer 且 availability 未验证；能枚举的 Profile 刷新失败不会回退成“发现”（`internal/app/admin_invocation_targets.go:353-445`）。Provider metadata mapper 仅处理 `MetadataSourceProvider` 的目标，手输 ID 和本地 offer 不能伪造 provider_metadata 能力（`internal/app/admin_invocation_targets.go:606-652`）。未知/超目录能力必须显式 `operator_declared`，并受 binding ceiling 与依赖规则限制（`internal/app/admin_deployments.go:1157-1212`）。Capability claim 还绑定 provider、target、binding、profile、location 和有效期（`internal/domain/invocation_target.go:71-87`、`155-208`）。

## 确认问题

### R1-01 — 条款 revision 升级不会使既有受限连接失效

- **严重度/置信度**：P1 / 高。
- **可达条件**：版本 N 中，管理员以 revision `R1` 成功创建 BigModel Coding Plan 或 MiniMax Subscription 凭证/连接；版本 N+1 因上游条款变化把对应 Surface 的 `UsagePolicyRevision` 改为 `R2`；原连接保持 enabled，期间不换凭证、不换 Profile。
- **完整路径**：当前 revision 只存在于内存 Surface 表（`internal/domain/provider_offering.go:190-194`、`244-281`）。请求字段只在创建/换绑时传入（`internal/app/admin_providers.go:24-60`）；凭证模型没有已确认 revision（`internal/domain/models.go:197-217`），连接模型也没有（`internal/domain/models.go:400-450`）。创建凭证和连接会校验（`internal/app/admin_providers.go:91-100`、`241-247`），更新连接仅在 `CredentialID` 或 `ProfileID` 变化时重新校验（`internal/app/admin_providers.go:334-346`）。启动/激活 loader 装载 enabled 连接后直接校验凭证和 binding 并创建 adapter，从未比较 policy revision（`internal/app/providers.go:470-557`）；随后 route 正常注册（`internal/app/providers.go:697-725`）。因此 `R1` 确认会在运行效果上自动代表 `R2`。
- **已有防御**：新建/换绑缺确认返回 422，revision 不符返回 409；成功确认以 exact Offering/Surface/Region/revision 进入 audit metadata（`internal/app/admin_providers.go:1301-1331`）；audit intent 和产品变更同事务，投递可恢复（`internal/app/admin_audit_intent.go:14-30`、`98-120`）。这些防御只能证明历史上发生过 `R1` 确认，不能阻止 `R2` 下继续服务。
- **影响**：直接违反 INV-06“旧确认不得自动代表新 revision”。受限产品可以在新条款未确认时继续耗用订阅/entitlement，且控制面没有“确认已过期”的可解释状态；这是发布治理边界，不只是审计展示缺项。
- **修复要求**：持久化“当前有效确认”，至少绑定 Credential ID + Surface/Offering + AccountRegion + UsagePolicyRevision（连接若另需确认则也应绑定 Provider/Binding）。每次 topology 激活/启动都与当前 Surface revision 比较；不相等时 fail closed，只屏蔽该受限 binding/route，并暴露稳定原因码和重新确认入口。迁移不能把历史布尔值或 audit 中任意一次确认自动提升到当前 revision。
- **最小回归**：先以 `R1` 建立受限连接并确认可注册；使用注入/测试表把当前 revision 改成 `R2` 后重载 registry，断言 binding/route 被 withheld；精确确认 `R2` 后才能恢复。另测同 Offering 不同 Region 的确认不能互换。

### R1-02 — 永久 ID 的字面值兼容闸不完整

- **严重度/置信度**：P2 / 高。
- **可达条件**：维护者重命名某个 `ProviderProfileID`、`AccessSurface` 或 `ProviderOfferingID` 常量的字面值，并同步调整所有使用该常量的生产表；对当前开放 Profile，若同时有意更新 Admin golden 则也会绕过兼容保护；对保留/历史 Profile（尤其 Bedrock Runtime/Agent、Kimi Code）无需更新 golden 即可绕过。
- **证据**：Offering 明确声明 ID 永久、只能新增不能改写（`internal/domain/provider_offering.go:80-85`）。Connection Group 的测试把 group 值写成字符串字面量，因此能捕捉组重命名（`internal/domain/provider_table_test.go:53-88`）；但同一测试的 Profile key 使用符号常量。Surface 稳定测试的 map key、期望 Offering 都使用符号常量（`internal/domain/provider_table_test.go:90-130`），所以同时改常量值和表引用仍为绿。Profile/Surface 常量位于 `internal/domain/provider_profile.go:15-156`，Offering 常量位于 `internal/domain/provider_offering.go:61-78`。Admin golden 确实以字面 JSON 保护当前开放矩阵（`internal/app/admin_provider_profiles_golden_test.go:32-48`），但不覆盖全表保留身份，也不是明确的“旧 ID 不可改写”清单。
- **已有防御**：表唯一性、组稳定性、Surface→Offering/Region 关系、Admin 开放矩阵 golden 都有测试；因此随意修改通常会造成较大 diff。缺口是“旧字面 ID 必须仍解析为同一语义”没有全量、直接的机器闸。
- **影响**：这些值进入凭证/连接持久化、Target、预算/账本/usage/failure 和 append-only audit。重命名会把旧记录变成未知身份或把历史归因割裂；编译器和现有领域测试不保证阻止它。
- **修复要求**：新增一个覆盖 34 Profile、18 Surface、14 Offering 的字面值兼容 map，key 和期望值都使用原始字符串，不使用被测符号常量；同时固定每个 Profile 的 Connection Group、Surface、Scheme 和每个 Surface 的 Offering/RegionScope/Region/hosts。新增项允许追加，已有项不得删除、改名或重新指向。保留 Profile 也必须覆盖，因为它们可能对应历史持久化记录。

## 候选问题

### R1-C01 — MiniMax Global 的产品身份无法由 wire/endpoint 验证

- **严重度/置信度**：候选 P1 / 中。
- **可达条件**：管理员持有 MiniMax Global Subscription key，却把凭证声明成一般 `minimax-api` + `bearer.static`（误选或有意规避提示），base URL 仍为 `https://api.minimax.io`；随后创建一般 MiniMax Chat 连接。
- **证据链**：一般 Global Surface 发布 `api.minimax.io`（`internal/domain/provider_offering.go:214-221`），Global Subscription Surface 也发布同一 host（`internal/domain/provider_offering.go:276-281`）。一般 Chat profile 与 Subscription OpenAI Chat profile 的默认 host 相同（`internal/domain/provider_table.go:412-416`、`545-549`）；当前 adapter 都用 Bearer，OpenAI chat path 也相同。`SurfaceForEndpoint` 对一个 host 命中两个 Surface 时按设计返回 unknown（`internal/domain/provider_offering.go:477-525`），因此 `validateCredentialProductRegion` 无法拒绝这个组合（`internal/app/admin_providers.go:1193-1205`）。由于 Scheme 只是管理员提交的标签，而 token 是 opaque，连接创建也无法从材料中识别真实产品。最终一般产品路径不要求 usage acknowledgement，可绕过 Subscription 的 revision 确认。
- **已有防御**：Type 有多个身份时禁止默认猜测；Surface/Scheme 显式且随后不可无声串线；CN 一般 host `api.minimaxi.com` 与 CN Subscription host `api.minimax.cn` 不同，已有负测会拒绝（`internal/app/admin_provider_offering_test.go:452-466`）。这些都不覆盖 Global 同 host/同 wire 的产品误标。
- **为何暂列候选**：代码能确认 Halro 无法区分两类凭证，但尚无本次评审允许使用的非计费 fixture/上游响应证明“Subscription key 在一般 profile 的相同 path 上一定成功”；同时产品契约可能明确把管理员的产品选择视作可信声明。若该信任边界是设计决策，则它应作为 INV-05/06 的明确例外，而不是声称错误产品总能拒绝。
- **裁决所需证据**：第一方契约或脱敏真实响应，确认 Global Subscription key 的有效 host/path，以及能否通过非计费 introspection 得到产品类型。若同 path 可用且没有 introspection，应判为确认 P1：至少需要把产品分类声明与使用条款责任绑定、持续展示不可验证状态，并防止用一般 Surface 绕过受限产品确认；若严格要求机械拒绝，则不能同时开放这两个不可区分的组合。

## 建议项（非 finding）

1. `ProviderInstance.Validate` 仍有一份 Provider Type 硬编码 switch（`internal/domain/models.go:629-633`），而控制面已统一用 `IsRegisteredProviderType`（`internal/app/admin_providers.go:2040-2045`）。`internal/domain/registration_guard_test.go:8-42` 当前能捕捉漏同步，因此不是现存缺陷；建议领域校验也直接复用同一注册表，删除最后一个双源。
2. `IdentityForSurface` 把权威表中的 `Hosts` slice 直接返回（`internal/domain/provider_offering.go:315-328`）。当前调用方只读，未发现可达篡改；建议 clone 后返回，使“表是不可变权威”成为 API 保证，并加 mutation isolation 测试。
3. 把 R1-C01 的信任边界写入产品文档和 Admin 帮助文案：host 能验证 Region 不等于能验证 Offering，尤其同 host 多 Surface 时。当前 `SurfaceForEndpoint` 的 unknown 是正确保守结论，但控制面不应把“未发现矛盾”呈现为“已验证产品”。

## 验证记录

执行的窄测试（均 `-count=1`，均通过）：

```text
go test -count=1 ./internal/domain ./internal/modelcatalog ./internal/provider -run '<R1 domain/profile/offering/group/registration/catalog tests>'
ok github.com/akz142857/Halro/internal/domain       0.358s
ok github.com/akz142857/Halro/internal/modelcatalog 0.919s
ok github.com/akz142857/Halro/internal/provider     1.231s

go test -count=1 ./internal/app -run '<adapter/admin offering/wiring/metadata/catalog tests>'
ok github.com/akz142857/Halro/internal/app          2.189s
```

覆盖的关键现有测试包括：Connection Group 和 Surface 映射（`internal/domain/provider_table_test.go:21-130`）、受限产品 Surface metadata 完整性（`internal/domain/provider_offering_test.go`）、adapter 构建表精确覆盖开放 Profile（`internal/app/adapter_construction_guard_test.go:35-130`）、条款确认写入/audit（`internal/app/admin_provider_offering_test.go:167-245`）、跨产品/跨 Region host 拒绝（`internal/app/admin_provider_offering_test.go:452-485`）以及模型 offer/metadata 归因。

未运行完整 Go gate；本角色没有修改产品代码，且按仓库验证政策只运行能覆盖评审假设的窄测试。未执行任何真实 Provider 调用。

## 证据缺口

- 没有可在测试中替换 `surfaceTable` revision 的 seam，故 R1-01 通过静态完整链路确认，尚无自动化升级复现；这正是建议新增的回归。
- 未读取生产数据库或历史审计，所以没有量化现有受限连接数量，也没有证明 v0.8.0 实际会改变哪一条 policy revision；问题针对下一次 revision 变化是确定可达的。
- R1-C01 缺少真实/脱敏 MiniMax Global Subscription 响应或第一方可机读凭证分类接口，无法在“不调用真实 Provider”的约束下把“同 wire 一定被接受”从中置信度提升到高置信度。
- 稳定 ID 缺口没有通过故意改常量做 mutation test；现有测试结构足以静态证明符号常量同步改名不会被领域兼容测试识别，Admin golden 的部分覆盖已在严重度中折算。
