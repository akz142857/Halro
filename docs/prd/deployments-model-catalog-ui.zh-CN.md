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

### 1.5 新发现：Mantle chat 的枚举路由是错的

`modelCatalogURL()` 不套 `operationPathPrefix`：

```go
func (a *Adapter) modelCatalogURL() (url.URL, error) {
    …
    endpoint.Path = versionedPath(basePath, "models")   // 未使用 a.operationPathPrefix
```
（`internal/provider/openai/adapter.go:151-161`）

而同一个适配器的 `operationURL` 是套的（`:543-550`）。Mantle 的
`bedrock.mantle.openai.chat.v1` 带 `openai/v1` 前缀（`app/providers.go:759-770`），
于是它的模型枚举会打到 `<base>/v1/models` 而不是 `<base>/openai/v1/models` ——
发现与它服务的操作对不上同一个 URL，正是那段注释声称要避免的事。

这条第一版完全没看到，且它会让 F4 交付一个"Responses 能枚举、chat 静默走错路由"的
不一致状态，必须一起修。

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

| # | 缺口 | 位置 | 影响 |
| --- | --- | --- | --- |
| D1 | Mantle Responses profile 无模型枚举 | `provider/bedrockmantle/adapter.go` | 该 profile 下模型不进目录 |
| D2 | OpenAI 家族目录只有标识，无模态 | `provider/openai/adapter.go:206-222` | 这条路上模态行只能是"未知"（**注意 Anthropic 不在此列**，见 §1.4） |
| D3 | 无模型描述文案 | 全链路 | AWS 抽屉那段简介无对应数据 |
| D4 | 无服务商 logo | 全链路 | 卡片左上角图标无对应数据 |
| D5 | 目录层无价格 | 价格挂在部署上 | 目录卡片显示不了参考价 |
| D6 | `reasoning` 无探针 | `provider/capability_detection.go` | 面板需要 `not_probed` 承接 |
| **D7** | **部署不留存探针级结果** | `web/src/types.ts:465`、`:333` | 见下 |
| ~~**D8**~~ | ~~`modelCatalogURL` 忽略路由前缀~~ | `provider/openai/adapter.go` | **已修复**（P1b） |

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
| ~~P1b~~ | D8：`modelCatalogURL` 套上 `operationPathPrefix` | **已完成** —— 枚举与操作走同一路由，测试同时断言两者一致 |
| ~~P1c~~ | F4：Mantle Responses profile 加 `ListInvocationTargets` | **已完成** —— 断言了不实现 `ProviderMetadataMapper`，条目上限 2000 |
| ~~P2a~~ | 抽屉组件 | **不做** —— 操作者决定保留现有弹窗做添加/编辑 |
| ~~P2b~~ | 模态映射放进 `internal/domain` 加单测 | **已完成** —— `capability_modality.go`，经服务商 profiles 端点下发，golden 已更新 |
| ~~P3~~ | 卡片网格 | **已完成** —— 新增 `resource-card.css` 契约，漂移/路由/价格/状态词全部保留在卡片上 |
| 不做 | 「目录中可添加」分段 | 见 §3.1 |
| ~~待定~~ | F6 已完成；F5 受阻不做 | 见 §11 |

**P0a 加 P0b 加 P0c 合计约两天，交付全部已陈述的止痛。** 抽屉与卡片是另一件事，
建议等有值得画的模态数据之后，作为独立文档重开。

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
- **上游可控的展示字符串必须有渲染上限。** `normalizeInvocationTargets` 把
  `TargetID` 截到 2048，但 `DisplayName`、`OwnedBy`、`Region` 和**条目数**
  都没有上限（`internal/app/admin_invocation_targets.go:702-730`）。

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
`ListInvocationTargets`，读 `operationURL("models")`（`Probe` 已在打这个端点，
`adapter.go:93`），并声明 `CanEnumerate`、`CanDescribe`、`CanVerify`。
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

P0a/P0b/P0c、P1a/P1b/P1c、P2b，以及 F0、F2a、F2b、F3、F4、F6、F7 与 D8。要点：

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

### 12.3 P2a（抽屉）与 P3（卡片网格）：未开工，理由

这两项**没有做**，不是因为工作量，而是因为它们各自卡在一个本文自己列为待决的问题上：

- **D7 是硬阻塞。** 已保存的 `Deployment` 只带三档 `capability_evidence`，
  七值探针状态只挂在 `ModelCapabilityDetection` 上，**没有按部署返回探针结果的端点**。
  所以"点部署卡片 → 面板展开完整状态矩阵"目前不可实现。要么在 Deployment 上
  留存 detection 引用并加端点，要么把完整矩阵限定在创建流程、部署面板只给三档 ——
  这是产品决定，不是实现细节。
- **抽屉的模态性要先定。** §6.1 已经指出 `aria-modal` 加 Tab 陷阱与"不阻塞、
  可与列表同时可见"直接冲突，而 Modal 现有的陷阱实现与它的标记深度耦合、
  不可直接调用；且整个控制台没有滚动锁定。定成非阻塞就要抽
  `useDismissable()` 并让 Modal 一起用，这是一次 Modal 重构。
- **P3 还压着 §6.8 的契约问题**：卡片会让部署页成为唯一脱离 `resource-list.css`
  的列表，而那个文件存在的目的正是防止这种漂移。
- **§10 的四个问题里有两个会改变行为**，不是布局问题：漂移部署在抽屉里怎么呈现；
  重新识别会不会覆盖操作者手动声明的能力（`resetDetection()` 与
  `setManualDeclaration(true)` 成对出现，暗示互斥，但没有任何地方写明谁赢）。

按四份评审的一致意见：止痛已经交付完毕，剩下的是一次控制台重设计，
建议等这些决定有了答案、且有值得画的模态数据之后，作为独立文档重开。

---

## 13. 卡片网格（P3）落地

抽屉（P2a）按操作者决定**不做**：添加与编辑继续用现有弹窗。改的只有列表形态。

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
