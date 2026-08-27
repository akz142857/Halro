# V1 对抗验证：A2-2 `ContentInputImage.Detail` 跳过脱敏

## 裁决

**PARTIAL** —— 机制部分**逐条复现成立**（Detail 确实不被检查、全链无枚举校验、v0.3.0
确实拒绝、`engine.go:266-268` 的 "member for member unchanged" 注释确实不成立）；
但**严重程度与范围与原报告不同**，不应按"fail-open 泄漏"记为阻塞项：

1. 泄漏方向是 **调用方 → 管理员配置的上游 Provider**，不是 Provider → 调用方，
   也不进应答体、日志或审计。Detail 是**只进不回**的字段。
2. 部分机队上**有一道防线在 redaction 之前就拦住它**：
   `internal/compatibility/provider_fields.go:75`、`:101`（判定函数 `hasImageDetail`，
   `provider_fields.go:328-340`）对 `ProfileAnthropicMessages` /
   `ProfileBedrockMantleAnthropicMessages` 把任何 `detail != "auto"` 声明为不可承载字段，
   经 `internal/gateway/service.go:960 filterGenerateProfileCompatibility`
   （实现在 `service.go:2556-2560`）在 **redaction（`service.go:967`）之前**剔除目标；
   目标全被剔除即 400。OpenAI / Azure / DeepSeek / openai_compatible / Responses
   这几个 profile 没有这道门，Detail 会被原样渲染上行。
3. "回归"的根因比 Detail 宽，但**实际丢失的转发面比原报告窄**（见下 §2）。

## 证据

### 1. HEAD 侧：Detail 确实完全跳过（原报告成立）

`internal/redaction/engine.go:339-347`，`ContentInputImage` 分支只对 `part.URL`
跑 `ProcessText`，`part.Detail` 既不过项目策略也不过强制基线
（`validateMandatory`，`engine.go:727-734`；分支尾部 `engine.go:373-378` 只补查
`CallID` / `Name`）。

探针（临时测试，已删除，`go test -count=1`）：

```
detail 携带 sk-…            → err=<nil>，Detail 原样输出         （放行）
同一 secret 放进 URL query  → err=request contains secret material（拒绝）
```

取值域：**Halro 侧是自由字符串，无任何枚举校验**（原报告第一条前提成立，
"进 redaction 前已被校验成枚举"这一反驳不成立）：

- Chat wire 解码：`internal/compatibility/openai/mapping.go:263-267`（匿名 struct
  的 `Detail string`）、`:280-284`（直接塞进 `semantic.Content{…, Detail: detail}`），
  非 strict 解码，无值域检查。
- Responses wire 解码：`internal/openaiapi/responses.go:75`
  （`ResponseInputContent.Detail string`）、
  `internal/compatibility/openai/responses.go:178-183`。`strictUnmarshal` 只拒**未知成员**，
  不校验已知成员的值。
- 语义校验：`internal/semantic/content.go:151-154`，`ContentInputImage` 分支只要求
  `Role==user && URL!=""`，**完全不看 `Detail`**（对比同文件 `:137`、`:148`、`:156`、
  `:160`、`:175` 都只是"要求 Detail 为空"，唯独 image 分支不约束其内容）。
- 上行渲染：Chat `internal/compatibility/openai/mapping.go:322-327`；
  Responses `internal/compatibility/openai/provider_responses.go:92`。两处都把
  Detail 原样写给上游。

注意 OpenAI **协议本身**的 `detail` 是 `auto|low|high` 枚举，所以任何合规 SDK
都不会把 secret 放进去；能触发这条路径的只有**手写 JSON 的持钥调用方**。

### 2. v0.3.0 侧：确实拒绝，但拒绝的原因不是"脱敏了 detail"

v0.3.0 `ProcessInboundChat`（`engine.go:256-282`）对整个 `message.Content` 原始
JSON 调 `processRaw` → `transformValue`（`engine.go:424-452`），**递归遍历 map 的
每一个 key 与 string value**。detail 只是恰好作为一个 map 值被覆盖到。

v0.3.0 worktree（`scratchpad/v030`，abfc05c，v0.3.0+2）探针：

```
{"image_url":{"url":…,"detail":"sk-…"}}   → request contains secret material
{"image_url":{"url":…},"whatever":"sk-…"} → request contains secret material   ← 关键
```

第二行证明 v0.3.0 的覆盖来自**通用整段遍历**，而非对 detail 的专门处理。因此这是
"覆盖模型从 wire 整段遍历换成语义固定成员表"带来的**结构性收窄**，Detail 是其中一个实例。

但这同时**收窄了实际影响**：HEAD 的 Chat 解码是固定 struct 非 strict
（`mapping.go:259-268`），未知成员被**丢弃**，根本到不了上游；Responses 解码是
`strictUnmarshal`（`responses.go:169`），未知成员直接 400。所以 v0.3.0 能挡而 HEAD
放行**且仍会上行**的，只有语义模型真正承载的成员——即 `Detail`（本条）与
`ProviderToolCall.Status`（A2-3），不是"所有字符串"。

引入提交：`8bb4847`（#231）——v0.3.0 以来唯一改动 `internal/redaction/engine.go`
的提交，即本轮工作，"本轮引入"的说法成立。

### 3. 完整可达路径与泄漏面

前置条件：持有效 Gateway Key + 手写请求体（绕过 SDK）+ Project 路由命中一个
不含 `hasImageDetail` 规则的 profile（OpenAI / Azure / DeepSeek / openai_compatible /
OpenAI Responses）。

结果：任意字节随 `image_url.detail` / `input_image.detail` 上行至该 Provider。
回流面：**无**。`RenderResponseResult`（`responses.go:221`）与
`renderResponseOutput`（`responses.go:282`）只渲染助手输出，不回显输入；
`ContentInputImage` 经 `content.go:151` 限定为 `RoleUser`，
`ProcessOutboundGenerateResult`（`engine.go:299-314`）遍历的助手内容里不会出现它。
审计/日志不落请求内容。

所以泄漏面 = **一条通往管理员已配置、已在 SafeTransport 允许名单内的上游主机的
covert channel，收方非攻击者可控**；且真实 OpenAI 会对非枚举 detail 直接 400
（字节仍已发出）。它是 DLP 出口覆盖漏洞（检查面 < 接受面），不是机密披露。

### 4. 其他防线

- `provider_fields.go:75`、`:101`（+ `:328-340`）：Anthropic / Bedrock Mantle 两个
  profile 上，`detail != "auto"` 直接把目标判为不可承载，`service.go:960` 在
  redaction 之前执行 → 无可用目标即 400。**这是唯一一道在 redaction 之外真正拦住它的
  防御，但只覆盖这两个 profile。**
- semantic `Validate`：`content.go:151-154` **不拦**。
- wire 层：`mapping.go:280-284` / `responses.go:178-183` **不拦**。
- Provider 侧：真实 OpenAI 会拒绝非枚举值，但那是上游行为，不是 Halro 的防线，
  且拒绝发生在字节送达之后。

## 与原报告的差异

| 项 | 原报告 | 本次裁定 |
|---|---|---|
| Detail 跳过脱敏 | 成立 | **成立**（engine.go:339-347） |
| 全链无枚举校验 | 成立 | **成立**（mapping.go:280-284 / responses.go:178-183 / content.go:151-154） |
| v0.3.0 拒绝 | 成立 | **成立**，但原因是整段 JSON 遍历（v0.3.0 engine.go:424-452），非针对 detail |
| "member for member unchanged" 注释矛盾 | 成立 | **成立** |
| 定性为 fail-open、**阻塞候选** | — | **下调**：出口方向 DLP 覆盖漏洞，不回流调用方/日志/审计；需持钥方手写请求；Anthropic/Mantle 路由上被 provider_fields.go:75/101 提前拦下 |
| 影响面 | 未界定 | 仅 OpenAI/Azure/DeepSeek/openai_compatible/Responses profile，且仅 Detail 与 Status 两个成员（未知成员在 HEAD 被丢弃或 400） |

结论：**该修，不该拦发布**。与 A2-3 同一根因、同一次修完。

## 最小修复建议

优先做"把接受面钉回协议值域"，因为它同时消掉 A2-3 那一类"解码端不钉枚举 →
检查面小于接受面"的结构性问题：

1. 在两个解码入口把 `detail` 限定为 `""|auto|low|high`，否则 400：
   `internal/compatibility/openai/mapping.go:280-284`、
   `internal/compatibility/openai/responses.go:178-183`；
   并在 `internal/semantic/content.go:151-154` 的 `ContentInputImage` 分支加同样断言，
   使非 facade 来源也钉死（`hasImageDetail` 的 "auto 即默认" 语义已经预设了这个值域）。
2. 纵深防御一行：`internal/redaction/engine.go:339-347` 对 `part.Detail` 也跑一次
   `e.ProcessText`（与 `part.URL` 同法）。策略选 1 后这行几乎永不命中，但它让
   traversal 的"每个承载调用方字节的成员都过一遍"这条不变式在代码里自证。
3. 回归测试：断言 `detail:"sk-…"` 在 Chat 与 Responses 两个 facade 上都 400；
   断言 `ProcessInboundGenerate` 对 Detail 携带的 secret 返回 `ErrSecretDetected`。

属改接受面的行为变更（`"HIGH"` 这类大小写变体今后会 400）；pre-1.0.0，无需兼容层。
不需要重新初始化数据目录。
