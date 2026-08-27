# A4 · 测试盲区（阶段 1）

> 范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`，测试侧 diff 约 +3358/−105（45 个文件，含非测试文件）。
> 本文只判「新增测试守住了什么、没守住什么」，不重复跑全量套件。分级：【肯定 / 建议 / 问题 / 疑似BUG】。
> 判定基准编号沿用 `review-plan.md` §3（B1–B10）。

## 0. 反向验证的实际结果（方案点名要复核的一条）

#231 提交信息声称：「Reverse-verified: removing the branch makes the new detection test report the
probe addressing /v1/chat/completions.」

**实测执行（非推断）**：

1. 基线：`go test ./internal/provider/openai/ -run TestCapabilityDetectionOnTheResponsesProfileProbesTheResponsesEndpoint -count=1 -v` → PASS。
2. 手工回退分支：把 `internal/provider/openai/adapter.go:348` 的 `if a.responses {` 改为
   `if false && a.responses {`（即让 Chat 不再走 `chatViaResponses`）。
   **编辑已确认落地**（`grep -n "if false && a.responses" internal/provider/openai/adapter.go` → `348:`，
   符合 CLAUDE.md「反向验证不失败不算证据 / 断言编辑真的应用了」）。
3. 缺陷态运行 `-count=1`（无缓存）→ **FAIL**，失败消息为
   `responses_detection_test.go:53: the probe addressed [https://api.openai.com/v1/chat/completions]`。
4. `git checkout -- internal/provider/openai/adapter.go` 恢复；`git diff --stat` 空；
   `-count=1` 重跑 → ok。

**结论：该测试真的失败过，失败文案与提交信息逐字相符。#231 的反向验证声明属实。**
（附注：恢复后 `git status --porcelain` 显示 `?? .claude/`、`?? docs/review/260827/`、
`?? internal/store/bolt/zz_a7_live_migration_test.go` 三个未跟踪项。前两个是评审自身产物，
第三个是他人角色（A7）的实测脚手架，非本角色所写、亦非本次反向验证残留——本角色只改过
`adapter.go` 一处且已还原。）

---

## 1. 迁移 32 的存量数据方向

### A4-1 · 迁移 32 测的是「构造 fixture」而非「v0.3.0 真实写出的库」
- `internal/store/bolt/migration32_test.go:34`（用例）、`:158`（`writeV31Records`）、`:239`（`spellJSONModeBackwards`）
- 基准：CLAUDE.md「Verify, never assume ——用自己脑子里的模型构造的 fixture 测的是模型不是世界」
- 事实：fixture 用 **今天的** `domain.ProviderInstance`/`domain.Deployment` 结构体序列化，再把
  `json_object`/`structured_outputs` 两键「倒着拼写」回 `json_mode`，最后把 `meta.schema_version`
  改写为 31（`:204-206`）。它不是 v0.3.0 二进制真实写出的记录：v0.3.0 结构体在此期间还有其他字段
  演进（如 provider bindings、snapshot 子字段），fixture 里出现的是 HEAD 字段集合 + 一个被人工
  倒推的能力键。测试文件头注释（`:15-21`）对这一点是坦白的（"cannot be written with today's structs…
  so it is written with them and then spelled backwards"），因此这是**已知的方法学限制而非隐瞒**。
- 分级：【建议】。真实方向的验证是方案 R2/R3 的职责（用 v0.3.0 二进制建库→HEAD 启动），本条不
  要求单元测试补齐，但**要求 R2/R3 不得以本测试通过为由跳过**。
- 阻塞发布：否（但 R2/R3 是发布强制项，见 review-plan.md §7.1）。

### A4-2 · fixture 覆盖桶清单：六类覆盖五类，探测三桶是「空覆盖」
- `internal/store/bolt/migration32_test.go:38-136`
- 基准：B6（durable 格式改动必须使陈旧状态被拒而非误读）
- 逐桶核对（对照 range-map 表 1.2 与迁移体 `internal/store/bolt/store.go:839-898`）：

| 桶 / 字段 | 迁移体行 | fixture 是否写入 | 是否断言 |
|---|---|---|---|
| `deployments.capabilities` | store.go:844、918-921 | 是（`:71`） | 是（`:108`） |
| `deployments.capability_evidence` | store.go:847、945-948 | 是（`:72`） | 是（`:108`、`:143-150`） |
| `deployments.operator_disabled` | store.go:861、964-992 | 是（`:78` = `["json_mode","tools"]`） | **是**（`:116-118`） |
| `deployments.model_capability_snapshot.{capabilities,evidence}` | store.go:864-883 | 是（`:73-77`，且 snapshot 比 deployment 多勾 Tools） | 是（`:110-112`） |
| `providers.{capabilities,capability_evidence}` | store.go:849-854 | 是（`:53-55`） | 是（`:97`） |
| `providers.bindings[].{…}` | store.go:853 | 是（**一个** binding，`:56-65`） | 是（`:98-102`） |
| 探测三桶（detections / idem / index） | store.go:887-896 | **否——从未写入任何记录** | 「迁移后为 0 条」（`:129-135`） |
- 分级：【问题】。前五类是真覆盖；**探测三桶是空覆盖**：fixture 从头到尾没有向
  `bucketModelCapabilityDetections` / `bucketCapabilityDetectionIdem` /
  `bucketCapabilityDetectionIndex` 写过一条记录，因此 `:133` 的 `len(detections) != 0` 在
  「迁移把桶删了重建」与「桶本来就是空的」两种世界里同样通过。把 store.go:887-896 整段删掉，
  该断言依然 PASS。
  缓解：范围外的既有测试
  `internal/store/bolt/model_capability_detection_test.go:184-217` 用「先塞 stale 键、再回退
  schema 到 24」的方式实测过同一段三桶清空机制（迁移 25/26），机制本身有回归守护；缺的是
  **迁移 32 这一号**在有存量探测记录时的行为。
- 阻塞发布：否（机制已由 25/26 的同形测试间接守住；但这是 A4 认定的第一严重盲区，建议在
  migration32 fixture 里补三条 stale 键）。

### A4-3 · `operator_disabled` 的 json_mode 记录「迁移后不留痕」——有测试断言，且断言的是「不留痕」
- 断言：`internal/store/bolt/migration32_test.go:116-118`（`OperatorDisabled` 必须恰为 `["tools"]`）
- 实现：`internal/store/bolt/store.go:861` 调 `dropDisabledCapability(record,"operator_disabled","json_mode")`，
  该函数（store.go:964-992）**只删不加**——不写入任何后继名
- 基准：B2（fail-closed 的可行动性一面）、H1
- 事实：测试确实钉住了「`json_mode` 从列表消失」且「`tools` 不受影响」，也钉住了删空时整个字段被
  `delete`（store.go:982-984）。所以「有无测试断言」的答案是 **有**。
- 但**语义上的盲区在别处**：迁移把该部署的两半能力关掉，却**不把两个后继名写进
  `operator_disabled`**。后果（range-map §1.3 已抄下）是控制台把它渲染成
  `AvailableForReview`（"可以补勾"）而不是 `switchedOff`（"被管理员关掉了"），与「操作者从未勾过」
  的部署呈现完全相同。**没有任何测试断言这个呈现差异**——`web/` 侧与 `internal/app/capability_drift*`
  都没有「迁移 32 造成的两半皆关」这一输入的用例。
- 分级：【问题】（信息可见性，不是正确性）。这是 H1 的实证：运维只有在请求失败时才知道。
- 阻塞发布：否，但**必须写进 CHANGELOG / 发布说明**（review-plan.md §9 已列该条）。

### A4-4 · 迁移 32 无「失败点注入 / 原子性」测试
- `internal/store/bolt/store.go:839-898` 的 `migrationStep(step, "before_…")` / `"after_…"` 钩子
  在 `migration32_test.go` 中**零引用**
- 基准：B7（bbolt 是派生物，但一次半途而废的迁移会留下自相矛盾的记录）、B2
- 事实：迁移体自身提供了两个注入点，范围内新增测试一个都没用。既有同类测试
  `internal/store/bolt/store_test.go:234`（`TestProviderProfileMigrationFromV3IsAtomicAndConservative`）
  证明这类测试在本仓是有先例、可写的。
- 分级：【建议】。bbolt 的 `Update` 事务本身给了原子性，风险等级低于前两条。
- 阻塞发布：否（方案 R2 的「失败点注入」是 A7 的实测项）。

### A4-5 · 迁移历史断言是范围内唯一「版本号被占用一次」的守护
- `internal/store/bolt/store_test.go:228`（`history[31] != MigrationRecord{32, "structured_output_capability_split"}`）
- 基准：B6（迁移名/号不得复用）
- 分级：【肯定】。链完整性（store.go:1617-1620）+ 历史逐条比对，这一面守住了。

---

## 2. #231 声称的四处修复 · 逐条回归覆盖

### A4-6 · 探测打错端点：**有**正面测试 + **已实证的负面能力**
- `internal/provider/openai/responses_detection_test.go:23`（探针必须打 `/v1/responses`，
  且 `:52-54` 用 `seen` 数组逐个 URL 断言）、`:63`（Responses 档案的 `ChatStream` 必须直接拒绝）
- 基准：B10（探测必须问它将来会走的那张面）
- 分级：【肯定】。这是范围内**唯一一条经我实际反向验证、确认能在缺陷态失败的测试**（见 §0）。
  它走的是真的 `provider.NewLegacyAdapterBridge`（`:40`）而不是直接调 adapter，因此把
  「wrapper 不透传具体方法」这条也一并盖住了。
- 阻塞发布：否。

### A4-7 · B8 类顺序缺陷（已计费成功调用以 502 抵达）：**有**回归，但钉在 redaction 层而非端到端
- `internal/redaction/provider_tool_content_test.go:83`
  （`TestOutboundRedactionCollapsesACitationSpanItCanNoLongerPlace`）：
  `:93-95` 先自证「本次改写确实缩短了文本」（否则用例自废），`:98-100` 断言 span 归零，
  `:101-103` 断言来源本身不被丢，`:106-108` 断言 `processed.Validate()` 通过
- 基准：B8（拒绝要发生在能改变账目之前）
- 事实：「span 归零 → Validate 通过」这一半**钉住了**，且 `:104-105` 的注释点名了 502 场景。
  缺的是另一半：**没有端到端用例走完
  `Service.Responses` → `generate`（attempt.finish/run.finalize）→ `RenderResponseResult`，
  断言这条链在「脱敏改变了引用文本长度」的输入下返回 200 而非 502**。
  `internal/gateway/service.go:1108` 的 `RenderResponseResult` 仍在 `run.finalize`（:1046）之后，
  顺序本身没变——只是不再有能触发它的输入。也就是说：**守的是「不再产生坏输入」，不是「坏输入
  不再变成 502」**。若将来 redaction 新增一条会改变长度而 span 未归零的路径，这一层拦不住。
- 分级：【问题】。方案 S6 正是这条的实测剧本，本条应移交阶段 3 执行。
- 阻塞发布：否（当前无已知可达路径），但 S6 必须真跑。

### A4-8 · 「data URL 脱敏改变路由需求」守卫：**有**端到端测试
- `internal/gateway/service_test.go:972`（`TestRedactionThatChangesWhatARequestRequiresIsRefused`）
- 守卫实现：`internal/gateway/service.go:971`（一元）/ `:1925`（流式），
  `redactionPreservedRequirements` 实现于 `:2977`
- 基准：B4（能力过滤在 Provider I/O 之前）、B1（拒绝在预留之前）
- 事实：测试构造了一个**只有 Vision、没有 FetchedImage** 的 target（`:1000-1005` 注释点名了
  「能读你递来的图、不能自己去取图」这个区分），断言返回 `sensitive_data_detected` 且
  `f.adapter.calls` 未增长（`:1014-1016`），即**没到 Provider**。
- 分级：【肯定】。这是本范围内质量最高的一条新增测试：它同时钉住了错误码、拒绝位置、以及
  「守卫本身发生在预留之前」（provider calls 未增长可反推）。
- 阻塞发布：否。

### A4-9 · redaction 新遍历四条路径：覆盖完整
- `internal/redaction/provider_tool_content_test.go:49`（`ContentProviderToolCall` 的 query
  + `Citations` 的 URL/Title 都被改写，且 `:59-61` 用整体 JSON 序列化扫「密钥不得出现在结果任何位置」）、
  `:114`（clone：断言调用方原始 `Citations` 未被就地改写 —— 重试复用面）、
  `:129`（`default` 拒绝分支：未知 kind 必须报错且**错误里点名 kind**，`:142-144`）、
  `:157`（`ContentReasoning` 按普通文本脱敏）
- 基准：B2、B3
- 分级：【肯定】。`:11-15` 用 mandatory-baseline 命中的 `gw_` 形状密钥，使这四条在
  「Project 什么都没配」的最弱前提下成立——这比用自定义策略强得多。
  `:149-151` 还反向自证「拒绝时返回的是未处理结果」，防止用例哪天悄悄失去证明力。
- 阻塞发布：否。

### A4-10 · 「目录已覆盖模型的 token 上限归零」：有断言，且带自废保护
- `internal/app/admin_model_capability_detections_test.go:747-748`
  （`Recommended.MaxContextTokens/MaxOutputTokens` 必须等于 `Baseline` 的对应值）
- 基准：B4 的反面（能力/上限被静默归零 → 路由过滤失效 → 上游在预留之后拒绝）
- 事实：#231 提交信息自承「既有验证测试只断言了布尔能力、没断言这两个数，所以它一路都通过」。
  现在补上了，且 `:742-746` 的注释说明该用例会在目录 entry 不再声明上限时**主动失败**。
- 分级：【肯定】。
- 阻塞发布：否。

---

## 3. 探测预算 8→9

### A4-11 · 预算 9 的「计划侧」有测试，「时间片侧」的 H3 边界**无**测试
- 计划侧（有）：`internal/provider/capability_detection_test.go:373`
  （`TestCapabilityDetectionPlanNamesWhatTheBudgetDropped`）——构造一个serves 十项的 profile，
  断言 `len(plan.Probes) == maxDetectionProbes`（`:392`）、`Deferred` 恰为 `["reasoning"]`（`:395`）、
  且「不得既在计划里又在 deferred 里」（`:397-403`）；
  `:109` `TestLegacyProfileCapabilityDetectorHasBoundedSideEffectFreePlan` 断言不超 9、
  无持久副作用、单探针 `MaxInputBytes<=2048 / MaxOutputTokens<=16`；
  `internal/app/admin_model_capability_detections_test.go:984` 断言「预算挤掉的能力存为
  `probe_budget` 的 NotProbed 而不是 policy」。
- 新 `json_schema` 探针本身：`internal/provider/capability_detection.go:152-153` 生成，
  `:287-297` 载荷。**没有一条测试断言 `structured_outputs` 会产生 `json_schema` kind 探针、
  且其载荷是 strict schema**——`:373` 的用例只数总数与 deferred 名字，
  `:109` 的用例遍历 probe 但只查安全属性，不查 kind↔capability 的配对。
- 时间片侧（无）：分配逻辑在 `internal/app/admin_model_capability_detections.go:560-582`：
  `fairShare := time.Until(deadline) / max(runnable,1)`；
  `probeTimeout := min(fairShare, AttemptResponseHeaderTimeout)`；
  **仅当 `probeTimeout > 0` 才建子 context**（`:580`）。
  唯一相关测试是 `internal/app/admin_model_capability_detections_test.go:395`
  （`TestRootProbeIsBoundedByTheAttemptTimeoutNotAFractionOfTheBudget`），它用
  90s 总预算 / 60s attempt timeout，断言根探针 ≥45s、依赖探针 `>0 且 < root`（`:407-421`）。
  它证明的是**充裕**情形下的再分配，**不是 H3 问的边界**。
- **H3 的边界未被覆盖**：当 `TotalTimeout` 相对 9 个 runnable 探针偏小时，
  `fairShare` 可以小到不足以完成一次上游请求。此时探针在 `probeContext` 超时返回，
  `classifyCapabilityProbeError`（`internal/provider/capability_detection.go:419-420`）
  把 `context.DeadlineExceeded` 判为 `domain.ProbeUnavailable`——**不是** `ProbeUnsupported`。
  这一点使 H3 的最坏形态（「能力被静默判为不支持」）**不成立**：不足的时间片表现为
  Unavailable/inconclusive，不会落成 unsupported 证据。这是一条**已由代码结构缓解、但无测试
  钉住**的性质：没有任何测试断言「超时的探针不得被记为 unsupported」。
- 分级：【问题】（缺测试，不缺正确性）。两个具体缺口：
  (a) `structured_outputs → json_schema` 探针配对与载荷无断言；
  (b) 「探针时间片不足 → 记为 Unavailable 而非 Unsupported」无断言。
- 阻塞发布：否。

---

## 4. 流式路径（SSE）与「一元改了、流式没改」的接缝

### A4-12 · 词表拆分在流式路径上的过滤一致性：**无**直接测试（结构上共用，风险低）
- 一元：`internal/gateway/service.go:958-960`；流式：`:1906-1908`（同三道 filter、同顺序、同实现）
- 一元守卫：`:971`；流式守卫：`:1925`（同一 `redactionPreservedRequirements`）
- 一元估算：`:974`；流式估算：`:1929`（同一 `estimateGenerateInputTokens`）
- 测试侧：`internal/gateway/service_test.go:1213-1289` 的两个 filter 用例
  （`TestChatCapabilityFilterSelectsOnlyCompatibleFallback`、
  `TestSemanticCapabilityFilterRequiresTheCapabilityItFiltersOn`）**直接调用
  `filterSemanticCapabilities` 这个纯函数**，因此它们对两条路径同等有效——但也因此
  **不证明流式链真的调用了它**。
- 基准：B4
- 事实：`{JSONObject:true}` 与 `{StructuredOutputs:true}` 两个 requirement 在 `:1289` 的
  表驱动里各占一行，两半确实被当成两件事测了。缺的是「同一能力在 unary 与 stream 两条路径上
  过滤一致」的**端到端**用例——现有流式用例
  （`:1546 TestChatStreamUsesPublicModelAndSettlesUsage`、`:1625` / `:1663` 的回退边界）
  没有一个带 `response_format`。
- 分级：【建议】。共用同一实现，回归风险主要来自将来有人在一条链上加过滤而漏另一条。
- 阻塞发布：否。

### A4-13 · Responses **流式**路径的新内容种类（web_search_call / citations）无测试
- 实现：`internal/gateway/service.go:1115-1166`（`ResponsesStream`：
  `generateStream` 的 chunk → `openaiwire.DecodeEvent` → `NewResponseStreamRenderer.Accept` → emit）
- 基准：B4（承载不了就该拒绝，而不是抹掉来源返回答案）、B2
- 事实：`internal/compatibility/openai/provider_responses_test.go:54-58`、`:92-95` 覆盖的是
  **一元** 的 `ContentProviderToolCall`/`Citations` 解码与渲染；
  `internal/provider/openai/responses_profile_test.go:60` 覆盖的也是一元
  （`GenerateSemantic`，`:93-95` 断言 `store=false`、`:120-126` 断言 tool call 与 citation 回来）。
  **流式方向**：`openai.responses.v1` 档案**不绑 stream primitive**
  （`internal/provider/profile.go:400` 只有 `OperationChat`），且
  `responses_detection_test.go:63` 已断言该档案的 `ChatStream` 直接拒绝。
  因此 Responses 流式**只能落在 chat 档案上**，而 chat 档案的上限里没有
  `ProviderExecutedTools`（`internal/domain/provider_table.go:140`）——`web_search` 在流式方向
  **结构上不可达**。
- 分级：【肯定】+【建议】。不可达性是设计成立的（多道并行守护），但**没有一条测试把这条
  不可达性写下来**：没有用例断言「一个带 `web_search` 的 `stream:true` Responses 请求被拒绝
  且不到 Provider」。这属于「靠三处独立事实的巧合维持的不变量」，任一处放松即失效。
- 阻塞发布：否。

### A4-14 · 流式脱敏对新内容种类的行为无测试
- `internal/gateway/service.go:1996-2004`（流式脱敏走 `streamRedactor.Process(chunk)`，
  对象是**已渲染成 Chat wire 的 chunk**，不是 semantic Content）
- 基准：B2、B3
- 事实：`ProcessOutboundGenerateResult` 的 `default` 拒绝分支（A4-9 守住的那条）**只在一元路径上**。
  流式路径的脱敏是另一套（`streamRedactor`，按 chunk 文本流），新内容种类在这条路上
  **不经过那个 default**。当前不可达（A4-13），但「一元有 fail-closed default、流式没有对应物」
  这一不对称**无测试记录、也无注释记录**。
- 分级：【问题】（不对称本身未被写下）。
- 阻塞发布：否。

---

## 5. 盲区汇总表（不变量 × 有无测试守护）

| # | 不变量 / 行为 | 测试守护 | 位置 | 其他机制覆盖 | 分级 |
|---|---|---|---|---|---|
| 1 | 迁移 32：deployments 两半皆关 + 证据 unsupported | **有** | migration32_test.go:108 | 记录读取 Validate 双条件（provider_profile.go:310-317） | 肯定 |
| 2 | 迁移 32：providers + **每个 binding** 同步拆分 | **有**（单 binding） | migration32_test.go:98-102 | 同上 | 肯定 |
| 3 | 迁移 32：snapshot 两字段同步拆分 | **有** | migration32_test.go:110-112 | 同上 | 肯定 |
| 4 | 迁移 32：`operator_disabled` 移除 json_mode、不留痕 | **有** | migration32_test.go:116-118 | — | 肯定 |
| 5 | 迁移 32：探测三桶清空 | **空覆盖**（fixture 无记录） | migration32_test.go:129-135 | 迁移 25/26 同形机制已被 model_capability_detection_test.go:184-217 实测 | **问题（A4-2）** |
| 6 | 迁移 32：存量库来自 v0.3.0 真实二进制 | **无**（倒推 fixture） | migration32_test.go:158 | 方案 R2/R3 实测 | 建议（A4-1） |
| 7 | 迁移 32：失败点注入 / 原子性 | **无** | store.go:840/897 钩子未被引用 | bbolt Update 事务 | 建议（A4-4） |
| 8 | 迁移号/名不复用 | **有** | store_test.go:228 | 迁移链完整性 store.go:1617-1620 | 肯定 |
| 9 | 「两半皆关」对运维可见 | **无** | — | 仅失败时的 400（service.go:961） | 问题（A4-3，写发布说明） |
| 10 | 探测打在档案自己的表面（B10） | **有，且经实测反向验证** | responses_detection_test.go:23 | wire 层 `GenerateSemantic` 非 responses 即拒（adapter.go:421-424） | 肯定（A4-6） |
| 11 | Responses 档案不得流式 | **有** | responses_detection_test.go:63 | profile.go:400 不绑 stream primitive | 肯定 |
| 12 | span 归零 → Validate 通过（B8 前半） | **有** | provider_tool_content_test.go:83 | — | 肯定 |
| 13 | span 归零 → 端到端 200 而非 502（B8 后半） | **无** | — | 方案 S6 实测 | **问题（A4-7）** |
| 14 | data URL 脱敏改变需求 → 预留前拒绝 | **有（端到端）** | service_test.go:972 | 守卫在一元与流式两处都调（service.go:971/1925） | 肯定（A4-8） |
| 15 | ProviderToolCall / Citations 出站脱敏 | **有** | provider_tool_content_test.go:49 | — | 肯定 |
| 16 | 未知 content kind → 拒绝（一元） | **有** | provider_tool_content_test.go:129 | — | 肯定 |
| 17 | 未知 content kind → 拒绝（**流式**） | **无对应机制、也无测试** | — | 当前结构上不可达（A4-13） | 问题（A4-14） |
| 18 | citations clone（重试复用面） | **有** | provider_tool_content_test.go:114 | — | 肯定 |
| 19 | ContentReasoning 被脱敏 | **有** | provider_tool_content_test.go:157 | — | 肯定 |
| 20 | 预算 9 的计划上限与 Deferred 记名 | **有** | capability_detection_test.go:373、:109 | app 侧 NotProbed=probe_budget（admin_…_test.go:984） | 肯定 |
| 21 | `structured_outputs` → `json_schema` 探针配对与 strict 载荷 | **无** | capability_detection.go:152/287 无对应断言 | 契约版本 v5 使旧结果不被复用 | 问题（A4-11a） |
| 22 | 时间片不足 → Unavailable 而非 Unsupported（H3） | **无** | — | classifyCapabilityProbeError:419-420 结构上保证 | 问题（A4-11b） |
| 23 | 根探针不被均分饿死 | **有** | admin_…_test.go:395 | — | 肯定 |
| 24 | 两半在路由过滤上是两件事 | **有（纯函数级）** | service_test.go:1289 | wire 层双重校验（openaiapi/types.go:118、responses.go:152-165） | 肯定 |
| 25 | 一元 / 流式过滤一致性（接缝） | **无端到端** | — | 两链共用同一 filter 实现与顺序 | 建议（A4-12） |
| 26 | web_search 在流式方向不可达 | **无**（三处事实的巧合） | — | 档案无 stream primitive + chat 档案上限无该能力 + ChatStream 直拒 | 建议（A4-13） |
| 27 | 目录已覆盖模型的 token 上限不归零 | **有** | admin_…_test.go:747 | — | 肯定（A4-10） |
| 28 | 端点清单与实现一致（含两种 output kind） | **有** | manifest_derivable_coverage_test.go:145 | — | 肯定 |

统计：**28 条不变量中 20 条有测试守护（含 1 条经实测反向验证），8 条为盲区**——其中
【问题】5 条（#5、#9、#13、#17、#21/#22），【建议】3 条（#6、#7、#25/#26）。
无【疑似BUG】。**无一条阻塞发布**：5 条【问题】中，#5 与 #21/#22 有范围外机制或代码结构缓解，
#13 与 #17 当前无已知可达路径，#9 是可见性问题、由发布说明承接。

## 6. 移交阶段 3 的具体动作

- **S6 必须真跑**（对应 A4-7 / 表 #13）：这是 B8 唯一没有端到端守护的一半。
- **R2/R3 必须真跑**（对应 A4-1 / 表 #6）：migration32_test.go 的 fixture 是倒推的，
  它通过不能替代「v0.3.0 二进制写出的库被 HEAD 干净加载」。
- **R3 顺带核对探测三桶**（对应 A4-2 / 表 #5）：R2 建库时**务必先跑一次探测留下记录**，
  否则 R3 会和单元测试犯同一个错——在空桶上验证「桶被清空了」。
