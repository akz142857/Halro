# M11 AWS KMS 生产运行与事故 Runbook

状态：生产候选基线。只有 M11 真实 AWS 矩阵、独立恢复演练和四方签署完成后，才可把 AWS KMS 模式标记为 production-ready。

## 1. 发布前基线

- 使用单一 `halro` artifact；AWS SDK 仅存在于 `internal/kms/awskms` Adapter 边界。
- 生产配置使用 `storage.master_key.mode: key_slots`，Primary 与 Recovery 是两个独立 customer-managed symmetric KMS Keys，且至少在 Key、Policy/Role、账号、区域或管理边界之一隔离。
- 日常 Runtime Role 仅有 Primary `kms:Decrypt`；Recovery Role 不与日常 workload 关联；Lifecycle Role 离线使用。
- 先渲染并评审 `deploy/aws-kms/` 的 IAM/Key Policy 模板。必须使用精确 Key ARN 和 Encryption Context，禁止 `Resource: *` 的身份策略。
- 运行 `halro config check`、`doctor --no-kms`，再在目标 Workload Identity 下运行完整 `doctor`。
- 主机/容器必须禁 core dump、pprof、自动 crash upload 和非必要调试能力。进程启动在解锁 Master Key 前验证 `RLIMIT_CORE=0`；Linux 同时设置 non-dumpable。任何加固失败都阻止启动。
- Halro 没有注册 pprof endpoint，也没有 crash-upload client。Go managed heap 不能安全承诺 `mlock`、确定性擦除或按 slice 生命周期实施 `MADV_DONTDUMP`：对 Go heap 页直接 madvise 可能连带无关对象，因此当前明确不启用；这项剩余风险由禁 core、Linux non-dumpable、ptrace/capability 隔离、节点隔离和短生命周期 `clear()` 缓解。

## 2. Kubernetes / EKS

以 `deploy/kubernetes/halro-aws-kms.yaml` 为起点：

- 必须 `replicas: 1`、`strategy: Recreate`，PVC 只能由一个 writer 挂载；不得配置 HPA。
- 使用 digest-pinned image、non-root、read-only rootfs、`seccompProfile: RuntimeDefault`、drop all capabilities、禁止 privilege escalation/hostPID/hostPath。
- 为 ServiceAccount 创建 EKS Pod Identity association；关联信息不写入 Kubernetes 对象。确认 Agent 已就绪、Pod 能得到目标 Runtime Role，且不能得到 node role。
- readiness 使用容器内 loopback healthcheck。Workload Identity/KMS 尚未就绪时进程按有界 deadline 重试，Pod 不 Ready；超过 deadline 后退出，由有退避的控制器重启。
- 发布前执行三项 CrashLoop 测试：身份未就绪、Primary Policy deny、Primary Key disabled。确认没有调用风暴、没有第二副本、没有 Recovery 自动 fallback，并验证 `HalroTargetDown`/KMS 告警。
- swap 由节点基线禁用或使用经安全评审的加密 swap；禁止在共享、可调试节点运行。

## 3. VM / systemd

以 `deploy/systemd/halro-aws-kms.service` 为起点：

- 使用专用无登录用户，数据目录 `0700`，配置与 Metrics credential `0400/0600`；不要使用静态 AWS access key 环境变量。
- 使用 EC2 Instance Profile/受控 Workload Identity，仅授予 Primary policy。`LimitCORE=0`、空 capabilities、`NoNewPrivileges`、`ProtectProc` 和只读系统目录不得被本地 override 放宽。
- 禁用 systemd-coredump、ABRT/apport 等 crash collector；限制 `ptrace_scope`，仅安全值班审批可临时调试，且调试前必须停止服务并撤销 KMS 权限。
- 启动失败时先读取安全分类错误和 CloudTrail metadata；不得把 ARN、ciphertext、token 或完整环境转储到工单。

## 4. Audit 与 Metrics 关联

每次 Runtime KMS provider 调用会记录 Halro `security.kms.call` Audit，`correlation_id` 是受长度限制的 AWS request ID，可与 CloudTrail request metadata 关联。Audit 不记录 Key ARN、Encryption Context、ciphertext、payload、身份 token、provider error body 或 Master Key fingerprint。

低基数指标只使用固定枚举 `operation/status/error_class/purpose/state`，不含账号、ARN、Slot ID 或 request ID：

- `halro_kms_calls_total` / `halro_kms_call_duration_seconds`；
- `halro_kms_unlock_total`；
- `halro_kms_automatic_fallback_total`（必须恒为 0）；
- descriptor、Recovery readiness、pending rotation、Slot state/verification time；
- 从可信 Audit 恢复的 `halro_kms_recovery_last_used_timestamp_seconds`。

永久 KMS 启动失败时 Metrics endpoint 不会启动，因此必须把 `HalroTargetDown` 与结构化启动日志、CloudTrail 告警一起使用，不能只依赖进程内 counter。

## 5. 告警响应

- `HalroKMSPrimaryUnlockFailure`：核对 Workload Identity、Policy explicit deny、Key state、区域和 Context；禁止自动改用 Recovery。
- `HalroKMSRecoveryNotReady/VerificationExpired`：停止有风险的 Key 变更，安排受控 Recovery verify/restore drill。
- `HalroKMSRecoveryUsed`：按最高优先级确认审批、操作者和原因；完成后撤销临时身份并检查 Audit/CloudTrail。
- `HalroKMSVaultMismatch`：立即 fail closed，禁止手工替换 descriptor；保全 bbolt、Audit、备份和 CloudTrail，按安全事故处理。
- `HalroKMSPendingRotation`：使用原 operation ID 恢复，不得开启第二次轮换。

## 6. Key/身份泄露

- KMS Key、Key Policy、Grant 或可执行 `Decrypt` 的身份疑似泄露：立即隔离身份/Grant并保全证据；`rewrap` 不能修复已经暴露的 Master Key，必须执行 DEK rotate。
- Primary disabled/deleted：保持 live 数据不变，按 [灾备 Runbook](m11-kms-disaster-recovery.md) 临时授权独立 Recovery Role；恢复成功、创建新备份并验证后立即撤权。
- 生命周期与历史备份处置使用 [Key 生命周期 Runbook](m11-kms-key-lifecycle.md)，每份旧备份必须保留其旧 descriptor 与所依赖 KMS Key 的明确处置记录。

## 7. 发布证据与签署

Release Candidate 必须归档：精确 commit/tag、CI/race/vet、0 reachable vulnerability 报告、KMS boundary、Secret Canary、kill-point matrix、14 项真实 AWS 测试、Primary/Recovery 独立恢复、SBOM、checksums、每个 artifact 的 Sigstore bundle，以及 Security/Backend/SRE/Release 四方签署。使用 `tools/m11/release-evidence/README.md` 中的完整命令，以 expected commit/tag、实际 release-assets 目录校验最终脱敏 bundle；只有输出 `M11_RELEASE_EVIDENCE=PASS` 才能进入发布审批。任何一项待补时保持 Draft/Not production-ready。

GitHub 发布时，四方 reviewer 必须先下载 `provenance` job 的 `release-assets`，完成 SBOM、checksum、Sigstore 与 bundle 审核，再把脱敏 JSON 设置为 `v1-release` Environment 的 `M11_RELEASE_EVIDENCE_JSON` secret 并批准环境。`publish` job 会再次绑定 signed tag 的 commit/tag、重算每个 artifact SHA-256、执行 `sha256sum --check` 并独立执行 `cosign verify-blob`；缺 secret、跨 commit/tag、摘要不一致或签名验证失败都会阻止创建 Release。
