# Heimdall Master Key Key Slots PRD 1.1.0：未完成项收口

状态：Implementation complete — 外部生产验收仍 blocked

日期：2026-08-04

基线文档：`docs/prd-master-key-key-slots.zh-CN.md`

关联文档：

- `docs/prd-kms-envelope-integration.zh-CN.md`
- `docs/milestone-m11-master-key-custody-aws-kms.md`
- `docs/runbooks/m11-kms-key-lifecycle.md`
- `docs/runbooks/m11-kms-disaster-recovery.md`
- `docs/evidence/m11-07-production-readiness-2026-08-03.md`

## 1. 文档目的

本 PRD 只收录对 Key Slot 1.0 实现回归后确认的未完成项，不重复已经完成的统一 Master Key、File 模式、AWS Adapter、双 Slot 初始化、rewrap、DEK rotate、doctor、backup、restore、Audit、Metrics 和本地故障注入能力。

1.1.0 必须关闭以下四类缺口：

1. Slot 生命周期缺少可审计、可操作的 revoke 闭环；
2. Recovery 启动描述与当前离线恢复实现不一致；
3. Admin UI 是否承担 Master Key 管理职责边界不清；
4. 真实 AWS、独立恢复操作者和发布供应链证据尚未完成。

本 PRD 不得用“单元测试通过”“PR 已合并”或“存在底层函数”替代可执行运维入口和真实生产证据。

## 2. 当前已完成基线

以下能力视为 1.1.0 前置基线，不在本版本重复实现：

- File 模式最终配置、初始化、启动、轮换、备份和恢复；
- 严格拒绝旧 `storage.master_key_file` 和模式字段混用；
- provider-neutral KMS contract、AWS KMS Adapter 和依赖边界；
- versioned descriptor 与 `pending → active → retiring → revoked` 领域状态机；
- descriptor/Slot revision、bbolt 事务和最后可用 Slot 保护；
- Primary/Recovery 双 Slot 原子初始化；
- protected payload、Encryption Context、fingerprint 和 Vault Key Check；
- `ProductionReady()` 强制 active、verified Primary 和 Recovery；
- `key rewrap`、`key rotate`、COW、crash bridge、compaction 和 kill-point 恢复；
- `doctor`、backup、Primary/Recovery restore；
- Recovery 验证和 restore 的高严重度 Audit；
- KMS typed errors、有界重试、Secret Canary、Metrics 和告警；
- AWS IAM/Key Policy、Kubernetes、systemd 和发布工作流模板。

对上述基线的修改必须保持现有 fail-closed、安全清理和 crash recovery 不变量。

## 3. 范围与优先级

### 3.1 P0：发布前必须完成

- 离线 Slot revoke 应用服务与 CLI；
- revoke 的 revision、加强确认、独立解锁、Vault Key Check 和原子 Audit；
- Recovery 流程契约收敛为可执行命令链；
- 真实 AWS 14 项矩阵；
- 独立操作者 Recovery restore 演练；
- Recovery 临时权限撤销证据；
- 最终 RC Secret Canary、SBOM、checksum、Sigstore 和四方签署。

### 3.2 P1：运维可见性

- Admin UI 只读 Master Key Custody 状态页；
- Primary/Recovery 健康、最近验证、pending/retiring 状态和 Runbook 入口；
- 不允许通过 UI 修改 ARN、allowlist、Workload Identity 或执行 break-glass。

### 3.3 非目标

- Admin UI 在线 rewrap、rotate、revoke 或 Recovery 授权；
- 在 UI、API 请求或数据库中保存 AWS 静态凭据；
- 自动 Primary → Recovery fallback；
- File → KMS 在线迁移；
- Passphrase、助记词、Shamir、TPM/HSM/KMIP；
- GCP/Azure Adapter；
- active-active、多写者或 HPA。

## 4. P0-1：Slot revoke 运维闭环

### 4.1 问题

当前领域层和 bbolt 层已经实现 `RevokeSlot`/`RevokeKeySlot`，但生产应用层没有调用者，CLI 也没有 revoke 命令。`key rewrap` 会把旧 Slot 置为 `retiring`，Runbook 要求恢复窗口结束后显式 revoke，但操作者没有受支持的执行入口。

底层能力存在不等于生命周期完成。没有 revoke 入口会导致：

- 旧 wrapped ciphertext 和 Key reference 无法按策略清理；
- `retiring` Slot 长期累积；
- Runbook、Audit 和实际状态不一致；
- 无法证明旧 KMS Key/Grant 已退出 Heimdall 信任集合；
- 多次 rewrap 最终可能触达 descriptor Slot 数量上限。

### 4.2 CLI 契约

新增离线命令：

```bash
heimdall key slot revoke \
  --config /etc/heimdall/config.yaml \
  --slot-id slot_aws_primary_2026q2 \
  --expected-descriptor-revision 12 \
  --expected-slot-revision 3 \
  --confirm-slot-id slot_aws_primary_2026q2
```

要求：

- 仅允许 `retiring → revoked`；
- `--slot-id` 和 `--confirm-slot-id` 必须完全一致；
- descriptor revision 和 Slot revision 必填；
- 不提供 `--force`；
- 不允许直接 `active → revoked`；
- 不允许 revoke 后缺少 active Primary 或 active Recovery；
- 不允许 revoke 最后一个可解锁、已验证 Slot；
- 不调用 AWS `DisableKey`、`ScheduleKeyDeletion`、`PutKeyPolicy` 或 Grant API；
- 命令输出不得包含 ARN、wrapped ciphertext、provider parameters 或 fingerprint。

### 4.3 执行顺序

1. 加载并严格校验配置；
2. 获取唯一数据目录离线锁；
3. 读取 descriptor 并验证 expected revisions；
4. 选择当前仍 active 的独立同代解锁路径；
5. 完成 protected payload、fingerprint 和 Vault Key Check；
6. 验证目标 Slot 为 retiring 且 revoke 后仍 production-ready；
7. 在同一受控发布操作中写入 `security.master_key_slot.revoked` Audit 和 descriptor；
8. 清除目标 Slot 的 wrapped ciphertext、Key reference、algorithm 和 provider parameters；
9. 验证持久化 descriptor 与 Audit 链；
10. 按既有 COW/compaction 规则清理退休页面；
11. 返回非敏感结果并提示 AWS Key/Grant 的外部处置仍需独立审批。

### 4.4 幂等与崩溃恢复

- 同一 Slot 已 revoked 时，相同 revisions/确认不得创建第二条逻辑变更；
- revision 不匹配必须 fail closed；
- Audit 成功但 descriptor 未发布、descriptor 发布但进程未返回等 kill point 必须有确定恢复语义；
- 普通失败不得留下无法判断是否已 revoke 的状态；
- 重试不得重新引入已清除的 provider material。

### 4.5 Audit 与 Metrics

Audit 最低字段：

```text
action=security.master_key_slot.revoked
target_type=master_key_slot
target_id=<slot-id>
outcome=success|failure
reason_code=retirement_window_completed|incident_retirement
```

禁止记录完整 ARN、账号、ciphertext、payload、Workload Identity token 或 Master Key fingerprint。

现有 `heimdall_kms_slot_state{purpose,state}` 和 pending/retiring 告警必须能反映 revoke 后状态；不得使用 Slot ID 作为 Metrics label。

## 5. P0-2：Recovery 契约收敛

### 5.1 当前不一致

基线 PRD 描述“Recovery 启动后修复 Primary”，当前实现只提供：

- `key recover`：离线验证 Recovery Slot 并写 Audit；
- `restore --use-recovery-slot`：显式使用 Recovery 恢复备份；
- `key rewrap --purpose primary`：使用独立 Recovery Slot 解锁并创建新的 Primary；
- `start`：只选择 Primary Slot。

1.1.0 选择安全边界更窄的离线恢复模型，不新增携带 Recovery 权限运行 Gateway/Admin 的模式。

### 5.2 冻结决策

Recovery 标准流程为：

```text
停止 Heimdall
  → 临时启用受控 Recovery 身份
  → key recover 验证 Recovery/Vault/Audit
  → key rewrap --purpose primary 创建并验证新 Primary
  → 撤销 Recovery 临时权限
  → 使用 Runtime Primary 身份正常 start
```

因此：

- 不实现 `start --use-recovery-slot`；
- 不允许 Gateway/Admin 在 Recovery Role 下长期运行；
- 不允许自动 fallback；
- 基线 PRD 中“Recovery 启动”“启动后修复 Primary”改写为“Recovery 离线解锁并修复 Primary，撤权后正常启动”；
- 如果未来确需 Recovery Runtime，必须另立威胁模型和发布门禁。

### 5.3 Recovery 命令结果

`key recover` 成功结果必须包含：

- `slot_id`；
- `verified_at`；
- `vault_verified: true`；
- `recovery_audited: true`；
- 下一步为修复 Primary 和撤销 Recovery 权限的明确提示。

不得输出 ARN、Master Key、ciphertext、CloudTrail 原始响应或身份材料。

### 5.4 恢复 Primary

使用 Recovery 身份修复 Primary 时：

- 新 Primary Key ARN 必须已在可信配置 allowlist；
- 新 Primary Slot ID 必须与配置完全匹配；
- Recovery 只作为独立 source Slot；
- 新 Primary 必须经历 pending、独立 unwrap、fingerprint 和 Vault Key Check；
- 新 Primary active 前旧 Primary 不得被覆盖为可信状态；
- 成功后必须可以在不具备 Recovery Decrypt 权限的 Runtime 身份下冷启动。

## 6. P1：Admin UI 边界

### 6.1 决策

Master Key 是启动 Admin API 之前的根信任，因此 Admin UI 不负责初始化或修改 KMS 信任配置。1.1.0 的 Admin UI 只提供只读可见性，不提供根密钥写操作。

### 6.2 最小只读页面

建议页面名称：`Master Key Custody` / `主密钥托管`。

允许展示：

- 当前模式：`file` 或 `key_slots`；
- descriptor/local custody ready 状态；生产准入必须单独显示且不得替代真实 AWS、独立演练、RC 和签署门禁；
- Primary/Recovery 的 purpose、state 和脱敏 provider；
- 最近独立验证时间；
- 是否存在 pending/retiring Slot；
- Recovery 验证是否过期；
- rewrap/rotate 是否存在未完成操作；
- 对应 Runbook 和离线 CLI 文档入口。

禁止展示：

- 完整 ARN、AWS account、wrapped ciphertext 或 Encryption Context；
- Workload Identity token、Credential 或 Key Policy 内容；
- Master Key fingerprint 原值；
- 可复制的秘密或内部 provider response。

禁止操作：

- 切换 `file`/`key_slots`；
- 修改 Primary/Recovery Slot 或 allowlist；
- rewrap、rotate、revoke；
- 启用 Recovery Role；
- 创建、禁用或删除 AWS KMS Key。

### 6.3 API 与权限

- 只读 API 必须复用已验证的 descriptor 状态，不触发 KMS 调用；
- 响应使用低敏、稳定 DTO，不直接序列化内部 descriptor；
- 必须通过现有 Admin Session、MFA 和 CSRF 读取边界；
- 页面加载失败不得影响 Gateway 或 Admin 其他页面；
- UI 必须明确区分 `KEK rewrap` 与 `Master Key/DEK rotate`；
- 不具备该页面时，CLI 仍是唯一受支持的生命周期操作入口。

## 7. P0-3：真实 AWS 生产验收

### 7.1 环境前提

必须在审批的目标环境预先配置：

- 有效 AWS Workload Identity；
- 一个现有 Primary customer-managed symmetric KMS Key；
- 一个不同的 Recovery KMS Key；
- 一个不同的 Replacement Primary KMS Key；
- 独立 Runtime、Recovery 和 Lifecycle Role；
- CloudTrail 只读证据访问；
- 独立恢复操作者。

不得在对话、日志、工单或 evidence bundle 中提交访问密钥、token、明文 Master Key 或完整 ciphertext。

### 7.2 必须通过的 14 项场景

1. Primary unwrap；
2. Recovery unwrap；
3. Workload Identity 尚未就绪；
4. permission denied；
5. throttling；
6. Primary disabled；
7. Primary pending deletion 或等价不可用状态；
8. Encryption Context mismatch；
9. ciphertext/protected payload tamper；
10. 来自其他 Vault 的合法 Key；
11. Runtime 启动后 KMS 不可用，Gateway 继续服务；
12. 冷启动必须重新解锁且失败时不启动 Listener；
13. rotate publication kill points；
14. Primary 和 Recovery restore。

每项必须绑定同一 RC commit，记录 UTC、操作者、环境、稳定错误分类和脱敏 CloudTrail metadata。

### 7.3 独立恢复演练

独立操作者仅依照已发布 Runbook 完成：

1. 验证 Primary 不可用；
2. 临时启用 Recovery 身份；
3. 验证 Recovery Slot；
4. 从不可变备份恢复；
5. 修复或 rewrap Primary；
6. 使用 Runtime 身份冷启动；
7. 验证 Vault、Audit、WAL、Usage 和 Parquet；
8. 创建并验证新备份；
9. 撤销临时 Recovery 权限；
10. 归档撤权和 CloudTrail 证据。

演练失败时不得修改原 live tree，不得用当前 descriptor 覆盖历史备份 descriptor。

## 8. P0-4：最终 RC 与发布门禁

最终 evidence bundle 必须绑定同一个完整 commit SHA 和签名 tag，至少包含：

- Test、Race、Vet、KMS boundary 和 kill-point matrix；
- 真实 AWS 14 项场景；
- logs/errors/Audit/Metrics/bbolt/backup/heap Secret Canary；
- EKS 与 VM 部署证据；
- Primary/Recovery restore；
- 独立操作者和 Recovery 撤权；
- 所有发布制品 SHA-256；
- SPDX SBOM 审核；
- checksums verification；
- 每个 blob 的 Sigstore bundle 与 verification；
- Security、Backend、SRE、Release 四方签署。

任何占位值、跨 commit 证据、未验证签名、缺失操作者、raw AWS ARN 或未撤销 Recovery 权限都必须 fail closed。

## 9. 测试要求

### 9.1 Slot revoke

- active 不能直接 revoke；
- retiring 可以 revoke；
- revoked 幂等重试；
- descriptor/Slot revision 冲突；
- 最后可用 Slot保护；
- Primary/Recovery production-ready 保护；
- exact confirmation；
- independent source unwrap 与 Vault Key Check；
- wrapped material 清理；
- Audit/descriptor 原子性；
- 每个 publication kill point；
- 无 ARN、ciphertext、payload 或 fingerprint 泄露。

### 9.2 Recovery

- Runtime/Bootstrap/Admin 永远不自动选择 Recovery；
- `key recover` 错误确认 fail closed；
- Recovery 验证成功写高严重度 Audit；
- Recovery 身份可以离线修复 Primary；
- 撤销 Recovery 权限后 Runtime Primary 冷启动成功；
- Primary 仍不可用时 Runtime 不启动 Listener；
- 恢复失败不修改 live tree。

### 9.3 Admin UI

- 只读 DTO 不包含敏感字段；
- 页面不触发 KMS 调用；
- `file`/`key_slots`、healthy/degraded/pending/retiring 状态；
- rewrap 与 DEK rotate 文案严格区分；
- API/UI 错误不会导致全局页面白屏；
- 键盘、窄视口和屏幕阅读器基本可用。

### 9.4 回归

- `go test ./...`；
- Key Slot/App/Backup/Store 关键包 Race；
- `go vet ./...`；
- Web 全量测试和生产 bundle；
- `check-kms-boundaries.sh`；
- `check-production-assets.sh`；
- Prometheus/Alertmanager rules；
- Linux host-security 交叉编译；
- `govulncheck` 0 个 reachable vulnerability。

## 10. 实施切片

### KS-1：Slot revoke 应用闭环

- App Service、CLI、Audit、revision/confirmation；
- COW/compaction 与 kill-point 测试；
- 生命周期 Runbook 增加完整命令。

完成门禁：操作者无需直接调用 bbolt 或内部 Go API，即可安全结束 retiring Slot 生命周期。

### KS-2：Recovery 契约收敛

- 更新基线 PRD 中的 Recovery 启动描述；
- 明确 `recover → rewrap Primary → revoke permission → start`；
- 增加端到端 fake KMS 测试和 Runbook。

完成门禁：没有 Recovery Runtime，仍能在 Primary 永久失败时完成受控恢复并以新 Primary 冷启动。

### KS-3：只读 Custody UI

- 安全 DTO、Admin API、页面、可访问性和错误隔离；
- 不包含任何根信任写操作。

完成门禁：操作者能看见状态和下一步 Runbook，但无法通过 UI 扩大根信任或 Recovery 权限。

### KS-4：真实 AWS 与发布证据

- 14 项矩阵、独立恢复、撤权、RC evidence、供应链验证和四方签署。

完成门禁：`release-evidence/verify.py` 对最终 bundle 成功，且不存在占位值或跨 commit 证据。

## 11. 验收标准

- [x] 提供受支持的离线 Slot revoke CLI 和 App Service。
- [x] revoke 要求 exact confirmation、descriptor revision 和 Slot revision。
- [x] revoke 前使用独立 active Slot 解锁并通过 Vault Key Check。
- [x] revoke 后仍有 active、verified Primary 和 Recovery。
- [x] revoke 在 stage 中原子写 descriptor 与 Audit intent、清除 wrapped/provider material、compact 后一次发布，并可恢复交付最终 Audit。
- [x] rewrap 的 added、verified、retiring 均先原子持久化 descriptor 与确定性 Audit intent，再交付 success Audit，并覆盖 CLI/Runtime 恢复。
- [x] DEK rotate 的 started/completed 与 operation ID 绑定；completed 只在 crash bridge 清理、compact、验证和最终发布后交付，并支持启动恢复。
- [x] 离线 rewrap、rotate、revoke、Recovery 在可取得受信 Audit key 时记录低敏 `security.kms.call`，可与 CloudTrail request ID 对账。
- [x] revoke 的 publication kill points 和幂等重试通过本地 fake KMS 回归。
- [x] Recovery 契约明确为离线修复 Primary 后正常启动，不提供自动 fallback。
- [x] Recovery 身份不用于长期运行 Gateway/Admin。
- [x] Primary 永久不可用时可通过 Recovery 修复新 Primary 并冷启动的命令链与 fake KMS 路径已覆盖。
- [x] 生命周期 Runbook 包含可复制但不含秘密的完整 revoke/Recovery 命令。
- [x] Admin UI 边界冻结为只读 Custody 页面，明确区分 File Key 本地可用、key_slots descriptor ready 与外部生产准入。
- [x] 只读 UI/API 不触发 KMS 调用且不返回敏感 descriptor 字段，并以 not-applicable/missing/current/expired/invalid-future 展示 Recovery 状态。
- [x] 生命周期与灾备 Runbook 随二进制嵌入、受 Admin 会话保护并按 mode 展示，不依赖 GitHub 或移动分支。
- [ ] 真实 AWS 14 项矩阵全部绑定最终 RC commit 并通过。
- [ ] 独立操作者完成 Primary 失败、Recovery restore、Primary 修复和撤权。
- [ ] 最终 RC Secret Canary、SBOM、checksum 和 Sigstore verification 全部通过。
- [ ] Security、Backend、SRE、Release 四方完成同 commit/tag 签署。
- [ ] `docs/milestone-m11-master-key-custody-aws-kms.md` 与实际 PR/证据状态一致。
- [ ] 在以上门禁全部满足前，AWS KMS/Key Slot 模式不标记 production-ready。

## 12. 完成定义

1.1.0 只有在以下条件全部满足时才能标记 Complete：

```text
Slot revoke 运维闭环
  + Recovery 离线修复契约
  + 可选只读 Custody UI 的明确范围
  + 真实 AWS 14 项矩阵
  + 独立恢复与撤权
  + 最终 RC 供应链证据
  + 四方签署
```

仅底层状态机存在、测试使用 fake KMS、PR 已合并或 CI 通过，均不足以满足本完成定义。
