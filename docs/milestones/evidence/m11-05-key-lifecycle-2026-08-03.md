# M11-05 Key Lifecycle Evidence — 2026-08-03

状态：本地实现与自动化门禁完成；真实 AWS 生命周期演练待审批环境补齐。

## 已实现

- KEK rewrap：独立 Slot 解锁、目标 Slot `pending → verified active`、旧 Slot `retiring`，且 descriptor generation 不变。
- DEK rotate：随机内存 Master Key、双 Slot 预验证、descriptor/Keyring/Vault/Audit/Credential/MFA 的单一 bbolt COW 发布代。
- crash bridge、compaction、持久化 Primary 二次解锁，以及显式 `operation-id` 的恢复/完成幂等语义。
- KEK/Decrypt 身份疑似泄露时，rewrap 在 KMS I/O 前 fail closed，要求 DEK rotate 与历史备份处置。
- rewrap 的 add/verify/retire 只通过操作级 Store API 发布；DEK rotate 的新 generation 在 COW 写入前再次逐 Slot unwrap、核对指纹并通过 Vault Key Check，通用 descriptor 写入口不会重新开放。

## 自动化证据

- rewrap 前后 Master Key fingerprint、Credential Ciphertext、Credential KeyVersion 和 Keyring generation 不变。
- DEK rotate 推进 descriptor generation、Keyring 与 Credential KeyVersion；重加密 Credential/MFA；失效 Admin Session；保持 Audit HMAC 连续。
- rewrap 3 个 publication point 和 rotate 9 个 publication point 均可用同一稳定操作标识恢复；重复命令不会产生额外轮换。
- rotated descriptor 与 Vault material 必须在同一事务中匹配，否则持久层拒绝发布。

已通过：

```text
go test ./...
go test -race ./...
go vet ./...
go build -trimpath ./cmd/heimdall ./cmd/heimdall-deadman
./tools/m11/check-kms-boundaries.sh
sh -n tools/m11/aws-kms-lifecycle-smoke/run.sh
govulncheck v1.6.0 ./...  # 0 reachable vulnerabilities
```

## 待补真实 AWS 证据

- 使用三个现有且不同的 customer-managed symmetric KMS Keys（Primary、Recovery、Replacement Primary）和 Workload Identity 完成 rewrap/rotate/恢复演练。
- 归档 CloudTrail `Encrypt`/`Decrypt` 事件的非敏感 metadata，证明 Encryption Context 与 Key ARN 符合 allowlist。
- 验证运行时身份无 Recovery `Decrypt`，临时恢复授权在演练后撤销。
- 创建轮换后新备份并在目标恢复身份下完成 restore drill；归档历史备份处置清单。
