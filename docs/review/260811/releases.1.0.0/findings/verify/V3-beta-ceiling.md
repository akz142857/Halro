# V3 · 对抗验证 · A3-01（非 strict Beta profile 能力天花板对管理员可写）

对抗验证角色，按 role-prompts.md §1 + §8 执行。任务是**证伪** A3-01，默认它是错的。

运行模型：**Opus 5（claude-opus-5[1m]）**。未遇到任何拒答、`stop_reason: refusal` 或空响应。
仓库 HEAD = `2cd24a76a569fe53f878c1ab1be31441f4c008e0`（与 A3 一致）。全程未修改仓库任何文件。

---

## 1. 裁决：**PARTIAL —— 管理面的事实成立，安全结论不成立**

拆成两个可分别裁决的命题：

| # | A3-01 的命题 | 裁决 |
|---|---|---|
| M1 | `isStrictOperationProfile` 把 Gemini / Bedrock Converse / Mantle 排除在天花板校验之外，管理员可以把超出 profile 默认的 `capabilities` **写进** binding 并落盘 | **CONFIRMED**（逐字属实，line 全部核对无误） |
| M2 | 因此"**越过了** CLAUDE.md 钉死的 Beta 能力上限"——即被放宽的声明进入数据面能力判定、服务集真的高于钉死上限 | **REFUTED** |

A3-01 把 M1 当成了 M2 的充分条件。它没有走完数据面，自己在附录里也承认了这一点
（A3.md:241-243「未逐路追到 provider 层……不依赖下游行为即成立」）。**这一步恰恰是不成立的**：
`binding.Capabilities` 根本不是数据面用的那个天花板。真正决定服务集的天花板是**编译进
二进制、管理员无从写入**的另一份，而且它对 Gemini / Bedrock Converse 恰好**逐位等于**
CLAUDE.md 钉死的 profile 默认。

**严重度表态：高估约 2 档。P2「需裁决」应降为 P3「建议」，不阻塞 v1.0.0。**
剩下的是一个真实但性质完全不同的问题——管理面接受一个**无效**的声明（见 §5），
属于输入校验不严 / 控制台会显示误导性能力，不属于 CLAUDE.md 意义上的"放宽 Beta 能力上限"。

---

## 2. 关键问题的正面回答

> 即使管理员写进了一个超出 profile 默认的能力声明，一个真实请求带着那个能力打到
> Gemini/Bedrock 上，会发生什么？

**答：在任何 Provider I/O 发生之前被拒，返回 400。既不静默降级，也不会发到上游。**

以「管理员把 Gemini binding 的 `capabilities` 写成含 `tools: true`，并据此建
`mode=operator_declared` 的 Deployment + Route，然后客户端发一个带 `tools` 的
`POST /v1/chat/completions`」为例，请求依次撞上**四道**互相独立的闸门，其中前三道
就足以单独拦下，第四道是最后的 fail-closed 兜底：

```
Chat 请求
  └─ gateway.Service.Chat            internal/gateway/service.go:750
       ①  filterSemanticCapabilities(targets, requirements)      service.go:750 → :1940-1950
          target.Capabilities 来自 ↓
          app.deploymentCapabilities(deployment, adapter)        internal/app/providers.go:513 → :690-711
              = adapterCapabilitiesFor(adapter)  AND  deployment.Capabilities
                                                  ↑ 逐位 && 交集，声明只能缩、不能扩
              adapterCapabilitiesFor →            internal/app/providers.go:722-724
              gemini.(*Adapter).Capabilities() = {Chat,Streaming,Embeddings,DeveloperRole}
                                                  internal/provider/gemini/adapter.go:117-119  ← 硬编码
          ⇒ 有效 Tools = false && true = false ⇒ target 被剔除
       ②  filterGenerateProfileCompatibility(targets, canonical) service.go:751 → :1952-1956
          → compatibility.UnsupportedGenerateFields(ProfileGeminiText, req)
                                                  internal/compatibility/provider_fields.go:21-30
          仅按 ProfileID 分支，完全不看任何声明能力 ⇒ "tools" 命中 ⇒ target 被剔除
       ③  filterPrimitiveTargets(targets, OperationChat)         service.go:752 → :1964-1969
          算子集合来自编译期表 internal/provider/profile.go:70-86
       ⇒ len(targets)==0 ⇒ gatewayError("unsupported_feature", 400)   service.go:753-755
          （此时尚未 beginRequestRun，未预留额度，未建立任何上游连接）
       ④ 假设 ①②③ 全被绕过，仍有：
          gemini.translateChat()      internal/provider/gemini/adapter.go:416-417
          无条件 badRequest("Gemini Beta cannot represent one or more requested OpenAI fields")
          —— 在 encode / postJSON 之前，同样不看声明能力
```

Bedrock Converse 完全同构：`bedrock.(*Adapter).Capabilities()`
（`internal/provider/bedrock/adapter.go:284-298`，Converse 分支 `{Chat,Streaming,StreamUsage}`）、
`provider_fields.go:31-40`（`ProfileBedrockConverseText`）、
`bedrock.translateRequest`（`internal/provider/bedrock/adapter.go:484-486`）。

流式路径同样三道闸（`service.go:1407-1409`），Embeddings 路径同样
（`service.go:1608-1609`），native Anthropic Messages 路径走
`prepareNativeMessages`（`service.go:1060`，`:1250-1251` 无匹配 profile 即 400）。
**没有任何一条路径把声明能力当作放行依据。**

---

## 3. 为什么 M2 不成立：数据面的天花板不是 `binding.Capabilities`

这是本次证伪的核心，也是 A3-01 判断反转的唯一支点。

`internal/app/providers.go:690-711`：

```
func deploymentCapabilities(deployment domain.Deployment, adapter provider.Adapter) provider.Capabilities {
	available := adapterCapabilitiesFor(adapter)      // ← 编译进二进制
	declared := deployment.Capabilities               // ← 管理员可写
	...
	Tools:  available.Tools  && declared.Tools,       // 逐位 AND
	Vision: available.Vision && declared.Vision,
	...
	MaxContextTokens: minimumCapabilityLimit(available.MaxContextTokens, declared.MaxContextTokens),
}
```

`available` 从哪来，决定了这道交集是不是真闸。看构造点
`newProviderBindingAdapter`（`internal/app/providers.go:572-640`）：

| Profile | 构造调用 | 是否把 `binding.Capabilities` 传给 adapter | adapter 报告的能力 |
|---|---|---|---|
| **Gemini** | `providers.go:599-602` `geminiprovider.New(Options{Endpoint, Authorizer, Client})` | **否** —— `gemini.Options`（`gemini/adapter.go:25-30`）**根本没有 Capabilities 字段** | 硬编码 `{Chat,Streaming,Embeddings,DeveloperRole}`（`gemini/adapter.go:117-119`） |
| **Bedrock Converse** | `providers.go:606-609` `bedrockprovider.New(Options{..., ProfileID})` | **否** —— `bedrock.Options`（`bedrock/adapter.go:24-31`）**没有 Capabilities 字段** | 按 ProfileID 硬编码，Converse → `{Chat,Streaming,StreamUsage}`（`bedrock/adapter.go:284-298`） |
| Mantle ×3 | `providers.go:610-633` | **是** —— `Capabilities: capabilities`，而 `capabilities := providerCapabilities(binding.Capabilities)`（`providers.go:580`） | = binding 声明（无独立天花板） |

**把 `gemini/adapter.go:117-119` 和 `domain/models.go:500-501` 并排看：**

```
gemini/adapter.go:118   provider.Capabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
domain/models.go:501    ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
                        // "Beta profile intentionally declares only the translated text subset."
```

```
bedrock/adapter.go:297  provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true}
domain/models.go:504    ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
                        // "Beta profile intentionally declares only Converse text chat and usage."
```

**逐位相同。** 也就是说：CLAUDE.md 钉死的那个 Beta 上限，在数据面上是由**一份管理员碰不到
的编译期常量**执行的；`isStrictOperationProfile` 那道校验（`admin_providers.go:916-919`、
`:939-941`）只是管理面的一道**冗余**前置校验，它缺席不改变服务集。A3-01 把这道冗余校验
当成了唯一执行点（A3.md:93-96「`isStrictOperationProfile` 是唯一的天花板执行点……
下游 provider adapter 以同一 `binding.Capabilities` 构造（`internal/app/providers.go:580`），
无二次兜底」）——**这一句是本条发现事实层面的错误**：`providers.go:580` 那个 `capabilities`
变量只被 OpenAI / Anthropic / Azure / DeepSeek / Mantle 分支使用，**Gemini 与 Bedrock
Converse 两个分支根本没有引用它**（`providers.go:599-609`），而这两个正是 CLAUDE.md
点名钉死的对象。

`MaxContextTokens` / `MaxOutputTokens` 也不构成扩权：`minimumCapabilityLimit`
（`providers.go:713-721`）取较小非零值，而 `filterTokenCapabilities`
（`service.go:1971-1983`）只在 `> 0` 时收紧——把限额写大等价于不设限，不能突破任何东西。

---

## 4. Mantle 单独裁决：唯一缺少第①道闸的地方，但②③把缺口补满

Mantle 三个 profile 是全仓唯一 adapter 能力来自 binding 的地方（`providers.go:580` →
`:617/:625/:632`；`bedrockmantle/adapter.go:60-72` 把 `options.Capabilities` 原样存下）。
第①道闸对 Mantle 退化为恒等式。**但逐 profile 检查"能被放宽的具体位"，每一位都被另外
两道独立闸门堵死：**

| Profile | 钉死默认（`domain/models.go:517-526`） | 可被放宽的位 | 拦截点 |
|---|---|---|---|
| `ProfileBedrockMantleOpenAIChat` | Chat,Streaming,Tools,Vision,JSONMode,DeveloperRole,Reasoning,StreamUsage | chat 家族**已满**，只剩 Embeddings/Images/Moderations/Rerank/… | ③ `filterPrimitiveTargets` —— 算子表 `provider/profile.go:83` 只有 `OperationChat` / `OperationChatStream`，声明 Embeddings 解析不出 primitive，target 被剔除 |
| `ProfileBedrockMantleOpenAIResponses` | 同上但**无 Reasoning**（注释：canonical response mapper 保不住 reasoning items） | `Reasoning` | ② `provider_fields.go:49-53` 对该 ProfileID **无条件**把 `reasoning_effort` 列为不可表示 ⇒ target 被剔除 |
| `ProfileBedrockMantleAnthropicMessages` | 无 `JSONMode`、无 `DeveloperRole` | `JSONMode`、`DeveloperRole` | ② `provider_fields.go:41-47` 对该 ProfileID **无条件**列 `response_format` 与 `messages[].role=developer` ⇒ target 被剔除 |

②③ 的判定输入只有 `target.ProfileID` 与请求本身，**不含任何声明能力**，因此不受 binding
放宽影响。结论：Mantle 也无法把任何一位能力送到上游。

---

## 5. 证伪之后剩下的是什么（这才是应该保留的条目）

M1 属实，所以确实存在一个真实但**降级后**的问题——性质从"越权"变成"接受无效输入"：

1. **管理面接受一个永远不会生效的声明。** 一个 Gemini Provider 可以在 store 里、在 Admin
   API 响应里、在控制台上显示 `tools: true`，而数据面永远拒绝 tools 请求。运维看到的能力
   与系统实际行为不一致——这是**可诊断性 / 误导**问题（P3），不是能力上限被突破。
2. **失败被推迟到运行时。** 本该在 `POST /providers` 就拒的配置，变成了上线后每个 tools
   请求一个 400 `unsupported_feature`。按仓库"fail-closed 且尽早"的纪律，这一步应该前移。
3. **能力探测的额外计费调用（仅 Mantle）。** `LegacyAdapterBridge.CapabilityDetectionPlan`
   （`internal/provider/profile.go:48-90`）用 `b.Capabilities()` 生成探针，`MayBill: true`。
   Gemini/Bedrock Converse 走硬编码集合，不受影响（该函数 `:44-47` 的注释正是这么声明的，
   且经本次核对属实）；Mantle 因能力来自 binding，一个被放宽的声明会多产生一次注定失败的
   计费探针。爆炸半径 = 运维自己的账单，量级为单次 ≤2048 字节 / ≤16 token。
4. **修复方向不变，但理由变了。** 仍建议去掉 `isStrictOperationProfile` 前置、让
   `⊆ DefaultProviderCapabilitiesForProfile` 对所有 profile 生效——理由是"拒绝无效配置、
   让管理面与数据面说同一句话"，**而不是**"堵住一条越权路径"。同时建议补一条负面测试：
   Gemini/Bedrock-Converse Provider 声明 `tools` 必须在 `POST /providers` 被拒。

对 A3.md §5 下限 #2 结尾那句「唯一残留风险：旧备份里若含被放宽的 Beta binding，restore
会把那个软 ceiling 一并带回」——**同样 REFUTED**。restore 回来的放宽 binding 依旧被
`deploymentCapabilities` 的 AND 交集与 profile 键控过滤中和，服务集不变。

---

## 6. 应当被保护的既有控制点（本次证伪的正面产物）

- **C-1 `deploymentCapabilities` 的逐位 AND 交集**（`internal/app/providers.go:690-711`）：
  声明只能缩不能扩。这是把"管理员声明"降级为"意图"而非"授权"的关键一行。
- **C-2 Gemini / Bedrock adapter 的 `Options` 结构里没有 Capabilities 字段**
  （`gemini/adapter.go:25-30`、`bedrock/adapter.go:24-31`）：天花板不可注入是**类型层面**
  保证的，不靠调用方自觉。这比任何运行时校验都强，**不要为了"统一"给它们加上这个字段**。
- **C-3 profile 键控的兼容性过滤**（`internal/compatibility/provider_fields.go:12-75`，
  经 `service.go:751/1408/1609` 施加）：判定只看 ProfileID，与能力声明完全解耦，
  构成第二条独立防线。
- **C-4 编译期算子表**（`internal/provider/profile.go:70-86` + `service.go:752/1409/1608`）：
  profile 没有的 primitive，声明再宽也解析不出来。
- **C-5 adapter translate 期的无条件拒绝**（`gemini/adapter.go:416-417,443,452,456`；
  `bedrock/adapter.go:484-486`）：encode 前最后一道 fail-closed，且拒绝理由不含任何声明能力。
- **C-6 catalog 侧 `Key.Ceiling()`**（`internal/modelcatalog/catalog.go:215-217`）直接返回
  `DefaultProviderCapabilitiesForProfile`，与 binding 无关——目录永远不会为放宽的声明背书。

四道闸互相独立、分布在三个包、由不同机制（编译期常量 / 类型结构 / ProfileID 键控表 /
算子注册表）执行。这是纵深防御做对了的样子。

---

## 7. 附录

### 读过的文件
- `docs/review/260811/releases.1.0.0/role-prompts.md`（§1、§8）
- `docs/review/260811/releases.1.0.0/findings/A3.md`
- `internal/app/admin_providers.go`（`:880-1010`，含 `:909-919`、`:939-941`、`:975-982`）
- `internal/app/providers.go`（`:490-540`、`:560-645`、`:678-724`）
- `internal/domain/models.go`（`:255-300` binding Validate、`:354-374` ProviderCapabilities、`:480-530` profile 默认）
- `internal/provider/provider.go`（`:127-157` Capabilities / CapabilityReporter）
- `internal/provider/profile.go`（`:40-90` 算子表与探测计划、`:150-230` LegacyAdapterBridge、`:400-425` Mantle manifest）
- `internal/provider/gemini/adapter.go`（`:25-36`、`:110-130`、`:383-470`、`:525-570`）
- `internal/provider/bedrock/adapter.go`（`:24-36`、`:284-310`、`:460-510`）
- `internal/provider/bedrockmantle/adapter.go`（`:1-95`）
- `internal/provider/capability_detection.go`（`:35-130`）
- `internal/compatibility/provider_fields.go`（`:1-75`）
- `internal/compatibility/manifest.go`（`:125-185`）
- `internal/gateway/service.go`（`:745-790`、`:1240-1275`、`:1400-1445`、`:1600-1620`、`:1938-1985`）
- `internal/gateway/service_test.go`（`:1000-1065`）
- `internal/modelcatalog/catalog.go`（`:190-230`）
- `internal/semantic/request.go`（`:85-160` DeriveRequirements）
- `internal/app/admin_deployment_resolution_test.go`、`internal/modelcatalog/catalog_test.go`（定位既有守护）

### 走过的调用链
1. **管理面（M1 核对）**：`providerInput.provider(...)` → `:909-919` provider 级天花板 →
   `:939-941` binding 级天花板 → `isStrictOperationProfile` `:975-982` →
   `domain.ProviderProfileBinding.Validate`（`domain/models.go:269-289`）→
   `ValidateProviderProfile`。确认 Gemini/Converse/Mantle 三者的能力声明原样落盘。
2. **拓扑激活**：`newProviderBindingAdapter`（`app/providers.go:572-640`）→ 各 provider 构造 →
   `deploymentCapabilities`（`:513` → `:690-711`）→ `adapterCapabilitiesFor`（`:722-726`）→
   `provider.CapabilityReporter.Capabilities()` → `registry.Register(provider.Target{...})`。
3. **数据面 Chat**：`gateway.Service.Chat` → `openaiwire.DecodeGenerate` →
   `filterSemanticCapabilities`（`:750`/`:1940`）→ `filterGenerateProfileCompatibility`
   （`:751`/`:1952` → `compatibility.UnsupportedGenerateFields`）→ `filterPrimitiveTargets`
   （`:752`/`:1964`）→ `len(targets)==0` → 400 `unsupported_feature`（`:753-755`）。
   后续（若通过）`filterTokenCapabilities`（`:776`）→ `beginRequestRun` → attempt →
   `gemini.translateChat`（`:416`）/ `bedrock.translateRequest`（`:485`）→ `postJSON`。
4. **数据面流式 / Embeddings / native Messages**：`service.go:1407-1409`、`:1608-1609`、
   `:1060` → `prepareNativeMessages:1250-1251`。三条同样不以声明能力放行。
5. **探测**：`LegacyAdapterBridge.CapabilityDetectionPlan`（`provider/profile.go:48-90`）→
   `b.Capabilities()`（`:186-189`）→ 底层 adapter `Capabilities()`。

### 运行过的命令
- `git rev-parse HEAD`、`git status --porcelain`（前后各一次）
- 多次 `grep -rn` / `sed -n` 定位上述 file:line
- `go test ./internal/gateway/ ./internal/provider/gemini/ ./internal/provider/bedrock/ ./internal/compatibility/... -count=1`
  → 全部 `ok`（gateway 3.642s、gemini 1.751s、bedrock 2.321s、compatibility 三包全绿）

### 无法验证 / 声明
- 未构造真实的 widened Gemini/Bedrock Provider 走一次端到端实跑（角色约束：不写武器化验证
  代码，且不得修改仓库文件）。裁决基于四道闸门的静态代码路径，其中 C-2（Options 结构无
  Capabilities 字段）是类型层面的**编译期**事实，不依赖运行时行为，证据强度最高。
- 未审查 Admin 控制台前端如何呈现被放宽的能力声明（§5 第 1 点的用户可见后果），
  该部分归 D1 视角。
