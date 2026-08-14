# 阶段 4：对抗验证裁决

对 [findings-backend.md](findings-backend.md) 的四条"问题"级发现（F1、F2、F7、F11）各做一次证伪式
验证：默认发现为错，要求在代码中复现完整可达路径，或找出拦截防御。裁决
CONFIRMED / REFUTED / PARTIAL。F19 为结构事实，不经对抗，直接进设计裁决（连同 W4、W8）。

---

## F1 · 墓碑编辑五跳不一致 — **CONFIRMED（实证）**

**验证方式**：临时探针测试（`zz_review_tombstone_probe_test.go`，已删除）经真实 admin router 执行：
1. 经真实 DELETE handler 墓碑化 bootstrap Route，随后用当前 revision PUT——
   **返回 200**，`public_model` 改为 `renamed-after-death`，revision 2→3，`deleted_at` 保留；
2. 同法墓碑化 Project 后 PUT——**返回 200**，name 改写，revision 前进。

**证伪尝试与失败原因**：唯一候选拦截是"操作者拿不到墓碑的 revision"——不成立：DELETE 响应自带新
ETag（`admin_projects.go:192`、`admin_providers.go:974`）。Provider 更新 handler 同构缺失该检查
（`admin_providers.go:245-250`，未实测但代码路径与 Route 一致）。

**影响修正**：与原判一致——运行时无影响（registry 与 auth 快照都过滤墓碑），危害为审计噪声
（对已删对象记 `*.update` 成功事件）与僵尸记录可变。**审计维度比原判略重**：审计链上会出现
"删除后仍被修改"的事件序列，事后取证需要额外解释。
**修法确认**：Provider/Route/Project 三个 update handler 各加一行 `current.DeletedAt != nil` → 404，
与 Deployment（`admin_deployments.go:222`）、Key（`adminProjectKey`）对齐。

---

## F2 · 空 BindingID 回退拼写差异（Enabled 维度）— **REFUTED（作为可达缺陷）**

**攻击路径构造**：目标状态 = 启用的 Deployment 绑在 disabled binding 上。候选路径
"disable deployment → disable binding（守卫 `admin_providers.go:1553` 跳过 disabled deployment，
放行）→ re-enable deployment"。

**拦截防御（三层，全部命中）**：
1. 更新必经 `deploymentFromInput`（`admin_deployments.go:240`），其三种解析分支**全部**过滤
   disabled binding：detection 路径 `:694`、variant 路径经 `invocationTargetBindings`
   （`admin_invocation_targets.go:275-287`，零候选即 `provider binding is unavailable`）、catalog
   路径 `:1007`；
2. 目标身份不可变门（`:249-258`）阻止换绑定绕行；
3. 启用门要求当期健康测试（`:297-301`），而 disabled binding 无 adapter 可测。

直接 store 写入可造出该状态，但 registry 加载对其 withhold（`providers.go:548-562`），fail-closed。
**无任何受支持路径可达，可达时也不放大权限。**

**降级**：问题 → **建议**（与 F3 合并）：`validateDeploymentProviderProfile` 补
`deployment.Enabled && !binding.Enabled` 拒绝，把三种拼写收敛成一种——理由是防御纵深与单一事实，
不是修可达缺陷。

---

## F7 · 加载路径不复验、空能力整套采纳 adapter 能力 — **PARTIAL**

**证伪尝试**：为"存量记录带空能力集"找一条受支持的生成路径。
- 所有 store 写入过 `Deployment.Validate`（强制 AnyOperation，`models.go:952`）——堵死；
- 迁移路径：`normalizeDeploymentProfile`（`store_pricing.go:97`）对 legacy 记录用
  `instance.Capabilities` 回填，而 legacy provider 经 `normalizeProviderProfile` 拿到
  `DefaultProviderCapabilities`（已知类型非空）——堵死；
- 恢复路径：`.hmbk` 恢复是整目录替换（`app/backup.go:145-241`），有效存档里只有已验证记录——堵死；
- 剩余入口仅剩 bbolt 被篡改或外部工具手写。

**维持成立的部分**：`loadProviderRegistry` 确实不复验（信任 store 内容），且
`deploymentCapabilities` 的空声明分支（`providers.go:767-769`）在被触达时**方向是 fail-open**
（整套采纳 adapter 能力），与同文件对凭据不一致的 refuse、与 `Registry.Register` 对空交集的拒绝
（`provider.go:340-351`）方向都相反。

**裁决**：不可达（经支持路径）+ 真实的防御方向不一致 = PARTIAL。修法维持：该分支改 withhold，
一行改动消除整个讨论。严重度确认为低。

---

## F11 · 资源平面缺策略快照第二道闸 — **PARTIAL（非对称成立，直接可达被否）**

**拦截面核查**：激活域有四个（`activation_state.go:42-45`），redaction 与 token_guard 的重载失败
都会 `markStale`（`admin_redaction.go:332`、`admin_token_guard.go:350`），而
`refuseWhileSnapshotsStale` 挂在**两个**数据平面门面上、files/batches 路由在 guarded 组内
（`runtime.go:1304-1331`）。因此**所有被追踪的失稳**在资源平面同样被整体拒绝——直接构造可达路径
失败。

**维持成立的部分**：`assertPolicySnapshotsCoverProject` 存在的自述理由是拦"未被追踪"的情形——
"or one of those guards regressed"（`service.go:184-193`）。该保险只装在推理入口
（`service.go:231` 唯一调用点）；资源平面的 redaction 调用（`inference_resources_store.go:194-204`、
`redactBatchResults :864`）在守卫回归情形下按引擎语义静默 fail-open。两个平面对同一失效类的防御
纵深不等。

**裁决**：作为"可直接触发的缺陷"REFUTED；作为"防御非对称"CONFIRMED。修法维持且成本极低：
`resourcePrincipal`（`inference_resources_store.go:157`）加同一断言。运行时验证（人为制造快照回退）
不再必要——两平面的第一道闸已静态确认等价，差异只在假想的回归情形。

---

## 裁决汇总与对阶段 3 的收窄

| 发现 | 裁决 | 处置 |
|---|---|---|
| F1 | **CONFIRMED**（实证 200） | 修：三个 handler 加墓碑 404 |
| F2 | **REFUTED**（三层拦截） | 降为建议：store 层收敛拼写 |
| F7 | **PARTIAL**（不可达但方向反） | 修（低优先）：加载分支改 withhold |
| F11 | **PARTIAL**（非对称成立） | 修：resourcePrincipal 加断言 |

历史规律再次成立：四条无一"原样成立"——一条实证坐实、一条被三层防御证伪、两条收窄为
防御纵深问题。

**阶段 3 剧本收窄**：
- 原 R4 变体（disabled binding 复活路径）——F2 证伪已覆盖，**删除**；
- F11 运行时验证（人为快照回退）——两平面第一道闸已静态确认等价，**删除**；
- 保留 R1–R3、R5–R8：它们验证的是静态审查未触及的**真实二进制行为**（记账/审计内容、
  安全传输下的建链、真实 TOTP/step-up 流程），不与本轮裁决重叠。

**最终修复清单（按严重度 × 成本）**：
1. F11 — `resourcePrincipal` 加策略快照断言（一处调用）；
2. F1 — Provider/Route/Project update 各加墓碑 404（三行）；
3. F2+F3 — store 层引用检查统一排除墓碑与 disabled binding（收敛拼写）；
4. F7 — 加载期空能力 withhold（一个分支）；
5. F19+W4+W8 — 设计裁决项：`allowed_routes` 改名 `allowed_models`（durable schema，需再初始化
   计划）、前端放开或文档化两处更严约束。
