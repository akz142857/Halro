# 阶段 0：配置链路结构映射

本文件是 [review-plan.md](review-plan.md) 阶段 0 的交付物：**只记录链路的真实形状，不给判断**。所有条目
均来自实际代码，带 `文件:行号`。文末列出本阶段仍未映射的部分——那些地方阶段 1 不能当作已知。

基线提交：`1ce1113`（`main`，工作区含未提交改动，均不在本链路范围内）。

---

## 一、两条平面

链路在两个完全不同的时机被走完，两条路径的失败语义不同，这是全篇最重要的一个结构事实。

**控制平面（配置期，一次性把链路编译成路由表）**

```
store.ListProviders / ListRoutes / ListDeployments        internal/app/providers.go:321-332
  ├─ 每个 Provider：GetCredential → 类型/受众核对 → 解密       providers.go:369-426
  ├─ 每个启用的 Binding：内建 profile 核对 → 能力上限核对 → 建 adapter  providers.go:396-447
  └─ 每条启用的 Route：查 Deployment → 漂移复核 → 选价 → 求能力交集 → Register(Target)
                                                          providers.go:449-602
```

产物是 `provider.Registry`（`internal/provider/provider.go:256`），键是 **public model alias**，值是
`[]Target`（`provider.go:228`）。`Target` 是链路被压平后的结果：它同时持有 route ID、deployment ID、
provider ID、binding ID、上游模型名、价格、能力、并发上限。

**数据平面（请求期，只做两次查表）**

```
Authenticate(gw_...)  →  AuthResult{Key, Project}          internal/auth/snapshot.go:74-95
  ├─ AllowedRoutes 包含请求 model？                          internal/gateway/service.go:225
  ├─ 源 CIDR 授权、策略快照覆盖检查                            service.go:228-233
  └─ registry.ResolveCandidatesFor(model, operation)        service.go:234
```

数据平面**从不触碰 store 里的 Credential / Provider / Deployment / Route 记录**——它读的是
`Registry` 与 `auth.Snapshot` 两份内存快照。链路的六跳里，前四跳只在控制平面存在，后两跳
（Project、GatewayKey）只在 `auth.Snapshot` 里存在。例外：定价与记账在请求期按
`target.DeploymentID` 回读 store 的价格状态（见第七节）。

---

## 二、逐跳表

### 跳 1 · Credential（`internal/domain/models.go:116`）

| 项 | 事实 |
|---|---|
| 上游引用 | 无（链路起点） |
| 关键字段 | `Type` / `AccessSurface` / `Scheme` / `Audience` / `Ciphertext` / `KeyVersion` / `ExpiresAt`(可空，注释 `models.go:125` 明言 advisory——网关不据此拒流量) |
| `Validate()` | `models.go:141` — (Type, AccessSurface, Scheme) 三元组经 `ResolveCredentialProfile` 自洽；`Audience` 与 `Ciphertext` 非空；`ExpiresAt` 非零值 |
| store 写 | `PutCredential` `store_providers.go:157` — 先 `normalizeCredentialProfile` 补默认 profile，再 Validate，无跨对象检查 |
| store 删 | `DeleteCredential` `store_providers.go:195` — **硬删**；核 revision 后 `ensureCredentialUnreferenced`(`:356`) 遍历 `bucketProviders` 与 `bucketAlertWebhooks`，命中未软删引用则 `ErrCredentialInUse` |
| 生命周期 | **无 `DeletedAt`**。删即消失 |
| 运行时读取 | 仅控制平面 `providers.go:369`；数据平面不可达 |
| 明文出口 | `secretVault.DecryptCredential` `providers.go:420`，解密后立刻 `clear(plaintext)`(`:428`)；Admin 视图 `credentialViewFrom` `admin_providers.go:1591` 只出 `SecretConfigured` 布尔与 `BoundBaseURL` |

### 跳 2 · ProviderInstance（`models.go:255`）

| 项 | 事实 |
|---|---|
| 上游引用 | `CredentialID`（`models.go:261`） |
| 关键字段 | `Type` / `BaseURL` / `AllowedHosts` / `AccessSurface` / `ProfileID` / `CredentialScheme`（合称 legacy 投影）/ `Bindings` / `Capabilities` / `CapabilityEvidence` / `MaxConcurrency` / `BedrockProjectID` / `AllowedAnthropicBetas` |
| `Validate()` | `models.go:464` — 三条跨字段约束单独记：① `Bindings[0]` 必须与 legacy 投影三字段完全相等(`:552-556`)；② `Capabilities` 必须与 `BindingsCapabilitiesSummary(Bindings)` **双向子集**即相等(`:558`)；③ 无 bindings 且 profile 为不可变能力 profile 时，直接对 `DefaultProviderCapabilitiesForProfile` 收口(`:567`) |
| store 写 | `PutProvider` `store_providers.go:227` — 同一事务内：查 Credential 存在 → `validateProviderCredentialProfile`(`:298`，Type/AccessSurface/Scheme 三者相等) → **遍历全部未软删 Deployment**，任一 `validateDeploymentProviderProfile` 失败即整体拒绝(`:247-265`) |
| Admin 侧额外守卫 | 有 deployment 引用时禁止改 type/profile `admin_providers.go:266`；`validateProviderCanDeactivate` `admin_providers.go:1443`；`validateBindingsCanDeactivate` `admin_providers.go:1538` |
| 生命周期 | `DeletedAt` 软删（`admin_providers.go:345`，同时 `Enabled=false`） |
| 运行时读取 | 控制平面 `providers.go:365-448`，跳过 `!Enabled || DeletedAt != nil`(`:366`)；探测循环也按 deployment 回读(`health.go:46`) |

### 跳 2.5 · ProviderProfileBinding（`models.go:310`）

不是独立记录，内嵌在 ProviderInstance 里，但它是**能力与凭据方案的真实承载者**，链路的能力收窄实际发生
在这一层。

| 项 | 事实 |
|---|---|
| ID 规则 | `DefaultProviderProfileBindingID = providerID + ":" + profileID` `models.go:321` |
| `Validate()` | `models.go:329` — 带 `retired` 参数：软删记录跳过能力上限检查（否则超限旧记录永远删不掉，注释自述） |
| 控制平面校验 | `providers.go:400-419`：内建 profile 存在且三属性与 binding 相符；`ProviderCapabilitiesSubset(binding.Capabilities, MaxProviderCapabilitiesForProfile(...))`；binding 的 surface/scheme 与 Credential 相符 |
| 失败语义 | profile 不符 / 超上限 / adapter 建不出 → **withheld**（记 `report.Excluded`）；credential profile 不符 / 解密失败 → **refuse**（整个 registry 加载失败） |
| 归一化 | `normalizeProviderBindings` `store_providers.go:143` — 无 bindings 时从 legacy 投影合成一条；有 bindings 时反向用 summary 覆盖 `instance.Capabilities` 与 `CapabilityEvidence` |

### 跳 3 · Deployment（`models.go:873`）

| 项 | 事实 |
|---|---|
| 上游引用 | `ProviderID`(`:876`)、`BindingID`(`:881`，`omitempty`)、`ProfileID` + `AccessSurface`(`:879-880`) |
| 关键字段 | `ProviderModel`（上游模型标识）/ `TargetKind` / `Region` / `Capabilities` / `CapabilityEvidence` / `ModelCapabilitySnapshot` / `OperatorDisabled` / `MaxConcurrency` / `PricingQuarantined` |
| `Validate()` | `models.go:924` — 能力内部一致性（chat 特性依赖 chat、stream usage 依赖 streaming）、地域 profile 必须有 region、`CapabilityEvidence.Validate`、`ModelCapabilitySnapshot.Validate`、`OperatorDisabled` 与在用能力互斥 |
| store 写 | `PutDeployment` `store_pricing.go:129` — 同一事务：查 Provider 存在 → `deployment.Validate()` → `validateDeploymentProviderProfile`(`store_pricing.go:156`) |
| 跨跳收窄检查 | `validateDeploymentProviderProfile` `store_pricing.go:156-178`：binding 必须能解析；surface/profile 与 binding 相等；`ProviderCapabilitiesSubset(deployment.Capabilities, binding.Capabilities)`；逐项 evidence 等级不得高于 binding |
| BindingID 为空的解析 | 四处规则一致：`store_pricing.go:158`（`DefaultProviderProfileBindingID`）、`providers.go:521` 与 `:740`（`matchingBindingID`）、`admin_providers.go:1559`。`GetDeployment` `store_pricing.go:183` 读取时回填 |
| 快照语义 | `ModelCapabilitySnapshot` 注释(`models.go:719-720`)：**请求路径读快照、从不读活目录**——目录刷新不得改变已启用 deployment 的行为。复核状态不落盘(`:902-906`)，每次现算 |
| Admin 侧额外守卫 | `validateDeploymentCanDeactivate` `admin_deployments.go:1276` — 有启用且未软删 Route 指向则拒绝停用/删除 |
| 生命周期 | `DeletedAt` 软删（`admin_deployments.go:394`，同时 `Enabled=false`） |
| 运行时读取 | 控制平面 `providers.go:460`（按 route 反查）；探测循环 `health.go:33` 直接 `ListDeployments` |

### 跳 4 · Route（`models.go:1012`）

| 项 | 事实 |
|---|---|
| 上游引用 | `DeploymentID`(`:1019`)。注释(`:1015-1018`)明写：route 曾能直接带 provider+model，已收回 |
| 关键字段 | `PublicModel`（对外别名）/ `Priority` / `Strategy`(`ordered`\|`round_robin`) |
| `Validate()` | `models.go:1037` |
| store 写 | `PutRoute` `store_providers.go:317` — 事务内只检查 `bucketDeployments` 该 ID **存在**（不看 `Enabled`/`DeletedAt`） |
| Admin 侧额外守卫 | `validateAliasKeepsServingProjects` `admin_providers.go:1483` — 删除/改名使 alias 失去最后一条路由且仍有 Project 引用时 409（`admin_providers.go:906`/`:954`） |
| 生命周期 | `DeletedAt` 软删（`admin_providers.go:961`） |
| 运行时读取 | 控制平面 `providers.go:449`，跳过 `!Enabled || DeletedAt != nil`(`:450`) |
| 多路由同别名 | 允许。`Registry.Register` 按 `PublicModel` 聚合(`provider.go:370`)，同别名所有 target 的 `Strategy` 必须一致(`:363`)，按 `Priority` 再按 ID 排序(`:371-382`) |

### 跳 5 · Project（`models.go:174`）

| 项 | 事实 |
|---|---|
| 上游引用 | `AllowedRoutes []string`(`:178`)、`RedactionPolicyID`(`:188`)、`TokenGuardPolicyID`(`:189`) |
| `AllowedRoutes` 实际内容 | **public model 别名，不是 route ID**。三处一致：`admin_projects.go:451`（与 `route.PublicModel` 建集合比对 `:446`）、`admin_providers.go:1512`、`gateway/service.go:225` |
| 关键字段 | `Enabled` / `RPM` / `TPM` / `MaxConcurrency` / `DailyBudgetMicrosUSD` / `MaxInputTokens` / `MaxOutputTokens` / `MaxRequestBytes` / `MaxStreamDuration` / `AllowedCIDRs` |
| `Validate()` | `models.go:203` — 仅本地约束（名称 ≤128 等），**不校验任何引用** |
| store 写 | `PutProject` `store_projects.go:15` — 事务内**无任何跨对象检查** |
| Admin 侧引用校验 | `validateProjectReferences` `admin_projects.go:425` — 两个策略必须存在、未软删且 `Enabled`；`AllowedRoutes` 每个 alias 必须在未软删 Route 的 `PublicModel` 集合里（注释明说**禁用的 route 仍可绑定**，`:449`） |
| 生命周期 | `DeletedAt` 软删（`admin_projects.go:179`），删除需 step-up(`:162`) |
| 运行时读取 | `auth.Snapshot`，软删记录 `Refresh` 时排除(`snapshot.go:61`) |

### 跳 6 · GatewayKey（`models.go:233`）

| 项 | 事实 |
|---|---|
| 上游引用 | `ProjectID`(`:234`) |
| 关键字段 | `KeyHash [32]byte` / `HashVersion` / `Enabled` / `ExpiresAt`(可空) |
| `Validate()` | `models.go:1210` — ID、ProjectID、Name 非空，`HashVersion == 1` |
| store 写 | `PutGatewayKey` `store_projects.go:60` — 事务内检查 `bucketProjects` 存在该 ID（**不看 `DeletedAt`**）；维护 `bucketGatewayKeyHash` 反查索引，哈希冲突 `ErrKeyHashConflict`(`:75`) |
| 签发路径 | `admin_projects.go:230-285`：签发本身要 step-up(`:234`)；key ID 由幂等摘要确定性派生(`:240`，`gatewayKeyIdempotencyDigest` `:460`)，重试变成 `ErrAlreadyExists` → 409 幂等回放(`:259-268`)而非第二把活密钥；软删 Project 拒发(`:249`)；明文只出现在一次性 201 响应体(`:279`)，带 `Cache-Control: no-store`(`:276`) |
| 序列化约束 | 结构体注释(`models.go:229`)：Admin handler 必须序列化 `gatewayKeyView`，`TestAdminKeyResponsesNeverExposeKeyHash` 守护；`LastUsedAt` 已被有意移除(`models.go:246-253` 注释说明缘由) |
| 生命周期 | `DeletedAt` 软删（`admin_projects.go:367`），删除需 step-up |
| 运行时读取 | `auth.Snapshot.Authenticate` `snapshot.go:74` — 格式 → 哈希查表 → 常数时间比对 → `Enabled` → `ExpiresAt` → Project 存在且 `Enabled` |

---

## 三、失效语义矩阵（控制平面）

上游对象消失/停用时，`loadProviderRegistry` 的三种反应。阶段 1 判定 fail-closed 的底表。

| 情形 | 反应 | 位置 |
|---|---|---|
| Provider 的 Credential 读不到 | **refuse**（整个 registry 加载失败） | `providers.go:369-372` |
| Credential 类型与 Provider 不符 | **refuse** | `providers.go:373-375` |
| Credential 受众与端点不符 | **refuse** | `providers.go:392-394` |
| Binding 与 Credential 的 surface/scheme 不符 | **refuse** | `providers.go:417-419` |
| Credential 解密失败 | **refuse** | `providers.go:420-426` |
| Provider 端点不再通过 `safetransport` 策略 | **excludeProvider** | `providers.go:378-391` |
| Binding profile 与内建 profile 不符 | **excludeBinding** | `providers.go:400-404` |
| Binding 能力超出 profile 上限 | **excludeBinding**（注释明写"withhold 而非 clamp"） | `providers.go:413-416` |
| adapter 建不出 / bridge 失败 / 重复注册 | **excludeBinding** | `providers.go:429-446` |
| Route 指向的 Deployment 不存在/停用/软删 | **withheld**（`report.Dangling`） | `providers.go:460-473` |
| Bedrock Mantle 部署地域与 Provider 端点地域不符 | **withheld** | `providers.go:501-510` |
| 能力复核为 `drifted` | **withheld**（`report.Drifted`） | `providers.go:511-518` |
| 价格读取报非 `ErrPriceUnavailable` 的错 | **withheld** | `providers.go:525-537` |
| 无当前价（`ErrPriceUnavailable`） | **放行**，价格投影为 0 | `providers.go:538-544` |
| binding 的 adapter 不在（被关掉/移除） | **withheld** | `providers.go:548-562` |
| `ValidateProfileModel` 拒绝上游模型名 | **withheld** | `providers.go:563-572` |
| `Registry.Register` 拒绝（如能力交集为空） | **withheld** | `providers.go:594-601` |

分界线（`providers.go:346-350` 注释自述）：**说"存储状态被篡改"或"金库不可信"的失败才 refuse，其余
只排除受影响部分、继续服务其余**。

数据平面失效点：别名无候选（404 / 有 target 但 operation 不支持则 400，`service.go:235-240`）、不健康
target 被剔除（`provider.go:434-437`，见第五节）、Project 引用的策略未加载（503 fail-closed，
`service.go:184-213`，注释明言拦的是"快照落后于持久层"）、激活失稳（见第九节）。

---

## 四、能力收窄链（完整路径）

```
内建 profile 上限   MaxProviderCapabilitiesForProfile(type, profileID)      models.go:667
  └─ Binding.Capabilities        写入期: binding.Validate           models.go:539
                                 加载期: providers.go:413 (超限则 withhold)
      └─ ProviderInstance.Capabilities = BindingsCapabilitiesSummary(bindings)   models.go:557
                                 (双向子集校验，即强制相等)
          └─ Deployment.Capabilities ⊆ Binding.Capabilities   store_pricing.go:168
              且 evidence 逐项等级 ≤ binding                    store_pricing.go:171-176
              └─ Target.Capabilities = declared ∧ adapter 能力（逐位 AND）
                                 providers.go:546 → deploymentCapabilities providers.go:764-785
                  token 上限取两者较小（0 视为"无声明"）          minimumCapabilityLimit :787-795
                  declared 无任何 operation 时整套采用 adapter 能力    :767-769
                  └─ 请求期按 operation 过滤候选   provider.go:438-449
```

两个已记录的边界形状（判定留给阶段 1）：
- `deploymentCapabilities` 在 `!declared.AnyOperation()` 时**整套返回 adapter 能力**(`providers.go:767-769`)；
  而 `Registry.Register` 对能力交集为空的 CapabilityReporter target 是拒绝(`provider.go:340-351`)。
  两个"空"走向相反方向。
- 非 `CapabilityReporter` 的自定义 adapter 得到固定的 `{Chat, Streaming, Embeddings}`(`providers.go:797-803`)。

---

## 五、健康探测子系统（原未映射项 1，已补）

三份持久化 `LastTest*`（Provider `models.go:294-300` / Deployment `:890-894` / Route `:1023-1027`）与
一份内存态 `Registry.health` 并存。关系如下：

- **路由决策只读内存态**：`resolveCandidatesLocked` 按 `health[target.DeploymentID]` 剔除候选
  (`provider.go:434-437`)；`LastTest*` 三份**均不参与**任何路由或加载决策（`loadProviderRegistry` 全程
  不读）。
- **内存态唯一写入方**是主动探测循环 `probeDeployments`（`internal/app/health.go:32-72`），按
  `healthProbeInterval` 周期跑：deployment 或其 provider 停用/软删/adapter 缺失 → 记不健康
  (`health.go:48`/`:53`)；adapter 实现 `Prober` 则实际探测并记结果(`:65`)；**不实现 `Prober` 的 adapter
  跳过、保持可路由**(`:56-61`，注释明言靠被动熔断兜底)。
- **未探测 = 可路由**：`SetDeploymentHealthy` 注释(`provider.go:526-527`)明言未知 deployment 保持
  eligible，防启动期黑洞。registry 换代时旧健康状态向新表**保留搬运**(`provider.go:690-700`)。
- `LastTest*` 由 Admin 的手动测试端点写入（`admin_providers.go` 测试路径、`admin_deployments.go:319`
  一带），是**给运维看的旁证**，与内存健康态互不同步。

---

## 六、能力漂移复核（原未映射项 2，已补）

`internal/app/capability_drift.go`。四个状态（`models.go:693-708`）：

| 状态 | 含义 | 放行？ |
|---|---|---|
| `current` | 快照仍与目录一致 | 是 |
| `review_available` | 目录比快照多——不自动启用，等运维复核 | 是（注释：目录多出的东西不改变 deployment 已在做的事） |
| `drifted` | 快照声明的多于目录或运行 profile 现在支持的 | **否，fail-closed** |
| `catalog_unavailable` | 无可验证目录时签名快照仍是权威 | 是 |

`capabilityReviewAdmitsTraffic`(`capability_drift.go:235-243`) 刻意写成 **allowlist**：新增状态默认
不可路由（注释自述理由）。控制平面在 `providers.go:511-518` 消费；判定入口
`reviewCapabilitiesWithCatalogState`(`capability_drift.go:86`)。

---

## 七、定价隔离拦截点（原未映射项 7 / H5，已补）

`PricingQuarantined` 的权威不是 Deployment 结构体字段，而是独立 bucket
`bucketDeploymentPricingHighWater` 里的 `Quarantined` 位；结构体字段是 Admin 读侧投影
（`admin_resources.go:160-180` 现查现填）。拦截发生在**请求期、每次 attempt**：

- `PrepareDeploymentPricePin`（`store_pricing.go:690-850`）事务内读 high-water：`Quarantined` 为真 →
  `ErrPricingQuarantined`(`:736`/`:846`)；检测到墙钟前跳还会**当场置隔离位**(`:738-742`，
  `wall_clock_forward_jump`)。
- 网关侧 `service.go:388-417`：该错误不属于 `ErrPriceUnavailable` 分支，落入通用错误路径 → 释放
  租约、结束 attempt，对外 503 `accounting_unavailable`。
- 控制平面**不读隔离位**——隔离态 deployment 照常进 registry、可被选中，拦在预留之前的选价一步。
  `providers.go:525-537` 只处理读价失败，与隔离无关。

---

## 八、激活时序（原未映射项 5，已补）

Admin 提交与快照换代之间的中间态有明确的机器可见语义：

- **提交点是 store 事务**。激活失败**不回传给 Admin 调用方**（`activateTopologyAfterCommit`
  `providers.go:99-110` 注释：报失败等于把已生效的变更说成没生效）。
- 激活用**独立于 Admin 请求的 context**（`activationContext` `providers.go:60-81`，30s 上限
  `:58`）——注释记录了历史缺陷：客户端断连曾中止重建，使已吊销的 key 继续放行。
- 激活失败 → `activation.markStale`（topology 域 `providers.go:92`；auth 域
  `admin_projects.go:485`）→ **数据平面整体拒绝**：`refuseWhileSnapshotsStale` 中间件挂在 OpenAI 与
  Anthropic 两个门面上（`runtime.go:1304`/`:1327`）；恢复循环 `runActivationRecovery`
  (`activation_state.go:185`) 重试直到 `markCurrent`。
- 签名目录换代与 Admin 拓扑变更共用 `adminTopologyMu` 串行化（`prepareModelCatalogActivation`
  `providers.go:112-131`），防止过期候选 registry 复活已删除的拓扑。

---

## 九、Invocation Target（原未映射项 8，已补概要）

`internal/domain/invocation_target.go` + `internal/app/admin_invocation_targets.go`（ADR 0019）。位置：
**Provider 与 Deployment 之间的发现/证据层**，不是链路的持久跳——Deployment 不引用它。

- `InvocationTargetScopeKey`(`invocation_target.go:51`) 把能力证据锁定在
  (provider, target, binding, profile, location) 五元组，注释明言防证据跨实例泄用。
- `AvailabilityState` 刻意独立于能力证据(`:11-13` 注释：出现在目录里不证明能做什么)。
- Deployment 创建时经 `resolveDeploymentTargetWithCatalog`(`admin_deployments.go:993`) 消费：先按
  "该 binding 能否服务该模型"筛 binding 候选（单模型 pin 提前到选择前，`:999-1005` 注释），再解析能力。
  `NormalizedModelMetadata`(`:38-40` 注释) 只保留白名单字段，原始上游 JSON 不落盘。

Bedrock 地域（原未映射项 7 的另一半）：`providerRegion`(`admin_deployments.go:792`) 从端点主机名解析
地域，`deploymentRegion`(`:781`) 优先取运维显式输入；控制平面用同一 `providerRegion` 做地域不符
withhold（`providers.go:501-510`）。

---

## 十、Admin API 与前端映射

| 跳 | Admin 端点前缀 | 前端位置 |
|---|---|---|
| Credential | `/credentials` `web/src/api.ts:284-300` | `ProvidersPage.tsx` 的 credentials 子视图（`:132` 拉取、`:213` 行渲染、`:465` 创建）——**无独立 CredentialsPage** |
| Provider | `/providers` `api.ts:303-353` | `web/src/pages/ProvidersPage.tsx`（971 行） |
| Deployment | `/deployments` `api.ts:355-408` | `web/src/pages/DeploymentsPage.tsx`（1679 行） |
| Route | `/routes` `api.ts:413-447` | `web/src/pages/RoutesPage.tsx`（290 行） |
| Project | `/projects` `api.ts:236-250` | `web/src/pages/ProjectsPage.tsx`（675 行） |
| GatewayKey | `/projects/{id}/keys` `api.ts:252-282` | 同 `ProjectsPage.tsx` |

链路引导痕迹：`ProvidersPage.tsx:178` 无 Credential 时禁用"新增 Provider"并提示
`providers.createCredentialFirst`；`:594` 按类型与 Bedrock 访问面预选 Credential；`:213` `useCount`
显示 Credential 被引用数。破坏性操作（Project/Route/Deployment/Key 删除）均经 `stepUpBody(reauth)`
传 step-up（`api.ts:245`/`:443`）。

---

## 十一、旁支（挂在链路上，阶段 1 不得跳过）

| 旁支 | 挂载点 | 位置 |
|---|---|---|
| RedactionPolicy / TokenGuardPolicy | Project 软引用 | `models.go:188-189`；引用校验 `admin_projects.go:426-437`；反向停用守卫 `admin_redaction.go:275`、`admin_token_guard.go:303`；请求期覆盖检查 `service.go:195` |
| DeploymentPriceVersion / 定价隔离 | Deployment | 见第七节 |
| ProviderResource（ADR 0021 上游孪生体） | (project, provider, deployment) 三元组 | `models.go:32`；写入三向存在性校验 `store_providers.go:386-394`；读/删按 project 收口 `:415-424`、`:470-486`；数据平面入口 `inference_resources_store.go:336`/`:416`；`ListProviderResources`(`:430`) 不收口，注释限定备份用 |
| AlertWebhook | 也引用 `CredentialID` | 参与 `ensureCredentialUnreferenced` `store_providers.go:357` |
| ModelCatalog | 参与漂移复核与激活串行化 | 第六、八节；ADR 0020 |
| 并发限额 | Provider 与 Deployment 两级 | `Target.MaxConcurrency`/`DeploymentConcurrency` `provider.go:251-252`，请求期 `service.go:839` |

---

## 十二、对方案里六条待验证假设的事实澄清

阶段 0 只摆事实，**判定留给阶段 1**。

- **H1（`AllowedRoutes` 名实）**：事实成立——存的是 public model 别名，三处读写一致
  （`admin_projects.go:451`、`admin_providers.go:1512`、`service.go:225`），双向引用校验都以别名为
  单位。"授权粒度是别名而非路由"是**结构事实**：同别名多条 Route 由 Registry 聚合为候选列表
  (`provider.go:370`)，Project 无法只授权其一。阶段 1 判定是否设计意图（B8：字段名与实义不符）。
- **H2（删除语义不齐）**：事实成立且更细——Credential 是链路**唯一硬删**对象，由
  `ensureCredentialUnreferenced` 守删除；其余五跳软删。控制平面对读不到的 Credential 是 **refuse
  整个 registry**（`providers.go:369-372`），不是排除单个 provider。
- **H3（`BindingID` 可空）**：空值回退规则确定且四处一致（跳 3 表），`admin_providers.go:1556` 注释
  明言刻意对齐。阶段 1 查回退是否覆盖所有路径、四处是否真等价。
- **H4（探测三副本）**：**已解**（第五节）——三份持久化 `LastTest*` 不参与任何决策，路由只读
  `Registry.health` 内存态；未探测默认可路由，非 `Prober` adapter 恒不被主动探测。阶段 1 的问题
  收窄为：默认可路由 + 换代保留搬运的组合是否有 B3 意义上的陈旧窗口。
- **H5（定价隔离）**：**已解**（第七节）——控制平面不读隔离位，拦截在请求期每次 attempt 的
  `PrepareDeploymentPricePin` 事务内，fail-closed 为 503。阶段 1 的问题收窄为：不走价格钉路径的
  operation（`service.go:400` else 分支 `prepareAccountingLease`）是否同样受拦。
- **H6（资源跨 Project 可达）**：store 层读/删已按 projectID 收口，数据平面调用点传
  `principal.Project.ID`。阶段 1 覆盖其余调用点（`inference_resources_store.go` 1218 行未通读）。

---

## 十三、本阶段仍未映射的部分

1. **前端各页表单约束与 409 处理细节**——阶段 2 的正题，阶段 0 只映射了页面归属。
2. **`inference_resources_store.go`（1218 行）与 `admin_invocation_targets.go`（907 行）未通读**——
   只映射了入口与收口点。
3. **`prepareProviderRegistryActivation` 的 finalize 两相提交细节**（`providers.go:143-277` 只读了头部）。
4. **被动熔断（`internal/circuit`）与健康态的交互**——`health.go:59` 注释提到"passive circuit health
   still applies"，该路径未展开。
5. **`admin_resources.go` 之外是否还有 Deployment 视图出口未经隔离位投影**。
