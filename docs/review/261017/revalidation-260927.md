# 260927 结论复核（R-001–R-014）

> 复核对象：冻结 `main` `2e749db1`。方法：读最终代码；对仍存在的每项修复，找到守护测试并 `-count=1` 运行；对 R-002、R-005、R-006、R-007、R-008、R-013、R-014 做反向验证——把修复改回缺陷形态（每次以 `git diff --shortstat` 确认改动确实落地），确认测试失败，再还原。反向验证只在 scratch 的冻结 worktree 中进行，未进入评审分支。

## 结论表

| ID | 原级别 | 今天的结论 | 证据 |
| --- | --- | --- | --- |
| R-001 | P0 | **随 HA 移除；单机无同类问题** | 原缺陷是 `in_flight` 未进入 HA confirmed 前缀即调用上游，提升后可复活为 `reserved`。单机没有提升；`noteInferenceDispatch`（`internal/gateway/inference_idempotency.go:63-74`）在三处上游调用前（`internal/gateway/service.go:1394,2452,2639`）以 bbolt 提交 `in_flight`，store 未设 `NoSync`（`internal/store/bolt/store.go:1593-1596`），标记失败即请求失败。HA 专用测试 `TestProviderResourceWaitsForConfirmationBeforeInFlightReturns`、`TestProviderInFlightBoundaryWaitsForConfirmation` 随 #404 删除，正确 |
| R-002 | P1 | **仍有效** | `resourcePrincipal` 要求 `inference`（`internal/gateway/inference_resources_store.go:268-270`）；`TestEveryResourceFamilyRequiresInferenceScope` 通过，去掉检查后失败 |
| R-003 | P1 | **随 HA 移除** | planned stepdown 不存在；`installPlannedStepdownDrain` 与四个 drain 测试随 `52c45f42` 删除，正确 |
| R-004 | P1 | **随 HA 移除；单机无同类问题** | 原缺陷是会话/MFA 一次性状态在提升后从未确认后缀复活。单机这些写入是 bbolt 已提交事务，不存在未确认后缀；HA 专用测试 `TestAdminOneTimeStateTransitionsWaitForConfirmation`、`TestAuthorityWithdrawingDeletesRequireConfirmation` 随 #404 删除，正确 |
| R-005 | P2 | **仍有效** | 文档内 expiry 进入 `Credential.ExpiresAt`，与顶层值冲突时拒绝（`internal/app/admin_providers.go:1384-1397`）；`TestClaudeSubscriptionCredentialPersistsItsDocumentExpiry` 通过，跳过 `materialExpiry` 后失败 |
| R-006 | P2 | **仍有效** | 严格正整数版本（`internal/config/retirement.go:311-317`）；符号链接、属主、mode、fsync 失败清理等 9 个 `TestMigrate*` 通过；把解析改回 `Sscanf` 后 `TestMigrationRequiresACompletePositiveIntegerVersion` 以 `"1junk"`、`"1.0"` 失败 |
| R-007 | P2 | **仍有效**（#404 改动后重点复核） | #404 的 `202a1419` 让离线 clear 改回直接开库，但删除与审计意图仍在同一事务（`internal/app/route_suspensions.go:98-111` → `DeleteRouteSuspensionWithAuditIntent`），投递失败保留待处理意图。`TestOfflineRouteSuspensionClearKeepsAnIntentWhenAuditDeliveryFails` 通过；把 intent 换成 nil 后以 `pending intents=[]` 失败 |
| R-008 | P2 | **仍有效** | 持久挂起读失败返回存储错误（`internal/app/admin_route_suspensions.go:57-65`）；`TestRouteSuspensionListingFailsClosedWhenTheDurableStateCannotBeRead` 通过，去掉 `adminStoreError` 后失败 |
| R-009 | P2 | **仍有效** | `web/src/pages/RouteSuspensionsPanel.test.tsx` 17 个测试通过，含"确认后按句柄清除"与"短时 live-only 挂起不提供清除" |
| R-010 | P2 | **门禁仍有效；问题在本范围内复现** | `release.yml` 仍拒绝缺失 `## Changelog coverage` 或含 `TODO` 的评估（`.github/workflows/release.yml:96-107`）。但 `main` 的 Unreleased 再次缺项：P2、P3、对账、控制台两项、Go 1.26.9 均无条目，是在 #426 的逐提交表中才发现的。门禁在发布时起作用，`main` 上的 CHANGELOG 漂移仍会发生 → 见 V-008 |
| R-011 | P2 | **仍有效** | 原列 8 条告警各有 3–4 个 `alert_rule_test`；本范围新增的 `HalroLedgerHotStateHigh`/`Rejecting` 各 2 个。缺测试的 `HalroReloadFailing`、`HalroTLSCertificateExpired`、`HalroTLSCertificateExpiringSoon` 在 v0.8.5 已存在且已缺测试，不在本范围 |
| R-012 | P2 | **随 HA 移除** | seed 不存在 |
| R-013 | P2 | **仍有效** | `discovery ⇒ inference` 在 domain 校验（`internal/domain/models.go:1516-1520`）；`TestGatewayKeyDiscoveryScopeRequiresInference` 通过，去掉蕴含检查后失败 |
| R-014 | P2 | **仍有效** | 固定上限在认证后、scope/CIDR 前（`internal/gateway/models.go`、`inference_resources_store.go:265-267`）；`TestAuthenticatedDiscoveryDenialsAreBoundedByTheBuiltInKeyCeiling` 通过，把上限移到 scope 检查之后失败 |

## 小结

- 10 项仍有效（R-002、R-005–R-011、R-013、R-014），其中 7 项经反向验证证明测试确实守护修复；
- 4 项随 HA 移除（R-001、R-003、R-004、R-012），其中 R-001、R-004 检查了单机下的同类问题，不存在；
- 没有"回退后失效、需要重修"的项；
- R-010 的门禁有效，但同类问题在本范围复现（V-008）。

## F 项前提复核

| F | 结论 |
| --- | --- |
| F09 Advisor | 不依赖用量 checkpoint/rollup（`internal/advisor` 无引用）；45/60/90 s 桶仍在 `internal/usage/aggregate.go:44`，P1 删除 rollup 不影响 |
| F10 可观测性 | `deploy/observability`、`docs/runbooks`、`docs/observability`、`docs/contracts/metrics-reference.md` 引用的每个 `halro_*` 指标都由代码导出（仅 4 个通配前缀）；无 `halro_usage_window_*`/`halro_usage_checkpoint_*` 悬空引用 |
| F11、F12 | 已移除，见 `revert-integrity.md` |
| 其他 | 在阶段 2–3 的净 diff 与交叉点中复核，结论见 `range-map.md` |
