# 能力模型评估：三条论点的深度核查

评审日期 2026-08-23。触发背景：一次"给已有部署打开视觉、用远程图片 URL 做识别"的联调，连续撞上五种不同的拒绝，每一种的提示都不指向真正的原因。本文核查提出的三条论点，逐条给出结论、证据和代价。

约定：【成立】【部分成立】【已具备】【不成立】四种判定；每条发现带 `文件:行号`。

---

## 结论摘要

| # | 论点 | 判定 | 一句话 |
|---|---|---|---|
| 1 | 应该有一个自己的全能力模型，各平台只是适配参数 | **成立，且比论点描述的更严重** | 不是"没有统一模型"，是**有四个互不知道对方存在的模型**，同一个事实分散在三套词表里 |
| 2 | 适配应该有独立域，否则就是打补丁写 if | **部分成立，但要害不在 if** | 适配域已经有了（一家一个包，请求路径零分支）；真正的成本是**一个平台的描述被切成 6 份散在 4 个包里** |
| 3 | 能力模型应统一在服务端构建，前后端共用 | **已具备一半，裂缝在另一半** | 能力布尔已经服务端发布并有双向校验；但**决定路由的字段级模型一个字节都没发给前端** |

一句话总括：**运营者能编辑的能力模型，和真正决定路由的能力模型，是两个不同的模型，而控制台只显示前一个。** 三条论点都指向这同一处断裂的不同侧面。

---

## 完成度（2026-08-23 实施后复核）

本文提出 P0/P1/P2 三级建议，其中 P0 的一项、P1 的两步已实施。**三条论点都没有全部完成**，逐条如下。数字是实施后重新量的，不是文中原始数据。

| 论点 | 状态 | 已关闭的 | 仍然存在的 |
|---|---|---|---|
| 1 全能力模型 | **大部分已关闭** | ①② 命名统一、合成一张配对表、漏配对会红；一个事实（能取图）从 ③ 移入 ① 并因此在控制台可见；能力字典收敛成一张访问器表——拷贝 4 → 2，手写转换器 3 → 0，按名字的 switch 6 → 0 | ③ 字段清单与 ④ 发布清单仍是独立表示（它们承载的是没有能力对应的字段级事实，收敛属于 P2 第二阶段） |
| 2 独立适配域 | **部分** | 六个登记点里最后一个静默的（字段规则）改成按 profile 登记，漏登记会指名报错；一个新 profile 加进表后，缺哪一层由测试逐条点出来 | 六处登记仍是六处——物理合并被包依赖方向挡住（见下）；清单仍是 20 端点 × 65 条手写 coverage |
| 3 服务端构建 | **已关闭** | 字段级限制已发布到控制台并在勾选处显示；前端的三份镜像现在各有双向守卫，服务端加一项能力时三个都会红 | 守卫仍是 CI 期而非运行时，但链条是闭合的（见下） |

论点 3 的守卫链，逐环列出，因为"构建期而非运行时"这句话本身容易被读成有缺口：

1. 服务端改了能力字典 → 不重新生成 golden，`TestProviderProfilesGoldenMatchesConsoleFixture` 失败
2. 重新生成 golden → `useProviderProfiles.test.ts` 比对 `emptyCapabilities` 的键集，类型没跟上就失败
3. 同上 → `i18n.test.tsx` 比对两个 locale 的 `capabilities.*`，任一语言缺文案就失败
4. 同上 → `DeploymentsPage.test.tsx` 比对分组，没有分组画它就失败

四环都是双向的（服务端有前端没有、前端有服务端没有，各自报错）。CI 不可能在漂移状态下通过。类型那一环的主体是 `emptyCapabilities` 而不是接口本身，因为类型没有运行时形状——但它是该类型的完整字面量，TypeScript 已经强制它与接口一一对应，所以比对它的键就是比对接口。

值得记下的是这一环之前为什么是缺口：接口漏一个字段时 `tsc --noEmit` **一声不吭**（反向验证实测），字段只是不存在，于是每条有类型的路径都会静默忽略服务端发来的这项能力。

加 `fetched_image` 时动了 20 余个文件，其中两处按名字枚举的地方（`admin_deployments.go` 的 `capabilitiesEnabledByName`、`models.go` 里逐字段 OR 的绑定汇总）是靠测试失败才发现的——它们是 P2 第一阶段的直接动因。

### P2 第一阶段实测（2026-08-23）

`internal/domain/capability_dictionary.go` 是现在唯一写下"能力名 ↔ 存储成员"的地方。名字列表、按名读、按名写、证据投影、证据校验、子集判断、差异报告，原先是七处各自枚举同一批名字，现在全部走这张表。

| 指标 | 之前 | 现在 |
|---|---|---|
| `ProviderCapabilities` 拷贝 | 4（Go 3 + TS 1） | **2**（Go 1 + TS 1，后者有守卫） |
| 手写逐字段转换器 | 3 | **0** |
| 按名字枚举的 switch/map | 6 | **0** |

`provider.Capabilities` 与 `app.providerCapabilityView` 都成了 `domain.ProviderCapabilities` 的类型别名——它们的字段和 json tag 本来就逐一相同，"只为保持一致而存在的拷贝"长的就是这个样子。

两个新守卫用反射而不是又一份清单：结构体加了 bool 成员而字典没加、或两个名字指向同一个成员，都会红。两种都做了反向验证。

顺带查出一处被拷贝掩盖的分歧：`provider.Capabilities.AnyOperation()` 把 `Streaming` 算作一项操作，`domain` 版不算。合并后按 domain 的定义——流式是对话的修饰而非独立操作——这收紧了注册表的一道闸门，方向是 fail-closed。

### P2 第二阶段：为什么不是"一份描述"，而是"漏了就红"（2026-08-23）

本文原先的设想是"一个平台一份描述，注册时提供，六处登记收敛成一处"。实施时发现**物理合并做不到**，原因是包依赖方向：

- 能力上限、访问面、凭据方案在 `domain`
- Operation → Primitive 绑定在 `provider`（`provider` 依赖 `domain`）
- 字段规则是对 `semantic.GenerateRequest` 的闭包，在 `compatibility`（也依赖 `domain`）
- 适配器构造在 `app`（依赖全部）

一份能同时装下这四样的结构体必须住在一个被这四个包都导入的地方，而它又要引用它们的类型——是环。要真做，得新开一个顶层包并把 `semantic`/`provider` 的类型上移，或者走代码生成。两者都不是这一轮的规模，而且前者会把"谁定义什么"这件事搅乱。

所以第二阶段改为攻击真正的痛点：**六处登记不痛，静默地漏掉其中一处才痛。**

盘点下来，六处里已经有守卫的是 profile manifest（`TestCeilingWithinProfileManifestOperations`，走 domain 表）和端点 coverage（`EndpointCompatibilityManifest.Validate` 要求覆盖每个 profile）。唯一静默的是字段规则：一个新 profile 落到 `default` 分支后**看起来是工作的**——平台跑得起来、纯文本能过，工具、图片、结构化输出被拒且没有任何东西说明为什么。

它现在是按 profile 登记的表（`generateFieldRules`），`legacyFieldRules` 保留为普通函数，所以"登记过"和"回落了"可区分。配套两个测试都走 domain 表而不是自带清单：

- 表里有、字段规则没登记 → `newplatform.chat.v1 serves chat but declares no generate field rules; register it in provider_fields.go`
- 表里有、任何端点清单都不覆盖 → `newplatform.chat.v1 is in the profile table and reachable through no endpoint manifest`
- 反向：登记了表里没有的 profile → 同样报错（留着的规则会静默收养下一个复用该标识符的 profile）

反向验证用的是真的往表里塞一个新平台，两条都如实点名。

**剩下的**：③ 字段清单（11 个 profile 分支）与 ④ 发布清单（20 端点 × 65 条）仍是独立表示。它们承载的是没有能力对应的字段级事实（seed、n、detail），收敛它们属于 P2 第二阶段，和论点 2 是同一件事。

### 建议清单的逐项状态

| 项 | 状态 |
|---|---|
| P0-1 发布字段级限制给控制台 | **已实施** |
| P0-2 拒绝原因入用量记录 | **未做**，文中已重新定级为接近 P1（账本事件是持久化 schema） |
| P0-3 模型目录做厚 + 覆盖率可见 | **一半已存在**（能力证据来源标签），**一半未做**（把目录做厚需逐个模型核对官方文档，是资料工作） |
| P1 收敛表示 | **两步都已实施**，范围如上表 |
| P2 第一阶段 能力字典收敛 | **已实施** |
| P2 第二阶段 登记完整性 | **已实施**（见下：物理合并不可行，改为让遗漏发声） |
| P2 第三阶段 清单 coverage 由描述生成 | **未做** |

---

## 一、能力这件事，系统里现在有四种表示

### 1.1 四个模型

| # | 表示 | 位置 | 规模 | 谁在用 |
|---|---|---|---|---|
| ① | 能力布尔集 `domain.ProviderCapabilities` | `internal/domain/models.go:466` | 18 个开关 + 2 个上限 | 控制台勾选、Profile 上限、部署声明、模型目录 |
| ② | 需求布尔集 `semantic.Requirements` | `internal/semantic/request.go:9` | 11 个开关 | 从请求内容推导，供路由过滤 |
| ③ | 字段名字符串表 `UnsupportedGenerateFields` | `internal/compatibility/provider_fields.go:33` | 11 个 profile 分支、329 行 | 逐 profile 声明"载不动哪些字段" |
| ④ | 发布清单 `endpoint-manifests.json` | `internal/compatibility/manifest.go` | 20 个端点、65 条 profile coverage | 对外发布的兼容契约，golden 快照 |

### 1.2 ① 和 ② 之间没有类型关系

两者靠 `filterSemanticCapabilities`（`internal/gateway/service.go:2410`）里 **7 行手写配对** 连接，而且名字系统性地对不上：

| ② 需求 | ① 能力 |
|---|---|
| `InputImage` | `Vision` |
| `StructuredJSON` | `JSONMode` |
| `Tools` | `Tools` |
| `ParallelTools` | **无对应能力** |
| `Seed` | **无对应能力** |
| `MultipleCandidates` | **无对应能力** |
| `EndUserReference` | **无对应能力** |

② 有 4 项在 ① 里没有对应能力，只能落到 ③ 用字段名表达。于是"某个平台支不支持某件事"这个问题，**答案在哪个词表里，取决于那件事历史上被归成了"能力"还是"字段"**——这个归类没有任何原则可循，`seed` 是字段而 `vision` 是能力，纯属历史。

### 1.3 ① 本身还有三份 Go 拷贝 + 一份 TS 拷贝

| 位置 | 字段数 | 同步方式 |
|---|---|---|
| `internal/domain/models.go:466` | 20 | 权威 |
| `internal/provider/provider.go:166` | 20 | `internal/app/providers.go:800` 手写逐字段转换 |
| `internal/app/admin_provider_profiles.go:23` | 20 | `:47` 手写逐字段转换（字段顺序还不一样） |
| `web/src/types.ts:341` | 20 | 手工镜像 |

加一项能力要动的文件（精确统计，已排除 `revision` 误匹配）：**23 个**，其中前端 5 个。

### 1.4 这就是这两天所有困惑的机制

Bedrock 的事实是"**能看图，但不会替调用方去取 URL**"。① 里只有 `Vision bool`，装不下这个区分。于是这半条事实被迫写进 ③（`provider_fields.go:173`）。

而控制台只显示 ①。所以运营者的体验是：

1. 勾上"视觉"，看到 `1/1 项启用`，全绿
2. 发一个带远程 URL 的请求
3. 被拒，提示 `model route does not support the requested chat capabilities`
4. **拒它的那条规则，控制台从来没有见过，也没有任何界面能显示它**

这不是提示文案不好，是模型缺了一个维度。

---

## 二、论点 2：适配域

### 2.1 已经做到的部分（论点在这里估计偏低）

- **一家一个包**：`internal/provider/{openai,anthropic,bedrock,bedrockmantle,gemini}/`
- **接口很小**：`Adapter` 只有 4 个方法（`internal/provider/provider.go:158`）——`Type/Chat/ChatStream/Embed/Close`
- **请求路径零服务商分支**：`gateway/service.go` 的可移植路径上没有任何 `switch providerType`；仅有的 3 处 profile 判断（`:1296`、`:1538-1539`）全在 Anthropic **原生**路径上，而原生模式按定义就是绑定某一家协议的，不算泄漏
- **那个 11 分支的 switch 不是补丁**：`provider_fields.go` 的 `default` 分支 fail-closed（`:320`），未知 profile 一律拒掉富语义。**新平台不写它 = 只能跑纯文本，不会出错**。它是解锁清单，不是补丁堆

### 2.2 真正的成本：注册面

加一个 Provider Profile 要在 **6 个文件**登记：

| 文件 | 登记什么 |
|---|---|
| `internal/domain/provider_profile.go:48` | Profile ID 常量 |
| `internal/domain/provider_table.go:217` | 表行：访问面、凭据方案、端点模板、Defaults/Ceiling |
| `internal/provider/profile.go:84` | Operation → Primitive 绑定 |
| `internal/compatibility/provider_fields.go:173` | 字段级损失声明 |
| `internal/compatibility/manifest.go:193,207,229…` | 每个它服务的端点各一条 coverage |
| `internal/app/providers.go:719,766` | 适配器构造 |

清单那一项是组合爆炸的：**20 个端点 × 65 条 coverage，全部手写**。一个服务 3 个端点的新 profile 要写 3 条，每条都得引用该端点 RequestFields 里已存在的字段名，否则 `Validate()` 拒绝。

### 2.3 论点 2 的正确形式

缺的不是"适配域"——那个已经有了。缺的是**"平台描述"这个一等公民**：

> 现在一个平台的描述被切成 6 份散在 4 个包里，系统里**没有任何一个地方**能回答"Bedrock Mantle 到底支持什么"。

要回答这个问题，今天必须同时读 profile 表行、primitive 绑定、字段 switch 分支、N 条清单 coverage、以及模型目录条目。这才是"后续增加平台困难"的真实来源，而不是 if。

---

## 三、论点 3：服务端构建 / 前后端共用

### 3.1 已具备的部分

服务端**已经**把能力模型的键集和依赖关系发布出去了：

```
GET /admin/api/v1/provider-profiles
  capability_names:        []string            // internal/app/admin_provider_profiles.go:96,144
  capability_dependencies: map[string][]string // :101,145
```

而且前端有**双向** golden 校验（`web/src/pages/DeploymentsPage.test.tsx:1130`）：

- 服务端提供、前端没画 → 失败（"capabilities the server offers that no group draws"）
- 前端画了、服务端没有 → 失败（"capabilities drawn that the server does not offer"）
- 画在两个分组里 → 失败

所以"前端自己维护、和服务端脱节"这个担心，在**能力键集**这一层已经被挡住了。

### 3.2 仍然手工维护的三份

| 位置 | 内容 | 有没有守卫 |
|---|---|---|
| `web/src/types.ts:341` | 20 字段接口 | 无——纯手工镜像，加字段不会有任何测试失败 |
| `web/src/pages/DeploymentsPage.tsx:938` | 分组清单（含全部能力名） | 有（3.1 的双向测试） |
| `web/src/i18n/locales/{zh-CN,en-US}.ts` | 标签表 | 无 |

而且守卫是 **golden 快照**，靠 `HALRO_UPDATE_GOLDEN=1` 由 Go 测试重新生成——是构建期契约，不是运行时契约。服务端加一项能力，前端测试要等有人重新生成 fixture 才会红。

### 3.3 真正的裂缝：③④ 一个字节都没发给前端

```
$ grep -rn "UnsupportedGenerateFields\|BuiltinEndpointManifests" internal/app/*.go
（无输出）

$ grep -rn "unsupported_request_fields\|manifest" web/src --include="*.ts" --include="*.tsx"
（无输出）
```

`BuiltinEndpointManifests()` 在整个 `internal/app` 里**零引用**。清单只以仓库里一份静态 JSON 的形式存在，Admin API 不服务它，控制台不知道它存在。

于是：

- 部署表单能编辑 ①
- 路由用 ①+③ 决定放不放行
- 运营者只看得见 ①

**论点 3 应该改成**：不只是"能力模型服务端构建"，而是"**决定路由的那个完整模型要发布出去**"。只同步 ① 是治标——今天这条 bug 链里，① 从头到尾都是同步的。

---

## 四、案例：一次联调撞上的五种拒绝

这条链是上面三条结论最好的佐证。同一个目标（让模型认一张图），五次拒绝，五个不同的层，没有一次的提示指向真正的原因。

| # | 现象 | 真正原因 | 所在层 | 提示准不准 |
|---|---|---|---|---|
| 1 | `unsupported_feature` | 部署没声明 `vision` | ① 能力 | 不准——不说是哪一项 |
| 2 | `token_limit_exceeded` | base64 被按散文估算令牌（0.34×原图字节） | 令牌预估 | 不准——像是图太大 |
| 3 | `provider rejected the request` | Bedrock 不抓远程 URL | ③ 字段（当时**尚未声明**） | 不准——上游 code 被丢弃 |
| 4 | `unsupported_feature` | 同上，声明补上后正确拦截 | ③ 字段 | 不准——不说是哪个字段 |
| 5 | `capability_expansion_requires_revalidation` | 打开能力需重新验证，路由启用中 | 部署生命周期 | 不准——中文文案说"数据已被修改，请刷新" |

第 3 条尤其能说明问题：那次拒绝**来自上游**，因为 Halro 当时的模型里根本没有"能看图 / 能取图"的区分，所以它把一个必然失败的请求发了出去，付了一次往返，然后把上游的解释丢掉了。

本轮已修复的部分：#2 的令牌估算、#3 的 `provider_code` 日志、#3/#4 的字段声明、#4 的拒绝原因点名、#5 的中文文案。**但每一处都是补在各自那一层上的**，模型的断裂没有动。

---

## 五、建议

### P0 — 止血，不动架构

1. **把字段级限制发布给控制台** — **已实施（2026-08-23）**。

   落点与最初设想不同：没有新开端点，而是挂在控制台已经读的 `GET /admin/api/v1/provider-profiles` 上，每个 profile 多一个 `request_constraints`。理由是那个端点本来就是能力模型的发布口，它的注释写着"the list is the server's, the wording is the renderer's"——把缺的那一半补进同一个出口，比再开一个平行出口更符合它已有的契约。

   - `internal/compatibility/manifest.go` — `ProfileRequestConstraints(profileID)`，把已发布的 coverage 按 profile 而不是按端点重排
   - `internal/app/admin_provider_profiles.go` — `providerProfileView.RequestConstraints`
   - `web/src/pages/DeploymentsPage.tsx` — 能力勾选区下方列出该接口载不动的成员，按端点分组，附声明里的原因句
   - 效果：勾"视觉"的同时就能看到 `messages[].content[].image_url` 及其原因，#3/#4 两种拒绝在点保存之前可见

2. **拒绝原因贯通到用量记录** — **未做，需重新定级**。

   本文最初把它放进 P0 是判断失误：用量记录的字段属于账本事件，是持久化 schema，加一列要 bump 格式版本并重新初始化数据目录。这不是"止血、不动架构"，代价接近 P1。建议单独立项，并与 P1 的能力代数一起做——那时拒绝原因的取值域正好由新模型给出，不必先定义一套临时枚举再改。

3. **模型目录做厚 + 覆盖率可见** — **一半已存在，一半待做**。

   覆盖率其实已经可见：部署表单显示能力证据来源（`deployments.capabilityEvidenceSources`），`builtin_catalog` 与 `operator_declared` 是两个不同的标签，运营者看到的"管理员声明"就是后者。缺的是这个标签没有说清它的含义——看到"管理员声明"，不会想到"因为这个模型不在目录里"。这是文案，不是架构。

   真正待做的是把目录做厚：`gpt-5.4` 不在目录里（`internal/modelcatalog/builtin.go:165` 只有 `gpt-5/-mini/-nano`），运营者被迫 `operator_declared`，进而触发"重新验证 + 先离开路由"整套流程。目录未覆盖时，治理机制变成阻力而不是护栏。这需要逐个模型核对官方文档，按种子策略只写有出处的精确标识符——是一次资料工作，不是代码工作。

### P1 — 收敛表示（真正解决论点 1）— **第一步已实施（2026-08-23）**

原计划是把 ① 和 ② 合并成一套带"形态"的能力代数。实施时选了更小也更贴合本仓库既有做法的形状：**把形态表达成一项从属能力**，而不是新造一层类型。

依据是仓库自己的判例——`stream_usage` 依赖 `streaming`，本来就是"某能力的一种模式"用从属能力表示。于是：

```
fetched_image  依赖  vision
```

`vision` 说这个目标能读图，`fetched_image` 说它会去取一张请求只给了地址的图。**这两件事没有任何服务商当成一件**：Bedrock 只读请求里带的字节，OpenAI / Anthropic 直连 / DeepSeek 两样都做。

已落地：

- `internal/domain/models.go` — `ProviderCapabilities.FetchedImage`；`provider_table.go` — 各 Profile 上限按各家文档区分（Mantle 五个都没有）
- `internal/semantic/content.go` — `Content.Inline()`，"这张图是内联还是地址"从此只有一个答案；`request.go` — `Requirements.FetchedImage` 由请求推导
- `internal/gateway/service.go` — 需求↔能力的配对收敛成**一张表** `capabilityRequirements`，过滤器和"拒绝原因点名"共用它，不再是两份手写清单
- `internal/compatibility/` — Mantle 的三条 `image_url` 字段声明**删除**。同一个事实在三个端点用三种拼法各写一遍，本身就是它不属于字段层的证据
- `internal/store/bolt` — 迁移 31，schemaVersion 30 → 31

**不需要重新初始化数据目录**——本文原先那句判断是错的，仓库有编号迁移阶梯。迁移 31 照搬迁移 28（`provider_executed_tools`）的做法：把新能力在旧记录里记为 `unsupported`，不做回填。

**代价说清楚**：现有部署的 `fetched_image` 一律为关。一个今天靠远程 URL 工作的 OpenAI 部署，升级后要手工勾上这一项才能继续。不回填是因为回填必须知道每条记录的 Profile 上限才不会给 Bedrock 连接加上它做不到的能力，而一个跨 bucket 判断记录能声明什么的迁移，出错时是静默的。

**顺带证实了本文 §1.3 的判断**：实施过程中，同一份能力枚举在代码里被第 4、第 5 次发现——`admin_deployments.go` 的 `capabilitiesEnabledByName`、`admin_invocation_targets.go` 的同类 switch、以及 `models.go` 里逐字段 OR 的绑定汇总。每一处遗漏都表现为一次真实的功能故障（能力被判为 unsupported、连接汇总丢掉该项），全部由既有测试抓到。**加一项能力要动 23 个文件这个数字，是实测出来的，不是估算。**

**第二步也已实施（同日）**：①② 的命名统一了。`Requirements.InputImage` → `Vision`、`StructuredJSON` → `JSONMode`，JSON 名同步为 `vision` / `json_mode`。两侧现在共用能力字典的名字，一个读者不必先找到配对表才知道它们是同一件事。

统一之后 `Requirements` 的 13 个成员分成两类，没有第三类：

- **8 个与能力同名并配对**：tools、vision、fetched_image、json_mode、developer_role、reasoning、stream_usage、provider_executed_tools
- **5 个没有能力对应**：streaming（由 operation 过滤，不是能力）、parallel_tools / seed / multiple_candidates / end_user_reference（是字段级事实，由 ③ 逐 profile 声明）

而且加了 `TestEveryCapabilityShapedRequirementIsPaired`：反射遍历 `Requirements`，每个成员**要么**在配对表里，**要么**在一份写明理由的"故意不配对"清单里，二者必居其一；同时校验配对表引用的名字都在能力字典中，以及清单里没有已不存在的过期条目。

这把漂移的代价从"改一处就得改这张表"降到了"漏了就红"。§1.2 描述的那个裂缝——两套词表靠 7 行手写配对连接——到此闭合。

### P2 — 平台描述一等公民（解决论点 2）

一个平台一份描述（结构体或声明文件），注册时提供，把今天 6 处登记收敛成 1 处；清单 coverage 由描述**生成**而不是手写。

代价：大，是一次真正的重构。但它同时消掉"20 端点 × 65 条手写 coverage"这个组合爆炸，越晚做越贵——现在是 12 个 profile，第 13 个平台进来时是 13 × N。

### 排序建议

P0 立刻做（一天量级，直接改善运营体验）；P1 在下一个平台接入**之前**做（它决定了第 13 个平台的接入成本）；P2 可以在 P1 之后分批推进，因为 P1 已经把最痛的那一半解决了。

---

## 附：量化数据

| 指标 | 值 | 来源 |
|---|---|---|
| 能力模型的独立表示数 | 4 | §1.1 |
| `ProviderCapabilities` 的拷贝数（Go 3 + TS 1） | 4 | §1.3 |
| 手写逐字段转换函数 | 2 | `providers.go:800`、`admin_provider_profiles.go:47` |
| 加一项能力要改的文件数 | 23（前端 5） | `grep -rlE '(\.Vision\b\|Vision:\|"vision")'` |
| 加一个 profile 要登记的文件数 | 6 | §2.2 |
| 清单端点数 × coverage 条目数 | 20 × 65 | `endpoint-manifests.json` |
| `provider_fields.go` 的 profile 分支数 / 行数 | 11 / 329 | — |
| 请求可移植路径上的服务商分支数 | 0 | §2.1 |
| 服务端已发布给前端的能力元数据 | `capability_names`、`capability_dependencies` | `admin_provider_profiles.go:96,101` |
| 服务端发布给前端的**字段级**兼容信息 | 0 | §3.3 |
