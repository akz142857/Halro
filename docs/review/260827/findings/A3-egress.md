# A3 · 安全（出网与新表面）

> 阶段 1 独立评审。范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`。事实底座取 `range-map.md` 表 3，
> 每条结论自行复核过行号。基准编号见 `review-plan.md` §3。
> 未读同目录其他角色文件与 `findings-web.md`。

靶心：`web_search` 是本仓第一条**不经 `internal/safetransport`** 的出网——主机白名单、DNS/IP
校验、禁重定向对它一概不适用，因为发起请求的是上游而不是 Halro。以下十一条按"这条出网能不能
在没有显式运维动作的情况下被打开、被打开之后看不看得见、以及有没有第二扇门"排列。

计数：肯定 5 · 建议 3 · 问题 3 · 疑似BUG 0。阻塞发布：**无**（阻塞候选 0；最严重的
A3-4 与 A3-6 均为"发布说明/后续修"级）。

---

## A3-1 【肯定】"默认关闭"在档案表与两道写边界上成立

`internal/domain/provider_table.go:150-151`（Responses 行 `Defaults: openAIResponsesSet`、
`Ceiling: withProviderExecutedTools(...)`）、`:122-125`（该集合不含 `ProviderExecutedTools`）、
`:157-158`（Anthropic Messages 同形）。`withProviderExecutedTools` 在 `:327-329`，只在 Ceiling 侧使用。

上限不是只在 Admin handler 里挡：域层写边界 `internal/domain/models.go:390-395`
（`ProviderProfileBinding.Validate` → `MaxProviderCapabilitiesForProfile`）是 `PutProvider` 必经之路，
`:378-389` 的注释明确说这是"Admin API、恢复、以及将来还不存在的调用方"都要过的那道门。
运行期加载再拦一次：`internal/app/providers.go:427-428` 把超上限的 binding 整条排除
（`excludedCapabilityCeilingExceeded`，`:221`），不是 clamp（`:420-426` 注释给了理由）。

基准 B4/B2。不阻塞。

## A3-2 【肯定】Deployment 不能单方面打开；拒绝行是这三行

1. `internal/app/admin_deployments.go:555-556` —
   `if !domain.ProviderCapabilitiesSubset(capabilities, binding.Capabilities) { ... "deployment capabilities exceed provider capabilities" }`。
   Provider 层没开而部署勾了 → **写入被拒**，不是静默收窄。
2. 运行期逐位与：`internal/app/providers.go:827`
   （`ProviderExecutedTools: available.ProviderExecutedTools && declared.ProviderExecutedTools`），
   `available` 来自适配器，适配器的能力集就是 binding 的（`:683`、`:696-749` 逐档案传入 `Capabilities: capabilities`）。
3. 空能力集不被读成"未指定"：`internal/app/providers.go:474-487` 把这种记录直接扣留
   （`withheldCapabilitiesInvalid`），注释点名这是"绕过 store 写进来的记录"，扣留而非采纳适配器全集
   ——即 fail-closed 方向。

注意一处**不对称**（并入 A3-8 的同类观察，不单列）：`domain.Deployment.Validate`
（`internal/domain/models.go:943-952`）只校验依赖（PET 需要 chat，`:946-948`），**没有**档案上限检查；
Deployment 侧的上限只由上面第 1 条的 handler 与第 2、3 条的加载期承担。binding 有域层门、
deployment 没有。今天后果被第 2、3 条兜住，写下来是因为它与 `models.go:378-389` 自陈的原则不一致。

基准 B4。不阻塞。

## A3-3 【肯定】迁移、备份恢复、批量导入三条路径都置不了真

- **迁移 28**（`internal/store/bolt/store.go:669-707`，配 `:1034` 的 `newCapabilityEvidenceMembers`）：
  PET 进入词表那次迁移**只补证据为 `unsupported`**，从不把能力位置真；`:660-668` 注释写明理由
  （"能力当时不存在，任何记录都不可能声明过它"）。
- **迁移 32**（`store.go:839-898`，`structured_output_capability_split`）：只处理 `json_mode` 一族
  （`:843-847`、`:918-921`、`:945-948`），不触碰 `provider_executed_tools`。已复核 grep：
  `provider_executed` 在 `store.go` 内的命中全部属于迁移 28 与 `:755-770` 的迁移 31 注释。
- **批量导入**：`internal/app/` 下没有 Deployment/Provider 的批量导入端点（grep 无命中）。
  这条"路径"在本仓不存在，不是"检查通过"。
- **备份恢复**：`internal/app/backup.go` 的 restore 是数据目录级恢复，不绕过 A3-1/A3-2 的加载期两道门
  ——恢复回来的记录若超上限，`providers.go:427-428` 仍排除。**未核实**：未在真实 `.hmbk` 上实测
  （属 R4 范畴）。

基准 B2/B6。不阻塞。

## A3-4 【问题】远端签名目录 / 变体解析可以在**没有部署级勾选**的情况下把 PET 带进 Deployment

新建 Deployment 且请求体不带 `capabilities` 时（`retained == nil`）：

- 目录路径：`internal/app/admin_deployments.go:1064` 取
  `capabilities: modelcatalog.Clamp(entry.Capabilities, binding.Capabilities)`，
  `:1077-1085` 在 `retained == nil` 时**原样返回该集合**；
- 变体路径：`internal/app/admin_deployments.go:947` `retained := selected.Capabilities`，
  同样在未显式声明时整份采纳。

也就是说，只要（a）连接侧 binding 已开 PET，（b）某条目录条目声明了 `provider_executed_tools`，
新部署就带着这条出网能力落库，运维在部署这一层没有做过任何动作。`modelcatalog.Key.Ceiling()`
（`internal/modelcatalog/catalog.go:214-225`）**刻意**用 Ceiling 而非 Defaults 作目录声明的上限，
注释里点名"provider-executed tools on Anthropic Messages"就是它要允许的那类逐模型声明——
所以这不是遗漏，是设计允许的形状。

今天不可达的原因只有一个：内置目录**没有任何条目声明它**（`internal/modelcatalog/builtin.go:226-229`
注释即为此写），且 `provider_metadata` 映射不产出该名（全仓 grep：适配器元数据映射零命中）。
剩下的唯一来源是签名目录，而背景刷新是 1.1.0 才开的开关
（`docs/contracts/provider-capabilities.md:122-131`）。

判为**问题**而非建议，理由是"默认关闭"这句承诺在这条路径上依赖的是"目录恰好没声明"，
而不是一道拒绝。建议二选一：目录侧禁止声明 PET（在 `Key.Ceiling()` 上单独扣掉），
或采纳处要求显式勾选（`retained == nil` 时把 PET 清零）。

基准 B4、威胁模型。**不阻塞发布**（签名目录本轮不启用），但应进 CHANGELOG 的已知边界。

## A3-5 【问题】数据面对"上游自行发起了检索"零痕迹

- `internal/gateway/` 全包不写审计：grep `audit`/`Audit` 在该包内只命中 `service.go:239` 的一句注释，
  `internal/audit/log.go` 的 `Append`/`AppendBatch` 调用点全在 Admin 面。一次含 `web_search_call`
  的应答不产生任何审计记录。
- 用量侧也没有：`internal/usage/aggregate.go:37-77` 的 `AttemptEvent` 没有任何工具/检索维度字段；
  Parquet 行由它派生。
- 指标侧没有对应计数器（grep `web_search`/`provider_executed` 在 metrics 相关文件零命中）。
- 唯一可见处是**返回给调用方的响应体**：`internal/compatibility/openai/provider_responses.go:154-173`
  解码出 `ContentProviderToolCall`，`internal/compatibility/openai/responses.go:306-311` 渲染回
  `web_search_call`。也就是说发起方看得见，运维看不见。

这不是泄漏（B3 的正面成立：query 与 citation 不进日志/审计），而是 B3 反面的可观测性缺口：
出了事之后无法从本地记录回答"哪些请求让上游自己上了网"。域层注释
（`internal/domain/provider_table.go:462-471`）与控制台文案
（`web/src/i18n/locales/zh-CN.ts:1018`：「也不会出现在审计记录里」）都把它当成已知事实写下了，
所以是**知情的缺口**而非疏漏。

建议：一个低基数计数器（按 profile/deployment 维度，不带 query），或 attempt 上一个布尔标志。
基准 B3（反面）。不阻塞。

## A3-6 【问题】开启 PET 后，成本记账系统性低估

`web_search` 由上游**按检索次数**另行计费，而 Halro 的价格模型只有
input / cached input / output 三个按 token 的分量加一个**按请求**的固定项
（`internal/domain/pricing.go:201` `FixedRequestMicrosUSD`，校验 `:233-242`）。全仓没有任何
"按工具调用次数"的价格分量，结算读的是 token 用量
（`internal/usage/aggregate.go:52-66` 的成本字段全部由 token × 价格 或固定项得出）。
一次触发 N 次检索的请求，其上游账单与 Halro 记的 cost 相差 N 次检索费，
项目预算（B1 的保护对象）按偏低的数在放行。

控制台的知情文案只讲出网、不讲计费
（`web/src/i18n/locales/zh-CN.ts:1018` / `en-US.ts:1013`）；
`docs/contracts/openai-compatibility.md:57-67` 亦未提。

判为问题、**不阻塞**：它不是记账**错误**（没有把已知成本记错），而是一类成本 Halro 从未观测到；
但它是本组最贴近 B1 的一条，发布说明与那段 opt-in 文案里应当补一句。

## A3-7 【建议】知情开启只在连接页，部署页缺失——而部署那一勾才是生效的那一半

服务端把"该能力需要说明"下发给前端：`internal/domain/provider_table.go:475-477`
（`CapabilityOptInWarnings`），经 `internal/app/admin_provider_profiles.go:82-86、134` 出到 API。
连接页照做了：`web/src/pages/ProvidersPage.tsx:652`（汇总告警）、`:848`（复选框旁 `capabilityEgressTag`）。

部署页没有：`web/src/pages/DeploymentsPage.tsx:1209` 把 `provider_executed_tools` 放进 `protocol`
组，与 streaming、tools 并列渲染成一个普通复选框；`capabilityNeedsOptInWarning`
（`web/src/hooks/useProviderProfiles.ts:208`）的唯一调用方是 ProvidersPage。
按 A3-2 第 2 条，运行期生效需要 binding 与 deployment **两处都为真**，所以部署页的这一勾
与连接页的那一勾同等重要，却少了说明。

基准：`review-plan.md` §6 第二条（"默认关闭要在 UI 上是显式选择，且旁边必须说清它意味着上游自行出网"）。
不阻塞。与阶段 2 的靶子重叠，此处只记后端契约已下发、前端只用了一半。

## A3-8 【建议】`RenderGenerateRequest` 把 provider-executed 工具静默降级为函数工具

`internal/compatibility/openai/mapping.go:81-82`：

```go
for _, tool := range request.Tools {
    result.Tools = append(result.Tools, openaiapi.Tool{Type: "function", Function: openaiapi.ToolFunction{Name: tool.Name, ...}})
}
```

`semantic.Tool.Execution` 在这里被忽略：一个 `Execution == ToolExecutionProvider` 的工具会被渲染成
名为 `web_search` 的**调用方函数工具**发给上游——上游随后会回调一个调用方从未声明的函数。
对称位置的解码侧是拒绝的（`mapping.go:41-43`：`tool.Type != "function"` 即拒），渲染侧没有对应拒绝。

调用方是 OpenAI-wire 的通用生成 primitive（`internal/provider/primitive.go:153`、`:169`）
与 Anthropic portable 中转（`internal/gateway/service.go:1183`、`:1215`）。

**今天不可达**，三重拦截各在一行：PET 只出现在 `openai.responses.v1`（走 `GenerateSemantic`，
`internal/provider/openai/adapter.go:412-447`）与 `anthropic.messages.v1`（走 Anthropic primitive）的上限上；
`internal/compatibility/provider_fields.go:285` 的 `providerExecutedToolProfiles` 只含前者，
其余档案在字段过滤即被删候选；Anthropic portable 在 `internal/compatibility/anthropic/mapping.go:51`
就把这类工具拒了。

仍写下来：这是 B4 明令禁止的形状（不支持的字段"拒绝而非静默丢弃"），且一旦将来某个 OpenAI-wire
档案获得 PET，缺陷立刻变为"给上游一个假函数"。修法是在渲染循环里对 `ToolExecutionProvider` 返回错误。
基准 B4。不阻塞。

## A3-9 【肯定】`code_interpreter` / `file_search` 未发现第三条入口

逐面复核，每一面都在 Provider I/O 之前：

| 面 | 拒绝点 | 形态 |
|---|---|---|
| OpenAI Chat wire | `internal/compatibility/openai/mapping.go:41-43` | 非 `function` 即拒；`openaiapi.Tool` 结构本身只有 `function` 成员（`internal/openaiapi/types.go:47-56`） |
| OpenAI Responses wire | `internal/openaiapi/responses.go:130-138` | `web_search` 唯一放行且不得带函数字段；其余非 function 类型报 "must be a named function or a supported provider-executed tool" |
| 语义模型 | `internal/semantic/request.go:147-156` | 名字不是 `ProviderToolWebSearch`（`:64`）或带 schema/description 即拒 |
| 语义→provider 渲染 | `internal/compatibility/openai/provider_responses.go:48-52` | 再拒一次 |
| Anthropic Messages · portable | `internal/compatibility/anthropic/mapping.go:47-52` | `IsAnthropicDefined()` 把 `code_execution_*`、`web_fetch_*` 等族（`internal/anthropicapi/types.go:423-428`）整体拒于便携路径之外 |
| Anthropic Messages · native | `internal/anthropicapi/types.go:474-486` + `internal/compatibility/anthropic/native.go:129` + `internal/gateway/service.go:1593-1595` | wire 层接受、**能力层**拒：`NativeRequirements` 抬起 `ProviderExecutedTools`，`filterSemanticCapabilities`（`service.go:2545-2553`、配对表 `:2542`）删掉未声明该能力的 target，候选清空即 400 `unsupported_feature`（`:2459-2470`） |
| Bedrock Mantle Responses | `internal/domain/provider_table.go:316-320`（`mantleOpenAIResponsesSet` 无 PET）+ `:271`、`:278`（Defaults == Ceiling） | 北向 `/v1/responses` 的 `web_search` 请求对 Mantle target 在能力过滤即被删；再遇 `provider_fields.go:285` 的档案白名单 |

注意 Anthropic native 是**第二扇合法的门**：`web_search_*` 经 native 模式原样转发，上游同样自行出网。
它由同一个能力门控，且早于本范围存在（v0.3.0 `provider_table.go:118` 已是 `withProviderExecutedTools`），
契约文档也写了（`docs/compatibility/endpoint-manifests.json:670`、`:722`）。因此"`openai.responses.v1`
是唯一承载 `web_search` 的 wire"只对 **OpenAI 侧**成立——`range-map.md` §3.3 第 5 条的措辞正确，
但读者容易读成"全仓唯一"。不阻塞，属措辞提醒。

基准 B4。不阻塞。

## A3-10 【建议】H2 判定：这条信任边界"已知，但没写进威胁模型"

- `docs/architecture/threat-model.md` 本范围内**零改动**（`git diff --name-only` 无此文件）。
  信任边界图（`:16-30`）只画到 `→ SafeTransport → external provider`；
  SSRF 一行（`:49`）仍把 SafeTransport 写成唯一控制。上游代 Halro 出网这条边界不在文档里。
- `docs/contracts/provider-capabilities.md` 的 +55 行**没有**写这条边界：只在能力清单里加了一行
  "provider-executed tools (tools the upstream runs itself)"（`:12`），并在档案表里写
  "plus web search at the ceiling"（`:163`）。没有 SafeTransport 字样。
- 真正落笔的地方是：代码注释 `internal/domain/provider_table.go:462-471`（写得最完整：无白名单、
  无 DNS pinning、审计无痕）、`docs/contracts/openai-compatibility.md:57-67`、
  `docs/compatibility/endpoint-manifests.json:477`/`:510`/`:670`/`:722`、控制台文案
  `web/src/i18n/locales/zh-CN.ts:1016-1018`。

判定：**"已知并接受"，但没写进威胁模型**——按 H2 的分级即建议级（若文档与实现不符才是问题级，
此处不存在不符）。补法很便宜：威胁模型「Primary threats and controls」加一行
"Provider-executed tool egress | capability off by default, ceiling-only, informed opt-in;
**not** covered by SafeTransport"，并在 Default assumptions 里点名。不阻塞。

## A3-11 【肯定】citation 回流没有 SSRF 二次利用面（附一条 scheme 观察）

- 解码：`internal/compatibility/openai/provider_responses.go:194-215`，annotation 只被读成
  `semantic.Citation{URL, Title, StartIndex, EndIndex}` 字符串，非 `url_citation` 类型直接拒（`:200-202`）。
- 渲染回北向：`internal/compatibility/openai/responses.go:296-300`，原样回传。
- **网关不抓取**：全仓无以 citation URL 发起请求的代码；`fetched_image` 那条先例的注释
  （`internal/domain/provider_table.go:300-305`）把这条原则写死了——"Halro 不得代取调用方给的地址，
  那正是 SafeTransport 的白名单要防的请求伪造"。因此不存在"上游给一个地址、网关去取"的二次利用。
- 入站方向也进不来：`semantic` 拒绝非文本内容携带 citations（`internal/semantic/content.go:181`），
  Responses 入站解码不产出 citations（`internal/compatibility/openai/responses.go:34-38` 只解码 input 消息）。

附带观察（建议级，不单列编号）：`citation.URL` **无 scheme 校验**，只要非空即接受
（`provider_responses.go:200`）。`javascript:`/`data:` 会原样出现在返回给调用方的
`annotations[].url` 里。Halro 自己不渲染它，风险落在调用方 UI；但若要加一道，
`decodeProviderAnnotations` 是唯一的口子，成本一行。

基准 B4/威胁模型。不阻塞。

---

## 未核实（不以推断顶替）

1. **真实二进制上的一次 `web_search` 应答未跑**（方案 S4/R6，计费项）。本报告全部结论来自静态路径
   与针对性单测；"审计无痕"是代码事实（无写入点），不是抓包结论。
2. **签名目录端到端未跑**：A3-4 的可达性推断建立在 `Clamp`/`retained == nil` 两段代码上，
   没有真实签名目录样本验证"一条声明 PET 的条目确实会被采纳"。
3. **备份恢复未实测**（A3-3 第四点，属 R4）。
4. **A3-6 的上游计费口径**取自 OpenAI 对 hosted tool 按次计费的公开做法，未在本仓找到任何记录该费率的
   地方——"Halro 侧没有承载它的价格分量"是代码事实，"上游确实另收"是外部前提。

## 已运行

```
go test ./internal/domain/ -run 'Ceiling|ProviderExecutedTools' -count=1   ok
go test ./internal/provider/ -run 'Ceiling' -count=1                       ok
go test ./internal/semantic/ -run 'Tool|ProviderExecuted|Hosted' -count=1  ok
go test ./internal/app/ -run 'Capab|Unservable|CeilingExceeded|ProbeResultWrite|CapabilityDetection' -count=1  ok
grep -rn provider_executed_tools（全仓，含 docs/web）
```
