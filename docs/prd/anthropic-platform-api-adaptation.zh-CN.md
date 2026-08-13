# Anthropic Platform API 适配 — 进展与遗留

状态：**已完成并归档。** §1 的原有工作加上 §2/§3 全部遗留项均已实现；随后在真实账号验证中发现四处缺陷，已修复（见 §5）
更新日期：2026-08-13
范围：`internal/anthropicapi`、`internal/provider/anthropic`、`internal/compatibility/anthropic`、`internal/compatibility`、`internal/gateway`、`internal/gatewayapi`、`internal/redaction`、`internal/semantic`、`internal/provider`、`internal/domain`、`internal/modelcatalog`、`internal/store/bolt`、`internal/app`、`web/src`

---

## 0. 出发点

改造前，Halro 对 Anthropic 的适配是 Messages API 的一个**刻意收窄的切片**：解码器用三层白名单（顶层 `DisallowUnknownFields`、内容块类型白名单、工具类型白名单），每一个新字段默认拒绝。后果是"支持某特性"永远等于"改 `internal/anthropicapi/types.go` 里的 struct"，Anthropic 每次发版都要重开同一个文件。

目标不是逐个补字段，而是把判据换轴——从"struct 认不认识"换成"这个东西会不会导致上游侧执行或出站"——使接受面能随上游演进而不必逐版本改代码。

---

## 1. 第一轮（已提交 `8780050`、`67c04b6`）

### 1.1 安全修复

**修复了一个既存于 main 的泄漏**：把 Gateway Key 放进 `metadata` 或 `thinking` 对象，它会原样送达上游服务商。成因是两道护栏的缝隙——信封的 `containsCredentialField` 只识别 `sk-`/`AKIA`/`AIza`/`bearer ` 前缀，不认 `gw_`；而 native 入站脱敏走的是可移植投影，投影前显式执行 `projection.Metadata = nil`、`projection.Thinking = nil`。

修复方式是让脱敏检查直接遍历原始载荷字节，并在同一提交内堵上重复键绕过（`encoding/json` 后键胜出，而 native 逐字节转发调用方原始内容）。

### 1.2 ~1.6

工具与内容块白名单按执行位置换轴（族前缀分类 + `Tool.Raw` 保真 + `document`/`search_result` 放行）；native 请求保真（`preparePayload` 外科式改写）；`output_config` 进语义层；每连接可配的 `anthropic-beta` 允许列表；三个 `MessagesService` 实现补上编译期接口断言。

---

## 2. 第二轮：全部遗留项已实现

### 2.1 服务端执行工具的能力开关（原 §2.1）

新增能力 `provider_executed_tools`，贯通 `semantic.Requirements` → `provider.Capabilities` →
`domain.ProviderCapabilities` + 能力名注册表 + evidence → `filterSemanticCapabilities` →
`modelcatalog` → 前端与 i18n。

- 判据从"解码器拒绝"换成"连接是否承担该出站"：`anthropicapi` 现在接受 `web_search_*` 等声明，
  由 `NativeRequirements` 产出需求，在选目标时过滤。
- **默认关，且与默认值分离**：新增 `domain.MaxProviderCapabilitiesForProfile`（前端对应
  `maxProviderCapabilities`），把"运营者可以开什么"从"新连接默认开什么"里拆出来。二者合一时，
  任何可选能力只能是恒开或不可达——正是原文档指出的阻塞点。
- Bedrock Mantle 的天花板保持不变（CLAUDE.md 要求，需单独契约评审）。
- **需要 store migration**：能力名进入字典后，evidence 集合按整个字典校验，旧记录会拒绝加载。
  migration 28 `provider_executed_tools_capability` 把 provider / binding / deployment /
  snapshot 四处 evidence 补成 `unsupported`——该能力当时不存在，这是唯一诚实的取值。**不需要
  重新初始化数据目录。**

### 2.2 `count_tokens` 端点（原 §2.2）

`POST /v1/messages/count_tokens` 已实现：route（`internal/app/runtime.go`）、handler
（`gatewayapi.Handler.CountTokens`）、service（`gateway.MessagesCountTokens`）、adapter
（`anthropic.Adapter.CountTokensNative` + `provider.NativeTokenCountAdapter`）、manifest 条目
（`anthropic.messages.count-tokens.2023-06-01`）、测试。

**已决策问题（记账）**：走 attempt/settlement，写零成本条目。理由是不变量取舍已经定死——
Anthropic 不计费，但这是一次用运营者凭据发出的真实 Provider 调用，把它排除在账外会在"每次
Provider 调用都可审计"上开一个类别级的洞。实现上以 0 prepared tokens 预留（因此预留额为 0），
settlement 显式 `CommittedMicrosUSD: 0`，`ProviderInputTokens` 记上游报的真实数。

其它约束：

- 仅直连 `ProfileAnthropicMessages` 提供。Bedrock Mantle 共用 Messages 线格式，但其
  count_tokens 面未经证实，猜测等于把提示词发往没人验证过的端点。
- 只替换 `model`，不注入 `stream`、不剥离 `max_tokens`——后者由 Anthropic 判断。
- 只有一种执行形态，因此显式 `Halro-Route-Mode: portable` 被拒绝（缺省不带头则放行）。

**未采纳**：用它替换 `internal/compatibility/anthropic/native.go` 里 `len(payload)/4` 的输入
token 粗估。那会给每个请求加一次额外的 Provider 往返，成本与收益不成比例；原文档也只写"可评估"。

### 2.3 原 §3.1（本次引入、当时未修）—— 10/10 已修

| 位置 | 修法 |
|---|---|
| `internal/compatibility/provider_fields.go` | `portableEffortLevels` = Anthropic 阶梯 ∩ `openaiapi.ReasoningEffortLevels`。`max` 现在在路由阶段被声明为不支持，而不是在预算预留之后于渲染阶段失败。顺带把 OpenAI 侧的阶梯导出为单一事实源。 |
| `internal/anthropicapi/types.go` | 新增 `Tool.UnknownMembers()` / `OutputConfig.UnknownMembers()`（含 `format.*`）。native 仍逐字节转发；**portable 改为拒绝**——那条路径会重写请求体，静默丢弃 `task_budget`（约束花费）是不可接受的。 |
| `internal/compatibility/anthropic/native.go` | `NativeHeaders(version, betas)` 现在记录真正会发出的 beta 头，允许列表条目不再失效。`checkAnthropicBetas` 移进 `prepareNativeMessages`，在建信封之前。 |
| `internal/compatibility/anthropic/mapping.go` | 无 `name` 的 `json_schema` 在 wire 层就以指名字段的错误拒绝，不再让请求走到语义层再报"request is not portable"。**没有**同时加进 `UnsupportedGenerateFields`：`semantic.Validate` 已保证到那一步的请求必有 name，加了就是永不触发的死声明。 |
| `internal/compatibility/anthropic/mapping.go` | `Strict` 对称化：解码仍置 `true`（那是 Anthropic 的语义），渲染遇到 `strict:false` 直接拒绝，并在 `UnsupportedGenerateFields` 声明——不再静默升级。 |
| `internal/anthropicapi/types.go` | 族匹配锚定为 `<family>_<YYYYMMDD>`。`bash_code_execution_20250825` 去掉 `bash_` 后余下 `code_execution_20250825` 不是版本号，因此不会被当成客户端执行的 shell 工具；它落到服务端执行族。 |
| `internal/domain/models.go`、`web/src/pages/ProvidersPage.tsx` | beta 允许列表改按 **profile** 判定（`domain.ProfileSendsAnthropicBetas`），Mantle 的 OpenAI chat/responses 连接不再能存下永不发送的 token。 |
| `internal/compatibility/anthropic/native.go` | `document` 块置 `Requirements.InputImage`：PDF 与图片走同一条多模态管线。 |
| `web/src/pages/ProvidersPage.tsx` | `errors.anthropicBetas` 有了客户端校验（数量、长度、字符集、去重），字段级高亮不再是死键。 |
| `internal/anthropicapi/types.go` | `ParseBetaTokens` 跳过空元素。HTTP 列表语法容忍它们，靠拼接构造该头的 SDK 会踩到尾随逗号。 |

### 2.4 原 §3.2（既存缺陷）—— 4/4 已修

**native 流式对文本从来没工作过**（高）。旧实现把每个 delta 的文本喂进 `redaction.Stream` 再
逐字节比对——而该流处理器按设计会扣留末尾约 2 KiB（实测 width=2086）以处理跨 chunk 边界，
两者永远不可能相等。仓库唯一的 native 流式测试只用 `signature_delta`（不含文本），所以这个模式
看上去健康、实际不可用。

新结构 `redaction.StreamInspector` + `gateway.nativeStreamGate`：

- inspector 只回答"原文有多少字节已确认未被改写"，从不产出重写副本；
- gate 把事件排队，**直到它携带的文本全部落进已确认区间才下发**，`content_block_stop` 关闭该块
  的通道并把扣留的尾部逼过规则，流结束时 `Finish()` 关闭所有通道；
- 因此扣留窗口第一次成为有效防线：拆在两个 delta 之间的密钥两半都不会发出。

反向验证：把 HEAD 的比较逻辑原样取出跑同样输入，短文本与 1 万字符文本**均被拒绝**——缺陷属实，
新测试在旧实现下必然失败。

其余三条：

- `wireValue()` 的 default 分支不再输出 `{"type": ...}`。`image`/`document` 有了带 `source` 的
  真实分支，未建模类型返回错误——Halro 自己构造的块没有原始字节可转发，这里输出什么，服务商就
  收到什么。
- `redaction` 的遍历补上**对象键与数字**：键与数字被检查但不被重写（改键名改的是文档形状，把
  数字换成掩码串改的是类型），命中即报错。`processRaw` 改用 `UseNumber`，长整数不再丢精度。
- native 路径现在调用 `filterSemanticCapabilities`：`NativeRequirements` 从
  `extractNativeGovernance` 中提出并导出，在选目标前生效，`output_config.format` 不再能抵达
  没有 `JSONMode` 的目标。

第四条（入站遍历成本）一并解决：新增 `redaction.Engine.InspectJSON` 一次解析走完，取代原先
"canonicalJSON 解析+序列化" 与 "ProcessJSON 解析+序列化" 的两轮，`gateway.canonicalJSON` 已删除。

---

## 3. 验证记录

- `go build ./...`、`go vet ./...`：0
- `go test ./...`：0，全部包通过
- `go test -race ./internal/gateway/ ./internal/redaction/`：0
- 前端：`typecheck` 0、`vitest` 全部通过、`npm run build` 已重建 `internal/webui/dist`
  （密钥扫描 11 文件干净）
- 反向验证：
  - native 流式——取 HEAD 的比较逻辑原样执行，短/长文本一律被拒（缺陷属实）
  - 前端 beta 校验——去掉字符集分支后，对应测试失败
- `TestPreparePayloadRewritesOnlyModelAndStream` 的不可区分问题已补齐：新增
  `TestPreparePayloadPreservesShapesTheStructRoundTripDestroyed`，用 `"content":"hi"`
  字符串形式与 `"stop_sequences":[]` 两个载荷区分外科式改写与 struct 往返。

---

## 4. 升级注意事项

- **不需要重新初始化数据目录，但持久化格式版本会前进。** migration 28 把新能力名补进已有记录的
  evidence 集合。本文初稿曾写"未改动任何持久化格式版本"，那是错的——当时 `schemaVersion` 仍是 27，
  migration 28 因此从未被执行（迁移循环的条件是 `currentVersion < schemaVersion`）。已有记录保持
  17 项 evidence，而校验开始要求 18 项：读取不校验，所以直到下一次写入才暴露，表现为连接测试探测
  成功、写回结果时被拒。修复是把常量推到 28（提交 `fbbaa6c`），这**就是**一次持久化格式版本变更：
  升级后旧二进制会拒绝加载该数据目录，因为版本比它支持的新。这是设计的失败方式，但回滚方案要据此规划。
- **现存 Anthropic 连接需手工启用 `json_mode`。** 能力默认值只在新建时生效，
  `ProfileAnthropicMessages` 不在 immutable 名单中，因此已存记录保持 `json_mode: false`，
  结构化输出请求会被路由过滤掉——**native 模式现在同样受此约束**（native 路径已开始做能力过滤）。
- **`provider_executed_tools` 默认关。** 想让 `web_search_*` 等通过，需要在 provider 连接与
  deployment 两处显式勾选；勾选即承担该连接会发起 Halro 看不到、SafeTransport 不过滤的上游出站。
- **portable 模式收紧了三处**：`tools[]` / `output_config` 的未知成员、`strict:false` 的
  json_schema、无 `name` 的 json_schema，现在都会被拒绝或被路由绕开，而不是静默丢弃/升级。

---

## 5. 归档后续：真实账号验证发现的四处缺陷（2026-08-13）

本文声明"已实现并验证"之后，第一次用真实 Anthropic 账号走通全链路，当天在这项工作范围内又发现四处
缺陷。记录在这里，是因为它们说明了一件事：**本文 §3 的验证记录全部是自测，而这四处没有一处是自测
能发现的。**

1. **连接测试对空模型构造 Messages 请求**，被 Halro 自身的校验拒绝，从未发出——控制台把一次本地
   拒绝报成了上游拒绝。改为回退读 Models API（`d724e1f`）。
2. **Models API 解码读错字段名与能力键**：输出上限是 `max_tokens` 而非 `max_output_tokens`；
   `capabilities` 里根本没有 `messages`/`streaming`/`tool_use` 这三个键，而它们是 chat 声明的
   全部依据（同上）。fixture 是照着实现者的假设写的，所以测试与代码一起错、全绿。
3. **adaptive thinking 默认开启**导致 portable 路径必然失败：签名 thinking 块在 OpenAI 形状的
   响应里无处安放，于是上游执行并计费、调用方收到 502。portable 现在显式发送
   `thinking: {"type":"disabled"}`（`0e0a4ef`）。
4. **termination 词汇表用错方向**：本适配写的是 OpenAI 线协议词汇，而语义字段期待自己的词汇；
   叠加 `finish_reason` 无条件透传上游原词，`/v1/responses` 最终拒绝了网关自己造出的响应（同上）。

第 2 项最值得记住。它的测试 fixture 与被测代码出自同一份假设，因此二者一起错、且全部通过——这正是
真实账号回归存在的理由，也是为什么 `internal/provider/anthropic/real_smoke_test.go` 在同一天被补上
并接入 GA 门禁（`8f9c30b`）。该冒烟已对 `claude-opus-5` 实跑，七项全部通过。
