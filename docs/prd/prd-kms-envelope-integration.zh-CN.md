# Heimdall KMS Envelope Key Slot PRD

状态：Draft — 评审阻塞项已纳入，尚未进入实现排期

日期：2026-08-03

里程碑跟踪：`docs/milestones/milestone-m11-master-key-custody-aws-kms.md`

## 1. 文档定位

本 PRD 是 `docs/prd/prd-master-key-key-slots.zh-CN.md` 的 KMS Slot 子方案，定义云 KMS 如何包装并解锁 Heimdall Master Key。M11 只实现并验收 AWS KMS；GCP Cloud KMS 和 Azure Key Vault Keys 仅保留为未来适配时必须重新评审的能力差异，不进入当前发布范围。

总体模型、File Slot、Master Key 生命周期和共同安全边界由上层 Key Slot PRD 负责。本 PRD 不再定义单一 `wrapped_master_key` 模式；任何生产 KMS 部署都必须服从多 Slot 与独立恢复路径要求。

云 SDK 隔离和发布形态由 `docs/adr/0010-kms-sdk-dependency-isolation.md` 约束。

## 2. 问题陈述

Heimdall 当前将 32 字节随机 Master Key 以明文 `master.key` 文件保存在本地。文件权限和 Vault 加密可以抵御数据库单独泄露，但数据库、主机快照和 Master Key 同时泄露时，攻击者无需破解 AES 即可解密 Provider Credential、MFA 等 Vault 数据。

企业团队通常还要求：

- 通过 IAM、Workload Identity、Key Policy 和云审计统一控制解锁权限；
- 不在主机磁盘长期保存裸 Master Key；
- 区分 KMS KEK rewrap 与 Heimdall DEK rotate；
- 能在 KMS Key 误删、禁用、区域故障、账号隔离或策略错误时恢复；
- 保持 Heimdall 核心云中立、File 模式无云行为，正常请求不依赖 KMS 可用性。

原始架构方向保持不变：

> 外部 KMS 管理 KEK，Heimdall 保留随机 DEK 和现有 Vault。

```text
AWS KMS（M11）/ 未来经独立评审的其他 KMS
                  │
          Workload Identity
                  │
                  ▼
        unwrap protected key payload
                  │
                  ▼
       32-byte Heimdall Master Key
                  │
                  ▼
  现有 HKDF + AES-GCM + AAD + bbolt Vault
```

## 3. 目标用户与场景

### 3.1 目标用户

- 当前在 AWS 运行单实例 Heimdall、未来可能选择其他托管环境的平台团队；
- 需要 IAM/KMS 审计和职责分离的安全团队；
- 负责冷启动、升级、备份、恢复和 DR 演练的 SRE；
- 需要选择独立 File 模式或可选 KMS 扩展的部署操作者。

### 3.2 核心用户故事

1. 作为平台管理员，我可以使用 Workload Identity 自动解锁 Heimdall，而不在磁盘或部署清单保存云静态凭据和裸 Master Key。
2. 作为安全管理员，我可以更换 KMS Key 并 rewrap 同一个 Master Key，而不触发 Credential 全量重加密。
3. 作为 SRE，我可以在主 KMS Key、区域或账号不可用时，通过另一个已验证 Slot 恢复服务和备份。
4. 作为审计人员，我可以确认谁在何时执行 unwrap、rewrap、Slot 变更和 DEK rotate，但任何日志都不包含密钥材料。
5. 作为独立部署用户，我可以选择 File 模式而不安装或调用任何云扩展。

## 4. 目标

- 使用云 KMS 包装随机 32 字节 Heimdall Master Key，明文 DEK 不落盘。
- KMS 只进入启动、恢复、rewrap、rotate 和诊断控制面，不进入 Gateway 请求热路径。
- 使用 Workload Identity/Managed Identity，生产路径不依赖静态云访问密钥。
- 每个生产实例至少有两个独立、已验证的解锁路径。
- 定义跨区域、跨账号和人工 break-glass 恢复边界。
- 定义启动超时、重试、错误分类、readiness 和告警。
- 使用云原生绑定能力和通用受保护 payload 防止交叉解包、降级和 confused-deputy 风险。
- 将 `runtime`、`doctor`、备份、恢复、审计、Admin、Metrics、bootstrap 和轮换统一接入 Master Key 抽象。
- 保持 File 模式无云初始化和云调用；AWS SDK 的 module/artifact 边界由 ADR 0010 的实测结果决定。
- 保留现有 Vault Key Check 作为 DEK 的最终信任锚。

## 5. 非目标

- 不在请求热路径按次调用 KMS。
- 不把 Provider API Key 改为 Secrets Manager 外部引用；这是独立后续需求。
- 不提供 active-active、多写者或 HPA 扩副本能力。
- 不把 Kubernetes KMS Provider 当作 Heimdall 的通用 KMS API。
- 不在本阶段承诺 FIPS 认证、SOC 2 认证或组织级多租户密钥隔离。
- 不承诺抵御已控制宿主机 root、调试器或 Heimdall 进程内代码执行的攻击者。
- 不在根模块通过 build tag 隐藏实际仍存在于 `go.mod` 的云 SDK。
- 不实现通用运行时插件系统或调用云 CLI。

## 6. 当前系统影响范围

实现前必须确认并改造所有直接文件调用。当前至少包括：

- Runtime 启动；
- 初始化和 bootstrap；
- Admin 密码、MFA 离线重置；
- Audit verify；
- Metrics 派生凭据；
- Provider 配置加载；
- `doctor`；
- backup create/verify/restore；
- Master Key rotate；
- 相关测试 fixture 和配置生成。

Phase 0 必须先完成统一 Master Key 模型：所有路径通过统一解锁接口获取密钥，直接采用最终配置 schema，并保留 File 模式现有安全检查。KMS 实现不得与核心接口改造混在同一变更中。

## 7. 产品与安全不变量

- Master Key 继续由 `crypto/rand` 生成，固定为 32 字节随机 DEK。
- KMS 解出的任何值在通过 Vault Key Check 前都不可信。
- fingerprint 只用于诊断和快速拒绝，绝不能跳过 Vault Key Check。
- 解锁失败不得生成新 Key、覆盖 descriptor、清空 Vault 或启动部分 Runtime。
- File 模式保持默认可用且不引入云依赖。
- 正常 Gateway 流量不因 KMS 短暂故障中断；冷启动和恢复仍依赖至少一个 Slot 可用。
- Primary KMS Slot 和 Recovery KMS Slot 都包装同一个 Master Key；File 模式独立运行。
- 任何 Slot rewrap 不改变 Master Key fingerprint 或 Credential KeyVersion。
- KEK 疑似泄露不能只做 rewrap；必须执行 DEK rotate 并处置历史备份。
- 生产 KMS 模式不得只有一个解锁 Slot。

## 8. 架构与依赖边界

### 8.1 核心接口

核心模块拥有 provider-neutral 接口，概念签名如下：

```go
type KMSWrapper interface {
	Provider() string
	Wrap(ctx context.Context, request WrapRequest) (WrapResult, error)
	Unwrap(ctx context.Context, request UnwrapRequest) (UnwrapResult, error)
}
```

核心负责：

- Slot 选择、状态机和持久化；
- 配置 allowlist；
- protected payload 编解码和验证；
- Vault Key Check；
- 超时、重试和错误分类策略；
- COW/crash recovery；
- Audit、Metrics 和稳定错误响应。

云 Adapter 只负责：

- Workload Identity/Managed Identity；
- provider endpoint 和官方 SDK 调用；
- wrap/unwrap；
- provider-native context/AAD；
- 将原生错误映射为稳定类型。

Adapter 不得访问 bbolt、Vault Credential、Gateway 请求、Admin Session 或备份内容，也不得自行决定 fallback 或无限重试。

### 8.2 SDK 与发布边界

遵循 ADR 0010：

- 固定使用窄、显式、进程内 Adapter，不实现运行时插件系统；
- M11 Phase 0 实测比较“主制品包含 AWS SDK”和“core/AWS 两个制品”；
- ADR 0010 更新为 Accepted 前，不得合入生产 AWS SDK 实现；
- 无论选择哪种制品方式，File 模式都不得初始化 AWS SDK、读取 AWS 身份或产生网络调用；
- GCP/Azure 的 SDK、module 和 artifact 方式不在本次决策中预设。

## 9. Key Slot 与 descriptor

KMS Slot 是上层 Key Slot descriptor 的一种，建议字段如下：

```json
{
  "id": "slot_aws_primary",
  "format_version": 1,
  "type": "kms-envelope",
  "state": "active",
  "provider": "aws-kms",
  "key_id": "arn:aws:kms:ap-southeast-1:123456789012:key/...",
  "region": "ap-southeast-1",
  "algorithm": "SYMMETRIC_DEFAULT",
  "ciphertext": "base64...",
  "payload_version": 1,
  "master_key_fingerprint": "sha256:...",
  "created_at": "...",
  "verified_at": "...",
  "revision": 1
}
```

注意：`algorithm` 是 provider-specific 字段，不能把 AWS 的 `SYMMETRIC_DEFAULT` 当作跨云统一值。Azure、GCP 的允许算法必须由对应 Adapter 和可信配置分别校验。

descriptor 存入 bbolt 未加密系统元数据区域并包含在 Heimdall 加密备份中。它不是秘密，但必须受到：

- bbolt 事务和 revision 控制；
- 配置 allowlist；
- protected payload 内部绑定；
- Vault Key Check；
- Audit 和备份 manifest digest；
- COW/compaction 发布语义。

禁止仅根据数据库中的 provider、endpoint、region、tenant、account 或 `key_id` 发起网络请求。

## 10. Protected Key Payload 与外层绑定

KMS 不直接包装裸 32 字节 DEK，也不包装体积不确定的 JSON。真实 wire payload 使用固定 112 字节规范二进制编码，以适应 RSA-OAEP 和 AES Key Wrap 等 provider 算法的输入限制：

```text
offset  size  field
0       8     magic = "HKMSKEY1"
8       2     format_version (big endian)
10      2     flags (v1 必须为 0)
12      32    SHA-256(instance_id 的规范编码)
44      32    SHA-256(slot_id 的规范编码)
76      32    master_key
108     4     reserved (v1 必须为 0)
```

Master Key fingerprint 由解包后的 32 字节重新计算并与 descriptor 比较，不在 payload 中重复保存。payload 必须严格拒绝错误 magic/version/flags/reserved、错误固定长度、绑定摘要不匹配和尾随数据。`master_key` 只能在内存中的受控字节缓冲区出现。

### 10.1 Provider 绑定矩阵

| Provider | 原生绑定 | 通用要求 |
|---|---|---|
| AWS KMS | 对称 Encrypt/Decrypt 使用 Encryption Context | Context 绑定 instance ID、Slot ID、format version 和用途 |
| GCP Cloud KMS | Encrypt/Decrypt 使用 Additional Authenticated Data | AAD 绑定相同语义 |
| Azure Key Vault Keys | wrap/unwrap 算法不统一提供等价 AAD | 严格 key/algorithm allowlist、完整 protected payload、fingerprint 和 Vault Key Check |

所有 Provider 都必须执行相同的解包后验证顺序：

1. 配置允许 provider、账号/项目/tenant、region、endpoint 和 key ID；
2. descriptor schema、状态、revision、算法和 payload version 合法；
3. provider-native Context/AAD 在支持时完全匹配；
4. payload magic、instance ID、Slot ID、format version 完全匹配；
5. Master Key 恰好 32 字节，fingerprint 匹配；
6. `verifyVaultKeyCheck` 成功；
7. 才允许派生 Admin、Audit、Metrics 和 Vault 子密钥。

Azure 缺少等价 AAD 不得被描述为与 AWS/GCP 密码学能力完全一致。其 residual risk 必须记录：同一允许 KMS Key 下的 ciphertext 替换应被 instance/Slot/payload/Vault 检查拒绝并导致 fail closed，但不能宣称 provider 层已经提供相同的 AAD 保证。

## 11. 配置模型

项目仍在开发期，配置直接采用最终嵌套结构，不保留旧 `master_key_file` 字段或迁移状态。

```yaml
version: 1

storage:
  data_dir: /var/lib/heimdall
  metadata_file: heimdall.db

  master_key:
    mode: key_slots
    primary_slot: slot_aws_primary
    recovery_slot: slot_aws_recovery
    startup_deadline: 60s
    call_timeout: 5s
    allowed_kms_keys:
      - purpose: primary
        provider: aws-kms
        region: ap-southeast-1
        account: "123456789012"
        key_id: arn:aws:kms:...
      - purpose: recovery
        provider: aws-kms
        region: ...
        account: "..."
        key_id: arn:aws:kms:...
```

规则：

- `storage.master_key.mode` 必须显式选择 `file` 或 `key_slots`；
- File 与 Key Slot 专属字段混用时校验失败；
- `primary_slot` 和 `recovery_slot` 必须引用不同 Slot，且分别匹配 `allowed_kms_keys[].purpose`；
- `startup_deadline` 是启动解锁的总时限，`call_timeout` 是单次调用时限，且单次时限必须小于总时限；
- 未知字段继续失败；
- schema version 和严格字段校验必须有测试；
- 配置白名单变更需要重启并先通过 `config check`；
- `config check` 只做静态检查，不主动调用 KMS。

最终 Go 结构和 schema version 在 Phase 0 设计评审中冻结。

## 12. 生产 Slot 冗余与 DR

### 12.1 强制要求

KMS 模式进入 production-ready 状态前，必须至少有：

- 一个主 KMS Slot；以及
- 一个已独立解包并通过 Vault Key Check 的备用 Slot。

M11 的备用路径必须是 AWS KMS Recovery Slot，并至少在 KMS Key、Key Policy 管理边界、AWS 账号、恢复身份或目标区域中的一个维度与 Primary 隔离。日常 Runtime 身份默认不得拥有 Recovery Key 的 Decrypt 权限。

Recovery Seed、助记词和跨云 Slot 不属于 M11。要求自动区域恢复的部署必须配置可由目标恢复环境访问的 Recovery KMS Slot；仅有人工临时授权流程时，不得宣称自动恢复 SLA。

### 12.2 恢复矩阵

| 故障 | 自动启动目标 | 必需准备 | 恢复路径 |
|---|---|---|---|
| 主 KMS 短暂超时/限流 | 是 | 同 Slot 有界重试 | 重试成功后启动 |
| 主 KMS Key disabled/policy 拒绝 | 可选 | 第二 active Slot | 显式按优先级尝试备用 Slot |
| 主区域不可用 | 有 SLA 要求时是 | 跨区域 Slot + 身份 + 配置 allowlist | 在 DR 环境使用区域备用 Slot |
| 主账号不可访问 | 有 SLA 要求时是 | 跨账号 Recovery Slot 与预置身份 | 使用隔离域备用 Slot |
| Primary 路径不可用 | 否，除非已预置自动恢复身份 | Recovery KMS Slot + runbook | 人工 break-glass 授权并恢复 |
| Primary KMS Key 永久删除 | 否，除非 Recovery 可用 | Recovery KMS Slot | 解锁同一 DEK，立即创建新 KMS Slot |
| KEK 疑似泄露 | 否 | 旧 Slot + 新 Slot + 备份清单 | 执行 DEK rotate，不仅是 rewrap |

每个生产发布必须记录部署采用哪一行恢复承诺。没有跨区域/跨账号 Slot 时，不得宣称自动 DR。

### 12.3 定期演练

- 至少按组织规定周期执行备用 Slot 解包和 Vault Key Check；
- 定期在隔离恢复环境验证备份；
- 演练不得输出 DEK、Recovery 身份凭据或 Credential；
- `verified_at`、演练结果和稳定错误分类进入 Audit；
- 备用 KMS Slot 未在规定周期验证时触发告警。

## 13. 启动、重试和故障语义

### 13.1 默认时限

建议默认值，最终由容量和真实云测试确认：

- 单次 KMS 调用超时：5 秒；
- 总启动解锁 deadline：60 秒；
- 初始退避：250 毫秒；
- 最大退避：5 秒；
- full jitter；
- 同一 Slot 在 deadline 内有界重试；
- Slot fallback 顺序必须由可信配置明确指定。

不得把重试交给核心和云 SDK 两层无限叠加。Adapter SDK 的内部最大尝试次数必须受控并计入总 deadline。

### 13.2 错误分类

| 稳定分类 | 示例 | 行为 |
|---|---|---|
| `kms_transient` | timeout、连接中断、临时 5xx | deadline 内退避重试 |
| `kms_throttled` | provider rate limit | 尊重 Retry-After 后有界重试 |
| `kms_identity_not_ready` | projected identity/token 尚未可用 | deadline 内短期重试 |
| `kms_permission_denied` | IAM/Policy/Grant 拒绝 | 当前 Slot 快速失败，不盲目长重试 |
| `kms_key_unavailable` | key disabled、deleted、pending deletion | 当前 Slot 快速失败并按配置尝试备用 Slot |
| `kms_config_invalid` | region/key/algorithm 不在 allowlist | 立即失败，不发起不受信网络请求 |
| `kms_ciphertext_invalid` | context/AAD/ciphertext 验证失败 | 立即安全失败并告警 |
| `kms_payload_invalid` | payload magic/instance/Slot/version 错误 | 立即安全失败并清理明文 |
| `kms_vault_mismatch` | Vault Key Check 失败 | 立即失败；不得尝试启动部分 Runtime |
| `kms_adapter_unavailable` | 当前 artifact 未编译 Adapter | 配置阶段失败 |

自动 fallback 只能在配置列出的 active Slot 之间发生。安全篡改类错误是否继续其他 Slot 必须由策略明确；默认记录高严重度事件后允许尝试独立恢复 Slot，但整体不得掩盖原 Slot 异常。

### 13.3 Readiness

- 解锁完成、Vault Key Check 成功、Audit/metadata 校验完成前 readiness 为 false；
- KMS 解锁过程中不得启动 Gateway 或 Admin Listener；
- 错误输出包含稳定分类、Slot ID 和 provider，不包含 ciphertext、payload、key material 或原生敏感响应；
- 已成功启动的进程不因后续 KMS 不可用而停止处理请求；
- 进程重启必须重新解锁，不能把明文 DEK 缓存到磁盘作为“宽限”。

## 14. Kubernetes 与单活部署

Heimdall 保持一个 active writer 独占数据目录。KMS 支持不改变该约束。

- Kubernetes 工作负载默认 `replicas: 1`；
- 使用 `Recreate`，不宣称 RollingUpdate/active-active；
- 不使用 HPA 扩 Heimdall 副本；
- 第二 Pod 无法取得数据锁时不得持续高频调用 KMS；锁和静态配置检查应尽可能在解包前完成；
- IRSA、GCP Workload Identity、Azure Workload Identity 未就绪映射为 `kms_identity_not_ready` 并受总 deadline 限制；
- CrashLoopBackOff、Pod disruption、节点驱逐和 KMS 限流必须进入部署测试；
- ServiceAccount、IAM/Role、Key Policy 和 Grant 生命周期必须有独立 runbook。

## 15. 初始化

### 15.1 KMS 新实例 bootstrap

1. 验证配置、Adapter、Workload Identity 和目标 KMS Key allowlist。
2. 使用 `crypto/rand` 在进程内生成 32 字节 Master Key。
3. 构造固定长度 protected payload。
4. 使用主 KMS Slot wrap。
5. 立即独立 unwrap、验证 payload，并确认解出的 Master Key 与本次生成值一致。
6. 创建并验证第二恢复 Slot。
7. 在一个可恢复发布状态机中写入 descriptors、Keyring、Vault Key Check 和 Audit 材料。
8. `fsync`/原子发布后，通过首选 Slot 重新解锁并验证已经持久化的 Vault Key Check。
9. 清理内存中的 DEK、payload 和临时 KEK 材料。
10. 未创建第二有效 Slot 时实例状态不得标记 production-ready。

任何失败都不得留下看似已初始化但无法解锁的实例。

## 16. Rewrap、泄露响应与 DEK Rotate

### 16.1 KEK rewrap

正常 KMS Key 更换：

1. 使用现有 Slot 解包并通过 Vault Key Check。
2. 使用新 KMS Key 包装同一个 protected payload/Master Key。
3. 写入 pending 新 Slot。
4. 立即用新 KMS Key 独立 unwrap 和验证。
5. 原子切换新 Slot active、旧 Slot retiring。
6. 保留足够恢复窗口后显式撤销旧 Slot。

rewrap：

- 不重加密 Credential；
- 不改变 Master Key fingerprint；
- 不推进 Credential KeyVersion；
- 不自动使 Admin Session 失效；
- 不能修复历史备份已经暴露给旧 KEK 的问题。

### 16.2 KEK 泄露响应

如果旧 KEK、其授权身份或可执行 Decrypt 的权限疑似泄露，仅 rewrap 不足。历史备份仍包含旧 Slot descriptor，攻击者可以使用泄露的 KEK 解出历史 DEK。

必须执行：

1. 隔离泄露身份并保全审计证据；
2. 使用可信 Slot 解锁；
3. 执行 Heimdall DEK rotate，全量重加密 Vault；
4. 用未泄露 KMS Key 创建新 Slots；
5. 创建新的可信备份；
6. 按保留和合规策略隔离、重加密或销毁受影响历史备份；
7. 撤销旧 Grants/Policies/Slots，并记录事件。

### 16.3 DEK rotate 与 crash recovery

KMS 模式必须把 descriptor 发布映射到现有 lock、COW、Keyring 和 crash bridge 状态机：

- 新 DEK 的全部必需 active Slots 必须先成功 wrap/unwrap 验证；
- metadata COW 中的新 Vault 材料、Keyring、Vault Key Check 和新 Slot 集合必须形成一致恢复代；
- 数据库发布前后每个 kill point 都必须能由旧或新已认证路径恢复；
- 旧/新 descriptor 不得以独立非原子文件与 bbolt 产生双文件失配；
- 发布后通过新 Slot 解锁并完成 Vault/Audit 校验，才能压缩移除 recovery bridge 和 retired ciphertext pages；
- 任何 fingerprint 匹配都不能替代 Vault Key Check。

实现状态机和所有 publication kill points 必须在编码前形成单独设计说明和测试矩阵。

## 17. Doctor、备份与恢复

### 17.1 Doctor

KMS 模式下 `doctor` 分为：

- 完整模式：允许只读 KMS unwrap，验证 payload、Vault Key Check、Audit、WAL、Parquet 和配置引用；
- 静态模式：`--no-kms`，只检查 descriptor schema、配置 allowlist、文件权限和未加密结构，结果明确标记 `vault_unverified`，不得报告整体通过。

完整模式仍必须保持本地文件前后哈希不变。KMS Decrypt 会产生外部云审计事件，文档必须明确它不是“无任何外部副作用”，但不得改变 KMS Key、Grant 或 ciphertext。

### 17.2 Backup create/verify

- backup create 必须先通过 Master Key 抽象解锁并验证 Vault；
- 备份包含 Key Slot descriptors、descriptor digest 和 Master Key fingerprint；
- 不包含 Workload Identity token、Recovery 身份凭据、密码或明文 DEK；
- backup verify 在没有解锁材料时只能验证外层备份完整性和 manifest；
- 要宣称可恢复，必须在目标恢复身份下执行带 Slot 解锁的 restore drill；
- 历史备份保留创建时的 Slot，rewrap 不追溯修改历史备份。

### 17.3 Restore 顺序

KMS/Key Slot 模式恢复顺序必须改为：

1. 验证备份外层认证和确认 Backup ID；
2. 在 staging 中读取可信 manifest、配置副本和 Key Slot descriptor；
3. 用目标环境配置 allowlist 限制 provider/key/region/account；
4. 通过 KMS 或 Recovery Slot 解锁候选 Master Key；
5. 验证 payload、fingerprint 和 Vault Key Check；
6. 再验证 bbolt schema、Audit、WAL、Usage 和 Parquet；
7. 按现有同文件系统原子目录切换发布恢复结果。

restore 必须通过统一 Master Key 接口选择当前配置模式，不能硬编码为先读取某个文件路径。

## 18. 内存与主机安全

KMS 保护静态 DEK，不保护运行中已解锁进程。PRD 不得把“KMS Key 不可导出”表述为运行期明文不可读取。

最低要求：

- 所有 DEK、payload 和 Credential 字节缓冲区尽快 `clear()`；
- 禁止生产 pprof、core dump 和自动 crash upload；
- 运维文档要求限制 ptrace、调试权限、swap 和容器 capabilities；
- KMS Adapter 不记录原生敏感响应体；
- 评估 `RLIMIT_CORE=0`、`MADV_DONTDUMP` 和可行的内存锁定；
- Go GC 和字符串/HTTP Header 副本意味着不能宣称完整 mlock 或确定性擦除；
- 主机 root 和进程内代码执行仍在信任边界之外。

## 19. Audit、Metrics 与告警

### 19.1 Audit

记录但不包含秘密：

- Slot wrap、unwrap 验证、激活、retire、撤销；
- fallback 到备用 Slot；
- KMS Key/region/account 变更；
- rewrap 和 DEK rotate 生命周期；
- Recovery Slot 使用；
- DR 演练；
- 稳定错误分类和操作者动作。

不得记录 ciphertext、protected payload、DEK、Recovery 身份凭据、Workload Identity token 或 Provider 原始响应体。

### 19.2 Metrics

建议增加：

```text
heimdall_kms_calls_total{provider,operation,result}
heimdall_kms_call_duration_seconds{provider,operation}
heimdall_kms_unlock_attempts_total{provider,result}
heimdall_kms_slot_fallbacks_total{from_type,to_type,result}
heimdall_kms_rewrap_total{provider,result}
heimdall_kms_dek_rotations_total{result}
heimdall_kms_slot_last_verified_timestamp_seconds{slot_type}
```

禁止使用完整 key ID、ARN、项目、tenant、账号、Slot ID 或 ciphertext 作为 Metrics label，避免敏感信息和高基数。

### 19.3 告警

- KMS 解锁持续失败或接近 startup deadline；
- `permission_denied`、`ciphertext_invalid`、`payload_invalid`、`vault_mismatch`；
- 自动 fallback 到备用 Slot；
- 没有第二个 active/verified Slot；
- 备用 Slot 超过验证周期；
- rewrap/rotate pending 状态超时；
- Kubernetes CrashLoop 造成异常 KMS 调用速率。

## 20. Runbook 交付要求

上线前必须具备：

1. 首次 KMS bootstrap；
2. KMS Key Policy、IAM、Grant 和 Workload Identity 生命周期；
3. 启动失败排障和错误分类；
4. 主 KMS Key disabled/deleted 的 break-glass；
5. 跨区域、跨账号恢复；
6. 定期备份恢复和备用 Slot 演练；
7. KEK 正常 rewrap；
8. KEK 泄露后的 DEK rotate 与历史备份处置；
9. crash/pending rotation 恢复；
10. Kubernetes 单副本、`Recreate` 和身份就绪；
11. Metrics、告警和云审计关联排障。

## 21. 测试策略

### 21.1 Provider-neutral fake KMS

离线 CI 必须覆盖：

- wrap/unwrap 成功；
- context/AAD/payload/instance/Slot 篡改；
- timeout、取消、限流、5xx、权限拒绝、key disabled/deleted；
- 身份延迟就绪；
- deadline、backoff 和 jitter 边界；
- fallback 顺序；
- Vault Key Check 强制执行；
- Adapter 不可用；
- 敏感信息不进入错误、日志、Audit 和 Metrics。

### 21.2 状态机与故障注入

- Slot create/verify/activate/retire/revoke 每个 publication kill point；
- rewrap 每个 publication kill point；
- DEK rotate 的 metadata、descriptor、bridge、compaction kill points；
- KMS 成功但本地 bbolt/fsync 失败；
- bbolt 发布成功但进程在验证/清理前终止；
- 恢复使用旧、新、备用和 Recovery Slot；
- 重复命令幂等性。

### 21.3 真实云验证

每个 Adapter 上线前必须在真实账号验证：

- 官方 Workload Identity；
- wrap/unwrap 和 provider-native Context/AAD；
- Key Policy/Grant/region/tenant 错误；
- 限流和真实错误映射；
- 审计日志可关联但无秘密；
- K8s/VM 冷启动；
- disabled、pending deletion 或不可访问 Key；
- DR 身份和备用 Slot；
- ADR 0010 选定 AWS artifact 集合的 SBOM、漏洞扫描和签名。

## 22. 成功指标

- KMS 模式磁盘和备份中不存在明文 Master Key。
- File 模式不初始化 AWS SDK 或产生云调用，AWS SDK 的 module/artifact 边界符合 Accepted ADR 0010。
- 100% KMS 生产配置拥有至少两个已验证解锁 Slot。
- 正常 Gateway 请求产生 0 次 KMS 调用。
- 所有候选 DEK 在使用前 100% 通过 Vault Key Check。
- transient 故障在配置 deadline 内按策略恢复，永久权限/配置错误不会无限重试。
- rewrap 不改变 Master Key fingerprint、Credential Ciphertext 和 KeyVersion。
- 每个支持的 DR 承诺都有最近一次成功恢复演练证据。
- Secret canary 在日志、错误、Audit、Metrics、bbolt、备份和允许的 heap 检查中均不泄露。

## 23. 依赖与风险登记

| 风险 | 影响 | 缓解/门槛 |
|---|---|---|
| 单 KMS Key 误删/禁用 | 永久不可恢复 | 强制第二 verified Slot；删除演练与告警 |
| 区域/账号不可用 | DR 失败 | 跨故障域 Slot；恢复身份和 allowlist 预置 |
| KMS/IAM 短暂故障 | 冷启动失败 | 有界重试、备用 Slot、readiness 和告警 |
| descriptor/算法篡改 | confused deputy/DoS | allowlist、Context/AAD、protected payload、Vault Check |
| Azure 无等价 AAD | provider 绑定较弱 | 记录差异；严格 payload、allowlist 和 Vault Check |
| 云 SDK 依赖膨胀 | 产品和供应链边界扩大 | ADR 0010 实测并冻结 module/artifact 方式 |
| 轮换中崩溃 | metadata/DEK 失配 | COW、bridge、kill-point 和幂等恢复 |
| KEK 泄露后仅 rewrap | 历史备份仍可解密 | 强制 DEK rotate 事故流程 |
| Go 运行时内存副本 | 运行期 DEK 泄露 | 主机加固、禁 core、最短生命周期和明确边界 |
| 单写者被误配多副本 | CrashLoop/KMS 调用风暴 | 单副本、Recreate、先锁后解包和部署校验 |

## 24. 发布阶段与门禁

### Phase 0：核心模型与决策冻结

- 完成统一 Master Key 抽象和最终配置 schema；
- 冻结 Key Slot schema、最终配置、KMSWrapper 和错误分类；
- 完成 AWS SDK spike，并由 ADR 0010 冻结 module/artifact 与发布 CI；
- File 模式的安全不变量和完整生命周期测试必须通过。

门禁：不得包含任何云 SDK 或 KMS 行为。

### Phase 1：通用 Envelope 与恢复路径

- 实现 protected payload、fake KMS、Slot 状态机和 Vault Check 锚定；
- 实现第二 Recovery Slot 强制要求；
- 改造 `doctor`、backup、restore、rotate 和 Audit/Metrics；
- 完成初始化、失败清理和 crash injection。

门禁：单 KMS Slot 配置不能标记 production-ready。

### Phase 2：AWS 生产验证

- 完成 AWS Workload Identity、Encryption Context、真实错误和 DR 演练；
- 按 ADR 0010 的 Accepted 结论交付选定 artifact、SBOM、签名、runbook 和告警。

门禁：真实恢复演练和全部 kill-point 测试通过。

### Phase 3：未来云 Adapter（不属于 M11）

- 出现明确生产需求后分别建立实现范围和验收计划；
- 重新评审 SDK 发布方式，不继承 AWS artifact 结论；
- Azure 等 provider 必须明确 AAD 能力差异和 residual risk。

### Phase 4：后续需求

- 根据客户需求评估 Provider Credential 外部 Secret 引用；
- 根据合规需求评估 FIPS/HSM/KMIP；
- 不与 M11 生产发布捆绑。

## 25. 验收标准

- [ ] File 模式通过统一新配置完成安全校验、CLI、备份、恢复和轮换。
- [ ] AWS SDK module/artifact 方式经过可复现 spike，并由 Accepted ADR 0010 固化。
- [ ] 配置只接受最终统一 schema，未知字段和模式字段混用继续失败。
- [ ] KMS 模式不在磁盘或备份保存明文 Master Key。
- [ ] 生产 KMS 状态要求至少两个 active、verified 解锁 Slot。
- [ ] 跨区域/账号/人工恢复承诺与实际 Slot 配置一致。
- [ ] AWS 使用原生 Encryption Context；未来 provider 的 AAD 差异不进入 M11 验收。
- [ ] 所有解包路径验证 protected payload、fingerprint 和 Vault Key Check。
- [ ] transient、throttled、identity-not-ready、permission、key-unavailable 和篡改错误按表处理。
- [ ] KMS 不可用时已启动 Gateway 不受影响，重启不使用磁盘明文缓存绕过解锁。
- [ ] `doctor` 完整/静态模式语义明确且本地文件保持只读。
- [ ] backup/restore 通过统一 Master Key 接口工作，不依赖固定文件字段。
- [ ] rewrap 不改变 DEK；KEK 泄露 runbook 明确要求 DEK rotate。
- [ ] DEK rotate 的 Slot/metadata/bridge/COW 状态机通过全部 kill-point 测试。
- [ ] Metrics、告警、Audit 和 runbook 全部交付且无敏感 label/payload。
- [ ] Kubernetes 单副本、Recreate、身份延迟和 CrashLoop 场景通过测试。
- [ ] ADR 选定的 AWS artifact 集合具有 SBOM、漏洞报告、签名和真实 AWS 恢复证据。

## 26. 排期结论

架构方向批准并允许预留接口，但在 Phase 0 完成之前不得引入云 SDK；在第二恢复 Slot、启动故障语义、doctor/backup/restore 改造、外层绑定、依赖隔离和 crash recovery 全部满足验收门槛前，不得把 KMS 模式标记为 production-ready 或对外承诺自动 DR。
