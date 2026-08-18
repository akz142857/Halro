# DeepSeek 适配方案 — 已接入，但与真实上游不符

状态：**片 1–4 已实现，片 5 未做**（无真实凭据）；上游契约已按官方文档逐条抓取核对（2026-08-18），仍未跑过真实账号
建立日期：2026-08-14
修订日期：2026-08-17（对着当日 `main` 重核六条：四条原样成立；§2.5 的缓存档位已落地，正文按现状重写，
　　　　　并因此改变了 §2.4 的性质与顺序；§2.6 已由能力矩阵单一真相关闭，保留为已关闭记录）
实施日期：2026-08-17（片 1–4；实施中发现方案本身写错四处，记在 §6）
文档复核：2026-08-18（第一次真正抓取官方文档；§1 有四处不准，其中两处已是实现缺陷，均已修，记在 §7）
范围：`internal/modelcatalog`、`internal/compatibility`、`internal/openaiapi`、`internal/provider/openai`、`internal/domain`
相关：[适配链条的未完成项](../prd/adaptation-open-items.zh-CN.md)、[Provider 适配缺口](../prd/provider-adaptation-gaps.zh-CN.md)

## 0. 这份方案要回答什么

**DeepSeek 不是待接入的新供应商，它已经接进来了。** `deepseek.chat.v1` 是一个完整的 Provider
Profile：类型 `deepseek`、Access Surface `deepseek-api`、Bearer 静态凭据
（`internal/domain/provider_profile.go:18,30`），两个 Primitive
（`internal/provider/primitive.go:26-27`），profile manifest 声明 chat 与 chat stream
（`internal/provider/profile.go:398-403`），能力上限为
`{Chat, Streaming, Tools, JSONMode, Reasoning, StreamUsage}`（`internal/domain/models.go:604-607`），
三个北向端点的兼容清单里都列了它（`internal/compatibility/manifest.go:199,219,239`），
真实账号 matrix 也早就有 `HALRO_MATRIX_DEEPSEEK_` 这一档
（`docs/verification/provider-real-matrix.md:35`）。

所以要回答的不是「怎么接」，而是另一个问题：**这条已经在跑的链，哪些地方与今天的真实上游不符。**
2026-08-14 逐条核对下来有六处，其中两处会让账目算错、三处会让调用方拿到 200 但语义已经变了。
2026-08-17 重核：**四处原样成立，一处（§2.5）因缓存档位落地而改变形状，一处（§2.6）已关闭。**

执行上它落在一个特殊位置：DeepSeek 走的是「直接用 OpenAI wire」那条分支
（`internal/app/providers.go:657` 与 OpenAI、openai-compatible 共用适配器；
`internal/compatibility/provider_fields.go:109` 与它们共用字段申报），**整条链上没有一行
DeepSeek 专属代码**。这既是它当初接得快的原因，也是这六处差异的共同成因。

## 1. 上游契约（2026-08-14 核对官方文档）

核对来源，均为当日抓取：

- `https://api-docs.deepseek.com/api/create-chat-completion`
- `https://api-docs.deepseek.com/api/list-models`
- `https://api-docs.deepseek.com/quick_start/pricing`
- `https://api-docs.deepseek.com/guides/kv_cache`
- `https://api-docs.deepseek.com/guides/reasoning_model`

**模型**：`GET /models` 现在只列 `deepseek-v4-flash` 与 `deepseek-v4-pro`，上下文 1M、最大输出 384K。
`deepseek-chat` / `deepseek-reasoner` 这两个名字在当前文档里已经不出现。

**请求字段**（Chat Completions 接受的全集）：`messages`、`model`、`thinking`、`max_tokens`、
`response_format`、`stop`、`stream`、`stream_options`、`temperature`、`top_p`、`tools`、
`tool_choice`、`logprobs`、`top_logprobs`、`user_id`。

`frequency_penalty` 与 `presence_penalty` 文档明写 "This parameter is no longer supported"。
`n`、`seed`、`parallel_tool_calls`、顶层 `reasoning_effort`、`user` **不在接受列表里**。

**推理**：不再是「换一个模型」，而是同一模型上的开关：`"thinking": {"type": "enabled"}`，
配 `reasoning_effort`（`low`/`high`/`max`）。两个模型都支持。

**用量**：`usage` 返回 `prompt_tokens`、`completion_tokens`、`total_tokens`、
`completion_tokens_details.reasoning_tokens`，以及 **`prompt_cache_hit_tokens`**、
**`prompt_cache_miss_tokens`**。上下文缓存默认开启、无需改代码、按前缀命中，几小时到几天后自然失效。

**价格**：缓存命中与未命中是两个价位，差距约 50 倍（v4-flash 约 $0.0028 与 $0.14 每百万输入 token）；
另有分时折扣，文档记为非高峰时段减半，2026-08-16 起生效。**具体数值以官方页面为准，本文不做定价数据源。**

> 这一节全部来自文档，**没有一条经过真实账号验证**。这条链上一次真实运行的教训写在
> [批处理方案 §5](../prd/anthropic-batches-plan.zh-CN.md)：同一天里假上游三次没能拦住真实缺陷。
> 所以 §4 的验证不是收尾，是完成条件。

### 1.1 2026-08-18 第一次真正抓了官方文档，本节四处不准（详见 §7）

上面这一节是**转述**，写方案时并没有把页面抓下来逐条比对。2026-08-18 实际抓取
`api-docs.deepseek.com/zh-cn/` 之后，模型名、上下文/输出上限、请求字段清单、用量字段名**全部对上**，
但有四处不准，其中两处已经变成实现里的缺陷（已修）：

| 本节写的 | 文档实际 | 后果 |
|---|---|---|
| `thinking` 只记了怎么开 | `type` 取 `enabled` / `disabled`，**默认 `enabled`** | `none` 被错误申报为不支持；且默认开启意味着不发这个字段就会思考并计费 |
| `response_format` 只写「接受」 | 只接受 `text` 与 `json_object`，**没有 schema 模式** | `json_schema` 会在预算预留之后吃 400 |
| 缓存命中/未命中差距「约 50 倍」 | 两个模型都是 **30 倍**（flash 0.05 / 1.5 元，pro 0.15 / 4.5 元，每百万 token） | 仅文案，注释与文档已订正 |
| 分时折扣「2026-08-16 起生效」 | 已在生效，高峰为北京时间 **9:00–12:00 与 14:00–18:00**，非高峰减半 | 不改变 §5 的「不做」决定，但价差是常态而非将来时 |

`reasoning_effort` 的默认值文档记为 `high`，仍嵌在 `thinking` 里、没有顶层版本——这两条与本节一致。

## 2. 六处差异

### 2.1 内置目录记的是两个已经不存在的模型

`internal/modelcatalog/builtin.go:220-229` 记 `deepseek-chat`（131072/8192）与
`deepseek-reasoner`（131072/65536，带 Reasoning、刻意不带 Tools 与 JSONMode）。上游现列的是
`deepseek-v4-flash` 与 `deepseek-v4-pro`，1M 上下文、384K 输出。

后果分两层：目录覆盖不到操作者真正会填的模型名，于是新建部署走「目录未覆盖」那条路，能力只能靠
识别或管理员声明；而上下文/输出上限差了一个数量级，Token Guard 与预算按 131072 判断，
`max_tokens` 也会被按 8192 截。

`deepseek-reasoner` 那条注释里「刻意不给 tools 与 JSON mode」的判断，在 thinking 变成开关之后
已经失去对象——没有一个「推理模型」可以单独降能力了。

> 已修，且**没有等片 5**，理由见 [§6.4](#64-片-4-没有等片-5)。

### 2.2 字段申报按 OpenAI 全集，实际上游只接受一个子集

`internal/compatibility/provider_fields.go:109` 把 DeepSeek 与 OpenAI、Azure、openai-compatible、
Mantle Chat 放在同一个分支，只申报 `messages[].content[].is_error` 不支持。而
`RenderGenerateRequest`（`internal/compatibility/openai/mapping.go:68-69`）会把 `n`、`seed`、
`parallel_tool_calls`、`reasoning_effort`、`user` 一并渲染上线。

对着 §1 的接受列表，这五个字段没有一个在里面。后果不是同一种：

- 上游拒绝的（按文档 `frequency_penalty`/`presence_penalty` 明确不再支持）：请求在**预算预留之后**
  失败，错误里出现调用方确实写过、但网关本可以提前拦下的字段；
- 上游忽略的：调用方拿到 200，而 `seed`、`n`、`parallel_tool_calls` 从未生效——这正是
  「不支持的字段拒绝而非静默丢弃」这条要挡的情况；
- `user` 是第三种：DeepSeek 有这个概念但字段名是 `user_id`。**发 `user` 等于既没申报也没透传。**

修法的形状与 Gemini、Converse 那两支一致：DeepSeek 从直通分支里拆出来，按真实子集申报，
`internal/compatibility/manifest.go` 的三处 `ProfileDeepSeekChat` 覆盖同步。
`user` 是唯一一个可以真正承载的，改渲染而不是申报。

> **本节的字段清单不准，见 [§6.1](#61-22-少数了一个字段多算了两个)**：少了
> `max_completion_tokens`，而 `frequency_penalty` / `presence_penalty` 这一条是空的。

### 2.3 thinking 接口不可达，而 manifest 说它「不支持」

能力位 `Reasoning` 在 DeepSeek 的默认能力里是开着的（`internal/domain/models.go:604-607`），
但网关能发出去的只有 OpenAI 的顶层 `reasoning_effort`，DeepSeek 的接受列表里没有这个字段——
**能力开着，路走不通**。

同时 `internal/compatibility/manifest.go:239` 对 `/v1/messages` 北向声明 DeepSeek 不支持
`thinking`。这句话在写下时是对的（DeepSeek 那时没有这个概念），现在变成了一句错话：
上游有 `thinking`，缺的是 Halro 这一段映射。**先接上映射，再改这句声明**——反过来做就是在
manifest 里留一句没有实现支撑的承诺。

映射本身有现成的对照物：`semantic` 已有 ReasoningEffort，Anthropic 那支已经在做
「语义档位 → 供应商自己的思考开关」的翻译（`internal/compatibility/anthropic/mapping.go`），
DeepSeek 是同一形状的第三例，不需要新概念。

> **上面第二段读错了 manifest，见 [§6.2](#62-23-后半段是读错了-manifest)**：那句 `thinking`
> unsupported 说的是 Anthropic 北向的 `thinking` 块配置，对所有非 Anthropic profile 都成立，
> 保留不动；改的是同一行的 `output_config.effort`。第一段成立且已修。

### 2.4 缓存命中的 token 一个都没记下来

`openaiapi.Usage` 只从 `prompt_tokens_details.cached_tokens` 读缓存
（`internal/openaiapi/types.go:200,220-224`），语义层再由
`internal/compatibility/openai/mapping.go:471,497` 搬进 `CachedInputTokens`。
DeepSeek 不发这个结构，它发的是 `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`，
**于是 DeepSeek 的缓存命中恒为 0**。

Anthropic（`internal/provider/anthropic/adapter.go:414`）、Bedrock
（`internal/provider/bedrock/adapter.go:721`）、Mantle 都在记这个数，只有 DeepSeek 这一路是空的。

**2026-08-17 起这一条不再只是「记不记得下来」。** 提案时缓存命中即使记下来也没有费率可用，所以
本条与 §2.5 被刻意分开。缓存档位当天落地后（`DeploymentPriceVersion.CachedInputMicrosPerMillion`，
`internal/domain/pricing.go:199`；拆档计价见 `internal/domain/pricing_cost.go` 的
`CalculateUSDTokensV1`），费率已经在跑，缺的只剩这个解码：**DeepSeek 报的缓存命中恒为 0，
于是每一次命中都按未命中价结算。** 按官方价目表，那一段贵 30 倍（§1 原写「约 50 倍」，见 §1.1）。

这把本条从「为将来的计价留输入」变成「今天的账就是错的，而修法只差一个解码」——
它因此排到最前面（见 §3）。

### 2.5 缓存档位已落地，分时价位仍没有落点（跨供应商，非 DeepSeek 独有）

**提案时（2026-08-14）**：`DeploymentPriceVersion` 只有 `InputMicrosPerMillion` /
`OutputMicrosPerMillion` / `FixedRequestMicrosUSD`，两个缓存维度都无处安放，所以本条被划为
「不作为 DeepSeek 适配的副作用来做」的独立决定。

**2026-08-17 起，两个维度已经分开：**

- **缓存档位已落地。** `DeploymentPriceVersion` / `DeploymentPriceProposal` / `PriceSnapshot`
  都有了 `CachedInputMicrosPerMillion`（`internal/domain/pricing.go:199`），
  `CalculateUSDTokensV1` 按 `(input - cached) × input_rate + cached × cached_rate + …` 拆档
  （`internal/domain/pricing_cost.go`），见 [ADR 0022](../adr/0022-cache-read-input-pricing.md)。
  **对 DeepSeek 的意义：费率已经能表达命中价，唯一缺口是 §2.4 的解码。** 这正是把 §2.4 提到
  片 1 的理由。
- **分时折扣仍没有落点，但缺的不是时间轴。** `DeploymentPriceVersion.EffectiveFrom`
  （`internal/domain/pricing.go:202`）本来就在，价格版本可以在某个时刻整体切换。缺的是
  **按每日时段重复的折扣**——一条「非高峰减半」的规则需要周期性调度，而不是再来一个版本。
  这仍然是独立决定：它改的是价格版本的持久结构与所有供应商的既有记录，属于
  `CLAUDE.md` 说的「契约变更不能顺手发生」那一类。

**本方案对分时折扣仍然只负责记录，不实施**（见 §5）。写这一节时要避免的两个错：
说「计价没有缓存档位」（已经有了），和说「价格版本没有时间轴」（`EffectiveFrom` 一直在）。

> **2026-08-18 更新：能力已具备，本方案的范围不变。** 分时折扣经
> [独立评审](../prd/time-of-day-pricing-review.zh-CN.md)后已实施，见
> [ADR 0023](../adr/0023-time-of-day-pricing.md)：价格版本可携带按供应商本地时段的费率表。
> 也就是说操作者现在**可以**把 DeepSeek 的高峰（北京时间 9:00–12:00 与 14:00–18:00）
> 与非高峰分别填成两档，不必再在一个固定价里二选一。这是配置动作，不是本方案的交付物——
> 本方案仍然只负责把这条事实记清楚。

### 2.6 ~~能力上限的三份真相同样适用~~（已关闭）

**2026-08-17 关闭。** 提案时 DeepSeek 的能力集存在 `DefaultProviderCapabilitiesForProfile`、
`MaxProviderCapabilitiesForProfile` 与 `web/src/pages/ProvidersPage.tsx` 三处。现已收敛为
`internal/domain/provider_table.go` 一张表（DeepSeek 见 `deepSeekSet`，defaults 与 ceiling 同源），
控制台不再手抄，能力矩阵由 `GET /admin/api/v1/provider-profiles` 下发。见
[适用能力改由服务端统一下发](../prd/provider-capability-single-source.zh-CN.md)。

**对本方案的影响**：§2.1 若改动 DeepSeek 的能力集，**改 `provider_table.go` 一处即可**，
前端自动跟随，不再需要三处同步，也不再有「勾了但后端 400」这个失败模式。

## 3. 切片划分

2026-08-17 重排。原顺序是纯依赖顺序；现在片 1 换成用量解码，因为缓存档位落地后它是唯一一条
**有真实金额偏差、修法明确、不依赖凭据**的。片 1–4 不需要凭据，片 5 需要。

| # | 内容 | 判据 | 是否需要真实凭据 | 状态 |
|---|---|---|---|---|
| 1 | 用量解码：`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` → `SetCachedPromptTokens` | 缓存命中不再恒为 0；命中 + 未命中 = `prompt_tokens`；结算按命中价而不是未命中价 | 否（假上游可验证形状，真实账号才验证语义） | **已实现** |
| 2 | 字段申报：DeepSeek 从 OpenAI-wire 直通分支拆出，按 §1 接受列表申报；`user` 改渲染为 `user_id`；manifest 三处同步 | 接受列表之外的字段，要么被申报、要么被渲染成上游认识的形状，不存在第三种 | 否 | **已实现**（比方案多一个字段，见 §6.1；`parallel_tool_calls` 的粒度改了一次，见 §6.5） |
| 3 | thinking 映射：语义 ReasoningEffort → `thinking:{type,reasoning_effort}`；映射接上后再改 manifest 那句 `thinking` unsupported | 打开 Reasoning 能力的部署，发出的请求里带 `thinking` | 否 | **已实现**；manifest 那句**不改**，理由见 §6.2 |
| 4 | 目录订正：模型名换成 `deepseek-v4-flash` / `deepseek-v4-pro`，上下文 1M、输出 384K，删掉 reasoner 专属条目 | 目录里没有上游列不出的模型名 | 否，但**建议等片 5 的真实 `GET /models`** | **已实现**（未等片 5，见 §6.4） |
| 5 | 真实账号 smoke：matrix runner 已有 `HALRO_MATRIX_DEEPSEEK_` 这一档，跑一次非流式、一次流式、一次带 thinking、一次重复前缀看缓存命中 | 四项都拿到真实响应，且 §1 的每条推断各自被证实或推翻 | **是** | **未做**；四项的断言已写进 `TestRealProviderSmoke`，缺的只是凭据 |

**片 4 排在最后是刻意的。** 它写死的是上下文与输出上限，而 Token Guard、预算与 `max_tokens`
截断都读这两个数：写错的代价比其余几片都高，而它们眼下**只有文档依据**。若真实凭据一时拿不到，
片 4 可以先做「删掉上游列不出的模型名」这一半，把新模型的上限留到片 5 之后再填。

**片 5 之外的四片都不构成完成。** §1 全部来自文档，文档与真实上游不一致这件事在这个仓库里已经
发生过三次（见[批处理方案 §5](../prd/anthropic-batches-plan.zh-CN.md)）。

### 3.0 评审前先确认：修的路径与在跑的路径是不是同一条

本方案六处差异修的全是 **`deepseek.chat.v1` 直连 profile**（`api.deepseek.com`，Bearer 静态凭据）。

2026-08-17 在一台开发实例上抽查当日用量分区时发现：其中的 `deepseek.v3.2` 调用走的是
**`bedrock.mantle.openai.chat.v1`**，即经 AWS Bedrock Mantle 提供的 DeepSeek，而不是直连 profile。
样本只有一天七条，**不足以证明没有人用直连**；但它足以说明评审时要先问一句：

- 直连 profile 今天有没有真实流量？若没有、且短期不会启用，片 1–4 的紧迫性要重新排。
- 若 DeepSeek 主要经 Mantle 使用，则 §2.4 的缓存解码在 Mantle 那条路上是否同样缺失，
  是一个本方案没有覆盖、但金额后果相同的问题——**需要单独核实，不要假设两条路一样**。

### 3.1 pre-1.0.0 的处理方式

按 `CLAUDE.md`：**旧模型名直接换掉，不与新名并存**。不保留 `deepseek-chat` 作为别名，不加
「兼容旧目录」的第二条读路径。

需要说明的影响：已经建好、指向 `deepseek-chat` 的 Deployment 不会被目录覆盖到，会显示为
「目录未覆盖」并要求管理员声明或重新识别。**这不需要重新初始化数据目录**，但要操作者知道会看到
这个状态——它正是能力评审机制该有的反应，不是故障。

## 4. 验证计划

- 片 1–4 各自的单元与契约测试，每处修复先写一个在缺陷状态下会红的测试，再做反向验证
  （退回旧行为确认变红，`-count=1`，且断言脚本替换真的落了）
- 字段申报那一片的测试必须**双向**：不支持的要申报，能透传的要断言不申报——只查前一半的表
  发现不了漏申报（这正是 Mantle `messages[].name` 那条的成因，见
  [Provider 适配缺口 §5.1](../prd/provider-adaptation-gaps.zh-CN.md)）
- 片 1 的判据要一路断到**结算金额**，不能只断到解码：解码正确而费率没接上、或接上了却仍按未命中
  价结算，都是这条要挡的（缓存档位见 §2.5）。一条命中 + 未命中混合的用量，其
  `CommittedMicrosUSD` 应当低于同样 token 数全按未命中价算出的值
- 能力集若随 §2.1 改动，改 `internal/domain/provider_table.go` 一处即可，控制台由端点下发自动跟随
  （§2.6 已关闭）。**本方案预计不触碰 `web/src`，因此也不应产生 `internal/webui/dist` 的改动**；
  若产生了，说明改错了地方

## 5. 明确不做的

- **不引入分时价位**（§2.5）。独立决定，涉及价格版本的持久结构与所有供应商。
  缓存档位已不在此列——它已于 2026-08-17 落地，本方案片 1 要做的是把 DeepSeek 的命中数**喂给**
  这个已有的费率，而不是新建计价结构
- **不接 DeepSeek 的 Anthropic 兼容端点**。同一个供应商出现第二条 Access Surface 会让路由、
  凭据绑定与能力上限各多一份真相，收益是 SDK 兼容而 Halro 的北向已经提供了 `/v1/messages`。
  要做也应当是一次独立评审。

  **2026-08-18 核实：端点存在**，`https://api.deepseek.com/anthropic`，凭据走 `x-api-key`
  （[文档](https://api-docs.deepseek.com/zh-cn/guides/anthropic_api)）。核完之后**更不该顺手接**，
  理由比原来那条更硬：它**按前缀改写模型名**——`claude-opus*` → `deepseek-v4-pro`，
  `claude-haiku*` / `claude-sonnet*` → `deepseek-v4-flash`，**认不出的名字一律落到
  `deepseek-v4-flash`**。Halro 的成本归属建立在「一个别名解析到确定的一个上游模型」之上，
  一个静默兜底的上游会把这个前提废掉：账记在哪个模型上不再由 Halro 决定。真要接，这一条本身
  就得是评审的第一个议题。另外它忽略 `anthropic-version` / `anthropic-beta`，不支持图片、文档、
  签名 thinking，`metadata` 只认 `user_id`，`thinking` 忽略 `budget_tokens`

- **不接 DeepSeek 的 Responses 端点**（2026-08-18 新增记录）。DeepSeek 有原生 Responses 实现
  （[文档](https://api-docs.deepseek.com/zh-cn/guides/responses_api)），无状态、不支持
  `previous_response_id` / `conversation` / `store`，与 Halro 的 `openai.responses.stateless.v1`
  北向几乎同形。今天 Halro 的 `/v1/responses` 打到 DeepSeek 走的是 Chat Completions 原语，
  换成原生 Responses 是新 profile + 新 Primitive，与上一条同属独立评审。

  **有一条值得单独记**：该文档明写不支持的参数是**静默忽略**（"静默忽略"），不是报错。
  这正是 §2.2 那条规则要挡的形状——所以即便将来接了原生 Responses，字段申报仍然要由 Halro 自己做，
  不能指望上游把不认识的字段顶回来
- **不动 openai-compatible 那一支**。它服务的是「未知的兼容服务器」，保守默认是它的正确姿态；
  DeepSeek 拆出来之后，两者的差别正好变得显式。**实施时确认了一处具体后果**：
  `openAICompatibleModels()` 里的 `deepseek-chat` / `deepseek-reasoner` 两条**保留不动**——那是
  第三方兼容服务器可能仍在托管的模型名，且只声明 chat + streaming，与直连 profile 的目录是两回事

## 6. 实施记录（2026-08-17）：方案本身写错的三处

片 1–4 落地时逐条对着代码核，发现三处方案的判断不成立。都按代码的事实改，方案正文这里改口。

### 6.1 §2.2 少数了一个字段，多算了两个

**少的那个是 `max_completion_tokens`。** §2.2 列了五个「渲染上线但上游不接受」的字段
（`n`、`seed`、`parallel_tool_calls`、`reasoning_effort`、`user`），但 `RenderGenerateRequest`
还渲染 `MaxCompletionTokens`，而它同样不在 §1 的接受列表里。按方案自己的规则
（「要么被申报、要么被渲染成上游认识的形状，不存在第三种」），它必须申报。

**没把它改名成 `max_tokens`，是刻意的**：OpenAI 的 `max_completion_tokens` 是把推理算进去的总预算，
DeepSeek 的 `max_tokens` 是另一个量，把前者当后者发就是悄悄改了调用方的请求。两个限额因此分开，
只有 DeepSeek 表达不了的那个被申报。

连带后果，方案没预见到：`/v1/responses` 的 `max_output_tokens` 解码进的正是 `CompletionTokenLimit`
（`internal/compatibility/openai/responses.go:24`），所以**经 `/v1/responses` 且写了
`max_output_tokens` 的请求，从此不再路由到 DeepSeek**。这是个真实的收窄，代价明确、修法要等片 5
把 `max_tokens` 的语义核实清楚。manifest 的 responses 覆盖里已按此申报。

**多算的两个是 `frequency_penalty` / `presence_penalty`。** §2.2 说文档明写它们「不再支持」，
这没错；但 `openaiapi.ChatCompletionRequest` 根本没有这两个字段，Halro 从来没往上游发过它们。
这一条对本方案是空的。

### 6.2 §2.3 后半段是读错了 manifest

§2.3 说 `manifest.go` 对 `/v1/messages` 声明 DeepSeek 不支持 `thinking`「现在变成了一句错话」。
**这句话仍然是对的**，而且和 DeepSeek 无关：那里的 `thinking` 指的是 **Anthropic 北向请求里的
`thinking` 块配置**（`{type, budget_tokens}`），`DecodePortable` 对**所有**非 Anthropic profile 一律
拒绝它（`internal/compatibility/anthropic/mapping.go:23`），OpenAI、Azure、Mantle Chat 那几行写的
是同一句话。DeepSeek 上游的思考开关不是靠这个字段到达的。

那条端点上真正通向推理的是 `output_config.effort`（`mapping.go:68` → 语义 `ReasoningEffort`）。
所以改的是它：DeepSeek 的覆盖里新增 `output_config.effort` 的按值申报，`thinking` 原样保留，
并把「为什么保留」写进该行的 declared transform，免得下一个人再读错一次。

§2.3 前半段成立且已修：能力位 `Reasoning` 开着而路走不通，现在 `low` / `high` 两档经
`thinking:{type:"enabled",reasoning_effort:…}` 到达上游。

**顺带发现、本方案未修**：Gemini 与 Converse 在 `/v1/messages` 那份覆盖里也没申报
`output_config.effort`，而它们的 `provider_fields.go` 分支是无条件申报 `reasoning_effort` 的。
两者不一致，属于同形的另一处 manifest 缺口，需要单独一次评审。

### 6.3 `none` 档按失败关闭处理

§1 只记了怎么把思考**打开**（`{"type": "enabled"}`），没有记怎么关。所以 OpenAI 阶梯上的 `none`
被申报为不支持，而不是「不发 `thinking` 就等于 none」——后者要成立，前提是 DeepSeek 默认不思考，
而这一点没有依据。片 5 可以一次问清楚，然后收窄这条申报。

同理，可达档位是 `low` / `high` 两个：DeepSeek 的 `max` 在上游真实存在，但每个 portable 请求都要过
OpenAI 阶梯，那条阶梯止于 `xhigh`，所以 `max` 到不了；`minimal` / `medium` / `xhigh` 则是反过来，
portable 有而 DeepSeek 没有。两头的界各有出处，测试里分别记着。

### 6.4 片 4 没有等片 5

§3 建议片 4「等真实 `GET /models`」，理由是上下文与输出上限写错的代价最高。**没等**，因为等下去的
代价是把旧的两个名字继续留在目录里——那才是今天真实生效的错误声明：`131072` / `8192` 会让 Token Guard
与预算按十分之一的量判断，而这两个名字上游已经列不出来了。1M / 384K 只有文档依据，这一点写在
`deepSeekModels()` 的注释里，片 5 是它的验证条件。

### 6.5 `parallel_tool_calls` 第一版申报得太宽（已修）

先按「出现即申报」写，测试也照这个写了，是错的。`DecodeToolChoice`
（`internal/compatibility/toolchoice.go:79`）**永远**返回一个非 nil 的并行标志，默认「允许并行」；
于是 `/v1/messages` portable 上**任何带 `tool_choice` 的请求**都会带上这个标志，DeepSeek 因此被从
候选里删掉——包括那些对并行只字未提的请求。从 OpenAI 那一侧完全看不出来，因为那条路上这个字段是
调用方给的。

正确的粒度：**只有「关掉并行」才是损失**。允许并行本来就是不发这个字段的含义，和 `n=1` 一样；
Anthropic 那支的渲染器也正是只在 `!parallel` 时才动作（`anthropic/mapping.go:334`）。申报与渲染器
双双改成按值判断，并补了一条从 Anthropic 北向进、经 `DecodePortable` 到申报的测试——
反向验证时三个测试同时变红。

**这条留在这里是有用的**：一个字段「出现即申报」看着像最保守的写法，但保守的方向要看这个字段是
谁填的。调用方填的，出现即意图；协议解码器填的，出现只是默认值。

### 6.6 连带修的一处：能力探测的输出上限参数

方案没有提到能力探测这条路。`DetectCapability`（`internal/provider/capability_detection.go`）给每个
探针都带 `max_completion_tokens`，而 DeepSeek 不接受它——南向请求体收窄之后，**DeepSeek 的每一个探针
都会在进程内失败**，连接测试回来什么都没建立起来。

值得记的是：这条在收窄之前就已经坏了，只是坏在上游。`real_smoke_test.go` 的注释里早就写着
「DeepSeek 与已评审的兼容端点用的是旧参数」，但能力探测那条路对所有 profile 一律发新参数。
收窄只是把失败从上游挪到了本地，并因此让它变得可见。

修法是按 profile 选参数名。**这不是「悄悄改写请求」那一条要挡的情况**：这个上限是 Halro 自己为了让
探针便宜而设的，没有调用方的意图要保全，只有一个参数名要写对。openai-compatible 那一支同样可能需要
旧参数，但那属于「不动 openai-compatible」的范围（§5），没有跟着改。

### 6.7 ~~片 5 要顺带核的一条：`response_format` 的取值范围~~（已由文档定论，见 §7.2）

提案与实施时都只知道「`response_format` 在接受列表里」，不知道它接受哪些 `type`，所以刻意没有
按猜测收窄。2026-08-18 抓到官方文档后不必等片 5 了：只接受 `text` 与 `json_object`。已按值申报。

### 6.8 §4 的两条预期都成立

- `web/src` 未触碰，`internal/webui/dist` 无改动。能力集也未改——`deepSeekSet` 本来就带 `Reasoning`
- 每处修复都做了反向验证：退回旧行为确认变红、`-count=1`、且脚本替换前先断言搜索串仍在
  （用量解码、请求整形、字段申报、目录订正、探测上限、并行标志六处各一次）
- 全量门跑过一遍：`go test ./...`、`go vet ./...`、`gofmt` 均干净。前端门未跑，因为没有碰 `web/`

## 7. 2026-08-18：第一次抓官方文档，改掉两处实现缺陷

起因是一条连接测试失败的日志（见 §8），顺着去看文档，才发现 §1 一直是转述而非抓取。
抓下来之后模型名、上下文/输出上限、请求字段清单、用量字段名全部对上，但另有四处不准（表见 §1.1），
其中两处已经是代码里的缺陷。

### 7.1 `none` 档不该被拒绝，而且默认是「思考开着」

实施时按「文档只写了怎么开、没写怎么关」把 `none` 申报成不支持（原 §6.3）。**文档实际写着
`thinking.type` 取 `enabled` / `disabled`，默认 `enabled`。**

两个后果，第二个更重要：

- `none` 是可服务的，现在映射到 `thinking:{"type":"disabled"}`，不再被路由排除；
- **默认是开着的**，所以「什么都不说」不等于「不思考」。

> **本条当天下午被一条真实日志推翻，见 §9。** 当时的结论是「Halro 对未指定的请求仍然不发这个字段，
> 因为那是上游的默认，替调用方改它就是替他选了深度和账单」。这个判断只核了
> `/v1/chat/completions`（那条路 `reasoning_content` 确实能原样返回），**没有核 `/v1/responses`**
> ——而那条路会直接失败。现在未指定即发 `{"type":"disabled"}`，与 Anthropic 那支同规则。

片 5 因此多了一个探针：发一次 `none`，断言 `reasoning_tokens == 0`。这是「关得掉」和
「默认真的是开着」两件事唯一的真实证据。

### 7.2 `response_format` 只有 `text` 与 `json_object`

没有 schema 模式。已按值申报：`json_schema` 路由排除，`json_object` 照常。形状与 Anthropic 那支
的 `response_format` 按值申报一致。这条原本挂在 §6.7 等片 5，现在文档直接定论了。

### 7.3 价差是 30 倍，不是 50 倍

flash 缓存命中 0.05 元 / 未命中 1.5 元，pro 0.15 / 4.5，每百万 token，两个都是 30 倍。
注释、测试注释与 `provider-real-matrix.md` 已订正。**这不改变 §2.4 的结论**——命中恒为 0 仍然是
每次命中都按未命中价结算——只是倍数写错了。

### 7.4 分时折扣已经在生效

高峰为北京时间 9:00–12:00 与 14:00–18:00，非高峰减半。§5 的「不做」决定不变，但要明白它现在的代价：
操作者配的是一个固定价，因此**每天有一半时间的账是错的**（非高峰按高峰价记，偏贵一倍）。
这比提案时写的「将来要处理」更迫切一点，仍然是独立评审。

### 7.5 另外两条 Access Surface 的核实结果记在 §5

Anthropic 兼容端点与 Responses 端点都真实存在，核实之后**更不该顺手接**——尤其前者会把认不出的
模型名静默兜底成 `deepseek-v4-flash`，直接废掉 Halro 成本归属的前提。详见 §5。

## 8. 触发这次复核的那条日志：与本方案无关

```
provider probe failed: Get "https://api.deepseek.com:443/v1/models/deepseek-v4-flash":
outbound host "api.deepseek.com": refused before any bytes were sent:
reserved address 198.18.4.112 is not allowed
```

**不是 Halro 的缺陷，也不是模型名错。** `198.18.0.0/15` 是 RFC 2544 基准测试保留段，
`safetransport` 明确拒绝（`internal/safetransport/transport.go:220`）。开发机上跑着 fake-IP 模式的
本地代理（mihomo / Clash Party，`utun1500`，网关 `198.18.0.1`），系统解析器把
`api.deepseek.com` 解成了这个池子里的假地址；`dig` 直接问 DNS 拿到的是真地址
`123.125.246.121` / `116.140.43.136`。

SafeTransport 自己解析、自己校验、只拨已校验的那个 IP（pinned dialing），假地址在代理的映射表之外
没有意义，拒绝是对的。而且是**发出任何字节之前**拒绝的，没有泄漏也没有计费。

Halro 刻意不读环境代理变量，所以没有「给它配个代理」这条路。解决在本机 DNS 那一侧：
把 `api.deepseek.com` 走直连规则，或把代理从 fake-ip 换成 redir-host。

**不要因此放宽 `deniedPrefixes`**——那是失败关闭策略，`CLAUDE.md` 明确要求这类放宽必须单独评审。

## 9. 2026-08-18 下午：`/v1/responses` 流式直接失败，推翻 §7.1 的结论

```
error_class: malformed_response  retryable: false  ambiguous: true
reason: consume canonical provider stream:
        Phase 1A Responses streaming only supports output text
```

**不是 DeepSeek 不支持 SSE。** SSE 正常，Halro 也正常消费到了语义事件——是 Halro 自己的
Responses 流式渲染器（`internal/compatibility/openai/responses.go`）拒绝了一个不是输出文本的增量。

链条：

1. 调用方打 `POST /v1/responses`，`stream: true`；
2. 该端点**明确拒绝** `reasoning` 请求字段（`internal/openaiapi/responses.go:133`，
   "reasoning output cannot be represented losslessly in Phase 1A"）——**调用方根本无法要求推理**；
3. 于是渲染出的 Chat 请求不带 `reasoning_effort`；
4. **DeepSeek 的 `thinking` 默认 `enabled`、默认 effort `high`**，所以它思考了；
5. 流里带 `delta.reasoning_content` → `DecodeEvent` 映射成 `semantic.ContentReasoning`；
6. `ResponseStreamRenderer.Accept` 拒绝，请求以 `malformed_response` 结束。

后果比「多花钱」重：`ambiguous: true`，按保守口径入账。**调用方为一段自己无权请求的推理付了钱，
而且什么都没拿到。**

### 9.1 §7.1 错在只核了一条北向

§7.1 的原话是「DeepSeek 的 `reasoning_content` portable 侧装得下，没有表达问题，只有账单问题」。
这句话对 `/v1/chat/completions` 成立（`openaiapi.Message.ReasoningContent` 能原样返回），
**对 `/v1/responses` 不成立**。同一个语义结果，能不能表达取决于北向端点，而供应商侧的渲染
看不见调用方走的是哪个端点。

于是当时那句「同一个默认，因为失败模式不同而给出不同答案」正好说反了：失败模式**是**同一个，
就是 `anthropic/mapping.go:344-357` 那段注释描述的东西——上游默认思考，portable 侧装不下，
调用方拿到一个上游已经执行并计费的请求的失败。

### 9.2 修法：未指定即关，与 Anthropic 同规则

`RenderDeepSeekChatRequest` 在 `ReasoningEffort == ""` 时发 `thinking:{"type":"disabled"}`。

除了修掉这个失败，它还消掉一处**与调用方无关的差异**：在此之前，同一个 portable 请求，
路由到 Anthropic 就不思考、路由到 DeepSeek 就思考并多计一段输出费。带回退的路由下，
这两条路是同一个请求可能走到的两个地方。**网关存在的意义之一就是消掉这种差异。**

推理在 DeepSeek 上仍然可达，只是要明说：Chat Completions 用 `reasoning_effort`，
Messages 用 `output_config.effort`，`low` / `high` 两档。

### 9.3 顺带：那句拒绝以前不说自己看见了什么

原文案是「Phase 1A Responses streaming only supports output text」，不说看见的是哪一种内容。
一个思考模型、一次工具调用、一个畸形分片，在日志里长得一模一样，而客户端只拿到一个
`provider_error`。现在把 content kind 带上——那是固定枚举，不是上游文本，不泄漏任何东西。

### 9.4 还没有验证的部分

「默认 `enabled`」来自 API 参考页，仍未经真实账号确认；片 5 的探针（发 `none`、断言
`reasoning_tokens == 0`）是它唯一的真实证据。但**本条修复不依赖那个默认值是否为真**：
未指定即显式关闭，无论上游默认是什么，行为都确定。
