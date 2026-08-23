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

> 上表是 2026-08-23 实施**之前**的判定，保留原样作为起点记录。实施之后的逐条复核见下一节
> 「完成度复核」——那一节的数字是对合并后的 main 重新测量的。

---

## 完成度复核（2026-08-23，实测）

问题被再次提出：三条论点是否全部修复、本文的任务是否全部完成。这一节是对**合并后的 main** 重新测量的结果，不引用实施过程中的中间数字。

结论先说：**论点 3 已关闭，论点 1 大部分关闭，论点 2 部分关闭。本文的建议清单有两项未做。**

| 论点 | 判定 |
|---|---|
| 1 全能力模型 | **大部分关闭** —— 能力本身收敛到一处，加一项能力的每个落点都有测试兜住；但"能力"与"字段"仍是两套表示 |
| 2 独立适配域 | **部分** —— 六处登记仍是六处，物理合并被包依赖方向挡住；但遗漏不再静默，五处由测试点名、一处运行时明确报错 |
| 3 服务端构建 | **已关闭** |

### 测量方法

不数 grep，直接做两个实验：往代码里真加一项能力、真加一个平台，然后看**编译和测试到底拦不拦得住、拦在哪里**。下面的表格是实验输出，不是估算。

### 实验一：加一项能力

往 `ProviderCapabilities` 加一个 `audio_input bool`，其余一概不动。

`go build ./...` **通过** —— 没有任何编译期强制。随后：

| 必须改的地方 | 由谁拦下 |
|---|---|
| `domain/models.go` 结构体成员 | （实验的起点） |
| `domain/capability_dictionary.go` 访问器表 | `TestTheDictionaryCoversEveryCapabilityMember` |
| `modelcatalog/catalog.go` 的 `CapabilityNames`（顺序独立，条目摘要依赖它） | `TestCapabilityNamesCoverTheDomainStruct` |
| 控制台 golden 快照 | `TestProviderProfilesGoldenMatchesConsoleFixture` |
| `web/src/types.ts` 与 `emptyCapabilities` | `useProviderProfiles.test.ts` |
| 两个 locale 的 `capabilities.*` | `i18n.test.tsx` |
| `DeploymentsPage.tsx` 的分组清单 | `DeploymentsPage.test.tsx` |

**六个落点，六个都有测试点名。** 前端那三道要等 golden 重新生成之后才会红——这不是缺口而是链条：服务端没同步时第一道就拦住了，压根不会广播这项能力。

对照本文原始测量的"加一项能力要动 23 个文件、其中两处靠测试失败才发现"：文件数降到六，而"靠测试失败才发现"从缺陷变成了**设计**——每一处都是被指名报出来的，报错信息带文件名。

### 实验二：加一个平台

往 profile 表加一行 `acme.chat.v1`，其余一概不动。`go build ./...` 同样通过，然后：

| 漏掉的登记 | 由谁拦下 |
|---|---|
| `provider/profile.go` 的 profile manifest | `TestCeilingWithinProfileManifestOperations` |
| `compatibility/provider_fields.go` 的字段规则 | `TestEveryProfileRegistersItsOwnFieldRules`（报错里直接写了该改哪个文件） |
| 端点清单 coverage | `TestEveryChatProfileAppearsInAnEndpointManifest` |
| `provider_table_test.go` 的端点期望 | `TestResolvedEndpointsMatchWhatTheConsoleOffered` |
| 控制台 golden | `TestProviderProfilesGoldenMatchesConsoleFixture` |
| `app/providers.go` 的适配器构造 | **没有测试**——运行时报 `provider profile is not implemented` |

六处里五处在测试期被点名，第六处在运行时明确报错。没有一处是静默的。

### 三条论点的逐条判定

**论点 1 —— 大部分关闭。**

关掉的：能力的**存储与命名**收敛到 `capability_dictionary.go` 一张表（19 项 + 2 个上限），拷贝从 4 份降到 2 份（Go 1 + TS 1，后者有守卫），手写逐字段转换器 3 → 0，按名字枚举的 switch/map 6 → 0；`semantic.Requirements`（13 个成员）与能力共用字典的名字，配对集中在一张表且漏配对会红。

没关掉的：**"能力"和"字段"仍是两套表示**。有能力对应的事实（vision、fetched_image、tools…）走 ①②；没有能力对应的事实（seed、n、stop、detail）走 ③ 的 8 组字段规则和 ④ 的 20 端点 × 65 条 coverage。一个读者要回答"这个平台支不支持 X"，仍然要知道 X 属于哪一类。

这不是疏漏而是边界问题：`seed` 不是一项能力，把它塞进能力字典会让"能力"这个词失去意义。真正的收敛要求 ③ 能从平台描述里生成，那是论点 2 的事。

**论点 2 —— 部分关闭。**

关掉的：遗漏不再静默（见实验二）。

没关掉的：六处登记仍是六处。本文原本设想的"一份平台描述"**做不到**，原因是包依赖方向——能力上限在 `domain`，Operation→Primitive 绑定在 `provider`，字段规则是对 `semantic.GenerateRequest` 的闭包在 `compatibility`，适配器构造在 `app`。一份装得下这四样的结构体必须被这四个包都导入，同时又引用它们的类型，是环。要做得新开顶层包并把 `semantic`/`provider` 的类型上移，或走代码生成。

清单 coverage 仍是 65 条手写。

**论点 3 —— 已关闭。**

服务端发布能力键集、依赖关系、opt-in 警告、以及字段级请求约束；前端的三份镜像各有双向守卫，实验一里三道全部如实报红。守卫在 CI 期而非运行时，但链条闭合：服务端改了不同步 golden，Go 测试红；同步了 golden 而前端没跟上，前端三个测试红。

### 本文建议清单的最终状态

| 项 | 状态 |
|---|---|
| P0-1 发布字段级限制给控制台 | **已实施** |
| P0-2 拒绝原因入用量记录 | **已改做，形式与原描述不同**。原描述有两处错：一是把它当成"给用量记录加一列"，二是据此定级为持久化 schema 变更。实测下来，路由能力拒绝发生在 `beginRequestRun` 之前（`service.go:916` vs `:945`），而第一次写账本在 `BeginRequestDetailed`——**根本没有一条记录可以加列**。要让它进账本，就得为从未开始的请求造记录：这会改变账本里"一次请求"的含义，并给任何持有有效 Key 的调用方一条廉价的写放大路径。真正的需求是"运营者事后能看到路由为什么拒"，已由 `route_capability` 计数器满足——和 RPM/TPM/预算/Token Guard 同一套机制，出现在仪表盘的拒绝构成与 `halro_policy_rejections_total` 上。逐请求的原因仍在响应与日志里 |
| P0-3 模型目录做厚 + 覆盖率可见 | **已实施**。覆盖率那一半原就有（能力证据来源标签区分 `builtin_catalog` 与 `operator_declared`）；做厚那一半于 2026-08-23 完成，逐个核对官方文档后补入 OpenAI 5.4/5.5/5.6 三代、Anthropic 5 代、以及**此前完全没有覆盖的 Bedrock Mantle**。过程中修正了 `claude-sonnet-4-6` 的最大输出（64k → 文档所载的 128k）|
| P1 收敛表示 | **两步都已实施** |
| P2 第一阶段 能力字典收敛 | **已实施** |
| P2 第二阶段 登记完整性 | **已实施**（物理合并不可行，改为让遗漏发声） |
| P2 第三阶段 清单与字段规则对齐 | **已实施，形式与原描述不同**。原描述说"由描述生成"，且判断它必须先解开包依赖环——两点都不成立。coverage 需要的字段规则和清单在**同一个包**里，无环可解；而**整体生成做不到**，因为清单不是字段规则的纯函数：Anthropic 的 `top_k`、`thinking`、`metadata`、`service_tier` 在可移植投影里就被丢掉了，任何规则都看不到它们，清单合理地说得比规则能推导的多。做得到也该做的是**包含性守卫**：规则拒绝的、且该端点建模了的字段，清单必须声明。它上线时立刻查出四处真实缺口（详见下）|

建议清单全部有了着落：P0-1、P1、P2 第一/二/三阶段已实施；P0-2 与 P2 第三阶段以与原描述不同的形式实施，理由各自写在上表；P0-3 已补齐。

### "一份平台描述"：验证后的结论是不做（2026-08-23）

这是本文剩下的最后一条。按依赖图核实之后，结论是**它不该做**，而不是"代价太大暂缓"。

```
domain         无内部依赖（叶子）
semantic       无内部依赖（叶子）
compatibility  → domain, semantic
provider       → compatibility, domain, semantic
```

能同时装下 primitive 绑定和字段规则的最低层是 `provider`。但 profile 表必须留在
`domain`——它携带能力上限，而 `ProviderProfileBinding.Validate` 要用这个上限在任何其他
逻辑之前校验一条已存储的记录。表移上去，domain 就失去上限；上限留在原处，就是复制一份。
**两者都比六处登记更糟**，所以分层保持不变，改为让登记"少"变成让遗漏"响"。

真正还缺的不是合并，是**可发现性**：新人不知道有这六处。这由
`docs/contracts/adding-a-platform.md` 解决——六步各自写明该改哪个文件、跳过它会由哪个
测试报错。文档本身由 `TestTheChecklistNamesGuardsThatExist` 守住：它遍历仓库里所有测试
函数名，文档引用了一个不存在的守卫就红。地图会过期，这一条防的正是过期。

至此本文三条论点与全部建议都有了着落。仍然为真的一句话是：六处登记仍是六处，而且没有一处
是静默的。

### 什么时候该做剩下的

P2 第三阶段的收益随平台数量线性增长，而代价是一次性的。现在是 12 个 profile；如果下一个接入平台已经在计划里，先做它比先接平台便宜。如果没有，它就是一笔可以等的债——而且现在这笔债**不会静默扩大**，因为每一处遗漏都会报出来。

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
