# V4 · 证伪 A5-1（识别阶段探针时间片按全部探针均分）

## 裁决

**CONFIRMED（缺陷成立，非阻塞）** —— 但原报告的 OpenAI 复现路径里有一步算错，
真正精确落在 10s 的是 Bedrock Mantle 双路由那对候选，不是 OpenAI chat/responses 那对。

分母、根探针条数、10s、`ProbeUnavailable`、`DetectionFailed`、两次计费调用，全部实测复现，
没有找到任何拦截防御。唯一无法在本机独立验证的是「10s 不够」这个前提本身——它依赖仓库自己
在 `3503658` 里记录的真实观测，不是评审自造的假设。

---

## 证据

### 1. 分母确实是 9（对 chat 类档案）

`internal/app/admin_model_capability_detections.go:762-767`

```go
if deadline, ok := ctx.Deadline(); ok && len(remaining) > 0 {
    fairShare := time.Until(deadline) / time.Duration(len(remaining))
    if probeTimeout := min(fairShare, r.config.Gateway.AttemptResponseHeaderTimeout.Value()); probeTimeout > 0 {
```

`remaining` 只有一个调用点：`:654` 传入 `plan.Probes`，即**整个计划**，不是「本阶段会跑的那几条」。
（`grep spendDetectionProbe` 全仓只有 `:654` 一处调用、`:737` 一处定义。）

用一次性测试（写在 scratchpad 逻辑里、已删除）实测各档案的 `plan.Probes`：

| profile | probes | roots | deferred |
|---|---|---|---|
| `openai.chat-embeddings.v1` | **9** | **2**（chat, embeddings） | `[reasoning]` |
| `openai.responses.v1` | **6** | **1**（chat） | — |
| `bedrock.mantle.chat.v1` | **9** | **1**（chat） | — |
| `bedrock.mantle.openai.chat.v1` | **9** | **1**（chat） | — |
| `bedrock.mantle.responses.v1` | 8 | 1 | — |
| `anthropic.messages.2023-06-01` | 6 | 1 | — |

来源：`internal/provider/capability_detection.go:127-183`（add 顺序 chat→streaming→stream_usage→
tools→json_object→structured_outputs→developer_role→vision→embeddings→moderations→rerank→reasoning）
与 `internal/domain/provider_table.go:87-92`（`openAIChatSet`）、`:122-125`（`openAIResponsesSet`）。

原报告没有把两个 remaining 搞混：`:654` 传的确实是全量 `plan.Probes`。

### 2. 识别阶段确实只跑根探针

`:642-645`：`for _, probe := range plan.Probes { if len(probe.DependsOn) > 0 { continue } }`，
`:648` 单候选直接 `break`（不花钱），`:667-669` 一旦 `Supported` 立刻 `break`。
所以每个候选实际最多跑 1–2 条，而分母是 9（或 6/8）。

### 3. 10s 的算式（默认配置代入）

- `TotalTimeout = 90s` —— `internal/config/default.go:52`
- `AttemptResponseHeaderTimeout = 60s` —— `internal/config/default.go:72`
- `MaxProviderCalls = 10` —— `internal/config/default.go:53`（对本条无约束，识别最多花 4 次）
- `ctx` 的 deadline 由 `:446` `context.WithTimeout(parent, TotalTimeout)` 设定

`probeTimeout = min(90s / len(plan.Probes), 60s)`：

| 版本 | chat 档案 probes | fairShare | min(…, 60s) |
|---|---|---|---|
| v0.3.0 `abfc05c` | 8（`capability_detection.go:66,68` 字面量 8；`json_mode` 未拆） | 11.25s | **11.25s** |
| HEAD `8bb4847` | 9（`capability_detection.go:27` `maxDetectionProbes = 9`） | 10s | **10s** |

`min()` 里的 60s 完全不起作用——原报告这一点正确。

**实测**（临时测试，两个候选、9 条探针、1 根，跑真实 `identifyCapabilityBinding`）：

```
status=ambiguous calls=2
candidate A budgets=map[…openai.chat-embeddings.v1/chat:9.994583129s]
candidate B budgets=map[b-media/chat:9.997246236s]
```

9.99s ≈ 10s，算式复现无误。

### 4. 10s 够不够——本条成立与否的关键

探针本体（`internal/provider/capability_detection.go:229-243, 246-251`）：

- 请求体：一条 user message `"Reply briefly."`，`MaxInputBytes: 2048`（`:132-133`）
- 输出上限：`MaxOutputTokens: 16`（`:132-133`），落到 `max_completion_tokens`（DeepSeek 走 `max_tokens`）
- **根探针 kind 恒为 `minimal_chat`，不设 `reasoning_effort`**——`reasoning_effort` 只在
  `"reasoning_effort"` 这个 kind 里设（`:320-334`），而该探针 `DependsOn: ["chat"]`，识别阶段永远选不中
- 非流式，无重试（`DetectCapability` 一次 `b.Chat` 即返回）

也就是说：请求极小、输出被压到 16 token、不主动要求深思考。对普通模型 10s 绰绰有余。

但**仓库自己已经把这个前提写死了**。`3503658`（2026-08-21，`fix(detection): give the probe
everything else waits on the whole budget`）的提交信息：

> …a seventh of 90s on the Bedrock Mantle plan, 12.9s. **A frontier reasoning model cannot
> answer one non-streaming completion in that**, so a model that works was reported as a
> timeout on every capability.

那次修复面对的是**同一条 `minimal_chat` 探针、同样的 `MaxOutputTokens: 16`**
（`git show 3503658^:internal/provider/capability_detection.go:72` 已是 16）。既然 12.9s 被判为不够，
10s 只能更不够。这条前提我在本机无法独立复测（需要真实计费账号），但它不是评审自造的：
它是仓库为了做那次修复而记录的观测，并且现在还以注释形式留在 `:560-571`。

**同一条探针，两条路径，预算差 6 倍**：单候选走主循环 `:572-578`，`runnable` 在 probeIndex=0 时
恒为 1（其余全 `DependsOn: ["chat"]` 且 `d.Results` 为空），`fairShare = 90s` → `min(90s,60s) = 60s`
（`admin_model_capability_detections_test.go:394-417` 断言 `root >= 45s`）；多候选走识别路径 → 10s。

### 5. 后果校准

- 超时 → `classifyCapabilityProbeError`（`capability_detection.go:419-421`）→ **`ProbeUnavailable`**，
  `ErrorClass = "timeout"`（`:472-474`）。原报告说的没错，能力不会被静默判成 unsupported。
- 两候选都 unavailable → `:692` `finishDetectionWithoutProbe(d, DetectionFailed)`。
  账目、`Baseline`、`RecommendedFromProbes` 都不受影响。
- 真实损失：**2 次计费调用 + 一个能用的模型被标 detection failed**，且这是**确定性**的，
  重试必然重现同样结果。
- **运维看到的信息说不清原因。** `web/src/pages/DeploymentsPage.tsx:2229-2250` 渲染失败候选时
  只取 `candidate.capability` 与 `candidate.status`，**不渲染 `error_class`**；
  `zh-CN.ts:841` 把 `unavailable` 译作「暂时不可用」。于是一个**必然复现**的、由 Halro 自己
  10s 截断造成的失败，在界面上呈现为「chat：暂时不可用」，指向上游抖动。`error_class: "timeout"`
  只在 API 记录里（`publicCapabilityDetection` `:957-969` 不删 `binding_candidates`），控制台不显示。
  这一层把「可接受的探测失败」推向「缺陷」：错误信息把确定性故障说成了暂时性故障。

### 6. 其它防线：没有

- `spendDetectionProbe` 里没有 `max(runnable,1)` 之类的下限，只有 `len(remaining) > 0` 的除零保护（`:762`）。
- 无重试：`DetectCapability` 单次调用，识别失败即终止，没有更宽预算的第二轮。
- `d.ProviderCalls >= d.MaxProviderCalls`（`:651`）是 10，识别最多用 4 次，管不到超时。
- 上层 `min(…, AttemptResponseHeaderTimeout=60s)` 是**上限**，只会更小不会更大。
- `safetransport` 的 `ResponseHeaderTimeout` 同为 60s，同样不构成下限。

---

## 与原报告的差异

1. **【原报告算错一步】** 复现路径第 5 步说 OpenAI 两个候选都得 `min(90s/9, 60s) = 10s`。
   实际 `openai.responses.v1` 的计划只有 **6** 条探针（无 streaming、无 embeddings、无 reasoning，
   见 `provider_table.go:122-125`），分母是 6 不是 9 → `90s/6 = 15s`。
   所以第 7 步「两个候选都如此 → answered 为空」在 OpenAI 这对候选上并不必然成立。
2. **【更强的路径原报告漏了】** `bedrock.mantle.chat.v1` 与 `bedrock.mantle.openai.chat.v1`
   两条路由**各有 9 条探针、各只有 1 条根探针**，两边都精确落在 10s。而这恰好就是
   `internal/app/providers.go:723-729` 明说的排布（同一个 Mantle 连接的两条路由，一个模型只在其中
   一条上；注释举的例子 `openai.gpt-5.6-sol` 正是前沿推理模型），也正是 `3503658` 记录那次超时的
   同一个 provider。识别就是为这个场景造的。
3. **【值得记一笔的次生后果】** 若两个候选预算不等（如 OpenAI 的 10s vs 15s），可能出现
   chat 被 10s 截断、responses 在 15s 内答上 → `answered` 只剩一个 → 识别**静默选中 responses**，
   而不是报 ambiguous 让运维选。运维因此失去选择权，并丢掉只有 chat 档案才有的 embeddings/streaming。
   这不改变分级，但说明「按全量探针均分」的伤害不止于超时。
4. **【8→9 的归因】原报告自己已说清且正确**：缺陷本体在 v0.3.0 就有（11.25s，同样低于 12.9s），
   本范围 `8bb4847` 把 `maxDetectionProbes` 提到 9 只是把它从 11.25s 收窄到 10s。
   不是本范围引入的，是本范围加重的。
5. 原报告说「主循环 `:572-578` 已修掉同一缺陷、识别路径没同步」——属实，且有时间线佐证：
   `spendDetectionProbe` 由 `7d1a48b`（08-11）引入，`3503658`（08-21）只改了主循环。

---

## 最小修复建议

把 `:762-767` 的分母换成与主循环 `:572-578` 同样的 runnable 计数。识别阶段 `d.Results` 为空，
`probeDependenciesSupported` 对任何有依赖的探针都返回 false，所以 runnable 恒等于「无依赖的根探针数」——
正是该阶段真正会跑的条数。

```go
if deadline, ok := ctx.Deadline(); ok {
    runnable := 0
    for _, pending := range remaining {
        if probeDependenciesSupported(d.Results, pending.DependsOn) {
            runnable++
        }
    }
    fairShare := time.Until(deadline) / time.Duration(max(runnable, 1))
    if probeTimeout := min(fairShare, r.config.Gateway.AttemptResponseHeaderTimeout.Value()); probeTimeout > 0 {
        probeContext, probeCancel = context.WithTimeout(ctx, probeTimeout)
    }
}
```

修复后：Mantle 路由 `90s/1 → min(90s,60s) = 60s`；OpenAI chat 档案 `90s/2 = 45s`。
与主循环给同一条探针的预算一致，这正是 `3503658` 想要的性质。

两个附带建议（各自独立，不必与本修复捆绑）：

- 这段 `runnable` 计算现在会在主循环和 `spendDetectionProbe` 里各存一份。既然本仓 pre-1.0.0
  的规矩是「一个错误构造不得与其替代品并存」，更干净的做法是抽成一个
  `probeTimeoutFor(deadline, results, remaining, attemptTimeout)` 帮手，让两条路径不可能再走散一次。
- `DeploymentsPage.tsx:2229-2250` 的失败候选列表应把 `candidate.error_class` 一并渲染出来。
  否则一个确定性的 10s 截断在界面上永远读作「暂时不可用」，运维只会不停重试。

## 复现方式（本机已跑，测试文件未留仓库）

1. `internal/provider`：用 `DefaultProviderCapabilitiesForProfile` 造各档案的 bridge，
   打印 `len(plan.Probes)` 与根探针数 → 得上表。
2. `internal/app`：用 `twoInterfaceProviderForTest` + 两个 9 条探针/1 根的 detector，
   在 `DetectCapability` 里记 `time.Until(ctx.Deadline())`，跑 `runDetectionForTest` → 9.99s。
   （对照组：现有 `TestRootProbeIsBoundedByTheAttemptTimeoutNotAFractionOfTheBudget` 单候选走主循环，断言 ≥45s。）
