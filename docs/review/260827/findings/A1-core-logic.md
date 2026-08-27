# A1 · 核心逻辑（热路径改道后的顺序正确性）

> 角色 A1，独立评审。范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`。行号以 2026-08-27 工作区 HEAD
> 为准，旧版行号标注「v0.3.0」并以 `git show v0.3.0:<path>` 为准。
> 已运行：`go test ./internal/gateway/ ./internal/semantic/ -count=1` → 全部 ok。
>
> 计数：21 条。肯定 12、建议 4、问题 4、疑问 1。阻塞候选 0（A1-4 为最接近阻塞线的一条，
> 判定见该条）。

---

## 一、顺序断言的复核结论（任务 1）

range-map 表 2 的断言「能力过滤 < 入站脱敏 < 估算 < 预留 < I/O < attempt.finish < 出站脱敏
< run.finalize」，逐链复核结果：

| 链 | 断言是否成立 | 例外 |
|---|---|---|
| OpenAI Chat 一元（`generate`） | 成立 | **出站渲染**落在 `run.finalize` 之后（A1-4） |
| OpenAI Responses 一元（`generate`） | 成立 | 同上（A1-4） |
| OpenAI Chat / Responses 流式（`generateStream`） | **不成立且正确**：出站脱敏在 `attempt.finish` **之前**（在 I/O 回调内逐 chunk 进行） | 设计如此，见 A1-2 |
| Anthropic portable Messages | 成立（经 Chat wire 中转进 `generate`） | 出站渲染在 finalize 之后（A1-16） |
| Anthropic native Messages | **不成立且正确**：出站脱敏在 `attempt.finish` 之前 | 设计如此，见 A1-3 |
| 重试/回退路径 | 成立：脱敏只做一次、位于循环之外，第二次 attempt 用的是同一份脱敏后请求 | 无（A1-10） |
| 错误路径 | 成立：所有 `return` 前都有 `finish`/`finalize` 或 `abort` | 无 |

即：断言在**一元 portable 链**上成立，但「出站脱敏在 attempt.finish 之后」这一条只对一元链
成立，流式与 native 两链都相反——两版皆然，非本轮回归。真正的例外是**出站渲染**（wire 渲染）
在本轮从 attempt 循环内被移到了 facade，这是 range-map 表 2 未列的一跳。

---

## 二、逐条发现

### A1-1 一元 portable 链的相对次序与改前一致，且预留仍在 I/O 之前
`internal/gateway/service.go:958-960 / 967 / 971 / 974-985 / 986 / 990 / 1031 / 1035 / 1039 / 1046`
基准 B1、B4、B8 ·【肯定】· 不阻塞

实测次序：能力过滤三道（958-960）→ 入站脱敏（967）→ 需求保持守卫（971）→ token 估算
（974-985）→ token 能力过滤（986）→ `beginRequestRun`（990，内含 TokenGuard/limiter/账本开单）
→ `startAttempt`→`ReserveAttemptDetailed`/`ReserveLeaseDetailed`（457/459）→ `MarkStarted`（499）
→ Provider I/O（1031）→ `attempt.finish`（1035）→ 出站脱敏（1039）→ `run.finalize`（1046）。
与 v0.3.0（:908-910 / 917 / 925 / 937 / 941 / 982 / 993 / 997 / 1004）逐跳对应，无缺跳、无换位。
B1「预留在 Provider 请求之前持久化」成立；B4「能力过滤在 Provider I/O 之前」成立。

### A1-2 流式链的次序自洽，且响应字节可见后不再切换 Provider
`internal/gateway/service.go:1915 / 1922 / 1926 / 1929-1940 / 1945 / 1966 / 1984 / 2001-2013 / 2050 / 2065 / 2074`
基准 B5、B8 ·【肯定】· 不阻塞

`generateStream` 的次序：`AllowsStreaming`（1915）→ 入站脱敏（1922）→ 守卫（1926）→ 估算
（1929）→ 预留（1966 → 457/459）→ 流式脱敏器构造（1984）→ I/O 期间**逐 chunk 出站脱敏**
（2001-2013，在 `attempt.finish` 2050 之前）→ finish（2050）→ finalize（2065）。
出站脱敏在结算之前，是与一元链相反的次序，但对流式是唯一可能的次序（字节边发边走）。
重试收口 `if emitted || !retryable(providerErr)`（2074）保证一旦有字节递给调用方就不再切换
Provider——B5 成立。两版此处形状一致（v0.3.0 同位置同判据）。

### A1-3 native Anthropic 链：出站脱敏在结算之前，且拒绝时按真实用量记账、不静默退款
`internal/gateway/service.go:1305-1320 / 1322-1326 / 1327 / 1330-1338 / 1350-1353`
基准 B1、B8 ·【肯定】· 不阻塞

`checkNativeOutboundRedaction` 的三分支（1310-1319）把「策略拒绝」与「无法检查」分开：前者
落 `redactionErr`、不污染 `providerErr`，后者才算 Provider 失败。随后 `semanticResult.Usage`
由 `nativeAnthropicUsage(message.Usage)` 填入（1322-1325），`settlementForResult` 以**真实上游
用量**结算（1326），`finish`（1327），`finalize("policy_rejected")`（1330-1338），最后以 422
`sensitive_output_detected` 答复（1350-1353）。这正是 B8 的判定标准所要求的自洽形态：账目按
上游真实花费保守计入、不退款，错误码与 outcome 都说明了「Halro 拒绝了一次已发生的生成」。
代码注释（1310-1316）明确记录了「折进 providerErr 会把已完成的生成按零结算——一次静默退款」
这一历史缺陷，是本范围内做得最好的一处。

### A1-4 出站 wire 渲染从 attempt 循环内移到 facade，落在 `run.finalize("success")` 之后
`internal/gateway/service.go:923`（Chat）、`:1108`（Responses）；对照 v0.3.0 `service.go:983-989`
基准 B8 ·【问题】· **不阻塞，但为本轮最严重一条**

v0.3.0 的渲染在 attempt 循环内、结算之前：

```
semanticResponse, providerErr := generation.Generate(...)
response := openaiapi.ChatCompletionResponse{}
if providerErr == nil {
    response, err = openaiwire.RenderGenerateResult(semanticResponse)
    if err != nil {
        providerErr = &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, ...}
    }
}
settlement := settlementForResult(semanticResponse, providerErr, ...)
```

渲染失败会成为一个 `Ambiguous` 的 malformed provider error，因而 settlement 的 outcome 是
`provider_error`、`run.finalize("provider_error")`、调用方得 `terminalProviderError`。

HEAD 把渲染整体移出循环：`generate` 在 1046 处已 `run.finalize("success")` 并 return，渲染在
facade 的 923（Chat）/1108（Responses）才发生，失败走 `s.returnFailure(...)` → 502
`provider_error`。**结果是：账本记 `success`，调用方收 502。**

- 记账**金额**无变化（两版都按上游真实用量或保守估算计费，不退款），所以不构成 B1 的记账错误；
- 但 outcome 标签与调用方所见相反，正是 #231 提交信息所描述的那一类（「一次已计费的成功调用
  以 502 抵达调用方」），只是这一次金额是对的、标签是错的；
- 可达性：`RenderGenerateResult` 的失败条件在 `internal/compatibility/openai/mapping.go:307-312`
  （text 带 citations→拒绝）与 `:337-343`（未知内容种类，含 `ContentProviderToolCall`→拒绝）。
  一个普通 Chat 请求**可以**被路由到 `openai.responses.v1` 目标（`filterGenerateProfileCompatibility`
  只在 `UnsupportedGenerateFields` 非空时剔除，而不带 provider-executed tool 的请求对该档案无
  不可承载字段），上游若自带检索并回带 `url_citation` 注解即触发。属可达但罕见。

判定为不阻塞（金额正确、无静默退款、无无界重试——`Ambiguous` 在两版都使 `retryable` 返回
false），但建议在发布前把渲染的失败重新纳入 outcome：要么把渲染移回 `generate` 内、失败时
仍归类为 ambiguous provider error，要么让 `returnFailure` 之前有一次 outcome 更正。

### A1-5 出站脱敏拒绝路径本身自洽
`internal/gateway/service.go:1039-1059`
基准 B1、B8 ·【肯定】· 不阻塞

一元链上出站脱敏失败时：`outcome = "policy_rejected"` → `run.finalize(outcome)`（1046）→ 422
`sensitive_output_detected`（1051-1055）。账在 `attempt.finish`（1035）已按真实用量提交，不退款；
outcome 明确记录为策略拒绝而非成功；错误码对调用方是可行动的（「你的输出触发了项目策略」）。
按任务书给出的判定标准（自洽即可，不要求退款），这一条通过。与 v0.3.0（:997-1010）同形。

### A1-6 「脱敏改变了请求对目标的要求」守卫位于预留之前
`internal/gateway/service.go:971`（一元）、`:1926`（流式）、实现 `:2961-2983`
基准 B1 ·【肯定】· 不阻塞

守卫在 `beginRequestRun`（990 / 1945）之前，因而在 TokenGuard、limiter、账本开单、预算预留
全部之前，也在任何 Provider I/O 之前。任务问题 3 的「它自身是否在预留之前」——是。
`internal/gateway/service_test.go:TestRedactionThatChangesWhatARequestRequiresIsRefused` 断言了
`f.adapter.calls` 未增加，即无 Provider 调用。

### A1-7 守卫只复核 Requirements，不复核档案字段兼容性与 primitive
`internal/gateway/service.go:2977-2983`，对照过滤三道 `:958-960`
基准 B4 ·【建议】· 不阻塞

脱敏前做了三道过滤：`filterSemanticCapabilities`（语义能力）、`filterGenerateProfileCompatibility`
（档案字段，`compatibility.UnsupportedGenerateFields`）、`filterPrimitiveTargets`。脱敏后的守卫
只重算并比较 `DeriveRequirements()`，**没有**重算 `UnsupportedGenerateFields`。

复核后风险很低：`UnsupportedGenerateFields` 读的字段（`hasProviderExecutedTool` 读 `Tools`、
`hasFailedToolResult` 读 `ToolError`、`messages[].name` 读 `Message.Name`）全部是脱敏遍历
**不改写**的成员（`Message.Name` 走 `validateMandatory` 拒绝而非改写，
`internal/redaction/engine.go:287-289`）。所以今天不构成缺陷。但这是一个不对称：守卫的注释
（`service.go:2961-2976`）把理由写成「脱敏能移动一个 requirement」，而档案字段规则同样是一条
「按一组条件路由、按另一组执行」的通道，只是当前没有被触发的成员。建议要么把守卫扩为
「重算三道过滤的判据并比较」，要么在注释里写明为什么 Requirements 足够。

### A1-8 守卫的错误码与文案不足以行动
`internal/gateway/service.go:971-973`、`:2981`
基准 疑问 ·【建议】· 不阻塞

对外：`sensitive_data_detected` / 400 / "redacted request no longer matches the route it was
filtered against"。内部原因：`errors.New("redaction changed the capabilities the request requires")`。
两处都不指名**哪一项**能力发生了变化。调用方拿到的是一个语义上并不准确的错误码（并没有
「检测到敏感数据」，而是「脱敏动作改变了路由前提」），且无法从错误体推断该改哪一处。
`unservableError`（2454-2461）已有把缺失能力名拼进消息的成例，此处可复用同样的做法——
把 `derived` 与 `request.Requirements` 的差集按 `capabilityRequirements` 的名字列出。

### A1-9 守卫覆盖了全部内容种类，没有种类能绕过
`internal/semantic/request.go:204-239`、`internal/redaction/engine.go:317-368`
基准 B4 ·【肯定】· 不阻塞

任务问题 3 的「覆盖哪些内容种类、是否有内容种类能绕过」，逐项核过：

- `DeriveRequirements` 从内容读的只有三处：`part.Kind`（脱敏不改）、`part.Inline()`
  （读 `part.URL`，脱敏**会**改写，`engine.go:337-345`）、`message.Role`（脱敏不改）。
  换言之，唯一能被脱敏移动的 requirement 就是 `FetchedImage`，而守卫的 `!=` 是整个
  `Requirements` 结构体的相等比较，天然覆盖它以及将来任何新增位。
- 内容种类没有旁路：`processContent` 的 `default` 分支（`engine.go:359-367`）对未知种类**拒绝**
  而非跳过，所以不存在「某个 kind 未被遍历因而其 URL 不被改写、守卫也就看不见」的组合。
- `ContentProviderToolCall` 与 `Citations` 两条新路径不产生任何 requirement
  （`request.go:224-236` 的 switch 无对应 case），改写它们不会让守卫误报。
- `request.Tools` 不参与遍历（见 A1-11），也不参与守卫能看到的变化——但 `Tools` 本身脱敏不动，
  所以不构成漏洞。

### A1-10 重试路径上第二次 attempt 拿到的是脱敏后的请求，与改前一致
`internal/gateway/service.go:967`（`canonical, err = ...` 就地覆写）、`:1031`（循环内读同一
`canonical`）；`internal/redaction/engine.go:314-322`、`:389-396`
基准 B1、B8 ·【肯定】· 不阻塞

任务问题 4：`ProcessInboundGenerate` 的结果覆写了 `canonical`，而重试循环在其后，因而**每一次
attempt 都用同一份脱敏后请求，且脱敏只执行一次**（不会因重试而二次脱敏、二次掩码）。
v0.3.0 是同一语义（`request, err = s.redactor.ProcessInboundChat(...)` 覆写后再 `DecodeGenerate`
一次，循环读该 `canonical`），故无漂移。

别名问题也已被处理：`processContent` 先 `make`+`copy` 再改写（`engine.go:314-322` 的注释直指
「重试会再次路由同一 message」），`processCitations` 对 `[]Citation` 单独再拷一层
（`engine.go:389-396`）。`ProcessInboundGenerate` 自身也复制了 `Messages` 切片（`engine.go:283-284`）。
未发现改前请求被就地污染的路径。

### A1-11 `request.Tools` 的 name/description/schema 两版都不经过脱敏
`internal/redaction/engine.go:282-296`（遍历只覆盖 `messages[].content`）；对照 v0.3.0
`engine.go: ProcessInboundChat`（同样只遍历 `request.Messages`）
基准 B3 ·【疑问】· 不阻塞

一个调用方声明的工具（函数名、描述、JSON Schema）会原样送到上游，不受项目策略也不受强制基线
约束，而它的字节**计入**估算（`internal/semantic/request.go:298-300`）。这是**两版平价**、不是
本轮回归，因此记为疑问而非问题；但既然这一轮把遍历整体重写了一次，是把这个缺口一并处理的
自然时机。判断它是否该被覆盖属于策略决定（与 `ContentReasoning` 入站不被检查是同一类决定，
`engine.go:272-276` 已把那一条明确写成「留给决定这件事的人」）——建议同样明确写下来。

### A1-12 `requestedOutputTokens` 改读语义字段后与改前逐路径等价
`internal/gateway/service.go:2905-2930`；对照 v0.3.0 `service.go:2865-2890`
基准 B1 ·【肯定】· 不阻塞

改前读 wire 的 `MaxTokens`/`MaxCompletionTokens`，改后读语义的
`VisibleOutputTokenLimit`/`CompletionTokenLimit`，优先级（后者覆盖前者）、项目上限判据、
「两者皆空时取项目上限」三条分支逐字对应。映射侧：Chat 的 `DecodeGenerate`
（`compatibility/openai/mapping.go:27-28`）把两者一一对应；Responses 的
`DecodeResponseGenerate`（`responses.go:24`）只填 `CompletionTokenLimit`，而 v0.3.0 的
Responses 经 `RenderGenerateRequest`（`mapping.go:66`）也只填 `MaxCompletionTokens`。
两条路径都等价，未发现预留基数漂移。

### A1-13 估算输入源由 wire 字节改为语义字节，使同一请求的估值系统性变小
`internal/gateway/service.go:974`、`internal/semantic/request.go:285-301`；对照 v0.3.0
`service.go:925` + `internal/openaiapi/types.go:127-139`
基准 B1 ·【建议】· 不阻塞

v0.3.0 计的是 wire 字节：`len(message.Content)` 是**原始 JSON**（含引号、转义、`{"type":"text",
"text":...}` 的结构字节），并另计 `ReasoningContent`、`ToolCallID`、`call.Type`。
HEAD 计的是语义字段长度之和：`Role + Name + Text + URL + Detail + CallID + Name + Arguments`，
结构字节、`call.Type`（"function"，每次 8 字节）都不再计入。

后果：**同一请求在 HEAD 的估算严格不大于 v0.3.0**，富文本（多 part 数组）差距最大。估算是
项目 `MaxInputTokens` 闸门、`filterTokenCapabilities` 的上下文窗判据、TPM 租约与**预算预留基数**
共同的输入，因此这四道闸门在 HEAD 一律更松。

这不构成记账错误——结算走上游报告用量（`settlementForResult`，`:2634-2675`），预留只是上界
预估；而且「同一内容不因端点而估得不同」（`request.go:276-280` 的注释）是一个正当的设计目标。
但「更松」这一后果本身没有在任何注释或文档里被写下来，建议在发布记录里点名，或考虑把结构
开销以一个固定的 per-part 常数补回来，使新估算不低于旧估算。

### A1-14 记账生命周期的五个函数与 v0.3.0 逐字节相同
`internal/gateway/service.go:253`（`beginRequestRun`）、`:371`（`startAttempt`）、`:558`
（`attempt.finish`）、`:165`（`run.finalize`）、`:2634`（`settlementForResult`）
基准 B1、B7 ·【肯定】· 不阻塞

逐函数抽取后 `diff` 为空——热路径改道没有触碰任何一处结算逻辑。价格 pin、TokenGuard 复核、
`MarkStarted`、失败清理（`DeletePreparedDeploymentPricePin` / `settleAttempt` / `finalize`）
的顺序与错误汇聚全部原样。这是本轮改动最令人放心的一点：+268 行的改动面里，账目本身零改动。

### A1-15 未分类 Provider 错误不再把错误文本写进日志
`internal/gateway/service.go:638-653`；对照 v0.3.0 `service.go:635`
基准 B3 ·【肯定】· 不阻塞

v0.3.0 在 `else` 分支写 `"reason", providerErr.Error()`——一个未经 `*provider.Error` 分类的错误
可能持有响应体。HEAD 改为写 `"error_type", fmt.Sprintf("%T", providerErr)`：类型名是代码产生的
标识符而非上游文本，回答了这个分支真正要问的问题（哪个组件产生了未分类失败），同时把可能
携带响应体的句子挡在日志外。注释（624-637）把推理写全了。B3 的正面改进。

### A1-16 Anthropic portable Messages 未改道，同样在计费后于渲染处 502
`internal/gateway/service.go:1179-1200`（尤其 `:1195`）
基准 B8 ·【问题】· 不阻塞

portable Messages 仍走 语义→Chat wire→`s.Chat`→再解码→`anthropicwire.RenderResult` 的中转。
`RenderResult`（`internal/compatibility/anthropic/mapping.go:245-247、256-257`）对带 citations
的 text 与任何未知内容种类都返回「provider result contains non-portable content」，此时
`generate` 内的 `run.finalize("success")` 早已提交——与 A1-4 完全同形，且这条链上叠了两次
（Chat 层 923 一次，Messages 层 1195 一次）。

与 A1-4 的差别：Messages 层的渲染在 v0.3.0 **也**在计费之后（v0.3.0 `:1148` 之后），故这一半是
存量形状而非本轮回归；本轮新增的是 Chat 层（923）那一次。合并处置即可。

### A1-17 公开别名回填收敛到一处，上游模型标识不会离开 `generate`
`internal/gateway/service.go:1056-1060`、`:2016`、`:2036`
基准 B3 ·【肯定】· 不阻塞

改前每个 facade 各自 `response.Model = request.Model`；改后在 `generate` 内对语义结果统一
`semanticResponse.Model = publicModel`（1060），流式在两处 emit 前统一 `safeChunk.Model =
publicModel`（2016、2036）。注释（1056-1059）说明了理由——「每个端点各回填一次就是每个端点
各有一次忘记的机会」。这正是 Responses 直连后本该出现的风险，被提前收掉了。
另核：失败路径（422 / `terminalProviderError` / `exhaustedAttemptsError`）都不返回语义结果，
不存在上游标识经错误体外泄的路径。

### A1-18 `json_mode` 在需求↔能力配对表上就地拆为两行，无并存
`internal/gateway/service.go:2537-2538`；对照 v0.3.0 `service.go:2480`
基准 B6、B9 ·【肯定】· 不阻塞

`{"json_mode", r.JSONMode, c.JSONMode}` 一行被替换为 `json_object` 与 `structured_outputs` 两行，
旧行不存在、没有兼容别名、没有「保留旧名同时新增」。`capabilityRequirements` 是过滤
（`filterSemanticCapabilities`, 2545-2554）与拒绝理由（`missingCapabilities`, 2470-2482）**共用**
的唯一表，所以拆分对两侧同时生效，不会出现「过滤按新名、报错按旧名」的漂移。符合 B6 的
就地修复与 B9 的「一次能力开关只描述一件事」。

### A1-19 随拆分删除的死代码没有留下并存构造
`internal/gateway/service.go`（v0.3.0 `:2549-2566` 的 `requestUsesJSONMode` / `hasJSONValue` 在
HEAD 已不存在），`import "bytes"` 同步移除
基准 B6 ·【肯定】· 不阻塞

两个只服务于旧单 bit 判定的 wire 层辅助函数被整体删除，而不是保留下来「以防万一」。
`requestUsesVision`（2604-2620）因仍被引用而保留，属正常。

### A1-20 `unservableReasons` 在脱敏之前计算，报出的是脱敏前的理由
`internal/gateway/service.go:961-966`、`:2467-2469`
基准 B4 ·【肯定】· 不阻塞

候选清空的 400 发生在脱敏之前，所以 `unservableReasons(candidates, canonical, ...)` 读的是
调用方原始请求的需求——这是正确的一侧：告诉调用方「你**送来的**请求需要 X 而这条路由没有 X」，
而不是「脱敏后的请求需要 X」。若报后者，操作者会去查一个自己从未写过的需求。
`reasonSet` 按表序去重（2470-2476 的注释：「顺序不同的拒绝是无法 diff 的拒绝」）保证了
错误体的确定性。

### A1-21 建议：range-map 表 2.3 的次序串应补上「出站渲染」这一跳
`docs/review/260827/range-map.md:206-213`
基准 疑问 ·【建议】· 不阻塞

表 2.3 的次序串止于「出站脱敏(1039) < run.finalize(1046) < 响应返回」，把 wire 渲染折进了
「响应返回」。而本轮恰恰是这一跳换了位置（A1-4）。建议把串改为
`... < 出站脱敏(1039) < run.finalize(1046) < 出站渲染(923/1108) < 响应返回`，
并在表 2.1 增加一行「出站 wire 渲染：v0.3.0 在 attempt 循环内、结算之前 / HEAD 在 facade、
finalize 之后」——阶段 0 的事实底座漏了这一跳，后续角色会据表 2 判 B8，容易一起漏掉。

---

## 三、未核实 / 超出本角色范围

- `streamSettlement`（流式结算函数）未与 v0.3.0 逐字段比对；A1-2 只断言了次序与重试收口，
  未断言流式结算金额语义无漂移。
- `Service.Embeddings`（`:2087`）链未比对（任务范围限定生成热路径）。
- `MessagesNativeStream`（`:1447`）与 `MessagesCountTokens`（`:1360`）的次序未逐跳比对。
- 脱敏遍历本身的规则覆盖（强制基线是否作用于三条新路径、span 归零策略）属 A2 靶子，本文只
  从「顺序」与「守卫覆盖面」的角度触及。
