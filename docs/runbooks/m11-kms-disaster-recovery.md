# M11 AWS KMS 备份、恢复与灾备 Runbook

本 Runbook 面向 `storage.master_key.mode: key_slots`。完整 doctor、backup create 和 restore 会调用只读 KMS `Decrypt` 并产生 AWS CloudTrail 事件；`doctor --no-kms` 与 `backup verify` 不调用 KMS。

## 1. Doctor

静态检查：

```bash
heimdall doctor --config /etc/heimdall/config.yaml --no-kms
```

预期结果为 `healthy: false`、`vault_status: vault_unverified`、`external_audit_events: false`。它只验证配置、descriptor、Keyring、Primary/Recovery 状态、allowlist、权限、bbolt/WAL/Usage/Parquet 等静态结构，不能证明 Vault 可恢复。

完整检查：

```bash
heimdall doctor --config /etc/heimdall/config.yaml
```

预期 `vault_status: verified`。该命令只读本地文件，但会通过 Primary Slot 执行 KMS unwrap，因此 `external_audit_events: true`；运行前后应记录数据目录文件 SHA-256 树并确认完全一致。

## 2. Backup create 与外层验证

```bash
heimdall backup create --config /etc/heimdall/config.yaml \
  --output /secure/backups/heimdall-UTC.hmbk \
  --key-file /secure/backup.key

heimdall backup verify --file /secure/backups/heimdall-UTC.hmbk \
  --key-file /secure/backup.key
```

Manifest 必须包含 `master_key_fingerprint`、`key_slot_descriptor_sha256`，且 `restore_drill_verified` 为 `false`。`backup verify` 只证明外层加密认证、manifest 和文件 checksum 完整；它没有使用目标 Workload Identity 解锁，绝不能据此宣称备份可恢复。

备份不得包含明文 Master Key/DEK、Web Identity token、静态云凭据或 Recovery 身份材料。descriptor 和 wrapped Master Key ciphertext 是必需的受保护恢复材料。

## 3. Primary restore drill

目标环境必须准备与备份 descriptor 匹配的 provider/region/account/Key ARN allowlist，但不得复制源环境 token。停止目标 Heimdall 后执行：

```bash
heimdall backup restore --config /etc/heimdall/config.yaml \
  --file /secure/backups/heimdall-UTC.hmbk \
  --key-file /secure/backup.key \
  --confirm-backup-id BACKUP_ID
```

程序先验证外层和 staging descriptor digest，再以目标 Primary Slot 解锁。protected payload、fingerprint、Vault Key Check 成功后，才验证 Audit、WAL、Usage 与 Parquet 并发布。成功结果必须包含 `unlock_path: primary`、`vault_verified: true`。

## 4. Primary disabled/deleted 时的 Recovery restore

1. 不要修改备份或手工编辑 descriptor。
2. 记录 Primary 的 AWS 错误分类和 CloudTrail 证据。
3. 通过审批临时授予独立恢复身份对 Recovery KMS Key 的最小 `Decrypt` 权限。
4. 明确输入 Recovery Slot ID：

   ```bash
   heimdall backup restore --config /etc/heimdall/config.yaml \
     --file /secure/backups/heimdall-UTC.hmbk \
     --key-file /secure/backup.key \
     --confirm-backup-id BACKUP_ID \
     --use-recovery-slot \
     --confirm-recovery-slot slot_aws_recovery
   ```

5. 成功结果必须包含 `unlock_path: recovery`、`vault_verified: true`、`recovery_audited: true`；恢复后的 Heimdall Audit 必须包含 `security.master_key.recovery_used` 和 `break_glass_restore`。
6. Recovery 只用于离线验证/restore，不用于启动 Runtime。恢复 live tree 后保持 Listener 停止，先验证 Recovery：

   ```bash
   heimdall key recover --config /etc/heimdall/config.yaml \
     --confirm-recovery-slot slot_aws_recovery
   ```

   成功 JSON 必须包含 `vault_verified: true`、`recovery_audited: true` 与修复 Primary 的 `next_action`。
7. 在配置中加入新的 Primary Key allowlist 项并把 `primary_slot` 指向新的 Slot ID，然后仍使用临时 Recovery 身份离线修复 Primary：

   ```bash
   heimdall key rewrap --config /etc/heimdall/config.yaml \
     --purpose primary \
     --slot-id slot_aws_primary_recovered \
     --key-reference arn:aws:kms:REGION:ACCOUNT:key/KEY_ID
   ```

8. 撤销临时 Recovery Grant/Role session，使用日常 Runtime 身份执行完整 `doctor`，再冷启动 Heimdall。Primary 仍不可用时必须在绑定 Listener 前 fail closed；不得用 Recovery 身份长期运行 Gateway/Admin。
9. 创建并验证新备份，按 Key 生命周期 Runbook 审批并 revoke 旧 retiring Primary Slot。Recovery 不会自动 fallback，以免掩盖权限篡改或 Key 异常。

## 5. 发布与回滚

- 所有域验证完成前，live data directory 不变。
- 成功发布时，原 live tree 被原子保存在 `.heimdall-pre-restore-*`，用于受控回滚；不得只复制其中个别文件回 live tree。
- 恢复会失效备份中捕获的 Admin Session 和 MFA challenge。
- 旧 KMS Key：在所有适用历史备份完成保留期、恢复演练和审批前保持可解密；之后才按 AWS Key Policy 停用/计划删除流程处理。
- 旧 descriptor：作为对应历史备份的认证恢复材料一并保留，不得用当前 descriptor 覆盖。
- 历史备份：保持不可变，继续使用其原 descriptor digest 与旧 KMS Key 恢复；达到备份保留期后按审批销毁。
- 每次 rewrap/rotate 后使用 [Key 生命周期清单](m11-kms-key-lifecycle.md) 记录三者的 owner、保留截止时间、最近恢复证据和最终处置。

## 6. 独立操作者证据

由未参与实现的操作者仅使用本 Runbook 完成一次 Primary 与一次 Recovery restore。归档：操作者/审批号、UTC 时间、Backup ID hash、descriptor digest hash、目标账号/区域、KMS Key ARN hash、命令版本、结果 JSON、CloudTrail event metadata、Heimdall Audit 验证、临时权限撤销时间、数据目录前后 hash、回滚目录位置。禁止归档 token、完整 ARN、ciphertext 或明文密钥。
