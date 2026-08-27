# 阶段 4 · 对抗验证裁决

对阶段 1 最严重的五条各派一个独立证伪角色：默认发现为错，要求在代码里复现完整可达路径或
找出拦截防御。REFUTED 与 PARTIAL 必须写出拦截它的那道防御在哪一行——没有行号的 REFUTED 不成立。

方法与前提见 [`review-plan.md`](review-plan.md) §8。本仓 260805 / 260807 / 260814 三轮评审的最严重
发现**无一条原样成立**，本轮延续这条规律：五条里两条 CONFIRMED（其中一条比原报告更宽、一条的
复现路径原报告算错）、两条 PARTIAL、一条 REFUTED。

## 裁决总览

| 编号 | 原发现 | 裁决 | 一句话 |
|---|---|---|---|
| [V1](findings/verify/V1-A2-2-input-image-detail.md) | A2-2 `ContentInputImage.Detail` 跳过脱敏 | **PARTIAL** | 机制成立但不是"回归"，是覆盖模型收窄；泄漏面只进不回 |
| [V2](findings/verify/V2-A2-4-replace-template.md) | A2-4 replace 模板 `"$1"` 清空 citation.URL → 已计费 502 | **CONFIRMED** | 比原报告更宽：触发写法 4 种而非 1 种；账目形态为实测 |
| [V3](findings/verify/V3-A1-4-render-after-finalize.md) | A1-4 / A1-16 出站渲染在 finalize 之后 | **CONFIRMED / PARTIAL** | 缺陷成立且实测复现三次；但触发面与"本轮新增"归因均被改写 |
| [V4](findings/verify/V4-A5-1-probe-budget.md) | A5-1 识别阶段探针预算分母用全量 | **CONFIRMED** | 成立，但原报告的复现路径算错；真实更强的路径是 Mantle |
| [V5](findings/verify/V5-A3-4-catalog-ceiling.md) | A3-4 目录 Ceiling 采纳可绕过 PET 默认关闭 | **REFUTED** | `admin_deployments.go:1064` 的 Clamp 拦住；实测构造 PET 目录条目打不开部署 |

## V1 · A2-2 → PARTIAL

**成立的部分**：`Detail` 在 Halro 侧确是自由字符串，全链无枚举校验
（`mapping.go:263-267/280-284`、`responses.go:178-183`、`semantic/content.go:151-154`），脱敏跳过它
属实；`engine.go:266-268` 的 "member for member unchanged" 注释确实不成立；引入提交确认为
`8bb4847`(#231)。

**被改写的部分**：原报告称"v0.3.0 同一输入被拒 = 本轮 fail-open 回归"。实测（v030 worktree）
v0.3.0 的拒绝来自 `ProcessInboundChat` 对整段 `message.Content` JSON 的通用递归遍历——证伪者
把 secret 放进一个**完全不存在的成员 `"whatever"`** 同样被拒。所以 v0.3.0 挡住 Detail 不是因为
脱敏了 detail，而是因为它扫整个 JSON。这是**覆盖模型收窄**，不是针对性删除。

该结论反过来收窄了影响面：HEAD 的 Chat 解码是固定 struct（未知成员丢弃）、Responses 是 strict
（未知成员 400），真正"v0.3.0 挡得住、HEAD 放行且仍上行"的只剩 `Detail` 与 A2-3 的 `Status`。

**泄漏面**：只进不回——调用方 → 管理员自己配置的上游 Provider。不回显、不落日志与审计
（`responses.go:221/282` 不回显输入；`content.go:151` 把 `ContentInputImage` 限定为 RoleUser，
出站遍历碰不到）。是一条收方非攻击者可控的 covert channel，不是机密披露。

**已有纵深（拦截行号）**：`compatibility/provider_fields.go:75/101/328-340` 对 Anthropic 与
Bedrock Mantle profile 把 `detail != "auto"` 判为不可承载，经 `gateway/service.go:960`
（实现 `:2556-2560`）在脱敏（`:967`）**之前**剔除目标。OpenAI/Azure/DeepSeek/openai_compatible/
Responses 无此门。

**不阻塞**。修复主张在解码层把 detail 钉成 `""|auto|low|high`，与 A2-3 同根因一并修。

## V2 · A2-4 → CONFIRMED（比原报告更宽）

**前提独立验证**：最小 Go 程序实测 `ExpandString`——无捕获组时 `$1`/`${1}`/`$2x`/`$name` 全部
展开为 `""`；**有组但该组未参与匹配**（`(foo)?`）也是 `""`；`$n` 越界同样。`$0` 与 `$$` 非空。
即触发写法有 **4 种而非 1 种**，只判"无捕获组"会漏掉两种。展开点 `engine.go:836`，调用方 `:639`。

**确无第二道校验**：前端 `RedactionPoliciesSection.tsx:61` 只 trim 非空；域校验
`domain/redaction.go:126/129` 只 trim + ≤256；`compileRule`（`engine.go:748-791`）**根本不读
`Replacement`**——不查捕获组数、无模板与正则的配对校验；`admin_redaction.go:264` 原样透传。
策略预览是可选的，不是强制 dry-run。

**可达性**：规则无字段级 target（只有 inbound/outbound），`processCitations`（`engine.go:397-424`）
对每条 citation 无条件 `ProcessText(citation.URL)`（`:412`）。前置条件收窄但不阻断：citation 唯一
来源是 OpenAI Responses 上游 profile 的 `url_citation`（`provider_responses.go:212`），需 web_search
链路且规则整串命中 URL。`semantic/content.go:141-143` 对空 URL **报错返回**，无丢弃分支。

**账目形态（实测，非推断）**：`attempt: outcome="success", http_status=200,
committed_micros_usd=20`；`run: outcome="success"`；调用方收 **502 `provider_error`**
（`service.go:1110` → `:3050-3057`）。顺序 `finish`(:1035) → `finalize`(:1046) → render 失败。
`generate()` 内**没有任何 `result.Validate()`**。

不退款是正确的（上游真实花费）。不自洽的是两点：账本写 success/200 而调用方收 502；错误码
`provider_error` 把网关自身的缺陷**归因给上游**，运维排障会去查上游。

**最小修复（根治整类）**：`service.go:1038-1044` 在出站脱敏后追加 `semanticResponse.Validate()`，
落入既有 `policy_rejected` 分支（422，且在 finalize 之前）。附带：`compileRule` 拒绝可能展开为空
的模板，判据须同时要求"引用有效"**且**"模板含非引用字面字节"——只查前者挡不住可选组。

## V3 · A1-4 → CONFIRMED（归因改写）／A1-16 → PARTIAL

**成立**：缺陷可达，HEAD 上用临时探针**复现三次**——Responses 门面、portable Messages
（`ContentReasoning`）、portable Messages（非法 tool 参数 JSON），全部 `outcome="success"` +
调用方 502 + `calls=1`。账目断言核实无误：`returnFailure`（`service.go:3050-3058`）只写一行日志，
`run` 在门面不可见，`run.finalize` 的 `finalized` 幂等位（`:166-168`）会吞掉补记——**没有任何
补记路径**。

**触发面被改写**：原报告点名的 `:1195` citations **不可达**——输入来自
`DecodeGenerateResult(chatResponse)`，Chat wire 没有 citation 成员
（`compatibility/openai/mapping.go:270-299` 从不写 `Citations`）；即便有，`:923` 也会先失败。
真正确定性可达的是 `ContentReasoning`（`mapping.go:291-292` → `anthropic/mapping.go:257` /
`openai/responses.go:322`）与非法 tool JSON（`anthropic/mapping.go:253`）——原报告一条都没列。
`:923` 上的 citations/PET 触发要求"Responses 族档案 + 上游未被要求即自带检索"，因为 Chat wire 在
`mapping.go:42-43` 拒掉非 function 工具，请求永远没要过 web_search；属上游条件性，非罕见即达。

**"本轮新增"被推翻一半（v030 双侧对照）**：Responses 渲染在 v0.3.0 **就已在 facade、就已在计费
之后**（v030 `service.go:1065-1068`）。本轮变的不是渲染位置，而是 `renderResponseOutput` 新增了
default 拒绝分支——v0.3.0（`responses.go:258-280`）无 default，**静默丢弃 reasoning 并 200 返回**。
按 B4，本轮把一次静默丢弃换成了一次响亮失败，方向是对的。Messages 层两版逐字相同、实测行为
相同，**纯存量**；"这条链上叠两次"不成立。只有 Chat（`:923`）确实移出了循环，但 v0.3.0 整棵树
没有 `Citations`/`ContentProviderToolCall`，那个渲染点当时无从失败。

**严重度**：原报告的"金额正确、不退款、无无界重试"三点成立（ambiguous 使 `retryable` 返回 false，
settlement 按真实用量）。**漏了最贵的一项**：推理链上没有幂等键（`internal/idempotency` 只服务
Admin），502 会被 OpenAI/Anthropic SDK 默认自动重试，而失败是确定性的——于是按 SDK 重试上限
倍数产生真实费用。仍不阻塞，但比原报告描述的贵。

**修复优先级与原报告相反**：Responses(`:1108`) > Messages(`:1195`) > Chat(`:923`)。建议给
`generate` 传入 `render func(semantic.GenerateResult) error`，在 `settlementForResult` **之前**于
循环内调用，失败折成 ambiguous malformed provider error（即 v0.3.0 `service.go:983-987` 的形状）
——一处覆盖三条链，且不破坏本轮 Responses 直连的设计。当前套件对这三个渲染点的 outcome
**没有任何断言**。

## V4 · A5-1 → CONFIRMED（复现路径原报告算错）

**成立**：`spendDetectionProbe` 全仓唯一调用点 `admin_model_capability_detections.go:654` 传的是
全量 `plan.Probes`；识别阶段确只跑根探针（`:642-645` 跳过有依赖的，`:667-669` 一中即停），
每候选 1–2 条对分母 9。算式实测复现：TotalTimeout 90s（`config/default.go:52`）、
AttemptResponseHeaderTimeout 60s（`:72`）→ `min(90/9, 60) = 10s`，真实识别路径实测 **9.99s**；
v0.3.0 是 8 条 → 11.25s。60s 上限完全不起作用。**无任何防线**：无 `max(runnable,1)`、无重试、
`MaxProviderCalls=10` 管不到。

**"10s 够不够"不是评审自造**：探针请求极小、`MaxOutputTokens: 16`、根探针恒为 `minimal_chat`
且不设 reasoning_effort（`capability_detection.go:132,229-243,320-334`），对普通模型绰绰有余。
但 commit `3503658` 为做主循环那次修复而记录的**真实观测**就是"同一条 16-token 上限的非流式
探针，12.9s 不够"。同一条探针，单候选走主循环拿 60s，多候选走识别拿 10s，**差 6 倍**。

**原报告算错的一步**：`openai.responses.v1` 分母是 6 不是 9（15s），所以它给的"OpenAI 两候选都
超时"不必然成立。**但它漏了更强的路径**：`bedrock.mantle.chat.v1` 与
`bedrock.mantle.openai.chat.v1` 各 9 条/1 根，**两边都精确 10s**——而这正是 `providers.go:723-729`
明说的排布，也正是 `3503658` 观测到超时的同一个 provider。

**后果比原报告更刺眼**：超时归 `ProbeUnavailable` + `error_class="timeout"` → `DetectionFailed`，
不丢能力、不动账目（这点原报告对）。但控制台 `DeploymentsPage.tsx:2229-2250` **不渲染
`error_class`**，`unavailable` 译作"暂时不可用"——一个确定性的、Halro 自己造成的截断被呈现为
上游抖动，运维只会反复重试反复扣费。次生：两候选预算不等时（10s vs 15s）可能静默选中慢的那个
赢不到的接口，而不是报 ambiguous 交还运维。

**最小修复**：`:762-767` 分母换成主循环 `:572-578` 同款 runnable 计数（识别阶段 `d.Results` 为空，
runnable 恒等于根探针数）。修后 Mantle 得 60s、OpenAI chat 得 45s。附带：抽成共用帮手（现两份
重复）、控制台补渲染 `error_class`。

## V5 · A3-4 → REFUTED

**拦截防御（行号）**：`admin_deployments.go:1064` 的 `Clamp(entry.Capabilities,
binding.Capabilities)`（变体路径同形，`admin_invocation_targets.go:579`）。左值 `binding.Capabilities`
新建时来自 `provider_table.go:151/158` 的 **Defaults**（不含 PET），且
`provider_connection.go:112-126` 的 `AssignConnectionCapabilities` 只在 `requested` 已开位上分配、
**从不加宽**。

**实测**：构造一条声明 PET 的 Anthropic Messages 目录条目（`Entry.Validate()` 通过，证明目录侧
确实允许声明），跑 `resolveDeploymentTargetWithCatalog(..., deploymentInput{}, ..., nil, cat)`——
binding 在 Defaults 时 `ProviderExecutedTools=false`；binding 显式开了 PET 时才为 true。
**目录声明打不开一个没在连接层勾过的部署。** 原发现的核心前提"默认关闭靠的是目录恰好没人
声明"不成立——靠的是 Defaults 列 + 这道 Clamp，是结构性默认关闭。

**顺带纠正原报告两处事实**：内置目录独立遍历 143 条，**0 条声明 PET**，连构造 helper 都没有。
"签名远端目录 1.1.0 才启用"不准确——`config.go:291-301` + `default.yaml:218-219` 今天就有
`model_catalog.enabled`；但真实 gate 更硬：`trust.go:20` 的 `ReleaseTrustRoots` 只能由 ldflags
注入，`Makefile:24` 不注入、`Dockerfile:19` ARG 默认空，且
`tools/modelcatalog/test_workflow_contract.py:44-47` 断言 release workflow 里不出现该变量。
实测 `ProductionTrustRoots()` 返回 0 个根 → `snapshot.go:263-289` 拒绝一切签名目录。

**残留（降为建议）**：裸 API 省略 `capabilities` 时，默认值来自"目录 ∩ binding"。控制台流程走不到
这条路径（`DeploymentsPage.tsx:1209/2103` 保存时总是显式带 `capabilities`），数据面还需客户端
主动请求 provider tool（`gateway/service.go:2542`）。收紧的最小形状：只对
`CapabilityOptInWarnings()` 里的名字要求显式出现（缺失即关闭），落点 `admin_deployments.go:1077-1085`
与 `:947`，约 3 行，不动 `Key.Ceiling()` 的表达能力。

## 两条方法学观察

**一、五条里有三条的"归因"或"复现路径"被证伪，而结论只有一条被推翻。** 发现型角色擅长指出
"这段代码有问题"，不擅长回答"这条路径在完整系统里真能走到吗"、"它是本轮引入的吗"。V1 与 V3
各自推翻了一半归因（"回归"实为覆盖模型收窄；"本轮新增"实为存量 + 静默丢弃改响亮失败），
V4 推翻了复现路径却找到了更强的一条。这正是这一环存在的理由。

**二、三个互不知情的角色（A1、A2、A4）从三个方向指向同一处结构**：渲染或校验失败发生在账目
提交之后。#231 修的是这个模式的一个实例（span 半边），模式本身还在。V2 与 V3 分别从策略侧与
内容种类侧复现了它，且两者给出的最小修复形状一致——**在 finalize 之前补一次出站语义校验**。
这是本轮最有杠杆的一处整改。
