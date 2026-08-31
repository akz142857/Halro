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

## 3. 能合并的三处

不动分层就能砍掉的重复：

### 3.1 `profileAllowsPrimitive` 与 `ProfileManifest`

`internal/provider/profile.go` 里两张表，**在同一个文件里说了两遍重叠的话**：
前者是 `profile → operation → primitive` 的白名单，后者是同一组绑定的完整声明。
`ProfileManifest.Validate` 拿前者校验后者。

合并方向：只留 manifest，把 `profileAllowsPrimitive` 变成从 manifest 派生的一致性检查，
或者反过来只留白名单、manifest 由它生成。**要小心的是**：两张表现在互相校验，
合成一张就少了一层，需要用别的东西补上——比如把 manifest 变成
`map[profileID]map[Operation]Primitive` 一张表加派生。

### 3.2 字段申报与端点清单的 `ProfileCoverage`

`internal/compatibility/provider_fields.go` 说「这个 profile carry 不了什么」，
`manifest.go` 的 `ProfileCoverage.UnsupportedRequestFields` 说同一件事的另一种写法。
两者现在靠 `TestTheManifestDeclaresEverythingTheRulesRefuse` 绑在一起。

合并方向：让 coverage 从字段规则派生。**做不到完全派生**，因为 manifest 允许声明比规则
更多的东西（有些端点成员根本到不了语义模型，没有规则会拒它们）——所以只能派生一部分，
剩下的仍要手写。收益因此比看上去小。

### 3.3 `app/providers.go` 的构造 switch

可以变成随 profile 注册的构造函数：`provider` 层暴露一个
`func(ConnectionContext) (Adapter, error)`，平台包各自注册，`app` 只做查表。

**这一处收益最实在**，而且它正是 MiniMax 那次「六步里唯一没有测试」的那一步。
现在它有守卫了，但守卫是「每个 profile 都得有分支」，而不是「分支自己声明自己」。

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

顺序：片 1 跑通 → MiniMax 的假设收敛 → 再做 §3。

## 6. 明确不做的

| 项 | 理由 |
|---|---|
| 把 profile 表搬出 `domain` | `ProviderProfileBinding.Validate` 要用能力上限校验**已存储的记录**，那时适配器还不存在。搬走就得在启动时先加载再校验，那是 fail-open |
| 运行时插件加载（Go plugin / WASM / 动态注册） | 单进程、安全优先；出网只有 SafeTransport 一条路。插件自己发 HTTP，那层就没了 |
| 启动期把平台描述符注入 domain 的全局表 | 可变全局状态：单独 import `domain` 的测试会看到空表，`ValidateProviderProfile` 的行为取决于 init 顺序 |
| 声明式 provider descriptor（用数据描述路径/认证/字段映射/错误码映射） | MiniMax 恰好证明了难点在哪：`base_resp` 码到熔断类别的映射、`reasoning_effort` 到 `thinking` 的条件翻译（还要分 M3 / M2.x）、流式 usage 在分块边界上的判定。DSL 要么覆盖不到，要么复杂到等于用另一种语法写代码——而那种语法没有类型检查、没有 `go vet`、没有 §2 那批会变红的测试 |
