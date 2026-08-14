# DeepSeek 适配方案 — 已接入，但与真实上游不符

状态：**提案待评审**；六条差异已对着代码核过，上游契约按官方文档核过（2026-08-14），**未跑过真实账号**
建立日期：2026-08-14
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
这一条只关乎「记不记得下来」，不涉及计价——计价是 §2.5，两件事要分开做。

### 2.5 缓存与分时价位没有落点（跨供应商，非 DeepSeek 独有）

`DeploymentPriceVersion` 只有 `InputMicrosPerMillion` / `OutputMicrosPerMillion` /
`FixedRequestMicrosUSD`（`internal/domain/models.go:885-886`），
`CalculateUSDTokensV1` 就是这三项相加（`internal/domain/pricing_cost.go:31-46`）。

也就是说：**即使 §2.4 把缓存命中数记下来，账本也仍会按未命中价给它计费。** 在 DeepSeek 上这个
偏差约 50 倍（命中价约为未命中价的 1/50），在 Anthropic 与 Bedrock 上是同一个洞、倍数小一些。
分时折扣是第二个维度——价格版本没有时间轴，账目会随时段整体偏离最多一倍。

**这一条明确不作为 DeepSeek 适配的副作用来做。** 它改的是计价模型与价格版本的持久结构，
影响每一个供应商与既有的价格记录，属于独立决定（与 `CLAUDE.md` 对 Beta 上限的要求同类：
放宽或改变契约不能顺手发生）。本方案只负责把它记下来，并保证 §2.4 的记录先到位——
没有记录，将来任何计价方案都没有输入。

### 2.6 能力上限的三份真相同样适用

`DefaultProviderCapabilitiesForProfile`、`MaxProviderCapabilitiesForProfile` 与
`web/src/pages/ProvidersPage.tsx` 里手抄的表——DeepSeek 的能力集在这三处各存一份。这不是本方案
新增的问题，逐条记在
[适配链条的未完成项 §2](../prd/adaptation-open-items.zh-CN.md)。§2.1 若改动 DeepSeek 的能力集，
三处要一起改，否则控制台会出现「勾了但后端 400」。

## 3. 切片划分

顺序是依赖顺序，不是优先级。片 1、2 不需要凭据，片 5 需要。

| # | 内容 | 判据 | 是否需要真实凭据 |
|---|---|---|---|
| 1 | 目录订正：模型名换成 `deepseek-v4-flash` / `deepseek-v4-pro`，上下文 1M、输出 384K，删掉 reasoner 专属条目 | 目录里没有上游列不出的模型名 | 否 |
| 2 | 字段申报：DeepSeek 从 OpenAI-wire 直通分支拆出，按 §1 接受列表申报；`user` 改渲染为 `user_id`；manifest 三处同步 | 接受列表之外的字段，要么被申报、要么被渲染成上游认识的形状，不存在第三种 | 否 |
| 3 | thinking 映射：语义 ReasoningEffort → `thinking:{type,reasoning_effort}`；映射接上后再改 manifest 那句 `thinking` unsupported | 打开 Reasoning 能力的部署，发出的请求里带 `thinking` | 否 |
| 4 | 用量解码：`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` → `SetCachedPromptTokens` | 缓存命中不再恒为 0；命中 + 未命中 = `prompt_tokens` | 否（假上游可验证形状，真实账号才验证语义） |
| 5 | 真实账号 smoke：matrix runner 已有 `HALRO_MATRIX_DEEPSEEK_` 这一档，跑一次非流式、一次流式、一次带 thinking、一次重复前缀看缓存命中 | 四项都拿到真实响应，且 §1 的每条推断各自被证实或推翻 | **是** |

**片 5 之外的四片都不构成完成。** §1 全部来自文档，文档与真实上游不一致这件事在这个仓库里已经
发生过三次（见[批处理方案 §5](../prd/anthropic-batches-plan.zh-CN.md)）。

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
- 前端 `ProvidersPage.tsx` 的能力表若随 §2.6 改动，`npm run build` 后一并提交 `internal/webui/dist`

## 5. 明确不做的

- **不引入缓存档位与分时价位**（§2.5）。独立决定，涉及价格版本的持久结构与所有供应商
- **不接 DeepSeek 的 Anthropic 兼容端点**。同一个供应商出现第二条 Access Surface 会让路由、
  凭据绑定与能力上限各多一份真相，收益是 SDK 兼容而 Halro 的北向已经提供了 `/v1/messages`。
  要做也应当是一次独立评审。**注意：本条未核实该端点当前是否仍存在**，评审时先核
- **不动 openai-compatible 那一支**。它服务的是「未知的兼容服务器」，保守默认是它的正确姿态；
  DeepSeek 拆出来之后，两者的差别正好变得显式
