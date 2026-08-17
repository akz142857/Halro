# DeepSeek 适配方案 — 已接入，但与真实上游不符

状态：**提案待评审**；上游契约按官方文档核过（2026-08-14），**未跑过真实账号**
建立日期：2026-08-14
修订日期：2026-08-17（对着当日 `main` 重核六条：四条原样成立；§2.5 的缓存档位已落地，正文按现状重写，
　　　　　并因此改变了 §2.4 的性质与顺序；§2.6 已由能力矩阵单一真相关闭，保留为已关闭记录）
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
于是每一次命中都按未命中价结算。** 按 §1 的价差，那一段贵约 50 倍。

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

| # | 内容 | 判据 | 是否需要真实凭据 |
|---|---|---|---|
| 1 | 用量解码：`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` → `SetCachedPromptTokens` | 缓存命中不再恒为 0；命中 + 未命中 = `prompt_tokens`；结算按命中价而不是未命中价 | 否（假上游可验证形状，真实账号才验证语义） |
| 2 | 字段申报：DeepSeek 从 OpenAI-wire 直通分支拆出，按 §1 接受列表申报；`user` 改渲染为 `user_id`；manifest 三处同步 | 接受列表之外的字段，要么被申报、要么被渲染成上游认识的形状，不存在第三种 | 否 |
| 3 | thinking 映射：语义 ReasoningEffort → `thinking:{type,reasoning_effort}`；映射接上后再改 manifest 那句 `thinking` unsupported | 打开 Reasoning 能力的部署，发出的请求里带 `thinking` | 否 |
| 4 | 目录订正：模型名换成 `deepseek-v4-flash` / `deepseek-v4-pro`，上下文 1M、输出 384K，删掉 reasoner 专属条目 | 目录里没有上游列不出的模型名 | 否，但**建议等片 5 的真实 `GET /models`**（见下） |
| 5 | 真实账号 smoke：matrix runner 已有 `HALRO_MATRIX_DEEPSEEK_` 这一档，跑一次非流式、一次流式、一次带 thinking、一次重复前缀看缓存命中 | 四项都拿到真实响应，且 §1 的每条推断各自被证实或推翻 | **是** |

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
  要做也应当是一次独立评审。**注意：本条未核实该端点当前是否仍存在**，评审时先核
- **不动 openai-compatible 那一支**。它服务的是「未知的兼容服务器」，保守默认是它的正确姿态；
  DeepSeek 拆出来之后，两者的差别正好变得显式
