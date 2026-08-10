# Halro DLP（脱敏与数据防泄漏）升级方案

状态：**提案，待评审**  
更新日期：2026-08-10  
范围：`internal/domain`、`internal/redaction`、`internal/contentscan`、`internal/gateway`、`internal/app/admin_redaction.go`、`web/src/pages/RedactionPoliciesSection.tsx`、Project 绑定、审计与指标

---

## 0. 决策摘要

Halro 当前的 Redaction Policy 已经具备可靠的数据面基础，但控制面仍把“敏感数据识别、规则组合、命中处置、项目绑定”压在同一个对象中。规则较少时这很直接；当检测器、策略和项目数量增长后，会产生重复配置、误报难调、变更影响不透明以及规则无法复用的问题。

本方案将 DLP 拆成四个层级：

```text
敏感数据标识符（识别什么）
        ↓
检测配置文件（把哪些标识符归为一组）
        ↓
DLP 策略（在什么条件下如何处置）
        ↓
Project 绑定（在哪里生效）
        ↓
不可变编译快照（数据面实际执行版本）
```

关键决策：

1. 保留并强化当前 `internal/redaction` 数据面；升级重点是控制面模型、误报治理、可解释性和发布流程。
2. 内置检测器、RE2、自定义字典都只是“敏感数据标识符”的不同实现，不再与动作直接耦合。
3. 第一阶段仍保持“每个 Project 最多绑定一个生效 DLP 策略”，通过策略内引用多个检测配置文件获得复用能力；暂不引入多个策略叠加造成的冲突语义。
4. 数据面改为“先在不可变原文上检测，再统一裁决和变换”的两阶段执行，避免前一条掩码规则改变文本后影响后一条规则。
5. 不可关闭的 Secret 基线继续独立于租户策略执行：入站 Secret 仍拒绝，出站 Secret 仍强制清理；管理员不能通过普通 DLP 配置绕过。
6. 严格模式仍是出站强制防泄漏的默认值。有界流式只允许编译器能证明最大匹配宽度的规则；无法证明的规则只能严格缓冲或仅检测。
7. 测试、事件、日志、审计和指标都不得保存原始 Prompt、响应、命中原文、凭据或可逆证据。
8. Halro 尚未正式发布 1.0.0。领域模型切换时按仓库约定原位修正，不保留并行的旧结构或 `V2` API；持久化结构变化需要明确要求重新初始化数据目录。

---

## 1. 当前基线

### 1.1 已实现能力

当前 `RedactionPolicy` 包含 0–128 条规则，每条规则具有名称、检测类型、作用方向、动作、优先级和启用状态。

| 能力 | 当前实现 |
| --- | --- |
| 检测类型 | `builtin`、`regex`（Go RE2）、`dictionary` |
| 内置 PII | 中国手机号、邮箱、中国身份证、银行卡候选 |
| 内置 Secret | Gateway Key、OpenAI/Anthropic/Google Key、AWS Access Key、Bearer Token、私钥 |
| 方向 | `inbound`、`outbound` |
| 动作 | `detect_only`、`mask`、`replace`、`reject` |
| 流式模式 | `strict`、`bounded_stream`、`detect_only_stream` |
| 有界证明 | 编译期计算 `computed_max_match_bytes`，有界流式最大 4096 bytes |
| 结构化内容 | 递归处理 JSON 字符串和 Tool arguments；不完整流式 Tool JSON 采取保守裁决 |
| 固有防线 | Secret 类别作为 mandatory baseline，不受 Project 策略开关控制 |
| 安全测试 | 只返回规则 ID、类别、动作和字段，不返回输入原文 |
| 热加载 | 策略保存后编译并替换运行时快照 |

这些能力必须被视为升级起点，而不是待替换的原型。

### 1.2 当前模型的主要问题

1. **识别与处置耦合。** 同一个“手机号”检测逻辑要在多个策略重复配置，无法统一修正误报。
2. **缺少上下文证据。** 自定义规则只有正则或字典，不能表达“主模式 + 邻近关键词 + 忽略词 + 校验器”。
3. **缺少阈值。** 单次匹配即触发动作，无法表达“命中至少 N 次”或按置信度分级处置。
4. **缺少例外治理。** 测试号码、公开邮箱、示例 Key 等非敏感内容无法通过可审计的 Allow List 排除。
5. **内置标识符面向实现。** UI 暴露 `china_phone` 等内部代码，管理员难以理解检测范围、校验方式和流式限制。
6. **执行过程是逐规则修改。** 当前规则按“拒绝优先、其余优先级从高到低”逐条处理；早先规则的替换结果可能改变后续规则的输入。
7. **命中计数能力有限。** 内存计数不能支撑按策略版本、规则、方向、动作和发布阶段进行运营分析，也不适合重启后的趋势判断。
8. **缺少发布生命周期。** 只有启用/禁用，无法明确区分草稿、观察、强制执行和回滚。
9. **策略绑定影响不够透明。** 保存前能看到绑定数量，但缺少精确 Project 清单、当前生效 revision 和变更影响预览。
10. **“脱敏策略”名称偏窄。** 系统同时执行检测、掩码、替换和拒绝，实质已经是 DLP；控制面术语需要明确，但不应把普通用户暴露给不必要的行业缩写。

---

## 2. 目标与非目标

### 2.1 目标

1. 支持内置、正则、字典和上下文组合检测，并能持续增加检测能力而不复制策略。
2. 把“识别什么”和“命中后做什么”解耦，让标识符与配置文件可复用、可测试、可版本化。
3. 对每次命中提供可解释但不泄漏原文的元数据：策略、revision、规则、标识符、方向、位置类别、动作和裁决原因。
4. 通过 Allow List、邻近关键词、校验器、置信度和命中阈值降低误报。
5. 提供“草稿 → 观察 → 强制执行”的安全发布路径，并能回滚到上一已编译 revision。
6. 保证非流式、SSE、Tool Call、Tool Result 和递归 JSON 路径遵循同一裁决语义。
7. 保持有界内存、确定性执行、热路径无外部依赖、失败关闭和 Secret 不落盘。
8. 为后续 Exact Data Match（EDM）和组织级数据集预留清晰扩展点，但不在首期引入可逆敏感数据存储。

### 2.2 非目标

- 不在首期加入 OCR、图片理解、音频转写或通用机器学习分类器。
- 不调用外部 SaaS DLP 服务执行在线检测；Provider 请求热路径不得依赖外部控制面。
- 不持久化原始 Prompt、响应、测试正文或命中片段作为“证据”。
- 不提供按请求 Header 绕过 DLP 的能力。
- 不提供可逆 Tokenization、格式保持加密或解密服务；这需要独立密钥权限与审计设计。
- 不在首期支持管理员或终端用户对一次阻断进行临时 Override。
- 不把 DLP 当作内容安全、越狱检测、恶意软件扫描或授权系统的替代品。
- 不允许自定义脚本、插件或任意代码在 Gateway 进程内作为检测器运行。

---

## 3. 设计原则与安全不变量

### 3.1 检测与处置分离

敏感数据标识符只回答“是否检测到某类数据、置信度和位置是什么”；策略规则才回答“在某个方向和上下文中命中后如何处置”。同一个标识符可被多个配置文件和策略复用。

### 3.2 原文只存在于当前请求内存

- 管理 API、审计、日志、指标、Usage、WAL 和 bbolt 都不得持久化输入正文或匹配片段。
- 测试 API 接收样本后必须在请求结束前释放引用，不进入历史记录。
- 安全事件只记录稳定 ID、版本、枚举、数量、长度区间和不可逆摘要；默认不记录字段路径中的用户自定义键名。
- Panic、错误和诊断不得包含正则捕获组或命中值。

### 3.3 Mandatory Secret 基线不可降级

当前密钥检测基线继续早于 Project DLP 策略执行：

- 入站命中密钥：拒绝进入 Provider。
- 出站命中一般密钥：替换为固定占位符。
- 出站命中私钥：拒绝返回。
- 普通策略不能关闭、降级、Allow List 或覆盖 mandatory detector。
- Mandatory detector 的规则集或动作变化属于安全契约变更，需要单独 ADR 和回归证据。

### 3.4 确定性和失败关闭

- 相同编译快照和相同输入必须得到相同发现、裁决和输出。
- 标识符、配置文件或策略引用缺失时，绑定 Project 不得继续放行流量。
- 编译失败不得替换当前生效快照。
- 运行时快照必须完整生成后一次性原子替换。
- 未知动作、范围、标识符类型或版本拒绝加载，不做默认放行。

### 3.5 有界资源

- 正则继续使用 RE2，不支持回溯型正则引擎。
- 字典、关键词、忽略词、规则数、Profile 数、匹配数和事件大小都有硬上限。
- 流式 tail 大小由编译器计算并设置总上限，不允许管理员手工声称一个无界规则是安全有界的。
- 达到单请求最大 findings 数时采取保守裁决并设置 `findings_truncated=true`，不能继续无界收集。

---

## 4. 目标领域模型

### 4.1 Sensitive Data Identifier：敏感数据标识符

标识符是可复用、可测试的检测定义，不包含掩码或拒绝动作。

```go
type SensitiveDataIdentifier struct {
    ID          string
    Name        string
    Description string
    Kind        string // builtin | regex | dictionary | composite | exact_match

    BuiltinID   string
    Pattern     string
    Dictionary  []string

    SupportingKeywords []string
    IgnoreWords        []string
    ProximityBytes     int
    Validator          string // none | luhn | china_id_checksum | ...，只允许注册表值
    CaseSensitive      bool

    DefaultSeverity   string // low | medium | high | critical
    Confidence         string // low | medium | high；编译结果，不由客户端随意伪造
    Enabled            bool
    Revision           uint64
}
```

约束：

- `builtin` 只能引用编译进二进制并带版本的 Catalog 条目。
- `regex` 只接受 RE2，并在保存时编译。
- `dictionary` 首期保持最多 1024 项、每项 1–256 bytes；扩大规模前先更换专用匹配结构并做内存基线。
- `composite` 表达主检测器与辅助关键词/忽略词/邻近距离，不允许运行任意布尔脚本。
- `exact_match` 只作为未来扩展；数据集只能存 keyed digest 或经过独立评审的安全索引，禁止保存原始敏感列。
- 系统内置标识符不可编辑；管理员可以复制为自定义标识符后调整上下文条件。

### 4.2 Detection Profile：检测配置文件

Profile 把多个标识符组合成业务语义，例如“中国个人信息”“支付信息”“模型与云凭据”。

```go
type DetectionProfile struct {
    ID          string
    Name        string
    Description string
    Entries     []DetectionProfileEntry
    Revision    uint64
}

type DetectionProfileEntry struct {
    IdentifierID       string
    MinimumConfidence  string
    MinimumOccurrences int
    SeverityOverride   string
    Enabled            bool
}
```

约束：

- Profile 不包含方向和动作，因此能被入站/出站策略复用。
- `MinimumOccurrences` 在一次独立检查对象内计算；不能跨请求累计后阻断当前请求。
- Profile revision 变化不会静默改变已生效数据面；必须生成并发布新的 Policy snapshot。
- 引用已归档或不可用标识符时，Profile 编译失败。

### 4.3 DLP Policy：策略与处置规则

```go
type DLPPolicy struct {
    ID          string
    Name        string
    Description string
    State       string // draft | observe | enforce | disabled
    StreamMode  string // strict | bounded_stream | detect_only_stream
    Rules       []DLPPolicyRule
    Revision    uint64
}

type DLPPolicyRule struct {
    ID          string
    Name        string
    ProfileID   string
    Scopes      []string // inbound | outbound
    Locations   []string // prompt | system | tool_arguments | tool_result | response
    Action      string   // detect_only | mask | replace | reject
    TransformID string
    Priority    int
    Stop        bool
    Enabled     bool
}
```

首期约束：

- `observe` 状态强制把普通策略规则动作降为 `detect_only`，但 mandatory baseline 不受影响。
- `enforce` 才执行 mask、replace 和 reject。
- `disabled` 不能被启用 Project 引用。
- 一个 Project 只引用一个 Policy ID；Policy 内可引用多个 Profile。
- `Locations` 首期至少支持 Prompt/Tool arguments/Response，并明确映射当前 inbound/outbound 数据路径；未识别位置不得静默跳过。
- `Stop` 只停止同一策略中更低优先级的普通规则，不影响 mandatory baseline。

### 4.4 Transform：变换模板

掩码不应只有一个隐式实现。变换模板定义如何处理已经裁决的 span：

| 变换 | 示例 | 首期 |
| --- | --- | --- |
| 固定占位符 | `[REDACTED]`、`[PHONE]` | 是 |
| 保留后 N 位 | `••••5088` | 是 |
| 邮箱域名保留 | `•••@example.com` | 是 |
| 完全删除 | 空字符串 | 是，但 UI 必须提示结构风险 |
| 固定替换文本 | 管理员配置值 | 是，长度受限 |
| keyed hash | 稳定不可逆标识 | 后续，需密钥与关联风险评审 |
| Tokenization / FPE | 可逆或保格式替换 | 非首期 |

变换模板不能插入原文捕获组，避免管理员把敏感内容通过替换格式重新输出。

### 4.5 Binding 与不可变编译快照

Project 保存 `DLPPolicyID`，运行时不直接追踪可变 Profile/Identifier。发布策略时生成不可变快照：

```text
Policy revision
  + Profile revisions
  + Identifier revisions
  + Built-in catalog version
  + Compiler version
  + Stream bound proof
  = Compiled Policy Snapshot + digest
```

快照至少记录所有依赖 revision 和 SHA-256 digest。数据面只读取已完整编译的快照；控制面修改 Identifier 或 Profile 后，UI 显示“有未发布的依赖更新”，由管理员显式重新编译发布。

---

## 5. 检测与裁决语义

### 5.1 两阶段执行

现有逐规则修改方式升级为：

```text
不可变输入
  → 所有适用检测器扫描
  → 生成 findings（仅内存）
  → 上下文、Allow List、阈值与置信度过滤
  → 冲突裁决
  → reject 或统一生成变换计划
  → 从后向前一次性应用 span 变换
```

这样可以保证：

- 一条掩码规则不会使另一条规则漏检。
- 安全测试与实际执行使用同一 findings 和裁决代码。
- 重叠匹配有稳定、可解释的结果。
- 变换不会因 map 遍历顺序或规则保存顺序改变。

### 5.2 发现结构

Finding 只在请求内存中保存完整 span；对外事件不包含原文：

```go
type Finding struct {
    PolicyID, RuleID, ProfileID, IdentifierID string
    PolicyRevision                            uint64
    Scope, Location, Action, Severity          string
    Confidence                                string
    StartByte, EndByte                         int // 仅当前处理过程
    Occurrence                                 int
}
```

### 5.3 冲突规则

当多个规则命中相同或重叠 span 时，按以下顺序裁决：

1. Mandatory baseline 永远优先。
2. `reject` 高于任何普通变换。
3. `mask` / `replace` 高于 `detect_only`。
4. 动作相同时按 `priority` 从高到低。
5. 优先级相同时选择覆盖范围更大的 finding。
6. 仍相同时按稳定的 Rule ID 字典序裁决。

只要任一适用 finding 最终裁决为 reject，整个当前安全边界拒绝；不能先发送部分响应再决定拒绝。

### 5.4 上下文、置信度和阈值

置信度来自可审计的检测证据，不是模型概率：

| 证据 | 典型影响 |
| --- | --- |
| 仅正则主模式 | 低或中 |
| 主模式 + 语义校验（Luhn/身份证校验） | 提升 |
| 主模式 + 邻近业务关键词 | 提升 |
| 命中 Ignore Word / Allow List | 排除 |
| 结构化字段位置符合预期 | 提升 |

首期只提供 `low / medium / high` 三档，避免展示虚假的精确百分比。Profile 决定最低置信度和最少命中次数，Policy 决定动作。

### 5.5 Allow List

Allow List 是独立、可复用、可审计对象，不混入普通 Dictionary detector：

- 支持精确文本和受限 RE2 两种形式。
- 只作用于明确选择的 Identifier/Profile。
- 不得作用于 mandatory Secret baseline。
- UI 必须显示被多少 Profile/Policy 引用。
- 测试结果要解释“原始检测命中，但被 Allow List 排除”，但不显示命中原文。

---

## 6. 流式与结构化内容

### 6.1 三种流式模式继续保留

| 模式 | 防泄漏保证 | 行为 |
| --- | --- | --- |
| `strict` | 最强 | 完整响应通过检测与变换后才向客户端释放；会增加首 Token 延迟 |
| `bounded_stream` | 对可证明有限宽度的规则提供强制防护 | 保留编译器计算的有限 tail，跨 chunk 匹配后再安全释放 |
| `detect_only_stream` | 只提供发现，不承诺阻止泄漏 | 不允许普通规则执行 mask/replace/reject |

约束：

- 出站 enforce 默认使用 `strict`。
- `bounded_stream` 只允许所有强制动作规则的最大匹配宽度可证明且不超过上限。
- 上下文邻近距离计入最大流式窗口。
- Exact Match、无界正则或未来 ML 检测器默认不允许进入 bounded enforcement。
- 一旦响应字节已对客户端可见，不允许切换 Provider、重新执行规则或把后续 reject 伪装成完整阻断。

### 6.2 AI 消息位置

DLP 不应把整段 JSON 当作无差别文本。适配层需要标注有限位置枚举：

- `prompt.user`
- `prompt.system`
- `tool.arguments`
- `tool.result`
- `response.text`
- `response.tool_arguments`
- `resource.metadata`

未知或新增 Provider 字段必须先进入协议/Profile 评审；不能因为字段不在旧枚举中而绕过 outbound DLP。

### 6.3 Tool arguments

- 完整 JSON：递归处理字符串值，键名默认不纳入普通内容检测。
- 流式不完整 JSON：只有能证明不会破坏语法的变换才允许；否则 fail closed。
- 安全事件只记录 `tool.arguments` 位置，不记录工具名、字段名或参数内容，避免高基数和业务信息泄漏。

---

## 7. Admin 信息架构与交互方案

### 7.1 页面结构

安全策略页的“脱敏策略”逐步升级为“数据防泄漏（DLP）”，内部提供三个子视图：

1. **策略**：发布状态、流式保证、规则摘要、Project 绑定、最近命中趋势。
2. **检测配置文件**：PII、支付、凭据等可复用组合。
3. **敏感数据标识符**：内置和自定义检测器、版本、测试与引用关系。

产品文案仍优先使用中文业务术语，DLP 作为括号中的行业名，不要求管理员理解内部对象名。

### 7.2 标识符编辑器

将当前“类型”改为“检测方式”，使用清晰的互斥选择：

- 内置检测器
- RE2 正则
- 字典
- 组合检测（后续）
- 精确数据匹配（后续）

内置检测器显示：

```text
中国大陆手机号
11 位移动号码，支持可选 +86；适用于文本内容
标识符：china_phone · 内置 · 支持有界流式
```

内部代码只作为次要技术信息，不再作为主选项文字。每个检测器同时展示：检测说明、校验方式、默认置信度、最大匹配宽度、支持的流式模式和可能误报提示。

### 7.3 策略规则编辑器

规则详情按决策顺序组织：

1. 规则名称。
2. 检测配置文件（识别什么）。
3. 检查方向和消息位置（在哪里）。
4. 发布状态下的处置动作（怎么办）。
5. 优先级与是否停止更低优先级规则。

左侧摘要行显示：

```text
出站手机号保护
中国个人信息 · 出站/模型响应
掩码 · 优先级 100 · 强制执行
```

### 7.4 测试与解释

安全测试返回：

- 命中规则名称，而不是内置类别代码。
- 标识符显示名称和内部代码。
- 置信度、命中次数、最终动作。
- 是否被上下文条件、阈值或 Allow List 排除。
- 是否满足当前流式模式的强制防护条件。

不返回：

- 输入原文。
- 命中片段。
- 前后文。
- 可推断 Secret 的捕获组。

测试界面允许管理员在本地准备“应命中/不应命中”样本逐个提交，但首期不保存样本集。未来若提供持久测试集，只允许合成数据，并且需要独立的加密、访问控制与保留期设计。

### 7.5 发布与变更影响

保存草稿不直接改变在线流量。发布前展示：

- 将生效的 Policy revision 和依赖 digest。
- 绑定 Project 清单与启用状态。
- 新增、删除、动作升级和流式保证变化。
- 是否从 observe 进入 enforce。
- 是否包含新增 reject 规则。
- 是否需要完整响应缓冲以及预期延迟影响。

从 observe 切换到 enforce、降低置信度阈值、增加 reject、关闭策略或修改 mandatory 边界都属于高风险变更，要求 recent re-authentication，并写入不含敏感正文的 Audit metadata。

---

## 8. API 与持久化演进

### 8.1 目标 Admin API

```text
GET/POST       /admin/api/v1/sensitive-data-identifiers
GET/PUT/DELETE /admin/api/v1/sensitive-data-identifiers/{id}
POST           /admin/api/v1/sensitive-data-identifiers/{id}/test

GET/POST       /admin/api/v1/dlp-profiles
GET/PUT/DELETE /admin/api/v1/dlp-profiles/{id}

GET/POST       /admin/api/v1/dlp-policies
GET/PUT/DELETE /admin/api/v1/dlp-policies/{id}
POST           /admin/api/v1/dlp-policies/{id}/compile
POST           /admin/api/v1/dlp-policies/{id}/test
POST           /admin/api/v1/dlp-policies/{id}/publish
POST           /admin/api/v1/dlp-policies/{id}/rollback
```

所有修改继续使用 revision/ETag 乐观锁。发布、回滚、关闭生效策略和删除被引用对象必须执行引用完整性检查；安全能力削弱需要 step-up。

### 8.2 预 1.0 数据切换

本方案不建议长期保留 `RedactionRule` 和 `DLPPolicyRule` 两套结构。进入“标识符/Profile 解耦”阶段时：

1. 原位替换领域对象和 Admin API。
2. `Project.RedactionPolicyID` 改为语义正确的 `DLPPolicyID`。
3. 更新 bbolt bucket/schema version。
4. 明确要求开发/测试实例重新初始化数据目录。
5. 不提供双写、双读、旧字段别名或 `V2` 类型。

如果 1.0.0 在该阶段前发布，则必须先另写迁移 ADR；不能直接采用上述重初始化策略。

### 8.3 编译事务

发布过程必须满足：

1. 在事务外读取并复制完整依赖快照。
2. 编译 Identifier、Profile、Policy 和流式边界。
3. 生成 digest 和有限的编译诊断。
4. 在 revision 未变化的前提下提交发布记录。
5. 完整构建新的内存 Engine。
6. 原子替换运行时快照。
7. 若任一步失败，旧快照继续生效，控制面明确报告未发布。

不能先写入“已发布”再尝试热加载。

---

## 9. 事件、审计与可观测性

### 9.1 DLP 安全事件

首期事件只保留有界元数据：

```json
{
  "event_type": "dlp.finding",
  "policy_id": "dlp_...",
  "policy_revision": 4,
  "rule_id": "dlr_...",
  "profile_id": "dpf_...",
  "identifier_id": "sdi_...",
  "scope": "outbound",
  "location": "response.text",
  "action": "mask",
  "severity": "high",
  "confidence": "high",
  "occurrences_bucket": "1",
  "findings_truncated": false
}
```

事件不得包含原文、span、字段名、正则、字典内容、替换前值或模型输出。若事件要进入 Alert/SIEM，必须先定义稳定 schema 和去重语义。

### 9.2 指标

建议提供低基数指标：

- `halro_dlp_requests_total{scope,decision}`
- `halro_dlp_actions_total{scope,action,severity}`
- `halro_dlp_compile_total{result}`
- `halro_dlp_stream_mode_total{mode}`
- `halro_dlp_findings_truncated_total{scope}`
- `halro_dlp_processing_duration_seconds{scope,mode}`

不得把 Policy ID、Rule ID、Project ID、Identifier ID、模型 ID 或用户字段作为 Prometheus label。按具体对象的分析由受访问控制的 Admin 聚合读取完成。

### 9.3 审计

审计覆盖创建、修改、发布、回滚、启用/禁用、绑定、Allow List 变更和测试动作。Audit metadata 只包含对象 ID、revision、状态变化、依赖 digest、绑定数量和风险分类；不保存规则内容快照和测试正文。

---

## 10. 分阶段交付计划

### Phase 0：现有体验与契约固化

目标：在不改变领域模型的前提下消除当前理解成本，并为后续重构建立回归基线。

- [ ] 将“类型”改为“检测方式”。
- [ ] 内置类别显示中文名、说明、校验方式、流式支持；内部代码降为次要信息。
- [ ] 补全规则名称、类别名称、Rule ID 的显示边界。
- [ ] 安全测试同时覆盖应命中与不应命中的合成样本。
- [ ] 固化当前 mandatory Secret 行为、执行顺序、跨 chunk 和 Tool arguments 契约测试。
- [ ] 为当前 API/领域模型生成基线 fixture，作为后续原位切换的反向验证输入。

验收标准：管理员能明确知道当前可选的不只有内置类别；测试结果始终以规则名称为主，且不泄漏正文。

### Phase 1：标识符与 Profile 解耦

目标：建立目标领域模型和可复用检测资产。

- [ ] 新增 Sensitive Data Identifier、Detection Profile、DLP Policy、Compiled Snapshot。
- [ ] 内置 Catalog 带稳定 ID、显示名、说明、版本、默认严重性和能力元数据。
- [ ] Project 改为绑定一个 DLP Policy。
- [ ] 实现依赖 revision、digest、原子编译发布和失败保留旧快照。
- [ ] 控制台增加策略/Profile/标识符三个子视图。
- [ ] 执行预 1.0 schema cut，并在发布说明中明确需要重新初始化数据目录。

验收标准：修改一个共享标识符不会静默改变在线策略；重新发布后所有引用策略使用可证明的依赖快照。

### Phase 2：上下文检测与确定性裁决

目标：降低误报并消除逐规则修改带来的顺序副作用。

- [ ] 实现两阶段 scan/decide/transform 管线。
- [ ] 实现邻近关键词、Ignore Words、Allow List、校验器注册表。
- [ ] 实现 low/medium/high 置信度、最小命中次数和严重性覆盖。
- [ ] 实现重叠 span 的稳定冲突规则。
- [ ] 将上下文窗口纳入流式最大宽度证明。
- [ ] 安全测试解释命中、排除、阈值和最终裁决。

验收标准：规则保存顺序不影响结果；同一快照与输入得到逐字节一致输出；上下文/Allow List 行为有 property test 和 fuzz 覆盖。

### Phase 3：观察、发布、回滚与运营闭环

目标：让管理员能安全地从检测过渡到强制防护。

- [ ] 实现 `draft / observe / enforce / disabled` 生命周期。
- [ ] 保存草稿与发布在线版本解耦。
- [ ] 实现变更影响预览、step-up、发布与回滚。
- [ ] 增加低基数指标、受控聚合和隐私安全事件。
- [ ] 提供按规则/标识符的命中趋势和误报反馈计数，不存正文。
- [ ] 增加“观察期建议”：至少覆盖业务高峰和典型调用类型后再 enforce；不自动替管理员启用阻断。

验收标准：草稿编辑不影响在线流量；observe 不执行普通强制动作；回滚能恢复上一完整编译快照；所有高风险操作可审计。

### Phase 4：Exact Data Match 与企业级扩展

目标：支持组织特定敏感数据，而不把原始数据集引入 Gateway 热路径或普通存储。

- [ ] 单独完成 EDM 威胁模型、密钥派生、碰撞、枚举攻击和备份边界评审。
- [ ] 只保存 keyed digest/安全索引，不保存原始数据单元格。
- [ ] 数据集构建离线化，发布不可变版本和 digest。
- [ ] 评估大型字典的专用匹配结构与内存上限。
- [ ] 评估结构化字段选择器和数据标签集成。
- [ ] OCR/图片/音频/ML 分类器分别立项，不直接塞进字符串引擎。

验收标准：EDM 数据泄漏不能通过 bbolt、备份、日志、审计、指标或错误恢复；离线数据集不可用时在线策略 fail closed 或保持已验证快照，不临时降级为放行。

---

## 11. 测试与发布门禁

### 11.1 单元和契约测试

- 每个内置标识符有正样本、近似负样本、边界和 Unicode 样本。
- Luhn、身份证日期/校验位等 validator 分离测试。
- Identifier/Profile/Policy 引用与 revision 冲突测试。
- observe/enforce/disabled 状态矩阵。
- inbound/outbound 与所有支持 Location 的动作矩阵。
- 重叠 finding 的固定优先级与稳定 tie-break。
- Allow List 不得影响 mandatory Secret baseline。
- 编译失败、缺引用、未知枚举和旧 revision 全部 fail closed。

### 11.2 Property 与 fuzz

- 非重叠变换不改变未命中字节。
- 两阶段执行结果不受规则保存顺序影响。
- bounded stream 与非流式处理在相同规则下结果一致。
- 任意 chunk 切分、UTF-8 边界和 SSE 帧边界不能绕过检测。
- JSON/Tool arguments 变换后保持语法有效；无法保证时拒绝。
- 错误、Panic、审计、指标和响应元数据永不包含 seeded secret。
- 替换结果不能通过捕获组重新输出原始敏感值。

### 11.3 性能门禁

- 保留当前 strict 与 bounded stream benchmark 作为基线。
- 相同规则集下 p95 延迟或 allocations 回归超过 20% 必须解释并评审。
- 编译在控制面完成，热路径不编译正则、不构建字典、不访问 bbolt。
- 单请求扫描次数、finding 数、tail、递归深度和结构化节点数受限。
- 128 条规则与最大允许字典规模要有容量测试和内存上界。

### 11.4 发布证据

每个阶段完成时至少需要：

- `go test ./...`
- `go test -race ./...`（Engine 快照、计数或生命周期变化时）
- `go vet ./...`
- Redaction/contentscan/SSE fuzz 固定时长结果
- 前端 typecheck、测试和 production build
- Secret Canary 扫描覆盖日志、错误、bbolt、WAL、Usage、Audit 和浏览器产物
- 同一 RC commit 的性能基线与变更说明

真实 Provider 测试必须由用户明确授权；普通开发和 CI 不执行可能计费的调用。

---

## 12. 风险与缓解

| 风险 | 后果 | 缓解 |
| --- | --- | --- |
| 控制面对象过多 | 小型用户配置困难 | 提供内置 Profile 和“从模板创建策略”的普通流程；自定义 Identifier 放入高级入口 |
| 共享标识符修改影响过大 | 多策略行为意外变化 | 在线执行绑定不可变依赖 revision；修改后必须显式重新发布 |
| 上下文窗口增大 | 流式延迟和内存增加 | 编译计算总窗口、设置硬上限、UI 展示影响，不满足时只允许 strict/detect-only |
| Allow List 被滥用 | 真实敏感数据被排除 | 禁止作用于 mandatory baseline；变更 step-up、引用影响和审计 |
| 观察事件泄漏业务信息 | DLP 自身成为泄漏源 | 只保存有限枚举和 ID；不保存正文、span、字段名和自定义标签 |
| 多规则重叠结果不稳定 | 相同输入产生不同输出 | 两阶段执行、固定冲突顺序、稳定 Rule ID tie-break 和 property test |
| 严格缓冲增加首 Token 延迟 | 用户体验下降 | 保存前预览影响；允许仅入站检查或经证明的 bounded stream，但不虚假宣称防泄漏 |
| Profile/Identifier 缺失 | 策略意外 fail open | 启动校验、发布编译、Project 启用校验和数据面缺引用 fail closed |
| 过度检测导致误杀 | 生产请求被拒 | observe 生命周期、上下文/置信度/阈值、负样本测试和一键回滚 |
| EDM 可被枚举 | 散列数据集反推原文 | 独立 keyed digest 设计、访问隔离、速率与容量限制；未完成评审前不实现 |

---

## 13. 待评审决策

以下问题必须在对应阶段编码前关闭：

1. 控制台最终主名称使用“数据防泄漏（DLP）”还是继续使用“脱敏策略”，API 和领域模型仍按 DLP 语义设计。
2. `observe` 是否允许产生可选 Alert，还是只计数；无论哪种都不得保存正文。
3. 安全事件是否进入现有 Alert 通道，还是先只提供受控聚合；需要先确定事件 schema 和风暴抑制。
4. 普通自定义 Identifier 是否允许被多个 Profile 引用，还是首期只允许内置 Identifier 共享。
5. 是否需要“完全删除”变换；该动作可能破坏 JSON 内嵌文本、代码和自然语言语义。
6. Profile 变化后是否允许批量发布所有引用策略；默认建议不允许，避免大范围隐式变更。
7. Project 未来是否需要多 Policy 叠加；本方案首期明确不支持，除非先定义跨 Policy 冲突和回滚语义。
8. 1.0.0 发布时间是否早于 Phase 1 schema cut；若是，必须把持久化迁移和 API 兼容性升级为正式 ADR。

---

## 14. 建议的首个实施切片

不要直接从完整重构开始。第一个可合并切片建议只做 Phase 0，并形成后续可依赖的产品语言和测试契约：

1. 新增内置 Catalog 元数据：中文/英文显示名、说明、类别、validator、默认严重性和流式能力。
2. UI 将 `china_phone` 等代码降为次级说明，“检测方式”和“内置检测器”成为主文案。
3. 安全测试显示规则名称、检测器名称、动作和方向，并继续不回显原文。
4. 为全部内置 detector 增加正负语料和安全边界测试。
5. 写一份 Phase 1 ADR，冻结 Identifier/Profile/Policy/Snapshot 的最终持久化模型后再进行 schema cut。

这个切片风险低、对用户立即可见，同时不会提前锁死后续领域模型。

---

## 15. 参考设计

- Google Sensitive Data Protection 将检查模板与去标识化模板分离，并支持内置、自定义正则、字典和上下文检测：  
  <https://docs.cloud.google.com/sensitive-data-protection/docs/concepts-templates>  
  <https://docs.cloud.google.com/sensitive-data-protection/docs/deidentify-sensitive-data>
- Microsoft Purview Sensitive Information Type 使用主元素、辅助证据、邻近距离和置信度组合识别敏感数据：  
  <https://learn.microsoft.com/en-us/purview/sit-sensitive-information-type-learn-about>
- AWS Macie Custom Data Identifier 使用正则、关键词、Ignore Words、邻近距离和命中次数严重性；Allow List 独立管理：  
  <https://docs.aws.amazon.com/macie/latest/user/cdis-create.html>  
  <https://docs.aws.amazon.com/macie/latest/user/allow-lists-create.html>
- Cloudflare AI Gateway DLP 区分检测 Profile、请求/响应检查和 Flag/Block 动作，并明确完整响应检查对流式首 Token 延迟的影响：  
  <https://developers.cloudflare.com/ai-gateway/features/dlp/>  
  <https://developers.cloudflare.com/ai-gateway/features/dlp/set-up-dlp/>

这些产品用于校验概念边界，不意味着 Halro 应复制其云端依赖、许可模型或数据留存方式。Halro 的实现仍以本地单进程、无外部热路径依赖、失败关闭和不持久化正文为最高约束。
