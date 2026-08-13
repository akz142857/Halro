# Anthropic Platform API 适配 — 进展与遗留

状态：**部分完成。安全修复已独立合入；特性改动待评审**
更新日期：2026-08-13
范围：`internal/anthropicapi`、`internal/provider/anthropic`、`internal/compatibility/anthropic`、`internal/compatibility`、`internal/gateway`、`internal/gatewayapi`、`internal/domain`、`internal/app`、`web/src`

---

## 0. 出发点

改造前，Halro 对 Anthropic 的适配是 Messages API 的一个**刻意收窄的切片**：解码器用三层白名单（顶层 `DisallowUnknownFields`、内容块类型白名单、工具类型白名单），每一个新字段默认拒绝。后果是"支持某特性"永远等于"改 `internal/anthropicapi/types.go` 里的 struct"，Anthropic 每次发版都要重开同一个文件。

本次目标不是逐个补字段，而是把判据换轴——从"struct 认不认识"换成"这个东西会不会导致上游侧执行或出站"——使接受面能随上游演进而不必逐版本改代码。

---

## 1. 已完成

### 1.1 安全修复（已独立提交 `8780050`）

**修复了一个既存于 main 的泄漏**：把 Gateway Key 放进 `metadata` 或 `thinking` 对象，它会原样送达上游服务商。成因是两道护栏的缝隙——信封的 `containsCredentialField` 只识别 `sk-`/`AKIA`/`AIza`/`bearer ` 前缀，不认 `gw_`；而 native 入站脱敏走的是可移植投影，投影前显式执行 `projection.Metadata = nil`、`projection.Thinking = nil`。两道都不覆盖，于是漏了。

修复方式是让脱敏检查直接遍历原始载荷字节。`redaction.Engine.ProcessJSON` 本身就是完全递归的 JSON 遍历器，不需要 Halro 认识 schema，因此检查面能覆盖每一个规范模型无法表达的字段。

流式按事件类型分流：增量文本仍走有状态的 `redaction.Stream`（跨 chunk 边界检测的唯一手段），整包事件走同一套原始遍历——否则 `content_block_start` 携带的未建模块类型会完全不受检查。

**同一提交内还堵上了重复键绕过。** `encoding/json` 对重复成员是后键胜出，而 native 路径逐字节转发调用方的原始内容，于是 `{"user_id":"<密钥>","user_id":"benign"}` 检查时是干净的、线上仍带着密钥。拒绝这种歧义是"检查的文档"与"发出的文档"保持同一份的唯一办法（不重写调用方字节的前提下）。这条必须排在任何逐字节转发改动**之前**落地，否则就是先开洞再补。

两半都做过反向验证：回退对应生产代码后测试确实失败，且失败输出显示密钥抵达了假服务商。

### 1.2 工具与内容块白名单按执行位置换轴

- 工具判据从 `Type != "custom"` 换成**族前缀**分类：`bash_`/`text_editor_`/`memory_`/`computer_` 属客户端执行，任意版本后缀放行；`web_search_`/`web_fetch_`/`code_execution_`/`advisor_`/`tool_search_` 属上游执行，拒绝；未知族仍 fail-closed。此前这些客户端执行工具与服务端工具共用一条判断被一起拦掉，属于误伤。
- `Tool` 增加 `Raw` 保真（沿用 `ContentBlocks` 已有手法），使 `cache_control`、`defer_loading`、`computer_*` 的显示尺寸等原样抵达上游，而不是靠往 struct 上逐个加字段。
- `document`（PDF 输入）与 `search_result` 内容块放行——它们与已放行的 `image` 同属数据载体。

### 1.3 native 请求保真

`preparePayload` 从"解进 struct 再重新序列化"改为对原始字节做外科式改写，只覆盖 `model` 与 `stream`。

附带修复了一个既存的 native 模式损坏：旧的 struct 往返会把 `"content":"hi"`（字符串形式）改写成 `["hi"]`，**那是无效的 Anthropic 线格式**；也会因 `omitempty` 丢掉 `"stop_sequences":[]`。

### 1.4 `output_config` 进语义层

`effort` 与 `format` 映射到语义层**已有的** `ReasoningEffort` 与 `OutputFormat`，`DeriveRequirements` 自动产出 `StructuredJSON`/`Reasoning` 供路由过滤。`task_budget` 等未建模成员经 `Raw` 透传。直连 Anthropic 的能力默认加上 `json_mode`，前端上限同步。

Bedrock Mantle Anthropic 的 immutable 能力上限**按 CLAUDE.md 要求保持不动**，留待单独契约评审。

### 1.5 每连接可配的 `anthropic-beta` 允许列表

此前对该头一刀切拒绝，等于永远接不到 Anthropic 的新特性（structured outputs beta、fast mode、task budgets、compaction、context management 全走 beta 头）。

现在改为每连接可配的 token 允许列表：`domain.ProviderInstance.AllowedAnthropicBetas`（含字符集与数量校验）→ `provider.Target` → `NativeMessageCall.Betas` → adapter 头。只在 native 模式生效——portable 模式会重写请求体，携带描述"原样请求"的 beta token 是不成立的。

### 1.6 顺带修复：编译期接口断言

`handler.messages, _ = service.(MessagesService)` 是 comma-ok 断言，签名漂移不会编译失败，而是把 `messages` 悄悄置 nil、`/v1/messages` 运行时返回 501。本次改签名时正是这样先得到一次假绿。已为三个实现（`*gateway.Service`、测试替身、`tests/compatibility/server`）各加 `var _ MessagesService = (*T)(nil)`，并验证了断言确实拦得住漂移。

---

## 2. 未完成

### 2.1 服务端执行工具的能力开关

现状：`web_search_` 等一律拒绝。目标：挂到一个显式的 provider capability 后面，由运营者承担上游出站含义后开启。

阻塞点：需要贯通 `semantic.Requirements` + `provider.Capabilities` + `domain` 能力名注册表与 evidence + `filterSemanticCapabilities` + 前端上限与 i18n。且前端目前 `capabilityCeiling === defaultProviderCapabilities`，要做"可选但默认关"必须先把这两者拆开。

### 2.2 `count_tokens` 端点

需要 route + handler + service（复用 `OperationMessages` 选目标、走 native 入站脱敏）+ adapter 方法 + manifest 条目 + 测试。

**未决策问题**：Anthropic 对 `count_tokens` 不计费，但它仍是一次真实的 Provider 调用。要不要走 attempt/settlement 机制写零成本 ledger 条目，还是只做鉴权与项目路由校验而不留账？这关系到"每次 Provider 调用都可审计"的取舍，需要先定再实现。

顺带可评估用它替换 `internal/compatibility/anthropic/native.go` 里 `len(payload)/4` 的输入 token 粗估。

---

## 3. 多角色 review 发现、尚未修复的问题

四个独立视角（安全 / 记账与不变量 / API 契约与 pre-1.0 政策 / 实现与边界）审查后的结果。已修复的两条见 §1.1。

### 3.1 本次引入、尚未修复

| 严重度 | 位置 | 问题 |
|---|---|---|
| 高 | `internal/compatibility/provider_fields.go` | `effort: "max"` 在 portable 模式不可路由。portable 永远经 OpenAI 中间表示，而 `openaiapi` 的 `reasoning_effort` 只到 `xhigh`。判据用了含 `max` 的 `anthropicapi.EffortLevels`，请求不会被路由避开，而是在渲染阶段失败——**且发生在预算预留之后**，错误信息还指向调用方从未发送的字段。 |
| 中 | `internal/anthropicapi/types.go` | `Tool` 与 `OutputConfig` 的自定义 `UnmarshalJSON` 让顶层 `DisallowUnknownFields` 穿不进去，嵌套未知成员被静默接受并转发。portable 模式下这些成员会被静默**丢弃**——包括 `task_budget`，它约束花费。违反"不支持的字段必须拒绝、绝不静默丢弃"，也与 manifest 的 `"unknown fields"` 声明矛盾。 |
| 中 | `internal/compatibility/anthropic/native.go` | `NativeHeaders(version)` 仍只返回 `anthropic-version`，而 `AllowedHeaders` 已加入 beta 头。结果是允许列表条目失效，且被证明的 native 信封没有记录那个改变上游行为的头。 |
| 中 | `internal/compatibility/anthropic/mapping.go` | 不带 `name` 的 `json_schema`（Anthropic 中 `name` 可选）能过 wire 校验，却被 `semantic` 层以缺 name 拒绝，最终返回 "request is not portable"——归因错误。native 模式下同样请求体正常。 |
| 中 | `internal/compatibility/anthropic/mapping.go` | `Strict` 跨可移植边界不对称：解码时硬编码 `true`，渲染时完全忽略 `format.Strict`。OpenAI 侧的 `strict:false` 会被静默升级为 Anthropic 的 schema 强制，且未在 `UnsupportedGenerateFields` 中声明。 |
| 中 | `internal/anthropicapi/types.go` | 族前缀在已知前缀**内**是 fail-open。`bash_code_execution_20250825` 会被判为客户端执行工具——而 Anthropic 的代码执行容器已经在发 `bash_code_execution` 块，所以这不是臆想的命名。建议锚定为 `^(bash|text_editor|memory|computer)_[0-9]{8}$`。 |
| 低 | `internal/domain/models.go`、`internal/app/admin_providers.go`、`web/src/pages/ProvidersPage.tsx` | beta 允许列表按 **access surface** 而非 profile 判定，于是 Mantle 的 OpenAI chat/responses 连接也能存下永远不会被发送的 token——正是 `models.go` 注释声称要防止的"存了却什么都不做的设置"。 |
| 低 | `internal/compatibility/anthropic/native.go` | `document` 块未置 `Requirements.InputImage`，信封低报了多模态请求。 |
| 低 | `web/src/pages/ProvidersPage.tsx` | `errors.anthropicBetas` 是死键：既无客户端校验，服务端也未返回对应 code，操作者输入非法 token 只会看到通用横幅、字段无高亮。 |
| 低 | `internal/anthropicapi/types.go` | `ParseBetaTokens` 拒绝尾随逗号。HTTP 列表语法容忍空元素，靠拼接构造该头的 SDK 会踩到。 |

### 3.2 既存缺陷（本次 review 暴露，非本次引入）

| 严重度 | 位置 | 问题 |
|---|---|---|
| 高 | `internal/gateway/service.go` | **native 流式对文本内容从来就没工作过。** `stream.Process` 会扣留尾部约 2047 字节以处理跨 chunk 边界，因此 `processed` 永远不等于 `original`，每个含文本的 delta 都被拒。已实测短文本、长文本一律被拒，且 HEAD 的比较逻辑逐字相同。仓库唯一的 native 流式测试只用 `signature_delta`，所以一直不可见。本次改动把 `thinking_delta` 也拖进了这条路径。另：`Stream.Flush()` 在 native 流结束时从未被调用，比较逻辑修好后这会立刻变成活的问题。 |
| 高 | `internal/anthropicapi/types.go` | `wireValue()` 的 default 分支对没有 `Raw` 的块只输出 `{"type": ...}`。portable 视觉请求发往 Anthropic 时会送出**没有 source 的 image 块**（`Validate` 检查的是 struct 不是字节，所以放行）。本次新增的 `document`/`search_result` 又停在同一个坏默认上。 |
| 中 | `internal/redaction/engine.go` | `ProcessJSON` 只遍历值，不遍历对象键，也不遍历数字。密钥放在键位置能通过；`{"card":4111111111111111}` 作为数字也能通过。这削弱了 §1.1 修复所依据的"检查面等于接受面"。 |
| 中 | `internal/gateway/service.go` | `extractNativeGovernance` 产出的 `Requirements` 对路由是**死代码**——native 路径从不调用 `filterSemanticCapabilities`。因此 `output_config.format` 能抵达能力上限没有 `JSONMode` 的 Mantle 目标。`provider_fields.go` 中声称 Mantle 上限受保护的注释断言了并不存在的保护。 |
| 低 | `internal/gateway/service.go` | 入站遍历为 4 MiB 上限载荷额外增加约两次完整泛型解析（实测约 444 MB 分配 / 540 ms）。受项目并发上限约束，属放大而非无界向量。 |

---

## 4. 验证记录

- `go build ./...`、`go vet ./...`：0
- `go test ./...`：0，54 个包通过
- 前端：`typecheck` 0、324 个测试通过、`internal/webui/dist` 已重建（密钥扫描 11 文件干净）
- 反向验证：安全修复的两半均确认"回退生产代码后测试失败"
- 已知不可区分的测试：`TestPreparePayloadRewritesOnlyModelAndStream` 在旧实现下同样通过（注释已如实说明）。review 找到了两个能区分的载荷（`"content":"hi"` 字符串形式、`"stop_sequences":[]`），应补入该测试

---

## 5. 升级注意事项

- **不需要重新初始化数据目录。** 未改动任何持久化格式版本；`AllowedAnthropicBetas` 空值即旧行为，历史记录原样解码。
- **现存 Anthropic 连接需手工启用 `json_mode`。** 能力默认值只在新建时生效，`ProfileAnthropicMessages` 不在 immutable 名单中，因此已存记录保持 `json_mode: false`，portable 结构化输出请求会被路由过滤掉，直到运营者在该连接上勾选。native 模式不受影响（该路径不做能力过滤）。

---

## 6. 建议的后续顺序

1. 修 §3.1 中的高/中项，重点是 `effort: "max"`、嵌套未知字段、族前缀锚定——这三条都关乎"发出去的东西"。
2. 修 §3.1 的契约不一致（`NativeHeaders`、`Strict` 不对称、nameless `json_schema`）。
3. 单独立项处理 §3.2 的两条高危既存缺陷，尤其 native 流式——它意味着该模式目前对真实负载不可用。
4. 再回到 §2 的两项未完成工作。
