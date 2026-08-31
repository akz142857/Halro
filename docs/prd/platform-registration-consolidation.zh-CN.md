# 平台登记点合并 —— 提案，未实施

状态：**提案。实施要等 MiniMax 的真实账号验证跑完**（见 §5）
建立日期：2026-08-31
起因：MiniMax 适配过程中两次被登记点咬到，以及随后关于「为什么不做成插件接口」的讨论
范围：`internal/provider`、`internal/compatibility`、`internal/app`
相关：[Adding a provider platform](../contracts/adding-a-platform.md)、
　　　[MiniMax 适配方案](minimax-adaptation-plan.zh-CN.md) §8.6 §8.7

## 0. 这份提案要回答什么

接一个平台要动八处。有人会说：为什么不设计一套接口，一平台一目录，实现接口即可。

**这个方向对，而且一半已经在了。** `internal/provider/` 下已经是一平台一目录，核心接口
`provider.Adapter` 四个方法，外加十几个可选接口按需实现。MiniMax 甚至没有新建 provider 包。

散在八处的不是**实现**，是**声明**。这份提案要回答的是：哪些声明能合并、哪些不能、
以及为什么合并不是解决那两个 bug 的办法。

## 1. 先把因果关系摆对

八处里出事的是两处：

| 出事的登记点 | 有守卫吗 |
|---|---|
| `implementedProviderType`（Admin 写入路径的类型准入） | **没有**，而且不在清单里 |
| `CanEnumerate` 兼作凭据测试的判据 | **没有**，是建模错误不是登记遗漏 |

其余六处每一处漏掉都会红一个点名的测试，六处都做对了。

**所以相关性不是「登记点多 ⇒ 出错」，是「登记点没有守卫 ⇒ 出错」。**

这条区分决定了优先级：先补守卫（已完成，见 §2），再谈合并。一套插件接口如果没有
「每个平台都必须回答这几个问题」的编译期或测试期强制，照样会漏——
只是漏的位置从八个文件变成一个接口里没人实现的方法。

第二个 bug 尤其说明问题：把 MiniMax 挪进独立目录、实现接口，**一样会犯**，而且更糟——
如果接口里只有 `CanEnumerate()` 这一个方法，每个平台都会撞同一堵墙，因为墙在接口本身。

## 2. 已完成的部分：补守卫

2026-08-31 已落地，不在本提案的待办里：

| 守卫 | 守住哪个登记点 |
|---|---|
| `TestEveryOfferedProviderTypeIsAcceptedOnSave` | 控制台提供的类型，写入路径必须接受 |
| `TestEveryRegisteredProviderTypePassesInstanceValidation` | 表与 `ProviderInstance.Validate` 的类型 switch |
| `TestNoProviderTypeDefaultsToAWithheldProfile` | 类型的默认 profile 必须可达 |
| `TestEveryReachableProfileBuildsAnAdapter` | 第 6 步，曾被文档标为「唯一没有测试覆盖」 |
| `TestEveryReachableProfileReachesTheNetworkWhenCalled` | 建得出来 ≠ 调得动 |
| `TestNativeAnthropicListsAgree` | native schema / 网关准入 / beta 允许三份清单 |
| `TestNoNativeProfileIsWithheld` | native profile 不能是被 withheld 的 |
| `TestEveryPrimitiveConstantIsBoundBySomeProfile` | 没有 profile 绑定的死常量 |
| `TestSemanticGenerationPrimitivesAreAllBound` | 声明为 semantic 的原语必须被绑定 |
| `TestEveryAnthropicWireProfileIsExcludedFromTheReasoningProbe` | Anthropic wire 的推理探测排除 |
| `domain.IsRegisteredProviderType` | 删掉第三份类型清单，改为查表 |

每一条都做了反向验证：拆掉登记、确认变红、再装回去。其中一条反向验证**没有红**，
因此推翻了一个原本写在源码注释里的错误论断——详见 §4。

## 3. 能合并的三处（§3.1、§3.3 已完成）

不动分层就能砍掉的重复：

### 3.1 `profileAllowsPrimitive` 与 `ProfileManifest` —— **已实施（2026-08-31）**

**上一版把它写成「两张表」，实际动手才发现是同一份真相写了四遍**，其中三遍已经被
`Validate` 互相校验着——那也正是它们没走散的原因，以及只有一份在真正携带信息的信号：

1. `profileAllowsPrimitive` 的 `profile → operation → primitive` 白名单；
2. manifest 的 `Operations`（就是绑定的操作，按声明顺序）；
3. manifest 的 `PrimitiveBindings[].SemanticOperation`（`semanticOperationFor` 从操作算得出来，
   而 `Validate` 本来就在拒绝不一致的值）；
4. manifest 的 `ProviderType` / `AccessSurface` / `CredentialScheme`——**domain 表里已经有了**，
   而 `Validate` 调的 `domain.ValidateProviderProfile` 正是拿它们去核 domain 表。
   一个必须与别处相等、且已经被校验相等的值，是不携带信息的。

合并成 `internal/provider/profile_bindings.go` 一张
`map[ProviderProfileID]{Revision, []operationBinding}`，其余全部派生。
`Revision` 留着，它是这里唯一推不出来的东西：它记录一个 profile 在标识符不变的情况下
含义变过（两条 Mantle 行换过路由、Anthropic 行长出了 files 与 batches），树里没有别的地方记得。

`profile.go` 少了 122 行；两个重复的 `chatPair` / `anthropicWire` 形状抽成了命名函数。

**替换是证出来的，不是假设的**：新旧并存跑过一次等价性对比——全部注册 profile 的 manifest
逐字段 `DeepEqual`，加上 12,298 组 `(profile, operation, primitive)` 交叉比对，绿了才删旧表。
对比发现**一处差异，方向是收紧的**：旧 map 靠索引作答，未服务的操作返回零值 `Primitive`，
拿它和空原语比就是 `true`——读作「未绑定的操作允许空绑定」。这条路 `Validate` 走不到
（它先拒空原语），新实现答 `false`。差异写进了源码注释，没有抹平。

**代价说清楚**：合并后 `Validate` 校验内置 manifest 变成拿表核自己，是同义反复；
它对**调用方传入的** manifest 仍然有效，而那才是它当初要防的场景。
**但没有损失保护**——旧结构只能发现两张表**互相矛盾**，一个在两处都写错的绑定当时也照样通过。
所以合并去掉的是「矛盾的可能性」，不是一层正确性检查，这里从来没有过那层。

仍然抓不到的：把两个 profile 的原语对调（两个都还绑着，没有孤儿常量）。
已记进契约文档的「没有守卫的步骤」，并指明平台自己的 wiring 测试是补这一格的办法。

### 3.2 字段申报与端点清单的 `ProfileCoverage`

`internal/compatibility/provider_fields.go` 说「这个 profile carry 不了什么」，
`manifest.go` 的 `ProfileCoverage.UnsupportedRequestFields` 说同一件事的另一种写法。
两者现在靠 `TestTheManifestDeclaresEverythingTheRulesRefuse` 绑在一起。

合并方向：让 coverage 从字段规则派生。**做不到完全派生**，因为 manifest 允许声明比规则
更多的东西（有些端点成员根本到不了语义模型，没有规则会拒它们）——所以只能派生一部分，
剩下的仍要手写。收益因此比看上去小。

### 3.3 `app/providers.go` 的构造 switch —— **已实施（2026-08-31）**

改成了 `map[ProviderProfileID]adapterBuilder`，一 profile 一行，三个阶段：
`validateEndpoint`（可选，只有绑定到特定 host 的 profile 有）→ `authorize` → `build`。
原来的 127 行嵌套 switch 变成 5 行查表，实现搬到 `internal/app/provider_adapters.go`。

**先纠正这份提案自己写错的一句话。** 上一版写着改完之后「漏一个平台在编译期就没法通过」。
**做不到。** Go 没有 exhaustive switch，map 字面量也没有「必须覆盖全部 key」的编译检查。
唯一能做到编译期强制的形态，是把构造函数放进 profile 表行里——而表在 `domain`，
不能 import 平台包（§6 第一行）。所以这条路是封死的，不是没做。

真实收益是三条，都不是正确性：

1. **可枚举。** switch 的分支列不出来，map 可以。于是多了一个原来写不出来的守卫：
   `TestAdapterBuilderTableCoversExactlyTheRegisteredProfiles` 查**两个方向**——
   缺行，以及**孤儿行**（构造还在、profile 已经不在表里了）。后者用 switch 根本发现不了，
   一个死分支可以留到天荒地老，而且 grep 到它的人会以为那个平台还支持。
2. **一处读完。** 原来是「类型 switch 里套两层 profile switch」，MiniMax 和 Bedrock 各套一层，
   凭据头名分散在三个缩进层级里。现在一 profile 一行，头名和 fallback 在同一行上。
3. **组合根瘦了。** `providers.go` 少了 122 行。

**没有做的**：让平台包自己 `init()` 注册。那要么成环（registry import 平台包，平台包 import
registry），要么退化成 blank import + init 副作用的可变全局状态——就是 §6 第三行拒绝的那种，
而且换来的仍然不是编译期强制。

守卫两个方向都做了反向验证：删掉 `minimax.responses.v1` 一行→两条测试点名报错；
加一条 `retired.profile.v1` 孤儿行→覆盖测试点名报错。

## 4. 一处必须记下来的教训

写第 2 节那批守卫时，我给 `semanticGenerationPrimitives` 写了一条注释，说漏登记会让请求
「掉进 legacy Chat 路径，那个分支会拒绝——在路由已经选定目标之后」。

**反向验证把这句话推翻了。** 拆掉登记，测试没有红。查代码：`Chat` 在 Responses 分支上会走
`chatViaResponses`，翻译之后仍然打 `/v1/responses`，不拒绝。真实后果是**保真度**而不是可用性——
语义请求会绕道 OpenAI Chat 中间表示，吃掉那层的损失，而这些损失该 profile 的字段规则没有申报，
因为走语义路径时它们不会发生。

这个后果**没有任何机械检查看得见**，已按原样记进契约文档的「没有守卫的步骤」一节，
而不是留着一句听起来被覆盖了的话。

顺带说明了一件事：**给一个登记点写守卫之前，先去验它漏掉时到底会发生什么。**
凭想象写出来的危害，会连带写出一条抓不到它的守卫。

## 5. 为什么现在不做

MiniMax 还没有跑过真实账号（[适配方案](minimax-adaptation-plan.zh-CN.md) §7 有十三条未验证项）。
片 1 的结论会改动能力集、字段申报、错误归类，这些正好落在 §3 要重构的那几处。

**现在动结构，片 1 的结果回来时会分不清是适配错了还是重构错了。** 这是本仓库
「要归因一个症状到某次改动，就把两边都构建出来跑同一个输入」那条规矩的一个直接推论：
两个变量同时动，就没有那个「两边」了。

**这条推迟理由下得太宽了，2026-08-31 订正**：对着片 1 的六条产出逐条比，只有 §3.2 真的撞上。
片 1 会改能力集（domain 表）、字段申报（compatibility）、错误归类（provider/openai）、
以及凭据方案（如果 Bearer 在 Anthropic 路由上不通就要拆 scheme）——**primitive 绑定和构造分支
一条都不沾**。

所以：

- §3.1（primitive 绑定两张表合并）**不撞**，可以做。
- §3.2（字段申报与 `ProfileCoverage` 派生）**撞**，等片 1。
- §3.3（构造表）**不撞**，已完成。

顺序：§3.3 ✅ → §3.1 ✅ → 片 1 跑通 → §3.2。

## 6. 明确不做的

| 项 | 理由 |
|---|---|
| 把 profile 表搬出 `domain` | `ProviderProfileBinding.Validate` 要用能力上限校验**已存储的记录**，那时适配器还不存在。搬走就得在启动时先加载再校验，那是 fail-open |
| 运行时插件加载（Go plugin / WASM / 动态注册） | 单进程、安全优先；出网只有 SafeTransport 一条路。插件自己发 HTTP，那层就没了 |
| 启动期把平台描述符注入 domain 的全局表 | 可变全局状态：单独 import `domain` 的测试会看到空表，`ValidateProviderProfile` 的行为取决于 init 顺序 |
| 声明式 provider descriptor（用数据描述路径/认证/字段映射/错误码映射） | MiniMax 恰好证明了难点在哪：`base_resp` 码到熔断类别的映射、`reasoning_effort` 到 `thinking` 的条件翻译（还要分 M3 / M2.x）、流式 usage 在分块边界上的判定。DSL 要么覆盖不到，要么复杂到等于用另一种语法写代码——而那种语法没有类型检查、没有 `go vet`、没有 §2 那批会变红的测试 |
