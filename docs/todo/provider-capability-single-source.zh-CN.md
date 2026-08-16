# 适用能力改由服务端统一下发（设计提案）

状态：**已实施**（分支 `feat/provider-capability-single-source`，5 个提交）。
第 1–5 步全部落地，§3.3 的服务端拆分**仍未做**——见文末「实施记录」。
建立日期：2026-08-16
修订日期：2026-08-17（第二稿：权威数据的存储形态定为 Go 声明式表；Base URL / region 进 `config.yaml`）
　　　　　2026-08-16（第一稿评审：后端 / 前端 / 安全 / API 契约 / 操作员体验 / 事实核查 六角色）
范围：`internal/domain`、`internal/config`、`internal/app`（新增只读 Admin 端点）、
`web/src/pages/ProvidersPage.tsx`、`web/src/pages/DeploymentsPage.tsx`、`web/src/types.ts`、`web/src/api.ts`
相关：`internal/app/admin_providers.go` 的能力上限校验、[endpoint-manifests](../compatibility/endpoint-manifests.json)

> **修订摘要**
>
> **第二稿（2026-08-17）** — 回答了一个第一稿没问的问题：权威那一份该以什么形态存在。
> 结论是**把散在六处的 switch 重构成一张 Go 声明式表**（§3.0），而不是第一稿的「导出几个
> 枚举函数」，也不是候选过的 YAML 配置文件（§4.3 记下了取舍）。同时把 Base URL 与 region
> 从「domain 新增常量」改为**URL 模板在表里、region 在 `config.yaml`**（§3.1 注 A）。
>
> **第一稿评审（2026-08-16）** — 初稿有四处照原文实施会出错：§5 并列的两个修法其中之一
> 在数值上限字段上语义错误；默认 Base URL 被当成「服务端已有数据」而它只存在于前端；
> §3.2 必做步骤与 §3.3 可选步骤互相矛盾；domain 没有可枚举的 Profile 清单，端点组装层
> 必然成为第三份副本。另有三处事实不实：前端镜像不止 `ProvidersPage.tsx` 一个文件，
> Bedrock 是 8 个 Profile 不是 7 个，示例里的 `access_surface` 值写错。逐条修正，并把
> 「可选」的第 4 步改为必做。

---

## 1. 问题：同一份事实，多处硬编码

「服务商连接」表单里的适用能力棋盘（默认勾选哪些、哪些可勾、哪些置灰）目前由前端
自己算，后端保存时再用自己的一份重新校验。两边各自硬编码，靠注释里一句
"Mirrors domain.MaxProviderCapabilitiesForProfile" 提醒人工同步。

**后端权威版本**（保存时强制执行）：

| 内容 | 位置 |
| --- | --- |
| 能力名清单（18 项） | `internal/domain/provider_profile.go:59`（未导出） |
| 各类型默认能力 | `internal/domain/models.go:592`（`DefaultProviderCapabilities`） |
| 各 Profile 默认能力 | `internal/domain/models.go:624`（`DefaultProviderCapabilitiesForProfile`） |
| 各 Profile 能力上限 | `internal/domain/models.go:667`（`MaxProviderCapabilitiesForProfile`） |
| Profile → Surface / 凭据方案 | `internal/domain/provider_profile.go:93`（`RegisteredProviderProfile`，只能按 ID 点查） |
| 全部 Profile 的唯一全量清单 | `internal/domain/provider_profile.go:130-135`（`ResolveCredentialProfile` 函数体内的内联切片） |
| 能力依赖规则（streaming 需 chat 等） | `internal/domain/models.go:342`、`:515`、`:955-958` |
| 数值上限的子集语义 | `internal/domain/provider_profile.go:324`（`capabilityLimitSubset`，`0` 表示不限） |
| 不可变 Profile 判定 | `domain.IsImmutableCapabilityProfile` |
| 默认 Base URL | **不存在**——服务端从未持有这份数据（见 §3.1 注 A） |
| Bedrock region | **不存在**——只在前端硬编码为 `us-east-1`（见 §3.1 注 A） |

这九行分散在六个函数里，彼此靠人脑关联：`DefaultProviderCapabilities`（按类型 switch）、
`DefaultProviderCapabilitiesForProfile`（按 Profile switch）、`MaxProviderCapabilitiesForProfile`
（默认值加一个特例）、`RegisteredProviderProfile`（switch）、`ResolveCredentialProfile`
（内联切片）、`Validate` 的类型 switch。**没有任何一处能把这张矩阵当成矩阵读出来**——这是
§3.0 要一并解决的问题。

**前端镜像版本**（逐类型 `if/else` 硬编码，分布在三个文件）：

`web/src/pages/ProvidersPage.tsx`：

| 内容 | 位置 |
| --- | --- |
| 能力名清单 + 展示顺序 | `:821`（`capabilityNames`） |
| 默认能力 | `:851`（`defaultProviderCapabilities`，含 Titan Embed 的 `max_context_tokens: 8192`，`:875`） |
| 能力上限 | `:886`（`maxProviderCapabilities`） |
| 能力依赖联动 | `:826`（`updateCapabilitySelection` 的 `chatFeatures` 清单，`:828`） |
| 不可变 Profile 清单 | `:837`（`isStrictCapabilityProfile`） |
| Bedrock Profile 清单 | `:75`（`bedrockProfiles`） |
| Bedrock Surface/凭据/默认 URL | `:91`（`bedrockProfileConfig`）、`:101`（`bedrockCredentialConfig`） |
| 各类型默认 Base URL | `:38`（`defaultBaseURL`） |
| OpenAI 双 Profile 拆分规则 | `:88`、`:895`、`:968`（`openAIChatProfile`/`openAIMediaProfile`、两个能力集合、`openAIBindings`） |
| 其他 domain 常量镜像 | `:914` `normalizeBedrockProjectID`、`:921` `maxBedrockProjectIDLength`、`:958` `maxAnthropicBetaTokens`/`maxAnthropicBetaTokenLength` |

`web/src/pages/DeploymentsPage.tsx`：

| 内容 | 位置 |
| --- | --- |
| 能力名清单（第三份，与 domain `:59` 逐项相同） | `:697`（`deploymentCapabilityNames`） |
| 能力展示分组 | `:700-704`（`deploymentCapabilityGroups`） |

`web/src/types.ts`：`:341` 的 `ProviderCapabilities` 接口把全部能力键再枚举一遍（类型层，无法避免，但同样需人工同步）。

每次后端调整任何一个 Profile 的能力集，都必须记得同步改上述位置，否则：

- 前端比后端宽 → 操作员勾了能力、保存被 400 拒绝，且错误信息不指认是哪一项；
- 前端比后端窄 → 能力明明支持却勾不上，功能静默丢失。**后端新增一个能力时，`DeploymentsPage.tsx:697`
  不改，部署表单的能力棋盘就永远不会出现它**（该清单还用于过滤服务端返回的 ceiling）。

这不是理论风险。2026-08-16 排查的「provider capabilities exceed what this profile can
serve」故障就是两份世界模型分叉的直接产物：前端按「多 binding、顶层能力是并集」的
模型提交（`ProvidersPage.tsx:630`），后端 `46fcfe6` 收紧校验时按「单 Profile」的模型
比对（`admin_providers.go:1331`），**OpenAI 类型打开表单什么都不改、直接保存默认值即必现**
（默认能力含六项媒体能力，`ProvidersPage.tsx:860`）。两份逻辑没有同一个来源，就没有
机制阻止它们再次分叉。

反例证明统一是可行的：部署表单的能力**上限**已经不硬编码——由服务端返回的 provider
bindings 与模型目录求交集推导（`DeploymentsPage.tsx:985-995`），后端改了前端自动跟上。
本提案就是把连接表单也拉到同一模式（注意：部署表单的能力**名清单**仍是硬编码，见上表）。

## 2. 目标与非目标

**目标**

- 能力名清单、能力依赖规则、各 Profile 的默认/上限/不可变标记、Surface 与凭据方案、
  Base URL 模板，只在 `internal/domain` 存在一份权威，且**以一张可以当矩阵读的声明式表
  存在**（§3.0）；前端只渲染，不再自己算。
- 因部署而异的量（Bedrock region、可选的 Base URL 覆盖）进 `config.yaml`，与能力分开
  （§3.1 注 A）。
- 按 pre-1.0「原地修正」原则：前端镜像函数**删除**，不保留为 fallback。
- 顺手消灭「顶层并集 vs 单 Profile 上限」这一类校验分叉（见 §5 前置修复）。

**非目标**

- 不改变能力语义、任何 Profile 的能力集合、Gemini/Bedrock Beta 的钉死上限。
- 不改变部署表单的**上限推导**逻辑（它已是目标模式）；但其**能力名清单与分组**
  （`DeploymentsPage.tsx:697,700-704`）纳入本提案范围——否则 §2 第一条目标达不到。
- 能力的显示文案（`capabilities.*` i18n 键）留在前端——服务端只下发稳定的能力键名，
  翻译本来就是前端资产；键与文案的同步由 CI 保证（见 §3.2 注 C），不做运行时兜底。
- 能力的**展示顺序**留在前端（见 §3.1 注 D）。
- `normalizeBedrockProjectID`、`maxBedrockProjectIDLength`、`maxAnthropicBetaTokens`
  等校验常量镜像（`ProvidersPage.tsx:914-960`）**不在本次范围**：它们是标量校验规则，
  与能力矩阵不同源、变更频率极低。此处显式声明排除，避免「单一来源」被读作已全覆盖；
  若日后要收，走同一端点加 `limits` 字段即可。
- 模型目录（`/admin/api/v1/model-catalog`）是另一层（具体模型的能力），不在本文范围。

## 3. 方案：新增只读元数据端点

### 3.0 前置：把矩阵重构为一张 Go 声明式表

**这是端点能成立的前提，不是实现细节。** 今天 `RegisteredProviderProfile`
（`provider_profile.go:93`）只能按单个 ID 点查，全部 Profile 的唯一全量清单是
`ResolveCredentialProfile` 函数体内的内联切片（`:130-135`），provider type 集合只存在于
`Validate` 的 switch（`models.go:472-473`），能力名清单 `capabilityNames`（`:59`）与
name→字段映射 `capabilityEnabled`（`:179-182`）均未导出。

若端点组装层在 `internal/app` 自写一份「type→profiles」列表，**它就是新的第三份副本**，
且新增 Profile 时忘改它没有任何机制能发现。

第一稿的解法是「导出几个枚举函数」。本稿改为更彻底的一步：**把散在六处的 switch 收敛成
一张声明式表**，那六个函数退化为对它的查表。表本身就是枚举，端点直接序列化它，
「端点不是第三份副本」这句话由此在结构上成立，而不是靠一条测试去维持。

```go
// internal/domain/provider_table.go
type profileRow struct {
    Type           ProviderType
    Surface        AccessSurface
    Scheme         CredentialScheme
    BaseURLTemplate string      // 见 §3.1 注 A；region 之类的替换量来自 config
    Immutable      bool
    Defaults       ProviderCapabilities
    Ceiling        ProviderCapabilities
}

var profileTable = map[ProviderProfileID]profileRow{
    ProfileOpenAIChatEmbeddings: {
        Type: ProviderOpenAI, Surface: SurfaceOpenAI, Scheme: CredentialBearerStatic,
        BaseURLTemplate: "https://api.openai.com",
        Defaults: ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, /* … */},
        Ceiling:  ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, /* … */},
    },
    ProfileBedrockConverseText: {
        Type: ProviderBedrock, Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
        BaseURLTemplate: "https://bedrock-runtime.{region}.amazonaws.com",
        Defaults: ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true},
        Ceiling:  ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true},
    },
    // … 15 行，一个 Profile 一行
}
```

配套导出：`AllProviderProfiles()`（遍历表，按 type 分组）、`CapabilityNames()`、能力键→字段
的读写映射；`ResolveCredentialProfile`、`RegisteredProviderProfile`、
`DefaultProviderCapabilitiesForProfile`、`MaxProviderCapabilitiesForProfile` 全部改为查表，
内联切片与各处 switch 删除。

**为什么是 Go 表而不是 YAML 文件**：见 §4.3。一句话——能力上限是关于「这个二进制里编译了
哪些 Primitive」的断言，它应该和那些 Primitive 待在同一种语言里，并保留编译期类型检查。

**这一步顺带补上一个今天就缺的不变量。** `ProfileManifest.Validate`（`internal/provider/profile.go:23`）
强制每个声明的 Operation 都有编译好的 `PrimitiveBinding`，而能力上限不得超出这些 Primitive
覆盖的操作集——但**今天没有任何测试把两者对起来**（只有 `capability_detection_test.go:169-181`
在 Bedrock 的检测计划里顺带同时取了 manifest 与 ceiling，不是不变量断言）。表落地后加一条
`TestCeilingWithinProfileManifestOperations`：遍历 `profileTable`，用 `BuiltinProfile(id)` 取
manifest，断言 ceiling 声明的每一项能力都能落到该 manifest 的某个 Operation 上。

不变量测试一律**遍历表**逐项比对端点输出，而不是遍历端点输出去查表——后者结构上发现不了
「表里新增而端点漏列」。

### 3.1 端点

```
GET /admin/api/v1/provider-profiles        （requireAdmin；与现有 GET /providers 同门槛，
                                             read_only 角色的会话可访问）
```

内容是编译期常量（`profileTable`）加一个启动期配置量（region），进程启动时组装一次并
缓存于内存；无存储访问，因此除认证与网络层外无失败源。**不做 HTTP 缓存协商**（注 B）。
响应形如：

```jsonc
{
  // 布尔能力键；不承载展示顺序（注 D）
  "capability_names": ["chat", "streaming", "embeddings", /* … 共 18 项 */],
  // 能力依赖：键依赖 chat，取消 chat 时须一并关闭（注 E）
  "capability_requires_chat": ["streaming", "tools", "vision", "json_mode",
                               "developer_role", "reasoning", "stream_usage",
                               "provider_executed_tools"],
  "provider_types": [
    {
      "type": "openai",
      "default_profile_id": "openai.chat-embeddings.v1",
      "profiles": [
        {
          "id": "openai.chat-embeddings.v1",
          "access_surface": "openai-api",          // 真实值，非 "openai"
          "credential_scheme": "bearer.static",
          // 挂在 profile 层级；已是解析后的最终值（Bedrock 的 {region} 已替换），
          // 前端不需要知道模板的存在（注 A）
          "default_base_url": "https://api.openai.com",
          "immutable": false,
          // 完整的 ProviderCapabilities 序列化，含 max_context_tokens /
          // max_output_tokens 两个数值字段（0 表示不限）
          "defaults":  { "chat": true, /* … */ "max_context_tokens": 0, "max_output_tokens": 0 },
          "ceiling":   { "chat": true, /* … */ "max_context_tokens": 0, "max_output_tokens": 0 }
        },
        {
          "id": "openai.media-resources.v1",
          "access_surface": "openai-api",
          "credential_scheme": "bearer.static",
          "default_base_url": "https://api.openai.com",
          "immutable": true,
          "defaults": { "moderations": true, "images": true, /* … */ },
          "ceiling":  { /* 同 defaults */ }
        }
      ]
    }
    // anthropic / azure_openai / deepseek / openai_compatible / gemini / bedrock
    // 共 7 个类型、15 个 Profile（bedrock 名下 8 个）
  ]
}
```

要点与四条注：

- `defaults` / `ceiling` 是 `DefaultProviderCapabilitiesForProfile` 与
  `MaxProviderCapabilitiesForProfile` 返回值的**完整 JSON 序列化**（`ProviderCapabilities`
  无 `omitempty`，数值字段天然包含），不是重新誊写的布尔表。矩阵的**值**由此不产生副本；
  矩阵的**行**由 §3.0 的枚举保证。
- **可勾 Profile 组合受约束**：服务端要求每个 binding 的 Surface 与凭据方案与连接凭据
  一致（`admin_providers.go:1352-1355`），即只有**同 Surface + 同凭据方案**的 Profile 才能
  组合进一个连接（Bedrock 的 runtime 四件套可组合，但不能与 mantle 或 agent-runtime 混配）。
  该约束由端点下发的 `access_surface`/`credential_scheme` 直接推出，前端不再自写。
- 连接级可勾上限 = **该连接所用 Profile 组合的 ceiling 并集**。对 OpenAI 是「该类型全部
  Profile 的并集」（操作员看不到 Profile 选择）；对 Bedrock 是「所选『能力实现』的 ceiling」
  （操作员可见的选择器沿用既有命名「能力实现」，`ProvidersPage.tsx:713`，不引入
  Profile/binding/ceiling 这类实现术语）。

> **注 A — Base URL 是新增的权威数据，且要拆成「模板」与「部署量」两半。**
>
> 它今天**只存在于前端**（`ProvidersPage.tsx:38-44` 的 `defaultBaseURL`、`:91-99` 的
> `bedrockProfileConfig`），`internal/domain` 与 `internal/app` 的非测试代码里没有任何
> `api.openai.com` 之类的字符串。对这一项，本提案是「先建立权威，再统一」，而非「把已有的
> 一份搬出来」。
>
> 它**必须挂在 profile 层级而非 type 层级**：Bedrock 的 URL 随 Surface 变化
> （`bedrock-runtime` / `bedrock-agent-runtime` 各自的 amazonaws.com 域名、`bedrock-mantle`
> 的 api.aws 域名），type 级单字段替换不了 `bedrockProfileConfig`。
>
> **拆分规则**——两者性质不同，不放在一起：
>
> - **URL 模板**（`https://api.openai.com`、`https://bedrock-runtime.{region}.amazonaws.com`）
>   是厂商事实，与能力同源，进 §3.0 的表。
> - **region** 是部署选择，不是厂商事实。今天前端把它硬编码成 `us-east-1`
>   （`ProvidersPage.tsx:92-98`），一个跑在别的区域的操作员每次新建连接都要手改 URL。
>   它进 `config.yaml`，与 `server`、`storage` 并列：
>
>   ```yaml
>   providers:
>     bedrock:
>       default_region: us-east-1     # 省略即保持现值，不改变现有行为
>   ```
>
> 端点下发的是**解析后的最终值**（模板里的 `{region}` 已替换），前端拿到的仍是一个可直接
> 填进表单的 URL，不需要知道模板的存在。
>
> **安全面很小，但要说清楚为什么小**：SafeTransport 的主机允许列表是从**已保存的连接**
> 推导的（`admin_providers.go:1315` 取 `endpoint.Hostname()`），不是从默认值来的；而操作员
> 今天本来就能在表单里输入任意 URL。配置化改变的只是**表单预填值**，不放宽任何出站边界。
>
> **实现注意**：`config.Config` 的 `Version` 走精确相等校验（`internal/config/config.go:623`，
> `SchemaVersion = 1`）。新增的 `providers:` 段必须是可省略且零值即现有行为的，这样
> `version` 保持 1、既有 `config.yaml` 不需要任何改动即可继续启动。
> `internal/config/default.yaml`（写给操作员的带注释模板）同步加上该段并附说明，
> `TestDefaultTemplateMatchesDefault` 会强制它与 `Default()` 保持一致。

> **注 B — 不做 HTTP 缓存协商（初稿的「可带 ETag」已删除）。**
> 仓库现有的 ETag 全部是 `revisionETag(revision)` 形态、配合 `If-Match` 做乐观并发控制
> （如 `admin_deployments.go:183`），`internal/app` 里没有任何 `If-None-Match` / 304 处理；
> 且 `adminSecurityHeaders` 对全部 Admin 响应设 `Cache-Control: no-store`
> （`runtime.go:1478`），`no-store` 下浏览器根本不会发条件请求。为它开例外等于在 Admin 面
> 上破坏「一律 no-store」这条无需逐端点判断的简单规则，而收益近乎为零——响应是几 KB
> 的编译期常量，且前端 `staleTime: Infinity` 一个会话只取一次。

> **注 C — 见 §3.2。**

> **注 D — 展示顺序不由端点决定。**
> domain 的清单顺序（`provider_profile.go:59-62`：chat, streaming, embeddings, tools, vision,
> …, provider_executed_tools, moderations, images, …）与前端现有展示顺序
> （`ProvidersPage.tsx:821-824`：chat, streaming, embeddings, moderations, images, …, tools, …）
> **不同**。若规定「数组顺序即展示顺序」，照抄会静默改变控制台的徽章与棋盘排列。
> 结论：`capability_names` 只是键集合，顺序不承载语义；展示顺序与 i18n 文案同属前端资产。

> **注 E — 能力依赖必须下发。**
> 「哪些能力依赖 chat」今天前后端各有一份（前端 `ProvidersPage.tsx:828` 的 `chatFeatures`，
> 后端 `models.go:342`、`:515`、`:955-958`），初稿把它整个漏了。不下发的话，后端新增一个
> 依赖 chat 的能力时前端联动静默漂移，操作员勾出的组合会被保存时 400 拒绝——正是本提案
> 要消灭的故障形态。

### 3.2 前端改造

- `web/src/api.ts` 增加 `getProviderProfiles()`；TanStack Query 拉取（`staleTime: Infinity`）。
- **删除清单**（§1 表格里的全部能力矩阵镜像）：`ProvidersPage.tsx` 的 `capabilityNames`
  (`:821`)、`updateCapabilitySelection` 的 `chatFeatures` (`:828`)、`defaultProviderCapabilities`
  (`:851`)、`maxProviderCapabilities` (`:886`)、`isStrictCapabilityProfile` (`:837`)、
  `bedrockProfiles` (`:75`)、`bedrockProfileConfig` (`:91`)、`bedrockCredentialConfig` (`:101`)、
  **`defaultBaseURL` (`:38`)**、`openAIChatCapabilities`/`openAIMediaCapabilities` (`:895-901`)；
  以及 `DeploymentsPage.tsx` 的 `deploymentCapabilityNames` (`:697`) 与
  `deploymentCapabilityGroups` (`:700-704`)。
- **凭据表单一并纳入本步**（初稿把它列为开放问题，实际没有选择余地）：`bedrockCredentialConfig`
  的三条分支全部调用 `bedrockProfileConfig`（`:102-104`），后者删除后前者无法编译；且
  `CredentialForm` 在 `:428`、`:450`、`:500`、`:511` 消费 `defaultBaseURL` 与
  `bedrockCredentialConfig`，同样需要元数据。
- **fail-closed 的门设在表单挂载，不是提交**：`ProviderForm` 的初始 state 在挂载时就消费
  镜像（`capabilities` 初值 `:581`、`baseURL` 初值 `:575`、初始 profile `:573-574`；
  `CredentialForm` 同理 `:428`）。React `useState` 初始化器只跑一次，元数据晚到无法回填。
  正确形态是「元数据未就绪不渲染表单」，仓库已有现成模式（`:219` 以 `credentials.isSuccess`
  为条件才挂 `ProviderForm`；`:681-686` 凭据为空时用 notice 替换整个表单）。
  失败态复用页面既有 `ErrorState` 承载原因与重试，**不用禁用按钮沉默拒绝**——那是本文件
  明确反对的形态（`:811-813` 的注释）。
- **展示路径不被 fail-closed 波及**：服务商列表的能力徽章与计数今天经 `enabledCapabilities`
  (`:413-414`) 消费 `capabilityNames`。列表应改为遍历 `provider.capabilities` 自身的键，
  端点故障时列表仍可用——否则相对今天是回归。fail-closed 严格限定在**表单**。

> **注 C — i18n 键与文案的同步放在 CI，不做运行时兜底。**
> 初稿写的「遇到无翻译的新键显示原始键名兜底」有两个问题：其一与 §2 非目标里
> 「控制台与后端同一二进制出厂，不存在键名超前于文案」自相矛盾（若断言成立，兜底是死代码）；
> 其二它会把 `provider_executed_tools` 这类 snake_case 实现名摆到操作员面前，违反本仓库
> 「界面语言面向操作员的问题、不用实现术语」的偏好；其三实现上它也不是免费的——
> `web/src/i18n/index.ts:65-72` 没有 `parseMissingKeyHandler`，i18next 缺键时返回的是带前缀的
> **完整键名**（`"capabilities.new_cap"`），要显示裸名得显式写 `defaultValue`。
> 正确层次是测试期保证：`web/src/i18n/i18n.test.tsx` 已有键审计机制，§6 的不变量测试加一条
> 「端点 `capability_names` 每项在 zh-CN 与 en-US 都有 `capabilities.*` 文案」即可。

### 3.3 提交语义收敛（必做，可后置为独立 PR）

**初稿把这一步标为「可选」，评审判定这会让一个双轨中间态无限期存在，与 pre-1.0 原则
冲突。本稿改为必做。**

彻底的收敛是把「能力如何拆成 bindings」也收回服务端：前端只提交**类型 + Profile 选择 +
一份扁平能力集**，`openAIBindings`（`ProvidersPage.tsx:968`）那样的拆分规则由
`providerFromInput` 按 Profile 归属自行分解。这样 §5 那类分叉在结构上不可能再出现——
前端从头到尾不知道 binding 的存在。

三条必须在契约评审中钉死的规则：

1. **无归属能力必须 400 拒绝，不得静默丢弃。** 扁平能力集中落不进任何所选 Profile
   ceiling 的能力，最自然的实现（`flat ∩ ceiling`）恰恰是静默丢弃，而 CLAUDE.md 的能力
   过滤不变量是 "unsupported fields are rejected, never silently dropped"。
2. **ceiling 重叠时的拆分歧义要有定论。** OpenAI 双 Profile 的 ceiling 不相交
   （`models.go:592-599` vs `:626-627`），拆分确定；但 `ProfileBedrockMantleOpenAIChat`
   （`:637`）与 `ProfileBedrockMantleOpenAIResponses`（`:641`）的 ceiling 仅差 `reasoning`
   一项。现行 API 允许每个 binding 单独声明能力（`admin_providers.go:1339-1370`，
   `admin_providers_test.go:97-100`），扁平提交 + 交集分解会把重叠能力同时点亮到两个
   binding，操作员失去「responses binding 不开 chat」这类粒度。仍受 ceiling 约束
   （`models.go:357`）故不构成 fail-open，但属请求契约的实义收窄，须作为验收条件写明。
3. **终局是顶层 `capabilities` 与 `bindings` 不再并存**：要么前端不再发送顶层字段，要么
   服务端在 bindings 存在时**拒收**它。不可停在 §5 修复后的「接受但静默忽略」状态
   （见 §5 的契约说明）。

### 3.4 关于 §3.2 与 §3.3 之间的过渡态（评审新增）

§3.2 删除 `openAIChatCapabilities`/`openAIMediaCapabilities`，而 OpenAI 的提交路径
（`ProvidersPage.tsx:630` → `:968-975`）在 §3.3 落地前仍需要一份拆分依据。**这条依据无法
从端点 ceiling 推导**：`max_context_tokens`/`max_output_tokens` 归 chat binding、media
binding 置 0 这条规则，在两个 OpenAI Profile 的数值上限都是 0（"不限"）时无从区分。

因此二选一，**本提案取后者**：

- ~~在第 3 步保留一份拆分规则镜像，等第 4 步删除~~（承认一处镜像过夜，违背 §2）；
- **✅ §3.3 与 §3.2 同期落地**，即第 3、4 步合并为一个 PR。前端一次性切换到「扁平能力
  提交」，拆分规则从此只在服务端存在，不存在过渡期镜像。

这也是把 §3.3 从「可选」改为「必做」的直接原因：它不只是终局的洁癖，它是第 3 步能够
干净完成的前提。

## 4. 被考虑并放弃的替代方案

### 4.1 构建期代码生成（Go → TS）

`go generate` 从 domain 吐一份 TS 常量，无运行时请求。防漂移效果相同（控制台与后端同
二进制出厂，本就锁版本），但要引入并维护一条生成管线，且产物仍是「第二份文件」——
审阅 diff 时它与手写镜像难以区分。运行时端点无新构建机制，还能顺带服务未来的 API
消费者（脚本、Terraform 类工具）。

### 4.2 把矩阵写进 `docs/compatibility/endpoint-manifests.json` 让前端 import

该清单面向的是北向端点兼容性，混入 Admin 表单元数据会让两个受众互相牵制；且它仍是
构建期快照，不解决「一份来源」以外的任何问题。

### 4.3 把矩阵写进 YAML 配置文件，服务端读取后经端点下发

**这是本提案最接近被采纳的替代方案**，值得完整记录取舍——它与 §3.0 的 Go 声明式表只
差「用什么语言写这张表」，其余（端点下发、前端只渲染）完全相同。

先分清两个变体，它们的结论截然不同：

**变体 B：操作员可编辑的 YAML（放在 `config.yaml` 或旁挂文件）——否决。** 三条理由：

1. **配置文件写不出 Primitive。** 每个 Profile 的清单把它声明的每个 Operation 绑定到一个
   编译进二进制的 Go 函数，`ProfileManifest.Validate`（`internal/provider/profile.go:23`）
   强制两者数量一致。YAML 里声明「Gemini 支持工具调用」得到的不是这个能力，而是路由把
   请求分给一个只会返回错误的连接——正是 `admin_providers.go:1326-1330` 说明的、上限存在
   的全部理由。
2. **它把契约评审降级成文本编辑。** CLAUDE.md 明确「Gemini/Bedrock Beta 的能力上限是刻意
   钉死的，放宽需要 deliberate contract review」。`internal/domain/capability_ceiling_test.go`
   的存在本身就是证据：它是因为 Mantle 三个 Profile 曾漏出不可变清单、上限一度无人强制
   才补上的。
3. **`provider_executed_tools` 是实打实的出网边界。** 开启即接受不经过 SafeTransport 的上游
   出网（`models.go:656-663`）。这一项的上限不该由编辑一个文本文件来放开。

**变体 A：`go:embed` 的 YAML（随二进制出厂，改了要重新编译）——不否决，但输给 Go 表。**
信任边界与 Go 常量完全相同，所以这纯粹是工程取舍。它的好处（可枚举、能当矩阵读、diff
是一行）**Go 声明式表全都有**，而它额外要付三笔代价：

1. **能力上限与 Primitive 的耦合会跨语言。** 上限不得超出 manifest 已绑定 Primitive 的操作
   集；两边都是 Go 时，这条不变量测试是几行的事（§3.0 已列入）。分处两种格式后，同一条
   检查要跨解析边界写，且没有编译器帮忙。
2. **丢掉编译期类型检查。** 今天给 `ProviderCapabilities` 加字段，所有构造点都会编译报错；
   YAML 里一个拼错的键（`json_mod`）会静默变成 `false`。可用严格解码兜住，但那是新欠的、
   必须记得还的债。
3. **它在本仓库是个新先例。** 全仓库只有两处 `go:embed`：`internal/webui/dist`（前端产物）
   与 `internal/config/default.yaml`——而后者恰恰**不是权威**，它是写给操作员的带注释模板，
   Go 的 `Default()` 才是权威，靠 `TestDefaultTemplateMatchesDefault` 防漂移。
   `docs/compatibility/endpoint-manifests.json` 是 golden 产物（`manifest_test.go:28` 拿代码
   生成的结果比对它）。`internal/modelcatalog/builtin.go` 是 Go 代码。**仓库里没有任何一份
   声明式文件是行为的权威来源**，开这个先例需要一个理由，而这里找不到。

**结论**：采纳 §3.0 的 Go 声明式表。它是「把数据当数据」这个正确直觉的落地形态，只是
落在 Go 而非 YAML——因为能力上限是关于「这个二进制里编译了哪些 Primitive」的断言，
应当与那些 Primitive 同语言、同编译单元。

**唯一确实该进配置文件的是部署量**：Bedrock region（见 §3.1 注 A）。它不是能力声明，是
部署拓扑，因部署而异且不影响任何安全边界。

> **附带说明：配置文件化本身并不解决本提案要解决的问题。**
> 痛点是 Go 与 TypeScript 各有一份。把 Go 那份挪进 YAML，得到的是 YAML 一份、TypeScript
> 仍然一份——数量没变，只是其中一份换了格式。真正消灭第二份的是 §3.1 的下发端点：一旦
> 前端从服务端读，权威那份是 Go 常量还是 YAML 文件，前端完全不关心。两件事正交。

### 4.4 让操作员用配置声明 `openai_compatible` 的能力

`openai_compatible` 的上限刻意保守（只有对话、流式、嵌入），注释写着 "Compatibility
servers vary"（`models.go:610-611`）——一个跑 vLLM 且确实支持工具调用的操作员今天表达不了
这件事，这是「让能力可配置」最有说服力的真实用例。

不采纳，因为仓库对这个问题已有更好的答案：`internal/provider` 的能力探测——**去探这台
服务器实际支持什么并产出证据**，而不是让人在文件里断言。断言会错，探测不会；这也与
`CapabilityEvidence` 的 `declared` / `verified` 分级一致（`builtin.go` 的准入策略说明：
内置条目封顶 `declared`，只有真实探测才产生 `verified`）。
另一半需求——「我知道这个部署不支持视觉」——今天本来就合法：逐连接收窄一直允许
（`capability_ceiling_test.go` 的 `TestImmutableProfileBindingMayNarrowItsCeiling`），不需要
动上限。

## 5. 前置修复（不等本提案，应立即做）

`46fcfe6` 的回归本身要先修，且修复与本提案正交。

**修法只有一个：`input.Bindings != nil` 时跳过顶层校验。** 初稿把它与「对照所有 binding
上限的并集校验」并列为「或」，评审判定后者语义错误，本稿删去该备选：

- **跳过是正确的**：带 bindings 时顶层 capabilities 在 `admin_providers.go:1381` 被
  `BindingsCapabilitiesSummary` 无条件覆盖，被校验的值随即丢弃；per-binding ceiling 在
  handler（`:1356-1361`）与 domain 写边界（`models.go:345-362` 的 `binding.Validate`，经
  `:1391` 的 `instance.Validate()`）双重强制；「至少一项操作能力」由 `models.go:512` 兜底
  （`admin_providers_test.go:84-92` 已验证全禁用 binding 的启用连接仍 400），
  streaming-requires-chat 由 `models.go:515` 与 `binding.Validate` 在最终值上兜底。
  跳过不放松任何不变量。
- **并集校验是错的**，三个理由：(1) 它校验一个随后被丢弃的值，且「并集」本身就是本提案
  要消灭的第二份规则；(2) 对数值字段语义错误——`capabilityLimitSubset`
  （`provider_profile.go:324`）在上限 >0 时要求提交值 >0，若并集上限含 Titan Embed 的
  `MaxContextTokens: 8192`（`models.go:628-629`）而顶层载荷数值为 0，会**重新制造出与本次
  故障同构的 400**；(3) 若按 `BindingsCapabilitiesSummary` 的 max 方式合并
  （`models.go:417-422`），「无上限(0) ∪ 8192」会被并成 8192——0 上限的其他 Bedrock Profile
  与 Titan Embed 同 Surface 同凭据方案、可共存于一个连接，等于把无上限收紧成 8192。

**必须一并写明的契约变化**：修复后，直接 API 调用者在 `bindings` 旁提交任意顶层
`capabilities` 都会被**接受且不生效**。这是「接受但完全忽略的请求字段」，与仓库
"rejected, never silently dropped" 的一贯口径不符，只能作为**临时状态**存在——§3.3 是它的
终局（顶层字段不再发送或被拒收）。修复 PR 的注释与本文都要写明这一点，避免它悄悄固化
进 Admin API 契约。

**回归测试必须复刻控制台的真实载荷**（顶层能力 = 并集 + 双 binding）。现有测试只发
`bindings` 不发顶层 `capabilities`（`admin_providers_test.go:61-104`），恰好绕开了故障路径。
2026-08-16 排查时已写出可用的复现测试（OpenAI 全 15 项能力 + 双 binding 期望 201，
当前 main 返回 400），可直接收编进 `internal/app`。

**修复落地前操作员的临时绕过**：新建/编辑 OpenAI 连接时，手动取消全部六项媒体能力
（内容审核、图像、音频转写、语音合成、文件、批处理）后再保存。代价是该连接失去媒体
功能。之所以需要写明：故障在**默认表单**上即触发（默认勾选就含这六项，
`ProvidersPage.tsx:860`），而操作员看到的是通用标题「请求内容无效，请检查后重试。」
加一句英文实现术语 `provider capabilities exceed what this profile can serve`
（`admin_providers.go:1335` 原样透出），既不指认哪一项、也不是操作员语言，无从自行推断。

## 6. 实施顺序

1. **修回归**（§5）：跳过顶层校验 + 契约变化注释 + 控制台真实载荷回归测试。独立 PR，先行合入。
2. **domain 重构为声明式表**（§3.0）：建 `profileTable`，六处 switch 与内联切片改为查表，
   导出 `AllProviderProfiles()` / `CapabilityNames()` / 能力键映射。**纯重构，行为不变**——
   现有测试应原样通过，这是这一步的验收标准。同时补
   `TestCeilingWithinProfileManifestOperations`（今天缺失的不变量）。
3. **Base URL 模板进表、region 进 config**（§3.1 注 A）：表加 `BaseURLTemplate`；
   `config.yaml` 加可省略的 `providers.bedrock.default_region`；`default.yaml` 同步；
   确认 `version` 保持 1、既有配置文件不改也能启动。
4. **端点**：`GET /admin/api/v1/provider-profiles` + 组装代码（组装 = 遍历表 + 替换 region）。
   配三条不变量测试：每个 Profile 的 `defaults ⊆ ceiling`（用
   `domain.ProviderCapabilitiesSubset` 的数值语义，不是纯布尔）；**遍历 `profileTable`**
   逐项比对端点输出（防漏列）；`capability_names` 每项在 zh-CN 与 en-US 都有
   `capabilities.*` 文案。
5. **前端切换 + 提交语义收敛**（§3.2 + §3.3，合并为一个 PR，理由见 §3.4）：拉取端点、
   删除全部镜像（含 `DeploymentsPage` 与凭据表单）、扁平能力提交、补挂载/失败态；
   `npm run build` 重新内嵌 `internal/webui/dist`。

第 2–4 步应同一 PR 合入：端点没有消费者不该单独存在。第 5 步紧随其后，镜像在同一
发布周期内删除，避免「两套并存」过夜。

## 7. 验证

- **第 2 步（重构）单独验证**：它声称行为不变，所以 `go test ./...` 应在**不修改任何现有
  测试**的前提下通过。若某条既有测试需要改动，说明这不是纯重构，要停下来解释为什么。
- Go：`go test ./internal/app/ ./internal/domain/ ./internal/config/`
  （新端点测试 + §5 回归测试 + 四条不变量测试 + `TestDefaultTemplateMatchesDefault`）。
- 前端：`ProvidersPage` / `DeploymentsPage` 相关 vitest 改为 mock 端点响应；`npm run typecheck`。
- **既有 `config.yaml` 必须不改也能启动**：拿一份不含 `providers:` 段的现有配置起一次
  `halro start`，确认 region 回落到 `us-east-1` 且 `version` 仍为 1。这条按仓库的
  「Verify, never assume」写明：**用真实的既有配置文件跑，不用测试里现造的 fixture**——
  fail-closed 的配置校验一旦判错，代价是拒绝启动。
- 真实二进制：起一次 `halro start`，控制台里对**每个 provider 类型**各建一个连接、
  全选可勾能力并保存成功；**Bedrock 逐个「能力实现」各建一次**（Titan Embed 的
  `max_context_tokens` 数值路径只有它覆盖得到）；改一次 `default_region` 重启，确认表单
  预填值随之变化；再验证端点失败时表单给出可读原因、服务商列表仍可用。
  §5 的故障正是只有真实控制台载荷才踩得到的。
- push 前过一次完整 gate，含 `git diff --exit-code -- internal/webui/dist`。

## 8. 开放问题

> 已关闭：**权威数据的存储形态**（Go 声明式表 vs YAML 配置文件）→ 见 §4.3，采纳 Go 表。
> **Bedrock region 的处置** → 见 §3.1 注 A，进 `config.yaml`，省略即保持 us-east-1。

- **`provider_executed_tools` 的出网警示**：该能力「上限允许但默认关闭、开启即接受不经
  SafeTransport 的上游出网」，这一后果目前只存在于 Go 注释（`models.go:656-663`），棋盘上
  与其他复选框完全同构渲染（`ProvidersPage.tsx:762`），操作员勾选时无从知晓。对一个
  security-first 网关这是实质缺口。端点是否加一个标注位（如 `opt_in_warning: true`）？
  若加，**文案须用操作员语言陈述后果**（例如「开启后上游可自行执行工具并额外访问网络，
  该流量不经过 Halro 审查」），形态可复用本页既有的 notice warning
  （`ProvidersPage.tsx:744-748` 的计费探测警示块）；标注位只作为线上的开关，不得由它
  直接派生英文实现名到 UI。
- **端点故障的运维含义**：今天能力矩阵是纯前端常量，对话框打开即用、不依赖任何请求。
  改造后端点失败（会话过期、网络中断）时，操作员将无法新建或编辑任何服务商连接与凭据——
  这是硬编码时代不存在的新失败模式。端点内容为编译期常量、除认证与网络层外无失败源，
  评审时需就此权衡是否接受，以及恢复路径（重试 / 重新登录）如何呈现。
- **端点粒度**：一次性下发全矩阵（几 KB）还是按类型查询？倾向全矩阵——数据是编译期
  常量，体积可忽略，且表单切换类型时零额外请求。

---

## 9. 实施记录（2026-08-17）

分支 `feat/provider-capability-single-source`，五个提交，每个都跑过完整 `go test ./...`
与 `go vet ./...`，前端跑过 typecheck + 333 个 vitest + build。

| 提交 | 对应 | 内容 |
| --- | --- | --- |
| `035ecdb` | §5 | 带 bindings 时不再校验随后被覆盖的顶层能力；三条回归测试复刻控制台真实载荷 |
| `7cb9b1d` | §3.0 | 六处 switch 与内联切片收敛成 `internal/domain/provider_table.go`；新增 ceiling↔manifest 不变量 |
| `d25253e` | §3.1 注 A | Base URL 模板进表、`providers.bedrock.region` 进 `config.yaml` |
| `b788a6d` | §3.1 | `GET /admin/api/v1/provider-profiles` |
| `f07ab24` | §3.2 | 前端删除全部镜像，改为消费端点 |

**做到的**：§1 列出的镜像全部消失。`ProvidersPage.tsx` 不再有能力清单、默认值、上限、
不可变 Profile 清单、Bedrock Profile 清单与配置、默认 Base URL、OpenAI 拆分集合；
`DeploymentsPage.tsx` 的第二份能力名清单改由展示分组推导。

**每条修复都做了反向验证**（按 CLAUDE.md：不失败的反向验证不是证据），且每次都先确认
编辑真的生效：退掉 §5 的修复 → 回归测试以用户报告的同一条错误失败；给 Gemini 的 ceiling
加一项没有 Primitive 的能力 → 新不变量失败；关掉 region 的默认填充 → 兼容性测试失败；
让端点漏掉一个 profile → 防漏列测试失败；从展示分组里删掉 `vision` → 分组完整性测试失败。

**真实二进制验证**（临时数据目录，未触碰本机 `data/`）：新装实例的 `config.yaml` 带出
注释齐全的 `providers` 段；把该段整个删掉后**实例照常启动**（这是兼容性的核心断言，
用真实生成的配置文件验证，不是测试里现造的 fixture）；`/provider-profiles` 未认证返回
401、拼错路径返回 404，说明路由真的挂上且鉴权真的生效。

**前端测试的 fixture 来自真实端点**：`web/src/test/provider-profiles.golden.json` 由
`TestProviderProfilesGoldenMatchesConsoleFixture` 从跑起来的 handler 生成并校验，
两边漂移即失败。手写 fixture 只能测到写的人对矩阵的想象，而"对矩阵的想象与服务端不符"
正是本提案要消灭的东西。

### 仍未做：§3.3 的服务端拆分

前端现在提交的仍是 bindings，只不过**拆分规则不再硬编码**——它由端点下发的各 profile
ceiling 推导（能力归入 ceiling 覆盖它的那个 profile）。§2 的「删除镜像」目标因此已达成，
但 §3.3 想要的「前端完全不知道 binding 存在」尚未达成。

实施中撞上了 §3.3 rule 2 预告的那个歧义，并且把它量化了：

| 共享 (type, surface, scheme) 的组 | profile 数 | ceiling 是否互斥 |
| --- | --- | --- |
| `openai` / `openai-api` / `bearer.static` | 2 | **互斥**（chat 集 vs media 集） |
| `bedrock` / `bedrock-runtime` / `sigv4` | 4 | **互斥**（对话 / 嵌入 / 图像 / 异步各一） |
| `bedrock` / `bedrock-mantle` / `api-key` | 3 | **重叠严重**（三者共享 chat/streaming/tools/vision/stream_usage） |

所以自动拆分对前两组无歧义、对第三组有歧义。**但第三组不需要自动拆分**：Mantle 与
Bedrock runtime 的 profile 都由操作员在「能力实现」里显式选定，只产生一个 binding。

由此得到 §3.3 落地时该钉死的规则（这是实施带回来的结论，不是提案时的猜测）：

> **操作员显式选定 profile 时 → 单个 binding；未选定（OpenAI）时 → 按 ceiling 自动拆分。**
> 后者只在 ceiling 互斥的组里发生，因此确定。

剩余工作与其验收条件：

1. 请求契约改为「扁平能力 + 可选 profile_id，不带 bindings」，服务端 `providerFromInput`
   按上面的规则分解。
2. §3.3 rule 1 必须实现：落不进任何候选 profile 的能力**返回 400 并指名**，不得静默丢弃。
   前端现在已经在本地拦下这种情况并指名（`validationCapabilityUnservable`），服务端还没有。
3. rule 3 的终局：`bindings` 存在时顶层 `capabilities` 从「接受但静默忽略」（§5 修复后的
   临时状态，已在 `admin_providers.go` 注释里写明）改为**拒收或不再发送**。
4. 需要一次请求契约评审——这是 API 形状变更，且 pre-1.0 可原地改，值得单独一个 PR。

### 其他遗留

- `providers.bedrock.region` 之外，`defaultBaseURL` 对 `azure_openai` 与 `openai_compatible`
  仍返回 `https://api.openai.com`。这是搬迁前就有的行为，原样保留了（纯迁移不改行为），
  但对这两个类型都不合理：Azure 需要资源专属域名，兼容服务器是自建地址。建议改为空值
  或要求显式填写，属独立的小改动。
- §8 的两个开放问题仍然开放：`provider_executed_tools` 的出网警示文案、端点故障的运维
  含义（后者已部分缓解——失败时页面顶部报错、创建按钮禁用并给出原因，但没有重试入口）。
