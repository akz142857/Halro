# A5 · BUG 排查（阶段 1，独立评审）

> 范围 `v0.3.0(abfc05c)..HEAD(8bb4847)` 全部生产 Go 代码（`internal/webui/dist`、纯测试文件除外）。
> 行号以 2026-08-27 工作区 HEAD 为准；标注「v0.3.0」的以 `git show abfc05c:<path>` 为准。
> 基准编号引用 `review-plan.md` §3 的 B1–B10。
> 已执行：`go vet` 六包全过；`go test -race ./internal/provider/ ./internal/redaction/ ./internal/semantic/ -count=1` 全过；
> `go test -race ./internal/app/ -run 'Detection|Probe|Deployment' -count=1` 全过（63.5s）。

计数：12 条。疑似BUG 1、建议 2、疑问 3、肯定 6。**阻塞发布：0**。

---

## A5-1 · 识别阶段的探针时间片按全部探针均分，预算 8→9 使之进一步收窄

- `internal/app/admin_model_capability_detections.go:762-767`
- 基准：**B4 的邻域**（不是静默丢能力，而是把可用模型报成不可用）；靶子 H3
- 分级：**【疑似BUG】**
- 阻塞发布：**否**

**缺陷本体。** `spendDetectionProbe` 是识别阶段（`identifyCapabilityBinding`）唯一的花钱入口，其时间片是

```go
fairShare := time.Until(deadline) / time.Duration(len(remaining))   // :763
```

`remaining` 由调用点 `:654` 传入 `plan.Probes`，即**整个计划的全部探针**。但识别阶段只跑
无依赖的根探针（`:643-645` `if len(probe.DependsOn) > 0 { continue }`）——OpenAI chat 档案的
根探针是 `chat` 与 `embeddings` 两个，`openai.responses.v1` 只有 `chat` 一个。于是一个必然
要跑的根探针只拿到总预算的 1/9。

**这正是主循环已经修掉的那个缺陷。** 主循环 `:572-578` 只把剩余时间分给**依赖已满足、真正会
跑**的探针：

```go
runnable := 0
for _, pending := range plan.Probes[probeIndex:] {
    if probeDependenciesSupported(d.Results, pending.DependsOn) { runnable++ }
}
fairShare := time.Until(deadline) / time.Duration(max(runnable, 1))
```

并且 `:560-571` 的注释逐字写明了理由：「dividing evenly gave the one probe that always runs …
the smallest share: a seventh of the budget on a plan of seven. A frontier reasoning model
cannot answer a non-streaming completion in that, so detection reported a timeout for a model
that works.」**同一段话原样适用于 `spendDetectionProbe`，而那里没有改。**

**本范围的关联。** `len(remaining)` 这个分母在 v0.3.0 已是如此（v0.3.0 同文件 `:683`），缺陷本体
是既有的；但本范围把 `maxDetectionProbes` 从字面量 8 提到常量 9
（`internal/provider/capability_detection.go:27`），于是识别阶段每个根探针的时间片
**从 total/8 缩到 total/9**。默认 `TotalTimeout = 90s`（`internal/config/default.go:52`）：
11.25s → **10s**，而同一路径的上限 `AttemptResponseHeaderTimeout` 默认是 60s
（`default.go:72`），即 `min()` 完全由 fairShare 决定，60s 的上限根本不起作用。

**可复现路径（完整可达）。**

1. 一个 OpenAI 连接同时启用两个 binding：`openai.chat-embeddings.v1` 与 `openai.responses.v1`
   （二者共享 type/surface/scheme，`provider_table.go:142-152` 明说这是有意支持的排布）。
2. 管理员对某前沿推理模型发 `POST /admin/providers/{id}/model-capability-detections`，
   不带 `binding_id`/`profile_id`。
3. `capabilityDetectionCandidates`（`:319-359`）为两个 binding 各建一个 probeCandidate；
   目录未覆盖该模型 → `catalogCandidates` 为空 → `resolved = probeCandidates`（2 个）。
4. `runCapabilityDetection:479` → `identifyCapabilityBinding:626`。因 `len(d.Candidates) == 2`，
   `:648` 的单候选短路不生效，`:654` 对每个候选的根探针调用 `spendDetectionProbe`。
5. `:762-767` 计算 `probeTimeout = min(90s/9, 60s) = 10s`，探针在 10s 后被
   `context.DeadlineExceeded` 打断。
6. `classifyCapabilityProbeError`（`internal/provider/capability_detection.go:419-421`）
   → `ProbeUnavailable` → `candidate.Answered = false`（`:667`）。
7. 两个候选都如此 → `answered` 为空 → `:692` `finishDetectionWithoutProbe(d, DetectionFailed)`。

结果：**一个工作正常的模型被判 detection failed，并且已经花掉了两次计费探针调用。**

**为什么不是阻塞类。** 超时归 `ProbeUnavailable` 而非 `ProbeUnsupported`，而
`RecommendedFromProbes`（`internal/domain/model_capability_detection.go:376-383`）对
unavailable 走 `default` 分支保留 baseline claim，不会把能力删掉。所以这是「浪费计费调用 +
错误结论」，不是 B4 的静默丢能力，也不改变账目。修法是一行：把分母换成与主循环同样的
runnable 计数（识别阶段的 runnable 恒等于「无依赖的根探针数」）。

---

## A5-2 · 主循环的 9 探针预算分配没有把探针饿到静默 unsupported（H3 的另一半，证伪）

- `internal/provider/capability_detection.go:27`、`:122-184`；
  `internal/app/admin_model_capability_detections.go:558-583`、`:533-537`
- 基准：B4
- 分级：**【肯定】**
- 阻塞发布：否

H3 问的是「9 个探针在 attempt timeout 下的实际分配，边界上是否有探针拿到不足以完成一次请求
的时间片——那会表现为能力被静默判为不支持」。逐条查完，**这一半不成立**，理由三条，每条一个
行号：

1. **分母不是 9，是 runnable。** `:572-577` 只数依赖已满足的探针。计划里 `chat` 排第一且无
   依赖，其余 chat 系全部依赖 `chat`——第一轮 runnable = 1（`chat` 自己）+ 无依赖的
   `embeddings`/`moderations`/`rerank`，所以 `chat` 拿到的是接近全部剩余的时间，不是 1/9。
2. **超时不会变成 unsupported。** 时间片耗尽 → `context.DeadlineExceeded` →
   `classifyCapabilityProbeError:419-421` → `ProbeUnavailable`；
   `RecommendedFromProbes:376-383` 把 unavailable 归入 `default`，保留 baseline 的声明。
   只有 `ProbeSupported`/`ProbeUnsupported` 两种「回答」才改写声明——这正是该函数
   `:356-364` 注释所承诺的语义，代码与注释一致。
3. **下限 1 不会退化成 0 或负。** `max(runnable, 1)`（`:578`）保证分母 ≥1；
   `if probeTimeout > 0`（`:579-582`）保证总预算已耗尽时不建子 context——此时父 ctx 本身已过期，
   探针立即以 DeadlineExceeded 失败，仍是 unavailable，不是 fail-open。

另外核对了 8→9 的取值本身：`maxDetectionProbes = 9` 与 `CapabilityDetectionPlan` 里 11 个
`add()` 调用点（`:137-179`）——一个档案最多能提出 11 个探针，超出的 2 个进 `Deferred`
（`:130-133`），并在 `:496-501` 落成 `probe_budget` 记录。`reasoning` 被刻意放在最后
（`:170-174` 注释说明理由），是溢出时第一个被让出的。这条设计是自洽的。

---

## A5-3 · `unwrapSemanticGenerator` 缺自环守卫，三处 wrapper 解包实现互不一致

- `internal/provider/primitive.go:388-399`
- 对照：`internal/app/admin_model_capability_detections.go:370-386`（`capabilityDetectorFor`）、
  `internal/app/admin_invocation_targets.go:900-918`（`unwrapOptional`）
- 基准：无直接对应（健壮性）
- 分级：**【建议】**
- 阻塞发布：否

范围内新增的 `unwrapSemanticGenerator` 是第三份 wrapper 解包实现，也是唯一一份没有终止守卫的：

```go
func unwrapSemanticGenerator(adapter Adapter) (SemanticGenerator, bool) {
	for {
		if generator, ok := adapter.(SemanticGenerator); ok { return generator, true }
		unwrapper, ok := adapter.(interface{ UnwrapAdapter() Adapter })
		if !ok { return nil, false }
		adapter = unwrapper.UnwrapAdapter()   // :397 —— 无 next == adapter 判断
	}
}
```

另外两处都写了 `next := wrapper.UnwrapAdapter(); if next == adapter { return zero, false }`
以及 `for adapter != nil` 的循环条件。这里两者皆无。

- **nil 是安全的**：`UnwrapAdapter()` 返回 nil 接口时两次类型断言都 false，函数返回 `false`。
- **自环是死循环**：若某个 wrapper 的 `UnwrapAdapter()` 返回自身，本函数在**热路径**
  （`profile.go:126`，`operationSet.Resolve` 每个请求都走）无限自旋，请求永不返回。

**可复现路径：不完整。** 当前唯一实现 `AdapterUnwrapper` 的类型是 `LegacyAdapterBridge`
（`profile.go:189`），它返回被包装的 `b.Adapter`，不可能是自身（`NewLegacyAdapterBridge:168-170`
拒绝 nil adapter，且 bridge 与 adapter 是不同类型）。所以**当前不可达**，按任务规则降级为建议。
理由仍然成立：#231 提交信息自陈「Getting that wrong resolves nothing, drops the target from
every route」，而该次修复只加了测试（`provider_test.go` 钉住 Responses 档案能解到 primitive），
没有把守卫补齐——三份实现里两份有守卫、一份没有，正是那条测试**没有钉住的邻居**。

---

## A5-4 · 目录 clamp 与 binding 上限的取值优先级：#231 的修复覆盖了唯一暴露路径（证伪）

- `internal/app/admin_model_capability_detections.go:207-214`、`:721`、`:849-856`；
  `internal/modelcatalog/catalog.go:439-451`；`internal/domain/model_capability_detection.go:339-352`
- 基准：B4
- 分级：**【肯定】**
- 阻塞发布：否

靶子问的是「还有哪个字段走『binding 自带值优先于目录值』而 binding 值几乎总为零」。逐条走完
取值链，**答案是没有**，依据如下：

1. **走这条路的字段只有两个。** `isLimit`（`catalog.go:707-709`）把「限额」定义为
   `max_context_tokens` 与 `max_output_tokens` 两个，`ProviderCapabilities` 里没有第三个数值
   字段。所有其余能力都是布尔，由 `RecommendedFromProbes` 的探针结果 / baseline 三态逻辑决定
   （`model_capability_detection.go:374-387`），不经过 `:721` 那条 binding 直取。
2. **`Baseline == nil` 的场景是「目录本来就没有值」。** `Baseline` 只在
   `len(catalogCandidates) == 1 && verify`（`:207`）时设置。穷举另外三种：
   - `catalogCandidates == 1 && !ForceRefresh` → `catalogKnown = true`（`:122`）→ `:222-224`
     直接 completed，`Recommended = modelcatalog.Clamp(entry, binding)`，两个限额走 Clamp 的
     `narrowerLimit`，正确；
   - `catalogCandidates > 1` → `capabilityCandidateError:409` 返回 ambiguous，请求被拒；
   - `catalogCandidates == 0` → 目录不覆盖该模型，没有目录值可优先，限额取 binding 自带值
     （通常是 0 = 未声明）是正确语义。

   即：**「目录有值而被 binding 的零值盖掉」只在第一种情况可能发生，而那正是 #231 修的那条**
   （`:849-856`）。
3. **验证器与写入路径读同一函数。** `validateDetectionRecommendation:340-341` 重算
   `RecommendedFromProbes` 后把两个限额直接采信 `d.Recommended`，与 `finalize:838-856` 的
   计算顺序一致，不会出现「写得进、读不出」的不对称。

---

## A5-5 · 预算耗尽在三个地方留下三种不同的探针记号

- `internal/app/admin_model_capability_detections.go:534`（第三种）
  对照 `:498`（`probe_budget`）与 `:835`（`risk_policy`）
- 基准：无直接对应（可行动性 / 一致性）
- 分级：**【建议】**
- 阻塞发布：否

同一个原因——「预算不够，没跑」——在三处写成三种 `ProbeKind`：

| 位置 | 条件 | 写入的 ProbeKind |
|---|---|---|
| `:498` | 计划阶段就超 `maxDetectionProbes`，进 `Deferred` | `"probe_budget"` |
| `:534` | 运行中 `d.ProviderCalls >= d.MaxProviderCalls` | **`probe.Kind`**（如 `"inline_image"`） |
| `:835` | 计划从未覆盖该能力 | `"risk_policy"` |

`:502-509` 的注释把这件事说得很清楚：控制台**只**认 `probe_budget` 为「操作者可以提高上限」的
那一类，`risk_policy` 会被从两个列表里过滤掉。而 `:534` 写的既不是 `probe_budget` 也不是
`risk_policy`，而是探针自己的 kind——从控制台看去与「探过了、没通过」难以区分。

**可复现路径（完整可达）。** `MaxProviderCalls` 默认 10（`internal/config/default.go:53`），
一个档案最多 9 个探针。三个候选 binding 的识别阶段各花 1 次（`:651` 允许，`:654` 支出）→
主循环开跑时 `ProviderCalls = 3`，剩 7 次；计划有 9 个探针，第 8、9 个探针命中 `:533` 的
`d.ProviderCalls >= d.MaxProviderCalls`，落成 `NotProbed` + 探针 kind。

修法与 `:498` 对齐即可：这一分支的两个原因（依赖未满足 / 预算耗尽）应分别写
`"risk_policy"` 与 `"probe_budget"`，而不是共用 `probe.Kind`。

---

## A5-6 · 探测主循环忽略一次带 ctx 的持久化错误，取消竞态下可留下永久 `running` 记录

- `internal/app/admin_model_capability_detections.go:536`
- 基准：B2（fail-closed）的邻域
- 分级：**【疑问】**（既有代码，非本范围引入）
- 阻塞发布：否

```go
d, _ = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)   // :536 —— 错误被丢弃
```

`PutModelCapabilityDetection`（`internal/store/bolt/model_capability_detection.go:93-104`）
在 `ctx.Err() != nil` 时返回**零值** detection 与错误。此处丢掉错误后 `d` 被零值覆盖
（`d.ID == ""`），下一轮循环 `:518` 用空 ID 去 `Get` → 失败 → `return`，
**绕过了 `:523` 的 `finalizeCanceledDetection`**。记录留在 `DetectionRunning`；
`maintainCapabilityDetections:1000` 只清理 terminal 状态的记录。

对比同函数内其他调用点：`:511` 检查错误并 return，`:544`/`:554`/`:607` 都赋给 `err` 并检查，
`:607` 还刻意换成 `context.Background()` 以免取消影响收尾。**只有 `:536` 既用 ctx 又丢错误。**

**降级为疑问的理由**：(1) 这一分支在 v0.3.0 已存在同形（本范围 diff 未触及该行），严格说不在
范围内；(2) 触发需要「取消请求恰好落在这一次 Put 上」的竞态，我没有构造出确定性复现；
(3) 注释 `:548-551` 提到有启动恢复把 reserved/running 当 UNKNOWN 处理，重启后是否自愈**未核实**。

---

## A5-7 · 迁移 32 的桶覆盖面完整、单事务原子、无半写窗口

- `internal/store/bolt/store.go:839-898`、`:906-992`、`:1598-1677`
- 基准：B6、B7
- 分级：**【肯定】**
- 阻塞发布：否

三个子问题逐条查完：

**漏桶：没有。** `grep 'json:"capabilities"|json:"capability_evidence"|json:"evidence"|json:"operator_disabled"'`
于 `internal/domain/*.go` 得到的持久化承载点共五处：`models.go:322-323`（Provider）、
`:349-350`（binding）、`:874-875`（Deployment）、`:729/735`（ModelCapabilitySnapshot）、
`:902`（OperatorDisabled），加上 `model_capability_detection.go:170` 的 `Results`。迁移 32 逐一
覆盖：providers + bindings（`:849-854`）、deployments（`:857-858`）、operator_disabled（`:861`）、
model_capability_snapshot（`:864-883`）、三个探测桶整体清空（`:887-896`）。
`invocation_target.go:127-136` 的 `CapabilityClaim.CapabilityID` 也带能力名，但它只存在于
`DeploymentVariant`（进程内解析产物），不落任何 bbolt 桶——`bucketProviderResources` 存的是
`domain.ProviderResource`（`models.go:32-60`），无能力字段。

**半写：不存在。** 整条迁移链在**一个** `s.db.Update` 事务里（`:1599`），版本号写入
（`:1647`）与 migration_history 追加（`:1661`）同在该事务内。迁移中途崩溃 → bbolt 回滚 →
下次启动仍是 schema 31，重跑。`stepHook`/`afterUp` 的注入点（`:1621-1666`）也只是在事务内
返回错误，同样回滚。

**幂等性：由版本门保证，不由函数本身保证。** `splitJSONModeCapabilities:919-921` 是**无条件**
写两半为 false——对一条已迁移、且操作者事后勾开了 `json_object` 的记录重跑，会把它重新关掉。
但 `:1615` 的 `for currentVersion < schemaVersion` 使迁移只跑一次，重放需要有人手工回退
`meta.schema_version`。契约上（CLAUDE.md「迁移名不得复用」）这是可接受的。

---

## A5-8 · 迁移 32 对 `capability_evidence` 缺失/为 null 的记录只改一半

- `internal/store/bolt/store.go:906-928`（capabilities 半）与 `:933-955`（evidence 半）；
  `internal/domain/provider_profile.go:289-303`
- 基准：B2、B6
- 分级：**【疑问】**
- 阻塞发布：否

两个 split 函数的空值处理不对称在于它们守卫的对象不同：

- `splitJSONModeCapabilities` 守卫 `object["capabilities"]` 不存在或为 `null` → 跳过；
- `splitJSONModeEvidence` 守卫 `object["capability_evidence"]` 不存在或为 `null` → 跳过。

`CapabilityEvidenceSet` 是 `map[string]CapabilityEvidence`（`provider_profile.go:69`），nil map
序列化成 `null`。所以一条 **capabilities 有值而 evidence 为 null** 的存量记录，迁移后
capabilities 会带上 `json_object=false`/`structured_outputs=false`，而 evidence 仍是 null →
`CapabilityEvidenceSet.Validate:290-291` 的 `len(e) == 0` 直接返回
"capability evidence is required"，记录不可载入。

**降级为疑问的理由**：这类记录在**迁移之前**同样过不了 `Validate`（空集必拒，与 `json_mode`
无关），所以这不是迁移引入的新失败，只是迁移不会修好它。**是否存在这样的存量记录，必须在真实
`data/` 上实测**（对应方案 R2/R3）——我没有真实存量目录，按 CLAUDE.md「验证不可能时说明」记为
未核实。若 R2 出现「schema 31→32 后某记录拒载」，第一个该看的就是这两个函数的空值分支。

---

## A5-9 · 260814 遗留 F23（空候选误报 400）已修，且本范围的新增 400 路径判对了类别

- `internal/gateway/service.go:233-249`（F23 的修复）、`:961-966`、`:2446-2460`
- 基准：B2
- 分级：**【肯定】**
- 阻塞发布：否

F23 的修复在 `resolveRequest` 里，三分支且注释写明了顺序理由（`:235-241`
「candidate resolution drops probe-unhealthy targets before the operation filter … Reporting
that as 400 blames the request for an upstream state」）：

| 条件 | 状态码 | 行 |
|---|---|---|
| 该 alias 对该 operation **有**注册 target，只是全部 probe 不健康 | **503** `provider_unavailable` | `:242-244` |
| alias 有 target 但都不支持该 operation | 400 `unsupported_feature` | `:245-247` |
| alias 无任何 target | 404 `model_not_found` | `:248` |

本范围新增的 `unservableError`（`:2454-2459`）返回 400，判对了：它在 `resolveRequest` 已经确认
「有健康候选」之后才运行，删掉候选的三个过滤器（`:958-960`）依据的全是**请求内容**——能力需求
（`filterSemanticCapabilities:2545`）、档案不可承载的字段（`filterGenerateProfileCompatibility:2556`）、
operation（`filterPrimitiveTargets:2568`）。所以「候选清空」在这里确实是请求的问题，400 成立。
`unservableReasons:2467-2477` 附带的原因名也不含调用方数据（`:2451-2453` 已论证）。

`filterTokenCapabilities` 为空时的 400 `token_limit_exceeded`（`:987-989`）同理。

---

## A5-10 · `filterPrimitiveTargets` 清空候选时也走 400，装配错误会被报成请求错误

- `internal/gateway/service.go:960-966`；`internal/provider/profile.go:125-131`
- 基准：B2
- 分级：**【疑问】**
- 阻塞发布：否

`operationSet.Resolve` 在 `semanticGenerationPrimitives[binding.Primitive]` 为真而
`unwrapSemanticGenerator` 解包失败时返回 `(nil, false)`（`profile.go:126-129`）。该 target 随即
被 `filterPrimitiveTargets` 删除，候选清空后返回 **400** "model route does not support the
requested chat capabilities"。但这个失败的成因是**装配**（档案声明了语义 primitive，adapter 没
实现 `SemanticGenerator`），不是调用方的请求——与 F23 同型的类别错误。

**可复现路径：不完整。** 需要一个绑定 `PrimitiveOpenAIResponses` 却不是 `SemanticGenerator` 的
adapter；当前 `openai.Adapter` 实现了 `GenerateSemantic`（`adapter.go:421`），
`profileAllowsPrimitive`（`profile.go:72`）只允许 `ProfileOpenAIResponses` 绑它，
`providers.go:687-697` 只为该 profile 建 `Responses: true` 的 adapter。**当前不可达**，降级为疑问。
与 A5-3 同源：`semanticGenerationPrimitives` 表新增一项而 adapter 未跟上时，两条都会一起出现，
且症状（"connection serves nothing"）与 #231 提交信息描述的完全一致。

---

## A5-11 · H6 的遍历证据：消掉了两次，新增了一批小的（供 R5 解释用）

- `internal/gateway/service.go:915/919`、`:1100-1108`、`:967`、`:974`；
  `internal/semantic/request.go:285-302`；`internal/redaction/engine.go:278-296`、`:320-381`
  对照 v0.3.0 `service.go:903/917/921/925`、`:1049-1057`、`engine.go` 的 `ProcessInboundChat`
- 基准：无（H6 靶子）
- 分级：**【肯定】**
- 阻塞发布：否

**被消掉的（改善方向，两处，都是整轮遍历级）：**

1. **脱敏后的二次翻译。** v0.3.0 在 `ProcessInboundChat`（`:917`）之后**重新** `DecodeGenerate`
   一遍已脱敏的 wire 请求（`:921`）——一次完整的 wire→semantic 翻译。HEAD 直接在 semantic 上
   脱敏（`:967`），没有第二次翻译。`service.go:2974-2976` 的注释确认了这就是旧形。
2. **Responses↔Chat 的往返。** v0.3.0 的 Responses 走 Responses→Chat wire→再解码
   （`:1049/1053/1057`），HEAD 直达 `generate`（`:1104`）。省掉一次渲染 + 一次解码 + 相应的
   JSON marshal/unmarshal。

**被新增的（退化方向，量级小、次数多）：**

3. **每个 content part 一次字符串 JSON 往返。** HEAD 的 `processContent:330` 对每个文本 part
   调 `ProcessText`，后者是 `json.Marshal(string)` → `processRaw`（内含 `decodeJSONValue` +
   `json.Marshal`）→ `json.Unmarshal`，即**每 part 4 次 JSON 操作**。v0.3.0 的
   `ProcessInboundChat` 对整条 `message.Content`（一段 RawMessage）只做一次
   `decodeJSONValue` + `json.Marshal`，即**每 message 2 次**。多 part 消息（Responses 的
   item 结构天然是多 part）在这一项上次数更多。
4. **估算源换了对象但没换次数。** v0.3.0 `openaiapi.ChatCompletionRequest.EstimatedInputBytes`
   与 HEAD `semantic.GenerateRequest.EstimatedInputBytes`（`request.go:285-302`）都是对
   messages 的一次线性遍历，`estimateGenerateInputTokens`（`service.go:2944-2959`）额外走一次
   content parts。两版对称。
5. **新增一次需求推导。** `redactionPreservedRequirements:2977-2983` 调用
   `DeriveRequirements`，遍历一次 messages + tools。这是新增的一整轮遍历，但只做布尔或运算，
   不做序列化。

**给 R5 的结论**：Responses 路径应当**改善**（2 处整轮往返被消掉），Chat 路径的净效应取决于
每条消息的 part 数——单 part 纯文本消息上 HEAD 的 JSON 操作次数是 v0.3.0 的两倍。
如果 R5 在 Chat 上测不出退化，合理解释是基准用的是单 part 短消息、JSON 操作绝对开销小于
被消掉的翻译；**如果要证明「无退化」是真的而不是没测到，基准里应当有一条多 part 消息的用例。**

---

## A5-12 · Responses 档案承载不了的内容都在预留之前被拒（B8 反向核对）

- `internal/compatibility/openai/responses.go:102-144`；`internal/gateway/service.go:958-960`；
  `internal/compatibility/openai/provider_responses.go:88-104`；
  `internal/domain/provider_table.go:118-125`、`:150-151`
- 基准：B4、B8
- 分级：**【肯定】**
- 阻塞发布：否

`renderProviderResponseItems` 的 `default`（`provider_responses.go:103`
"content is not portable to Responses"）发生在 adapter 内部，即**预算预留之后**
（`service.go:1010` 预留 < `:1031` Provider I/O）。逐条查它能拒到什么，结论是**够不到**：

- **`ContentProviderToolCall` 作为输入回传**（多轮把上一轮的 `web_search_call` 发回来）：
  北向 `decodeResponseInput` 的 item 类型白名单只有 `message`/`function_call`/
  `function_call_output`（`responses.go:117-141`），`web_search_call` 落 `default` →
  400，**在 facade 内、进 `generate` 之前**。不可达。
- **`ContentReasoning`**：`DeriveRequirements:233-234` 把它提成 `Reasoning` 需求，
  `capabilityRequirements` 里 `reasoning` 有配对（`service.go:2540`），而
  `openAIResponsesSet` 刻意不含 Reasoning（`provider_table.go:122-125`，理由在 `:118-121`）
  → `filterSemanticCapabilities`（`:958`）在预留前删掉 target。不可达。
- **`Stop`/`Seed`/`Candidates`**：`RenderProviderResponseRequest:21-28` 会在预留后拒，但
  `compatibility/provider_fields.go:129-143` 已把 `n`/`stop`/`seed` 声明为该档案不可承载，
  `filterGenerateProfileCompatibility`（`:959`）在预留前删掉 target。不可达。

三条堵路各自有行号，`provider_table.go:118-121` 的注释也把这条规则写成了档案设计原则
（「承载不了的声明就是预留之后才失败的请求」）。这一处的 B4/B8 是成立的。

---

## 未核实（交给后续阶段）

1. **迁移 32 的存量数据方向**（A5-8 的实测面）——需要 v0.3.0 二进制建库后用 HEAD 启动，
   对应方案 R2/R3。本轮无真实存量目录。
2. **控制台如何呈现 A5-5 的三种 ProbeKind**——属阶段 2（`findings-web.md`），按任务约定未读。
3. **A5-6 的启动恢复是否把 `running` 探测收敛为终态**——`:548-551` 的注释声称有，未找到并核实
   该恢复代码路径。
4. **Bedrock Mantle 四个 Responses/Chat 档案的探针往返保真度**——`chatViaResponses` 的同型往返
   在 Mantle adapter 上是既有代码，本轮未逐探针核对（对应方案 S2 / R6）。
