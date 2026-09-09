# Provider Offering 与订阅接入统一方案

状态：**阶段 1 已实施；阶段 0 / 2 / 3 / 4 未实施**（实施差异见 §14）  
建立日期：2026-09-09  
最近修订：2026-09-09（第 1 轮 review 并实施阶段 1）  
范围：`internal/domain`、`internal/provider`、`internal/compatibility`、`internal/app`、
`internal/modelcatalog`、`internal/usage`、`web/src`、运维与用户文档  
首期落地：通用 Offering 抽象、控制台统一选择器、BigModel / Z.AI Coding Plan  
后续候选：Kimi Code、MiniMax Token Plan，以及需要 OAuth 的 OpenAI Codex 与 Claude Pro/Max

相关：

- [BigModel / Z.AI 适配方案](bigmodel-adaptation-plan.zh-CN.md)
- [Kimi 适配方案](kimi-adaptation-plan.zh-CN.md)
- [MiniMax 适配方案](minimax-adaptation-plan.zh-CN.md)
- [平台登记点合并](platform-registration-consolidation.zh-CN.md)
- [Provider 能力单一来源](provider-capability-single-source.zh-CN.md)
- [Adding a provider platform](../contracts/adding-a-platform.md)

---

## 0. 决策摘要

Halro 应新增一个跨服务商的 **Provider Offering（上游接入产品）** 概念，统一表达：

- 按量 API 平台；
- Coding Plan / Token Plan 等订阅产品；
- 企业合约或预置吞吐产品；
- 由账号 OAuth 提供额度的官方客户端产品。

Offering 不是模型，也不只是计费标签。它可能同时改变：

- 凭据来源和鉴权方式；
- Base URL 与路径前缀；
- 可用协议；
- 可用模型集合；
- 并发、周期额度和余额语义；
- 允许使用的工具或业务场景；
- 错误码和用量查询方式。

因此，**不得**把 `Coding Plan`、`Token Plan` 放进"创建模型部署"的模型 ID 列表。部署层仍然只保存
上游真实模型标识符，例如 `glm-5.2`、`kimi-k3`、`MiniMax-M3`。

统一层级如下。注意 Offering 与 Region 挂在 **Access Surface** 上，不挂在 Profile 上——理由见
§2.3：

```text
Provider Type（厂商）
  └─ Access Surface（凭据实际进入的产品表面）
       ├─ Offering（接入产品）        ← surface 声明，profile 派生
       ├─ Region（账号/数据地域）      ← surface 声明，profile 派生
       └─ Provider Profile（协议、路径、能力与原语绑定）
            └─ Credential（凭据，存 surface + scheme）
                 └─ Provider Connection（连接，存 profile_id）
                      └─ Deployment（真实模型 ID）
```

核心实现取舍：

1. **Offering 是统一的服务端元数据，不是 BigModel 专用字段。**控制台从
   `GET /admin/api/v1/provider-profiles` 获取，不维护服务商私有判断表。
2. **Offering 与 Region 声明在 Access Surface 层。**凭据存的是 `(type, surface, scheme)`，
   这也是 `domain.ResolveCredentialProfile` 的解析键；把 Offering 挂到 profile 行上会让同一
   surface 下的多个 profile 可以声明不同 Offering，而凭据无法唯一解析产品身份。见 §2.3。
3. **Profile 仍是持久化和路由的真实事实。**连接存 `profile_id`；Offering、地域和展示分组从
   `profile → surface → offering` 派生，不在数据库再保存一份可能冲突的状态。
4. **凭据先绑定 Offering/地域，连接只能选择匹配的 Profile。**不能把订阅 Key 绑定到按量 API
   surface，也不能把国内 Key 配到海外 endpoint。
5. **模型部署只选择模型。**Offering 信息作为连接标签展示，不进入 `provider_model`。
6. **静态 Key 与 OAuth 分阶段实施。**BigModel Coding Plan、Kimi Code、MiniMax Token Plan 可以在
   真实协议验证后走静态凭据阶段；OpenAI、Anthropic 的账号订阅需要 OAuth 生命周期和官方授权边界，
   不得伪装成 API Key。
7. **受限订阅必须明确用途边界。**控制台展示官方使用限制并要求操作者确认；Halro 不声称它能判断
   下游工具是否满足上游条款。
8. **首期不是"纯新增"。**当前控制台不发 `access_surface`，服务端空值回退到 provider type 的默认
   profile，已经让海外 BigModel 凭据落在 CN surface 上。这是存量数据缺陷，不是展示问题；修法和
   影响见 §8。

---

## 1. 为什么不能用"订阅 / API"一个布尔字段

不同厂商虽然都有"订阅"和"按量 API"的区分，但产品形态并不一致：

| 厂商 | 按量/平台产品 | 订阅或额度产品 | 主要差异 |
| --- | --- | --- | --- |
| BigModel / Z.AI | General API | GLM Coding Plan | 静态 API Key，Coding 使用独立路径，模型与周期额度不同 |
| Kimi | Kimi Open Platform | Kimi Code | Base URL、Key 来源、计费与产品完全隔离 |
| MiniMax | API Platform | Token Plan | Token Plan 使用专用 Key，套餐并发和共享额度独立 |
| OpenAI | OpenAI API Platform | ChatGPT / Codex 额度 | API Key 与 ChatGPT 账号登录是两套鉴权和计费 |
| Anthropic | Anthropic Console API | Claude Pro / Max | Console API Key 与 Claude 账号 OAuth 是两套入口 |

一个 `is_subscription` 布尔值无法回答以下问题：

- 用哪个 endpoint；
- 用 API Key、订阅 Key，还是 OAuth refresh token；
- 一个凭据能组合哪些 Chat / Responses / Messages profile；
- 能否枚举模型；
- 额度耗尽应解释为余额不足、周期上限还是并发限制；
- 是否只允许 Coding Agent 场景。

所以使用稳定的 `OfferingID`，由每个厂商注册自己的 Offering；`Kind` 只用于展示和治理，不参与
路径或鉴权推断。

---

## 2. 领域模型

### 2.1 新增稳定标识符

建议在 `internal/domain` 增加：

```go
type ProviderOfferingID string
type ProviderOfferingKind string
type ProviderRegionID string
type ProviderRegionScope string

const (
    OfferingMeteredAPI   ProviderOfferingKind = "metered_api"
    OfferingSubscription ProviderOfferingKind = "subscription"
    OfferingEnterprise   ProviderOfferingKind = "enterprise"
)
```

`ProviderOfferingKind` 不能决定行为。行为仍由 Profile 的 surface、credential scheme、adapter builder
和 primitive bindings 决定。

Offering 必须覆盖**全部**已注册 provider type，不只是有订阅产品的那几家：`AllProviderProfiles`
是唯一枚举，不变量测试会走全表（含 withheld 行），所以每个 profile——包括 withheld 的五个 Bedrock
Runtime / Agent Runtime profile——都要能解析出一个 Offering。

反过来也成立：**没有 surface 引用的 Offering 不注册。**`openai.codex-subscription` 与
`anthropic.claude-subscription` 在本方案里被命名，但它们要等到拿到 surface（阶段 4）才进表——
一个没有任何东西能解析到的常量，只会逼不变量测试为它写例外。首批常量：

```go
const (
    OfferingOpenAIAPI        ProviderOfferingID = "openai.api-platform"
    OfferingAnthropicAPI     ProviderOfferingID = "anthropic.console-api"
    OfferingAzureOpenAI      ProviderOfferingID = "azure-openai.resource"
    OfferingDeepSeekAPI      ProviderOfferingID = "deepseek.api-platform"
    OfferingGeminiAPI        ProviderOfferingID = "google.gemini-api"
    OfferingBedrockRuntime   ProviderOfferingID = "aws.bedrock-runtime" // 全部 profile 当前 withheld
    OfferingBedrockMantle    ProviderOfferingID = "aws.bedrock-mantle"
    OfferingOpenAICompatible ProviderOfferingID = "openai-compatible.self-declared"
    OfferingKimiOpenPlatform ProviderOfferingID = "kimi.open-platform"
    OfferingMiniMaxAPI       ProviderOfferingID = "minimax.api-platform"
    OfferingBigModelGeneral  ProviderOfferingID = "bigmodel.general-api"
)
```

后续阶段加入（拿到 surface 时才注册）：`bigmodel.coding-plan`、`kimi.code`、
`minimax.token-plan`、`openai.codex-subscription`、`anthropic.claude-subscription`。

地域使用语义 ID，例如：

```go
RegionCN     ProviderRegionID = "cn"
RegionGlobal ProviderRegionID = "global"
RegionNone   ProviderRegionID = ""
```

地域表示账号、凭据、余额与目录隔离边界，不承担云基础设施 region 的职责。AWS Bedrock 的
`us-east-1` 等运行地域继续使用现有配置（`profileRow.BaseURLTemplate` 里的 `{region}` 占位与
`domain.ResolveBaseURL`），不与这里的产品地域混为一谈——Bedrock 的产品地域是 `RegionNone`。

### 2.2 Offering 元数据

新增只读表：

```go
type providerOfferingRow struct {
    ID   ProviderOfferingID
    Type ProviderType
    Kind ProviderOfferingKind
}
```

不把显示名称或长篇警告文案放进 Go 表：

- 服务端下发稳定 ID；
- 前端 i18n 按 ID 渲染名称和说明；
- CI 校验每个**可达** Offering 都有中英文文案，并且没有多余文案（`withheld` 产品的文案会被判为
  不可达而删除，重新提供时测试的另一半又会把它要回来）。

**`RequiresUsageWarning` 与 `DocumentationURL` 不在阶段 1 加。**阶段 1 的每个 Offering 都是按量
API，没有需要确认的使用范围，也就没有真值可填；而订阅产品需要的文档链接是按 `(offering, region)`
的——BigModel 通用 API 的一手文档在 `docs.bigmodel.cn`（大陆）和 `docs.z.ai`（海外）是两份，一个
Offering 一条 URL 对其中一边必然是错的。这两个字段与第一个真正需要它们的订阅 Offering 一起进表，
而不是先作为空列让控制台绕着渲染。

**Offering ID 是持久标识符，不得改名或复用。**§7.1 的使用范围确认会把 Offering ID 写进 Admin 审计
事件，审计记录是只读且完整性受校验的，改名会让既有审计条目不可解释。这条与事件 kind 号、frame
epoch、migration 名同级：可以新增，不可改写，不可让同一个 ID 换含义。

### 2.3 Offering 与 Region 声明在 Access Surface 表

**不**在 `profileRow` 上加 `OfferingID` / `RegionID`。原因是凭据解析：

```go
// internal/domain/provider_profile.go
func ResolveCredentialProfile(providerType ProviderType, surface AccessSurface, scheme CredentialScheme)
```

凭据只存 `(type, surface, scheme)`，而多个 profile 共享同一个三元组是常态而非例外——OpenAI 的
chat 与 media、Bedrock 的四个 Runtime profile、Mantle 的三个、Kimi 的三个、MiniMax 的三个都是
如此，profileTable 的行顺序就是解析优先级。若 Offering 挂在 profile 行上，同一 surface 下的两个
profile 可以合法地声明不同 Offering，而凭据没有任何字段能选中其中之一。那正是 §0 第 3 条要避免
的"第二份真相"，只是换了个位置。

因此新增一张 surface 表，它是 Offering/Region 的唯一声明处：

```go
type surfaceRow struct {
    Surface     AccessSurface
    Type        ProviderType
    Offering    ProviderOfferingID
    Region      ProviderRegionID    // 仅 RegionScopeFixed 时非空
    RegionScope ProviderRegionScope
    // Hosts 是上游自己公布的地址，按表单展示顺序排列。ByEndpoint 时它就是那个
    // 选择本身，各行地域不同；Fixed 时各行都带该 surface 自己的地域，用来把一个
    // 绑定端点识别成"属于别处"。它是识别表，不是允许名单，见 §2.6。
    Hosts []RegionHost
}
```

派生方向单一：

```text
credential → (type, surface, scheme) → surfaceRow → offering_id + region
profile_id → profileRow → surface → surfaceRow → offering_id + region
```

不变量（§10.1 逐条落测）：

- 每个 `AccessSurface` 在 surface 表里恰好一行；
- `profileRow.Surface` 必须在 surface 表中存在，且 `surfaceRow.Type == profileRow.Type`；
- `同 (type, surface) ⇒ 同 (offering, region)`，由表结构保证，测试断言没有绕过它的第二处声明；
- 对 `RegionScopeFixed` 的 surface，`(type, offering, region)` 必须唯一解析出一个 surface——
  否则控制台的"产品 + 地域"两级选择无法落到一个 surface 上。

管理 API 若接收 `offering_id`，它只作为创建时的选择条件，服务端必须将其解析为 exact surface /
profile，不得原样持久化为第二份真相。

### 2.4 Access Surface 仍然保留

Offering 不能替代 Access Surface。二者回答的问题不同：

- Offering：操作者购买或开通了哪个上游产品；
- Access Surface：这个凭据实际发往哪个隔离的网络/协议表面。

例如 BigModel Coding Plan 至少需要：

```go
SurfaceBigModelCNCoding     AccessSurface = "bigmodel-cn-coding-api"
SurfaceBigModelGlobalCoding AccessSurface = "bigmodel-global-coding-api"
```

即使 General API 与 Coding Plan 使用同一个 host，也必须拆 surface，因为凭据权益、路径、模型和
额度证据不能互相复用。

### 2.5 Credential Scheme

认证方式按真实凭据生命周期建模，而不是都叫 subscription：

| 模式 | Credential Scheme 示例 | 存储方式 |
| --- | --- | --- |
| 普通静态 API Key | `bearer.static`、`bigmodel.api-key` | Vault 加密保存 |
| 订阅静态 Key | `bigmodel.coding-plan-key`、`minimax.token-plan-key` | Vault 加密保存，绑定专属 surface |
| OAuth 账号授权 | `openai.codex.oauth`、`anthropic.claude.oauth` | access/refresh token 加密保存并支持刷新与撤销 |

首期不得为了复用表单，把 OAuth token 填入 API Key 输入框。OAuth 的完整代价见 §7.2。

### 2.6 地域的两种形态：RegionScope

上游表达"账号地域"的方式不止一种，现有代码里已经同时存在两种，方案必须承认这一点，否则控制台会
出现"BigModel 有地域选择器、Kimi 没有"的不对称：

```go
const (
    RegionScopeNone       ProviderRegionScope = "none"
    RegionScopeFixed      ProviderRegionScope = "fixed"
    RegionScopeByEndpoint ProviderRegionScope = "by_endpoint"
)
```

- **`fixed`**：surface 本身钉住一个地域。BigModel 就是这样——`bigmodel-cn-general-api` 与
  `bigmodel-global-general-api` 是两个 surface、两条 profile、两套能力集。地域选择器选的是 surface。
- **`by_endpoint`**：一个 surface 服务多个账号地域，靠凭据的 bound URL 区分。Kimi 与 MiniMax 是
  这样：`SurfaceKimi` 一条覆盖 `api.moonshot.ai` 与 `api.moonshot.cn`（该 surface 的注释已记录
  两站是同一契约、Key 不通用），`SurfaceMiniMax` 覆盖 `api.minimax.io` 与 `api.minimaxi.com`。
  地域选择器选的是 host，写入 bound URL。
- **`none`**：没有产品地域。OpenAI、Anthropic、DeepSeek、Gemini、Azure（资源自带 host）、Bedrock
  Mantle（云 region 由配置给出）、openai_compatible 都属于这一类。

`Hosts` 是**识别表，不是允许名单**。`profileRow` 的既有契约明确写了 `BaseURLTemplate` 是
prefill 而非 bound，操作者可以填任何 endpoint（企业代理、私有入口），出站允许名单从已保存连接
派生。因此：

- host 命中 → 地域已知，控制台按 ID 展示；
- host 未命中 → 地域为 unknown，控制台显示"自定义端点（地域未知）"，**不拒绝保存**；
- 地域 unknown 不影响任何隔离，因为运行时的模型目录缓存键已经是 `instance.ID + binding.ID` 并
  校验 provider/credential revision（§6.3），不依赖地域标签。

`fixed` 的 surface 同样列 host，虽然它的地域不需要从 endpoint 读。原因是 `halro doctor`：
`SurfaceForEndpoint(type, endpoint)` 要能回答"这个 host 属于同类型的哪个 surface"，才能报出
"凭据封存在大陆 surface、却绑定到海外 host"这一存量缺陷（§8.3）。识别到**别的** surface 才算发现；
识别不出来的（代理、私有入口）什么都不说。

这条同时给出 §11 验收里"endpoint 不匹配即拒绝"的准确形式：**拒绝只发生在有 profile 级 endpoint
规则的 surface 上**（今天只有 Bedrock Mantle 的 `bedrockmantleprovider.ValidateEndpoint`），其余
surface 的 host 不匹配是展示与告警，不是 400。把 prefill 变成 bound 是另一个设计决定，不在本方案
范围内。

---

## 3. Admin 元数据契约

扩展现有：

```http
GET /admin/api/v1/provider-profiles
```

建议响应增加：

```jsonc
{
  "provider_types": [
    {
      "type": "bigmodel",
      "default_profile_id": "bigmodel.cn.chat-embeddings.v1",
      "offerings": [
        {
          "id": "bigmodel.general-api",
          "kind": "metered_api",
          "requires_usage_warning": false,
          "documentation_url": "https://docs.bigmodel.cn/cn/api/introduction",
          "region_scope": "fixed",
          "regions": ["cn", "global"]
        },
        {
          "id": "bigmodel.coding-plan",
          "kind": "subscription",
          "requires_usage_warning": true,
          "documentation_url": "https://docs.bigmodel.cn/cn/coding-plan/quick-start",
          "region_scope": "fixed",
          "regions": ["cn", "global"]
        }
      ],
      "profiles": [
        {
          "id": "bigmodel.cn.coding.chat.v1",
          "offering_id": "bigmodel.coding-plan",
          "region_id": "cn",
          "access_surface": "bigmodel-cn-coding-api",
          "credential_scheme": "bigmodel.coding-plan-key",
          "default_base_url": "https://open.bigmodel.cn"
        }
      ]
    }
  ]
}
```

`region_scope: "by_endpoint"` 的 Offering 额外下发 host 识别表，让控制台能把地域选择器渲染成
endpoint 选择：

```jsonc
{
  "id": "kimi.open-platform",
  "kind": "metered_api",
  "region_scope": "by_endpoint",
  "region_hosts": [
    { "region": "cn",     "host": "api.moonshot.cn" },
    { "region": "global", "host": "api.moonshot.ai" }
  ]
}
```

约束：

- `offerings`、`regions`、`region_hosts` 和 `profiles` 必须从 domain 的 surface / offering 表派生；
- 不允许 Admin handler 自写第二份 provider → offering 清单，理由与 `buildProviderProfilesView`
  当初存在的理由相同（消除第二份副本，而不是同步它）；
- withheld profile 不得出现在可选列表——现有实现已经过滤 profile 本身及 `combines_with` 里的
  withheld 同伴，Offering 沿用同一规则；
- **一个 Offering 的可达 profile 数为 0 时，必须整条从 `offerings` 数组里省略。**不能只依赖
  "候选 Offering 除外"的措辞，否则控制台会列出一个选得中、但下面没有任何 profile 可选的产品。
  `openai.codex-subscription` 与 `anthropic.claude-subscription` 首期就是这种状态；
- 一个可达 profile 必须引用一个同 provider type 的可达 Offering；
- 每个 provider type 的默认 profile 必须能解析出 Offering。

---

## 4. 控制台交互

### 4.1 凭据保险库：选择产品身份

凭据创建顺序：

```text
服务商 → 接入产品 → 地域 → 鉴权方式 → 凭据/登录
```

示例：

```text
服务商：BigModel / Z.AI
接入产品：GLM Coding Plan
地域：海外 Z.AI
鉴权方式：Coding Plan API Key
```

规则：

- Offering 只有一个时隐藏选择器，但仍由服务端元数据决定；
- 地域按 `region_scope` 渲染：`fixed` 选 surface，`by_endpoint` 选 host 并写入 bound URL，
  `none` 不渲染；
- 鉴权方式只有一个时显示为只读说明；
- 选择改变时重置 endpoint 与 profile，不能静默沿用；**已输入的密钥不清空**——表单顺序是
  「服务商 → 产品 → 地域 → 密钥」，改产品时密钥通常还是空的，而清掉操作者刚粘贴、无法凭记忆
  重打的材料，正是既有轮换代码专门避免的失败模式；
- 凭据保存后展示 `服务商 · Offering · 地域 · 鉴权方式`；
- 订阅产品的使用范围提示紧邻 Offering，不放在提交后的错误里；
- `requires_usage_warning=true` 时，提交前要求一次明确确认，并写入 Admin 审计事件。

**有选择时必须显式发送 `access_surface` 与 `scheme`。**今天的表单只在 Bedrock 分支发这两个字段，
其余类型留空，服务端于是回退到 provider type 的默认 profile——这正是 §8.3 那个缺陷的成因。
修法不是"一律要求显式"，而是**按是否有歧义**：该 provider type 只有一个可达 `(surface, scheme)`
时继续解析（"只有一个答案的问题不问"，与控制台既有的 Bedrock 注释同一条原则），有两个及以上时
具名拒绝并列出可选项。控制台则始终发送，因为它总是知道自己选了哪个。

**轮换（rotate）时产品身份只读。**现有轮换路径故意不发 surface/scheme，以保留凭据被封存时的取值
（包括本 build 已不再提供的 surface，那种凭据只能删除、不能被静默改指向）。因此 UI 的"改选择即
重置"只适用于创建：改 Offering 或地域 = 新建凭据，不是轮换。§7.2 的"切换 Offering 必须新建或重新
绑定"与这条是同一条规则的两面。

### 4.2 服务商连接：从凭据派生产品

当前控制台先选 provider type，再从同类型凭据中挑选；Profile 选择器只为 Bedrock 特判。
这无法正确表达 BigModel 的国内/海外 general profile，也不能扩展订阅产品。

调整为：

```text
连接名称 → 凭据 → 接口实现/Profile → 能力 → 并发
```

选择凭据后：

- Provider Type、Offering、Region、Access Surface、Credential Scheme 从凭据身份派生并只读展示；
- Profile 下拉只列出与该凭据 surface/scheme 匹配的 profile；
- Base URL 使用 Profile 默认值，并继续执行凭据 bound URL 一致性检查；
- 如果允许企业代理或私有入口覆盖 Base URL，放入"高级设置"，修改后要求重新绑定凭据；
- 不再使用 `type === "bedrock"` 的专用 Profile 分支。实现选择器的渲染条件由元数据给出，而不是
  provider 名：**该 type 有多于一个凭据身份**（BigModel 的两个地域产品），**或者该连接组是
  route-partitioned 的**（Bedrock Mantle，模型各自只在其中一条路由上应答）。两者都不成立时，
  组内 profile 同乘一个连接，没有要问的，控件不渲染。`route_partitioned` 因此进入服务端元数据。

**要清掉的前端私有表比"一个 bedrock 分支"多。**`web/src/pages/ProvidersPage.tsx` 里目前有：

- 7 处 `type === "bedrock"` 条件，外加 `bedrockCredentialProfile` 辅助函数与四处
  `findProfile(catalog, "bedrock", …)`；
- `supportsAnthropicBetas` 里硬编码的 profile ID `bedrock.mantle.anthropic.messages.v1`——这同样
  是 provider 私有枚举。服务端把 `sends_anthropic_betas` 作为元数据下发（来源是既有的
  `domain.ProfileSendsAnthropicBetas`），并且**写路径也改用同一个判断**：原先按 surface 检查
  （`anthropic` 或 `bedrock-mantle`）比事实宽——Mantle 上锚在 OpenAI chat/responses profile 的
  连接会存下永远不会被发送的 beta token；
- `regionHintKey` 的 provider type switch——它应由 §2.6 的 `region_scope` / `region_hosts` 取代。

保留的是 `providerTypes` 数组：它是**下拉框的展示顺序**，不是能力或产品判断。服务端元数据里
provider type 的顺序是 domain 表的登记顺序，不承担"先给操作者看哪个"的职责；把展示顺序也搬到
服务端，等于让后端替前端决定排版。这一项不算私有枚举表。

§11 第 1 条"前端无 provider 私有枚举表"以上面各项清除为准。

控件沿用控制台既有的"能力实现"标签（不是"计费方式"）；产品与地域是凭据表单的问题，不在这里
重复问。选项文案按 profile ID 取 i18n，缺文案时回退到"产品 · 地域"，例如：

```text
BigModel / GLM Coding Plan / 中国大陆 / OpenAI Chat Completions
Kimi / Kimi Code / Global / Anthropic Messages
```

### 4.3 模型部署：只选择真实调用目标

"创建模型部署"保持：

```text
部署名称 → 服务商连接 → 模型 ID → 能力与限额
```

改进展示：

- 服务商连接选项增加 Offering 与地域 badge，例如 `Z · Coding Plan · 海外`；
- 模型 ID 只来自该连接的上游枚举或该 exact profile 的内建能力目录；
- `Coding Plan`、`Token Plan`、`API Platform` 永远不是模型 ID；
- 用户切换连接时清空原模型 ID，避免把 general profile 的模型带到 subscription profile；
- Refresh 调用所选连接自己的模型目录，不能复用同 provider type 的其他连接结果（现有实现已如此，
  见 §6.3）；
- 未验证模型保持 unknown，允许操作者声明能力，但不从名称猜测。

### 4.4 错误与用量提示

订阅产品的 429 不能全部显示为"请求过于频繁"。应在保存原始上游业务码的前提下分类：

- 并发/速率限制；
- 周期额度已耗尽及重置时间；
- 套餐已过期；
- 当前套餐没有模型权限；
- 产品 endpoint 与凭据不匹配；
- 平台过载。

北向 API 仍返回稳定、兼容的错误类型；Admin Failure Detail 展示经过脱敏的上游 code、message、
Offering 和 profile，帮助操作者区分"换 Key""换模型""等额度恢复"和"稍后重试"。

---

## 5. 服务商接入矩阵与分期

本节引用的所有上游 URL 均按该文自己在 §5.1 定的规矩处理：**标注复核日期，未复核的显式写明**。
下列链接为撰写时按官方站点结构记录，尚未逐条抓取复核，实施阶段 0 必须逐条打开并补日期。

### 5.1 BigModel / Z.AI

通用 API：

| Region | Offering | Surface | Profile | 根地址 | Path Prefix |
| --- | --- | --- | --- | --- | --- |
| CN | `bigmodel.general-api` | `bigmodel-cn-general-api` | `bigmodel.cn.chat-embeddings.v1` | `https://open.bigmodel.cn` | `/api/paas/v4` |
| Global | `bigmodel.general-api` | `bigmodel-global-general-api` | `bigmodel.global.chat.v1` | `https://api.z.ai` | `/api/paas/v4` |

新增 Coding Plan：

| Region | Offering | 新 Surface | 新 Profile | 根地址 | Path Prefix |
| --- | --- | --- | --- | --- | --- |
| CN | `bigmodel.coding-plan` | `bigmodel-cn-coding-api` | `bigmodel.cn.coding.chat.v1` | `https://open.bigmodel.cn` | `/api/coding/paas/v4` |
| Global | `bigmodel.coding-plan` | `bigmodel-global-coding-api` | `bigmodel.global.coding.chat.v1` | `https://api.z.ai` | `/api/coding/paas/v4` |

上表的 path prefix 是**待验证项，不是已知事实**：`/api/coding/paas/v4` 取自 BigModel 国内文档，
Z.AI 海外站是否使用同一前缀尚未核实。见 §13 第 1、2 问。

首期 Coding profile 只声明真实验证过的 OpenAI Chat 能力。不得从 general profile 自动继承：

- Embeddings；
- Vision；
- Structured Outputs / JSON mode；
- provider-executed tools；
- general API 的模型目录和模型能力证据。

需要先用真实订阅账号验证：

1. `GET /api/coding/paas/v4/models` 是否存在；
2. 返回 shape 与账号实际可见模型；
3. Chat unary / stream；
4. usage、缓存 token 和 finish reason；
5. tool calls 与 arguments shape；
6. thinking 默认值、开关和 effort；
7. 额度耗尽、模型无权限、套餐过期的原始业务码；
8. 国内与海外 Key 是否严格隔离；
9. 大小写模型标识符是否归一化或严格区分；
10. 海外站的 coding path prefix。

如果上游提供模型列表，则上游列表是"谁存在"的唯一来源；内建目录只补充能力。如果订阅 endpoint
确实没有模型列表，才允许使用带来源和复核日期的订阅模型 seed。

官方资料（未复核）：

- [BigModel 通用 API](https://docs.bigmodel.cn/cn/api/introduction)
- [BigModel Coding Plan 快速开始](https://docs.bigmodel.cn/cn/coding-plan/quick-start)
- [Z.AI General/Coding Endpoint](https://docs.z.ai/api-reference/introduction)
- [Z.AI Coding Plan](https://docs.z.ai/devpack/quick-start)

### 5.2 Kimi

Kimi Open Platform 与 Kimi Code 是独立产品，Base URL、Key 来源和额度不互通。Kimi Code 官方资料
给出的订阅入口为 Anthropic-compatible `https://api.kimi.com/coding/`（未复核）。

候选 Offering：

| Offering | 阶段 | 说明 |
| --- | --- | --- |
| `kimi.open-platform` | 已有能力迁移元数据 | 现有 Kimi profile 归属该 Offering，**含当前 withheld 的 `kimi.responses.v1`**；`SurfaceKimi` 的 `region_scope` 为 `by_endpoint` |
| `kimi.code` | 第二批静态 Key | 新 surface、凭据方案和 Anthropic profile；先真实验证 |

`kimi.responses.v1` 是被从一个已提供的 profile 组中间抽掉的第一条 profile，它仍要有 Offering
归属（不变量走全表），但不出现在 `offerings[].profiles` 和 `combines_with` 里。

不得把现有 `api.moonshot.cn` / `api.moonshot.ai` profile 改地址来兼容 Kimi Code；那会让开放平台
凭据和订阅凭据共享模型与能力证据。

官方资料（未复核；路径形式取自 [Kimi 适配方案](kimi-adaptation-plan.zh-CN.md) 记录的文档站结构，
大陆站与国际站分列）：

- Kimi Code FAQ：`https://www.kimi.com/code/docs/en/kimi-code/faq.html`
- Kimi Open Platform 文档索引：`https://platform.kimi.com/docs/llms.txt`（大陆）、
  `https://platform.kimi.ai/docs/llms.txt`（国际）
- OpenAPI Schema：`https://platform.kimi.com/docs/openapi.json` /
  `https://platform.kimi.ai/docs/openapi.json`

### 5.3 MiniMax

MiniMax 的订阅产品在官方站点上以 Coding Plan 入口呈现，业内也称 Token Plan；本方案统一使用
`minimax.token-plan` 作为 Offering ID，并在阶段 0 确认官方正式名称后固定 i18n 文案（ID 本身
不再改，见 §2.2）。它提供独立套餐权益和专用 `sk-cp` Key（未复核）。

`SurfaceMiniMax` 的 `region_scope` 为 `by_endpoint`（`api.minimax.io` 与国内站），与 Kimi 同形。

实施前必须确认：

- 国内、海外 endpoint；
- `sk-cp` 对应的 surface 和 Header；
- Chat / Responses / Anthropic 三条 wire 是否都开放；
- 模型枚举路径与实际 response；
- 多模态是否与文本共享同一 endpoint 和配额；
- 套餐使用范围和并发语义。

在这些证据完成前只登记 Offering 提案，不注册可达 Profile。

官方资料（未复核）：`https://platform.minimax.io/subscribe/coding-plan`；两站文档索引见
[MiniMax 适配方案](minimax-adaptation-plan.zh-CN.md)（`platform.minimax.io` / `platform.minimax.cn`）。

### 5.4 OpenAI

OpenAI API 与 ChatGPT/Codex 订阅是不同产品。使用 API Key 走 API 定价；使用 ChatGPT 账号登录
Codex 才消耗订阅/agentic usage。ChatGPT credits 也不是 API credits。

因此：

- 现有 OpenAI profiles 归属 `openai.api-platform`；
- `openai.codex-subscription` 不能复用 `bearer.static`；
- 只有在官方提供并允许第三方网关使用的 OAuth / delegated access 契约后，才能注册可达 profile；
- 在此之前它是一个可达 profile 数为 0 的 Offering，按 §3 的规则**不出现在元数据里**。

官方资料（未复核，帮助中心文章号需在阶段 0 打开确认标题与内容）：

- ChatGPT Work / Codex 使用说明（help.openai.com）
- ChatGPT credits 与 API credits 的区别（help.openai.com）

### 5.5 Anthropic

Anthropic Console API 使用 API 账单与 API Key；Claude Pro/Max 可由 Claude Code 通过 Claude 账号
OAuth 使用。两者不是同一个 Credential Scheme。

因此：

- 现有 Anthropic profile 归属 `anthropic.console-api`；
- `anthropic.claude-subscription` 需要 OAuth 生命周期、workspace 绑定和官方授权边界；
- 不能让操作者把 Claude OAuth token 粘贴到 `x-api-key` 字段；
- 首期不注册可达订阅 profile，因而同样不出现在元数据里。

官方资料（未复核）：Claude Code 认证文档，现址为 `docs.claude.com` 下的 Claude Code 章节；旧的
`docs.anthropic.com` 路径需在阶段 0 复核后再写入。

---

## 6. Adapter 与模型目录

### 6.1 Adapter builder

adapter builder 必须按 exact profile 决定路径和认证。今天 `bigModelOpenAIAdapter`
（`internal/app/provider_adapters.go`）把 `api/paas/v4` 写死，Coding profile 需要改成按 profile
分支：

```go
func bigModelPathPrefix(profile ProviderProfileID) string {
    switch profile {
    case ProfileBigModelCNCodingChat, ProfileBigModelGlobalCodingChat:
        return "api/coding/paas/v4"
    default:
        return "api/paas/v4"
    }
}
```

不能根据 Key 前缀、模型 ID、余额错误或 host 猜 Offering。原因是：

- 相同 host 同时服务 general 与 coding；
- Key 前缀不是稳定公开协议；
- 猜错后可能消耗错误余额或违反使用范围；
- 失败后自动切换产品会造成不可解释的重复计费。

这个 switch 必须自带测试，理由见 §10.3：这类错误在本仓库属于"没有机械守卫"的一类。

### 6.2 Operation 与 primitive bindings

每个新 Profile 按 [Adding a provider platform](../contracts/adding-a-platform.md) 完整登记：

- domain profile row 与 surface row；
- operation → primitive bindings；
- field compatibility；
- endpoint manifest coverage；
- adapter builder；
- model catalog capability evidence；
- provider wiring tests；
- frontend golden metadata。

即使 Coding endpoint 使用 OpenAI-shaped Chat，也不能直接声明与 general profile 等价。请求字段、
response、错误和 usage 必须通过该 endpoint 的 fixture 与真实账号证据建立。

### 6.3 模型枚举与能力证据分离：保持现状，不要改

规则保持：

```text
上游 /models：谁存在
内建 catalog：已知模型会什么
capability detection：这个账号上的目标实测会什么
operator declaration：没有证据时的显式选择
```

**隔离要求已经被现有实现满足，本方案不引入新的缓存键。**记录在这里是为了让新 profile 的实施者
知道它已经成立，不要去"修"一个正确的东西：

- `internal/modelcatalog` 的 `Key` 已包含 `ProviderType + Profile + TargetKind + Model + Region`；
- 运行时的目标枚举缓存键是 `instance.ID + binding.ID`，并在命中时校验 provider revision 与
  credential revision（`internal/app/admin_invocation_targets.go`）——比按
  `provider_type + model_id` 缓存严格得多，也比 `provider_id + profile_id + provider_model` 严格。

新增 Coding profile 自动继承这套隔离，不需要额外工作；需要的是 §10.3 里"general key 不会打到
coding path、coding key 不会打到 general path"那组断言。

### 6.4 不做产品间自动 fallback

路由可以在操作者显式配置的多个 Deployment 之间 fallback，但不能在一个连接内部从订阅产品自动
切到按量 API，或反过来。两者计费和用途不同，自动切换会改变成本边界。

如果用户需要"订阅额度耗尽后转按量"，应创建两个 Provider Connection、两个 Deployment，并在 Route
中显式排序；管理端应清楚显示下一目标属于另一个 Offering。

**跨 Offering 的 Route 级 fallback 必须在归因上可分辨**：usage attribution 与 failure capture 要
能回答"这次请求最终落在哪个 Offering"，否则"订阅耗尽转按量"产生的按量费用无法归因到具体产品。
这依赖 §7.3 的 Offering 维度。

---

## 7. 安全、合规与审计

### 7.1 使用范围提示

部分 Coding/Token 产品只允许指定工具或场景。Offering 元数据提供：

```go
RequiresUsageWarning bool
DocumentationURL     string
```

控制台确认文案至少说明：

- 该产品可能仅允许官方列出的 Coding Agent 或工具；
- Halro 无法替操作者判断其下游使用方式是否符合上游条款；
- 生产、SaaS、通用聊天或转售场景应使用 General API；
- 上游可限流、锁定或终止违规使用。

**确认粒度：按 `(credential, profile)` 绑定一次。**创建凭据时确认一次；此后任何会改变实际产品
表面的动作——新建一个落在该 Offering 上的连接、更换连接的 profile——都要求重新确认。轮换密钥
不重新确认（产品身份未变，见 §4.1）。这条先定下来，不留作开放问题，否则前后端会各实现一套。

确认动作进入 Admin audit log，记录 Offering ID、Profile ID、操作者和时间，不记录 Key。Offering ID
一旦写入审计即不可改名（§2.2）。

### 7.2 凭据隔离

- Vault 记录继续加密；
- 列表与日志只显示 Key 指纹/尾部掩码；
- 不根据 Key 内容在浏览器端判断产品；
- surface、scheme、bound URL 必须三者匹配，匹配的含义按 §2.6 定义；
- 切换 Offering 必须新建或重新绑定凭据，不能在原连接上静默改路径；
- OAuth refresh token 的读写权限要比普通 Provider Key 更窄，并在备份/恢复文档单独说明。

**OAuth 自动刷新不是一次管理员轮换，这是阶段 4 的核心设计问题，不是一行 checklist。**现有凭据
写入路径的形状是：`KeyVersion + 1`、bump revision、要求 step-up 重认证（
`internal/app/admin_providers.go` 的凭据构建路径与控制台的 `useStepUpPrompt`），而模型目录缓存
正是以 credential revision 作为失效条件（§6.3）。一个后台自动刷新的 token 会：

- 持续 bump credential revision，反复作废目标目录缓存；
- 绕过 step-up 与"轮换是管理员动作"的审计模型；
- 让"凭据最后一次被改动"这个审计事实同时表示两件不同的事。

阶段 4 必须先回答：刷新是否写同一条 credential 记录；如果是，revision 语义如何与目录缓存解耦；
如果不是，token 存在哪里、备份/恢复与 master key 轮换如何覆盖它。

### 7.3 计费与预算

Halro 当前预算以 token/价格为核心，但订阅产品可能按周期额度、prompt、并发或动态公平使用策略限制。
首期：

- 仍记录 token usage；
- Offering ID 写入 span、failure capture 和 usage attribution；
- 不把订阅调用伪装成金额为零的"免费 API"；
- 未有稳定单价时金额保持 unknown，而不是 0；
- 429 原始业务码和 reset time 经过脱敏后保留；
- 预算不能跨 Offering 聚合成同一上游余额含义。

**"金额 unknown" 在现有实现里有明确代价，必须写给操作者看。**`domain.UnknownPricePolicyEvidence`
要求 `InstanceExplicitOptIn` 且 `CostGovernanceDisabled`，`reason_code` 固定为
`cost_governance_disabled`（`internal/domain/pricing_snapshot.go`）。也就是说：**一个没有稳定单价
的订阅 Deployment，只能在该项目显式关闭成本治理的前提下运行；此时预算不再拦截该项目的调用。**

因此阶段 2 必须二选一，并写进 operator guide：

- 订阅产品若能拿到可核对的等效单价，按正常 price version 计价，预算照常生效；
- 若拿不到，操作者必须为该项目显式接受"成本治理关闭"，控制台在创建订阅 Deployment 时就说明
  这一点，而不是等到调用被记成 unknown 才发现预算没生效。

**Offering 进 usage attribution 会动 usage schema。**`internal/usage` 的
`parquetSchemaVersion` 当前为 6，且有 min-readable 范围与就地升级逻辑，因此新增 offering 列是
bump 到 7、旧分区仍可读、旧行 offering 为空——**不需要重新初始化数据目录**，但 §8.1 的"不需要
迁移"必须限定为"元数据不需要迁移"，usage 侧是一次带 min-readable 范围的版本推进。

后续再设计 subscription allowance dashboard，不阻塞静态 Key 的正确路由。

---

## 8. 存量数据与迁移

### 8.1 元数据派生不需要迁移

现有 Profile 通过 surface 映射到默认 Offering：

| Existing Profile | Surface | Offering |
| --- | --- | --- |
| `openai.chat-embeddings.v1`、`openai.responses.v1`、`openai.media-resources.v1` | `openai-api` | `openai.api-platform` |
| `anthropic.messages.2023-06-01` | `anthropic-api` | `anthropic.console-api` |
| `azure-openai.chat-embeddings.v1` | `azure-openai` | `azure-openai.resource` |
| `deepseek.chat.v1` | `deepseek-api` | `deepseek.api-platform` |
| `gemini.generate-content.text.v1beta` | `gemini-generate-content` | `google.gemini-api` |
| `openai-compatible.chat-embeddings.v1` | `openai-compatible` | `openai-compatible.self-declared` |
| `bedrock.mantle.*` | `bedrock-mantle` | `aws.bedrock-mantle` |
| `bedrock.runtime.*`、`bedrock.agent-runtime.*`（withheld） | `bedrock-runtime` / `bedrock-agent-runtime` | `aws.bedrock-runtime` |
| `kimi.*`（含 withheld 的 `kimi.responses.v1`） | `kimi-api` | `kimi.open-platform` |
| `minimax.*` | `minimax-api` | `minimax.api-platform` |
| `bigmodel.cn.chat-embeddings.v1` | `bigmodel-cn-general-api` | `bigmodel.general-api` |
| `bigmodel.global.chat.v1` | `bigmodel-global-general-api` | `bigmodel.general-api` |

因为 Offering 从 surface 表派生，**元数据侧**不需要批量迁移。旧连接读取后自动获得 Offering 展示。
usage 侧的 schema 推进见 §7.3。

### 8.2 Admin API：不保留旧请求形状

Admin API 的唯一客户端是内嵌控制台，且 `decodeAdminJSON` 拒绝未知字段。按仓库 pre-1.0 政策
（"错误的构造不得与其替代物并存"），首期不保留"旧创建请求"分支：

- 读取响应只增加字段，旧读取端忽略新字段仍可用；
- **凭据创建在有歧义时必须携带 `access_surface` 与 `scheme`**：服务端删除"两者为空一律回退默认
  profile"的行为，改为只在该 type 恰好只有一个可达 `(surface, scheme)` 时解析，否则具名拒绝并
  列出可选产品。轮换沿用已存储取值是另一回事，保留；
- **连接创建在有选择时必须携带 exact `profile_id`**（以及与之一致的 `access_surface` /
  `credential_scheme`）。没有选择时不发：命名一个实现等于断言"已启用的能力落在它上面"，而当矩阵
  只给了一个选项时，表单没有资格作这个断言——能力如何分配到组内各 profile 是服务端的答案；
- 请求同时携带 offering/profile 时，服务端校验二者一致；
- 不接受仅有 offering 而无法唯一解析 surface / profile 的请求。

把"猜"限制在没有歧义的地方，正是 §8.3 那个缺陷的根治手段——不是在它旁边加一条正确路径。

### 8.3 存量缺陷：海外 BigModel 凭据存在错误的 surface 上

这不是"控制台不能显式选择 global profile"这么轻。事实链：

1. 凭据表单只在 Bedrock 分支发送 `access_surface` / `scheme`（`web/src/pages/ProvidersPage.tsx`）；
2. 服务端两者为空时走 `DefaultProviderProfile`；
3. `providerTypeTable` 里 BigModel 的默认 profile 是 `bigmodel.cn.chat-embeddings.v1`；
4. 因此**所有**已创建的 BigModel 凭据，无论 bound URL 是 `open.bigmodel.cn` 还是 `api.z.ai`，
   存的都是 `bigmodel-cn-general-api`；
5. CN 与 Global 的能力集不同——`bigModelCNSet` 含 `Embeddings`，`bigModelGlobalSet` 不含
   （`internal/domain/provider_table.go`）。

后果：指向 Z.AI 的连接跑在 CN profile 上，并可能已经开启了该 endpoint 并不提供的 Embeddings。
所以**验收不能声称"现有连接网络行为不变"**——对这类连接，行为本来就是错的。

首期必须一起做：

- Profile 控件由服务端 metadata 驱动，BigModel CN / Global 都可显式选择；
- 凭据按对应 surface 过滤；
- 删除创建路径的默认回退（§8.2）；
- 增加 `halro doctor` 检查：报告 bound URL host 与所属 surface 的已知 host 不一致的凭据，以及
  由这类凭据派生的连接与其已启用能力。**只报告，不自动改写**——静默把 surface 改到 global 会同时
  改变该连接已声明的能力，而能力收窄需要一次重新验证与路由下线，那是操作者的决定；
- 运维文档写清处置方式：重新创建凭据与连接，并复核 Deployment 的能力声明；
- 增加回归测试，确保选择 global 时保存的是 global profile，且创建请求缺 surface 时被具名拒绝。

---

## 9. 实施阶段

### 阶段 0：证据采集

- [ ] 为每个候选订阅产品建立官方来源、endpoint、认证、用途限制清单，逐条打开链接并记录复核日期。
- [ ] 使用专用测试账号验证订阅 endpoint；测试会消耗套餐额度，必须显式启用。
- [ ] 先捕获真实 `/models`、Chat、stream、tool、usage 与 error body，再写 decoder fixture。
- [ ] 明确哪些产品允许第三方网关或自定义 Coding Agent。
- [ ] 确认 Z.AI 海外站的 coding path prefix 是否与国内一致。
- [ ] 不使用生产 Key，不在测试日志打印 secret。

### 阶段 1：Offering 通用抽象，不新增上游能力 — **已实施**

- [x] 新增 `ProviderOfferingID`、Kind、Region、`RegionScope` 与 offering / surface 两张表
      （`internal/domain/provider_offering.go`）。
- [x] 为**全部 10 个 provider type**、全部 13 个 surface（含 withheld profile 所在的三个）补
      Offering 与 RegionScope 元数据。
- [x] 扩展 `/admin/api/v1/provider-profiles`：`offerings`（含 `region_scope` / `regions` /
      `region_hosts`）、profile 的派生 `offering_id` / `region_id`，以及 `route_partitioned` 与
      `sends_anthropic_betas`；可达 profile 数为 0 的 Offering 整条省略（Bedrock Runtime 即如此）。
- [x] 不变量测试（`internal/domain/provider_offering_test.go`、
      `internal/app/admin_provider_offering_test.go`）：surface 唯一且同 type、offering 与 surface
      互相不孤立、RegionScope 与 Region/Hosts 自洽、`(type,offering,region)` 在可达 surface 上唯一、
      每个 profile 与每个 type 默认 profile 都能解析 Offering、每个 type 至少一个凭据身份、
      surface 认得自己的 prefill。
- [x] 凭据创建的默认回退改为"只在无歧义时解析"，有歧义时具名拒绝并列出可选产品。
- [x] 更新 TypeScript 类型与 `web/src/test/provider-profiles.golden.json`。
- [x] Credential / Provider 表单改为元数据驱动选择器；轮换时产品身份只读。
- [x] 清除前端私有表：7 处 `type === "bedrock"`、`bedrockCredentialProfile`、`isBedrockProfile`、
      `supportsAnthropicBetas` 的硬编码 profile ID、`regionHintKey`。（保留的 `providerTypes`
      数组是**展示顺序**，不是能力判断；见 §14。）
- [x] 修复 BigModel Global Profile 无法在控制台显式选择的问题，并加回归测试。
- [x] 新增 `halro doctor` 的 `credential_product` 检查（§8.3），运维处置写入 operator guide。
- [x] 重新生成 `internal/webui/dist`。

此阶段不改变任何**正确配置的**现有连接的网络行为；对 §8.3 那类错配连接，它把问题暴露出来，
处置由操作者执行。唯一的行为收窄是 anthropic-beta 的接受条件（§4.2），它只影响一种控制台从未
产生过的组合。

### 阶段 2：BigModel / Z.AI Coding Plan

- [ ] 注册两个 Coding surface（带 offering/region）、credential scheme 与 Profile。
- [ ] adapter 按 profile 选择 `/api/coding/paas/v4`，并为该 switch 写路径断言测试。
- [ ] 以真实 response 编写 Chat、stream、usage、tool 与 error fixtures。
- [ ] 验证并接入 subscription `/models`；若不存在，记录探测证据后使用保守 catalog。
- [ ] 只声明真实验证过的模型能力。
- [ ] 增加 CN / Global 凭据和目录隔离测试。
- [ ] 增加 plan expired、quota exhausted、model unavailable、rate limited 分类。
- [ ] 决定订阅计价形态（等效单价 or 显式关闭成本治理），并在控制台与 operator guide 写明后果。
- [ ] usage attribution 增加 offering 维度，bump `parquetSchemaVersion`。
- [ ] 更新兼容 manifest、operator guide、user guide 和真实 provider matrix。

### 阶段 3：Kimi Code 与 MiniMax Token Plan

- [ ] 每个产品独立完成阶段 0。
- [ ] 复用 Offering/UI 抽象，不复用未经证明的 general profile 能力。
- [ ] Kimi Code 按 Anthropic-compatible endpoint 建立独立 surface/profile。
- [ ] MiniMax 先确认 `sk-cp` endpoint 与可用 wire，再注册 profile。
- [ ] 为订阅额度耗尽和产品不匹配错误建立 fixture。

### 阶段 4：OAuth 型订阅

- [ ] 先解决 §7.2 的刷新语义：refresh 与 credential revision、step-up、审计、目录缓存的关系。
- [ ] 独立设计 OAuth credential lifecycle。
- [ ] 确认 OpenAI、Anthropic 是否提供并允许 Halro 这种网关使用的授权契约。
- [ ] 实现授权回调/device flow、token refresh、撤销与 workspace 选择。
- [ ] 在官方授权边界未确认前，这两个 Offering 的可达 profile 数保持为 0，因而不出现在元数据里。

---

## 10. 测试计划

### 10.1 Domain 与 Admin API

- 每个 `AccessSurface` 在 surface 表中恰好一行，且 `surfaceRow.Type == profileRow.Type`；
- 每个 Profile（含 withheld）可解析出存在且同 provider type 的 Offering；
- `(type, offering, region)` 对 `RegionScopeFixed` 的 surface 唯一解析；
- 可达 profile 数为 0 的 Offering 不出现在 Admin metadata；
- default profile 可解析 Offering；
- Profile 的 surface/scheme 与 credential 保存匹配；
- Offering/Region 只在 surface 表声明，`ProviderInstance` / `Credential` 上不存第二份；
- 每个可达 Offering 都有中英文文案；`DocumentationURL` 为空只允许出现在白名单里的
  `openai-compatible.self-declared`；
- Admin metadata 完整列出所有可达 Offering/Profile；
- 凭据创建缺 `access_surface`/`scheme` 时返回具名 400（回退路径已删除）；
- offering/profile 不匹配返回具名 400。

### 10.2 前端

- 单 Offering 服务商不展示多余选择；
- 多 Offering 服务商展示服务端返回的选项；
- `region_scope=fixed` 渲染 surface 选择，`by_endpoint` 渲染 host 选择并写入 bound URL，
  `none` 不渲染地域控件；
- 未命中 `region_hosts` 的自定义 endpoint 显示"地域未知"且**可以保存**；
- 更换 Offering/Region 清空不兼容凭据和 Profile；轮换时产品身份为只读；
- Provider 连接只列出与凭据匹配的 Profile；
- BigModel Global 保存 exact global profile；
- 模型部署列表不出现 Offering ID；
- deployment provider 选项显示 Offering/Region；
- 使用限制提示可访问、可键盘操作且中英文完整；
- provider profile golden 与服务端保持一致；
- 断言 `ProvidersPage.tsx` 不再包含 provider type 字面量分支（§4.2 列出的四类）。

### 10.3 Adapter 与兼容性

- general/coding path prefix 精确——**这条必须是显式断言**：仓库契约
  ([Adding a provider platform](../contracts/adding-a-platform.md) 的 "Steps with no mechanical
  guard") 已记录，`profileOperationTable` 里写错或互换 primitive 时整棵树保持绿，platform 自己的
  wiring test 也读不到那张表。Coding profile 从 general 复制绑定正是这个错误的高发形态，因此至少
  要断言两个 profile 构造出的 adapter 所寻址的路径不同；
- 同 host 不因 host 相同合并 surface；
- subscription key 不会发送到 general path；
- general key 不会发送到 subscription path；
- `/models` 响应使用真实 shape；
- 请求字段在 provider I/O 前拒绝或转换；
- response/tool arguments/stream usage 不静默丢失；
- 429 业务码分类保留可操作信息；
- fallback 不跨 Offering，除非 Route 显式配置多个 Deployment，且归因可分辨 Offering。

### 10.4 推送前 gate

按仓库验证政策（`AGENTS.md`）执行一次，形状与 `CLAUDE.md` 一致：

```bash
go test ./...
go vet ./...
cd web && npm ci --ignore-scripts && npm run typecheck && npm test -- --run && npm run build && cd ..
git diff --exit-code -- internal/webui/dist
```

涉及 usage schema 的改动另跑 `./bin/halro usage verify` 与 `./bin/halro doctor`，对真实
`data/` 目录验证旧分区仍可读。真实订阅 smoke 默认 skip，只在操作者显式提供专用测试凭据时运行。

---

## 11. 验收标准

满足以下条件才算首期完成：

1. Offering/Region/Profile 全部由服务端权威元数据驱动；`ProvidersPage.tsx` 不再含 §4.2 列出的
   四类 provider 私有枚举。
2. Offering 与 Region 只在 surface 表声明一次，凭据与连接均可无歧义派生出产品身份。
3. 元数据侧无需数据迁移；usage schema 的版本推进在 min-readable 范围内，旧分区可读。
4. 控制台可以明确创建 BigModel CN/Global General API 连接。
5. 控制台可以明确创建 BigModel CN/Global Coding Plan 连接。
6. 凭据创建缺 surface/scheme、或 credential 与 profile 的 surface/scheme 不一致时，在保存前或
   保存时具名拒绝；endpoint 检查按 §2.6 执行——有 profile 级 endpoint 规则的 surface 拒绝，
   其余提示地域未知但不阻断。
7. `halro doctor` 能报出 §8.3 的错配凭据，运维文档给出处置步骤。
8. 模型部署只保存真实模型 ID，不出现 plan/Offering 伪模型。
9. General 与 Coding 的模型列表、能力检测、缓存和错误证据完全隔离。
10. Coding Plan Chat unary、stream、usage 和至少一个额度错误经过真实账号验证。
11. 未验证能力保持 unknown，不从 General API profile 或模型名称继承。
12. 订阅计价形态已决策：要么有等效单价，要么控制台在创建时说明"该项目成本治理将被关闭、预算不再
    拦截"，且 operator guide 记录之。
13. 控制台展示官方使用范围提示，确认按 `(credential, profile)` 粒度写入审计日志。
14. 完整 frontend gate、Go suite 与 bundle drift check 通过。

---

## 12. 非目标

- 不把所有网页会员订阅自动变成 Halro 可调用的 Provider。
- 不绕过上游对客户端、工具、场景或公平使用的限制。
- 不通过抓取浏览器 cookie、CLI 本地凭据或未公开 endpoint 接入订阅。
- 不把 Offering 当作模型能力或模型别名。
- 不假设订阅调用"免费"，也不把未知成本记为零。
- 不在一个 Provider Connection 内自动从订阅额度切到按量余额。
- 不在首期实现通用 OAuth 框架、订阅用量 dashboard 或自动续费。
- 不把 `BaseURLTemplate` 从 prefill 改成 bound：那是独立的设计决定，会影响企业代理与私有入口。

---

## 13. 开放问题

实施前仍需回答：

1. BigModel Coding Plan 的 `/models` 是否真实存在，国内与海外 response 是否一致？
2. Z.AI 海外站的 coding path prefix 是否也是 `/api/coding/paas/v4`？
3. BigModel 团队套餐 Key 是否需要独立于个人 Coding Plan 的 Credential Scheme？
4. Kimi Code 当前允许哪些第三方 agent，Halro 作为中间网关是否符合其使用范围？
5. MiniMax Token Plan 的 `sk-cp` Key 支持哪些 endpoint 和 wire profile？官方正式产品名是什么？
6. 订阅额度没有货币单价时，成本报表显示 unknown、额度单位，还是二者并列？（"关闭成本治理"这个
   前置条件已在 §7.3 定死，此问只关乎展示与后续 allowance dashboard。）
7. OAuth Offering 是否应属于 Provider Credential，还是独立的 Account Connection 资源？这一问与
   §7.2 的 refresh/revision 语义是同一个决定的两面。
8. 企业合同、预置吞吐与云市场转售产品是否复用 Offering，还是另设 Commercial Contract 层？

已在本轮定案、不再作为开放问题的：Offering/Region 的挂载层级（§2.3）、地域的两种形态（§2.6）、
使用范围确认的粒度（§7.1）、旧请求形状的兼容策略（§8.2）。

这些问题不阻塞阶段 1 的通用元数据与 UI 重构，但会阻塞对应 Offering 变成可达 Profile。

---

## 14. 实施状态与与本方案的差异

阶段 1 已实施（清单见 §9）。实施过程中有五处与方案原文不同，理由记在这里，方案正文已同步：

1. **凭据创建不是"一律显式"，而是"有歧义才拒绝"。**原文说删除默认回退。实际按可达
   `(surface, scheme)` 的个数分流：只有一个时解析，两个及以上时具名拒绝。理由是仓库自己的原则
   ——"只有一个答案的问题不问"——写在控制台既有的 Bedrock 注释里；要求每个调用方写出
   `openai-api` + `bearer.static` 是没有决策的仪式，而 BigModel 那一处正是需要拒绝的地方。
2. **Offering 表只有三个字段。**`RequiresUsageWarning` 与 `DocumentationURL` 推迟到第一个订阅
   Offering，理由见 §2.2：现在没有真值，且文档链接是按 `(offering, region)` 的。
3. **候选 Offering 不注册。**`openai.codex-subscription` / `anthropic.claude-subscription` 要等到
   有 surface 才进表，否则不变量测试要为它们写例外（§2.1）。
4. **`Hosts` 挂在所有 surface 上，不只 by_endpoint。**`fixed` 的 surface 也列 host，供
   `SurfaceForEndpoint` 支撑 doctor 的存量检查（§2.6、§8.3）。
5. **anthropic-beta 的接受条件收窄到 profile。**写路径原先按 surface 判断，比事实宽；改用与控制台
   同一个 `domain.ProfileSendsAnthropicBetas`（§4.2）。这是本阶段唯一的行为收窄，影响的组合
   （Mantle 上锚在 OpenAI profile 的连接携带 beta token）控制台从未产生过。

尚未实施、且**不能**由本仓库单独完成的部分：

- **阶段 0**：每个订阅产品的真实账号证据。需要真实的 GLM Coding Plan / Kimi Code / MiniMax Token
  Plan 订阅，会消耗套餐额度，且 §5 的外链仍全部标注"未复核"。
- **阶段 2/3**：BigModel Coding Plan、Kimi Code、MiniMax Token Plan 的 surface 与 profile。按
  [Adding a provider platform](../contracts/adding-a-platform.md)，一个 profile 要带 fixture、
  能力证据和 endpoint manifest；没有阶段 0 的证据就只能凭猜测申报能力，而那正是 §5.1 明令禁止的。
- **阶段 4**：OAuth 型订阅，还阻塞在 §7.2 的刷新语义与官方授权边界上。

阶段 1 之外、方案里提到但本次未做的小项：

- §4.3 的"服务商连接选项增加 Offering 与地域 badge"（模型部署页）——它不在阶段 1 清单里，且
  当前每个 type 只有一个 Offering，badge 只会显示地域；随阶段 2 一起做更省一次改动。
- §7.3 的 Offering 进 usage attribution 与 `parquetSchemaVersion` 推进——按方案属于阶段 2，
  因为在只有一个 Offering 的情况下该维度恒为常量。
