# 阶段 4 · 对抗验证裁决

按 `review-plan.md` §9：对阶段 1–3 最严重的几条，各派一个独立的**证伪型**角色，默认 finding
为错，要求在代码里复现完整路径或找出拦截防御。裁决 CONFIRMED / REFUTED / PARTIAL。

上两轮（260805、260807）这一环各自改写了结论——六条 P0 无一原样成立、五条最严重发现无一
原样成立。本轮同样：**第一条裁决就剔掉了原陈述里的记账半条，并把影响面从一个 profile
扩大到五个。**

---

## V1 · MiniMax Chat 的输出上限拒绝穿过能力过滤 → **CONFIRMED（剔除记账半条）**

**原陈述**（A5 F-A5-2 与 A6 F-A6-1 独立报出）：`minimax.chat.v1` 在渲染层拒绝两类请求，
但这两类拒绝既没进 `provider_fields.go` 的字段规则，也没进端点清单；于是能力过滤放行、
预算预留落盘、然后在 `encodeChatRequest` 里 400，且不触发 fallback。

**裁决：CONFIRMED。** 证伪角色用 overlay 探针端到端跑了 8 个真实请求，三条全部复现。

### 成立的部分

1. **最重的一条，比原陈述更绝对**：`/v1/messages` portable 路径上，
   **凡携带 `output_config.effort` 且落到 `minimax.chat.v1` 的请求，100% 在预留之后失败**。
   原因有两层：
   - `internal/compatibility/anthropic/mapping.go:31` 是**无条件**赋值，而
     `anthropicapi.MessageRequest.MaxTokens` 是非指针 `int64`
     （`internal/anthropicapi/types.go:148`）——**连"根本没写 max_tokens"的请求也拿到指向 0
     的非空指针**。
   - Anthropic 的合法 effort 阶梯是 `low/medium/high/xhigh/max`
     （`internal/anthropicapi/types.go:200`），**没有 `none`**。所以原陈述里"非 none 的
     effort"这个限定是多余的：任何合法 effort 都让 `minimax.go:154` 的 `thinkingOn` 为真。

   而 `internal/compatibility/manifest.go:353` 恰恰把 "output_config.effort reaches
   MiniMax's thinking switch" 声明为**受支持的 transform**。**已发布契约与实现直接矛盾**，
   且那条转换在这条路径上永远执行不到。

2. `/v1/chat/completions` 上两类请求同样穿过过滤：`max_tokens` + `max_completion_tokens`
   并存，或 `max_tokens` + 非 none 的 `reasoning_effort`。实测
   `UnsupportedGenerateFields(minimax.chat.v1) = []`，而对照组
   `UnsupportedGenerateFields(deepseek) = [max_completion_tokens]` 正确拦下。

3. **违反 B4**：拒绝发生在 `ReserveAttemptDetailed`/`MarkStarted`/attempt 落盘之后
   （`internal/gateway/service.go:464-501` → `:1063` → `internal/provider/openai/adapter.go:373`）；
   `retryable()` 对 `ErrorBadRequest` 返回 false（`service.go:2333-2342`），
   `:1126-1133` 直接跳出整个 target 循环——**同一别名下的第二个 target 拿不到请求**；
   客户端得到 400 `invalid_request_error`，`Param` 为空（`ProviderCode` 为空），**不指名字段**。

### 被推翻 / 必须剔除的部分

- **"记账问题"不成立。** `settlementForResult`（`internal/gateway/service.go:2711`）在
  `ambiguousProviderFailure` 为 false 时于 `:2733` **提前返回，从不调用 `setSettlementCost`**，
  `CommittedMicrosUSD` 保持 0，预留干净释放；断路器走 `availabilityFailure`
  （`:2343-2356`），`ErrorBadRequest` 不在名单里，记为成功。
  **这是可用性 + B4 契约问题，不是账目问题。** 代价只剩一条 attempt 行、一次额度、
  一次瞬时预留占用。
- **"manifest 完全没提"不精确。** Chat 端点 `manifest.go:290` 的 `DeclaredTransforms` 里用
  散文写了这条约束；缺的是 `UnsupportedRequestFields` 里的**机器可读条目**——所以能力过滤
  读不到它。Messages 端点（`:353`）则连散文都没有，而且方向相反。

### 机制能不能表达：能。这是漏写，不是表达不了

`fieldSink` 是 `func(condition bool, field string)`（`provider_fields.go:308`），condition 可以是
对整个 `semantic.GenerateRequest` 的任意布尔式。DeepSeek 那条本身就是双字段 + 取值组合
（`provider_fields.go:174-176`）。证伪角色把镜像规则 overlay 进去实测：三种失败请求的
过滤结果全部变成 `[max_tokens]`（路由前被排除），而正常请求仍 `[]` 并正常渲染。

**注意字段名要翻转**——两者是镜像而不是复制：

| | 上游原生上限成员 | 语义字段 | 多出来的那个 | 应声明的字段名 |
|---|---|---|---|---|
| DeepSeek | `max_tokens`（只管答案） | `VisibleOutputTokenLimit` | `max_completion_tokens` | `max_completion_tokens` ✅ 已声明 |
| MiniMax | `max_completion_tokens`（含推理） | `CompletionTokenLimit` | `max_tokens` | `max_tokens` ❌ **缺失** |

### 为什么会漏（直接证据）

DeepSeek 有 `internal/compatibility/deepseek_test.go:125`
`TestDeepSeekRendersEveryRequestItsDeclarationAdmits`，把"渲染器拒绝的必须已声明 /
声明放行的必须能上线"双向锁死。**`internal/compatibility/minimax_test.go` 没有任何等价测试**
——8 个 Test 全是纯渲染器断言，没有一个调用 `UnsupportedGenerateFields`。
而 `manifest_derivable_coverage_test.go:23` 只锁 manifest ↔ 字段规则的一致性，
**两边同时沉默时它是满意的**。

这正是 B12 的反面教材：注册点合并之后的门是穷举的，但"渲染器与声明一致"这道门是
**逐个平台登记**的，MiniMax 没登记。

### 顺带发现：同一个 B4 缺口影响五个 profile，不止 MiniMax

`output_config.effort: "max"` 是 Anthropic 合法值（`anthropicapi/types.go:200`），
但不在 `openaiapi.ReasoningEffortLevels`（`openaiapi/types.go:16`）里。实测该请求对
`openai.chat-embeddings.v1`、`azure-openai.chat-embeddings.v1`、
`openai-compatible.chat-embeddings.v1`、`minimax.chat.v1`、`bedrock.mantle.chat.v1`
的字段规则**全部返回 `[]`**，然后在 `internal/provider/primitive.go:163` 以
`reasoning_effort is invalid` / `ErrorBadRequest` 死在预留之后。
**只有 `deepseek.chat.v1` 声明了 `[reasoning_effort]`。**

这一条超出原 finding 范围，单列。

### 修复不是机械补一行

只加镜像规则，会让 `minimax.chat.v1` 被**每一个**带 effort 的 portable Messages 请求排除
（因为 `VisibleOutputTokenLimit` 在该路径上恒非 nil），从而与 `manifest.go:353` 的 transform
声明彻底对立。两条出路二选一，是设计决定：

- 把 `output_config.effort` 一并加进 Messages 端点那行的 `UnsupportedRequestFields`（承认这
  条转换在 MiniMax Chat 上不成立）；或
- 处理 `mapping.go:31` 在 `max_tokens` 缺省时不该产出 `ptr(0)` 的问题（更根本，但影响所有
  portable Messages 消费者）。

---

## V2 · MiniMax Chat 的 json_object 是否不可达 → **PARTIAL（核心推论被推翻）**

**原陈述**（A5 F-A5-1）：#250 依据实测把 `JSONObject: true` 加进 `minimaxChatSet`，manifest 与
渲染器都跟上了，但模型目录没有（`builtin.go:421` 的 `openAIChat := anthropicChat`）；
于是 MiniMax target 是 `MetadataSourceNone`、能力只能来自目录、运维勾不上、请求期被过滤 →
**"cd37927 声称修好的能力实际不可达"**。

**裁决：PARTIAL。核心事实成立，后果链在第 3 步断裂。**

### 三张能力表（探针实跑）

| profile | ceiling（= Defaults） | 目录 MiniMax-M3 | 目录 M2.x（7 个） |
|---|---|---|---|
| `minimax.anthropic.messages.v1` | chat streaming tools vision fetched_image reasoning stream_usage | 同左 · ctx=1,000,000 out=524,288 | chat streaming tools stream_usage · ctx=204,800 |
| `minimax.chat.v1` | 同上 + **json_object** | 同上（**无 json_object**） | chat streaming tools stream_usage |
| `minimax.responses.v1` | chat tools vision fetched_image | 同左 | chat tools |

`Clamp(目录, ceiling).json_object = false`，8/8 条 MiniMax 条目全部如此。

### 成立的部分

1. `openAIChat := anthropicChat`（`builtin.go:421`）属实；MiniMax target 是
   `MetadataSourceNone`，走不到 `MapCapabilityClaims`；`Clamp` 是交集、
   `dependencyClosure` 只减不加——**自动**能力确实只来自目录。
2. 走控制台默认路径（带 `resolution_revision`）新建的 MiniMax Chat 部署不会自带
   `json_object`；此时请求返回 **400 `unsupported_feature`**，reason 含 `json_object`。已实跑。
3. **唯一残留的真实缺口**：编辑**既有**的目录来源部署时，`json_object` 复选框**渲染但禁用**
   （`DeploymentsPage.tsx:1618-1628`），提示是 `deployments.notDeclaredByModel`
   （"此模型未声明该能力"），不像 `enableOnConnection` 那样给出下一步。原地加宽要重建部署。

### 被推翻的部分

1. **"能力实际不可达"——错。三条路可达**：
   - **控制台新建表单**以**连接上限**为界（`DeploymentsPage.tsx:2149`
     `<CapabilitySubsetEditor ceiling={bindingCeiling}>`），旁边的注释就是为这种情况写的：
     "a capability the connection carries and the catalogue merely did not record belongs here"。
   - **服务端 operator_declared 路径**（`admin_deployments.go:1096-1112` 明写"Letting an
     explicit declaration exceed a catalog entry is deliberate"）。实跑：无 mode 时报错并
     **提示改用 `mode=operator_declared`**；带 mode 时 `caps.json_object=true`；
     超出 ceiling（structured_outputs）仍被封顶。
   - **verified probe**：`capability_detection.go:149-151` 依 binding 能力发探针，
     **#250 加 ceiling 这一步正是让 MiniMax Chat 第一次拥有 json_object 探针的原因**，
     证据等级 `verified`，高于目录能给的 `declared`。
2. **"运维勾不上"——新建时勾得上**，只有编辑既有部署时勾不上。
3. **"对外承诺与实际不一致"——不成立。** `manifest.go:291` 陈述的是 **profile 渲染器**
   承载什么（且属实），从不承诺某个 deployment 已启用它。
4. **"目录没跟上 = 缺陷"——不成立，反而按 finding 去改才是 B11 的反面。**
   - `builtin.go:9-43` 的 seeding policy 明写："An entry that under-claims costs an operator
     one deliberate declaration; it does not brick their deployment"——第 2 条测出来的正是这个行为。
   - `json_object` 在 MiniMax 上**没有模型级证据**：三份文档都不提 `response_format`。
     唯一证据是 2026-08-31 的一次**裸 HTTP** 请求（`openai/minimax_real_smoke_test.go:235-261`，
     注释明说"Sent raw: the renderer refuses response_format by design"），
     **1 个模型（M3）、1 个端点、1 个 host（国际站）、1 个账号、1 次请求**。
     写进 8 个模型 = 用它给 7 个从未测过的模型放宽，且目录条目不带 region，会连带覆盖
     从未测过的 `api.minimaxi.com`。
   - 层次也对：`json_object` 在 MiniMax 上首先是**连接级事实**（改之前渲染器直接拒
     `response_format`，目录声明再多也发不出去）。
   - 这是全仓库常态形状而非 MiniMax 缺口：探针统计"ceiling 声明了但条目不声明"的位——
     `bedrock.mantle.chat.v1 / json_object 37/37`、
     `bedrock.mantle.openai.responses.v1 / structured_outputs 11/11`、
     `openai.responses.v1 / provider_executed_tools 15/15`。
5. 即使要加，也只有 `MiniMax-M3` + `ProfileMiniMaxChat` 一条有证据；但按目录自己的规则
   **仍然不该加**：`SourceBuiltin` 的证据上限是 `declared`，把一次真实测量写进 builtin
   反而把 `verified` 级事实降级成"评审声明"。测量的正确归宿是 `SourceVerifiedProbe`，
   而那条路 #250 已经打开了。

## V3 · 汇总端点的两段读竞态 → **CONFIRMED（已实测复现；严重度性质需改写）**

**原陈述**（A1 F-A1-4 与 A2 F-A2-1 独立报出）：`collectSummaryRows` 先读 bbolt
（`admin_usage_summary.go:289`）、再读内存增量（`:292`），两者之间无共同锁。若
`saveUsageCheckpoint` 在中间完成一次 checkpoint（`TakeCheckpoint` 清空 `rollupDelta`、
`PutUsageCheckpoint` 提交 bbolt），那批增量**两头落空**，在响应里计为零。

**裁决：CONFIRMED，并且是本轮唯一一条被实测复现的并发缺陷。**

### 实测复现（overlay 探针，仓库未改）

探针只在 `UsageRollupRange` 里加了一个 nil 钩子，业务逻辑一字未改；测试走真实 Runtime +
真实 admin HTTP 端点：

```
TestSummaryRaceCheckpointInsideView    inside-view:    attempts=2 watermark=20 (期望 4)  ← 复现
TestSummaryRaceCheckpointBetweenReads  between-reads:  attempts=2 watermark=20 (期望 4)  ← 复现
TestSummaryRaceControl                 control:        attempts=4 watermark=20           PASS
下一次刷新：attempts=4 watermark=20
```

`inside-view` 变体没有触发超时 ⇒ 写事务确实能在读事务打开期间提交完成，**没有 bbolt 死锁**。
这说明"窗口 = 整个 View 事务时长"是真实可达的，不是纸面推演。

### 窗口的真实大小：不是微秒级，也不是"那几行"

漏账的充要条件是 **drain 落在 `(View Begin, PendingRollup)` 之间**，即窗口 ≈ 整个
`UsageRollupRange` 的 View 时长 + 一次 bbolt Update 时长。实测（本机 SSD、页缓存热）：

| rollup 规模 | View 事务时长 | 每次刷新的命中率（60s checkpoint 间隔） |
|---|---:|---:|
| 30 天 × 5 键/维（默认日视图、小装机） | 4.3 ms | ~7e-5 |
| 400 天 × 5 键/维 | 57 ms | ~1e-3 |
| 400 天 × 50 键/维（10 万行） | 0.54 s | ~0.9% |
| 400 天 × 200 键/维（上限，40 万行） | **2.2 s** | **~3.6%** |

所以两种极端说法都被推翻：不是"微秒级、可忽略"，也不是"一定以秒计"——取决于 rollup 规模。
控制台自动刷新会线性放大次数。误差幅度上界是**一个 checkpoint 间隔的流量**（默认 ≤60s）。

### `watermark_sequence` 那半条：成立

`TakeCheckpoint` **不推进** watermark——watermark 早在 `Apply` 里就随记录推进了
（`aggregate.go:279-296`），被 drain 的增量对应的 sequence **早已包含在** `a.watermark` 里。
所以响应必然宣称覆盖到一个它没计入的 sequence。实测：竞态响应
`attempts=2, watermark_sequence=20`，正确值 `attempts=4, watermark_sequence=20`
——同一个 watermark 下两个数字。按 20 重放 WAL 得到 4，响应说 2 ⇒ **违反 B8**。

### 必须改写的部分：是瞬时读错，不是持久错账

被 drain 的增量由 `PutUsageCheckpoint` 在**一个** `Update` 里连同 rollup state 一起提交
（`store_usage.go:61-78`），磁盘上没有丢账；**下一次刷新即正确**（实测）。
所以定性应是"读路径合并算法的原子性缺陷：数字会无声偏小、且无法从 watermark 反查"，
**不是"记账权威受损"**。A2 把它定为"高"、A1 定为 B8 违反——方向对，但要写清它自愈。

### 修法不是换顺序

证伪角色算过：反转顺序（先内存后磁盘）会把两种异常的窗口都缩到几条指令，概率低几个数量级，
但引入重复计数模式。**正确的修法**是让两次读描述 WAL 的同一个前缀——store 已经在同一个
`Update` 里写了 `keyUsageRollupState`（`store_usage.go:74-77`），summary 可以在同一个 View 里
把它读出来，在 `PendingRollup` 之后发现它前进过就重试一次。

附带事实：`collectSummaryRows`（`:254-260`）与 `PendingRollup`（`rollup.go:222-228`）的注释
都没提读取顺序，**看不出现在的顺序是刻意选的**。

## V4 · MiniMax Anthropic 面缺 base_resp 守卫 → **PARTIAL（触发条件被证伪）**

**原陈述**（A3 F-A3-1）：`checkMiniMaxBaseResp` 只装在 OpenAI 两面，MiniMax 的**默认** profile
（Anthropic 面）没有；于是同一个上游 200-包错误在 Anthropic 面被判成 `Ambiguous` 并按估算收费，
在 Chat 面零成本——同一账号同一密钥，默认面收费、另一面不收费。

**裁决：PARTIAL。** 两件事必须分开裁决，证伪角色把它们拆开了：

### 成立（CONFIRMED）：守卫缺口是事实，且实现没覆盖自己方案书的范围

全树 `checkMiniMaxBaseResp` 调用点只有 `internal/provider/openai/adapter.go:500`（unary）与
`:673`（SSE）。`internal/provider/anthropic/adapter.go` 里**没有任何** 200-包错误检查：
2xx 直接进 `anthropicapi.DecodeMessage`。

而 `docs/prd/minimax-adaptation-plan.zh-CN.md` §3.1 的处置口径写的是"在 MiniMax 的
**三条 surface** 上，解码后一律先看 `base_resp`"。**实现只覆盖了两条。**
这是本条最扎实的部分：不是"可能有 bug"，是"实现没做到方案书写下的范围"。

### 成立（CONFIRMED，但只限非流路径）：后果链实跑通了

证伪角色用 httptest 返回 `200 + {"base_resp":{"status_code":1004}}`，直连 Anthropic 适配器：

```
MessagesNative:       class=malformed_response ambiguous=true  → 按估算提交，CostEstimated=true
MessagesNativeStream: class=malformed_response ambiguous=false → 零成本
```

gateway 包内实测（$0.3/M in、$1.2/M out）：`max_tokens=1024` → committed 1235 microsUSD；
`max_tokens=8192` → 9837。Chat 面同一个 1004 → committed 0。

**流式那一半不成立**：SSE 拿不到任何事件 → `emitted=false` → `Ambiguous: false`
（`adapter.go:634`）→ 零成本。

### 被证伪（REFUTED）：触发条件没有证据，而且有一次直接反证

`docs/prd/minimax-adaptation-plan.zh-CN.md` §1.3（2026-08-31 无凭据实测，三个 host 逐格相同）：

| 路由 | HTTP | 体 |
|---|---|---|
| `POST /v1/chat/completions` | **401** | `{"type":"error",...}` |
| `POST /anthropic/v1/messages` | **401** | `{"type":"error","error":{"type":"authentication_error",...}}` |
| `POST /v1/embeddings` | **200** | `{"base_resp":{"status_code":1004,...}}` |

**finding 举的那个例子（1004 在 Anthropic 面）恰恰被实测过，回的是规规矩矩的 401 +
Anthropic 错误信封**——走 `decodeHTTPError` → `ErrorAuthentication`、非模糊、零成本，
**与 Chat 面完全一致，不存在不对称**。

同一份 PRD §1.3 结论 4（标注"都不是推测"）：**`base_resp` 是原生路径的信封，两张兼容面
用 `{"type":"error"}`**。唯一实测到 200+base_resp 的路由是 `/v1/embeddings`，而 MiniMax 的
Embeddings 能力**根本没被声明**（`provider_table.go:386-389`）——**Halro 永远不会调那条路由。**

顺带：Anthropic 面的 real smoke（`internal/provider/anthropic/minimax_real_smoke_test.go`）
**只有一个子测试**（bearer 被接受），错误形态在该路由上从未被带凭据测过。

### 被证伪（REFUTED）："这是 MiniMax 新缺陷"

`adapter.go:853` 的 `malformed(...)` 被 `provider/anthropic` 包**所有**路径共用，对任何 upstream
的"2xx 但解不出 Message"都产出 `Ambiguous:true`。Bedrock Mantle 的 Anthropic 面与真 Anthropic
走同一行。对那两者这个默认是**对的**（200 却解不出 Message，最可能是生成跑过而被截断，
保守计账正是 CLAUDE.md 要求的）。

### 裁决后的严重度：低（防御纵深 / 一致性缺口），**不是发布阻断项**

登记口径应是"实现未覆盖 PRD §3.1 声明的三面范围"，**不是"计费缺陷"**。理由按权重：
触发形态无证据且有直接反证；唯一实测到 200-包错误的路由 Halro 不调用；只影响非流；
即使触发也是 `CostEstimated=true` 留痕可重算；MiniMax 是 Beta。

处置建议（若要做）：把 `checkMiniMaxBaseResp` 提到 `internal/provider` 共享层，在
`MessagesNative` 的 2xx 分支按 `ProviderType == ProviderMiniMax` 调一次；**同时**给 Anthropic 面
的 real smoke 补一个"不存在的模型"子测试，把这块空白**测出来**而不是靠推断堵上。

## V5 · 默认窗口不对齐 / 桶数上限不约束遍历量 → **A: PARTIAL｜B: CONFIRMED｜C: PARTIAL**

三条相关主张（A2 F-A2-3 与 F-A2-2）分别裁决。

### 主张 A（默认窗口不对齐周期边界）→ **PARTIAL**

**被推翻的两处**：

- **`Start`/`End` 是真实覆盖区间，且前端用了它们**。`bucketSpan`（`:375-381`）取该标签下
  实际存在的行的 min/max，残缺的 `2025-09` 桶带的是 `Start=2025-09-30`、`End=2025-10-01`
  ——一天，不是一个月。前端三处在用：`web/src/trend.ts:65`（**x 轴坐标就是 `bucket.start`**，
  并用 `bucket.end` 做缺口检测）、`UsageSummaryPanel.tsx:298`（下钻链接的绝对边界）、
  `:340`（CSV 的 start/end 两列）。
- **图不是柱状图**。`web/src/TrendChart.tsx:85-93` 是 uPlot 折线/面积图，x 轴是真实时间轴。
  残缺的 9 月点被画在 9 月 30 日的真实位置上，与 10 月 1 日只隔 1 天，而后面每两点相隔约
  30 天——**首段近乎垂直，本身就是视觉信号**，只是没有文字说明。
- **滚动窗口是刻意设计**。`docs/prd/usage-summary-reporting.zh-CN.md:395` 明写"month 取最近
  12 个月……均以当前账期日为末端"，`admin_usage_summary_test.go:301-303` 把起点
  `2025-09-30` 钉死。

**仍然成立的**：桶标签写着 `2025-09` 而数值只有 1 天；`totals` KPI 覆盖整个 start..end，
所以"最近 12 个月总花费"实际是 11 个月 + 1 天；界面与 i18n **没有任何"残缺周期"的标注**。
缓和因素：桶从有数据的行建起，9 月 30 日无流量时那个残桶根本不存在。

### 主张 B（月末规范化让默认窗口静默少一个月）→ **CONFIRMED**

证伪角色自己穷举 2024-01-01..2028-12-31（**1827** 天，原文写 1826，含两个闰年）：

```
granularity=month  expected=12  mismatches=34     per year: 2024=7 2025=6 2026=7 2027=7 2028=7
granularity=year   expected=3   mismatches=0
granularity=day    expected=30  mismatches=0
```

**34 这个数字对，三个例子逐字命中**，包括 `end=2026-01-31 → start=2025-03-03`
（`2025-02-31` 被 Go 规范化为 3 月 3 日）。唯一偏差是分布不是"每年 7 个"——2025 只有 6 个，
闰日吸收了一个（`2025-01-29 → 2024-02-29`，闰日存在、不被规范化）。

触发规律：`end` 落在 1/3/5/8/10 月的 29–31 号。

**分量比原文说的更重**：它违反 PRD 自己写的"最近 12 个月"合同，一年里有 6–7 天只给 11 个月。
而现有两个测试都看不见它——`TestUsageSummaryDefaultRangeFitsItsOwnCeiling` 只断言
`2 ≤ buckets ≤ ceiling`，`TestUsageSummaryRangeIsInclusiveAndBounded` 只用一个不触发规范化的
固定日期。

year 另有一个更轻的同类问题：`AddDate(-2,0,0)` 在 2/29 被规范化
（`2024-02-29 → 2022-03-01`），桶标签只取 `[:4]` 所以桶数不变，**代价是窗口静默短了 1 天**。

### 主张 C（桶数上限不约束遍历量）→ **PARTIAL**

**成立的**：

- `UsageRollupRange` 对范围内**每一行**无条件 `DecodeRollupKey` + `json.Unmarshal`，
  之后才交给 `keep` 按 `wanted` 过滤——不想要的维度白付全部解码代价。
  实测（overlay 建真实 bolt 库，365 天 × 1006 行/天）：
  ```
  wrote 367,190 rows; db = 682.0 MB
  walk: unmarshalled=367,190  kept=73,730  in 2.01s
  TotalAlloc delta = 565.1 MB（不强制 GC 时瞬时堆实测到 203 MB）
  ```
- **`UsageRollupRange` 与 `collectSummaryRows` 都不接 context**，bbolt View 事务本身不可取消；
  **admin `http.Server` 没有 `WriteTimeout`**（`runtime.go:1327-1337` 只设
  `ReadHeaderTimeout`/`ReadTimeout`/`MaxHeaderBytes`），全仓无 `TimeoutHandler`。
  **"客户端断开也不会停"成立**：服务端把整个游标走完、把几百 MB 建起来、再往一个断掉的连接写。
  read_only 管理员可反复触发，`requireAdmin` 无 GET 限流。
- 三份值拷贝的结构判断正确：`byDimension` 的 append 确实复制整个 480 字节 `summaryRow`
  （`DailyRollup = 424` 字节，实测 `unsafe.Sizeof`）。

**被推翻/夸大的**：

- **内存估算高了约一个数量级**。`keep` 先按 `wanted` 过滤，而 `wanted` 最多 2 个维度，
  所以每天只**留下** 202 行而不是 1006。实测 365 天常驻堆 106 MB（`merged` 释放后 77.6 MB），
  外推 3652 天约 **1.06 GB**——数字接近原文的 1.2GB，但推理路径错，且需要 db 长到 6.8 GB。
- **"现在就能被打爆"不成立**。3652 天需要**十年**真实日历时间；PeriodID 来自事件准入时打的
  账期戳，没有注入历史日期的路径；v0.5.0 新装是 0 天数据。
  现实拐点更早：day 视图上限 400 桶 = 400 天，**一年满负荷（367k 行）已经是 2 秒的 bbolt
  读事务 + 数百 MB churn**——这才是该写进发布记录的数字。
- **"注释说了它没做的事"夸大了**。`summaryBucketCeilings` 确实同时约束了走的天数
  （400 / 1096 / 3652），只是三个粒度差 9 倍、且与每桶成本方向相反——**是约束粗糙，不是虚假声明**。

附带：PRD `:584-587` 只讨论了**存储**体量（"最坏约 1000 行/天、36 万行/年"，与实测 367,190 行
/ 682 MB 吻合），**没有讨论查询时的内存与时延**。rollup 按 PRD D4-a 刻意不设保留期
（`PruneBefore` 只作用于 Parquet 分区，`internal/usage/parquet.go:333`），只增不减。
