# Provider 到 Project API 全链路 Review

> Review 对象：[`provider-to-project-api-call-chain.zh-CN.md`](provider-to-project-api-call-chain.zh-CN.md) 所描述的管理面和数据面链路。
>
> 基线：`main@b206285b920c`，并包含 2026-08-10 工作区中的未提交代码。
>
> 方法：静态代码走查，沿 Credential → Provider → Deployment → Route → Project → Gateway Key → `/v1/*` 的创建、激活、调用、失败和删除方向交叉核对。未修改业务代码，未执行真实 Provider 或计费 smoke test。
>
> 复核：2026-08-11 在 `main@4af0228`（较原基线前进 17 个提交）重新走查全链路，并对 F-01、F-02、F-04、F-05 逐条执行验证，不再只靠读代码推断。链路描述本身未发现错误；四条原 finding 全部仍然成立；新增 F-05。复核用的探针只读状态、只打日志，未修改业务代码，未发起任何 Provider 调用。

## 1. 结论

主数据面设计是闭环的：客户端身份、Project 策略、能力路由、版本价格、预算 reservation、Provider attempt、流式 delivery boundary 和结算的顺序清晰，且关键失败方向普遍采取 fail-closed。

本轮共 6 个点，按严重度排列（ID 按发现顺序分配，不重排）。四个已修复并推送，两个仍未处理：

| ID | 级别 | 类型 | 状态 | 结论 |
|---|---|---|---|---|
| F-05 | P0 | 确定缺陷 | **已修 `e1d94be`** | 禁用仍被启用 Deployment/Route 引用的 Profile Binding 不被拒绝：变更落盘、API 报 409、旧 registry 继续服务，此后每次拓扑变更都失败，**且进程再也无法启动** |
| F-01 | P0 | 确定缺陷 | **已修 `354428c`** | disable/delete 已持久化后，运行时刷新使用可取消的 Admin 请求上下文；刷新失败时旧 Key/Project/Route/Provider snapshot 继续服务 |
| F-02 | P1 | 确定缺陷 | **已修 `066a08a`** | 删除最后一个 Route 可留下仍引用该公共模型别名的 Project，且 Project 与 Route 使用不同协调锁，存在并发 TOCTOU |
| F-03 | P1 | 一致性风险 | **未处理** | topology mutation、snapshot activation、audit append 不是一个结果；API 报错时变更可能已持久化甚至已生效 |
| F-06 | P3 | 加固建议（原判 P1，已更正） | **未处理** | Provider 级装载失败仍然致命，与 F-05 同形，但**未能找到可达触发路径**；原文给出的 endpoint 场景经实测证伪，见该条 |
| F-07 | P2 | 确定缺陷 | **未处理** | `allow_private_provider_endpoints` 无法生效：所有 Provider/Credential 路径都调用非策略版 `safetransport.Audience`，私网地址一律被拒，开关形同虚设 |
| F-04 | P2 | 确定冗余 | **已修 `3580a89`** | Deployment `priority/weight` 被存储，但候选算法只使用 Route `priority/strategy`，字段对调用链无效 |

已修部分的共同做法：每条单独提交，每条都做反向验证——先退掉修复、断言退改真的生效（防止搜索串失效导致「什么都没改却通过」），再确认测试在缺陷态失败。详见 §10。

剩下三条：F-07 是确定缺陷但需要先定产品意图（让开关生效，还是删掉它）；F-06 降级为加固建议，因为在动手修它之前先做复现，结果把它自己的前提证伪了；F-03 最后，它需要先定义提交点语义，是设计决定而不是缺陷修复。

F-05 和 F-01 都是“durable mutation 已提交、runtime activation 没有发生”的实例，也就是 F-03 描述的那个缺失的提交协议。两者已分别修掉各自的可达路径：F-05 让坏记录不再能否决整次装载，F-01 让激活不再被客户端取消。但**都没有建立那个协议**，所以 F-03 仍然开着——见该条。

仓库里已经有这个协议的形状：`prepareModelCatalogActivation`（`internal/app/providers.go:84`）先构建候选 registry、成功后才提交，失败则整体放弃，全程持有 `adminTopologyMu`。Admin handler 仍然是先落盘再重建。

## 2. 已确认的正确设计

### 2.1 Route 不再绕过 Deployment

Route 只携带 `deployment_id`；Provider、上游模型、能力、健康、价格和并发都从 Deployment/Provider 导出。`domain.Route.Validate` 强制 Deployment ID，Store 再校验 Deployment 存在，Registry 装载时再次检查 enabled/deleted 和 Binding adapter。这消除了 Route 直接指向 Provider 时可能绕过价格、健康和能力快照的旁路。

证据：

- `internal/domain/models.go:880-917`
- `internal/store/bolt/store_providers.go:308-320`
- `internal/app/providers.go:241-332`

### 2.2 Provider I/O 之前先获得持久预算 reservation

每个 attempt 先通过熔断和并发，选择并 pin 价格，执行 Token Guard cost recheck，再写入独立 reservation 和 `AttemptStarted`，最后才调用 Provider primitive。这个顺序符合记账权威和 crash recovery 约束。

证据：

- `internal/gateway/service.go:307-458`
- `internal/gateway/service.go:800-832`
- `docs/contracts/gateway-correctness.md:5-19`

### 2.3 请求级与 attempt 级控制分层明确

Project RPM/TPM/并发和 Request lifecycle 每个客户端请求只计一次；Provider/Deployment 并发、reservation、usage 和 cost 每个 attempt 计一次。失败后的 retry/fallback 不会逃逸 Project 的请求级控制，也不会复用上一 attempt 的预算 reservation。

### 2.4 流式 delivery boundary 被显式守住

流式输出在 semantic validation、协议转换和跨 chunk redaction 后才下发。任何 payload 已成功 emit 后，错误不会切换到另一 Deployment，避免一个响应混入多个上游结果。

证据：

- `internal/gateway/service.go:1481-1577`
- `docs/contracts/gateway-correctness.md:21-36`

### 2.5 Snapshot 重复总体是有意设计

bbolt 到 Registry/Auth Snapshot 的重复是热路径编译结果，不应简单视为第二权威；Deployment 价格虽投影进 Target，请求时仍重新选择并 pin 版本价格。这个边界是合理的。真正的问题不是“有副本”，而是变更和副本激活失败时的语义，见 F-01/F-03。

## 3. Findings

### F-05 [P0] 禁用仍被引用的 Profile Binding 会写坏数据目录，使进程无法再启动

【确定缺陷；2026-08-11 实测复现；**已修 `e1d94be`**】

Deployment 和 Provider 两级都有停用守卫：`validateDeploymentCanDeactivate` 要求先停掉 Deployment 的活跃 Route，`validateProviderCanDeactivate` 要求先停掉 Provider 的活跃 Deployment。**Profile Binding 这一级没有对应守卫**。`updateAdminProvider` 只把 Provider 自身的 `Enabled` 传给守卫（`admin_providers.go:249`），而 `bindings` 是整体替换、每个 binding 的 `Enabled` 直接取自输入（`admin_providers.go:886-935`），没有任何“该 binding 是否仍被启用的 Deployment 引用”的检查。

`domain.ProviderInstance.Validate` 只要求“启用的 Provider 至少有一个启用的 binding”（`models.go:446`），所以单 binding 的 Provider 走不通，**双 binding 的 Provider 可以**：关掉其中一个、另一个保持启用，Provider 记录依然合法。

放大它的是 registry 装载的失败语义。走查底稿 §3.1 把 7 条规则都写成“筛选”，但只有能力 drift 那一条是跳过单条 Route（`providers.go:276-282` 的 `continue`）；其余不满足时是 `return fail(...)`，**整个 Registry 装载失败**。被禁用的 binding 不会产生 adapter（`providers.go:210` 的 `continue`），于是仍然启用的 Route 走到 `providers.go:305` 的 `route %q references an unavailable provider binding` 并让整次装载失败。`prepareProviderRegistryActivation`（`providers.go:67`）在装载失败时返回 error 且不替换任何东西，旧 registry 原样留在服务中。

实测后果链（`main@4af0228`，bootstrap 出的 OpenAI Provider + Deployment + Route，通过真实 Admin handler 发起 `PUT /admin/api/v1/providers/{id}`，把 Deployment 绑定的 chat binding 置为 `enabled:false`、media binding 保持启用）：

```text
PUT /admin/api/v1/providers/{id}   → 409 {"error":"configuration could not be activated"}
持久化状态                          → chat binding enabled=false        （已落盘）
live registry                       → 仍有 1 个候选                     （禁用未生效，fail-open）
审计                                → 无记录（handler 在 reload 失败处返回，早于 audit）
后续任意拓扑变更（实测：无关的 Route PUT）→ 409 configuration could not be activated
进程重启                            → Open 失败：
                                      route "rte_..." references an unavailable provider binding
```

也就是说：一次 Admin 编辑同时做到了四件事——撤销没有生效、没有留下审计、管理面的拓扑变更全部卡死、**数据目录进入一个进程无法启动的状态**。最后一条是不可自愈的：重启不能恢复，因为拒绝启动的正是这份已落盘的状态。恢复只能靠离线改回 binding 的 `enabled`。这正是仓库 CLAUDE.md 对 fail-closed 检查的告警——写错的代价是拒绝启动而不是降级。

这条路径**从普通 Admin UI 即可触发**，不需要手写 API 调用。Provider 编辑表单提交 `bindings: openAIBindings(...)`（`web/src/pages/ProvidersPage.tsx:490`），而 `openAIBindings`（同文件 `:687`）里每个 binding 的 `enabled` 是从勾选的能力推导出来的：`enabled: hasEnabledCapability(chat)`。运维在 Provider 页取消勾选 chat/embeddings 类能力（例如“这个 Provider 以后只做图像”），而某个 Deployment + Route 还绑在 chat binding 上，就会命中。

代码证据：

- Binding 级守卫缺失：`internal/app/admin_providers.go:249`、`1025-1041`
- Binding 整体替换、`Enabled` 取自输入：`internal/app/admin_providers.go:886-935`
- 只要求“至少一个启用的 binding”：`internal/domain/models.go:446`
- 禁用的 binding 不产生 adapter：`internal/app/providers.go:209-212`
- 引用不存在 adapter 的 Route 让整次装载失败：`internal/app/providers.go:303-306`
- 装载失败不替换、旧 registry 留存：`internal/app/providers.go:67-71`
- reload 失败在 audit 之前返回：`internal/app/admin_providers.go:266-272`
- UI 从能力勾选推导 binding 启停：`web/src/pages/ProvidersPage.tsx:490`、`687-694`

建议方向：

- 补上 binding 级停用守卫，与 Deployment/Provider 两级同形：拒绝停用或移除仍被非墓碑、启用中的 Deployment 引用的 binding，并在错误里点名是哪个 Deployment，而不是让运维撞上通用的激活失败；
- 更根本的是**先验证候选 registry 装载得起来，再提交 durable mutation**。目前顺序是“落盘 → 重建 → 失败则报错”，这让任何 handler 层漏掉的引用完整性检查都直接升级成写坏数据目录。`prepareModelCatalogActivation` 已经是 prepare-candidate → commit 的形状，Admin 拓扑变更应当共用；
- 启动路径应当能区分“这份状态装载不起来”和“进程该拒绝启动”。让一个 Route 的悬空引用带走整个进程，与 §2 里 drifted deployment 只被 withheld 的处理是两套语义，且方向相反；
- 回归测试至少覆盖：禁用被引用的 binding、从 `bindings` 列表中整体移除被引用的 binding、以及“落盘后能否重启”这一条断言。只断言 bbolt 字段已改变不足以发现本缺陷——本轮 API 返回 409 的同时状态已经写坏。

### F-06 [P3] Provider 级装载失败仍然致命，但未找到可达触发路径

【加固建议；**原判 P1「确定缺陷」，经实测更正**】

F-05 只把**单条 Route** 造不出 Target 的情况改成了 withheld。作用域更大的问题——「这个 Provider 根本装载不起来」——仍然 `return fail(...)`，让整次装载失败，因此在原理上保留 F-05 那条完整的后果链：变更已落盘、激活失败、此后每次拓扑变更都失败、进程再也起不来。

仍然致命的路径（`internal/app/providers.go:278-331`、`394`）：Credential 不存在/类型不匹配/audience 不匹配、endpoint 不符合当前安全策略、Binding profile 不兼容、凭据解密失败、adapter 或 bridge 构建失败、adapter 注册失败、非 `ErrPriceUnavailable` 的价格读取错误。

**原文给出的可达场景是错的，已由实测推翻。** 原文声称：运维开启 `allow_private_provider_endpoints: true` 建一个私网 Provider，之后把开关改回 `false`，装载即失败、进程起不来。动手修之前先复现，结果是**建不出这样的 Provider**：无论走 `Bootstrap` 还是 Admin API，创建 Credential 的那一步就被拒（`400 private address 10.1.2.3 is not allowed`）。原因是 F-07——`safetransport.Audience` 用空策略重新校验一次，私网地址一律被拒。既然私网 Provider 无法存在，收紧开关就不可能让实例起不来。

其余致命路径逐条看可达性：

- Credential 不存在：存储层拒绝删除仍被非墓碑 Provider 引用的 Credential。有守卫。
- 类型 / audience 不匹配：写入时校验，且 audience 由 `base_url` + 类型推导，两者都在记录里，不会因外部状态改变。
- endpoint 不符合策略：见上，不可达（前提是 F-07 保持现状；**若 F-07 按「让开关生效」修，这条立刻变为可达**）。
- 凭据解密失败：需要主密钥不可用或轮换出错。这是唯一看起来可达的一条，但也是最有理由保持致命的一条——不过「一个 Provider 的密钥有问题」是否应该让其余 Provider 全部停摆，仍然值得单独判断。
- adapter / bridge 构建、Target 注册：给定 profile 与配置是确定的，只有构建变更可能引入，属于推测。

因此这一条从「确定缺陷」降为「加固建议」：形状仍在，代价仍是拒绝启动而非降级，但没有已证实的触发方式。它值得做的理由是影响半径，而不是有 bug 在等着触发。

建议方向（若采纳）：

- 沿用 F-05 的模板：把作用域限于单个 Provider 的问题降级为「排除该 Provider 及其 Route」，记进 `loadReport` 并审计；`referenceWithholding` 已经是这个形状，缺一个 provider 级的同类；
- 一并决定：**是否存在应该让进程拒绝启动的装载失败**。若答案是「只有批量存储读取失败」，那么 `fail` 就该收敛到那一处，语义变成「装载永远成功，只是可能排除东西」，比留一个偶尔致命的分支更容易推理；
- 无论怎么选，`halro doctor` 都要能报出被排除的 Provider 和 Route，否则「装载永远成功」会把问题藏起来。

### F-07 [P2] `allow_private_provider_endpoints` 无法生效

【确定缺陷；2026-08-11 实测】

`safetransport.Audience(raw, semantic)` 转发到 `AudienceWithPolicy(raw, semantic, Policy{})`——**空策略**（`internal/safetransport/transport.go:118-120`）。空策略意味着 `AllowPrivate=false`，于是私网地址一律被拒，与配置无关。

而所有 Provider / Credential 路径用的都是这个非策略版本：

- `credentialFromInput`：`admin_providers.go:782`
- `providerFromInput`：`admin_providers.go:861`
- 连接测试：`admin_providers.go:1044`
- onboarding 就绪评估：`admin_onboarding.go:177`
- `Bootstrap`：`bootstrap.go:57`（这条连 `ValidateURL` 都没带策略，`bootstrap.go:53` 是硬编码的 `Policy{RequireHTTPS: true}`）
- **Registry 装载**：`providers.go:294`

这三处确实读了开关的 `ValidateURL`（`admin_providers.go:777`、`852`、`providers.go:287`）后面紧跟着一个非策略版 `Audience`，把刚放行的东西又拒掉。开关在每一个使用点都被抵消。

实测（`ValidateURL` 与 `Audience` 对同一地址的分歧）：

```text
ValidateURL("https://10.1.2.3", AllowPrivate=true)       → nil
Audience("https://10.1.2.3", "openai")                    → private address 10.1.2.3 is not allowed
AudienceWithPolicy("https://10.1.2.3", ..., AllowPrivate) → nil
```

对照组：webhook 那一侧用的是策略版 `AudienceWithPolicy`（`alerts.go:107`、`admin_alerts.go:304`），所以 `allow_private_webhooks` 是真的能生效的。两个同族开关，一个有效一个无效。

**方向是 fail-closed 的，所以这不是安全漏洞**：实际行为是「私网地址一律拒绝」，即更安全的那一侧，没有人因此被意外暴露。它的代价是一个写在 `config.yaml:62` 的能力从未可用，运维把它改成 `true` 会以为自己打开了自建/私网模型端点的支持，而实际什么都没变，且失败信息指向地址本身而不是这个开关。

需要先定产品意图，两条路互斥：

- **让开关生效**：把 Provider/Credential 路径改用 `AudienceWithPolicy` 并传入配置（Bootstrap 的硬编码策略也要一并修）。这会**启用一项当前完全关闭的、涉及 SSRF 面的能力**，因此属于安全相关变更，不该顺手做；且它会让 F-06 的 endpoint 场景从不可达变为可达，两条必须一起处理。
- **删掉开关**：按 pre-1.0「错误构造不与替代品并存」直接移除 `AllowPrivateProviderEndpoints`，明确声明不支持私网 Provider 端点。`Policy.AllowPrivate` 本身保留，webhook 那侧在用。

### F-01 [P0] 撤销类变更可能已持久化但未进入运行时，旧权限/旧路由继续生效

【确定缺陷；**已修 `354428c`**——激活改用 runtime 自己的有界 context，拓扑激活入口 `activateTopology` 不再接受任何 context 参数，因此调用方无法再把请求的传进来。若激活因真实原因失败，不一致仍然存在，见 F-03】

Project/Key 的 update/delete 先把 disabled/deleted 状态写入 bbolt，随后调用 `refreshAdminAuth(writer, request)`；该函数用 `request.Context()` 重读所有 Projects/Keys。Topology 的 Provider/Deployment/Route 变更同样先写 Store，再用 `request.Context()` 重建 Registry。

Store 的 `listJSON` 在读取前直接检查 `ctx.Err()`。因此存在完整可达路径：

1. Admin 发起禁用/删除 Key、Project、Route、Deployment 或 Provider；
2. bbolt transaction 已提交；
3. 客户端断开、反向代理取消或请求 deadline 到期；
4. refresh/reload 看到 canceled context 并返回；
5. `auth.Snapshot` 或 `provider.Registry` 没有 swap，旧快照继续服务；
6. 直到下一次成功刷新或进程重启，刚刚撤销的 Key/Project/Route/Provider 仍可能被数据面使用。

这是 fail-open 方向：持久化管理面显示已撤销，正在运行的数据面却仍授权/路由。它还会跳过后续成功 audit，因为 handler 在刷新错误后直接返回。

代码证据：

- Project delete：`internal/app/admin_projects.go:160-173`
- Key update/delete：`internal/app/admin_projects.go:302-317`、`348-360`
- Auth refresh 透传请求上下文：`internal/app/admin_projects.go:454-459`
- Snapshot 全量读取：`internal/auth/snapshot.go:47-71`
- Store 明确拒绝 canceled context：`internal/store/bolt/store.go:1352-1356`
- Route delete 先持久化再 reload：`internal/app/admin_providers.go:716-729`
- Deployment delete 先持久化再 reload：`internal/app/admin_deployments.go:392-405`
- Provider registry reload 不替换失败 candidate：`internal/app/providers.go:27-33`、`67-80`

建议方向：

- durable mutation 成功后，激活工作不应继续依赖客户端请求生命周期；使用有界、独立的内部 context；
- 对撤销类操作，若激活仍失败，不能继续保留旧 snapshot。应进入明确的 fail-closed 状态，或先准备 candidate、在单一协调协议中提交 mutation + swap；
- 增加在 Store commit 后取消请求的回归测试，分别覆盖 Key、Project、Route、Deployment、Provider；测试必须断言数据面立即拒绝，而不只是断言 bbolt 中字段已改变。

### F-02 [P1] 删除最后一个 Route 会破坏 Project.allowed_routes 的引用完整性

【确定缺陷；**已修 `066a08a`**——拒绝删除并返回 `route_referenced_by_project`；重命名别名走同一守卫；两条链统一为 `adminTopologyMu` → `adminProjectMu` 的固定顺序，TOCTOU 已关闭而非收窄】

创建/更新 Project 时，`validateProjectReferences` 会拒绝不存在的公共模型别名，并明确说明“无 Route 的别名只会在请求时静默失败，所以要在这里拒绝”。但反向删除 Route 时没有检查 Project；最后一个同名 Route 可以被删除，留下 Project 仍允许该别名。

此后认证和 `AllowedRoutes` 检查仍通过，但 Registry 没有候选，客户端得到 `model_not_found`。管理面此前承诺的引用完整性只在 Project 写入方向成立。

另外，Project 变更由 `adminProjectMu` 协调，Route 变更由 `adminTopologyMu` 协调。即使仅在 Route 删除前增加一次 Project 查询，若不统一锁/事务，仍可能发生：Project 校验看到 Route → Route 并发删除 → Project 提交，形成 TOCTOU 悬空引用。

代码证据：

- Project 写入方向校验：`internal/app/admin_projects.go:408-440`
- Route 删除无 Project 反向引用检查：`internal/app/admin_providers.go:693-735`
- 两条链分别使用不同 mutex：`internal/app/admin_projects.go:68-74`、`internal/app/admin_providers.go:705-720`
- 请求期先通过 allowed_routes，再因候选为空返回 not found：`internal/gateway/service.go:173-197`

建议方向：

- 明确产品语义：删除最后一个同名 Route 时，是拒绝删除、同时从 Projects 移除别名，还是允许 Project 进入“未配置”状态；
- 若目标是强引用完整性，应把 Project alias 与 Route alias 的变更纳入同一个 topology coordinator/事务检查；
- 回归测试至少覆盖单 Route、同别名多 Route、disabled Route、并发 Project update + Route delete。

### F-03 [P1] durable mutation、runtime activation、audit 三阶段没有统一成功语义

【一致性风险；**未处理**。F-01 和 F-05 各自的可达路径已修，窗口比原来窄得多，但提交协议本身没有建立】

Provider、Deployment、Route、Project 和 Key 的常见顺序是：

```text
Store commit → rebuild/swap runtime snapshot → append audit → return 2xx
```

后两步任一步失败，API 返回错误，但前一步不会回滚：

- rebuild 失败：Store 已变，live snapshot 仍旧；
- audit 失败：Store 已变，snapshot 可能已经生效，但 Admin 收到错误且没有对应成功审计；
- 调用方重试时会遇到 revision 冲突、already exists，或对“一次失败请求是否已生效”无法判断。

这不是要求把 bbolt、Registry 和 Audit WAL 强行做分布式事务；需要的是明确且可验证的提交协议。目前 handler 的 HTTP 结果表达的是“三步都成功”，但 mutation 的真实提交点是第一步，外部无法安全判定。

代码证据：

- Provider create：`internal/app/admin_providers.go:185-199`
- Deployment update：`internal/app/admin_deployments.go:338-362`
- Route create：`internal/app/admin_providers.go:623-637`
- Project create：`internal/app/admin_projects.go:74-87`
- Gateway Key create：`internal/app/admin_projects.go:237-267`

建议方向：

- 先定义提交点和错误响应：例如“Store commit 即成功，后续激活失败进入 degraded/fail-closed 并返回可查询 operation ID”，或采用 prepare-candidate → durable commit → guaranteed swap 的协议；
- Audit 若是强制条件，应在 durable mutation 前准备，或使用包含 mutation intent/outcome 的可恢复协议，避免“已成功但无成功审计”；
- 所有 create 都应具备幂等语义；当前 Gateway Key 已有确定 ID，但 Provider/Deployment/Route/Project 仍需要核对重试行为；
- doctor/status 应同时报告 Store revision 与 active snapshot generation，令运维能识别漂移。

### F-04 [P2] Deployment priority/weight 是无效配置，和 Route 调度字段重复

【确定冗余；**已修 `3580a89`**——字段已删除，不保留兼容。两点与原文不同：控制台从未渲染过这两个字段的编辑器（那两个 i18n 文案只被 locale 对齐测试引用，本身是死字符串），且不需要迁移或重新初始化——已对真实 `halro.db` 验证过旧记录仍可解码】

Deployment API 和 UI 读写 `priority`、`weight`，Domain 也校验并持久化；但 Registry 构造 Target 时只把 `route.Priority` 和 `route.Strategy` 写入 Target。候选排序只读取 Target Priority，round-robin 也只是旋转候选，没有读取 Deployment Weight。

结果是运维人员在 Deployment 页面修改“默认优先级/权重”，请求分配不会发生任何变化。真正生效的控制位于 Route 页面。该字段不仅冗余，还制造错误的控制感。

代码证据：

- Deployment 输入：`internal/app/admin_deployments.go:55-58`
- Deployment 保存：`internal/app/admin_deployments.go:635-645`
- UI 文案和表单回填：`web/src/i18n/locales/zh-CN.ts:668`、`web/src/pages/DeploymentsPage.tsx:178-179`
- Registry 只传 Route priority/strategy：`internal/app/providers.go:312-331`
- Router 只按 Target Priority 排序并按 strategy 轮转：`internal/provider/provider.go:335-347`、`375-394`

建议方向：

- 若调度属于 Route，删除 Deployment 的 `priority/weight` 字段和 UI；
- 若需要加权路由，应在 Route 层设计并实现 `weight`，定义它与 priority、health、operation filtering、fallback 的精确关系；
- 不建议让 Deployment 字段作为 Route 的隐式默认值，否则会形成“复制后何时同步”的第二套语义。

## 4. 待确认项，不作为缺陷计数

### Q-01 disabled Route 是否允许长期被 Project 引用

当前 Project 校验允许绑定 disabled Route，注释认为控制台会显示 unavailable。这可以支持预配置/维护窗口，但需要 UI 和 API 明确区分“已授权但暂不可用”与“不存在”。F-02 讨论的是 deleted/不存在，不反对 disabled 引用。

### Q-02 Provider 创建后是否必须测试

Provider 可以 enabled 创建；实际流量仍必须经过 disabled 新建、测试、定价、启用 Deployment 和 Route，因此 Provider 本身没有强制测试不会直接放行流量。当前边界合理，但 onboarding/doctor 应清楚显示哪一级尚未 ready。

### Q-03 无当前价格的 Route 为何能进入 Registry

Registry 会把无当前版本价格的 Target 以零投影装载，请求期再按 unknown-price policy 选择 fail-closed 或允许未知价格。由于请求期重新选择并 pin，Registry 投影不是计费权威；这属于有意设计。需要确保 UI/doctor 不把“可装载”误报成“可计费调用”。

## 5. 建议的验证矩阵

本轮没有修改代码，因此未新增/执行修复测试。实施修复时建议按以下矩阵验证：

| 场景 | 持久化状态 | 运行时预期 | API/审计预期 |
|---|---|---|---|
| Key disable 后请求取消 | disabled | 立即 401/403 | 有可恢复、唯一的 mutation outcome |
| Project delete 后请求取消 | deleted | 所有 Key 立即失效 | 不可继续使用旧 snapshot |
| Route delete 后请求取消 | deleted | 立即不再成为候选 | 不能继续命中旧 Target |
| Provider/Deployment disable 后请求取消 | disabled | 立即不再调用上游 | 无旧 adapter 新流量 |
| 删除最后一个同 alias Route | 取决于选定语义 | Project 不出现意外悬空 | 明确 409 或原子联动 |
| 同 alias 尚有另一个 Route | 删除一个 | 其余候选继续可用 | Project 引用保持有效 |
| 修改 Deployment priority/weight | 若字段保留 | 必须有定义明确的调度变化 | 否则 API 拒绝/字段删除 |
| Audit append 故障 | 可恢复 intent | fail-closed 或可查询完成 | 不产生“失败但已生效”的歧义 |
| 禁用被启用 Deployment 引用的 binding | 应当被拒绝、不落盘 | 旧 binding 不得继续服务 | 409 且状态未变（`e1d94be` 已覆盖） |
| 从 `bindings` 列表整体移除被引用的 binding | 同上 | 同上 | 同上 |
| 上述任一变更之后重启进程 | 不变 | **必须能启动** | 启动日志点名问题资源，而不是拒绝启动（`e1d94be` 覆盖 Route 级；Provider 级见 F-06，未覆盖） |
| 单 binding Provider 关掉唯一 binding | 应当被拒绝 | 无变化 | 已由 `models.go:446` 覆盖，保留回归 |

## 6. Review 边界

- 仅运行了一次用户明确授权的真实 Provider smoke test；没有执行重试压测或其他计费请求；
- 未对每个资源类端点逐一证明协议兼容性，主 review 聚焦共享控制链；
- 首轮（2026-08-10）未修改业务代码，因此没有执行全量测试门禁；2026-08-11 的复核同样未修改业务代码，只用临时探针执行了 `internal/app` 的定向测试；
- 行号分属两个基线：F-01～F-04 与 §2 的引用来自原基线 `main@b206285b920c`，复核时已确认普遍漂移（例如 F-01 引的 `store.go:1352-1356`，在 `4af0228` 上是 `1387-1390`）；F-05 与 §9 的引用来自 `main@4af0228`。两者都会随后续改动继续漂移，文件与函数名才是稳定定位入口。

## 7. 运行时非计费验证

2026-08-10 使用用户提供的临时 Project Gateway Key，对本机
`POST http://127.0.0.1:8080/v1/chat/completions` 发起无效公共模型别名探针。
请求只用于验证认证与 Project 授权边界，不记录 Key，也不使用任何可能命中
Registry 的真实模型别名。

结果：

```text
HTTP 403
code: model_not_allowed
message: model is not allowed for this project
```

可以据此确认：

1. Key 格式、hash 查找和 `auth.Snapshot` 认证成功；否则会返回
   `401 invalid_api_key`；
2. Key 所属 Project 当前为 enabled；否则认证阶段会拒绝；
3. `Project.AllowedRoutes` 检查发生在 Registry 候选解析之前；
4. 该探针没有进入 attempt、预算 reservation 或 Provider I/O，因此不会产生
   Provider 费用。

该结果不能验证 Route → Deployment → Provider → settlement 的成功路径，也不能
复现 F-01。后者需要在 Admin disable/delete 的 Store commit 与 snapshot refresh
之间注入请求取消，并断言旧 Key/Target 是否仍可使用。

## 8. 一次真实 Provider 全链路验证

2026-08-10 经用户明确授权，使用同一临时 Project Gateway Key，按项目公开 API
执行一次非流式 Responses 调用：

```text
POST http://127.0.0.1:8080/v1/responses
model: Chat
stream: false
input: 用一句话说明 Halro 如何路由这个请求。
```

为避免扩大计费范围，客户端设置 `--retry 0`，只发送一次请求。结果：

```text
HTTP 200
request_id: req_hv15bxsns4v4xsemctvt0fw3fw
response_status: completed
public_model: Chat
input_tokens: 18
output_tokens: 41
total_tokens: 59
```

响应文本：

> Halro 会先识别你的意图与所需能力，再将请求转发给最合适的后端模型或工具链处理，并把结果统一返回给你。

随后只读检查 `data/ledger/ledger.wal`，同一 Request ID 存在完整的五事件闭环：

| 顺序 | Ledger kind | 关键证据 |
|---:|---|---|
| 1 | RequestAccepted | Project、Key、公开模型 `Chat` 已记录 |
| 2 | ReservationCreated | Route、Deployment、Provider、价格快照和独立 attempt reservation 已持久化 |
| 3 | AttemptStarted | Provider I/O 前已标记 attempt started |
| 4 | AttemptSettled | `outcome=success`、`http_status=200`、Provider 报告 18/41 tokens |
| 5 | RequestFinalized | `outcome=success`，请求生命周期闭合 |

实际解析结果：

```text
Chat
  → route rte_hf1pvj0tc413pv7m8jp9hr2jq4
  → deployment dep_j3y5gpbnj5txm160sm822s9t4w
  → provider prv_z52k3d6ezq66e9s5xy72pdns2r
  → upstream model gpt-5.4
```

本次只有 `attempt_number=1`，没有发生 retry 或 fallback。Provider 调用耗时约
6.871 秒。Ledger 先按最大准备输出 16,384 tokens 保守预留 163,990 micros USD，
随后根据 Provider 权威 usage 结算并提交 590 micros USD：输入 180、输出 410
micros USD。这里的金额是 Halro 按当前 Deployment 手工价格快照计算的本地记账值，
不等同于对 Provider 账单的独立核验。

该实测确认了以下主链：

```text
Gateway Key → Project → public model authorization → Route → Deployment
→ Provider/upstream model → Provider response → usage settlement
→ RequestFinalized
```

它不覆盖 F-01 的撤销竞争窗口，也不覆盖多候选 retry/fallback、流式 delivery
boundary、Redaction 拒绝和 Token Guard 拒绝路径。

## 9. 2026-08-11 全链路复核记录

在 `main@4af0228` 重走全链路，并把结论从“读代码推断”提升为“执行验证”。走查底稿描述的链路顺序未发现错误：`resolveRequest`（`gateway/service.go:173`）的认证→别名→CIDR→候选五步、`startAttempt`（`:307`）的熔断→并发→价格 pin→Token Guard 复核→durable reservation→pin commit→`MarkStarted`→Provider I/O 十步、`retryable()`（`:1751`）对 `Ambiguous` 直接返回 false、流式以 `emitted` 闸住 delivery boundary（`:1559`、`:1576`），都与底稿一致。

四条原 finding 全部仍然成立，且两条的实测结果比原文更具体：

- **F-01**：durable revoke 落盘后，以取消的 request context 调用生产函数 `refreshAdminAuth`，返回 `ok=false`、写出 503，而被吊销的 Key **在 live snapshot 上仍然认证成功**；随后用未取消的 context 刷新一次即拒绝（`gateway key is disabled`）。缺的确实只有激活这一步。
- **F-02**：删除别名最后一条 Route 返回 204，Project 的 `allowed_routes` 仍为 `[chat]`，Registry 候选归零——与原文一致。**原文未提的一点**：该 Project 从此存不进去了，把它原封不动重新保存返回 `409 allowed_routes references unknown model alias chat`。运维要改这个 Project 的预算、限流或 CIDR 都被挡住，唯一出路是先摘掉悬空别名。这使 F-02 从“请求期报错不准确”升级为“Project 记录被锁死”。
- **F-04**：`.Weight` 全仓库仅两处非测试读取——`admin_deployments.go:564` 把输入拷进记录、`domain/models.go:866` 校验非负。`provider.Target` 结构中没有 Weight 字段，排序只读 `route.Priority`，round-robin 只旋转候选数组。确认为纯死字段。
- **F-03**：本轮以三种独立形态各撞见一次（F-05、F-01、以及 F-05 之后一次无关的 Route PUT），都是同一形状：API 报错、变更已落盘。

新增 F-05，见上。复核所用探针只读状态、只打日志、不修改业务代码、不发起任何 Provider 调用，验证完成后已删除；若要修复，应改写为带断言的回归测试。

## 10. 修复记录（2026-08-11）

四条已修，各自单独提交并推送到 `main`：

| 提交 | Finding | 做法 |
|---|---|---|
| `e1d94be` | F-05 | 单条 Route 造不出 Target 时改为 withheld 而非致命（`referenceWithholding`，审计 `route.reference_withheld`，与漂移指标分开）；新增 `validateBindingsCanDeactivate`，与 Provider/Deployment 两级守卫同形并点名挡路的 Deployment |
| `354428c` | F-01 | 激活改用从 runtime `backgroundCtx` 派生的 30s 有界 context；拓扑激活统一走 `activateTopology()`，该函数不接受 context 参数，调用方无法再传请求的进去；激活失败在两条路径上都记 error 日志 |
| `066a08a` | F-02 | 删除别名最后一条 Route 时返回 409 `route_referenced_by_project`，点名别名和 Project；重命名别名走同一守卫；两条链统一 `adminTopologyMu` → `adminProjectMu` 固定顺序 |
| `3580a89` | F-04 | 删除 `Deployment.Priority/Weight` 及其校验、API 字段、前端类型与死 i18n 文案；Route 的 priority 保留并有测试钉住它仍进入 Target |

每条都做了反向验证，且**先断言退改真的生效**（脚本用 `assert needle in s`，避免搜索串失效导致「什么都没改却通过」），再确认测试在缺陷态失败：

| 退掉的修复 | 缺陷态表现 |
|---|---|
| F-05 守卫 | `PUT /providers/{id}` 返回 200，chat binding 落盘为 `enabled=false` |
| F-05 withhold | 装载、热重载、**重启**三者全部 `route "..." references an unavailable provider binding` |
| F-01 | `refreshAdminAuth` 返回 503 `metadata unavailable`，被吊销的 Key 仍认证成功 |
| F-02 | 删除返回 204、重命名返回 200 且别名已改 |

门禁：每条提交前跑 `go test ./...`（54 包）+ `go vet ./...` + 前端 typecheck/test/build，并核对 `internal/webui/dist` 无漂移。race detector 只在 F-05、F-01、F-02 跑（`go test -race ./internal/app/`），F-04 未跑——删字段不触及并发。

F-04 的 no-migration 结论是对真实数据验证的，不是对 fixture：复制线上 `data/` 后用新结构体解码 `halro.db`，两条真实部署记录的存储 JSON 仍带 `weight`/`priority`，均能解码且 `Validate()` 通过，其余字段完好。`halro doctor` 未能用上——本机有一个 `go run ./cmd/halro start` 实例持有真实数据目录的锁，故改用直接解码真实字节的方式回答同一问题。

未处理：F-07（`allow_private_provider_endpoints` 无法生效，需先定产品意图）、F-06（Provider 级装载失败仍致命，但降级为加固建议）、F-03（提交点语义，设计决定）。

一条关于本轮方法的记录：F-06 原本被判为 P1 确定缺陷并已写进文档，动手修之前按惯例先复现，结果**前提被证伪**——它声称的 endpoint 场景根本建不出私网 Provider。如果当时直接按文档去实现「provider 级 withheld」，就会在一个假前提上改掉装载语义，而且这个改动本身很难被证伪。复现同时暴露了 F-07 这个真实缺陷。这正是仓库「verify, never assume」那一条要买的东西：**一份没跑过的 finding 是假设，不是结论**，包括本文档自己写下的。
