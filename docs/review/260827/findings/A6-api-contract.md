# A6 · API 兼容性契约（阶段 1 独立评审）

范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`。行号以 2026-08-27 工作区 HEAD 为准；v0.3.0 行号来自
`git show v0.3.0:<path>`。实测：`go test ./internal/compatibility/... -count=1` 全部通过
（compatibility 1.210s / anthropic 0.623s / openai 1.738s）。

## 结论总览

| 编号 | 主题 | 分级 | 阻塞发布 |
|---|---|---|---|
| A6-1 | CHANGELOG `[Unreleased]` 为空：词表 v2 / 迁移后果 / 新档案均无落笔 | 中（发布文档缺口） | **阻塞候选** |
| A6-2 | SDK 黑盒契约未覆盖 web_search_call / url_citation 渲染 | 中（测试盲区） | 否 |
| A6-3 | manifest 机器守护是单向的：拒绝声明与 transform 文字无代码校验 | 低（疑问/建议） | 否 |
| A6-4 | openai-compatibility.md 对 web_search「唯一档案」的归因不准确 | 低 | 否 |
| A6-5 | 词表 v2 对网关客户端：不可见性成立；错误消息词汇变化；迁移后存量部署 JSON 请求转 400 | 记录（并入 A6-1 落笔要求） | 否 |
| A6-6 | Anthropic facade 对新内容种类是拒绝而非丢弃（B4 满足） | 记录（通过） | 否 |
| A6-7 | Responses 流式拒绝的错误体形状 OpenAI SDK 可解析 | 记录（通过） | 否 |
| A6-8 | metrics-reference.md 7 行改动与代码一致 | 记录（通过） | 否 |

---

## A6-1 CHANGELOG 未记录任何本范围对外可见改动（中 · 阻塞候选）

**基准**：review-plan §1（本轮产出必须是可签字的发布记录）；CLAUDE.md「Say when a change
requires re-initialising / 说明可见后果」。

- `CHANGELOG.md:7`：`## [Unreleased]` 之下为空；`git log v0.3.0..HEAD -- CHANGELOG.md`
  输出为空。0.3.0 有完整条目（CHANGELOG.md:9 起），说明本仓库确实按版本维护 changelog。
- 未落笔的对外可见改动至少有三类：
  1. **Admin API 破坏性字段改名**：Deployment/Provider 记录的 `capabilities.json_mode`
     消失，替换为 `json_object` + `structured_outputs`（internal/domain/models.go:491-492，
     JSON tag 即 Admin API 响应体字段名）。pre-1.0 允许，但按方案 §5 A6 行的要求
     「docs/compatibility 或 CHANGELOG 里要有落笔」——两处都没有：
     `docs/contracts/provider-capabilities.md:7-9、25-28` 描述了新词表及拆分理由，但全文
     没有一句「原 `json_mode` 已移除/改名」；`grep -rn json_mode docs/`（排除
     prd/review/todo）零命中。
  2. **迁移 32 的调用方可见退化**：存量 `json_mode=true` 部署两半皆置 false
     （internal/store/bolt/store.go:918-921、945-948），升级后原本成功的
     `response_format=json_object/json_schema` 请求变为 400 `unsupported_feature`
     （internal/gateway/service.go:961-966），直至操作者重新勾选。刻意 fail-closed
     （store.go:818-825 注释），但没有任何对外文档告知这一后果。
  3. **新 Responses 档案与 `web_search`**（`openai.responses.v1`、
     provider_table.go:142-152）——全新对外能力，同样无 changelog 条目。
- **是否阻塞**：若 v0.4.0 发布流程在打 tag 前会统一补写 CHANGELOG，则降为流程项；
  但按本轮「可签字记录」的定位，签字前必须补齐，故列为阻塞候选。

## A6-2 官方 SDK 黑盒契约未覆盖 Responses 新内容种类（中 · 不阻塞）

**基准**：endpoint-manifests.json 为 `openai.responses.stateless.v1` 声明
`evidence: ["gateway_contract","provider_transport_fixture","sdk_blackbox"]` 与
`sdk_matrix: ["openai-go","openai-node","openai-python"]`（docs/compatibility/endpoint-manifests.json，
经 internal/compatibility/manifest.go:94 生成）。

- 范围内 `tests/` 仅改 `tests/compatibility/server/main.go` 2 行（`Annotations` 从 `[]any{}`
  改为 `[]openaiapi.ResponseAnnotation{}` 的类型跟随，git diff --stat v0.3.0..HEAD -- tests/）。
- 三个 SDK 套件对 `/v1/responses` 只测纯文本 create + stream（"ping"→"compat-ok"）：
  tests/compatibility/go/sdk_test.go:48-79、tests/compatibility/node/test-sdk.mjs:25-34、
  tests/compatibility/python/test_sdk.py:41-52。`grep web_search|url_citation|annotation`
  三套件零命中。
- compat server 是 **stub service + 真 facade**（tests/compatibility/server/main.go:240
  `gatewayapi.NewWithOptions`），Annotations 恒为空数组（main.go:93）；而 web_search_call /
  url_citation 的北向渲染在 gateway.Service 内
  （internal/compatibility/openai/responses.go:296-311、306-317），完全在 SDK 黑盒回路之外。
  即：**没有任何官方 SDK 解析过一条 Halro 渲染的 `web_search_call` item 或
  `url_citation` annotation**。gateway/gatewayapi 包内也无端到端 web_search 测试
  （`grep -rln web_search internal/gateway* --include=*_test.go` 仅命中 native_messages_test.go，
  那是 Anthropic 原生工具分类）。
- **缺的用例（逐 SDK 相同三条）**：openai-python / openai-node / openai-go 各补：
  1. `responses.create(tools=[{"type":"web_search"}])`，断言 `output` 含
     `type=="web_search_call"`（含 id/status/action.query）与 message item 的
     `annotations[0].type=="url_citation"`（url/title/start_index/end_index 被 SDK 类型接住）；
  2. `tools=[{"type":"code_interpreter"}]` → SDK 抛 400 BadRequestError 且错误体可解析；
  3. `stream=true` + `tools=[{"type":"web_search"}]` → 400（openaiapi/responses.go:147-149
     的「streaming tools」拒绝覆盖 hosted tool）。
- 现有覆盖：wire 校验 internal/openaiapi/responses_test.go、adapter fixture
  internal/provider/openai/responses_profile_test.go、映射 internal/compatibility/openai
  测试、脱敏 internal/redaction/provider_tool_content_test.go——单元/fixture 层是实的，
  故不阻塞；但 manifest 的 `sdk_blackbox` 证据在「新增内容种类」粒度上是盲区，
  下一轮 SDK 套件应补齐。

## A6-3 manifest 机器守护单向，拒绝声明无代码校验（低 · 疑问/建议）

**核实**：守护存在且通过——
- **golden 漂移门**：internal/compatibility/manifest_test.go:12-42 将
  `BuiltinEndpointManifests()` 的 JSON 序列化与 docs/compatibility/endpoint-manifests.json
  逐字节比对（HALRO_UPDATE_GOLDEN=1 才能改），本地实测通过 → 文档与代码内表**不可能漂移**。
- **规则⊆声明容器门**：manifest_derivable_coverage_test.go:23-59 用 20+ 探针请求逐档案
  调 `UnsupportedGenerateFields`，任何代码拒绝而 manifest 未声明的字段都报错（该测试
  自述曾抓到 Mantle Responses 两档案漏声明 messages[].name/tools）。
- **portable 链实走门**：manifest_portable_coverage_test.go:31 起，output_config 逐值
  走 DecodePortable→OpenAI wire→DecodeGenerate 真实链。

**缺口**（不构成缺陷指认，是守护边界）：
1. 容器门是单向的（manifest 允许「说得比规则多」，manifest_derivable_coverage_test.go:16-22
   注释言明）。若 manifest **多声明**一个实际会被承载的字段，测试不报——
   本轮逐字段抽查未发现此类多报（见 A6-5 核对表），但它靠人眼。
2. `RejectedRequestFields`（manifest.go:93 的 23 项，如 `input[].id`、`stream=true with tools`）
   与 `DeclaredTransforms` 是纯文字，无任何测试把它们钉到代码。本轮抽查逐条成立：
   `store=true` openaiapi/responses.go:107、`tools[].strict=true` :144-146、
   `stream=true with tools` :147-149、`reasoning` :151-153、hosted tool 白名单 :130-138、
   `input[].id` internal/compatibility/openai/responses.go:119、
   `input[].type=unsupported` :142、`previous_response_id`/`conversation`/`metadata` 等
   经 `DisallowUnknownFields`（openaiapi/responses.go:79）拒绝。
   建议（非本轮必做）：把 RejectedRequestFields 也纳入探针式校验。

## A6-4 openai-compatibility.md 对 web_search「唯一档案」的归因不准确（低）

**基准**：文档等价性（方案 §5 A6 行）。

docs/contracts/openai-compatibility.md:61-62：「It is served by the `openai.responses.v1`
provider profile and by no other, **because no other profile's wire form can carry it**」。
归因不准确：Bedrock Mantle 的两个 Responses 档案共享同一 stateless Responses wire form
（internal/compatibility/openai/provider_responses.go 同为其渲染器），wire 上完全放得下
`web_search`；真正的排除机制是 allowlist
`providerExecutedToolProfiles = [ProfileOpenAIResponses]`
（internal/compatibility/provider_fields.go:285）加 Mantle ceiling 不含
`provider_executed_tools`（provider_table.go 仅 :151、:158 两处 `withProviderExecutedTools`）。
行为（拒绝）与文档结论一致，仅理由句失真；且 Anthropic 档案 ceiling 其实含该能力
（:158，走 native 模式），「by no other」对 portable 路径成立、对 native Anthropic
provider-executed tools（endpoint-manifests.json anthropic.messages 条目自己声明的
web_search_* 准入）需要读者跨文档拼合。建议改写为按 allowlist+ceiling 归因。

## A6-5 词表 v2 对网关客户端：不可见性验证（记录）

**基准**：方案 §5 A6 行「请求体里没有能力词，理论上不可见，验证之」。

- **wire 校验两版逐字节相同**：`response_format.type` 接受 text/json_object/json_schema，
  v0.3.0 与 HEAD 的 internal/openaiapi/types.go:110-123 完全一致（git show 比对）。
  Responses 侧 `text.format` 同（openaiapi/responses.go:152-165）。
- **错误码与错误体形状不变**：候选清空 → `gatewayError("unsupported_feature", "model route
  does not support the requested chat capabilities", 400)`，v0.3.0 service.go:912-914 与
  HEAD service.go:961-966 同构；错误体均为 openaiapi.ErrorEnvelope
  （internal/gatewayapi/handler.go:1118-1130），本范围 facade 仅一个 hunk 且与此无关。
- **唯一的消息文本级变化**：unservableError 附加的 reasons 词汇由 `json_mode` 变为
  `json_object` / `structured_outputs`（service.go:2537-2538 配对表）。reasons 机制
  v0.3.0 已存在（v0.3.0 service.go:2397），非本范围新增；消息文本不属稳定契约，可接受。
- **接受/拒绝矩阵只按部署能力变化**：档案字段级规则两版一致——Anthropic 拒 json_object
  与非 strict json_schema（v0.3.0 provider_fields.go:84-85 ↔ HEAD :89-90 语义同），
  DeepSeek 拒 json_schema（v0.3.0 :161 ↔ HEAD :183）。变化仅在能力位：拆分前一个位
  同时放行两种 kind，拆分后各自把关（semantic/request.go:205-206 derive，
  service.go:2537-2538 配对）——正是 B9 要的形状。
- **升级可见退化**：迁移 32 后存量部署两半皆关 → 旧 json_mode 流量 400（见 A6-1 第 2 条），
  错误体 reasons 会列出缺失的能力名（service.go:2467-2477），操作者可据此重新勾选。
- **Admin API**：字段改名 + 探测契约 v1→v5（internal/provider/capability_detection.go:70）
  + 探测桶清空（store.go:887-896）对 Admin 客户端是破坏性的；pre-1.0 允许（B6），
  落笔缺失归 A6-1。

## A6-6 Anthropic Messages facade 对新内容种类：拒绝而非丢弃（记录 · B4 满足）

- `RenderResult`（internal/compatibility/anthropic/mapping.go:243-258）：带 `Citations`
  的文本 part 显式拒绝（:246-248，本范围新增 hunk）；`ContentProviderToolCall` 落入
  `default` 分支拒绝（:256-257「provider result contains non-portable content」）。
- portable 链中转的 Chat wire 渲染同样拒绝：internal/compatibility/openai/mapping.go:314-319
  （citations）、default 分支覆盖 provider tool call。
- 附注（移交 A1/B8）：这两处拒绝发生在结果渲染期，位于 attempt.finish/结算之后；
  但 portable Messages / Chat wire 均无法**请求** web_search（chat 工具仅 function，
  openaiapi types 校验），上游无端返回注解才会触发，风险极低。

## A6-7 流式：stream=true 打到 responses-only 路由的错误形状（记录 · 通过）

- 机制与 manifest 措辞一致：Responses 档案无 ChatStream 操作也无 Streaming 能力
  （provider/profile.go:400、provider_table.go:122-125），stream=true 的 Streaming 需求
  在 filterSemanticCapabilities + filterPrimitiveTargets 把 target 全部滤除 →
  「routed away rather than refused by field」（manifest.go:46 transform 原文）。
- 错误抵达形状：ResponsesStream 在发出任何 SSE 事件之前失败时，handler 走
  `!started && err != nil` 分支（internal/gatewayapi/handler.go:162-171），返回
  **HTTP 400 + OpenAI ErrorEnvelope JSON**（writeError:1118-1130，`{"error":{"message",
  "type","param","code"}}`），不是 SSE error 事件——官方 OpenAI SDK 会将其解析为
  BadRequestError。Chat 端 stream=true 同理（service.go:1891 起 generateStream 内
  同一 unservableError，service.go:1852-1854 同文案）。无「流已开再拒」的形态。

## A6-8 metrics-reference.md 7 行改动与代码一致（记录 · 通过）

- 新增 prose：「probe.state 为 healthy/unhealthy/not_probed 报到部署记录」——逐一落地：
  `Probe deploymentProbeView json:"probe"`（internal/app/admin_capability_review.go:22）、
  三个状态常量（:44-47）、`probeView`（:49-61）；「只携带 classified error」——
  `ErrorClass`（:40-41 注释「Classified, never the upstream's sentence」），值域为
  provider.ErrorClass 有界枚举（internal/provider/provider.go:35-46）。
- `halro_deployment_up` 本体：internal/app/metrics.go:365-372，范围内改动仅
  DeploymentHealth→DeploymentProbes 数据源替换（git diff 4 行），label 集不变
  （仅 deployment_id，有界托管 ID）；已删除部署的序列由 RetainDeploymentProbes 回收
  （internal/provider/provider.go:773-785），label 集不增长。
- 范围内唯一其他指标改动：`halro_invocation_target_resolution_total` 的 `status` label
  新增有界值 `covered_elsewhere`（internal/app/capability_metrics.go:155-157，
  internal/domain/invocation_target.go:214）；metrics-reference.md:123 只列 label 名
  不列值域，不构成文档漂移。无新增/改名指标。基数无失控。
