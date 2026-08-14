# 适配链条的未完成项

状态：**§1 八条全部已修（2026-08-14）；§2 四项等外部条件；§3 是一处撤销的记录**
建立日期：2026-08-14
来源：[`provider-adaptation-gaps`](provider-adaptation-gaps.zh-CN.md) §5.2 与「未完成项总览」、[`anthropic-batches-plan`](anthropic-batches-plan.zh-CN.md) §5
范围：`internal/provider/{anthropic,bedrock}`、`internal/compatibility`、`internal/gateway`、`internal/app`

这份文档只收**还没做完的事**。两份来源文档记的是「怎么决定的」，读起来要连着历史一起读；
这里要回答的是另一个问题：**现在还剩什么、哪些今天就能动手、哪些在等外部条件。**
每一条都在 2026-08-14 重新对着代码核过，不是照抄记录——其中一条核下来发现记录本身已经失效，
见 §3。

分类的轴是「卡在什么上」，不是优先级：优先级会变，阻塞条件不会。

---

## 1. 今天就能改的（七条代码缺陷 + 一条控制台缺陷）—— 已全部修完

全部来自 2026-08-14 的四角色适配评审（`provider-adaptation-gaps` §5.2），当时只修了三条，
这七条按建议顺序留着。第八条来自批处理首次真实运行。

**同日全部修完**，每条都先写一个在缺陷状态下会红的测试，再做反向验证（退回旧行为确认变红、
恢复后转绿，`-count=1` 跑，且每次都断言脚本替换真的落了）。逐条的修法写在各小节末尾。

§1.7 一度有一处覆盖缺口：策略在 `safetransport` 客户端里不可回读，能观测它的只有真实出站连接，
所以「派生结果有没有真的进到那条连接的允许列表」无法断言。**已补齐**，做法见 §1.7 末尾。

### 1.1 Anthropic 流式的终止原因用错词表 — 已修

`internal/provider/anthropic/adapter.go:843` 的 `anthropicwireDecodeStop` 返回
`stop` / `length` / `tool_calls` / `content_filter`——**OpenAI 的线协议词表**。而下游一律读语义词表
（`complete` / `max_output` / `tool_call` / `refusal` / `unknown`）。同一个缺陷在非流式的
`decodeStopReason`（`internal/compatibility/anthropic/mapping.go:483`）已经修过，注释里写明
「曾返回 OpenAI 词表，那是 bug」，流式这一份漏掉了。

核实过的三个后果：

- **`/v1/responses` 流式路由到 Anthropic 必然 502**，而且是在字节已经吐给调用方之后。
  `internal/compatibility/openai/responses.go:355` 只接受 `complete` 与 `max_output`，
  收到 `stop` 就返回 "provider stream termination is unsupported by the Phase 1A text contract"。
- **OpenAI 兼容流式的 `finish_reason` 恒为 `null`**。`renderTermination`
  （`internal/compatibility/openai/mapping.go:534`）不认识 `stop`，落到 `default` 返回空串。
- **`/v1/messages` portable 流式把被截断的回答说成正常结束**。`renderStopReason`
  （`internal/compatibility/anthropic/mapping.go:219`）不认识 `length`，落到 `default` 返回
  `end_turn`。未知的 `stop_reason` 也被压平成正常结束，而非流式那一份是刻意返回 `unknown` 的。

改法：删掉 `anthropicwireDecodeStop`，把 `decodeStopReason` 导出给适配器用。**不是在旁边再写一份正确的**
——同一状态两条写入路径正是这批缺陷的成因（见来源文档 §5.3 模式 A）。

### 1.2 `max_completion_tokens` 对 Anthropic 系被替换成 1024 — 已修

`internal/compatibility/anthropic/mapping.go:258-263` 只读 `VisibleOutputTokenLimit`，为 0 时填 1024。
Bedrock 的同类渲染优先取 `MaxCompletionTokens`（`internal/provider/bedrock/adapter.go:491-495`）。

这不是丢弃，是**替换成调用方从未写过的、可能大 16 倍的值**：调用方写 `max_completion_tokens: 64`，
上游收到 `max_tokens: 1024`。`/v1/responses` 的 `max_output_tokens` 走的正是
`CompletionTokenLimit`（`internal/compatibility/openai/responses.go:24`），而那是该面唯一的输出上限。

改法：先 `CompletionTokenLimit`，再 `VisibleOutputTokenLimit`，两者都没有时才用兜底值——
Anthropic 的 `max_tokens` 是必填字段，兜底本身要保留。

### 1.3 `parallel_tool_calls: false` 在无 `tool_choice` 时静默丢失 — 已修

`internal/compatibility/anthropic/mapping.go:296-305` 只在 `request.ToolChoice != nil` 的分支里
渲染 `disable_parallel_tool_use`。调用方只写 `parallel_tool_calls: false` 而不写 `tool_choice` 时，
这个约束整个消失，且没有申报为不支持——`Requirements.ParallelTools` 被派生出来却无人消费。

改法：有工具时按 `tool_choice: {"type":"auto","disable_parallel_tool_use":true}` 渲染，
`auto` 正是 Anthropic 在带工具而未指定选择时的默认值，所以这不是替调用方做决定。

### 1.4 portable 模式下 content block 的 `cache_control` / `tool_result.is_error` 静默丢弃 — 已修

同一个请求体内自相矛盾：`tools[]` 上的 `cache_control` 返回 400
（`internal/compatibility/anthropic/mapping.go:54`，理由写着「静默丢弃会产生调用方没发过的请求」），
而消息块上的同名字段在 `decodeMessage`（`:122-146`）里连看都不看。

`is_error` 的后果更实际：**出错的工具结果被当成成功结果喂给模型**。

改法分两半，因为两个字段的性质不同：

- `cache_control` 与其他未建模成员：给 `anthropicapi.ContentBlock` 加 `UnknownMembers()`，
  与 `Tool.UnknownMembers()` 同形（`internal/anthropicapi/types.go:46`），portable 下拒绝。
- `is_error`：它是可以承载的——给 `semantic.Content` 加一个字段，Anthropic 渲染侧写回去；
  承载不了的 profile 按 `messages[].name` 的既有先例在 `UnsupportedGenerateFields` 里申报，
  由路由过滤而不是静默丢弃。

### 1.5 rerank 分数恒为 0 — 已修

`provider.RerankItem` 的 tag 是 `relevance_score`（`internal/provider/inference_resources.go:168`），
AWS 返回的是 `relevanceScore`。下划线让 Go 的大小写不敏感回退也匹配不上，于是每一条结果的分数都是 0。
校验 `< 0 || > 1` 放行 0（`internal/provider/bedrock/inference_resources.go:123`），fail-open。

**仓库自己的 fixture 用的正是 AWS 的真实形状，但断言只检查 `index`、从不看分数**，所以测试一直绿。

根因是一个结构体同时当 AWS 解码目标与北向 wire 形状（来源文档 §5.3 模式 B）。改法就是把两者拆开：
Bedrock 侧用自己的解码结构体，`provider.RerankItem` 只保留北向形状（Cohere 与 Halro 的北向端点都用
`relevance_score`，这一半是对的）。测试补上分数断言。

### 1.6 `Registry.Register` 交集为空时回退到 adapter 全量能力 — 已修

`internal/provider/provider.go:340-348`：`!target.Capabilities.AnyOperation()` 时改用
`reporter.Capabilities()`。这把「交集为空」读成了「调用方没填」。

可达路径：`deploymentCapabilities`（`internal/app/providers.go:739-743`）返回的是
adapter 能力与 Deployment 声明能力的逐项与。一个只声明了 rerank 的 Deployment 挂在 Anthropic
适配器上，交集全 false，于是 `Register` 把它当成「没填」，转而授予 adapter 的全部能力——
Deployment 从没声明过的 chat、tools 全部生效。违反 fail-closed。

改法：能报告自身能力的 adapter，其目标能力为空即拒绝注册；`{Chat, Streaming, Embeddings}` 这个兜底
只留给不实现 `CapabilityReporter` 的测试与扩展适配器（那正是它注释里说的用途）。

app 侧不必另改：注册失败已经落到既有的 `withheldTargetRejected`，也就是记一条 dangling
再跳过这一个 target——降级排除而不是拒绝加载，与相邻检查的处理一致。

### 1.7 Bedrock 控制面模型发现被自身 host 白名单挡住 — 已修

`internal/provider/bedrock/models.go:215` 的 `controlPlaneEndpointFor` 把
`bedrock-runtime.<region>.amazonaws.com` 派生成 `bedrock.<region>.amazonaws.com`，用来列模型；
但发请求用的是数据面 client（`models.go:98`），而它的 SafeTransport 策略
`AllowedHosts` 只有 provider 记录里的那一个 runtime host
（`internal/app/providers.go:377`，值来自 `internal/app/admin_providers.go:1217`）。

不是绕过，是 fail-closed 过头导致功能静默失效：模型发现永远拿不到结果。

改法：为 bedrock-runtime 的绑定，把派生出的控制面 host 一并放进该 client 的策略。**只放派生结果**
——派生函数已经把「不是已批准的公共 runtime 形式就不给控制面」写死了，这条性质必须保住。

**覆盖问题一并解决了**（2026-08-14 第二轮）：

1. `safetransport` 给客户端加了一层 `pinnedTransport`，把归一化后的 `Policy` 存在上面，
   并提供 `PolicyOf(*http.Client) (Policy, bool)`。只读，返回深拷贝——持有答案的人不能反过来
   放宽一条正在跑的连接；非 Halro 构造的客户端回 `false`，不能被当成「空允许列表」
   （空列表等于放行所有通过地址检查的 host）。请求路径没有任何改变，包的其余部分照旧。
2. app 侧把派生折进 `newBindingClient`——**绑定的客户端只有这一条构造路径**，
   派生不再是一个可以忘记调用的独立步骤。
3. 测试断言的是**真正建出来的那个客户端**：runtime 绑定的允许列表有两个 host，
   agent-runtime、PrivateLink、Mantle 三种情形都只有一个，且调用方传入的策略没被就地改写。
   反向验证这次会红（删掉派生 → `the control plane host is not dialable`）。

`transport_test.go` 里那处 `client.Transport.(*http.Transport)` 断言改成穿过包装再看
`Proxy`，因为它要问的是底下那个真 transport。

### 1.8 `POST /v1/files` 返回的 `created_at` 恒为 0 — 已修

`internal/gateway/inference_resources_store.go:281` 的本地文件分支构造 `provider.FileObject`
时没有 `CreatedAt`。注意 `localFileObject`（`:375`）**有**填，所以 GET 是对的、POST 是 0 ——
来源文档只记了后者，实际是创建响应这一处。

北向形状照抄 OpenAI，那里这是文件创建时间戳，客户端会拿它排序和判新旧。

---

## 2. 等外部条件的（四项）

这四项今天动手也验证不了，记清楚触发条件比排优先级有用。

| 项 | 卡在什么上 | 触发后要做什么 |
|---|---|---|
| Anthropic 批处理端到端 | 上游批处理还没跑完 | 批处理 `batch_qkqmxerjvwytsqk9prsbze0yb8`，`expires_at` 为 2026-08-15 00:10:43，**窗口过了这一轮作废**。续跑步骤见[批处理方案 §5.1](anthropic-batches-plan.zh-CN.md) |
| 媒体资源 6 项中的 2 项 | 一把带 `api.files.write` 的 OpenAI 密钥，或把组织角色提到 Writer | 文件与批处理补真实证据 |
| Bedrock Converse 工具调用（#1） | Bedrock Runtime 的 SigV4 凭据 | 还要先决定按 Profile 放宽还是引入模型级能力证据 |
| 非文本模态第二供应商（#2） | Azure OpenAI 凭据 | 最小方案是复用现有适配器的 azure 分支加一个 media-resources profile |

另有两项是**排期问题而非阻塞**：

- **Anthropic Files API（#3b）** 未排期。与批处理无依赖，价值在 Messages 的文档/PDF 输入，
  需要 `files-api-2025-04-14` beta 头并与既有的每连接 beta 令牌允许列表衔接。
- **能力上限有三份真相**。`domain.DefaultProviderCapabilitiesForProfile`、
  `domain.MaxProviderCapabilitiesForProfile` 与 `web/src/pages/ProvidersPage.tsx` 里手抄的表，
  没有任何东西阻止漂移（2026-08-13 已经漂过一次）。正确修法是 Admin API 直接给出上限、前端不再自己算，
  那是一块涉及 Admin 响应形状与控制台表单的独立改动。§1 的修复收紧的是执行那一半，重复这一半没动。

---

## 3. 一处已失效的记录（本轮更正）

`provider-adaptation-gaps` 的「首次真实运行暴露的两个控制台缺口」里第二条——
**「网关密钥不显示剩余有效期」——不成立**，本轮逐层核实：

- `web/src/pages/ProjectsPage.tsx:349-367` 有 `expired` 判定，以及
  `expiredAt` / `expiresAt` / `neverExpires` 三态与状态点；
- Admin API 回传 `expires_at`（`internal/app/admin_projects.go:282,295,339`）；
- 内嵌 bundle 里有对应的中文串（`internal/webui/dist/assets/zh-CN-*.js` 的「已于 {{date}} 过期」）。

这段自 `b5f012f`（2026-08-05）就在，早于那条记录的 2026-08-13。当时的 401 应另有原因。
第一条（`created_at` 恒 0）是真的，列在 §1.8。
