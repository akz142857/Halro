# 阶段 0 · 范围事实底座（range-map）

> 只抄事实，不下判断。范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`。行号以 2026-08-27 工作区
> HEAD 为准；旧版行号以 `git show v0.3.0:<path> | cat -n` 为准并标注「v0.3.0」。
> 判断留给阶段 1–4；本文与方案描述不符之处集中列在文末（叫停点 1 的输入）。

## 0. 范围复核

- `git diff --stat v0.3.0..HEAD` → 182 files changed, 12913 insertions(+), 1520 deletions(-)。与方案 §2 一致。
- 范围内提交 3 个：`ee96d29`（#229）、`18a8cb3`（#230）、`8bb4847`（#231）。与方案 §2 一致。
- 方案 §2 列出的「未触及」包（`ledger`、`auth`、`adminauth`、`safetransport`、`budget`、`limiter`、
  `tokenguard`、`usage`、`audit`、`contentscan`、`sse`、`circuit`、`idempotency`）在
  `git diff --name-only v0.3.0..HEAD` 下输出为空。已复核，成立。
- v0.3.0 的 bbolt schemaVersion 为 **31**（v0.3.0 internal/store/bolt/store.go:24），HEAD 为 32
  （internal/store/bolt/store.go:24）。本范围只含迁移 32 一个 schema 步进（31→32，非方案 §2.1 所写的 30→32，见文末）。

---

## 表 1 · 能力词表迁移表（`json_mode` → `json_object` + `structured_outputs`）

### 1.1 读点与写点清单

**后端 · 词表本体（domain）**

| 位置 | 角色 | 内容 |
|---|---|---|
| internal/domain/capability_dictionary.go:30-31 | 词表定义 | `json_object`/`structured_outputs` 两个词表项，绑定 `ProviderCapabilities.JSONObject/.StructuredOutputs` |
| internal/domain/capability_dictionary.go:57-64 | 按名读 | `CapabilityValue`，未知名返回 `(false,false)` |
| internal/domain/capability_dictionary.go:69-77 | 按名写 | `SetCapability`，未知名拒绝写入（返回 false） |
| internal/domain/models.go:491-492 | 存储字段 | `JSONObject bool json:"json_object"`、`StructuredOutputs bool json:"structured_outputs"`；479-490 为拆分理由注释 |
| internal/domain/models.go:946-948 | 校验读 | Deployment.Validate：两半任一为真则要求 Chat 能力 |
| internal/domain/capability_modality.go:70 | 模态映射 | 两半列入 `nonModalCapabilities`（不表达任何输入/输出模态） |
| internal/domain/provider_table.go:87-92 | 档案默认/上限 | `openAIChatSet` 两半皆真 |
| internal/domain/provider_table.go:104-108 | 档案默认/上限 | `anthropicMessagesSet` 只有 `StructuredOutputs`（无 JSONObject，99-103 注释说明 Anthropic 无 schema-less 模式） |
| internal/domain/provider_table.go:122-125 | 档案默认/上限 | `openAIResponsesSet` 两半皆真 |
| internal/domain/provider_table.go:290-293 | 档案默认/上限 | DeepSeek 只有 `JSONObject`（290 注释：无 schema 模式） |
| internal/domain/provider_table.go:306-318、361 | 档案默认/上限 | Bedrock Mantle 三档两半皆真（306 注释：两半皆有正是旧单 bit 所说的） |
| internal/domain/provider_table.go:453-454 | 依赖表 | `json_object`→chat、`structured_outputs`→chat，两半互不依赖（450-452 注释给出理由） |
| internal/domain/provider_profile.go:293-303 | 证据校验读 | `CapabilityEvidenceSet.Validate` 要求词表每个名字都有证据（296-298），未知证据名拒绝（301-302） |
| internal/domain/provider_profile.go:310-317 | 证据校验读 | 双条件：启用能力不得 unsupported、禁用能力必须 unsupported（迁移 32 注释 store.go:827-832 引用的就是这条） |
| internal/domain/invocation_target.go:140、187 | 校验读 | 能力 claim 名不在词表 → 拒绝；未知 claim 不得带正向证据 |
| internal/domain/model_capability_detection.go:346-347 | 推荐读 | 完成判定读取 `Recommended.JSONObject/.StructuredOutputs` |
| internal/domain/model_capability_detection.go:393-396 | 依赖闭包写 | chat 探测不通过时两半连同其他 chat 系能力一起清零 |

**后端 · 路由与请求语义**

| 位置 | 角色 | 内容 |
|---|---|---|
| internal/semantic/request.go:25-26 | 需求字段 | `Requirements.JSONObject/.StructuredOutputs`（20-24 注释：一个需求拦不住 schema 请求落到只有 schema-less 的目标） |
| internal/semantic/request.go:98-99 | 输出格式 | `OutputJSONObject`/`OutputJSONSchema` 两种 kind |
| internal/semantic/request.go:205-206 | 需求推导写 | `DeriveRequirements`：`json_object` kind→JSONObject 需求，`json_schema` kind→StructuredOutputs 需求 |
| internal/gateway/service.go:2537-2538 | 需求↔能力配对 | `capabilityRequirements` 表中两半各一行 |
| internal/gateway/service.go:958、2545-2554 | 路由过滤读 | `filterSemanticCapabilities` 按上述配对删除不满足的 target |
| internal/app/providers.go:822-823 | 生效能力写 | 注册表加载时 生效 = 接口可用 && 部署声明，两半各自逐位与 |
| internal/app/admin_invocation_targets.go:778-787 | 依赖闭包写 | `dependencyClosure`：无 chat 则两半清零（781） |

**目录（modelcatalog）**

| 位置 | 角色 | 内容 |
|---|---|---|
| internal/modelcatalog/builtin.go:132-140 | 目录基线写 | `chat()` 共享形状含 `JSONObject: true`，不含 StructuredOutputs（132-136 注释：schema-less 是全家族共有，schema 是逐模型的） |
| internal/modelcatalog/builtin.go:172-180 | 目录基线写 | `structuredOutputs` 修饰器；172-175 注释：本目录收录的 OpenAI 模型均为 gpt-4o-2024-08-06 及以后 |
| internal/modelcatalog/snapshot.go:23-26 | 词表版本 | `CapabilityDictionaryVersion = 2`，23-25 注释：v1 目录用的名字本读者已不认识 |
| internal/modelcatalog/snapshot.go:232-233 | 版本门 | 远端目录词表版本 ≠ 2 即拒绝（详见表 4） |

**探测（detection）**

| 位置 | 角色 | 内容 |
|---|---|---|
| internal/provider/capability_detection.go:149-153 | 探针计划写 | JSONObject → `json_object` 探针；StructuredOutputs → `json_schema` 探针，各自独立 |
| internal/provider/capability_detection.go:274-276 | 探针载荷 | `json_object` 探针发 `response_format {"type":"json_object"}` |
| internal/provider/capability_detection.go:287-297 | 探针载荷 | `json_schema` 探针发 strict json_schema；288-295 注释：json_object 探针替代不了它 |
| internal/provider/capability_detection.go:67-70 | 契约版本 | v5 注释：v4 结果的 json_mode 探针「答的是一个不再被问的问题」 |

**协议翻译（compatibility）**

| 位置 | 角色 | 内容 |
|---|---|---|
| internal/openaiapi/types.go:118 | Chat wire 校验 | `response_format.type` 接受 text/json_object/json_schema |
| internal/openaiapi/responses.go:152-165 | Responses wire 校验 | `text.format.type` 同上三种；json_schema 要求 name+schema |
| internal/compatibility/openai/mapping.go:433-457 | Chat 翻译 | json_schema 的解码/渲染 |
| internal/compatibility/openai/responses.go:55-58 | Responses 翻译 | `text.format` → 语义 OutputFormat |
| internal/compatibility/anthropic/mapping.go:410、428-474 | Anthropic 翻译 | json_schema→`output_config.format`；474 注释：json_object 无 Anthropic 对应 |
| internal/compatibility/anthropic/native.go:134-135 | native 需求 | `output_config.format` 只 raise StructuredOutputs 需求 |
| internal/compatibility/deepseek.go:175-178 | DeepSeek 渲染 | 只接受 text/json_object，schema 在 wire 层拒绝 |
| internal/compatibility/provider_fields.go:177-181 | 字段规则 | DeepSeek 档案对 json_schema 请求声明 `response_format` 不可承载 |

**前端（web/src）**

| 位置 | 角色 | 内容 |
|---|---|---|
| web/src/types.ts:363-368 | 类型 | `ProviderCapabilities.json_object` / `.structured_outputs` 字段声明 |
| web/src/hooks/useProviderProfiles.ts:31 | 零值写 | `emptyCapabilities` 两半皆 false |
| web/src/pages/DeploymentsPage.tsx:1209 | 渲染分组 | `protocol` 组列出两半，各自独立复选框（经 2380 渲染） |
| web/src/pages/DeploymentsPage.tsx:2615-2616 | 零值写 | 表单 `emptyCapabilities()` 两半皆 false |
| web/src/i18n/locales/en-US.ts:723 / zh-CN.ts:729 | 文案 | "JSON object"/"Structured outputs"；「JSON 对象模式」/「结构化输出」 |
| web/src/test/provider-profiles.golden.json:9、24 及全文约 70 对布尔项 | 测试基线 | 两半在每个档案基线中成对出现 |
| web/src 全目录 | 旧词残留 | `json_mode` 零命中（仅 DeveloperPage 的 `developer.jsonMode` i18n 键指「原始 JSON 编辑 tab」，与能力无关：en-US.ts:153、zh-CN.ts:159、DeveloperPage.tsx:361） |

**端点清单与文档**

| 位置 | 角色 | 内容 |
|---|---|---|
| docs/compatibility/endpoint-manifests.json:117、138、544、808 | 对外契约 | json_object 的逐档案承载说明（Anthropic 无表示、DeepSeek 无 schema 模式等） |
| docs/contracts/provider-capabilities.md:25 | 对外契约 | 两种 JSON 格式是两个独立需求 |
| docs/guides/model-capability-detection.zh-CN.md:31-32 | 指南 | 两个探针各自发什么 |
| docs/contracts/openai-compatibility.md | 检索结果 | 该组标识零命中 |
| docs（contracts/compatibility/guides 之外） | 旧词残留 | `json_mode` 仅残留于 prd/review/todo 类历史文档（如 docs/prd/deployments-model-catalog-ui.zh-CN.md:123 等），contracts/compatibility/guides 目录零残留 |

### 1.2 迁移 32 前后字段值

迁移体：internal/store/bolt/store.go:839-898（`structured_output_capability_split`），辅助函数 906-992。
对一个 v0.3.0 下 `json_mode=true`、证据为 verified 的存量部署：

| 存储位置 | 迁移前（schema 31） | 迁移后（schema 32） | 执行行 |
|---|---|---|---|
| deployments.capabilities | `"json_mode": true`（v0.3.0 internal/domain/models.go:479） | `json_mode` 键删除；`json_object=false`、`structured_outputs=false` | store.go:918-921（经 857-858 调用） |
| deployments.capability_evidence | `"json_mode": "verified"` | `json_mode` 键删除；两半均 `"unsupported"` | store.go:945-948 |
| deployments.operator_disabled | 可能含 `"json_mode"` | 该项被移除；未加入任何后继名 | store.go:861、964-992 |
| deployments.model_capability_snapshot.capabilities / .evidence | 同上旧形 | 同上拆分 | store.go:864-883 |
| providers.capabilities / .capability_evidence 及每个 binding | 同上旧形 | 同上拆分 | store.go:849-854 |
| 探测三桶（detections / idem / index） | 存量探测记录，DetectorVersion=`capability-detector-v1`（v0.3.0 internal/provider/capability_detection.go:15） | 整桶删除并重建为空 | store.go:887-896 |
| meta.schema_version | 31 | 32 | store.go:1642-1649 |
| migration_history | 无 32 条目 | `{version:32, name:"structured_output_capability_split"}` | store.go:1653-1667 |

两半皆置 false 的理由原文在 store.go:818-825 注释（「Off refuses a request; on forwards one that the
upstream will reject after the budget is already reserved」）；证据同步置 unsupported 的原因是
provider_profile.go:315-316 的双条件（禁用能力必须 unsupported，否则记录拒载）。

### 1.3 「两半皆关」对存量部署的可见后果

**路由语义变化**：

- 携带 `response_format/text.format` 为 `json_object` 的请求 derive 出 JSONObject 需求
  （internal/semantic/request.go:205），`json_schema` derive 出 StructuredOutputs 需求（:206）。
- `filterSemanticCapabilities`（internal/gateway/service.go:958，配对表 2537-2538）把两半皆关的
  target 从候选中删除；候选清空时返回 400 `unsupported_feature`，消息
  "model route does not support the requested chat capabilities"，并附 `unservableReasons`
  列出缺失能力名（service.go:961-966、2467）。
- 不带这两种输出格式的请求不受影响：迁移后的部署对普通 chat 流量仍可路由（其 Chat 能力未被触碰，
  store.go:843-847 只处理 json_mode 一族）。「从可路由变为不可路由」的准确范围是：**该部署对
  JSON 输出类请求（旧 json_mode 流量）不再是路由候选**；若某公开模型的全部候选都是此类存量部署，
  则该模型的 JSON 输出请求整体不可服务。

**运维在哪里能看到（逐信号落行号）**：

| 信号面 | 有/无 | 位置与内容 |
|---|---|---|
| migration 记录 | 有 | bbolt `migration_history` 桶写入 32 号记录（store.go:1653-1667）。未找到 internal/app 或 cmd/halro 中读取该桶并对外展示的代码（grep `migration_history`/`MigrationHistory` 于 internal/app、cmd/halro 零命中，仅 pricing 专用迁移工具的文案 cmd/halro/main.go:151-201 与此无关） |
| 启动日志 | 未找到 | 迁移执行体与 `initialize`（store.go:1598-1677）不含任何日志输出。启动期能力相关日志只有 `logCapabilityWithholdings`（internal/app/providers.go:286-308），它只报 Drifted/Dangling/Excluded 三类；两半皆关的部署声明未超上限，不落入 Drifted（判定在 internal/app/capability_drift.go:97-104），故未找到本迁移触发的启动日志 |
| doctor | 间接、聚合计数 | `capability_drift` 检查（internal/app/doctor.go:543-581）。两半皆关不产生 fail（不是 drifted）；当部署 review 状态为 available 时产生 warn「N deployment(s) have catalog capabilities available for review; they keep serving what they already declare」（doctor.go:575-577），只报计数不报部署 ID（570-571 注释）。该状态成立与否取决于目录 entry revision 与快照 model_revision 是否不同（capability_drift.go:111-120）；迁移 32 不改快照的 model_revision（store.go:839-898 无该字段操作），目录侧 entry 因词表拆分而内容变化——每个存量模型的 revision 是否必然变化，未核实。doctor 的 `metadata` 行报告的是二进制侧 schema 常量（doctor.go:119「bbolt schema v32」），不区分是否刚做过迁移 |
| 控制台 | 有（一般性状态，非迁移专属） | 部署卡片结论行（web/src/pages/deploymentCondition.ts:115：`capabilitiesToReview`，quiet 级）；详情抽屉 `CapabilityReviewNotice`（web/src/pages/DeploymentsPage.tsx:665-702）在 review=available 时渲染 `deployments.canEnable` 列出可补勾的能力名（694-698）。因迁移未把后继名写入 operator_disabled（store.go:861），两半不会被展示为「已由管理员关闭」（`switchedOff`，DeploymentsPage.tsx:699）而是（在目录覆盖该模型时）落入 AvailableForReview（capability_drift.go:209-216）。抽屉能力区逐条证据渲染在 DeploymentsPage.tsx:552-563（`unsupported` 显示为「不再受支持」，zh-CN.ts:773） |
| 调用方 | 有（失败时） | 上述 400 `unsupported_feature` 响应（service.go:961-966），错误体含缺失能力名 |

没有任何一处信号写明「由迁移 32 关闭」；上述控制台/doctor 信号与「操作者从未勾选过」的部署呈现相同状态。

### 1.4 表 1 遗留未核实点

- 目录 entry revision 是否因词表拆分对**每个**存量模型都发生变化（决定 doctor warn 与控制台
  review 提示是否必然出现）——需要 R2/R3 实测。
- 详情抽屉能力区是否列出「已关闭」的能力（还是只列启用的），即操作者能否在抽屉里直接看到两半为关
  ——DeploymentsPage.tsx:552-563 的渲染范围未逐行核实。
- 存量部署若 model_capability_snapshot 缺失（record["model_capability_snapshot"] 不存在，
  store.go:864-866 直接跳过），review 路径行为未核实。

---

## 表 2 · 热路径改道表（一元生成，facade → Provider）

HTTP 路由注册两版相同：`/v1/chat/completions`、`/v1/responses`、`/v1/messages` 均由
`gatewayapi.Handler` 承接（internal/app/runtime.go:1334、1335、1357）。facade 包本范围内仅改
一处：529 状态判定不再采信上游 `ProviderCode` 字符串（internal/gatewayapi/handler.go 范围 diff
唯一 hunk，位于 `renderAnthropicGatewayError`）。v0.3.0 与 HEAD 一样，facade 不做语义翻译、
不触碰脱敏——两版的翻译/脱敏/估算全部在 `gateway.Service` 内。

### 2.1 逐跳对照（Chat / Responses 主链）

| 跳 | v0.3.0 | HEAD | 变化 |
|---|---|---|---|
| facade 进入 service | `Handler.ChatCompletions` → `service.Chat(ctx, key, wire)`（v0.3.0 handler.go:726、762） | 同（handler 本范围未改此路径） | 无 |
| wire→semantic 翻译 | 在 `Service.Chat` 内部、resolve 之后（v0.3.0 service.go:903 `DecodeGenerate`） | 在 facade 方法开头、进入 `generate` 之前：Chat 于 service.go:915，Responses 于 1100，ResponsesStream 于 1124（`DecodeResponseGenerate`，internal/compatibility/openai/responses.go:15） | 翻译上移到共同入口前；`generate`（944）只接 `semantic.GenerateRequest` |
| Responses 的中转 | Responses→Chat wire→再解码的往返（v0.3.0 service.go:1049、1053、1057；HEAD 注释 1088-1091 描述该旧形） | 直接 `generate`（service.go:1104），结果 `RenderResponseResult`（1108） | Responses 不再经过 Chat wire，Chat 表达不了的字段（provider-executed tool、citations）得以通过（注释 930-939） |
| 认证/路由解析 | `resolveRequest`（v0.3.0 :896，实现 215） | `resolveRequest`（service.go:950，实现 214） | 无 |
| 能力过滤（第一段） | 3 道：语义能力/档案字段/primitive（v0.3.0 :908-910） | 同 3 道（service.go:958-960，实现 2545/2556/2568） | 无（过滤对象改为语义请求） |
| **入站脱敏** | `ProcessInboundChat`，作用于 **wire** 请求，脱敏后**重新翻译**语义请求（v0.3.0 :917、921） | `ProcessInboundGenerate`，作用于 **semantic** 请求（service.go:967；实现 internal/redaction/engine.go:278），无二次翻译 | 脱敏对象从 wire 改为 semantic；两版都在能力过滤之后、估算与预留之前 |
| 脱敏后守卫 | 无（HEAD 注释 service.go:2974-2976：旧版重新推导需求但不比较，「按一组需求路由、按另一组执行」） | **新增** `redactionPreservedRequirements`（service.go:971，实现 2977-2983）：脱敏改变了请求需求即 400 拒绝，位于预留之前 | 新增 |
| **token 估算** | `estimateGenerateInputTokens(request.EstimatedInputBytes(), canonical)`，字节数来自**脱敏后的 wire** 请求（v0.3.0 :925；wire 字节计数 v0.3.0 internal/openaiapi/types.go:127） | 同名函数（service.go:974，实现 2944），字节数来自 **semantic** 请求 `canonical.EstimatedInputBytes()`（internal/semantic/request.go:285-302；注释 276-280：同一内容不因端点而估得不同） | 估算输入源改变；位置不变（脱敏后、预留前） |
| 输出 token / 溢出 / token 过滤 | v0.3.0 :929、933、937 | service.go:978、982、986 | 无 |
| 请求级准入 | `beginRequestRun`：TokenGuard→limiter→requestID→`BeginRequestDetailed`（v0.3.0 :941，实现 254） | 同（service.go:990，实现 253：TokenGuard 260、limiter 264、账本开单 278） | 无 |
| **预算预留** | `startAttempt` 内 `ReserveAttemptDetailed`/`ReserveLeaseDetailed`（v0.3.0 :458/460） | 同（service.go:457/459；价格 pin 407、TokenGuard 复核 437、`MarkStarted` 499） | 无 |
| **Provider I/O** | `generation.Generate(... Request: canonical)`（v0.3.0 :982） | 同（service.go:1031） | 无 |
| **attempt.finish** | v0.3.0 :993（实现 559） | service.go:1035（实现 558） | 无 |
| **出站脱敏** | `ProcessOutboundChat`，在 finish **之后**（v0.3.0 :997） | `ProcessOutboundGenerateResult`，在 finish **之后**（service.go:1039，实现 engine.go:299） | 对象改为 semantic 结果；相对位置不变 |
| **run.finalize** | 出站脱敏之后（v0.3.0 :1004），成功/策略拒绝各自 finalize | 同（service.go:1046，outcome=`success` 或 `policy_rejected`；422 在 finalize 之后返回 1051-1055） | 无 |
| provider 错误收尾 | finalize("provider_error")（v0.3.0 :1019、1031） | 同（service.go:1065、1077） | 无 |

`generate` 头注释（service.go:941-943）自述：「The steps and their order are unchanged. Resolution
and capability filtering come first, then redaction, then the estimate the reservation is made
against, and only then any provider work.」

### 2.2 各 facade 在 HEAD 的入链方式

| facade | HEAD 入链 | 位置 |
|---|---|---|
| OpenAI Chat（一元） | wire→semantic→`generate` | service.go:907-928 |
| OpenAI Responses（一元） | wire→semantic→`generate` | service.go:1092-1113 |
| OpenAI Responses（流式） | wire→semantic→`generateStream`（chunk 再渲染回 Responses 事件） | service.go:1115-1166 |
| Anthropic Messages · portable（一元） | **仍经 Chat wire 中转**：`DecodePortable`→`RenderGenerateRequest`（语义→Chat wire）→`s.Chat`（Chat 内再 wire→semantic） | service.go:1171-1200（1179、1183、1187）；v0.3.0 同形（:1140、1144、1148） |
| Anthropic Messages · native（一元） | 独立链，不经 `generate`：入站 inspect 脱敏（1270）→startAttempt（1282，预留仍在 457/459）→`MessagesNative`（1291）→出站脱敏（1312，**在 finish 之前**）→finish（1327）→finalize（1337） | service.go:1256-1360；v0.3.0 同形（出站脱敏 1273 < finish 1288） |

### 2.3 相对次序底座（B8 判定用，HEAD 一元 portable 链）

```
能力过滤(958-960) < 入站脱敏(967) < 需求保持守卫(971) < token 估算(974-985)
  < token 过滤(986) < TokenGuard/limiter/账本开单(990→260/264/278)
  < 预算预留(1010→457/459) < MarkStarted(499) < Provider I/O(1031)
  < attempt.finish(1035) < 出站脱敏(1039) < run.finalize(1046) < 响应返回
```

出站脱敏在两版中都位于 attempt.finish 之后、run.finalize 之前；native 链在两版中都相反
（出站脱敏在 finish 之前，v0.3.0 注释 1266-1272 声明策略拒绝不退款是刻意的）。

### 2.4 表 2 遗留未核实点

- `generateStream`（service.go:1891）与 `MessagesStream`/`MessagesNativeStream` 的流式次序未逐跳
  比对（任务范围限定一元）。
- HEAD `Service.Embeddings`（2087）链未比对。
- v0.3.0 与 HEAD 的 `settlementForResult` 语义是否逐字段一致，未比对（两版行号 2594/待查）。

---

## 表 3 · Responses 档案表（`openai.responses.v1`）

### 3.1 档案本体

| 维度 | 值 | 位置 |
|---|---|---|
| Profile ID | `openai.responses.v1` | internal/domain/provider_profile.go:28 |
| Manifest | Revision 1、ProviderType OpenAI | internal/provider/profile.go:397-402 |
| Access Surface | `domain.SurfaceOpenAI`（与 chat 档案同一 surface） | internal/provider/profile.go:399 |
| 凭据方案 | `domain.CredentialBearerStatic`（与 chat 档案相同） | internal/provider/profile.go:399 |
| Operations | 仅 `OperationChat`，无 stream、无 embeddings | internal/provider/profile.go:400 |
| Primitive | `PrimitiveOpenAIResponses`（"openai.responses"） | internal/provider/profile.go:72、401；internal/provider/primitive.go:23、254、380 |
| 端点模板 | `https://api.openai.com`，实际 POST 路径 `responses` | internal/domain/provider_table.go:149；internal/provider/openai/adapter.go:434 |
| 表内位序 | 刻意排在 chat 档案之后：二者共享 (type,surface,scheme)，凭据解析取首个匹配（142-146 注释：上移会静默改指端点） | internal/domain/provider_table.go:142-152 |

### 3.2 能力默认与上限

`openAIResponsesSet`（internal/domain/provider_table.go:122-125）：

- **默认**：Chat、Tools、Vision、FetchedImage、JSONObject、StructuredOutputs、DeveloperRole。
- **上限**：默认 + `ProviderExecutedTools`（provider_table.go:151，`withProviderExecutedTools` 327-329）。
- **刻意缺席**（118-121 注释）：streaming（该档案不绑流 primitive）、embeddings（在 chat 档案上）、
  reasoning（canonical 映射承载不了 reasoning item，「承载不了的声明就是预留之后才失败的请求」）。

### 3.3 `web_search` 的准入条件与默认值

准入链每一环（全部在 Provider I/O 之前）：

1. **wire 校验**：`tools[].type=web_search` 是唯一被接受的 hosted tool，且不得带
   name/description/parameters/strict（internal/openaiapi/responses.go:130-134）；
   其余非 function 类型（含 code_interpreter、file_search）在 :136-138 被拒，错误文案
   "tools[N] must be a named function or a supported provider-executed tool"。
2. **语义模型**：`ProviderToolWebSearch = "web_search"` 是语义模型携带的唯一 provider-executed
   工具（internal/semantic/request.go:64）；`Validate` 拒绝任何其他名字或带 schema/description 的
   provider-executed 工具（request.go:147-156）。code_interpreter/file_search 被排除的理由写在
   request.go:57-63（两者都是 provider 侧状态，单进程单数据目录放不下别人的状态句柄）。
3. **需求推导**：provider-executed 工具 raise `ProviderExecutedTools` 需求而非 Tools 需求
   （request.go:208-219）。
4. **能力过滤**：需求↔能力配对 `provider_executed_tools`（internal/gateway/service.go:2542），
   经 filterSemanticCapabilities（958）要求 target 生效能力含 ProviderExecutedTools；生效能力 =
   接口可用 && 部署声明（internal/app/providers.go:829）。
5. **档案字段过滤**：`providerExecutedToolProfiles = [ProfileOpenAIResponses]` 是唯一能承载它的
   wire（internal/compatibility/provider_fields.go:285，判定 272）；Anthropic 档案虽上限允许该
   能力但 portable 路径明确不在此列（276-284 注释）。
6. **默认值**：`ProviderExecutedTools` 不在 Defaults、只在 Ceiling（provider_table.go:150-151），
   即**默认关闭，需操作者显式勾选**。
7. **知情开启**：`CapabilityOptInWarnings() = ["provider_executed_tools"]`
   （provider_table.go:475-477；462-471 注释写明该流量不经 SafeTransport、无主机白名单、无 DNS
   pinning、审计无痕），经 Admin API 下发给控制台（internal/app/admin_provider_profiles.go:86、134）。
8. **依赖**：`provider_executed_tools` 依赖 chat（provider_table.go:457）。

出站方向：上游的 `web_search_call` item 解码为 `ContentProviderToolCall`
（internal/compatibility/openai/provider_responses.go:155-173）；渲染回 Responses wire 在
internal/compatibility/openai/responses.go:306-311。语义→provider 渲染时非 web_search 的
provider-executed 工具再次被拒（provider_responses.go:48-52）。

### 3.4 与 `openai.chat.v1`（`openai.chat-embeddings.v1`，internal/domain/provider_profile.go 定义、profile.go:391-396）的逐项差异

| 项 | openai.chat-embeddings.v1 | openai.responses.v1 | 位置 |
|---|---|---|---|
| Operations | Chat、ChatStream、Embeddings | 仅 Chat | profile.go:394 / 400 |
| Primitive | `openai.chat_completions`（+stream、embeddings） | `openai.responses` | profile.go:71-72 |
| 上游路径 | `chat/completions` | `responses` | internal/provider/openai/adapter.go:373 / 434 |
| Surface / 凭据 | SurfaceOpenAI / BearerStatic | 相同 | profile.go:393 / 399 |
| 默认能力 | Chat、Streaming、Embeddings、Tools、Vision、FetchedImage、JSONObject、StructuredOutputs、DeveloperRole、Reasoning、StreamUsage | Chat、Tools、Vision、FetchedImage、JSONObject、StructuredOutputs、DeveloperRole（无 Streaming/Embeddings/Reasoning/StreamUsage） | provider_table.go:87-92 / 122-125 |
| 上限 | = 默认（无 ProviderExecutedTools） | 默认 + ProviderExecutedTools | provider_table.go:140 / 151 |
| 语义生成入口 | Chat wire（`provider.ChatCall`） | `GenerateSemantic`（semantic 直达；非 responses 档案调用即拒，adapter.go:421-424） | adapter.go:347-385 / 412-447 |
| 探测/连接测试所走表面 | chat/completions | 仍以 Chat 形提问，但经 `chatViaResponses` 翻译后打到 `/v1/responses`（348-357 注释：探到别的表面会「给一个从不调用的表面存 verified 证据」，key 只授一边时会「探测通过、首个真实请求在预留之后失败」） | adapter.go:347-357、387-410 |
| 端点清单字段限制 | is_error 一项 | `messages[].name`、`n`、`stop`、`seed`、`reasoning_effort` 等不可承载 | internal/compatibility/manifest.go:245；internal/compatibility/provider_fields.go:129-143 |
| 北向端点清单 | — | `openai.responses.stateless.v1` 端点条目（Path `/v1/responses`） | internal/compatibility/manifest.go:290；docs/compatibility/endpoint-manifests.json:336-337、477、506-510 |

上游侧固定行为：`store=false` 恒发（internal/compatibility/openai/provider_responses.go:30-31 的
`Store: &store`；北向 `store=true` 在 wire 校验即拒，openaiapi/responses.go:106-108）、
`request.Stream=false` 恒置（adapter.go:429）。

### 3.5 表 3 遗留未核实点

- Responses 档案上 reasoning 请求的实际拒绝形态（能力过滤 + provider_fields.go:142 字段声明两道，
  哪道先命中、错误文案是什么）未实测。
- `chatViaResponses` 往返对探针语义的保真度（九个探针 kind 逐个经翻译后仍成立与否）未核实
  ——对应方案 S2。
- Bedrock Mantle 的四个 Responses/OpenAI 档案与本表的差异未展开（方案 R6 涉及）。

---

## 表 4 · durable 版本表

### 4.1 bbolt schemaVersion 32

| 角色 | 位置 | 内容 |
|---|---|---|
| 常量 | internal/store/bolt/store.go:24 | `const schemaVersion uint64 = 32`；`CurrentSchemaVersion()` :39 |
| 写者 | store.go:1598-1677（`initialize`） | 逐个应用缺失迁移（1615-1637），每步写 `meta.schema_version`（1642-1649）并追加 migration_history（1653-1667） |
| 读者（可写打开） | store.go:1601-1614 | 读出当前版本；**大于 32 即拒**："metadata schema version %d is newer than this build supports (%d)"（1609-1613）；小于 32 则原地升级 |
| 读者（只读打开） | store.go:1302-1304 | **不等于 32 即拒**（精确相等门）："metadata schema version %d does not match required version %d"；doctor 走此路径（internal/app/doctor.go:119 报 "bbolt schema v%d"，失败落 `metadata: fail`） |
| 迁移链完整性 | store.go:1617-1620 | 链上版本号错位即拒；1670-1674 要求全部必需桶存在 |

### 4.2 探测契约（HEAD 为 v5，非 v4；见文末出入）

| 角色 | 位置 | 内容 |
|---|---|---|
| 常量 | internal/provider/capability_detection.go:70 | `"capability-detector-v5"`；47-69 注释记录 v2/v3/v4/v5 各自语义（v4 = #230 的拒绝分类，v5 = #231 的词表拆分）。v0.3.0 时为 `"capability-detector-v1"`（v0.3.0 同文件 :15） |
| 写者 | internal/app/admin_model_capability_detections.go:202 | 新探测记录写 `DetectorVersion: CapabilityDetectorContractVersion`；计划本身也携带（capability_detection.go:183） |
| 读者 1 · 冷却指纹 | admin_model_capability_detections.go:159-163、186 | SelectionFingerprint 的哈希材料含 `detector_version`；旧契约记录指纹不同，不参与冷却比对（旧记录被绕过而非报错） |
| 读者 2 · 建部署核验 | internal/app/admin_deployments.go:692-696 | 期望指纹由当前契约版本重算（`detectionTargetFingerprint`，admin_model_capability_detections.go:421-428 含 `detector_version`）；存量探测的 TargetFingerprint 与之不符 → `errCapabilityDetectionStale` 拒绝（694-695） |
| 读者 3 · 记录自校验 | internal/domain/model_capability_detection.go:217-218 | `DetectorVersion == ""` 的记录无效 |
| 兜底 | store.go:887-896 | 迁移 32 把三个探测桶整体清空——本范围内旧契约记录实际上不存在于升级后的库 |
| 探针预算 | capability_detection.go:16-27 | `maxDetectionProbes = 9`（常量 + 超额记入 Deferred，130-133）；v0.3.0 为字面量 8 且超额**提前返回、无 Deferred 记录**（v0.3.0 同文件 :68） |

### 4.3 能力词表 v2

| 角色 | 位置 | 内容 |
|---|---|---|
| 常量 | internal/modelcatalog/snapshot.go:26 | `CapabilityDictionaryVersion = 2`（v0.3.0 同文件 :23 为 1） |
| 写者 | snapshot.go:85-88 | `BundledSnapshot()` 给内置目录盖 v2；远端目录由外部签发方写入该字段（Snapshot 结构 :39） |
| 读者 · 远端目录门 | snapshot.go:232-233 | **「远端目录在旧词表下发布会被拒」成立于此行**：`snapshot.CapabilityDictionaryVersion != CapabilityDictionaryVersion` → "catalog capability dictionary %d is incompatible with reader %d"（精确相等门；schema_version 则是范围门 :229-230） |
| 读者 · 存量记录门 | internal/domain/provider_profile.go:293-303 | 证据集必须覆盖词表全部名字（296-298）且不得含词表外名字（301-302）——仍带 `json_mode` 或缺两半的旧记录在读取校验时被拒；这正是迁移 32 必须同时改能力与证据的原因（store.go:827-832 注释） |
| 读者 · 名字写入门 | internal/domain/capability_dictionary.go:69-77 | `SetCapability` 拒绝未知名 |
| 词表↔目录联动 | snapshot.go:23-25 注释 | 「v1 目录命名的能力本读者已不携带，其条目无法读成任何一半」 |

### 4.4 表 4 遗留未核实点

- 三个版本门均未在真实存量数据目录上实测（对应 R2/R3）；只读精确相等门（store.go:1302）在
  「HEAD 二进制 + 未升级数据目录」组合下的 doctor 表现未实测。
- 远端签名目录功能在 v0.3.0 与 HEAD 之间是否有其他读者（除 snapshot.go 验证链外），未穷尽。
- backup/restore 路径对 schema 32 的处理（internal/app/backup.go:134-135 记录前后版本）未展开。

---

## 与方案 §2 描述的出入

1. **「探测契约 v4」与实际不符。** 方案 §2 表（review-plan.md:44）与 §4 表 4（:100）均写
   「探测契约 v4」。实际：HEAD 常量为 `"capability-detector-v5"`
   （internal/provider/capability_detection.go:70），v0.3.0 为 `"capability-detector-v1"`（v0.3.0
   同文件 :15）。本范围内契约从 v1 直接推进到 v5（v2/v3/v4/v5 四个号在注释中各有语义，其中 v4
   对应 #230、v5 对应 #231）。方案 §4 表 4「探测契约 v4 的写者/读者」应按 v5 核。
2. **「schema 30→32」与实际不符。** 方案 §2.1（review-plan.md:57）写「schema 30→32，含数据搬移的
   迁移 32」。实际：v0.3.0 的 schemaVersion 为 **31**（v0.3.0 internal/store/bolt/store.go:24），
   本范围步进为 **31→32**，仅迁移 32 一个；迁移 31（fetched_image）在 v0.3.0 已存在。
3. **「gateway/service.go（+268 行）」「redaction/engine.go（+196 行）」是变更总行数而非净增。**
   实际 numstat：service.go +170/−98（合计 268），engine.go +171/−25（合计 196）。方案的数字
   本身能对上，但「+N 行」的表述与 diffstat 的含义不同。
4. **「脱敏与 token 估算随之上移」的准确形状。** 方案 §2（review-plan.md:41）表述为语义请求上移
   带动两者上移。实际：两者在 v0.3.0 已经位于 service 层、且已在预算预留之前（v0.3.0
   service.go:917、925）；本范围改变的是**作用对象**（wire → semantic，HEAD service.go:967、974）
   与 Responses/Chat 的**入链翻译位置**（facade 方法内先行 Decode），相对次序不变（HEAD 注释
   941-943 明言 order unchanged）。新增物是脱敏后需求保持守卫（971，实现 2977）。
5. **Anthropic portable Messages 未改道。** 方案 §2 的「一元生成热路径改为以 semantic request
   进入」对 Chat 与 Responses 成立；Anthropic portable Messages 在 HEAD 仍经
   语义→Chat wire→`s.Chat`→再解码的中转（service.go:1179-1187），与 v0.3.0 同形。
6. **迁移 32 的清理面比方案 §2 所列多两项。** 方案 §2（review-plan.md:40）写「存量记录两半皆关、
   证据置 unsupported、探测结果清空」；实际还包括：deployments 的 `operator_disabled` 列表中
   `json_mode` 项被移除（store.go:861、964-992），以及 providers 记录**及其每个 binding**、
   deployments 的 `model_capability_snapshot` 同步拆分（store.go:849-854、864-883）。
7. 其余核对一致：182/12913/1520 的 diffstat、3 个提交、未触及包清单为空、`json_mode` 就地拆分
   （前端与 contracts/guides 文档旧词零残留，残留仅在 prd/review/todo 历史文档）、词表 v2、
   探针预算 8→9（v0.3.0 为字面量 8 且静默截断，HEAD 为常量 9 且超额记入 Deferred）、
   `web_search` 唯一 hosted tool 且默认关闭、迁移 32 位于 store.go:839。
