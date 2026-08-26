# 模型部署页改版：模型目录 + 侧边详情抽屉

状态：草案 · 第二版（已过多角色评审，结论有实质修正）
日期：2026-08-24
涉及页面：`web/src/pages/DeploymentsPage.tsx`
参考形态：AWS Bedrock 控制台「模型」页（卡片网格 + 右侧详情抽屉 + 能力图标）

## 修订记录

第一版的诊断有三处被评审推翻，已在本版就地改正 —— 按本仓库 pre-1.0 的规矩，
错的判断不与改正版并存，所以下面读到的就是当前认定的结论。为免评审意见丢失，
仅在此列出被撤回的三条：

1. **撤回**「Mantle 上所有模型都没有元数据来源」。实际只有两个 Responses profile 走
   `bedrockmantle` 适配器；Mantle 的 chat profile 由 **OpenAI 适配器**服务，
   Anthropic messages profile 由 **Anthropic 适配器**服务。
2. **撤回**「anthropic 适配器仅提供基本标识」。它填 `SupportedOperations` 与上下文/输出上限，
   并且 `MapCapabilityClaims` 明确把 `image_input` 映射成 `vision`。
3. **撤回**「缺失的 `ProviderCode` 是本次症状的成因」。那是 Responses profile 的真问题、
   已修复，但用户这个部署不在那条路上。真正的成因见第 1.2 节，是一处更早、更共性的缺陷。

---

## 1. 起因

操作者在 `AWS-Mantle-OpenAI` 服务商下配置 `openai.gpt-5.6-luna`。AWS 官方模型页明确列出该模型
输入支持 Text 与 Image、输出支持 Text，Halro 的识别结果却是「视觉：本次识别没有验证出来」，
复选框保持关闭。

### 1.1 这个部署实际走的是哪条路

截图里出现了「推理」这一行，而能力集是按 profile 上限渲染的：

```go
mantleOpenAIChatSet = ProviderCapabilities{
    Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true,
    DeveloperRole: true, Reasoning: true, StreamUsage: true,
}
mantleOpenAIResponsesSet = ProviderCapabilities{ /* 同上，但不含 Reasoning */ }
```
（`internal/domain/provider_table.go:264-271`）

含推理 ⇒ 这是 **chat profile**。而 chat profile 的适配器不是 `bedrockmantle`：

```go
case domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleOpenAIChat:
    adapter, err = openaiprovider.NewWithOptions(...)
case domain.ProfileBedrockMantleResponses, domain.ProfileBedrockMantleOpenAIResponses:
    adapter, err = bedrockmantleprovider.NewResponses(...)
case domain.ProfileBedrockMantleAnthropicMessages:
    adapter, err = anthropicprovider.New(...)
```
（`internal/app/providers.go:719-741`）

所以这次探针是由 **OpenAI 适配器**发出的，而 OpenAI 适配器一直都在填 `ProviderCode`。
第一版把成因归到「Mantle 没填 `ProviderCode`」，方向错了。

### 1.2 真正的断点：拼接串对不上精确匹配

`internal/provider/capability_detection.go:240-246` 是唯一把上游标识变成判决的地方：

```go
case ErrorBadRequest:
    // Only a structured, reviewed provider code is accepted as a stable
    // unsupported verdict. Free-form error text is never inspected.
    if strings.EqualFold(providerError.ProviderCode, "unsupported_parameter") || strings.EqualFold(providerError.ProviderCode, "unsupported_value") {
        return domain.ProbeUnsupported
    }
    return domain.ProbeInconclusive
```

它比的是**整串**。而 OpenAI 适配器把被拒的参数名拼进了同一个字段：

```go
if param := strings.TrimSpace(pointerValue(envelope.Error.Param)); param != "" && refusal.Code != "" {
    refusal.Code += ":" + param
}
```
（`internal/provider/openai/adapter.go:661-664`；`bedrockmantle/adapter.go:352-365` 的
`refusalCode` 是照它写的，行为相同）

实测输出：

| `ProviderCode` | 分类结果 |
| --- | --- |
| `unsupported_parameter` | `unsupported` |
| `unsupported_parameter:image_url` | **`inconclusive`** |
| `unsupported_value:input_image` | **`inconclusive`** |
| `invalid_request_error:image_url` | `inconclusive` |

**只要上游说了是哪个参数被拒 —— OpenAI 形状的错误体总会说 —— `ProbeUnsupported`
这条分支就不可达。** 这不是 Mantle 专有，是整个 OpenAI 家族（OpenAI 本体、
OpenAI 兼容端点、Mantle chat、Mantle Responses）共有的既有缺陷。

后果：这些接口上的探针，凡是被上游以 400 拒绝的，一律落成「没有得出结论」，
既拿不到「明确不支持」的确定答案，也没有任何线索说明为什么。
截图里 视觉 / 工具调用 / 推理 同时「没有得出结论」不是三次巧合，是同一处比较。

### 1.3 探针在 Halro 这一侧是好的

把五种探针载荷走一遍 Mantle Responses 适配器的渲染路径，用桩传输截下线上字节：

| 探针 | 渲染结果 |
| --- | --- |
| `minimal_chat` | `{"model":"openai.gpt-5.6-luna","input":[…input_text…],"max_output_tokens":16,"store":false}` |
| `inline_image` | 同上，且携带 `{"type":"input_image","image_url":"data:image/png;base64,…"}` |
| `tool_call` | 同上，且携带 `tools` 与 `tool_choice` |
| `json_object` | 同上，且携带 `text.format.type=json_object` |
| `developer_message` | 同上，且首条消息 `role: "developer"` |

五种全部渲染正确、全部到达线上，没有一条在 Halro 内部失败。**图片确实被发出去了。**

必须声明这次实测的边界：**它跑的是 Responses 适配器，不是用户实际走的 chat 路径**。
chat 路径的等价实测尚未做，是第 11.8 节列出的待补验证之一。

### 1.4 元数据来源的真实分布

第一版的适配器表有误，更正如下：

| 适配器 | 枚举 | 元数据内容 | 能否推出 `vision` |
| --- | --- | --- | --- |
| `provider/bedrock/models.go:65,129` | 有 | `inputModalities`、`outputModalities`、`inferenceTypes` | **能**（`:139-143`，`IMAGE+TEXT → vision`） |
| `provider/anthropic/adapter.go:137,180-189` | 有 | `SupportedOperations`、上下文/输出上限 | **能**（`:228-246`，`image_input → vision`，另有 `thinking → reasoning`、`structured_outputs → json_mode`） |
| `provider/gemini/adapter.go:128` | 有 | `inputTokenLimit`、`outputTokenLimit`、`supportedGenerationMethods` | 否 |
| `provider/openai/adapter.go:179,206-222` | 有 | 仅 `id`、`owned_by`（`MetadataSourceNone`） | 否 |
| `provider/bedrockmantle`（仅 Responses profile） | **无** | **无** | 否 |

所以「只有 Bedrock 原生提供模态元数据」是第一版的过度悲观说法：**Bedrock 原生与
Anthropic 都能产出 declared 级的 vision 证据**，Gemini 有规格但无模态，
OpenAI 家族（含 Mantle chat）只有标识。

### 1.5 目录是账号级的，枚举**故意**不套路由前缀

`modelCatalogURL()` 不套 `operationPathPrefix`，这不是缺陷，是实测之后的决定。
本文曾把它当成"新发现的缺陷"并要求与 F4 一起修；那次修改上线后，两个
`/openai/v1` 的 Mantle profile 上的每一次模型枚举与连接测试都答 404，于是被撤销
（`0c886fa`）。2026-08-25 对真实账号（us-east-2）的实测：

```
GET /v1/models                        200
GET /openai/v1/models                 404
GET /v1/models/openai.gpt-5.5         200
GET /openai/v1/models/openai.gpt-5.5  404
```

最后一行是关键：`gpt-5.5` 由 `/openai/v1` 服务，在那条路由下依然没有目录条目。
**目录是账号级的，推理是路由级的**，两个地址本就不该统一。理由现在写在
`internal/provider/openai/adapter.go:151` 的注释里，两个测试
（`TestModelCatalogIsNotScopedToTheOperationRoute`、
`TestPinnedRouteCarriesOperationsWhileCatalogStaysOnV1`）断言的正是两者**不一致**。

记在这里是因为它是本文第四次诊断被推翻，而前三次都记在开头的修订记录里。

### 1.6 顺带记录的既有缺口

- **`reasoning` 没有任何探针。** `CapabilityDetectionPlan` 有 chat / streaming /
  stream_usage / tools / json_mode / developer_role / vision / embeddings / moderations /
  rerank 十个分支，没有 `c.Reasoning`。
- **探针上限是 8**（`capability_detection.go:66` 分配、`:68` 强制）。`mantleOpenAIChatSet`
  的 7 项恰好排满其中 7 个，加推理正好到顶。注意 `mantleOpenAIResponsesSet`
  **本就不含 `Reasoning`**（`provider_table.go:268-271`，且被
  `internal/domain/capability_ceiling_test.go:21` 钉住），所以给推理加探针对
  Responses profile 毫无作用。
- **真正的预算比 8 更紧**：`MaxProviderCalls` 是操作者配置的总额，上限
  `MaxDetectionProviderCalls = 12`（`internal/domain/model_capability_detection.go:20`），
  接口识别先从里面花，花完之后剩下的探针被静默降级成 `ProbeNotProbed`
  （`internal/app/admin_model_capability_detections.go:453-454`）。

**结论：改 UI 解决不了这个问题**，第 11 节是可执行的修改清单。

---

## 2. 目标形态

对齐 AWS 控制台的三件事：

1. **模型以卡片网格呈现**，卡片上带一行能力图标（`T 图 → T`）、上下文窗口。
2. **点击卡片，右侧滑出详情面板**，展示模型标识、上下文窗口、最大输出，
   以及「输入」「输出」两组模态清单，每行带状态。
3. **面板是动作入口**：已有部署 → 编辑；没有部署 → 创建部署。

第 7 节会说明：这三件事都排在止痛之后，且其中一件建议不做。

---

## 3. 核心设计决定：AWS 的「模型」不是 Halro 的「部署」

AWS 那个页面只有一层对象：**上游模型**（只读、由服务商目录给出）。Halro 有两层：

| 层 | 对象 | 来源 | 可变性 |
| --- | --- | --- | --- |
| 上游模型 | `ResolvedInvocationTarget` | 服务商目录（`api.invocationTargets`） | 只读 |
| 部署 | `Deployment` | Halro 自己的记录 | 可创建/编辑/停用/定价/测试 |

一个上游模型可以对应 0..N 个部署（不同能力子集、不同并发上限、不同价格版本、
不同接口绑定）。部署还带着 AWS 那个页面完全没有的东西：启用状态、测试状态、
能力漂移复核状态、价格版本、路由依赖。

**不合并这两层**：合并会让"同一个上游模型的两个部署"没有独立的卡片，
而这是路由回退依赖的真实场景。

### 3.1 修正：不新建「目录中可添加」分段

第一版提出在部署页加第二个分段，罗列该服务商目录里尚无部署的模型。**撤销这个提议**，
三条理由：

1. **它重复了一个已经存在、且已经能扩展的控件。** `DeploymentsPage.tsx:1302-1306`
   与 `:1486-1525` 已经把服务商目录渲染成一个带过滤的组合框：`display_name` 子串匹配、
   实时计数、刷新按钮、`role="listbox"` 键盘导航、降级绑定提示。47 个模型能用，
   200 个也能用，因为输入即收窄。
2. **卡片网格在同一规模下不能用。** 它会在操作者真正管理的 5–10 个对象周围铺开
   190 张不可操作的卡片，而这些卡片唯一的动作是「创建部署」—— 正是操作者点进
   创建流程后才会用到的那个按钮。
3. **它会让页面加载本身发起上游调用。** 目录 GET 是按服务商的
   （`web/src/api.ts:313`），而部署页不是按服务商组织的；冷缓存的 GET 会直接落到
   `lister.ListInvocationTargets`（`internal/app/admin_invocation_targets.go:357`），
   缓存未命中是穿透而不是拒绝。这条 GET 只要求 `requireAdmin`，不校验角色
   （`internal/app/runtime.go:1446`），而 `AdminRoleReadOnly` 是真实存在的角色 ——
   于是一个只读会话打开页面，就替操作者花掉了每个服务商的一次上游枚举。
   今天这不成立，只因为前端把该查询限制在创建表单内
   （`DeploymentsPage.tsx:1034-1039`，`enabled: providerID && !identityLocked`）。

**替代做法**：模态事实要在选模型时可见，就画进现有组合框的选项行，不新开页面分段。
同时建议 `listAdminInvocationTargets` 改成只服务缓存 —— 冷的时候返回"未缓存，请刷新"
而不是拨号，这样只读会话永远无法发起一次服务商调用。

这条同时回答了第一版 §10 的问题一。

---

## 4. 能力 → 模态矩阵的映射

Halro 的能力是**操作 / 特性布尔**，AWS 是**输入模态 / 输出模态**。两者不同构。

### 4.0 先说清楚它不是什么

服务端一共提供 19 个能力（`DeploymentsPage.tsx:938-943` 的四组）。其中只有 8 个
在模态上有表达。`streaming`、`tools`、`json_mode`、`developer_role`、`reasoning`、
`stream_usage`、`provider_executed_tools`、`files`、`batches` 全部没有 ——
两个只差 tools / reasoning 的部署会渲染成一模一样的卡片。

**所以模态矩阵是一个模态视图，不是能力摘要。** 卡片上必须同时保留能力计数或徽章条
（今天展开行已经在做，`DeploymentsPage.tsx:384-389`），否则图标行会被误读成
"这个部署的全部能力"。

### 4.1 映射表（已按评审修正）

| 面板行 | 判定来源 | 说明 |
| --- | --- | --- |
| 输入 · Text | `operations` 组任一为真 | 不能只列 `chat`/`embeddings`：`images`（文生图）、`speech`（文生音）、`moderations`、`rerank` 全是文本输入操作，漏掉它们会让一个 Titan Image 部署渲染成"没有文本输入"，那是自信的错，比"未知"更糟 |
| 输入 · Image | `vision` | |
| 输入 · Image（由网关取回） | `fetched_image` | 与 `vision` 不同 —— 前者是 Halro 代取 URL，单独一行，不合并 |
| 输入 · Audio | `transcriptions` | |
| 输入 · Video | 无对应能力 | 恒为"未知"，不画叉 |
| 输出 · Text | `chat` | |
| 输出 · Image | `images` | |
| 输出 · Audio | `speech` | |
| 输出 · Embedding | `embeddings` | |

第一版有两行是错的，已删除：

- **删「输入 · Speech」**。`Speech` 是 TTS（文本进、音频出，`internal/domain/models.go:460`），
  它根本不是一种输入模态，标成"永远未知"是把类别错误固化下来。
- **删「输出 · Video ← `async_generate`」**。`AsyncGenerate` 是异步调用操作
  （`internal/domain/models.go:464`，绑定 `PrimitiveBedrockAsyncNovaReel`，
  `internal/provider/profile.go:445`），它不表示视频。第一版自己加的"先标未知"
  已经让这行没有任何作用。

### 4.2 映射放在服务端，不放在前端

第一版建议建 `web/src/modalityMatrix.ts`。**改为放在 `internal/domain` 并由 API 提供。**

理由：反向的那半边映射已经在 Go 侧了 —— `bedrock.MapCapabilityClaims`
（`internal/provider/bedrock/models.go:137-150`）把 `IMAGE+TEXT` 变成 `vision`，
`anthropic.MapCapabilityClaims`（`adapter.go:228-246`）把 `image_input` 变成 `vision`，
两者都经 `modelcatalog.Merge` / `Clamp` 合并（`internal/app/admin_invocation_targets.go:492-516`）。
在 TS 里再写一份正向映射，就是同一份知识的第二个来源 ——
这正是 §4.1 自己警告的那种分叉。

配套要求：单测断言 `modelcatalog.CapabilityNames` 里每一个名字，要么被映射，
要么在一份显式的"非模态"清单里，两者必居其一。

### 4.3 状态词表：复用既有的，不新造

第一版提出五态，并在第 11 节又扩成六态 —— 两张表互相矛盾，且都是新造的。
**撤销新词表。** 服务端早就有一份完整的：

```ts
export type CapabilityProbeStatus = "supported" | "unsupported" | "inconclusive"
  | "unavailable" | "unauthorized" | "not_probed" | "canceled";
```
（`web/src/types.ts:584`，服务端 `internal/domain/model_capability_detection.go:44-56`，
`Valid()` 在 `:58-61`）

七个值都已有中英文案（`web/src/i18n/locales/zh-CN.ts:801`、`en-US.ts:802`）。
第一版的五态表会把 `unavailable`（上游不可达）和 `unauthorized`（凭据被拒）
这两个**可行动且互不相同**的结果压没地方放。

`not_probed` 也**早就与 `inconclusive` 分开**：服务端给每个计划外的能力写入
`not_probed` + `probe_kind: "risk_policy"`（`internal/app/admin_model_capability_detections.go:748`），
前端在横幅过滤里已经正确使用了这个区分（`DeploymentsPage.tsx:1394-1399`）。
所以"把未探测与探了没结论区分开"不是待办事项 —— 见第 7 节 P0b。

### 4.4 呈现方式：每行带文字，取消常驻图例

第一版要求"形状 + 文本标签 + 常驻图例"。七个状态各配一对图形与词，
在 `--font-size-xs`（排版下限，`web/src/design-system.test.ts:204-209`）下会占掉
2–3 行中文，压在一个还要放标识、上下文窗口、约 10 行模态的面板上方。

**改为：状态词直接写在每一行上**（「已验证」「无法确认」「未探测」），
不设图例。图形退化成冗余而不是信号，颜色不作唯一区分的要求自动满足，
而且扩到七个状态也不需要图例。仓库已有同形状的先例（`DeploymentsPage.tsx:1697-1701`）。

---

## 5. 数据缺口清单

已关闭的条目就地删除，不留删除线：D1（Mantle Responses 枚举，F4）、D6（`reasoning`
探针，F6）、D8（枚举路由，见 §1.5，是被撤销而不是被修复）、D10（探针健康入 Admin
API，§14.17）。

| # | 缺口 | 位置 | 影响 |
| --- | --- | --- | --- |
| D2 | OpenAI 家族目录只有标识，无模态 | `provider/openai/adapter.go:206-222` | 这条路上模态行只能是"未知"（**注意 Anthropic 不在此列**，见 §1.4） |
| D3 | 无模型描述文案 | 全链路 | AWS 抽屉那段简介无对应数据 |
| D4 | 无服务商 logo | 全链路 | 卡片左上角图标无对应数据 |
| D5 | 目录层无价格 | 价格挂在部署上 | 目录卡片显示不了参考价 |
| **D7** | **部署不留存探针级结果** | `web/src/types.ts:465`、`:333` | 见下 |

### D7 —— 抽屉的完整状态矩阵在部署上无源可取

这是评审发现的、第一版完全遗漏的一条，且它决定了第 2 节能不能实现。

一个已保存的 `Deployment` 只带三档证据：

```ts
capability_evidence: Record<string, "verified" | "declared" | "unsupported">
```
（`web/src/types.ts:333`、`:465`；控制台在做摘要时还会丢弃 `unsupported`，
`DeploymentsPage.tsx:925-928`）

七值的 `CapabilityProbeStatus` **只存在于 `ModelCapabilityDetection` 上**，
按 detection id 拉取（`web/src/api.ts:337`），且只在创建流程里出现。
**没有任何按部署返回探针级结果的端点。**

所以"点部署卡片 → 面板展开完整状态矩阵"按第一版写法不可实现。两条路选一：

- 在 Deployment 上留存 detection 引用并提供端点；或
- 把完整矩阵限定在创建/识别流程，部署面板只给现有的三档证据。

后者便宜得多，建议先走后者。

### 关于 D3 / D4 —— 不做

模型简介文案和厂商 logo 是 AWS 页面的内容，抄过来是把第三方文案与商标搬进一个
自托管产品，且没有任何数据源能让它随上游更新。标题只用 `display_name` + `owned_by`
（`InvocationTargetDescriptor` 已有）。

### 关于 D5 —— 说清楚这不是设计收益

目录级价格 Halro 拿不到。用「未定价 / 已定价 · 生效中」替代 —— 但要如实说明：
这个状态**今天就已经在行里**（`DeploymentsPage.tsx:210-224`），所以这笔交换是
"拿不到参考价，继续显示已经在显示的东西"，不是新增价值。

---

## 6. UI 结构与实现要点

### 6.1 抽屉：先定模态性，再谈复用

第一版说"复用 `Modal` 已有的焦点陷阱、ESC、`aria-modal`、滚动锁定"。三处需要更正：

1. **路径错了。** `Modal` 在 `web/src/components.tsx:257`；`web/src/design-system/`
   目录里只有 CSS。
2. **滚动锁定不存在。** 整个控制台都没有：`.modal-backdrop` 是 `position:fixed`
   （`web/src/styles.css:1489`），没有任何代码碰 `document.body` 或 `inert`。
3. **焦点陷阱不可直接复用。** 它是 Modal 函数体内的一个 `useEffect`
   （`components.tsx:307-335`），闭包捕获 `dialog`、`closeDisabledRef`、
   `requestCloseRef`、`confirmingRef`，并硬编码 `[data-modal-initial]`（`:312`）
   与 `.discard-prompt`（`:322`）。抽出来是一次 Modal 重构，不是一次调用。

**更根本的矛盾**：`aria-modal="true"`（`:341`）和文档级 Tab 循环（`:315-329`）
与第一版自己写的"可与列表同时可见、不阻塞"直接冲突 —— `aria-modal` 会把列表
从辅助技术里隐藏，Tab 陷阱会阻止走到列表上。

**结论：先定模态性。** 若定为非阻塞，则用 `role="region"` / `complementary`
+ `aria-labelledby`，**不要** `aria-modal`，**不要** Tab 陷阱，只保留 ESC 关闭
与焦点归还；把这两件共用的东西抽成 `useDismissable()`，让 Modal 也用它，
避免出现第二份会漂移的实现。

### 6.2 卡片必须保留的两件事

现有行携带：启用点 + 名称 + 服务商（`DeploymentsPage.tsx:241-244`）、上游模型（`:245-248`）、
并发（`:249-252`）、路由依赖（`:253-258`）、价格状态（`:259-288`）、
启用/停用文字 + **能力复核状态**（`:289-299`）、六个动作（`:300-325`）、
行内测试失败原因（`:329-331`）。

第一版只保留四项，其中两项丢失是承重的：

1. **`capability_review.state`**。代码里就有一句写死的理由：
   「A drifted deployment is not routing whatever the enabled flag says, so the
   state that decides that has to be visible in the row.」（`:292-298`）
   卡片保留了会说谎的那个标志，却丢掉了纠正它的那个 —— 必须补回卡片，
   而且它的优先级高于图标行。
2. **`activeRouteCount`**。它是「停用」（`:315`）和「删除」（`:323`）两个动作的
   `disabledReason`。移进抽屉后，一个被禁用的控件的理由离它一次点击 ——
   违反仓库自己断言的规则（`web/src/pages/readOnlyRole.test.tsx:314-316`）。

另有两处无障碍问题：

- `StatusDot` 是 `aria-hidden`，不传 `label` 就不发文字（`components.tsx:34-41`），
  而部署行没传（`DeploymentsPage.tsx:242`），全靠旁边的 `resource-state` 文字（`:291`）
  —— 而那正是第一版要删的。删了就成了纯颜色区分，违反 §4.4 自己的要求。
- 「卡片整体可点」不可实现成 `button` 或 `role="button"`：卡片内要装价格链接、
  启停、编辑、内联测试控件和溢出菜单，`button` 不允许嵌交互后代，
  `role="button"` 会覆盖 `article` 语义。`stopPropagation` 只解决鼠标。
  **保留 `article` 与它的 id**（那个 id 是活的深链目标，`components.tsx:171`），
  把打开抽屉做成一个明确的按钮 —— 也就是把现有的 `deployment-expand`（`:307-311`）
  改成打开抽屉即可。

### 6.3 抽屉不是只读的

第一版写「Drawer = 只读详情」。它要替代的展开面板并不只读：取消排期中的价格版本
（`DeploymentsPage.tsx:376-381`）、价格隔离后的恢复确认（`:359`）、打开价格表单（`:374`）、
重试失败的价格读取（`:360-364`）。

必须写明：价格动作在抽屉里，各自唤起既有 Modal（`PriceVersionForm`、
`RestorePricingConfirm`），并明确失效策略 —— 一个非阻塞抽屉在上方 Modal 修改
同一份价格状态时会陈旧，这是现有的行没有的路径。

### 6.4 能力图标行：两态会重演本文开头那个误导

第一版说卡片只画"已验证"和"已声明"。那样一来，视觉被明确判为 `unsupported` 的模型，
和从未被探测过的模型，**在卡片上长得完全一样**（都没有图标），而"没有图标"
会被每个操作者读成"不支持"—— 正是第 1 节开头那个误读。这比 §4.3 拒绝的两态显示更糟，
因为至少一个显式的叉是明说的。

**改为**：只要该模态被宣称过就画图标，用笔画区分 —— 实心=已验证，
描边=已声明，描边加问号=无法确认/未探测，只有 `unsupported` 不画，
配 §6.5 要求的逐图标 `aria-label`。若卡片放不下，就整条去掉图标行，
改显示「N 项已验证 · M 项未确立」—— 至少不是错的。

另：`T 图 → T` 如果用真 emoji，会是控制台的第一处 UI emoji ——
无法 token 化、无法主题化、无法做对比度检查、各系统渲染不同。用 CSS/SVG 图形。

### 6.5 无障碍与设计系统的既有约束

- 逐图标 `aria-label`（如「输入：图像，已验证」）。这是一条带插值的独立文案键，
  不是拼接。
- `web/src/design-system.test.ts` 拒绝色值字面量与设计系统外的原始 token 使用（`:270`）、
  对没有样式的类名报错（`:335`）、拒绝六级字号之外或低于下限的业务字号（`:204-236`）。
  七个状态需要的语义 token 目前**不存在** —— 没有"无法确认"色系，
  最近的先例是 `--color-status-warning-text`（`styles.css:504`）。
- 每一个新增的卡片/抽屉类名必须在同一次改动里配好样式。

### 6.6 页面既有的两个约束，方案必须承认

- **这个页面不用 `ResourceToolbar`。** 它自带工具条（`DeploymentsPage.tsx:85-100`），
  因为需要第四个状态值 `attention`，而 `ResourceStatusFilter` 表达不了
  （`components.tsx:519`、`:542`）。`attention` 是按测试时效定义的
  （`DeploymentsPage.tsx:61-65`）—— 也就是第一版要从卡片上删掉的那个事实。
- **这个页面不用 `LoadMore`。** `api.deployments()` 只读一页并丢弃 `next_cursor`
  （`web/src/api.ts:360-361`）。也就是说页面**今天就在静默截断**。
  卡片网格不会让它变好；再加第二个无界列表会让它变差。

### 6.7 i18n

zh-CN 与 en-US 严格等键（`web/src/i18n/i18n.test.tsx:18`）。实际需要约 35–45 个键，
不是第一版估的那点：6 个模态名；状态标签**复用** `deployments.detectionProbeStatus`
（`zh-CN.ts:801`）而不是另起一套；模态加状态的插值 `aria-label` 模板；
`deployments.evidenceValues` 今天只有 `verified` 与 `declared` 两个（`zh-CN.ts:764`），
要补；「该接口不提供模态信息」与「未知」行文案；抽屉地标标签；「从目录创建部署」。

两条流程约束：locale 审计会对任何 `web/src` 里没人读的键报错（`i18n.test.tsx:151`），
所以 i18n 不能作为独立阶段先落地；中文文案必须用全角标点且不得含反引号
（`i18n.test.tsx:64`、`:82`）。

### 6.8 卡片会让部署页脱离共享契约

`resource-list.css` 是设计系统契约，服务商、凭据、部署三个列表共同渲染，
规则是"页面只拥有自己的列"（`web/src/design-system/resource-list.css:1-39`、
`design-system/README.md:62-92`）。改成卡片，部署页就成了唯一脱离该契约的列表 ——
正是那个文件存在的目的所要防止的漂移。

要做就把卡片声明成一个新的设计系统契约（`resource-card.css`）供其他列表采用；
不做，则 §7 的 P3 应当重新考虑。

---

## 7. 分期（已按评审重排）

**第一版的重心放错了。** 真正的止痛是三件小事，其余是一次控制台重设计，
而本文 §1 自己承认它「解决不了这个问题」、§9 又承认它在多数服务商上会画成一片"未知"。

| 期 | 内容 | 交付判据 |
| --- | --- | --- |
| ~~**P0a**~~ | 修 `code:param` 与精确匹配的比较（§11 F0） | **已完成** —— `TestCapabilityDetectorReadsTheCodeHalfOfAJoinedRefusal` |
| ~~**P0b**~~ | 前端 `capabilityUnavailableReason` 补 `not_probed` 分支加一条文案 | **已完成** —— 新增 `deployments.detectionNotProbed` |
| ~~**P0c**~~ | 在「验证没有得出结论」横幅里渲染已有的 `error_class` | **已完成** —— 复用既有文案键，零新增 |
| ~~P1a~~ | F2b：`provider_status` 与 `provider_code` 落到探针结果（**先收敛后持久化**） | **已完成** —— 收敛统一到 `provider.SafeProviderIdentifier` |
| P1b | `modelCatalogURL` 套上 `operationPathPrefix` | **已撤销** —— 实测证明目录是账号级，套前缀让每次枚举与连接测试答 404（`0c886fa`）。见 §1.5 |
| ~~P1c~~ | F4：Mantle Responses profile 加 `ListInvocationTargets` | **已完成** —— 断言了不实现 `ProviderMetadataMapper`，条目上限 2000 |
| ~~P2a~~ | 抽屉组件 | **已完成** —— 那句「不做」当天即被推翻，抽屉见 §14；添加/编辑仍用现有弹窗 |
| ~~P2b~~ | 模态映射放进 `internal/domain` 加单测 | **已完成** —— `capability_modality.go`，经服务商 profiles 端点下发，golden 已更新 |
| ~~P3~~ | 卡片网格 | **已完成** —— 新增 `resource-card.css` 契约，漂移/路由/价格/状态词全部保留在卡片上 |
| 不做 | 「目录中可添加」分段 | 见 §3.1 |
| ~~待定~~ | F6 已完成；F5 受阻不做 | 见 §11 |

**P0a 加 P0b 加 P0c 合计约两天，交付全部已陈述的止痛。** 抽屉与卡片当时被判为
「另一件事，建议独立文档重开」；实际上两者都在 08-24/08-25 就地做完了，过程记在
§13 与 §14。这张表只说这一期的三件止痛，**不要拿它当全文的进度表读** —— §15 的
P0–P3 才是今天的下一步。

---

## 8. 明确不做

- 不抄 AWS 的模型简介文案与厂商 logo（§5 D3/D4）。
- 不新造状态词表；复用 `CapabilityProbeStatus` 的七个值（§4.3）。
- 不把部署表单搬进抽屉（§6.1）。
- 不为了填满面板而猜模态。缺失就画"未知"。
- 不建「目录中可添加」分段（§3.1）。
- **抽屉不得渲染上游那句话。** `describeProbeFailure` 会把它作为 `error_detail`
  送到控制台（`internal/app/admin_providers.go:611`、`:681`），是个现成的诱惑；
  面板只展示 `provider_status` 与 `provider_code`。
- **展开任何目录视图都不得发起上游调用**（§3.1）。
- **上游可控的展示字符串必须有渲染上限。** 已落地（2026-08-26，见 §16.5）：条目数超限
  拒绝而非截断，`DisplayName` / `OwnedBy` 超长只丢标签不丢模型，全部在
  `normalizeInvocationTargets` 这一个交汇点执行。（`Region` 不在此列 —— 它在任何适配器里
  都不是上游可控量。）

---

## 9. 风险

| 风险 | 说明 | 应对 |
| --- | --- | --- |
| 面板大面积"未知" | Bedrock 原生与 Anthropic 有模态证据，Gemini 与 OpenAI 家族没有（§1.4） | 无元数据时抽屉直接说明"该接口不提供模态信息"，不画一列灰点 |
| 卡片信息密度下降 | 现有行有五个事实字段 | 漂移状态、路由计数、价格状态、测试时效必须留在卡片（§6.2、§6.6） |
| Mantle ID 映射猜错 | 强行对应会产生错误的 declared 证据 | 见 §11 F5，不满足前置条件就不做 |
| 探针预算 | 上限 8，且实际受 `MaxProviderCalls`（不超过 12）挤压 | 任何新增探针前先核算（§1.6） |
| 只读会话触发计费枚举 | 冷缓存 GET 会穿透到上游 | §3.1：目录只服务缓存 |
| 页面已在静默截断 | `next_cursor` 被丢弃（§6.6） | 卡片网格落地前先补分页或明确计数 |

---

## 10. 待确认（已替换为真正开放的问题）

第一版问的三个（目录段展开与否、抽屉宽度、是否保留行内展开）已经有答案：
第一个见 §3.1（不建该分段）；第二个用固定宽度，因为仓库没有任何客户端 UI 状态持久化
（`web/src/theme.ts:9-10` 写明了这条边界），可拖拽会在每次导航后忘记；
第三个是"抽屉确实承载了 `CapabilityReviewNotice`、价格隔离提示与价格版本时间线之后
才移除"，因为「已配置」价格单元格唯一的动作就是 `setExpanded(true)`（`:264-269`），
展开状态还决定该行的价格查询能否插队（`:154`）。

真正开放的四个：

1. **抽屉如何呈现一个 `drifted` 的部署？** 它不在路由，无论启用标志说什么。
2. **重新识别会不会覆盖操作者手动声明的能力？** 创建流程里
   `resetDetection()` 与 `setManualDeclaration(true)` 成对出现
   （`DeploymentsPage.tsx:1649`、`:1655`），暗示两条路互斥，但没有任何地方写明谁赢。
3. **推理探针到底加不加？** 受 §1.6 的预算约束，且对 Responses profile 无效。
   这是排期决定，不是实现细节。
4. **模态映射与既有的 `deploymentCapabilityGroups`（`DeploymentsPage.tsx:938-943`，
   已有 `modalities` 组且被测试钉住）如何避免分叉？** §4.2 给了方向，
   但两者的边界需要写死。

---

## 11. 「视觉未识别」的修改清单

### 11.1 修改项

| # | 项 | 状态 | 文件 |
| --- | --- | --- | --- |
| **F0** | **`code:param` 与精确匹配的比较** | **已完成** | `provider/capability_detection.go:240-256` |
| F1 | Mantle Responses 保留上游 refusal 标识 | 已完成（但不解释本次症状） | `provider/bedrockmantle/adapter.go` |
| F2a | 横幅里渲染已有的 `error_class` | **已完成** | `DeploymentsPage.tsx` 未确立横幅 |
| F2b | 探针结果落 `provider_status` 与 `provider_code` | **已完成** | `domain/model_capability_detection.go`、`provider/capability_detection.go` |
| F3 | 区分「拒绝但没解析出原因」与「答了但断言不成立」 | **已完成** —— 新状态 `assertion_failed` | `provider/capability_detection.go` |
| F4 | Mantle Responses profile 加 `ListInvocationTargets` | **已完成** | `provider/bedrockmantle/adapter.go` |
| F5 | Mantle 模态元数据来源 | **受阻，不做** —— 见 11.6 | 见该节 |
| F6 | `reasoning` 探针 | **已完成**（判据取响应字段法，探针上限未动） | `provider/capability_detection.go` |
| F7 | 前端 `not_probed` 分支 | **已完成** | `DeploymentsPage.tsx` `capabilityUnavailableReason` |

### 11.2 F0 —— 本次症状的解药（已完成）

落地形态：比较前用 `strings.Cut` 取冒号左侧的 code 半段，参数半段照旧随行。
同时把 `CapabilityDetectorContractVersion` 从 `capability-detector-v1` 提到 `v2` ——
它是识别选择指纹的输入之一（`app/admin_model_capability_detections.go:147-151`），
所以旧结果不会在新语义下被静默复用；这不是相等性闸门，旧记录只是不再命中指纹，
不会被拒绝。

`classifyCapabilityProbeError` 比的是整串，而适配器把 param 拼了进去（§1.2）。
改法：在比较前按第一个冒号拆开，只比 code 半段；或改成前缀匹配。

**必须配一个端到端的测试**：驱动一个带 `code` 与 `param` 的 400，
断言它一路走到 `ProbeUnsupported`。现有的
`TestResponsesAdapterKeepsTheRefusedParameterInTheIdentifier`
只断言了拼接串本身，从不断言分类结果 —— 这就是为什么它的反向验证通过了，
而功能其实没生效。这条教训要写进测试注释。

影响面：OpenAI 本体、OpenAI 兼容端点、Mantle chat、Mantle Responses，
四条路一起恢复。

### 11.3 F1 —— 已完成，但要如实标注它做了什么

`decodeHTTPError` 增加 `refusalCode(envelope)`：取 `code`，为空回退 `type`，
再把 `param` 以 `code:param` 拼进 `ProviderCode`，形状与
`provider/openai/adapter.go:643-668` 一致。

**它对本次症状没有作用**，因为用户走的是 chat profile（§1.1），
那条路的 `ProviderCode` 一直都有。它修的是 Responses profile 上"连 code 都没有"
这个真实缺口。而且在 F0 落地之前，它填进去的拼接串同样无法通过分类判据。

三件必须一并记下的事：

1. **`refusalCode` 目前没有长度与字符集限制**，而 `decodeHTTPError` 读到 1 MiB 的
   错误体（`bedrockmantle/adapter.go:323`，对比 OpenAI 的 4096，`openai/adapter.go:643`）。
   SSE 路径更直接：`ProviderCode: event.Code` 未经任何处理（`bedrockmantle/adapter.go:315`）。
   这个值会被原样追加进网关日志（`internal/gateway/service.go:623-624`）。
2. 仓库已有两个现成的收敛器，Mantle 两个都没用：`safeProviderCode`
   （128 字节、`[A-Za-z0-9._:-]`、越界丢弃，`provider/bedrock/adapter.go:860-875`）
   与 `probeIdentifier`（同字符集，`app/admin_providers.go:626-645`）。
   **应当在采集点（适配器内）收敛**，让所有下游消费者继承，而不是各自再推导一遍。
3. F1 让一条既有映射在 Mantle 上变得可达：`ProviderCode == "overloaded_error"`
   会让 Anthropic 门面无视上游状态码直接返回 HTTP 529
   （`internal/gatewayapi/handler.go:692`）。这是一个上游可控字符串在左右响应状态码。
   建议把该映射收紧为只看 `StatusCode == 529`。

### 11.4 F2 —— 拆成两步

**F2a（前端，先做）**：`error_class` 其实**已经在响应里**（`web/src/types.ts:618`），
UI 也已经会把它渲染成人类可读文本 —— 但只在 `detection.status === "failed"` 时
（`DeploymentsPage.tsx:1678`、`:1699`）。一个 `completed` 但有 inconclusive 项的识别，
操作者只看到光秃秃的「验证没有得出结论」，而原因就躺在同一个响应对象里。
把它渲染进 `detectionUnestablishedTitle` 横幅，零 schema 改动。

**F2b（服务端）**：`domain.CapabilityProbeResult` 目前只有
`Status`、`Evidence`、`ErrorClass`、`BindingID`、`ProbeKind`、`StartedAt`、`CompletedAt`。
新增两个 `omitempty` 字段并透传：

```go
ProviderStatus int    `json:"provider_status,omitempty"`
ProviderCode   string `json:"provider_code,omitempty"`
```

**只取标识与状态码，永不取 `Message`** —— 那是上游响应体。这条要写成禁令，
因为 §8 提到的 `describeProbeFailure` 是一个现成的反向先例。

四条硬性要求：

1. **先收敛再持久化。** 今天 `ProviderCode` 从不落库 —— 连接测试只存一个 class
   （`admin_providers.go:516`），且注释明说这些字段"只在响应与日志里流动，不持久化"（`:555`）。
   F2b 会让它成为**第一个进入 bbolt、Admin API 和备份的上游可控字符串**
   （`CapabilityProbeResult` 经 `ModelCapabilityDetection.Results` 落
   `bucketModelCapabilityDetections`）。必须走 `safeProviderCode` 等价物。
2. `ProviderStatus` 只接受 100 到 599，为 0 时省略；两者都要进
   `ModelCapabilityDetection.Validate`。
3. **不需要 bump `schemaVersion`**：两个 `omitempty` 字段是纯增量，旧记录以零值反序列化，
   `Validate()` 不要求它们，没有任何地方按位置读存量字节。也就**不需要重新初始化数据目录**。
4. **要 bump `CapabilityDetectorContractVersion`**（`provider/capability_detection.go`）：
   F0、F2b、F3 改变了探针语义，而 `DetectorVersion` 是存量结果里唯一标明
   "由哪个探测器产出"的标记。落地时 F0 与 F2b 同属一次未发布的改动窗口，
   共用一次 `v1 → v2` 的提升；F2b 单独看只是多记了两个字段，不改判决，
   已存在的 v2 结果缺这两个字段也无害（`omitempty`，零值反序列化）。
   F3 落地时再评估是否需要 `v3`。

### 11.5 F3 —— 两种 inconclusive 要分开

`DetectCapability` 每个 case 的成功判据形如
`if err == nil && len(response.Choices) > 0 { … }`，而 `result.Status` 初值就是
`ProbeInconclusive`（`provider/capability_detection.go:110`），`ErrorClass` 只在
`err != nil` 时写（`:218-220`）。所以**上游 200 返回、但断言不成立**
（如工具探针拿回一个没有 `tool_calls` 的回复，`:154-156`）与"拒绝了但没解析出原因"
完全无法区分，且不留痕迹。

新增状态 `assertion_failed`。四个消费点必须同步改，否则新值会被静默吞掉：

1. `CapabilityProbeStatus.Valid()` 的允许列表（`domain/model_capability_detection.go:58-61`）
   —— 漏了它，任何带该状态的 `PutModelCapabilityDetection` 会校验失败。
2. `probeOutcomeRank` 的 `default: return 0`（`app/admin_model_capability_detections.go:715-729`）
   —— 会让断言失败在接口识别里排到 `inconclusive` 之下。
3. 前端联合类型（`web/src/types.ts:584`）。
4. 两份 `detectionProbeStatus` 文案，以及 `unestablishedCapabilities` 过滤
   （`DeploymentsPage.tsx:1398`，目前只匹配 `inconclusive` 与 `not_probed`）——
   不改的话新状态会从横幅里掉出去，与 F3 的目的正好相反。

### 11.6 F4 / F5 / F6 / F7

**F4 —— Mantle Responses 模型枚举。** 给 `ResponsesAdapter` 实现
`ListInvocationTargets`，并声明 `CanEnumerate`、`CanDescribe`、`CanVerify`。
（落地时读的是 `operationURL("models")`；2026-08-25 实测后改走账号级的
`catalogURL()`，见 §1.5。）
限制写进注释：OpenAI 形状的 `/v1/models` 只给 `id` 与 `owned_by`，**不给模态**。
`MapCapabilityClaims` 这一期**不实现** —— 没有元数据就不要产 claim。
同时给枚举结果一个明确的条目上限（§8 最后一条）。

**F5 —— Mantle 模态元数据（条件性）。** 第一版说前置条件是"两套模型 ID 能否可靠对应"。
那是必要条件，但**远不充分**。评审查出三重独立阻塞：

1. **主机**：控制平面允许清单的放宽只对 `SurfaceBedrockRuntime` 生效
   （`internal/app/providers.go:658-663`），`ControlPlaneHostFor` 也只识别
   `bedrock-runtime.` 与 `bedrock-runtime-fips.` 前缀（`provider/bedrock/models.go:242-259`）。
   Mantle 绑定是 `SurfaceBedrockMantle`，主机形如 `bedrock-mantle.<region>.api.aws`
   （`domain/provider_table.go:210-245`），推导不出任何控制平面主机 ——
   SafeTransport 会直接拒绝拨号，`ValidateEndpoint` 也会拒绝非 Mantle 主机
   （`bedrockmantle/adapter.go:49-68`）。
2. **凭据**：Mantle 钉死 `CredentialBedrockAPIKey`（`adapter.go:74-76`），
   而控制平面读取用的是从 `CredentialJSON` 构造的 SigV4 授权器
   （`provider/bedrock/adapter.go:126,168`）。在任何 scheme 意义上都不是"同一份凭据"。
3. **IAM**：它要求该密钥额外携带 `bedrock:ListFoundationModels`。

**所以 F5 的第一道闸门不是 ID 映射，而是"给 Mantle 绑定的 Access Surface 增加
第二个主机是一次 provider profile 变更，需要专门的契约评审"。**

绕开全部三条的形态：从操作者**另行配置的原生 Bedrock 服务商记录**取模态 claim，
按 canonical model ref 合并，Mantle 绑定不获得任何新主机。

**F6 —— `reasoning` 探针（待定）。** 判据两选一：响应字段法（查 `reasoning_tokens`
或 reasoning 内容块，判据硬但每个 profile 一份实现）；不拒绝法（便宜，
但"没报错"不等于"真推理了"，按 fail-closed 取向不配拿 `verified`）。倾向前者。
**但要注意它对本次症状无用**：`mantleOpenAIResponsesSet` 本就不含 `Reasoning`，
即便加了探针，只对 chat profile 与 OpenAI 生效；而 chat set 的 7 项加推理正好把
上限 8 排满（§1.6）。

**F7 —— 前端 `not_probed` 分支。** `capabilityUnavailableReason`
（`DeploymentsPage.tsx:1274-1282`）只判 `unsupported`，其余一律归
`detectionUnestablished`，把服务端早已区分开的 `not_probed` 又压平了。
加一个分支加一条文案即可。

### 11.7 操作者当下能做的（已更正）

**第一版给的"重新识别加 grep 日志"是错的，删除。** 三个理由：

1. `"provider attempt failed"` 只从网关数据面发出（`internal/gateway/service.go:637`）。
   能力识别走 `admin_model_capability_detections.go`，直接调 `detector.DetectCapability`，
   **对失败的探针不记任何日志**。所以那条 grep 必然返回空。
2. 重新识别是真金白银的上游调用（所以它被 step-up 挡着）。在 F0 落地前，
   判决结果也不会改变。
3. `<data-dir>/logs/halro.log` 只在 `logging.output` 为 `file` 或 `both` 时存在，
   而该键不在 SIGHUP 热重载清单里（`docs/guides/operator-guide.md:289-296`），
   要改配置加重启；发行镜像是 distroless，里面没有 shell，也没有 `grep` 与 `python3`；
   Docker 与 Kubernetes 下操作者看的是 `docker logs` 或 `kubectl logs`，不是文件。

**当下唯一有效的动作**：如果确认该模型确实支持图片，就手动声明。
对一个**已存在的部署**，路径是「编辑该部署 → 能力 → 勾选视觉 → 保存」——
`widening` 分支会自动带上 `mode: "operator_declared"`（`DeploymentsPage.tsx:1158`、`:1209`），
并弹出 `widenDeclarationTitle` 说明后果。保存在缺少该声明时会被服务端拒绝，
所以这不是绕过，是既有的、留给这种情形的正式通道。

（顺带更正第一版的一个假设：这条出路的可发现性并不差 —— 创建流程里
「手动配置」出现在四处，包括那条 inconclusive 横幅内部，
`DeploymentsPage.tsx:1618`、`:1649`、`:1655`、`:1706`，`detectionUnestablishedDescription`
的文案本身也点了名。）

### 11.8 验证方式

- **F0**：`provider/capability_detection_test.go` 里驱动一个带 code 与 param 的 400，
  断言 `ProbeUnsupported`；再驱动一个未知 code，断言 `inconclusive`。
- **F1 已有**：`TestResponsesAdapterKeepsTheRefusedParameterInTheIdentifier`
  与状态矩阵测试的 `ProviderCode` 断言，已做反向验证。
  **但它们不覆盖分类结果** —— F0 的测试必须补上这一段。
- **F2b 与 F3**：桩上游分别返回「400 加 `unsupported_parameter`」「400 加未知 code」
  「200 且不含 `tool_calls`」，断言三者落在三个不同状态上。
- **F4**：桩 `/v1/models` 返回两个模型，断言 `can_enumerate` 为真、两个目标进目录、
  且**没有**产生任何 capability claim。
- **F5**：前置条件的比对结果本身就是交付物；不通过则关闭该项。
- **待补**：§1.3 的探针渲染实测只覆盖了 Responses 适配器。
  用户实际走的 **chat 路径（OpenAI 适配器加 `openai/v1` 前缀）需要等价的实测**，
  尤其要确认 `operationPathPrefix` 在 chat 与枚举两条路上是否一致（§1.5 说它不一致）。
- **端到端**：真实 Provider 冒烟测试是计费的、opt-in 的，按
  `docs/verification/provider-real-matrix.md` 走，不进常规 CI。

---

## 12. 落地记录（第二版之后）

### 12.1 已完成

P0a/P0b/P0c、P1a/P1c、P2b，以及 F0、F2a、F2b、F3、F4、F6、F7。要点：
（P1b 与 D8 曾记在这里，已于 2026-08-25 被实测撤销 —— 见 §1.5。）

- **F0**：`classifyCapabilityProbeError` 改用 `strings.Cut` 取 code 半段。
  `CapabilityDetectorContractVersion` 提到 `v2`（它是识别选择指纹的输入，
  旧结果因此不再命中指纹，而不是被拒绝）。
- **F2b**：`CapabilityProbeResult` 增 `ProviderStatus` / `ProviderCode`，
  采集点用新的 `provider.SafeProviderIdentifier` 收敛 —— 长度 128、
  字符集 `[A-Za-z0-9._-]`、越界丢弃不截断，**code 与 param 分别收敛**，
  免得一个带方括号的 JSON 路径把决定判决的那半也一起拖掉。
  `bedrock.safeProviderCode` 改为委托给它，只保留自己的 `#/` 尾段裁剪。
  `Validate` 拒收范围外状态码与超长 code。
- **F3**：新增探针状态 `assertion_failed`（上游正常应答、应答里没有证据），
  与 `inconclusive`（拒绝了但读不出原因）分开。四个消费点全部同步：
  `Valid()` 允许列表、`probeOutcomeRank`（重新编号并留出间隔）、
  前端联合类型、两份文案与横幅过滤。
- **F6**：判据取**响应字段法** —— 只认上游自己产出的东西（计费的
  `reasoning_tokens`，或应答里的 reasoning 内容）。"没报错"一律不算。
  effort 取 `minimal`：问的是"到底推不推理"，不是"推得多深"。
  **探针上限 8 没有动**（那是计费决定）。改为：排到上限之外的能力由
  `CapabilityDetectionPlan.Deferred` 点名，运行前就以
  `probe_kind: "probe_budget"` 写进结果 —— 原来的做法是提前 return，
  让"预算不够"和"策略上就不探"长得一样。推理排在计划最后，
  所以在满编的 OpenAI / Azure profile 上被让出的正是它。
- **前端**：`capabilityUnavailableReason` 从三分支扩到七分支
  （接口不承载 / 连接没开 / 明确不支持 / 应答无证据 / 上游不可达 /
  凭据无权 / 探了没结论 / 未探测 / 预算不够），每种一句话、一个不同的下一步。
- **P2b**：`internal/domain/capability_modality.go`。删掉了第一版表里两行错的
  （输入·Speech 是类别错误，输出·Video←`async_generate` 不成立），
  输入·Text 逐个列出而不是"operations 组任一"（`transcriptions` 的输入是音频，
  派生会给一个纯转写部署标上文本输入）。经服务商 profiles 端点下发，
  `web/src/test/provider-profiles.golden.json` 已重新生成（纯新增 77 行）。
  单测断言字典里每个能力要么被映射、要么被显式列为非模态，两者不可兼得也不可皆无。

每一项都做了反向验证：把改动摘掉，确认对应测试如期失败，再装回；
搜索串都断言过命中数，避免"改了个寂寞还以为通过了"。

### 12.2 F5：受阻，不做

第 11.6 节列的三重阻塞在代码里逐条确认过，没有一条能在不改 provider profile
契约的前提下绕开。另外它的必要条件（Mantle 与 `ListFoundationModels`
两套模型 ID 是否稳定一对一）需要拿两份**真实**的 AWS 列表比对才能回答，
这不是读代码能得出的结论。

所以 F5 保持关闭。要重开，先做两件事，顺序不能反：

1. 把 Mantle 绑定的 Access Surface 增加第二个主机，作为一次 provider profile
   契约评审提出来；
2. 拿到两份真实列表的比对结果。

在这之前，抽屉里 Mantle 的模态行显示"该接口不提供模态信息"。

---

## 13. 卡片网格（P3）落地

抽屉（P2a）当时按操作者决定**不做**：添加与编辑继续用现有弹窗，改的只有列表形态。
这个决定在 2026-08-24 被推翻，抽屉已落地 —— 以 §14 为准，本节只描述卡片网格。

### 13.1 §6.8 的契约问题怎么解决的

不是"给部署页另开一套样式"，而是**把卡片声明成设计系统的第二个容器契约**：
新增 `web/src/design-system/resource-card.css`，与 `resource-list.css` 并列，
并在 `design-system.test.ts` 的两处扫描清单里登记（未样式类名的棘轮、
裸数值间距的棘轮）—— 否则新文件里的类名会被算成"没有样式"。

关键决定写在那个文件的注释里：**卡片是一种布局，不是第二套字体系统。**
卡片内部继续用 `.resource-identity` / `.resource-fact` / `.resource-row-state` /
`.row-actions`，字号层级完全沿用 `resource-list.css` 的那四行规定。
变的只有容器：行把单元格排成一条线，卡片把它们叠成一块砖。
这样部署页并没有脱离共享契约，只是换了容器 —— 而 `resource-list.css` 存在的目的
（防止每个页面长出自己的排版）依然成立。

### 13.2 卡片上保留了什么，为什么

| 项 | 为什么不能移进展开面板 |
| --- | --- |
| 漂移 / 待复核 徽章 | 漂移的部署不承接流量，无论「已启用」写什么（`app/providers.go:500-508` 直接把它从路由候选摘掉）。卡片留着会说谎的那个标志，就必须留着纠正它的那个 |
| 状态**文字** | `StatusDot` 是 `aria-hidden`，不传 `label` 就不发文字。只留圆点等于把启停做成纯颜色 |
| 路由依赖计数 | 它是「停用」和「删除」两个按钮的 `disabledReason`。移走就会留下两个禁用控件，理由离它一次点击 |
| 价格设置 | 启用的前置条件；也是只读角色测试断言的那一格 |
| 测试控件 | `attention` 筛选按测试时效定义（`DeploymentsPage.tsx:61-65`），而工具条留在页面上 |

### 13.3 模态图标

`web/src/ModalityMarks.tsx`。三条硬规则：

1. **映射不在前端推导。** `capability_modalities` 随 profile bundle 下发（P2b），
   这个组件只负责画。浏览器里再写一份正向映射，就是控制台与服务端漂移的老路。
2. **不用 emoji。** 每个标记是内联 SVG —— emoji 无法 token 化、无法主题化、
   无法做对比度检查，各系统渲染成不同的图。
3. **图形永远不是唯一信号。** 每个标记带自己的 `aria-label`，写明方向、模态、
   证据等级（「输入：图像（已声明）」）；证据用**笔画粗细**表示（实心=已验证、
   描边=已声明），不是颜色 —— 单色渲染下依然分得出。

卡片上把「N 项能力」放在标记旁边：`streaming` / `tools` / `json_mode` 等协议类能力
没有任何模态可画，只有标记的话它们会被读成"不存在"。**模态矩阵是模态视图，
不是能力摘要** —— §4.0 就写了这一条，卡片上用计数兑现。

上游模型 ID 在卡片上是一串裸 mono 字符串，配了 `aria-label` 把原来那个列标签
（`deployments.upstreamTarget`）变成它的无障碍名 —— 列没了，标签不能跟着没。

### 13.4 展开

展开的卡片 `grid-column: 1 / -1` 横跨整行，而不是比邻居长一截 ——
打开一张不会让周围的砖重排。展开面板的内容一行没动。

### 13.5 未做的视觉核对

卡片由测试覆盖（新增两个：一个钉住卡片必须保留的四项事实，一个钉住模态标记的
来源与无障碍名），但**没有在真实浏览器里看过**：本地实例需要管理员口令登录，
我没有。间距、SVG 图形的实际观感、窄屏下的列数需要跑一次 `make dev` 自己看。

---

## 14. 详情抽屉（P2a）：已落地的部分，与升级规划

日期：2026-08-24。§13 说抽屉不做，操作者当天改了决定：卡片的展开面板改成了右侧抽屉。
那句话已在 §13 就地作废。本节先记已经落地的，再规划要改的。

### 14.0 已落地（第一步只搬不改）

- **抽屉是 `Modal` 的一个形态，不是第二套浮层。** `components.tsx` 的 `Modal`
  新增 `drawer`：portal、焦点陷阱、ESC、遮罩关闭、脏表单确认全部共用，只换停靠边
  与尺寸（`styles.css` 的 `.drawer-backdrop` / `.modal.drawer`）。
- **§6.1 让先定模态性 —— 定为阻塞式**（保留 `aria-modal` 与 Tab 陷阱）。理由：
  详情里含写操作（价格版本、隔离恢复），它们各自唤起既有 Modal，会叠在抽屉之上；
  非阻塞面板要同时维护"列表可操作 + 面板可操作 + 弹窗可操作"三层，而操作者在读一个
  部署的详情时并不需要同时操作列表。
- **§6.1 第 2 条「滚动锁定不存在」已作废**：现在有了。`holdDocumentStill()`
  （`components.tsx`）按引用计数锁 `documentElement`，`.modal` 上加
  `overscroll-behavior: contain` 挡住滚动链式传递；引用计数是必需的 ——
  价格弹窗关闭时抽屉还开着，不能提前把滚动交还。
- **只有最上层对话框响应键盘**：两个对话框都在 document 上监听，原来一次 ESC 会连底下
  的抽屉一起关掉。
- **抽屉内容 = 原展开面板内容，一行没动。这正是本节要改的东西。**

### 14.1 诊断：它现在是把代码顺序倒出来，不是回答问题

1. **抽屉里没有「这是什么」。** 标题只有名称；服务商、上游模型 ID、启用状态、接入面
   都不在正文。`provider_model` 只在卡片上 —— 抽屉里唯一能找到它的地方是折叠起来的
   「技术详情」中的三行 ID。
2. **九个事实等权重。** 一个 `billing_mode: free` 的部署照样列四行 US$0.00
   （价格状态 / 输入 / 缓存输入 / 输出 / 固定），把"免费"这个已经回答完的问题又问了四遍。
3. **「不可变价格时间线」名不副实。** 它只渲染 `status === "scheduled"` 的版本
   （`DeploymentsPage.tsx:178`），生效中的与历史版本一条都不列。截图里它是
   一个标题 + 一行原始英文枚举（`manual · asserted · temporary_estimate`）+ 一个按钮。
   那行英文还违反"面向操作者的字要说操作者的话"。
4. **能力被截断成 5 个 + 「+3」。** 能力是这个页面的主语之一，抽屉是唯一能全列的地方，
   偏偏在这里截断；而且不分「已验证 / 已声明」，信息量反而低于卡片上的模态标记。
5. **复核面板的排版是坏的，不是审美问题。** 全局 `dl div`（`styles.css:1490`）
   给它加了 `padding: 11px 0` 与 `border-top`，全局 `dd { text-align: right }`
   （`:1492`）让值右对齐而标签左对齐，而 `.deployment-review-facts dd`（`:538`）
   没有复位 `text-align` —— 截图里「保存的答案来自哪里 / 管理员声明」的错位、
   四行之间多出来的横线，就是这两条。
6. **四格里三格是空的**（不再受支持 —、可复核 —、已由管理员关闭 —）。
   规矩是"空面板保留行"，所以不是删掉，而是这一块需要换形态（见 14.3 段 2）。
7. **同一个抽屉里三套排版语言**：事实网格 `.deployment-facts`、复核面板
   `.deployment-review-facts`、技术详情（全局 `dl div`，标签左 / 值右 mono）。
8. **没有运行事实。** 最近一次测试（`last_tested_at` / `last_test_latency_millis` /
   `last_test_error_class` 都在 `Deployment` 上）、路由依赖、主动探针健康，一个都没有。
   探针失败会把部署从路由候选里摘掉（`provider/provider.go:544`），而抽屉对此只字不提。
9. **没有深链、没有上一个/下一个、没有动作。** 编辑 / 测试 / 启停 / 删除全在卡片上，
   看完详情必须先关抽屉。

### 14.2 抽屉要回答的四个问题

排序即优先级，页面结构按它来，而不是按数据结构：

- **Q1 它现在能不能承接流量？** 启用？复核有没有漂移？最近测试过了吗？有生效价格吗？挂着几条路由？
- **Q2 它按什么计费？** 当前版本、历史与排期、来源可信度。
- **Q3 它能做什么？** 全部能力 + 证据等级；与目录现在说法的差异。
- **Q4 它到底连到哪儿？** 服务商、上游模型、接入面、profile、binding、区域、各种 ID。

### 14.3 结构方案：状态条 + 四段 + 动作条

**段 0 · 状态条**（跟随 header，不随内容滚动）
五个结论词一行排开，每个下面一行小字给依据：启用状态 / 复核状态 / 最近测试 /
路由依赖 / 价格状态。这是 Q1 的完整答案，操作者不必往下读。

**段 1 · 计费**（Q2）
- 随 `billing_mode` 收敛：`free` 只出一行「免费（版本化）· v1 · 生效于 …」；
  `metered` 才出四格费率；有 `schedule` 时补窗口行。
- 下面接**真正的时间线**：`active` + `scheduled` + `superseded` 逆序列出，
  每条 `v# · 生效时间 · 计费模式 · 来源`；排期中的带取消按钮。
  数据现成 —— `api.deploymentPrices` 本来就返回全部版本，前端只是没渲染。
- 来源三元组（`type` / `assurance` / `reference`）翻成人话，不再直接印枚举。

**段 2 · 能力**（Q3）
- 全量列出，不截断；每项标证据（已验证 / 已声明），操作者关掉的单独成组。
- 复核状态从"浮在顶部的通知"变成这一段的一部分，用「保存的答案 ↔ 目录现在的说法」
  两列对照呈现，空值保留行但不再是四个孤零零的破折号。
- **数据缺口照旧**：§5 D7 成立 —— 部署上只有三档证据，七值探针状态取不到。
  这一段不承诺展示探针级结果。

**段 3 · 运行与限制**（Q1 的细节）
并发上限、区域、上下文窗口、输出上限、最近测试（时间 / 耗时 / 失败类别）、
路由依赖（→ 路由页）。

**段 4 · 连接与标识**（Q4）
服务商（→ 服务商页）、上游模型 ID、接入面、profile、binding、部署 ID、修订号、
创建/更新时间。**取消 `<details>` 折叠**：一个只装 ID 的折叠块，恰恰在排障时
每次都要多点一下；排在最后即可，不必藏起来。

### 14.4 布局与排版

- **收敛成一种事实排版。** 抽屉内统一复用 `.resource-fact`（设计系统已有），
  段容器定义一次网格；`.deployment-review-facts` 与全局 `dl div` 在抽屉内不再生效。
  14.1-5 的两处复位（`text-align`、`border-top`/`padding`）是这件事的最小版本，
  可以先单独落地。
- **宽度** 34rem → `clamp(30rem, 34vw, 46rem)`。34rem 下两列事实每列约 220px，
  中文标签加数值正好卡住。
- **不要"抽屉里套方框"**：段与段用标题 + 分隔线，不再各自带边框（现在是三个方框叠着）。
- 段标题不做 sticky：分段之后预计 2~3 屏，不值得。

### 14.5 交互

- **深链**：复用已经活着的 `#deployment-<id>` 锚（`components.tsx:171` 的幂等重放链接
  就指它，卡片 `article` 也带这个 id），刷新与分享保留抽屉。
- **上一个 / 下一个**：按当前筛选后的顺序在抽屉头部给两个按钮；关闭时焦点回到对应卡片的
  「查看详情」。
- **底部动作条**：编辑 / 测试 / 启停 / 更多（创建替代、删除）。与卡片重复是必要的
  —— 卡片必须能直接操作 —— 但**禁用理由的文案共用一处**，不许出现两份会漂移的副本。
- **与价格弹窗的层叠**：已经可用（引用计数滚动锁 + 只有最上层响应键盘）。
  失效策略是自动的：价格写操作 invalidate `["deployment-prices", id]` 与
  `["deployments"]`，抽屉读的是同一份 query。
- **只读角色**：抽屉里每个写操作都要带 `title` 说明理由（§7.3 的既有要求）。

### 14.6 数据缺口（补充 §5）
| # | 缺口 | 影响 |
| --- | --- | --- |
| D9 | `/usage` 没有 `deployment_id` 过滤（`internal/app/admin_usage.go:349-354` 只有 project / provider / request / model / provider_model / status） | 「这个部署最近花了多少」做不到精确；用 `provider_id` + `provider_model` 近似会把同一上游模型的两个部署混在一起 |
| D10 | 主动探针健康只出现在 metrics（`halro_deployment_up`，`internal/app/metrics.go:365`），Admin API 不返回 | 状态条里"它现在在不在候选里"没有数据源；卡片上的「测试通过」说的是**手动测试**，与探针是两回事 |
| D11 | 价格提案端点（`api.ts:412`）全站无人消费 | 抽屉可以是它的落点，但那是独立一期 |

### 14.7 分期

- **P0 · 缺陷修复**（各自独立，可先发）：复核面板排版复位；价格时间线补全历史版本；
  来源三元组人话化；能力不再截断。全在前端，无服务端依赖。
- **P1 · 结构**：状态条 + 四段重排 + 统一事实排版 + 宽度。
- **P2 · 交互**：深链、上一个/下一个、底部动作条。
- **P3 · 依赖服务端**：探针健康入 Admin API（D10）→ 状态条才算完整；
  按部署过滤用量（D9）→ 「近期用量」段；价格提案段（D11）。

### 14.8 验收

每期都要有：vitest 钉住结构（段标题存在、免费部署不再出现四行零、时间线列出
active + superseded、能力徽章数与计数一致、复核面板的值与标签同侧对齐）、
`design-system.test.ts` 的两条棘轮不回升、**并且在真实浏览器里核对一次** ——
§13.5 欠的那次视觉核对，这次要还。

### 14.9 明确不做 / 待定

不做：模型简介文案与厂商 logo（§5 D3/D4 已决）；七值探针矩阵（D7）；
抽屉内嵌编辑表单（编辑继续用现有弹窗）。

操作者已定（2026-08-24）：

1. **单栏滚动 + 段标题**，不做 tabs —— 排障时"能不能服务"和"按什么计费"要同屏对照。
2. **宽度 `clamp(30rem, 34vw, 46rem)`。**
3. **用量不进抽屉**，留在 P3，等 D9 有了按部署的过滤再说。
4. **P0 与 P1 合并成一轮做**；P2（深链、上一个/下一个、底部动作条）单独一轮。

### 14.10 落地记录：P0 + P1（2026-08-24）

按 14.7 的分期，缺陷修复与结构重排合并成一轮做完了。P2（深链、上一个/下一个、
底部动作条）未开工。

**结构**（`DeploymentsPage.tsx`）：状态条 + 四段，单栏滚动。

- **状态条**是 Q1 的完整答案：状态 / 能力复核 / 最近测试 / 路由依赖 / 价格，五个结论词
  各带一行依据。漂移时那格的依据写「已停止承接流量」——它比「已启用」更有决定权。
- **计费段**随 `billing_mode` 收敛：`free` 只说一次「免费（版本化）· v# · 生效时间」，
  四行 US$0.00 没了；`metered` 才出费率格。
- **价格时间线**改成真的时间线：`active` + `scheduled` + `superseded` 逆序全列，
  生效中的一行有底色，计划中的带取消按钮；`cancelled` 不列（它从未生效也不会生效）。
  数据一直都在 `api.deploymentPrices` 的返回里，之前只渲染了 `scheduled`。
- **来源人话化**：`manual · asserted · temporary_estimate` → 「临时估算 · 管理员录入 ·
  Halro 未验证」。三个标识来自三个不同的枚举，印原文读起来是系统 trace，不是
  「这个价格有多可信」的答案。
- **能力段**全列不截断，每项带证据（已验证 / 已声明 / 不再受支持 / 未记录证据）。
  「未记录证据」是新加的一档：没有任何东西确立过它，与「已声明」不是一回事。
- **复核面板**补上对照的右半边（`catalog_source` / `catalog_covered` →「目录现在的说法」），
  空行照旧保留。
- **技术标识**从 `<details>` 里放出来，成为最后一段「连接与标识」，并补进了服务商
  （链到服务商页）与上游模型 ID —— 后者此前只在卡片上，抽屉里反而没有。

**排版**：抽屉里的每个事实都是 `.resource-fact`（设计系统既有的标签/值对），只加两条
容器级覆盖 —— 抽屉没有列宽要守，所以值换行而不是省略号；缺失的必要项用警示色。
`deployment-facts` / `deployment-review-facts` / 全局 `dl div` 三套排版在抽屉里全部退场，
连同 26 条随之作废的 CSS 规则（价格面板、能力条、技术详情、以及断点里为它们写的覆盖）。
14.1-5 那两处对齐缺陷因此自然消失：新结构不再经过 `dl div` 与 `dd { text-align: right }`。

**宽度**：`min(clamp(30rem, 34vw, 46rem), 100%)`。

**i18n**：新增段标题、复核/测试/价格状态词、来源类型与保证等级词表；顺手删掉三个
被替换掉的死键（`priceSourceSummary` / `unknownPrice` / `versionedPrice`）和两个本来就
没人读的（`lastTest` 的旧插值、`notConfigured`），并把 i18n 未引用键台账里对应的一行删了。

**测试**：新增两条 —— 一条钉住「免费只说一次 + 时间线列出生效中与已被替代 + 来源是人话」，
一条钉住「能力全列且各带证据 + 四个段标题 + 上游模型在抽屉里 + 技术详情折叠已不存在」。
全套 402 项通过。

**仍然欠着**：真实浏览器里的视觉核对（§13.5 的老账）。本轮同样没有登录实例可用。

### 14.11 P1 之后的一处回归：抽屉正文没有内边距

重排时内容包装类从 `.deployment-details` 改成了 `.detail-drawer`，而给它留白的那条规则
（`.modal.drawer > .deployment-details`）还写着旧名字，于是整个正文贴到了抽屉左右边缘，
而上方 header 因为有自己的 padding 看起来是正常的。

值得记的是**为什么没被测出来**：`.detail-drawer` 本身是有样式的（它的 grid 规则），
所以「标记里的类名没有任何规则」那条棘轮天生看不见这种情况 —— 丢的是某一条规则，
不是全部规则。补了一条针对性的：`.modal.drawer > .<wrapper>` 里带 padding 的那条规则，
它选中的 wrapper 必须是抽屉真的在渲染的那个类名。反向验证过（把旧名字改回去，该测试失败）。

顺带把那条棘轮从「计数不许上升」改成「明确列出今天欠的名字」：计数挡不住改名 ——
这次同一个改动删掉四个类名、加进一个，总数是下降的。

另外把上下文/输出上限从 `1050000` 改成分组数字（`format.ts` 的 `exactNumber`）。
卡片上是缩写（1.1M），抽屉是读精确值的地方，而不分组的长串恰恰两头不讨好。

### 14.12 事实格的对齐与字重

**对齐**：`.resource-fact` 带 `align-content: center` —— 它是为「行」写的，一格坐在
一条固定高度的线上，居中是对的。叠进抽屉的网格里就正好相反：同一行里只要有一格的值
折成两行，整行变高，其余较矮的格子被垂直居中，于是本该成一条线的标签各自错开
（截图里「能力证据」与「修订号」差了约 20px）。`.detail-fact` 改成 `align-content: start`。
jsdom 没有布局，所以钉的是这条声明本身；反向验证过。

**字重**（按 Apple 的 text style 习惯，不新造尺寸）：

- `--font-weight-bold` 由 800 改为 **700**。800 是 SF 的 Heavy，Apple 的任何文本样式都不用它；
  13px 上它读起来是「墨更重」而不是「更强调」。
- 抽屉里的事实值改成 **常规字重**。卡片给事实值加粗是对的 —— 一块砖上就三个值，它们就是
  要一眼看到的东西；而抽屉一段里有八个，八个都强调等于一个都没强调。这正是 Apple 检查器
  面板的排法：说明用 caption、值用正文，层级来自颜色与位置，不来自墨色。
- **例外是状态条**：那五个值是结论，保持 semibold。价格时间线里每条的状态、以及各段标题
  与小标题，统一 semibold —— 不再用浏览器默认的 `<strong>`（现在是 700）。

尺寸一律沿用既有 token（12 / 13 / 15），它本来就贴着 macOS 的 caption / body / title3。

### 14.13 「连接与标识」删掉四项

按操作者要求删除：能力配置（`profile_id`）、绑定 ID、部署 ID、能力证据。留下服务商、
上游调用目标、访问面、修订号。

- **能力证据**是真的多余：它把整套证据压成一个词（「已声明」），而上面的能力段现在
  逐项写明证据等级，同一件事说了两遍。
- **能力配置与绑定 ID** 命名的是 Halro 自己的接线，不是操作者维护的东西。
- **部署 ID** 有一处代价要记下来：日志、指标（`halro_deployment_up{deployment_id=...}`）、
  用量记录都以它为连接键。日志 → 控制台这个方向还通 —— 页面的搜索框本来就匹配
  `deployment.id`（`DeploymentsPage.tsx` 的筛选），把日志里的 id 粘进去就能定位到卡片；
  反过来「我要拿这个部署去 grep 日志」则没有来源了。需要时在抽屉底部放一行可复制的
  小字即可恢复，不必回到事实网格里占一格。

`evidenceSummary()` 随之删除（部署页这一侧；服务商页有自己的一份）。i18n 里
`deployments.profile` / `deploymentID` / `bindingID` / `evidence` 四个键一并删掉 ——
其中 `evidence` 是未引用键检查看不见的那种：`deployments.evidenceValues.*` 的前缀
把它当成了被引用。

### 14.14 状态条的职责，以及修订号的去向

**状态条是什么**：它是 Q1「这个部署现在能不能承接流量」的答案，五格各自都是一个能单独
否决流量的前置条件 —— 启用标志、能力复核（漂移会被路由器从候选里摘掉，
`provider/provider.go:544`）、最近测试（没有当前版本的通过记录就无法启用）、
路由依赖（没有别名指向它同样不承接流量，它还是停用与删除的 `disabledReason`）、
价格设置（没有生效价格版本，启用会被拒绝）。

因此**不做默认折叠**：把答案藏到一次点击后面，抽屉打开先看到的会是「计费」，
而那不是第一个该问的问题。改的是它的重量 —— 列宽从 11rem 降到 7.5rem，五格排成一行，
不再是三加二、还空着一格。

**修订号不进状态条，且不再显示数字**：它不是流量前置条件，是乐观并发用的版本计数器，
和刚删掉的部署 ID、绑定 ID 属同一类。操作者读它是为了「有没有人动过、什么时候动的」，
那是时间戳而不是计数器 —— 「连接与标识」里换成「最近更新」。

### 14.15 详情抽屉多角色评审（2026-08-25）

六个角色各读一遍现在的抽屉：信息架构、交互、视觉排版、无障碍、运维/领域正确性、
前端实现。分级：**P0 必须改**（说谎或不可用）、**P1 该改**、**P2 可选**。

#### 先修的一条（本轮已改）

**F1 · 状态条的列宽从未生效。** `.detail-status` 与 `.detail-facts` 同权重，而
`.detail-facts` 声明在后 —— 上一轮把列宽降到 7.5rem「五格排成一行」的改动被源序吃掉了，
截图里仍是三加二、空着一格。改成 `.detail-facts.detail-status`，并补了一条测试：
状态条那条规则必须带两个类名。这类"同权重靠源序"的覆盖会重演，容器与变体的关系
要写成变体永远比容器多一个类。

#### 领域正确性（这个仓库最看重的一栏）

- **E1【P0】「最近测试」会被读成「它现在健康」。** 那一格来自 `last_test_status` ——
  操作者手动点「测试」的结果，可以是几天前的。而每 30 秒跑一次的主动探针
  （`app/health.go`）失败时会把部署从路由候选里摘掉（`provider/provider.go:544`），
  界面上却没有任何出处。所以一个部署可以在这里写着「通过 · 1336ms」，同时一条流量都不接
  —— 本次会话里那条 404 探测日志正是这个情形。在 D10（探针健康入 Admin API）补上之前，
  这一格的标签至少要说清它是**手动测试**，不能占着"健康"的位置。
- **E2【P2】「路由依赖 2 条路由」没说是"启用的"。** 计数只数 `enabled` 的路由（正确），
  但标签没说，读者会以为是全部。写成「2 条启用路由」。

#### 信息架构

- **B1【P1】同一个价格说了三遍**：状态条「价格设置 · 免费 · v1 08/23」、计费段
  「计费方式 · 免费（版本化）· v1 08/23」、时间线「v1 · 生效中 · 免费 · 08/23」。
  状态条那格是前置条件，留；计费段那一格删掉，让时间线第一条承担。
- **B2【P1】「不可变价格时间线」是 ADR 词。** 操作者问的是「这个部署收过什么价」。
  改成「价格版本」，只有一条时也读得通。
- **B3【P2】词汇不一致**：状态条叫「价格设置」，段叫「计费」。同一件事用一个词。
- **B4【P2】能力段右侧八行全是「已声明」。** 恒定值即噪音。只标注例外
  （已验证 / 不再受支持 / 未记录），段头写一句「均为已声明」。
- **B5【P2】「访问面 openai-api」是内部词。** 它回答的是「客户端该用哪套 API 形状」，
  写成「兼容接口 · OpenAI 兼容」。
- **B6【P2】「运行与限制」三格的附注（部署级 / 令牌 / 令牌）信息量低。**「令牌」是单位，
  留；「部署级」只在与服务商级并列时才有意义。

#### 交互

- **C1【P1】加载态文案错位。** 价格未就绪时，状态条那格显示「正在读取网关状态」——
  `common.loading` 是页面级句子，塞进一个格子里既长又不是那一格在说的事。需要一个
  短文案（`common.loadingShort`）或骨架。
- **C2【P1】服务商链接只到列表页。** `ProvidersPage` 的 `article` 带 `id="provider-<id>"`，
  链成 `/admin/providers#provider-<id>` 即可直接定位，成本几乎为零。
- **C3【P2】抽屉里没有主要动作**（编辑 / 测试 / 启停）—— 已排在 P2。
- **C4【P2】复核提示说「开启某项能力并重新测试」，抽屉里却没有去编辑的入口。**
  和 C3 一起做。

#### 无障碍

- **D1【P1】时间线里每个「取消」按钮的可及名称都一样。** 有两条排期版本时，
  屏幕阅读器听到的是两个「取消」。给 `aria-label` 带上版本号。
- **D2【P1】状态条没有分组名。** 五对标签值读下来，辅助技术不知道它们合起来在回答什么。
  `role="group"` + 一个说明它是什么的名字。
- **D3** 能力证据是颜色 + 文字，颜色不是唯一信号 —— 合规，保持。

#### 视觉排版

- 段标题 15px semibold + 分隔线、事实值常规字重、状态条 semibold —— 层级成立。
- **状态条与四个段是两种分段语言**（一个有底色无标题，四个有标题无底色）。可接受：
  它本来就不是"段"，而是段之前的答案。
- **F2【P2】** `CapabilityReviewNotice` 现在长在能力段里，仍带着自己的 `margin-top`，
  与段的 gap 叠加。
- **F3【P2】** 能力为 0 时是一个空 `<ul>`，没有空态文案。

### 14.16 评审落地：P0 + P1 + P2（2026-08-25）

§14.15 列的全部条目一轮做完。

**P0 · E1** —「最近测试」改名「最近手动测试」。那一格来自 `last_test_status`，是人点出来的
结果，可以是几天前的；每 30 秒的主动探针才决定它在不在路由候选里，而那份健康目前只在
metrics 里（D10）。改名不解决问题，只是不再让它冒充"现在健康"—— 真正的解药是 D10。

**P1**

- **B1** 免费部署的「计费方式」格删掉：价格由时间线里那条版本说一次，加上状态条那格
  作为前置条件，一共两处；原来是三处。费率网格只在按量计费或没有价格时出现。
- **B2** 「不可变价格时间线」→「价格版本」。
- **C1** 新增 `common.loadingShort`（「读取中…」）。`common.loading` 是「正在读取网关状态」，
  一句页面级的话塞进一个事实格里。
- **C2** 服务商链到 `/admin/providers#provider-<id>` —— 那个锚点 `ProvidersPage` 一直在渲染。
- **D1** 时间线里每个「取消」带 `aria-label="取消价格版本 v4"`。
- **D2** 状态条 `role="group"` + 「能否承接流量」。

**P2**

- **B3** 状态条那格改叫「计费」，与段名一致。
- **B4** 能力段：八行相同的「已声明」收进段头（「8 项能力 · 全部为已声明」），证据列只在
  各项不一致时出现 —— 那时它才是可比较的信息。
- **B5** 「访问面 openai-api」→「兼容接口 · OpenAI 接口」。问的是客户端要说哪套 API。
- **B6** 去掉「部署级」这类每格都一样的附注；「令牌」是单位，留。
- **E2** 「2 条路由」→「2 条启用路由」（计数一直只数启用的）。
- **C3 / C4** 抽屉底部补上动作条：测试 / 编辑 / 启用停用。**同一个组件**
  `DeploymentPrimaryActions` 同时供卡片和抽屉使用 —— 它们共享的不是外观，是"什么时候
  允许写、不允许时说什么"（只读、测试与修订不匹配、路由占用）。两份副本就是同一个问题的
  两个答案。删除与创建替代留在卡片：那是结束一个部署的生命，不是调整正在读的这一个。
  抽屉里点「编辑」会先关抽屉再开编辑弹窗。
- **F2** 复核提示的 `margin-top` 去掉，段的 gap 就是间距。
- **F3** 能力为 0 时给空态文案。

i18n 顺带删了两个随之作废的键（`deploymentScope`、`freePrice`）。测试新增两条
（状态条的分组名与手动测试措辞 + 启用路由计数；抽屉动作条的三个控件、禁用理由、
服务商深链），并更新了因改名而失效的四处断言。全套 407 项通过。

### 14.17 D10 落地：主动探针健康进 Admin API

§14.15 的 P0（E1）真正的解药。此前只有 `halro_deployment_up` 这一个出口，操作者要读
metrics 才知道一个部署已经不在路由候选里。

**服务端**

- `provider.Registry` 的 `health` 从 `map[string]bool` 改成 `map[string]DeploymentProbe`
  （健康与否、观测时间、**分类后的**错误类别）。理由写在类型注释里：光有结论，操作者面对的
  是一个「已启用、已测试、已定价却不接流量」的部署；而失败的那句话是上游对这次请求写的
  散文，它留在 error 里，不进入控制台和日志读的状态。按 pre-1.0 的规矩，
  `SetDeploymentHealthy` / `DeploymentHealth` 就地换成 `SetDeploymentProbe` /
  `DeploymentProbes`，不留旧的一份。
- `app/health.go` 新增 `recordDeploymentProbe`：类别用 `persistedProbeClass(describeProbeFailure(err))`
  —— 和手动连接测试落库的是同一个类别，所以控制台只需要一张措辞表。
  两个"根本没发出请求"的分支（服务商不存在/已停用、没有绑定适配器）以前只写 `false`，
  现在也给理由：`ErrorBadRequest` + 无上游状态码，`persistedProbeClass` 读作
  `bad_request_local`（「Halro 在发往上游之前拒绝了这次探测」），而不是让操作者去查一条
  从未发生的上游拒绝。
- `adminDeploymentView` 增加 `probe: { state, observed_at?, error_class? }`。
  `state` 三态：`healthy` / `unhealthy` / **`not_probed`**。第三态是独立状态而不是失败 ——
  注册表刻意让未探测过的部署保持可路由，报成 unhealthy 会让控制台在每次重启后都宣称故障。

**控制台**

- 状态条第六格「主动探测」：正常（带观测时间）/ 未通过（「已移出路由候选 · <分类原因>」，
  警示色）/ 尚未探测（「还没跑过探测，暂不影响路由」）。
- 卡片上加一条「主动探测未通过」，与漂移徽章同样的处理 —— 理由也一样：这两件事都会在
  「已启用」写着好话的时候把部署摘出路由。

**测试**：Go 侧四条（not_probed 与 unhealthy 不混淆、类别与观测时间的传递、健康时不带理由、
不可探测的服务商读作本地拒绝），前端两条（卡片徽章 + 抽屉里的后果与原因；未探测态不冒充失败）。
`go test ./internal/app ./internal/provider ./internal/gateway` 通过，前端 409 项通过。
文档：`docs/contracts/metrics-reference.md` 里补了一句，说明同一份探针结果现在也在部署记录上。

**仍未做**：探针失败不写审计、不发告警（现在只有 WARN 日志与这个字段）。要不要让它进
告警链路是单独一次设计决定，不在本轮。

### 14.18 抽屉底部动作条撤回

操作者看到实物后决定不要：抽屉里不放测试 / 编辑 / 启停，动作留在卡片上。§14.16 里
C3 / C4 那一条据此作废，本节为准。

一并撤回的是为它做的抽取：`DeploymentPrimaryActions` 只剩一个调用点，而它被抽出来的
唯一理由是"两个地方共享同一套写入前置条件"。一个只有一个使用者、名字却在讲共享的组件
是对自己存在理由的谎言，所以内容原样搬回卡片，组件删除。CSS 的 `.detail-actions` 与
钉住它的测试同样删除；那条测试里顺带断言的服务商深链（C2）移进了相邻的用例，没有丢。

**因此重新变成未解决的**：C4 —— 复核提示写着「开启某项能力并重新测试」，而抽屉里没有
通往编辑表单的入口，操作者要先关抽屉、再在卡片上点编辑。记在这里，等下一次讨论怎么解。

### 14.19 能力复核面板重写

操作者的原话：「只需要说明问题就行，不需要这么多文字，也不明白要做什么」。原来的面板
两句解释开头、五个事实（其中三个通常是破折号）、一句结论收尾，从头到尾没说要做什么。

**新的形状**：标题 + 每处分歧一行（标签 + 能力名）+ 一个动作。

- **标题按内容决定，不再只讲一半。** 「有可复核的新能力」压在一块最扎眼的是橙色
  「不再受支持」的面板上，只描述了发生的一半。有分歧时标题改为「目录与你的声明不一致」。
- **`no_longer_supported` 在两个状态下是两件事，不能共用一个标签和一种颜色。**
  漂移时它是「什么都不再支持它」；可复核时它是「目录不认可一个仍在服务的声明」——
  代码注释本来就写着后者「naming it so the disagreement is visible, not so anything is
  turned off」。现在漂移时叫「不再受支持」，可复核时叫「目录不认可」，后面跟一句
  「仍按你的声明运行」，且**不上警示色**（那是上一轮我加错的）。
- **「目录现在的说法」这个标签承诺内容、给出的却是来源**（内置模型目录）。整格删掉 ——
  它不改变读者要做什么。
- **空行不再占位。** 三行（可开启 / 目录不认可 / 已由管理员关闭）只在非空时出现，
  通常只剩一到两行。这与「空面板保留行」不冲突：那条规矩针对的是诊断面板里"查过了、
  没有"的事实，而这里的空行只是这一次分歧不涉及那个方向。
- **只在漂移时保留原因句与后果句**，因为那两句会改变读者要做的事：原因决定编辑能不能修好
  （profile 收窄编辑没用，目录丢了可以），后果是"它已经不承接流量"。可复核不花任何代价，
  两句都不需要。
- **C4 解决**：面板右侧是「去编辑能力」，点了先关抽屉再开编辑弹窗。它复用既有的
  `.notice.has-action` / `.notice-copy` / `.notice-action` 结构，不是新造一套。只读账户下
  按钮禁用并带原因。

i18n 删掉五个随之作废的键（`availableForReview`、`catalogSays`、`catalogNoLongerCovers`、
`reviewAvailableConsequence`、`capabilitySource`）。最后一个和之前的 `evidence` 一样是
未引用检查看不见的：`deployments.capabilitySources.` 这个动态前缀把它算成了被引用。

新增一条测试钉住整块：标题覆盖分歧、两行内容、不带警示色、空行与来源格不再出现、
「去编辑能力」关抽屉开表单。全套 409 项通过。

### 14.20 状态条撤销

§14.14 写过它的职责，操作者读了之后第二次问「这部分有什么用」。第二次问同一块界面，
问题就在这块界面上，不在读它的人：一切正常时它是六个令人安心的值和零个下一步，
而它的价值只在出问题时才出现 —— 一块只在异常时才有意义、却在正常时占满第一屏的面板，
读者当然不知道它是干什么的。整块删除。

facts 不丢，按「在抽屉里还有没有别的落脚点」分流：

- **能力复核**、**计费** —— 下面的能力段（复核提示）与计费段本来就各说了一遍，删掉重复。
- **状态**、**主动探测**、**最近手动测试**、**路由依赖** —— 抽屉里没有第二处，全部并入
  「运行与限制」，排在并发/区域/上下文/输出上限之前：先说它现在怎么运行，再说它的上限。

随之删除：`.detail-status` 及其列宽与字重规则、钉住列宽的那条测试（§14.15 F1 修的就是它，
现在连同被修的东西一起消失）、i18n 的 `readinessGroup` / `capabilitiesCurrent` /
`capabilityReview` / `driftedShort` 四个键。新增一条测试钉住「状态条不存在，且它独有的
四个事实都在运行段里」—— 否则下一次重排很容易把它们一起丢掉。

### 14.21 模态行改回文字（推翻 §13.3 的图标决定）

§13.3 定的是「每个标记是内联 SVG，不用 emoji，证据用笔画粗细」。图标这条被实物推翻了：
五个手绘的 16px 字形在卡片上认不出来 —— 图像（框 + 山 + 太阳点）糊成一个叉，
远程图片（框 + 弧线）读成括号，音频画的是话筒（那是"录音"，不是"音频数据"），
向量是一条毫无含义的虚线。要靠悬停才知道是什么的图标，等于一个多了几步的图例。

**改为写字**：文本 / 图像 / 远程图片 / 音频 / 向量 —— `modalityNames` 本来就是这几个词，
一直只用在 aria-label 里。中文界面下两个字比 16px 图标更短也更明确，英文下用同一张表。

§13.3 里仍然成立的两条：**图形/文字都不是唯一信号**（每个标记继续带
「输入：图像（已声明）」这样的可及名），**证据不靠颜色**（已验证用更重的字重，
已声明用常规字重加更浅的颜色 —— 单色渲染下仍然分得开）。`glyphs` 表与
`.modality-mark > svg` 规则一并删除。

测试补了三条断言：行里出现的是词、里面没有 `<svg>`、已声明的标记带 `data-evidence`。

### 14.22 卡片按 Apple 的密度规矩重排

操作者：「模型板块感觉非常拥挤」。一块砖里 11 条文字 + 2 条分隔线 + 5 个控件。五条一起改：

1. **空白分组，不用线。** HIG 里分隔线分隔的是同类条目的列表，不是一块卡片的内部。
   `.resource-card` 的组间距从 12px 提到 20px，`.resource-card-facts` 与
   `.resource-card-actions` 的 `border-top` 删除。
2. **事实只在不平静时出现。** 「已设置 · 不限 · 无启用路由」是两行"没事要做"，而且出现在
   每一张卡上。§13.2 要求它们留在卡片上的理由**恰好只在异常时成立**：价格是启用的前置条件，
   缺的时候才需要说；路由计数是「停用」「删除」的禁用理由，>0 时才存在；并发只在真有上限时
   是信息。三个都为静默时，整组消失。价格读取**未完成时也不显示** —— 那时还谈不上"缺"，
   否则每张卡会先冒出一行再收回去。
3. **「以上游为准」删掉。** 它是"没有这条信息"被写成了一条信息；抽屉里才是确认"未声明"的地方。
4. **尾部配件只留一个。** 名称行右侧只剩启用状态；漂移与探测失败两条警示移到正文首行、左对齐 ——
   它们是警示，不该和状态挤在同一个右上角，把每张卡读成一张两列表格。
5. **一个显著动作 + 一个菜单。** 底部从五个平级控件（两行）收成：测试 + 结果 / 编辑 · 查看详情 · ⋯。
   启用与停用进菜单：它们是低频、需要解释前置条件的操作，菜单行有地方写，四个按钮挤一行没有。
   `deployment-state-toggle` / `deployment-state-sizer` 这套等宽占位随之删除。

另外把服务商与上游模型合成一行标识（`AWSBedrock · openai.gpt-5.5`）—— 它们本来就是同一件事：
哪条连接、连接上的哪个模型。

常态卡片从 11 条文字降到 5 条。i18n 删掉两个随之作废的键（`priceConfigured`、
`viewDeploymentPrice`）—— 「已设置」这个可点入口连同它显示的那一格一起消失了，
进抽屉走「查看详情」。新增一条测试钉住"没事要做时什么都不说"，并把三处依赖旧结构的
断言改到新位置（启用按钮现在要先开菜单）。全套 410 项通过。

### 14.23 一行里的卡片等高

事实改成「只在不平静时出现」之后，同一行里的卡片高度不再一致：有路由依赖、有声明上限的
那张比邻居高出一截，行的下沿是锯齿形，几张卡的动作栏也各在不同高度上。**哪张卡高，说明的
不是这个部署的什么事，只是它恰好有几条事实需要说** —— 所以高度不该由内容决定。

- `.resource-card-grid` 的 `align-items` 从 `start` 改成 `stretch`：一行里每张卡都取这一行
  最高的那个高度。
- `.resource-card` 从 grid 改成 **flex 列**，`.resource-card-actions` 加 `margin-top: auto`：
  卡片被拉高时内容仍从顶部堆叠，动作栏贴在底边 —— 这才是几张卡的动作栏对齐的原因。
  grid 做不到这件事，因为卡片的子元素数量是变的（警示行、事实组、失败原因、错误块都可能
  不存在），`grid-template-rows` 里的 `1fr` 会落在哪一行取决于这次渲染有几个孩子。
- 原来靠 `grid-template-columns: minmax(0, 1fr)` 防止"一个不换行的孩子把整张卡撑出边界"
  的那条保护改成 `.resource-card > * { min-width: 0 }`，作用相同。design-system 的那条测试
  同步改成钉新的写法，并新增一条钉住「等高 + 动作栏贴底」；两条都做了反向验证。

**过程记一笔**：为了做反向验证，我用 `git checkout` 还原了 `resource-card.css`，那把本次会话
对这个文件的全部改动一起还原了（卡片宽度上限、间距、模态行文字化都在里面）。已按记录逐条
重建并用 diff 核对。反向验证不要用 `git checkout` 还原整个文件，改哪一行就临时改回那一行。

### 14.24 卡片重排：四个固定槽 + 一条结论行（2026-08-25）

操作者：「整体卡片布局还是比较乱」。四个角色（信息架构、运维、无障碍、前端实现）分头评审，
结论一致，而且都是量出来的，不是审美判断：

1. **状态行数学上放不下。** 测试控件 ~147px（`flex: 0 0 auto`，不可压缩）+ 三个事实 ~284px +
   间距 ≈ **447px**，卡片内容宽只有 **230–310px**（轨道 `clamp(17rem, 26vw, 22rem)` 减去
   两侧 `--space-5`）。它在任何视口都换行。
2. **高度差全部堆在一个缝里。** `margin-top: auto` 只锚住动作栏，所以一行里最高卡与最矮卡的
   ~106px 差额，整块沉淀在动作栏上方；测试按钮的纵向位置在相邻卡片间跳 ~52px。
3. **同一件事说了五遍。** 名称旁的点、右上状态词、漂移小字、测试结论、价格格 —— 四条可以同时
   为真，其中两条用相反的颜色语义（已停用=红点，测试通过=绿点，会同时出现在一张卡上）。
4. **等高的承诺早就破了。** `.row-test-failure` 与 `.deployment-card-error` 渲染在动作栏
   *之后*，而 `margin-top: auto` 会把其后的兄弟一起下推 —— 这两块一出现，动作栏就离开了
   邻居所在的那条线。
5. **卡片是设计系统的一个分叉。** `.resource-card*` 全仓只有这一个使用者，`styles.css` 里
   六处覆盖 `resource-card.css`，其中 `grid-template-columns: minmax(5.5rem, 1fr)` 完全不
   生效；另有 8 条死规则（`.deployment-facts`、`.deployment-row`）从改成卡片那次起就没有 markup。

**新结构：每张卡恒定四个槽。**

```
槽1 身份     名称（h3）+ 服务商·上游模型 | 状态词 + ⋯ 菜单
槽2 结论     ● 最高严重度的一条 · 分类原因 · 距今时长   [+N]  [测试]
槽3 规格     文本 → 文本 · 6 项能力 · 1M/384K · 并发 8   （单行，右侧先裁）
槽4 操作     编辑 · 查看详情 · 启用|禁用
```

- **对齐机制是 subgrid。** `.resource-card-grid` 本来就是 grid：卡片 `grid-row: span
  var(--resource-card-slots, 4)` + `grid-template-rows: subgrid`，于是槽 n 对同一行里每张卡
  都是同一条父轨道，天然共享高度。没有任何一处写死高度，某个槽这一行没人渲染就收成 0。
  `margin-top: auto` 与 `overflow-anchor: none` 一并删除 —— 前者是"不知道最后一块在哪"的
  变通，后者会让一张挂载后还会变高的卡片失去滚动锚点资格。父行间距 `--space-5` 大于卡内
  槽间距 `--space-3`：一张卡要先读成一个东西，再读成四块。
  subgrid 门槛（Chrome 117 / Safari 16 / FF 71）低于本仓已经在用的 `:has()`（FF 121）。
- **结论行是一条严重度阶梯**（`web/src/pages/deploymentCondition.ts`，纯函数，单测独立）：
  测试进行中 › 能力漂移 › 探测未通过 › 价格隔离 › 测试失败 › 价格不可用/未设置价格 › 需重测 ›
  有可复核的新能力 › 通过·延迟·距今 › 尚未测试。排序依据是**多快会停止承接流量**。只显示最高的
  一条，其余折成 `+N` 点进抽屉 —— 不静默丢弃。卡片上唯一的彩色点就是它，名称旁那个
  `StatusDot` 删掉（右边的词说的是同一件事，还和它撞色彩语义）。
- **测试结论加时间。** 「通过 · 151ms」不带时间时，两个月前对一个已下线模型的测试仍然显示绿色。
  新增 `formatAge()`（`now` 是入参，否则组件读墙上时钟就不可测）。
  **未做**：超期自动降级为「需重测」—— 那会改变启用前置条件（`testIsCurrent`），属于策略变更，
  另行决定。
- **移出卡片**：路由依赖（它拦住的两个控件已在点击处说明原因，抽屉保留计数）、静态并发上限
  改成规格行的一段、价格标签格、`.row-test-failure`（原因并入结论行，超长部分进 tooltip）、
  `.deployment-card-error`（启停/删除失败改走 notification 通道 —— 那是一次点击的瞬时答复，
  不是部署的属性）。随之删除 `priceBlockedEnable` 的对账 effect：阶梯每次渲染都从
  `activePrice` 重新求值，陈旧错误无法再和刷新后的价格自相矛盾。

**无障碍**（评审列出的硬伤，本次一并修）：

- 每卡一个 `role=status` → 30 张卡 30 个 live region，播报「失败」却不说是哪张卡，且筛选
  按键重挂载会再播一次。改为**整个网格一个具名区域**，卡片把「{name}：{verdict}」推进去。
- 部署名升为 `<h3>`，`<article aria-labelledby>` —— 此前 30 个匿名 article，按 H 直接跳过。
- 每个控件的可访问名带部署名，且**可见文字在前**（WCAG 2.5.3）：`测试 — {{name}}`。
- `aria-label` 从 `<code>` 上挪走（无角色元素上的 aria-label 浏览器会丢弃），改真实
  `sr-only` 文本 + 本地化的标签分隔符。
- 可点的结论文字**常态带虚下划线**，不靠悬停才出现 —— 只用色相区分可点与不可点是纯颜色信号。

**待办（本次明确不做）**：右上状态词仍然只反映 `deployment.enabled`，而服务商停用、binding
停用、端点被出网策略拒绝三种情况同样会让它不承接流量却显示"已启用"；把它变成派生的
「服务中 / 不在路由（原因）/ 已停用」需要后端暴露 registry 的排除原因，属于 C 档范围。
同理，列表页仍无排序与异常优先分组，`需要处理` 筛选的定义（`!testIsCurrent || unhealthy`）
仍然排除了探测失败、能力漂移、缺价格 —— 事故时筛出来的是错的一批，属于 B 档范围。

i18n 删掉五个随之作废的键（`priceSetting`、`setDeploymentPrice`、`contextSummary`、
`maxOutputSummary`、`activeRoutesCompact`），新增结论行与可访问名所需的键（中英同步）。
前端全套 421 项通过（新增 9 项：阶梯 7 项 + 卡片具名与单一 live region + 四槽恒定）。

## 15. 「创建模型部署」对话框：能力从哪来，以及为什么现在它是空的（2026-08-25）

### 15.1 现场

截图里的这次创建：服务商 `AWS-Mantle-OpenAI`（绑定 `/openai/v1` 路由），模型 ID
`deepseek.v3.1`，能力区三段全空（操作 0/1、模态 0/1、特性 0/6），底部红字「至少选择一个
核心操作能力」。

**这个部署即使建出来也调不通。** `deepseek.v3.1` 只在 `/v1` 路由上服务（2026-08-21 逐模型
实测，见 `docs/verification/provider-real-matrix.md`），发到 `/openai/v1` 会被上游以
``model `deepseek.v3.1` isn't supported on this route`` 拒绝，不会回退。表单当前不拦，也不
提示——它把一个「路由选错了」的问题，呈现成了一个「你还没勾能力」的问题。

### 15.2 前置事实：四家上游到底给不给能力元数据

这是决定对话框能做到什么的硬约束，逐个核对过代码与线上返回：

| 服务商 | 目录端点返回什么 | Halro 的映射 |
| --- | --- | --- |
| OpenAI | `id` / `object` / `created` / `owned_by`，无能力、无窗口 | 无。`openai/adapter.go:258` 写死 `MetadataSource: none` |
| DeepSeek | 同上（复用同一 adapter） | 无 |
| Anthropic | **有能力报告**：`image_input`、`thinking`、`structured_outputs`、`batch`、`pdf_input`、`citations`、`code_execution`、`context_management`、`effort` | `anthropic/adapter.go:228`，映射 4 项 → vision / reasoning / json_mode / batches；chat 与 streaming 由「它是 Messages 端点」推得；**工具调用没有字段，因此不 claim** |
| AWS Bedrock 控制面 | **半有**：`inputModalities`、`outputModalities`、`inferenceTypesSupported` | `bedrock/models.go:137`，只能推出 chat 与 vision |
| AWS Bedrock Mantle | 只有 `id` / `owned_by`，连上下文窗口都没有 | `bedrockmantle/adapter.go:134` 明确不实现映射：从标识符推能力是「披着 declared 外衣的猜测」 |
| Gemini | **有**：`supportedGenerationMethods` + `inputTokenLimit` + `outputTokenLimit` | `gemini/adapter.go:239` → chat / streaming / embeddings |
| Azure OpenAI | 数据面没有通用目录 | `modelCatalogURL` 直接拒绝 |

两条与既有结论的衔接，免得这一节读起来像没查过前文：

- **Mantle 拿不到控制面那半份元数据，这是已经查实的死路。** §11.6 F5 记下三重独立阻塞：
  控制平面允许清单只对 `SurfaceBedrockRuntime` 放宽，`ControlPlaneHostFor` 只认
  `bedrock-runtime.` 前缀，而 Mantle 绑定是 `SurfaceBedrockMantle`、主机形如
  `bedrock-mantle.<region>.api.aws`，推不出任何控制平面主机。所以上表 Bedrock 控制面那行
  的「半有」，对 Mantle 服务商**一项都不适用**——它的能力来源只剩内置目录与探测两条。
- **F4 里写的枚举端点已经变了。** 那一期写的是 `operationURL("models")`；2026-08-25 实测
  `/openai/v1/models` 返回 404 而 `/v1/models` 返回 200，枚举与连接测试已改走
  `catalogURL()`（账号级 `/v1`），推理仍按 profile 固定路由。证据见
  `docs/verification/provider-real-matrix.md`「目录是账号级，不是路由级」。

结论：**六家里只有两家给能力，一家给一半，三家什么都不给。** 所以能力永远有四个来源——
上游元数据、内置目录、探测、管理员声明——而对话框现在一处都没有说明某一格是哪来的，空的时候
也不说是为什么空。这不是可选的信息层，这是这个界面唯一要回答的问题。

### 15.3 诊断

**D1 组合列表已经拿到了答案，渲染时把它丢了。** `GET /providers/{id}/invocation-targets`
不带 `binding_id` 时会对服务商的**每个**绑定做一次解析，返回的每个 item 都带
`resolution_state` 与 `variants`（`admin_invocation_targets.go:97`、类型见
`web/src/types.ts:553`）。也就是说「这 50 个模型里哪些这个服务商真能承载、由哪个接口承载」
是**已经在响应体里**的。而选项渲染只有一行 `<strong>{model.display_name}</strong>`
（`DeploymentsPage.tsx:1835`）。列表里 37 个只在 `/v1`、11 个只在 `/openai/v1` 的模型，
外观完全一致。

**D2 「目录不认识它」和「目录认识它，但在另一条路由上」被压成了同一个状态。** 解析只对
服务商已绑定的接口查目录（`admin_invocation_targets.go:515` 的 `mergeKey` 取
`binding.ProfileID`），从不反问「目录里这个模型登记在哪些 profile 下」。于是
`deepseek.v3.1` + 只绑了 `bedrock.mantle.openai.chat.v1` 的服务商 → 无 claim →
`claimsExist` 为假 → `resolution_state = unknown`，操作者收到的建议是「手工声明，或跑一次
识别」。而正确答案目录里就有：这个模型登记在 `bedrock.mantle.chat.v1` 下，操作者要做的是
换一个绑定了那条接口的服务商，一次识别都不用跑。

这两种情况的下一步动作完全相反，压成一个状态之后，界面把操作者推向了错的那一个。

**D3 探测在这个场景下也答不出来，而且要花钱。** 「返回识别」会向 `/openai/v1/chat/completions`
发一次真实调用，被上游按路由拒绝。按 `classifyCapabilityProbeError` 的分类，只有 provider
code 是 `unsupported_parameter` / `unsupported_value` 才判 `unsupported`，其余 `ErrorBadRequest`
一律 `inconclusive`——路由拒绝几乎肯定不在那两个 code 里（**未实测，需验证**）。结果是：
花掉一次可能计费的调用，拿回「无法判定」，而答案本来就在目录里。

**D4 能力区的「空」有三种，界面一视同仁。** ①上游根本不给元数据（Mantle、OpenAI、DeepSeek）；
②目录没有这一条（本次补种后，Mantle 只剩 `zai.glm-4.6`）；③目录有，但被接口 ceiling
夹掉了。三者的下一步不同——第一种只能靠目录或探测，第二种要么换模型要么声明，第三种是接口
选错了。现在三种都渲染成同一片未勾选的方框。

**D5 「至少选择一个核心操作能力」把上游的沉默转写成了操作者的疏忽。** 这句红字出现的时刻，
操作者什么都没做错：他选了一个真实存在的模型，上游不肯说它能做什么。要求应该出现在有能力
可选的时候；没有来源的时候，界面该说的是「谁都没告诉我这个模型能做什么，你可以：换绑定 /
跑识别（会计费）/ 自己声明」。

**D6 Bedrock 控制面给了流式字段，我们没接。** `ListFoundationModels` 的
`responseStreamingSupported` 是明确的布尔量，`bedrock/models.go` 解析时只取了
`inputModalities`、`outputModalities`、`inferenceTypesSupported` 三项。这是一项白给的
declared 证据，成本为零。

### 15.4 方案

**S1 组合列表分档（纯前端，数据已在手）。** 每个选项按其 `resolution_state` 归档，档位在
列表内分组，不隐藏任何模型：

```
可直接创建      解析出唯一变体，选项右侧标出承载它的接口名
需要先声明      unknown —— 目录与上游都没说，创建时要走声明或识别
本服务商不承载  no_variant / covered_elsewhere —— 标出「由 X 接口服务」
```

排序把「可直接创建」放最前。**不隐藏**第三档：操作者是照着 AWS 控制台的名字来找的，
搜不到会以为 Halro 的目录坏了。

**S2 解析补一个状态：`covered_elsewhere`。** 当前服务商的任何绑定都解析不出变体时，再用
（provider_type, target_kind, model）去目录里反查它登记在哪些 profile 下。查到就返回
`covered_elsewhere` 并带上 profile 列表；查不到才是 `unknown`。前端据此把 D2 那句错误的
建议换成一句可执行的：「该模型由 `bedrock.mantle.chat.v1` 服务，当前服务商绑定的是
`bedrock.mantle.openai.chat.v1`；请改选绑定了前者的服务商，或在服务商里启用该接口」，
并给出直达链接。

这条依赖本次刚种进内置目录的 60 条 Mantle 条目——路由信息就是靠它们才在进程内可查的。

**S3 能力区空态按 D4 的三种分别说明**，每种带对应的下一步动作，且写明识别会计费、上限是
`MaxVerificationCalls`（这个值已经在 `discovery.max_verification_calls` 里）。

**S4 每一格能力标出来源。** 四态：上游元数据（declared）、内置目录（declared）、探测
（verified）、管理员声明（operator_declared）。`CapabilityClaim.source` 已经随每个变体返回
（`web/src/types.ts:518`），前端目前没有把它渲染出来。**不预勾**任何没有来源的能力——这条既有约束
本次不动摇，理由写在 `DeploymentsPage.tsx:1292`：从服务商 ceiling 预填，正是当初让部署
claim 出模型并不具备的能力的那个习惯。

**S5 接上 `responseStreamingSupported`**，让 Bedrock 控制面多给一项 streaming 的 declared
证据。

### 15.5 明确不做

- **不按名字猜能力。** 「模型 ID 里有 vision 就勾视觉」这类推断一次都不做，理由与
  `bedrockmantle/adapter.go:134` 相同。
- **不自动跑识别。** 识别是计费动作，只在操作者按下时发生，且事先说明会打几次调用。
- **不因为路由不匹配就禁用保存。** 目录可能过期、操作者可能知道我们不知道的事；给的是
  一条挡在路上的说明加一个直达链接，不是一把锁。
- **不在这一版做价格预填。** 价格是部署级、带来源审计的操作者输入，预填等于替他签字。

### 15.6 分期与验收

| 期 | 内容 | 依赖 |
| --- | --- | --- |
| ~~P0~~ | S1 组合列表分档 | **已完成**（2026-08-26），见 §16.5 |
| ~~P1~~ | S2 `covered_elsewhere` + 指路文案 | **已完成**（2026-08-26），见 §16.5 |
| ~~P2~~ | S3 空态三分 + S4 来源徽标 | **已完成**（2026-08-26），见 §16.6 |
| ~~P3~~ | S5 Bedrock 流式字段 | **已完成**（2026-08-26），见 §16.6 |

验收（每条都要能在没有真实凭据的情况下用测试证明）：

1. 服务商绑定 `/openai/v1`、模型选 `deepseek.v3.1` 时，列表把它归入「本服务商不承载」，
   且解析返回 `covered_elsewhere` 与 `bedrock.mantle.chat.v1`。
2. 同一个服务商选 `openai.gpt-5.6-sol` 时归入「可直接创建」，能力区非空且每格标出来源为
   「内置目录」。
3. `zai.glm-4.6`（目录故意不收，见 `builtin.go` 注释）归入「需要先声明」，空态给出的三个
   动作里包含「自己声明」。
4. Anthropic 服务商上的模型，能力来源标为「上游元数据」，且工具调用**不**被勾上——上游
   没有这个字段。
5. 任何一档下，都没有一个能力是在无来源的情况下被预先勾上的。

### 15.7 待定

- D3 那条需要一次实测：Mantle 按路由拒绝时返回的 provider code 到底是什么。如果它落在
  `unsupported_parameter` / `unsupported_value` 之外（预期如此），那么「路由不匹配」这件事
  探测永远判不出来，S2 就不只是省一次调用，而是唯一能给出答案的路径。
- Anthropic 的 `pdf_input`、`citations`、`code_execution`、`context_management`、`effort`
  五项在 Halro 的能力词典里没有对应项（`anthropic/adapter.go:249` 已记录）。要不要在详情里
  原样展示「上游还声明了这些，Halro 不承载」，属于能力词典演进的范围，另议。

---

## 16. 六角色评审与本轮修复（2026-08-26）

六个角色（领域正确性／运维、信息架构／产品、前端实现、无障碍、后端契约、文档自洽性）
各读一遍本文并**逐条打开代码核对行号**，前提是不采信本文自述 —— 本文此前已有三次
「诊断被推翻」的前科，第四次就在这次被找出来（§1.5）。

四条结论被两个以上角色**独立**发现，那几条是最硬的。

### 16.1 本轮已修

- **未分类的探针错误正文写进日志。** `describeProbeFailure` 无条件写 `failure.Reason`，
  未分类错误走不到 `errors.As`，`Status` 留 0，于是 `probeFailureAttributes` 里
  「无上游状态码 ⇒ Halro 写的 ⇒ 可以记正文」那条判据正好放行 —— 而这条判据需要一个
  已分类的错误才站得住。周期性探针每个间隔重复一次。
  `probeFailure` 增 `Type`，未分类分支记 Go 类型、不记正文；
  `health_probe_log_test.go` 补第三条用例（桩适配器返回一段含 Bedrock key 形状 canary
  的散文，该形状**不在** `safelog` 的模式表里，所以断言不可能靠识别格式通过）。
  这与网关侧同形状的那条修复（`9e9e06b`）是同一条规则的两半。
- **卡片的第五个孩子。** §14.24 声称移出的 `.deployment-card-error` 仍在渲染，且排在
  动作栏之后 —— 正是 `resource-card.css` 契约点名禁止的形状（`grid-row: span 4` +
  `subgrid`，第五个孩子落进隐式轨道，把整行邻居的槽对齐一起打掉）。连同
  `priceBlockedEnable` 对账 effect（它存在的唯一理由就是那个块会滞留）与
  `styles.css` 的规则一并删除。错误改由通知通道单独承担，去价格表单的入口是卡片
  结论行本来就带的 `price` 动作。
  **测试为什么没发现**：`renderPage()` 没有包 `NotificationProvider`，`useNotify` 在
  provider 之外是 no-op，所以整套测试里 toast 从未渲染 —— 双重上报无从暴露。已补上。
  四槽那条断言此前只驱动两条初始渲染的记录，两者都到不了失败的 mutation；现在它
  在一次被拒的写入之后再断言一次，并做过反向验证。
- **一条设计系统断言靠注释通过。** `design-system.test.ts` 断言
  `.resource-card-actions` 匹配 `margin-top: auto`，而该声明早已换成 `align-self: end`
  —— 正则命中的是同一规则体里解释这次替换的注释。守住「动作栏贴底」的唯一断言在守
  一段散文。新增 `ruleBody()`：取规则体时先剥注释，五处规则体断言全部改走它，
  并同时断言旧声明确实不在。
- **「需要处理」筛出来的是错的一批。** 定义 `!testIsCurrent || last_test_status === "unhealthy"`
  的第二个子句恒被第一个吞掉，等价于「没有当前版本的通过测试」：漂移、探测未通过、
  价格隔离 —— `deploymentCondition.ts` 判为 `blocked` 的三项一条都筛不出来，而一个
  刚建好、只是没测过的部署反而在里面。改为复用同一条阶梯
  （新增 `deploymentNeedsAttention()` 与 `recordedTestState()`），卡片与筛选从此读同一份
  定义。两个价格档次留在卡片：价格是按部署读的，列表没有那次读取，**筛选宁可比卡片
  少说一项，也不猜**。
- **列表静默截断。** `api.deployments` / `api.providers` / `api.routes` 都丢 `next_cursor`，
  服务端默认每页 50。派生事实因此说谎：`activeRouteCounts` 建在被截断的路由上，而它是
  「停用」「删除」的 `disabledReason`；计数文案 `显示 {{visible}} / {{total}}` 里的
  `total` 就是那 50。仓库里本来就有为这件事写的 fail-closed 版本 `allRoutes`
  （注释：a truncated answer must never look like a complete one），只有项目页在用 ——
  一件事的两个答案并存，按 pre-1.0 的规矩不该留。抽成 `listAll()` / `pageOfAll()`，
  三个列表全部改走它，`allRoutes` 删除，项目页改读 `["routes"]`。

### 16.2 本轮的文档止血

- §1.5 就地改写：那个「新发现的枚举路由缺陷」本身是错的诊断，`0c886fa` 用真实账号
  实测撤销了它（目录账号级、推理路由级，两个地址本就不该统一）。
- §5 缺口表删掉已关闭的 D1／D6／D8，并在表头写明「已关闭就地删除，不留删除线」。
- §7 的 P1b 改成「已撤销」，P2a 改成「已完成」，并写明这张表只讲那一期的三件止痛，
  不是全文进度表。
- §11.6 F4 补一句枚举端点已改走 `catalogURL()`。
- §12.1 的已完成清单去掉 P1b 与 D8。
- 删除 §12.3 整节 —— 它论证 P2a 与 P3「未开工」，而两者当天就做完了，全节五段今天
  一段不成立。

### 16.3 刻意没做（是设计决定，不是缺陷）

- **严重度阶梯的排序与真实停流量顺序不符。** 阶梯自称按「多快会停止承接流量」排，
  实测：默认配置下**没有生效价格**的部署每次请求都被 409 拒绝，阶梯给的是 `warn`；
  **手动测试失败**根本不参与路由判定，阶梯给的是 `blocked`；**无启用路由**是绝对零
  流量，根本不在阶梯里（还被 §14.24 主动移出了卡片）。另有四条 registry 扣留原因
  （区域不匹配、`ValidateProfileModel` 拒绝、能力集为空、价格读取失败）也不在。
  改档位会改变卡片上每一张的结论，属于产品决定；要么改档位，要么把那句注释改成实话。
- **§15 的 S2 按字面实现会拿走操作者唯一的出路。** `declaredModel` 完全绑在
  `resolution_state === "unknown"` 上，驱动手工声明面板、保存放行与那句红字的显示条件。
  把 `deepseek.v3.1` 改判 `covered_elsewhere` 后这几处全变假：声明面板消失，而红字反而
  常驻 —— §15.1 抱怨的症状会被放大，且与 §15.5「不因为路由不匹配就禁用保存」冲突。
  落地时 `declaredModel` 必须是 `unknown || covered_elsewhere`，`covered_elsewhere`
  只加一条指路横幅，不夺走声明路径。现有五条验收没有一条覆盖它，要补。
  另外 S2 的触发条件写的是「任何绑定都解析不出变体时」，按字面会把 `conflicting`
  也一起改写成 `covered_elsewhere`，把一个需要人看的冲突掩盖成一句「换个服务商」——
  要写死成只在 `unknown` 上触发。

### 16.4 评审查出、本轮未处理

- **暗色卡片上的「已声明」模态标记差 0.04 不达 AA**：`#82958f` on `#16302a` = 4.459:1，
  门槛 4.5。整条规格行同色。测不出来的原因很具体：`design-system.test.ts` 的对比度用例
  只枚举 `--color-canvas` 与 `--color-surface-default` 两个背景，**卡片实际用的
  `--color-surface-raised` 不在清单里**。
- **`Registry.health` 单调增长**：`Replace` 无条件搬回旧条目，全仓无 `delete`，已删部署
  会永远继续输出 `halro_deployment_up{deployment_id=...}`。
- **`h3` 升级只做了一半**：设计系统只给 `.resource-identity > strong` 定型，`h3` 落回
  全局 18px 并丢掉 ellipsis 钳制，长部署名换行 —— §14.24「槽 1 恒定两行」的前提失效。
- **§3.1 那条安全建议没做，而注释说做了**：`admin_invocation_targets.go` 写着
  「it never reaches a Provider」，紧接着缓存未命中就穿透；`requireAdmin` 不查角色，
  `AdminRoleReadOnly` 真实存在。§15 的 S1 正要把这条 GET 变成主数据源，先决定它。
- **i18n 未引用键检查的洞**：子串匹配藏住 9 个键，279 个键只靠动态前缀过关（其中 29 个
  靠一段式前缀），前缀提取扫的是所有模板字面量而不只是 `t()`。
- **px 棘轮又攒出 26 单位松弛**（基线 724，实际 698）。上次犯这个错时是 4 个单位。
- **⋯ 菜单没有最上层守卫**：菜单内开确认弹窗后按 ESC，Modal 与 `<details>` 同时响应，
  焦点归还给一个已被隐藏的按钮，落到 `<body>`。Modal 有这个守卫，OverflowMenu 没有。
- **`formatAge()` 为可测性加的 `now` 入参，唯一调用点不传**，且该函数零测试。
- **真实浏览器视觉核对**：这笔账记了三次、还了零次。评审的判断是它的**记法**已经失真 ——
  §14.15 之后的每一轮都是操作者看着实物驱动的，也就是说人已经在当那个核对员。
  该结掉，或者改写成它真正缺的那样东西：**没有自动化视觉回归**。

### 16.5 第二轮（2026-08-26）：§11.3、§8、以及 §15 的 P0 与 P1

**§11.3 的 529 —— 已修。** `renderAnthropicGatewayError` 接受 `ProviderCode ==
"overloaded_error"` 作为过载证据，于是一个上游写进响应体的字符串能决定 Halro 返回给
客户端的 HTTP 状态。而 Mantle Responses 的流式错误事件把 `ProviderCode` 直接取自事件、
**不带任何状态码**，所以一个自称 `overloaded_error` 的事件会把 502 改写成一个上游从未
发过的 529 —— 而 529 正是 SDK 用来决定「退避重试」的那个码。改为只读状态行。
这条建议从 08-24 起记在 F1 的附注里，此后 15 节没有一处再提。

**§8 的上限铁律 —— 已补。** 那条铁律此前只有 bedrockmantle 一家执行（`maxCatalogEntries
= 2000`），另外四家适配器与三个展示字段都只靠 16 MiB 的 body 预算兜着 —— 那个尺寸装得下
几十万条 `{"id":…,"owned_by":…}`。上限放到所有适配器唯一的交汇点
`normalizeInvocationTargets`：条目数超限**拒绝而不是截断**（截断出来的前缀与完整列表
不可区分），`DisplayName` / `OwnedBy` 超长则只丢标签不丢模型 —— 丢掉模型会让操作者找不到
他专程来找的那一个。顺带把 §8 里「`Region` 没有上限」那条删了：它在任何适配器里都不是
上游可控量（bedrock 从自己的 endpoint 主机名推）。

**结论行阶梯改档（操作者 2026-08-26 决定）。** 阶梯自称按「多快会停止承接流量」排，实测
三处不符。改后：`未设置价格` 升到 blocked（默认成本治理策略下每次请求都被 409 拒绝），
`测试失败` 降到 warn（`last_test_status` 不参与任何路由判定），新增 `已禁用` 与
`无启用路由` 两档 neutral —— 两者都是绝对零流量，此前完全不在阶梯里，而它们是操作者的
选择不是故障，所以不上警示色。
连带删掉动作栏末尾的状态词：结论行现在会说「已禁用」，两处就是同一张卡上说两遍 ——
§14.24 删 `StatusDot` 用的正是这条理由。§14.24 待办里「状态词只反映 `enabled`」那条
随之消失（服务商停用／binding 停用／出网拒绝三种会在一个探测周期内落到「主动探测未通过」）。

**§15 P0（S1 组合列表分档）—— 已完成。** 目录 GET 本来就对每个绑定做一次解析、每个条目
都带 `resolution_state`，而选项只渲染一行 `display_name`，于是「能直接建」和「这个服务商
根本不承载」长得一模一样，操作者填完表单才知道。现在按**下一步动作**分五档，
`resolved` 在最前。**分档不过滤**：操作者是照着上游自己的控制台抄的名字来找的，搜不到会
读成 Halro 的目录坏了。
无障碍上顺带补了三处既有欠账：档名放在 `role="group"` 上而不只是一行小字（只有视觉的
分档对屏幕阅读器等于没说）；计数条与空态移出 `listbox`（listbox 只该拥有 option 与 group）；
选项加 `tabIndex={-1}`（此前 Shift+Tab 能落进 popup，而 `aria-activedescendant` 还说焦点
在输入框上）以及 `aria-setsize` / `aria-posinset`（分组后浏览器推导的计数会以 group 为界）。

**§15 P1（S2 `covered_elsewhere`）—— 已完成。** `modelcatalog.Catalog` 加了一个按
（provider type, target kind, model）的反查索引（在 `New()` 里建，O(1)，纯进程内，
不发起任何上游调用），`ProfilesCovering` 回答「目录把这个模型登记在哪些接口下」。
解析梯**只在 `unknown` 上**升级为 `covered_elsewhere` —— `conflicting` 需要人看，
`no_variant` 已经知道是哪个绑定不够，把它们改写掉是拿一个能行动的答案换一个对它不成立的。
消费点四处同步：`boundedResolutionStatus` 允许列表（漏了会静默降级成 `"unknown"`）、
审计分支、前端联合类型、i18n。

**其中一条是既有测试抓出来的真实错误。** 第一版反查用 `instance.Type` 直接查，于是一个
OpenAI 兼容端点自称 `gpt-5` 时被判成「由另一套接口服务」——
`TestCompatibleTargetNameDoesNotImplyCanonicalCapabilityIdentity` 当场失败。逐绑定那条
路径本来就刻意不按名字读目录（`invocationTargetCatalogEntryWithCatalog` 里那个分支），
反查绕过了同一条规则。`coverageModel` 现在镜像它：没有显式 canonical 映射的兼容端点模型
返回空串，反查不问。新增一条测试钉住这件事。

**前端最关键的一条：`covered_elsewhere` 不得夺走手工声明。** `declaredModel` 此前完全
绑在 `resolution_state === "unknown"` 上，驱动手工声明面板、保存放行与那句红字的显示条件；
按字面实现 S2 会让这四处全变假 —— 声明面板消失，红字反而常驻，正是 §15.1 抱怨的症状被
放大。现在 `declaredModel` 是 `unknown || covered_elsewhere`，`covered_elsewhere` 只多加
一条指路横幅（点名承载它的接口 + 直达服务商页），不关任何门。这条做了反向验证：
把判断收窄回 `unknown`，「手动配置」按钮当场消失。

每一项都做了反向验证，搜索串都断言过命中数。门禁：`go vet ./...` 与 `go test ./...`
全绿（65 个包），前端 `typecheck` + **430 项** + `npm run build`（密钥扫描 27 文件干净）。

**§16.4 的清单未动**，仍然欠着；`§15` 的 P2（S3 空态三分 + S4 来源徽标）与 P3
（S5 Bedrock 流式字段）未开工。

### 16.6 第三轮（2026-08-26）：§16.4 的欠账与 §15 的 P2、P3

§16.4 那张清单除最后一条外全部补完，§15 四期做完。

**对比度（暗色卡片）。** `--color-text-tertiary` 在卡片上是 4.459:1，差 0.04 不达 AA。
真正的缺陷在检查本身：对比度用例只枚举 `--color-canvas` 与 `--color-surface-default`
两个背景，而卡片画在 `--color-surface-raised` 上，于是这块屏幕根本不在检查范围内。
把 raised 加进去之后**当场又暴露第二条同类缺陷**：卡片上的强边框只有 2.42:1，低于
非文本 3:1。两条都修了 —— `--p-ink-300` 提三档到 `#859892`（4.63:1）；边框不能动
`--p-ink-400`（它同时是亮色主题的三级文字，提亮会**削弱**那边的对比度），新增
`--p-ink-350` 只给暗色边框用。

**px 棘轮。** 基线 724、实际 698，26 个单位的松弛。降到实际值，并把断言从
`toBeLessThanOrEqual` 改成 `toBe`：这个错误犯过两次（上次 4 个单位），而上限型棘轮
只有在有人记得往下调时才是棘轮。

**`h3` 排版契约。** `.resource-identity > h3` 补进设计系统那条规则（连同 `margin: 0`）。
此前 h3 落回全局 18px 并丢掉 ellipsis 钳制，长部署名会把身份槽撑成两行 ——
§14.24「槽 1 恒定两行」的前提本来是假的。

**`Registry.health` 单调增长。** 新增 `RetainDeploymentProbes`，由探针循环调用 ——
它读的是 store 里的部署列表，是唯一知道哪些 ID 还真实存在的地方。`Replace` 的搬运
行为不动（那是对的：重载不该把健康的部署报成未探测）。此前已删除的部署会一直留着
最后的判决，`halro_deployment_up` 也一直为一个不存在的 ID 输出标签。

**⋯ 菜单缺最上层守卫。** 菜单内开确认弹窗后按 ESC，两者都在 document 上监听：菜单先
关，弹窗随后把焦点还给一个刚被 `<details>` 隐藏的按钮，于是焦点落到 `<body>`。加上与
`Modal` 同款的判断（有 `.modal-backdrop` 就不响应），指针事件同理。

**`formatAge`。** 那个为可测性加的 `now` 入参，此前零测试。补了每一档单位的边界、
「小于一分钟说此刻」、以及「浏览器时钟快几秒不得渲染成未来」那条规则；超过一分钟的
未来是真实信息，保留符号。调用点仍在渲染时读时钟 —— 入参存在是为了让这些断言成立，
而现在它们存在了。

**§3.1 只读会话触发计费枚举 —— 已堵。** `listAdminInvocationTargets` 上面写着
「it never reaches a Provider」，而缓存未命中会直接穿透；这条 GET 只过 `requireAdmin`，
不校验角色，而 `AdminRoleReadOnly` 真实存在。现在未命中就地返回 `not_cached: true`，
不拨号；拨号是隔壁那个 POST（`requireAdminMutation`）。注释改成实话。
`not_cached` 是独立信号而不是 `error_class`：上游根本没被访问，用 provider 错误类
描述它是范畴错误。控制台把「没取过」与「取不到」分开 —— 前者一次点击可解且要花钱，
后者是故障。

**i18n 未引用键检查。** 两个真洞修了：动态前缀只从 `t(` 开头的模板里提取（此前扫的是
全部模板字面量，一处 `curl ${…}` 就贡献了一个前缀），字面匹配加了键尾边界（此前
`deployments.evidence` 因为 `deployments.evidenceValues.` 存在而被算成已引用，另有
八个同样被藏住）。**没有**按评审建议拒绝一段式前缀 —— 实测那些前缀（`custody.`、
`navigation.` 等）确实出自真实的 `t()` 调用，判它们死会制造假阳性，比原来的洞更糟；
这是一条广的引用，收窄它要改调用点，不是改这个扫描。
暴露出的 9 个键逐个核实为零引用，按 pre-1.0 规矩**删除**而不是记进台账。

**§15 P3（S5）。** `foundationModelSummary` 接上 `responseStreamingSupported`，
`NormalizedModelMetadata` 加 `ResponseStreaming *bool`。**用指针不用 bool**：缺失与
false 是两个不同的答案，只有一个是上游给的；只有显式 true 才产 declared claim，
显式 false 是上游拒绝、缺失是上游没说，两者都不是能力成立的证据。

**§15 P2（S3 + S4）。** 空态按三种原因分开，各配各自的下一步：目录里有它但不在这条
接口上（识别答不出，所以那块**不显示**计费上限）、上游只给标识（再刷新也不会多出能力）、
内置目录没有这一条。原来一个面板用「未收录模型 · 需要识别」回答全部三种 —— 对第一种
是错的（目录明明有），对第二种是把人送回刷新按钮。
S4 逐项标来源，但只在各项**不一致**时出现：一列八行相同的「内置模型目录」不是可比较的
信息，段头已经说过一次 —— 这是 §14.15-B4 立的规矩，这里照办。

**顺带（操作者要求）**：「这个能力接口载不动的请求成员」那块去掉边框与底色，
用留白分组 —— 与 §14.22 第 1 条同一条理由。

门禁：`go vet ./...` 与 `go test ./...` 全绿；前端 `typecheck` + **439 项** +
`npm run build`（密钥扫描 27 文件干净）。

**仍然欠着，且这次也没还：真实浏览器里的视觉核对。** 本轮改了颜色 token、卡片密度、
组合框结构，全部是只有看才能验的东西，而本地实例仍需管理员口令登录。
按评审的判断，这笔账的记法本身已经失真 —— 它真正缺的不是某一次核对，而是
**没有自动化视觉回归**。要么补一套，要么由操作者跑一次 `make dev` 自己看。

### 16.7 能力编辑器的上限取错了（2026-08-26）

操作者：「高级设置中列出全面的能力，匹配的勾选、没有匹配的不勾选；漏选用户可以自己选。」

**症状**：`AWSBedrock` 上选 `openai.gpt-5.6-terra`，能力区只有「对话」「流式」两行。
视觉根本没出现 —— 不是灰掉，是没有那一行，所以想勾也勾不上。

**为什么是两行**：内置目录里 Mantle 那批条目用的是 `mantleChat()`
（`builtin.go:394`），全文只有 `Chat: true, Streaming: true`。种子注释写得很清楚：
路由归属与 Responses 支持是 08-21 实测的，窗口与最大输出是 08-25 从账号模型列表读的，
**每个模型的能力本身从未查证**；唯一例外是 `openai.gpt-5.5`，因为只有它的 card 被读过。
接口上限 `mantleOpenAIChatSet` 本身是带 `Vision: true` 的，所以拦住它的不是接口。

顺带一提，同一个模型在 OpenAI 服务商下有一张完整的卡（`builtin.go:196`，带 vision 与
reasoning）。目录按 **(服务商类型, 接口, 模型)** 做键，一条路上查证过的证据不会带到另一条路上。

**真正的缺陷在编辑器**：`CapabilitySubsetEditor` 的 `ceiling` 传的是
`selectedVariant.capabilities` —— 目录声明的那一小撮。于是「上限」和「已声明」是同一个值，
而行列表取的是窄的那个。**改为两个值分开**：

- `ceiling` = `bindingCeiling`（这条连接开了什么）→ 决定**有哪些行**
- `claimed` = 变体的声明 → 决定**哪些被勾上**

从上限预填仍然不做（`DeploymentsPage.tsx:1292` 那条既有约束），未记录的行标一句
「目录未记录」——它与「查过了、不支持」不是一回事。
选 `bindingCeiling` 而不是 profile 能服务的全集：连接没开的能力要去服务商页开，
下面的手动编辑器已经这么说了。

**保存要跟着换路**：变体路径会拒绝超出声明的能力
（`admin_deployments.go:943`「deployment capabilities exceed the selected deployment variant」），
所以勾了未记录的能力时 `mode: "operator_declared"`，并**去掉** `resolution_revision`——
同时送这两样，等于请求服务端既守住那个 pin 又打破它。
后果在保存前说明（新的提示条：证据会记成「管理员声明」而不是「已评审模型目录」；
上游真不支持的话，网关在调用时才会被拒；也可以改用识别实测，会计费）。

三条测试钉住：行来自接口而勾选来自目录、未记录的行带标记且可勾、勾了之后
`mode` 变成 `operator_declared` 且不带 `resolution_revision`。三条都做了反向验证。
前端全套 441 项通过。
