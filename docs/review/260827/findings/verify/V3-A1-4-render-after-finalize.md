# V3 证伪：A1-4 / A1-16 出站渲染落在 `run.finalize` 之后

角色：阶段 4 证伪 V3。对照基准：v0.3.0（`abfc05c`，worktree `scratchpad/v030`）与 HEAD `8bb4847`。
方法：代码走查 + 在两侧 worktree 跑同一组临时探针测试（已删除，未入库）。

---

## 一、裁决

| 发现 | 裁决 | 一句话 |
|---|---|---|
| **A1-4** | **CONFIRMED（结论成立，机理与归因需改写）** | 缺陷可达并已复现：账本记 `success`、调用方收 502。但报告给出的触发面（Chat 档案上的 citations）是上游条件性的罕见路径；真正确定性可达的是**Responses 门面遇到 reasoning 内容**，报告未列出。且「渲染从 attempt 循环内移到 facade」只对 Chat（923）成立，Responses（1108）在 v0.3.0 **就已在 facade、就已在计费之后**。 |
| **A1-16** | **PARTIAL（结论成立，触发条件 REFUTED，"叠两次" REFUTED）** | 缺陷可达并已复现（`outcome="success"` + 502）。但报告点名的触发条件 **citations 在这条链上不可达**——`:1195` 的输入来自 `DecodeGenerateResult(chatResponse)`，Chat wire 没有 citation 成员，`mapping.go:270-299` 从不写 `Citations`。真正的触发是 **ContentReasoning** 与 **非法 tool 参数 JSON**。与 A1-4 的两个渲染点触发集合互斥，不存在「这条链上叠了两次」。存量归因正确：v0.3.0 行为逐字相同。 |

---

## 二、结构事实（两条共用）

`generate` 在 `internal/gateway/service.go:1043-1046` 提交 `run.finalize("success")` 后 return；
三个门面随后各自渲染：

- `:923` `openaiwire.RenderGenerateResult(result)` → 失败走 `s.returnFailure(...)`
- `:1108` `openaiwire.RenderResponseResult(result, request)` → 同上
- `:1191` `openaiwire.DecodeGenerateResult(chatResponse)` / `:1195` `anthropicwire.RenderResult(result, ...)` → 同上

**问题 3（账目自洽性）答案：报告的「记 success」断言成立，且无补记路径。**
`returnFailure`（`service.go:3050-3058`）只写一行 `logger.Warn` 后返回 502，不接触 `run`；
`run` 在 `generate` 内是局部变量，门面根本拿不到它。`run.finalize`（`:165-171`）带
`finalized` 幂等位，即使拿得到也只会静默返回 nil，需要一条独立的「改写 outcome」路径才行。
落库形态是 `ledger.EventRequestFinalized{Outcome:"success"}`（`budget/manager.go:760-775`），
即用量视图/聚合会把这次请求算作成功。

实测（临时探针，`newFixture` + `fakeAdapter`，HEAD）：

```
messages  err=provider response cannot be rendered safely  outcome="success" calls=1
responses err=provider response cannot be rendered safely  outcome="success" calls=1
messages-badtooljson err=provider response cannot be rendered safely outcome="success" calls=1
```

---

## 三、问题 1：渲染失败输入的完整枚举与拦截防线

### 3.1 `:923` Chat 门面 `RenderGenerateResult`

| 失败输入 | 唯一生产者 | 更早的防线 | 可达性 |
|---|---|---|---|
| text 带 citations（`compatibility/openai/mapping.go:318`） | `DecodeProviderResponse`（`provider_responses.go:140`），即 **Responses 族档案**（`openai.responses.v1` / Mantle Responses） | 无。`filterGenerateProfileCompatibility` 只在 `UnsupportedGenerateFields` 非空时剔除；`provider_fields.go:129-142` 的 Responses 规则对一个不带 name/stop/seed/n/reasoning_effort 的普通 Chat 请求全部不触发 | **上游条件性**：Chat wire 在 `mapping.go:42-43` 就拒掉 `type != "function"` 的工具，所以请求**永远没要过** web_search；`RenderProviderResponseRequest`（`provider_responses.go:48-56`）也不会自行注入工具。要触发必须上游在没被要求的情况下自带检索并回带 `url_citation` |
| 未知内容种类（`mapping.go:340`） | 同上，唯一能落到 default 的是 `ContentProviderToolCall`（`provider_responses.go:172`） | 同上 | 同上 |
| `result.Validate()` / `message.Validate()` | 适配器已在解码处 validate；此处唯一新增的失效可能是出站脱敏把 citation URL 改成空串（`semantic/content.go:141`），而 `ProcessOutboundGenerateResult`（`redaction/engine.go:299-314`）解码后不再 validate | 无 | 需先有 citations，同上 |

> 对「text 档案收到带 citations 的应答」这一问的直接回答：**不开 PET 时请求侧不会要求检索**，
> 因此 OpenAI 官方 `/v1/responses` 不会主动回 citations。但该档案对**兼容端点**开放
> （`provider/openai/adapter.go:203、252` 的 `TargetCustomEndpointModel`），指向一个会主动标注来源的
> OpenAI 兼容 `/v1/responses` 实现时即可触发。**没有任何更早的防线拦它**，所以不是不可达，
> 而是「需要一类特定部署 + 上游主动行为」。这一条单独看属于「防御纵深不对称」。

### 3.2 `:1108` Responses 门面 `RenderResponseResult` —— 真正确定性可达的一条

| 失败输入 | 生产者 | 可达性 |
|---|---|---|
| **`ContentReasoning`**（`compatibility/openai/responses.go:322` 的 default 分支） | `mapping.go:291-292`——任何**走 Chat wire 的目标**回带 `reasoning_content` 即产生（`provider/primitive.go:161` 的 legacy 主路径） | **确定性可达，已复现**。DeepSeek 侧有请求端防线（`compatibility/deepseek.go:183`「没人要就把 thinking 关掉」），但它只覆盖「没人要」的情形，且只覆盖 DeepSeek 档案；`openai.chat.v1` 指向兼容端点上的推理模型（vLLM/GLM/Qwen 等都回 `reasoning_content`）时无任何防线。Responses 请求带 `reasoning.effort`（`responses.go:28-29`）时路由**只会**选中具备 reasoning 能力的目标，命中率反而更高 |
| PET 名字非 web_search（`:308`） | `DecodeProviderResponse` 只会写 `ProviderToolWebSearch` | 不可达 |
| refusal 带 citations（`:329`） | 需 `termination=="refusal"` 且有 citations；Responses 档案的 termination 只由 status 推出 `complete`/`max_output`（`provider_responses.go:178-181`），Chat 档案又没有 citations | 不可达 |
| citations 无文本可标注（`:336`） | 需空 `output_text` 带非零跨度注解，`Content.Validate` 先拒（`semantic/content.go:141-143`） | 近乎不可达 |

### 3.3 `:1195` Messages 门面 `anthropicwire.RenderResult`

| 失败输入 | 判定 |
|---|---|
| **citations**（`compatibility/anthropic/mapping.go:247`） | **不可达。** 输入是 `openaiwire.DecodeGenerateResult(chatResponse)`（`service.go:1191`），Chat wire 没有 citation 成员，`mapping.go:270-299` 的 `decodeMessage` 从不写 `Citations`。退一步说，即使语义结果真带 citations，`:923` 会**先**失败，`s.Chat` 直接返回错误，`:1195` 根本执行不到 |
| **`ContentReasoning`**（`mapping.go:257` default） | **可达，已复现。**`mapping.go:291-292` → `renderMessage:329` 写回 `reasoning_content` → `decodeMessage:291` 再解出 `ContentReasoning` → Anthropic 渲染器 default 拒绝 |
| **非法 tool 参数 JSON**（`mapping.go:253`） | **可达，已复现。**Chat wire 解码 tool_call 时不校验 arguments 是否为合法 JSON（`mapping.go:293-295`），`semantic` 的 Validate 只查长度（`content.go:155`） |

**因此「这条链上叠了两次（923 一次、1195 一次）」是错的**：923 的触发集合（citations / PET）与
1195 的触发集合（reasoning / 坏 tool JSON）互斥，两者不会同时发生，也不会先后各计一次。

---

## 四、问题 2：v0.3.0 双侧对照 —— 「本轮新增」的归因需要拆成三份

同一组探针在 `scratchpad/v030` 跑出的结果：

```
messages  err=provider response cannot be rendered safely  outcome="success" calls=1   ← 与 HEAD 完全相同
responses err=<nil>                                        outcome="success" calls=1   ← v0.3.0 成功返回
chat      err=<nil>                                        outcome="success"           ← 两版都正常
```

三个门面的归因各不相同：

1. **Chat（923）——「移出循环」属实，但当时的失败输入并不存在。**
   v0.3.0 `service.go:981-987` 确实在 attempt 循环内渲染，失败折成
   `&provider.Error{Class: ErrorMalformed, Ambiguous: true}`，`settlementForResult`（`:2643-2661`）
   走 ambiguous 分支、按**真实上游用量**结算，`retryable`（`:2260-2262`）对 ambiguous 返回 false，
   outcome 记 `provider_error`，调用方收 `terminalProviderError`——自洽。
   HEAD 同样金额、同样不重试，但 outcome 变成 `success`。**标签回归属实。**
   不过 v0.3.0 **整棵树没有 `Citations` 类型，也没有 `ContentProviderToolCall`**（已 grep 确认），
   所以这个渲染点在 v0.3.0 本就无从失败。准确的说法是：*触发条件与错误标签是同一轮一起引入的*。

2. **Responses（1108）——「移出循环」是错的，v0.3.0 就在 facade、就在计费之后。**
   v0.3.0 `service.go:1065-1068` 的 `RenderResponseResult` 已经在 `s.Chat` 返回**之后**调用，
   失败同样是 `s.returnFailure` → 502，`run.finalize("success")` 早已提交。位置没变。
   真正变的是 `renderResponseOutput`：v0.3.0（`responses.go:258-280`）**没有 default 分支**，
   reasoning 内容被静默丢弃后 200 返回；HEAD（`responses.go:315-322`）改为拒绝。
   也就是说，本轮把一处**静默丢弃**换成了**拒绝**——方向正确（与流式侧
   `responses_reasoning_test.go` 的既有约束终于一致），但这个拒绝落在了 finalize 的计费一侧。
   **归因应写成「新增的是拒绝分支，不是渲染位置」。**

3. **Messages（1195）——纯存量。**v0.3.0 `service.go:1152-1159` 与 HEAD `:1191-1195` 逐字相同，
   实测行为也逐字相同。报告对这一半的存量判断正确。

---

## 五、问题 5：严重程度校准

报告的「金额正确、不退款、无无界重试」三点**成立**，但漏了一项：

- **金额**：`providerErr == nil`，`settlementForResult`（`service.go:2663-2674`）按真实上游用量结算，
  与成功返回时完全一致。不退款、不多收。✔
- **重试**：`generate` 内部不重试（渲染发生在 return 之后），无无界重试。✔
- **预留/租约**：`attempt.finish` 已结算、`run.finalize` 已提交、`defer run.close()` 正常释放，无泄漏。✔
- **漏项——调用方侧的二次计费**：推理路径上**没有**幂等键（`internal/idempotency` 只服务 Admin
  写接口，网关推理链上没有接入点），而返回码是 **502**，主流 SDK（openai-python/anthropic-sdk）
  默认对 5xx 自动重试。失败又是**确定性**的（推理模型每次都回 `reasoning_content`），
  于是一次调用会被自动重试到上限，每次都产生一笔完整的、计入账本的上游生成，调用方一次都拿不到。
  这不是记账错误，但把「标签错」放大成了「按 SDK 默认重试次数倍数计费」。
  若要给严重度定档：**仍不阻塞**（金额每笔都对、无静默退款、重试有界），
  但比报告描述的「标签错」要贵，建议把错误码从 502 `provider_error` 改成一个不诱导重试的类，
  或至少与 outcome 一起修。

---

## 六、与原报告的差异汇总

| 原报告表述 | 核实结论 |
|---|---|
| 「出站 wire 渲染本轮从 attempt 循环内移到 facade（923/1108）」 | 只有 923 是移动；1108 在 v0.3.0 就在 facade（v030 `service.go:1065`）。1108 变的是 `renderResponseOutput` 新增了 default 拒绝分支（HEAD `responses.go:315-322`），v0.3.0 是静默丢弃 |
| 「可达性：text 带 citations，属可达但罕见」 | 该触发只存在于 923，且要求 Responses 族档案 + 上游主动检索；确定性可达的触发是 1108 上的 `ContentReasoning`，报告未列出 |
| A1-16「`RenderResult` 拒绝 citations」 | **不可达**：`:1195` 的输入来自 Chat wire 解码，`mapping.go:270-299` 从不产出 `Citations`；且 923 会先失败。实际触发是 `ContentReasoning`（`anthropic/mapping.go:257`）与非法 tool JSON（`:253`） |
| A1-16「这条链上叠了两次」 | 错。923 与 1195 的触发集合互斥 |
| A1-16「Messages 层是存量形状」 | **正确**，双侧实测逐字相同 |
| 「金额正确、不退款、无无界重试」 | 正确，但 502 + 无推理侧幂等 + 确定性失败 ⇒ SDK 默认重试会按倍数产生真实费用，报告未覆盖 |

---

## 七、最小修复建议

优先级：**Responses（1108）> Messages（1195）> Chat（923）**，与报告给出的顺序相反。

推荐做法（一处改动覆盖三条链，且不破坏本轮「Responses 直连」的设计目标）：
给 `generate` 增加一个 `render func(semantic.GenerateResult) error` 参数，由各门面传入自己的
wire 渲染闭包，在 **`settlementForResult` 之前**、attempt 循环内调用；失败时折成
`&provider.Error{Class: provider.ErrorMalformed, Ambiguous: true}`——正是 v0.3.0
`service.go:983-987` 的形状。结果：outcome 记 `provider_error`、按真实用量保守结算、
`retryable` 因 `Ambiguous` 返回 false 故不重试，三个门面一次性对齐。

次选（若不愿改 `generate` 签名）：让 `generate` 返回一个「改写 outcome」的句柄给门面，
`returnFailure` 之前调用。注意 `run.finalize` 的 `finalized` 幂等位（`service.go:166-168`）
会吞掉第二次调用，必须是一条独立的补记路径，而不是再调一次 `finalize`。

无论选哪条，建议同时补两个回归测试（当前套件里没有任何一处断言这三个渲染点失败时的 outcome）：
- Responses 门面 + 回带 `reasoning_content` 的适配器 → 断言 `EventRequestFinalized.Outcome != "success"`
- portable Messages + 回带 `reasoning_content` / 非法 tool 参数的适配器 → 同上
