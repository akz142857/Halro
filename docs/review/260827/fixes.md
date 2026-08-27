# 整改记录（叫停点 2）

对抗验证判 CONFIRMED 的三条中，两条是同一处结构的两个面，合并为一处修复；一条独立。
每条带回归测试与**实际执行过的**反向验证——按 `CLAUDE.md`，反向验证若不失败就不算证据，
所以每次退回都先断言编辑真的落地，再以 `-count=1` 无缓存运行。

## 修复一 · 出站失败发生在账目关闭之前（A2-4 / A1-4，V2+V3 CONFIRMED）

**问题**：`generate` 把语义结果交还门面，门面再渲染成调用方的 wire 形式。渲染失败时
`attempt.finish` 与 `run.finalize` 早已提交，`run.finalize` 的幂等位又吞掉任何补记，于是账本
记 `success/200` 而调用方收 502 `provider_error`——记录与答案互相矛盾，且把网关自身的缺陷
归因给上游。V2 实测到的形态：`attempt outcome="success", http_status=200,
committed_micros_usd=20`，调用方 502。

**改法**（`internal/gateway/service.go`）：`generate` 收一个 `render func(semantic.GenerateResult) error`
参数，与既有的 `generateStream(..., emit func(...) error)` 同形。出站脱敏之后、`run.finalize`
之前依次做三件事，任一失败都决定 outcome：

| 情形 | outcome | 答给调用方 |
|---|---|---|
| 脱敏策略拒绝 | `policy_rejected` | 422 `sensitive_output_detected`（不变） |
| 脱敏把结果改坏（Validate 失败） | `policy_rejected` | 500 `redaction_policy_error`（新） |
| wire 承载不了某内容种类 | `provider_error` | 502 `provider_error`（状态码不变，账目改对） |

第二行是新增的错误码。它存在的理由是归因：策略没有"拒绝"这个答案，是把它**改坏**了，
说清这一点才能让运维去查自己的规则，而 `provider_error` 会把他们支到上游去。

`semanticResponse.Model = publicModel` 相应上移到渲染之前——门面渲染的一直是公开别名，
这个赋值原本在 finalize 之后。

**不做退款也不做重试**：上游调用真实发生、真实计费。要保证的是记录与答案说同一件事。

**覆盖范围**：`Chat`(`:923`) 与 `Responses`(`:1108`) 两个门面。`Messages`(`:1195`) 走
`Chat` 之后还有两次自己的渲染（`DecodeGenerateResult` + `RenderResult`），仍在 `generate` 之外
——V3 判定该链为**存量形状**（v0.3.0 逐字节相同）且其点名的 citations 触发条件不可达，
故不在本次修复内，见下文"未做"。

**回归测试**（`internal/gateway/outbound_failure_outcome_test.go`，新增）：
1. `TestUnrenderableAnswerIsNotRecordedAsSuccess` — 渲染失败时账本 outcome 为 `provider_error`，
   调用方 502，且渲染自身的错误未被吞掉；
2. `TestRenderedAnswerIsStillRecordedAsSuccess` — 成功路径仍记 `success`（防止上一条对着一个
   "什么都失败"的服务通过）；
3. `TestRenderRunsBeforeTheRequestIsFinalized` — 渲染回调运行时回放 WAL，断言此刻**尚无**
   `RequestFinalized` 事件。次序本身是这次修复的全部内容，所以次序要被直接钉住。

`internal/redaction/empty_replacement_test.go`（新增）钉住机制的另一半：`$1` 模板在无捕获组的
规则上展开为空，把 citation URL 清空，产物 `Validate()` 失败。机制在 redaction 包、后果在
gateway 包，两处各钉一半。

**反向验证（已执行）**：删掉 `service.go` 里的 render 分支（脚本先 `assert old in s`，
确认搜索串未因 gofmt 重排而失效），三条测试全部失败：

```
--- FAIL: TestUnrenderableAnswerIsNotRecordedAsSuccess
    a render that failed answered the caller successfully
--- FAIL: TestRenderedAnswerIsStillRecordedAsSuccess
    generate returned without rendering the answer
--- FAIL: TestRenderRunsBeforeTheRequestIsFinalized
    the request was already finalized as "written before the render ran" when the render ran
```

恢复后复跑通过。

## 修复二 · 识别阶段的探针预算分母（A5-1，V4 CONFIRMED）

**问题**：`spendDetectionProbe`（`internal/app/admin_model_capability_detections.go`）按
`len(remaining)` 分时间片，而识别阶段只跑无依赖的根探针（1–2 条）。主循环早已改用可运行计数，
识别路径没同步。实测：90s 总预算、60s 尝试上限下，根探针拿到 **9.99s**（V4 复现），60s 上限
完全不起作用。真实最强路径是 Bedrock Mantle 两条路由各 9 探针/1 根，两边都精确 10s——正是
`3503658` 观测到"16-token 探针 12.9s 不够"的同一个 provider。

**改法**：把可运行计数抽成共用帮手 `runnableProbes(results, remaining)`，主循环与识别路径都调用它。
两份重复的计算正是漂移的成因，所以修的同时把它并成一份。修后 Mantle 得 60s、OpenAI chat 得 45s。

**回归测试**（`internal/app/detection_probe_budget_test.go`，新增）：
1. `TestProbeBudgetCountsOnlyProbesThatCanRun` / `TestProbeBudgetExcludesDependantsOfAnUnsupportedProbe`
   — 帮手自身的语义（依赖未确立不计、依赖被拒后其从属不计）；
2. `TestIdentificationRootProbeIsBoundedByTheAttemptTimeout` — 驱动真实识别路径，用既有的
   `budgetRecordingDetector` 读回探针实际拿到的截止时间，断言 ≥45s。

**反向验证（已执行）**：把调用点改回 `len(remaining)`（同样先断言编辑落地），测试失败并报出
确切数字：

```
--- FAIL: TestIdentificationRootProbeIsBoundedByTheAttemptTimeout
    identification gave the root probe 14.992045139s of a 90s budget with a 60s attempt timeout
```

14.99s 与 V4 的算式（90s ÷ 6 探针）吻合。恢复后复跑通过。

## 修复三 · 替换模板不得引用不存在的捕获组（V2 的第二条建议，窄化后采纳）

**问题**：模板是自由文本，域校验只查非空与 ≤256，`compileRule` 根本不读它，而 `Expand` 对不存在
的引用一律展开为空。于是 `$1` 写在一条没有捕获组的规则上会被存下来，然后把匹配到的内容**删掉**
而不是遮蔽——作用在必须非空的字段（如 citation URL）上，就把一条脱敏规则变成了一条毁答案的规则。

**改法**（`internal/redaction/engine.go`）：新增 `validateReplacementReferences`，按 `Expand` 自己的
读法解析模板（`$name` 取最长的字母数字下划线串、`${name}` 有界、`$$` 是转义而非引用），拒绝引用了
正则不存在的组号或组名的模板。

**只拒绝越界引用**，这是窄化的关键：引用一个存在但本次未参与匹配的组（可选组）同样展开为空，但那是
合法写法；模板整体就是一个 `$1`（只保留捕获的那部分，例如只留后四位）也是合法写法。两者继续放行，
由修复一在网关侧兜住后果。写方案时我判断"判据会误伤合法写法"——那对的是宽判据，窄判据没有这个问题。

**回归测试**（`internal/redaction/empty_replacement_test.go`）：
`TestReplacementReferringToAMissingGroupIsRefusedAtCompile` 覆盖 V2 列出的四种写法里可判定的三种
（`$1`/`${1}`/`$2x`/`$name` 对无捕获组的正则）；
`TestOptionalGroupReplacementIsAllowedAndCanStillEmptyAField` 钉住**放行**的那一半仍能清空字段——
守卫收窄的是笔误，不是这个情形，而站在它后面的是修复一。

**反向验证（已执行）**：删掉守卫后 `replacement "$1" was accepted against a pattern with no capture groups`。

## 修复四 · `Detail` 与 `Status` 纳入强制基线（A2-2 PARTIAL + A2-3）

**问题**：出站遍历里，`ContentInputImage.Detail` 与 `ContentProviderToolCall.Status` 是仅剩的两个
"这条路径上没有任何一道 pass 看过"的字符串成员。`Status` 尤其要紧——它是**出站**方向、由上游填写、
原样抵达调用方，而 #231 修 `action.query` 的理由（上游写的字符串必须过强制基线）逐字适用于它，
只是漏了一个成员。这是同一个缺陷修了一半。

**改法**：在遍历尾部与 `CallID`、`Name` 同处，对两者调用 `validateMandatory`。

**校验而非改写**，与 `CallID`/`Name` 一致：两者在另一端都按固定词表读取，遮蔽会递给调用方一个它
自己的 switch 没有分支的值；拒绝表达的是同一件事，且不发明无人能处置的新词。

**回归测试**：`TestMandatoryBaselineCoversDetailAndStatus`。
**反向验证（已执行）**：删掉两处校验后 `a provider key in status reached the caller`。

## 修复五 · `Messages` 门面的两次渲染进入请求内（A1-16，V3 PARTIAL）

**问题**：`Messages` 在 `s.Chat` 返回之后还要做 `DecodeGenerateResult` 与 `anthropicwire.RenderResult`
两次渲染，两次都在请求已被 finalize 之后。V3 实测可达的触发是 `ContentReasoning` 与非法 tool 参数
JSON（其点名的 citations 触发已被证伪）。后果与修复一同形：账本记 success，调用方收 502。

**改法**：给 `Chat` 抽出内部变体 `chatWithRender`，把 wire 渲染交给调用方；`Chat` 传自己的渲染，
`Messages` 把它今天做的三步（chat 渲染 → 解码 → anthropic 渲染）作为一个回调传进去。

**刻意不做的事**：没有让 `Messages` 绕开 Chat 直接进 `generate`。请求仍按 `Chat` 的方式解码与路由，
这正是把 portable Messages 请求约束在 Chat wire 能表达的范围内的那道限制。V3 说的"重构"指的是拆掉
这条中转，那是另一个决定，不在修复里做。

**回归测试**：`TestMessagesRenderFailureIsNotRecordedAsSuccess`（工具调用参数非法 JSON）。
**反向验证（已执行）**：退回原来的 `s.Chat` 三步后 `the ledger recorded success for a request the
caller saw fail`——调用方确实收到了失败，账本确实记了 success，A1-16 的形状被实证坐实。

## 未做，及其理由

- **`Detail` 在解码层钉成枚举**（V1 的主修主张）。本轮已让强制基线覆盖它，fail-open 关掉了；
  把 `""|auto|low|high` 钉进解码是接受面变更，前端要同步，宜与控制台一并做。
- **`Messages` 不再经由 `Chat` 中转**。见修复五"刻意不做的事"。
- **控制台补渲染 `error_class`**（V4 的附带建议）。探测超时今天仍呈现为"暂时不可用"，运维读成上游
  抖动就会反复重试反复扣费。属前端改动，与 W11 的能力回退提示是同一批。

## 验证

- `gofmt -l` 三包干净；`go build ./...` 通过。
- `go test ./internal/gateway/ ./internal/app/ ./internal/redaction/ -count=1` 通过。
- 完整门禁 `make check` 见 `runtime-evidence.md`。
