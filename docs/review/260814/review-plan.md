# 配置链路评审方案：凭据 → 服务商 → 部署 → 路由 → 项目 → 网关密钥

**评审对象**：不是某个包、也不是某次改动，而是**一条链路的端到端自洽性**。运维在控制台里按
`Credential → Provider → Deployment → Route → Project → Gateway Key` 的顺序把系统配起来，这条链
上每一跳都是一次"引用 + 收窄"：后一跳引用前一跳，并在前一跳允许的范围内继续收窄。本评审要回答的
是——**这些引用与收窄是否处处成立、且与系统设计（ADR / contracts / 结构体注释所声明的意图）一致**。

**范围**：后端链路（`internal/*`）、Admin 控制台（`web/`）、真实二进制运行时行为。文档本身不作为
审查对象，只作为判定基准（发现文档与实现冲突时记录，但不专门审文档新鲜度）。

**方式**：分层递进。每一阶段有独立交付物，阶段之间可以叫停。

**产出目录**：`docs/review/260814/`。

---

## 一、判定基准

一条发现要成立，必须能指认它违反了下列某一条基准，并给出 `文件:行号`。没有基准支撑的，写成"疑问"
而不是"问题"。

| 编号 | 基准 | 来源 |
|---|---|---|
| B1 | 能力只能逐跳收窄，不能任何一跳放宽；能力过滤发生在 Provider I/O 之前，不支持的字段拒绝而非静默丢弃 | `CLAUDE.md` 架构节、`docs/contracts/provider-capabilities.md`、ADR 0019 |
| B2 | 应用侧只见 Gateway Key 与 public model alias；Provider 凭据与上游模型标识不得沿链路回流到应答、日志、指标、审计 | `CLAUDE.md` 不变式、`docs/architecture/threat-model.md` |
| B3 | 损坏 / 不可用 / 语义不明 / 陈旧的状态一律 fail-closed | `CLAUDE.md` 不变式 |
| B4 | 权威变更先校验后提交，且保全 revision / tombstone / 回滚 / 审计四项不变式 | `CLAUDE.md` `store`/`domain` 职责 |
| B5 | 预算预留必须在 Provider 请求发出**之前**持久化；结算原子释放并提交 | ADR 0018、`docs/contracts/gateway-correctness.md` |
| B6 | 资源归属：Provider Resource 的 owner 三元组（project / provider / deployment）在每次读取时校验，不得跨 Project 可达 | ADR 0021、`ProviderResource` 结构体注释 |
| B7 | 前 1.0.0 规则：错误构造就地修复，不得与替代品并存 | `CLAUDE.md` |
| B8 | 结构体与字段注释所声明的意图，本身就是契约。注释说"这个字段是 X"而代码用它做 Y，是缺陷而非风格问题 | 本仓惯例（`models.go` 大量注释承担设计说明职责） |

---

## 二、阶段 0：链路结构映射（先做，无结论）

**目的**：在做任何判断之前，先把链路的真实形状抄下来。这一步刻意不产出结论——它产出的是后续所有
判断的事实底座，避免"用自己脑子里的模型去测自己脑子里的模型"。

**交付物**：`docs/review/260814/chain-map.md`，一张逐跳表，每跳记录：

1. **承载结构体**与文件位置（已知起点：`internal/domain/models.go`）；
2. **指向上一跳的引用字段**，以及该引用是强引用还是软引用（删除上一跳会怎样）；
3. **校验点**：`Validate()` 里管什么、handler 里管什么、store 事务里管什么——三处分别管了什么是关键，
   因为跨跳约束无法在单个 `Validate()` 里表达，只能落在 handler 或事务里，那里最容易漏；
4. **生命周期**：有无 `DeletedAt` tombstone、有无过期、有无探测状态副本；
5. **运行时读取路径**：请求进来后这一跳在哪里被解析（`internal/gateway/`、`internal/auth/`）。

已知的链路骨架（阶段 0 需逐条落实到行号并补全）：

```
Credential(models.go:116)  ──CredentialID──▶ ProviderInstance(models.go:255)
                                              └─ Bindings []ProviderProfileBinding(models.go:310)
ProviderInstance ──ProviderID + BindingID/ProfileID──▶ Deployment(models.go:873)
Deployment ──DeploymentID──▶ Route(models.go:1012)
Route ──??──▶ Project(models.go:174)          ← 这一跳形状存疑，见 H1
Project ──ProjectID──▶ GatewayKey(models.go:233)
```

**边界内还需映射的旁支**（它们挂在链路上，跳过就等于默认它们没问题）：
`TokenGuardPolicy` / `RedactionPolicy`（Project 以 ID 软引用）、`DeploymentPriceVersion` 与定价
隔离（`PricingQuarantined`）、`ProviderResource`（ADR 0021 的上游孪生体）、`ModelCapabilitySnapshot`
与 `CapabilityReviewState`。

---

## 三、阶段 1：静态一致性审查（后端）

八个检查族。每一族独立走一遍链路的全部六跳，而不是逐跳走完八族——**同一个视角横穿全链，才看得出
跨跳不一致**；逐跳纵切会让每一跳看起来都自洽。

### C1 引用完整性与删除语义
- 每个软引用（`CredentialID`、`ProviderID`、`DeploymentID`、`RedactionPolicyID`、`TokenGuardPolicyID`、
  `Project.AllowedRoutes`）在**指向对象消失/停用/软删/过期**时，读侧的行为是拒绝还是继续？按 B3，
  必须拒绝。
- 删除语义是否一致：`Project`/`ProviderInstance`/`Deployment`/`Route`/`GatewayKey` 都有 `DeletedAt`，
  `Credential`(models.go:116) 与 `ProviderResource`(models.go:32) 没有——这是有意的还是漏的？
- tombstone 是否对运行时读取路径可见，还是只对 Admin 列表可见。

### C2 能力收窄的单调性（B1）
链条是 `Provider Profile 上限 → ProviderInstance.Capabilities → Binding.Capabilities →
Deployment.Capabilities → 请求期过滤`。要查的不是每层各自的 `Validate()`（那些已存在），而是
**跨层比较在哪里做、能否被绕过**：
- `MaxProviderCapabilitiesForProfile`(models.go:667) 的调用点是否覆盖全部写入路径（含批量/导入/
  恢复/迁移路径），`admin_provider_ceiling_bypass_test.go` 已守住哪些、没守住哪些；
- `Deployment.BindingID` 是 `omitempty`(models.go:881) 而 `ProfileID`/`AccessSurface` 必填——绑定被
  改动或禁用后，Deployment 依据哪个字段解析，两者冲突时谁赢；
- `CapabilityEvidence` 与 `OperatorDisabled`(models.go:911) 的三态（从未确立 / 已验证 / 被运维关闭）
  在下游是否始终区分，还是某处塌缩成布尔。

### C3 秘密与上游标识不外泄（B2）
沿链路追四类值：凭据明文与密文、`Credential.Audience`/`KeyVersion`、`Deployment.ProviderModel`
（上游模型 ID）、`GatewayKey.KeyHash`。检查它们是否出现在：Admin API 响应体、Gateway 应答体与错误
体、结构化日志、Prometheus 标签、审计记录、前端 store。
`GatewayKey` 的注释(models.go:229)已声明必须走 `gatewayKeyView` 序列化——查是否有第二条序列化路径。
同一提问要对 `Credential` 与 `ProviderInstance` 各问一遍。

### C4 项目隔离与授权粒度
- Gateway Key → Project → 可达 Route/Deployment/Provider 的可达集，是否恰好等于运维配置的意图；
- 跨 Project 借用：一个 Project 的 key 能否触达另一个 Project 的 Route、`ProviderResource`（B6）、
  预算或用量；
- `Project.AllowedRoutes` 的语义（见 H1）——它决定授权粒度是"模型"还是"路由"，这是设计问题不是实现
  细节。

### C5 revision、并发与写入原子性（B4）
- 六跳的写入是否都走 revision 检查；跨对象一致性（例如改 Provider 能力的同时使某个 Deployment 失效）
  是在单个 bbolt 事务里，还是两次写之间存在可观察的中间态；
- Admin 写入与 Gateway 读取并发时，运行时快照（`internal/auth/snapshot.go`）的更新时序与失效语义。

### C6 探测状态与新鲜度
`LastTestStatus/LastTestedAt/LastTestRevision` 在 `ProviderInstance`、`Deployment`、`Route` 三处各有
一份副本。查：三份是否可能互相矛盾、`LastTestRevision` 是否真的用于判定陈旧、陈旧时按 B3 是否
fail-closed，以及"健康"是否被当作放行条件（若是，陈旧的健康结论就是 fail-open）。

### C7 命名与语义漂移（B8）
字段名承诺的东西与实际存放的东西不一致，是本仓最容易积累的一类债，且前端与运维都会照名字理解。
逐字段对照：名字 / 注释 / 实际写入值 / 实际读取用途。

### C8 错误语义与可行动性
链路配错时（悬空引用、能力冲突、凭据过期、绑定禁用），Admin API 返回的错误是否指出**哪一跳**错了；
Gateway 侧是否泄漏内部结构（与 C3 相反的一面：既不能泄漏，也不能含糊到无法排障）。

**交付物**：`findings-backend.md`，每条带 `文件:行号`、违反的基准编号、以及【肯定 / 建议 / 问题 /
疑似BUG】分级。

---

## 四、阶段 2：Admin 控制台一致性（`web/`）

前端不重新审 UI 质量（那是多角色评审里"交互/视觉"角色的事），只审**它是否忠实表达后端约束**：

- **约束镜像**：后端 `Validate()` 里的每条规则，前端要么同样拦截、要么把后端错误如实呈现；重复实现
  的规则是否已经与后端漂移（`MaxProjectNameLength = 128`(models.go:201) 的注释明说"web 表单镜像此值"
  ——这类镜像点全部列出并逐个核对）。
- **链路引导**：`ProvidersPage` / `DeploymentsPage` / `RoutesPage` / `ProjectsPage` 是否强制正确的建立
  顺序，以及上游对象不可用时下游表单的可选项是否随之收窄（能选出一个必然失败的组合，就是缺陷）。
- **revision 冲突**：409 的处理是提示重载还是静默覆盖。
- **秘密驻留**：一次性明文（Gateway Key、凭据）是否只存在于内存、是否进入任何持久化 store（CSRF 与
  密钥不落浏览器存储，见 `CLAUDE.md`）。

**交付物**：`findings-web.md`（与后端发现交叉引用：同一条约束两侧不一致的，单列为"契约漂移"）。

---

## 五、阶段 3：真实二进制运行时验证

静态审查产出的是"代码看起来会这样"，这一阶段回答"真跑起来是不是这样"。**只对阶段 1/2 里判定不确定
或后果严重的条目做**，不做全覆盖。

**环境**：一次性数据目录（不碰现有 `data/` 与 `master.key`），`make build` 后启动真实二进制，走真实
Admin API 建链，再用真实 `gw_` key 发请求。

**执行前需先解决的两个前置问题**（不预设答案，阶段 3 开头先验证）：
1. Admin API 的会话/TOTP/CSRF 流程能否脚本化驱动；不能则退化为"手动建链 + 脚本化断言"。
2. 上游打桩：`safetransport` 强制 HTTPS + 主机白名单 + 固定拨号，本地假上游能否被允许。如果不能，
   运行时剧本收缩为"只验证到 Provider 请求发出之前"的部分（预留、能力过滤、鉴权、隔离），并在报告
   里写明这一限制，而不是绕过 `safetransport`。

**剧本**（每条剧本 = 一个可复现的操作序列 + 一个明确的预期）：
| 编号 | 操作 | 预期 |
|---|---|---|
| R1 | 完整建链后正常调用 | 成功；审计与用量记录里出现完整 owner 三元组，且不含凭据与上游模型 ID |
| R2 | 删除链中被引用的 Credential | 下游按 B3 拒绝，且错误指明是凭据缺失 |
| R3 | 禁用 / 软删 Provider、Deployment、Route 各一次 | 每次都在 Gateway 侧立即失效，无窗口期 |
| R4 | 收窄 Provider 能力，使已有 Deployment 超出上限 | 写入被拒或 Deployment 随之失效；不得留下超限仍可用的 Deployment |
| R5 | 用 Project A 的 key 请求 Project B 的路由 / 资源 | 拒绝，且错误不泄漏 B 的存在 |
| R6 | 凭据过期时间已过 | 按 `Credential.ExpiresAt` 注释(models.go:125) 是 advisory——确认运行时确实**不**据此拒绝，且控制台确实给出提示。注释与行为不一致即为发现 |
| R7 | Gateway Key 过期 / 禁用 / 所属 Project 禁用 | 三种都拒绝 |
| R8 | 建链过程中并发改动上游对象 | revision 冲突可见，无静默覆盖 |

**交付物**：`runtime-evidence.md`，含实际命令、实际响应片段（脱敏）、以及与静态判断不符之处。

---

## 六、阶段 4：对抗验证

沿用本仓已验证的方法（见 `docs/review/README.md` 末节）：对阶段 1–3 中最严重的若干条，**各派一个独立
的证伪视角**，默认发现为错，要求在代码里复现完整可达路径或找出拦截防御，裁决 CONFIRMED / REFUTED /
PARTIAL。历史上两轮评审的最严重发现**无一条原样成立**，因此这一步不是可选项。

**交付物**：`260814.md`（总报告）——结论总览 → 各阶段发现 → 对抗裁决 → 按"严重度 × 修复成本"排序的
整改清单。

---

## 七、进入评审时已存在的待验证假设

下面几条是拟定方案时读代码顺手记下的**疑问**，不是发现。列出来是为了让阶段 1 有明确靶子，同时避免
它们被当成结论传播——每一条都必须在阶段 1 里独立证实或证伪。

- **H1 · `Project.AllowedRoutes` 名实不符，且可能决定了授权粒度。** 字段名与类型
  (`[]string`, models.go:178) 读起来是路由 ID 集合，但 `internal/gateway/service.go:225` 的比对是
  `slices.Contains(principal.Project.AllowedRoutes, model)`——比的是 public model。若属实，则两条
  public model 相同的 Route，Project 无法只授权其中一条；授权粒度实际是"模型"而非"路由"。要查清：
  这是设计意图（那么字段名与文档要改，B8），还是实现漂移。
- **H2 · 删除语义不齐。** `Credential` 与 `ProviderResource` 无 `DeletedAt`，链上其余对象都有。硬删
  凭据时，引用它的 Provider 变成悬空引用——按 B3 应 fail-closed，需确认读侧真的拒绝。
- **H3 · `BindingID` 可空。** `Deployment.BindingID` 为 `omitempty`(models.go:881)，而绑定是能力与
  凭据方案的实际承载者(models.go:310)。空值时能力上限依据什么解析，是否可能绕过 C2 的收窄链。
- **H4 · 探测状态三副本。** Provider / Deployment / Route 各存一份 `LastTest*`，且 Route 的那份反映的
  是它所指 Deployment 的健康——两份不一致时谁是准的，以及是否有任何放行决策读它（若有，即 B3 风险）。
- **H5 · 定价隔离与链路的关系。** `PricingQuarantined`(models.go:899) 是 Deployment 上的状态，但预算
  预留在链路更下游发生（B5）。隔离态的 Deployment 是否可能仍被路由选中并发出上游请求。
- **H6 · `ProviderResource` 的跨 Project 可达性。** 记录带 `ProjectID`(models.go:35)，但需确认每条读
  路径都校验它，而不是只在创建时写入（B6）。

---

## 八、执行顺序与叫停点

```
阶段 0 链路映射  ──▶ 阶段 1 后端八族  ──▶ 阶段 2 前端一致性  ──▶ 阶段 3 运行时  ──▶ 阶段 4 对抗验证
      ▲                    ▲                                          ▲
   叫停点 1            叫停点 2                                    叫停点 3
  （形状不符预期        （发现量足够多，                        （前置条件不成立，
    则先对齐设计）        可先修再继续）                          则收缩剧本范围）
```

阶段 0 与阶段 1 是必做的地基。阶段 2 可与阶段 1 并行。阶段 3 的价值取决于前置问题能否解决，若不能，
其未覆盖部分要在总报告里明确写为"未验证"，不得以静态推断顶替（`CLAUDE.md`：无法验证时说明无法验证，
不要把假设当发现）。
