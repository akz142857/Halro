# M11 AWS KMS Key 生命周期 Runbook

本文只覆盖正常 KEK rewrap、Heimdall Master Key/DEK rotate，以及中断恢复。所有命令均为离线控制面操作：先停止 Heimdall 实例，保留唯一数据目录副本，并使用受审批的 Workload Identity；不要向命令行、日志或工单粘贴 ciphertext、token 或明文 Master Key。

## 1. 语义边界

- `key rewrap` 只改变某一个 Slot 的 AWS KMS 包装。Master Key fingerprint、Credential Ciphertext、Credential KeyVersion、Admin Session 与 descriptor generation 必须保持不变。
- `key rotate` 生成新的 32 字节 Master Key，创建并验证全新的 Primary/Recovery 包装，重加密 Credential、MFA 和系统保护材料，推进 KeyVersion，并使 Admin Session 失效。
- KMS Key、Key Policy、Grant、Workload Identity 或任何可执行 `Decrypt` 的身份疑似泄露时，禁止把 rewrap 当成事故修复。必须执行 DEK rotate，并处置所有仍包含旧 descriptor 的历史备份。

## 2. 正常 KEK rewrap

1. 新建或选定新的 customer-managed symmetric KMS Key；让它与另一个用途的 Slot 保持独立故障域。
2. 把新 Key ARN 加入 `allowed_kms_keys`，并把对应的 `primary_slot` 或 `recovery_slot` 改为新的 Slot ID。另一条解锁路径必须保持不变且可用。
3. 使用不被替换的独立 Slot 解锁并执行：

   ```bash
   heimdall key rewrap --config /etc/heimdall/config.yaml \
     --purpose primary \
     --slot-id slot_aws_primary_2026q3 \
     --key-reference arn:aws:kms:REGION:ACCOUNT:key/KEY_ID
   ```

4. 命令依次持久化 `pending → active`，只有新 Slot 独立 unwrap 并通过 Vault Key Check 后才把旧 Slot 置为 `retiring`。每个 added、verified、retiring 变更都先在同一个 bbolt 事务写 descriptor 与确定性 Audit intent，再交付 success Audit；进程中断后由下一次 CLI 或 Runtime 启动恢复，Audit 不会先于状态宣称成功。
5. 验证 fingerprint、Credential ciphertext digest 和 KeyVersion 未改变。实例保持停止，使用受支持的低敏离线查询取得 descriptor 与 retiring Slot revision；该结果不包含 ARN、ciphertext、provider parameters 或 fingerprint：

   ```bash
   heimdall key slot status --config /etc/heimdall/config.yaml
   ```
6. 恢复窗口与备份清单审批完成后，精确确认 retiring Slot 并执行：

   ```bash
   heimdall key slot revoke --config /etc/heimdall/config.yaml \
     --slot-id slot_aws_primary_2026q2 \
     --expected-descriptor-revision DESCRIPTOR_REVISION \
     --expected-slot-revision SLOT_REVISION \
     --confirm-slot-id slot_aws_primary_2026q2 \
     --reason retirement_window_completed
   ```

   命令只接受 `retiring → revoked`，先用配置中的同用途 replacement active Slot 独立解锁并通过 Vault Key Check。revoke、持久化 Audit intent 和 compaction 在 stage 中完成并验证，随后一次 rename 发布无旧敏感页的 metadata，再交付最终 success Audit；启动会恢复未交付 intent。只有相同 revisions、确认和 reason 的重试才幂等，任何冲突 fail closed。事故退役可使用 `--reason incident_retirement`。
7. revoke 成功后从运行配置移除旧同用途 allowlist 条目，再撤销旧 AWS Grant/Policy；DEK rotate 要求 Primary、Recovery 各自只有一个明确的目标 KMS Key。
8. rewrap/revoke 不修改历史备份。旧 KMS Key 只有在备份清单证明不再需要它后才能安排禁用或删除。

若怀疑泄露，可带 `--compromised` 验证防误操作门禁；命令会在任何 KMS 调用前失败并要求 DEK rotate。

## 3. DEK rotate

每次操作选择一个可公开记录、可重复使用但不包含秘密的唯一 ID。进程中断后必须使用同一个 ID；新的轮换必须使用新 ID。

```bash
heimdall key rotate --config /etc/heimdall/config.yaml \
  --operation-id incident-2026-08-03-001
```

成功条件：

- 新 Primary 与 Recovery Slot 均完成 wrap、unwrap 和 Vault Key Check；
- descriptor generation、Keyring、Vault Key Check、Audit HMAC envelope、Credential 与 MFA ciphertext 在同一个 bbolt COW 发布代；
- Credential KeyVersion 前进，Admin Session 与未完成 MFA challenge 失效；
- Audit HMAC key 不变，历史与新增 Audit 记录形成连续链；
- 发布后重新通过持久化 Primary Slot 解锁，再清除 crash bridge 并 compact retired pages；
- 同一 `operation-id` 重复执行只恢复或返回已完成代，不会再次轮换。

## 4. 中断恢复

- metadata 发布前中断：当前完整旧代仍是信任源；用相同命令和 `operation-id` 重试。
- metadata 发布后、bridge 清理前中断：新 descriptor 与新 Vault 代已原子发布；重试会发现 Keyring 中的 operation ID 和 bridge，验证新 Primary/Vault/Audit 后完成清理。`security.master_key_rotation.completed` 仅在 bridge 清除、compaction、验证及最终 metadata 发布成功后交付；未交付的确定性 intent 会由 CLI 或 Runtime 启动恢复。
- bridge 清理发布后中断：同一 operation ID 被识别为已完成，重试不创建下一代。
- 若发现不同 operation ID 的 pending bridge，立即停止；不得启动第二次轮换或手工编辑 bbolt。
- 恢复过程中任一候选 Key 未通过 protected payload、fingerprint 或 Vault Key Check 时 fail closed。

## 5. KEK/Decrypt 身份泄露响应与备份清单

1. 隔离疑似泄露的身份、Session、Grant 与 Key Policy，并保全 AWS CloudTrail 和 Heimdall Audit。
2. 使用可信独立 Recovery Slot 验证恢复能力；完成后撤销临时 Recovery 授权。
3. 使用未泄露的 Primary/Recovery KMS Keys 配置执行 DEK rotate。
4. 创建新备份，并在目标恢复身份与隔离环境完成一次真实 restore drill。
5. 对每份历史备份登记下表，不能仅因当前实例已 rewrap/rotate 就认为历史备份安全：

| 字段 | 必填内容 |
| --- | --- |
| Backup ID / 创建时间 | 唯一标识与 UTC 时间 |
| descriptor digest / Master Key fingerprint | 备份创建时的值 |
| 依赖 KMS Key ARN 与 Slot ID | Primary、Recovery 全部列出 |
| 旧 KEK/身份是否疑似泄露 | `yes/no/unknown` |
| 保留与合规义务 | 负责人、到期日、legal hold |
| 处置 | 隔离、用可信外层重新加密、或经审批销毁 |
| 恢复验证 | 环境、身份、时间、结果与证据链接 |

6. 只有新备份恢复成功、历史备份均有明确处置、审计证据归档后，才可 revoke 旧 Slots/Grants 并按 AWS 等待期安排旧 KMS Key 删除。

## 6. 审核证据

- `security.master_key_slot.added/verified/retiring/revoked` 只记录 Slot ID、状态与受限 reason code，不记录 ARN、wrapped bytes、provider parameters 或 fingerprint。
- 离线 rewrap、rotate、revoke 与 Recovery 在成功取得受信 Audit HMAC key 后，把本次 AWS KMS operation、稳定错误分类和 provider request ID 写为 `security.kms.call`；本地 Audit 不记录 ARN、ciphertext 或 native error。若没有任何受信 Slot 能解锁 Audit key，失败调用只能依赖 AWS CloudTrail 取证，命令仍 fail closed。
- `security.master_key_rotation.started/completed` 必须连续可验证。
- 保存命令的非敏感 JSON 结果、CI kill-point matrix、CloudTrail event metadata 与备份清单；不得保存明文 Key、完整 ciphertext 或 Workload Identity token。
