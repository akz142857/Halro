# Heimdall Master Key 多 Key Slot 存储与解锁 PRD

状态：Draft — 云中立核心范围已收敛

日期：2026-08-03

里程碑跟踪：`docs/milestone-m11-master-key-custody-aws-kms.md`

## 1. 文档定位

本 PRD 定义 Heimdall Master Key 的最小多 Key Slot 模型，以及当前生产发布必须实现的 Slot 类型、状态机、轮换和恢复要求。

Heimdall 的核心仍然是独立、自包含、云中立的系统。File 模式是完整的一等运行模式，不安装或启用任何云扩展时不得产生外部服务依赖。

AWS 是当前实际使用环境和第一个计划实现的可选 KMS 扩展，但不是 Heimdall 核心架构前提。AWS KMS 的 protected payload、Encryption Context、启动错误、备份恢复、可观测性和真实云验收由 `docs/prd-kms-envelope-integration.zh-CN.md` 进一步约束。云 SDK 的依赖与发布方式由 `docs/adr/0010-kms-sdk-dependency-isolation.md` 决定。

## 2. 背景

Heimdall 当前使用独立的 `master.key` 文件保存 32 字节随机 Master Key。Master Key 通过 HKDF 派生 Provider Credential、Admin MFA、Admin Session、Audit HMAC 等用途的独立子密钥。

当前 Vault 密码学结构保持不变，但明文 `master.key` 长期存在于生产主机磁盘。数据库、主机快照和该文件同时泄露时，攻击者不需要破解 AES 即可解密 Vault。

可选 KMS Envelope 扩展可以消除对应云部署生产主机上的裸 Master Key，但单个外部 KMS Key 又会成为新的恢复单点。Key 被禁用、删除、Policy 配置错误或账号失去访问能力时，如果没有第二个独立包装副本，全部 Vault 数据和历史备份可能永久不可恢复。

因此当前设计采用：

> 一个随机 Master Key 可以由多个 Slot 包装。独立 File 模式保持完整；启用外部 KMS 的生产部署使用 Primary KMS Slot 和 Recovery KMS Slot 消除单 Key 恢复风险。

```text
                       ┌─ File Slot（独立本地模式）
随机 Heimdall Master Key
                       ├─ External KMS Primary Slot（日常启动）
                       └─ External KMS Recovery Slot（break-glass）
```

## 3. 目标

- 保留现有随机 32 字节 Master Key、HKDF、AES-GCM、AAD 和 Vault Key Check。
- 保持 File 模式独立、自包含、无云运行时依赖。
- 允许可选 KMS 扩展消除对应生产主机对明文 `master.key` 的长期依赖。
- 使用最小通用 Slot 状态机支持同一个 Master Key 的多个包装副本。
- 使用 KMS Primary Slot 完成 Workload Identity 无人值守启动。
- 使用独立管理边界的 KMS Recovery Slot 消除单 Key 永久数据丢失风险。
- 直接采用统一 Master Key 配置和 Slot 模型，不保留开发期旧 schema 的兼容包袱。
- 保持 provider-neutral 核心接口，AWS 作为首个扩展实现而非核心绑定。
- 严格区分 KMS KEK rewrap 与 Heimdall Master Key/DEK rotate。
- 保持现有 COW、crash bridge、Keyring、Audit 和备份恢复不变量。
- 解锁失败默认 fail closed，禁止生成新 Key 或静默切换到不受信来源。

## 4. 非目标

当前里程碑不实现：

- 管理员密码或 Passphrase Slot；
- Argon2id 密码解锁；
- 12/24 词恢复助记词；
- 数字货币钱包式恢复流程；
- Shamir Secret Sharing；
- TPM、Secure Enclave、HSM 或 KMIP；
- 当前里程碑内的 GCP Cloud KMS 或 Azure Key Vault Keys Adapter；
- 跨云自动恢复；
- Provider Credential 的 Secrets Manager 外部引用和刷新；
- active-active、多写者、水平扩展或 HPA；
- FIPS 或其他合规认证。

这些能力如出现明确生产需求，必须分别建立 PRD 和威胁模型，不能仅因为 Slot 接口可扩展就自动进入实现范围。

## 5. 核心原则

### 5.1 Master Key 仍然是随机 DEK

Master Key 必须由 CSPRNG 生成并保持 32 字节。KMS Key 是 KEK，只负责包装 Master Key，不替代现有 Vault 的数据加密层。

```text
External KMS KEK
     │
     ▼
wrapped Master Key
     │ unwrap
     ▼
32-byte Master Key
     │
     ▼
现有 Heimdall Vault
```

### 5.2 多 Slot 不等于多 Master Key

所有 active Slot 必须解出同一个 Master Key，并通过同一个 Vault Key Check。Slot 只代表不同的解锁路径，不代表独立 Vault、租户或 Credential 集合。

### 5.3 Slot rewrap 不等于 DEK rotate

- Slot rewrap：KEK 改变，Master Key 不变，不重加密 Credential。
- DEK rotate：Master Key 改变，全部 Vault 材料重新加密，Credential KeyVersion 前进。

CLI、Admin UI、Audit 和 Runbook 必须使用不同名称，不能都显示为“轮换”。

### 5.4 Vault Key Check 是最终信任锚

KMS 解包成功、fingerprint 匹配或 descriptor 格式正确都不足以证明 Master Key 属于当前 Vault。所有解锁路径必须通过 `verifyVaultKeyCheck` 后才能派生或使用任何子密钥。

### 5.5 启用外部 KMS 的生产环境必须有恢复路径

外部 KMS 模式只有在 Primary 和 Recovery Slot 都已经独立解包并通过 Vault Key Check 后，才能标记为 production-ready。纯 File 模式不因未配置 KMS Slot 被降级或判定不完整。

单 KMS Slot 可以作为初始化过程中的短暂 pending 状态，但不得作为发布完成状态。

## 6. 当前 Slot 类型

### 6.1 File Slot

File Slot 是独立部署的一等模式，用于不启用任何外部 KMS 的本地、自包含运行。

要求：

- 文件必须恰好为 32 字节；
- 必须是普通非符号链接文件；
- group/other 不得有任何权限；
- 现有 `0600`、`O_NOFOLLOW` 和原子替换校验不得降低；
- 初始化、启动、轮换、备份和恢复必须使用统一 Master Key 接口；
- 新配置直接采用本 PRD 定义的最终 schema，不保留开发期旧字段别名。

File Slot 与 KMS Slot 是两种明确部署模式。一个实例初始化时选择其模式；当前发布不实现两种模式之间的数据迁移流程。

### 6.2 External KMS Primary Slot

Primary Slot 是启用外部 KMS 后正常启动使用的包装副本。核心状态机不依赖具体云厂商；AWS KMS 是第一个 Adapter 实现。

要求：

- 使用云平台 Workload Identity、Managed Identity 或等价短期身份；
- 不接受长期静态云访问密钥作为推荐生产方式；
- 生产 Runtime 身份具有最小范围的 KMS Decrypt 权限；
- KMS Key、region、account、endpoint 和算法必须受可信配置 allowlist 约束；
- 使用 provider 支持的 Context/AAD 绑定 instance、Slot、格式版本和用途；
- 正常 Gateway 请求不调用 KMS；
- 启动、`doctor`、恢复、rewrap 和 rotate 按 KMS PRD 的故障语义调用。

AWS Adapter 额外使用 IAM Role/EC2/ECS/EKS 身份和 AWS Encryption Context；这些是 Adapter 要求，不得写入通用 Slot 状态机。

### 6.3 External KMS Recovery Slot

Recovery Slot 包装同一个 Master Key，但属于独立恢复管理边界。

至少应与 Primary Slot 在以下一个或多个维度隔离：

- 不同 KMS Key；
- 不同 Key Policy 管理者；
- 不同云账号、项目、tenant 或等价管理域；
- 不同恢复角色或授权流程；
- 存在区域级恢复要求时，目标恢复区域可访问。

推荐安全边界：

- 日常 Heimdall Runtime 身份不默认拥有 Recovery Key 的 Decrypt 权限；
- break-glass 时由受控恢复角色临时取得权限；
- 恢复操作要求加强确认并同时产生云平台和 Heimdall Audit；
- Recovery Slot 必须定期执行隔离恢复演练；
- Recovery Slot 长期未验证或 Key 不可用必须告警。

Recovery Slot 不一定自动 fallback。是否自动尝试必须由可信策略明确；默认行为是在 Primary 永久错误时 fail closed，提示操作者按 Runbook 启用恢复身份或显式选择 Recovery Slot。

## 7. Slot 数据模型

建议在 bbolt 未加密系统元数据区保存版本化 descriptor。以下使用首个 AWS Adapter 作为示例；通用 schema 不能要求所有 provider 使用 AWS ARN、region 或算法名称：

```json
{
  "format_version": 1,
  "master_key_fingerprint": "sha256:...",
  "active_generation": 1,
  "slots": [
    {
      "id": "slot_aws_primary",
      "type": "aws-kms",
      "purpose": "primary",
      "state": "active",
      "key_id": "arn:aws:kms:...",
      "region": "ap-southeast-1",
      "algorithm": "SYMMETRIC_DEFAULT",
      "wrapped_key": "base64...",
      "created_at": "...",
      "verified_at": "...",
      "revision": 1
    },
    {
      "id": "slot_aws_recovery",
      "type": "aws-kms",
      "purpose": "recovery",
      "state": "active",
      "key_id": "arn:aws:kms:...",
      "region": "...",
      "algorithm": "SYMMETRIC_DEFAULT",
      "wrapped_key": "base64...",
      "created_at": "...",
      "verified_at": "...",
      "revision": 1
    }
  ]
}
```

descriptor 不包含明文 Master Key，但仍属于安全敏感控制数据。它必须受到：

- bbolt 事务；
- revision/乐观并发控制；
- 配置 allowlist；
- protected payload 与 Encryption Context；
- Vault Key Check；
- Audit；
- 备份 manifest digest；
- COW 和 compaction 发布语义。

数据库中的 `key_id`、region、account、endpoint 和 algorithm 不得被直接信任。网络调用目标必须先与本地可信配置匹配。

## 8. Slot 状态机

Slot 状态限定为：

```text
pending → active → retiring → revoked
```

### pending

- wrapped payload 已生成；
- 尚未完成独立 unwrap 和 Vault Key Check；
- 不能用于正常启动；
- 不能计入生产恢复路径。

### active

- 已独立 unwrap；
- protected payload、fingerprint 和 Vault Key Check 全部通过；
- 可以按 purpose 用于 primary 或 recovery 解锁。

### retiring

- 正在退出正常选择范围；
- 在受控恢复窗口内仍可解锁；
- 不能被当作唯一 production-ready Slot。

### revoked

- 不得自动或手工用于正常解锁；
- descriptor 保留最小审计元数据；
- wrapped ciphertext 的物理移除遵守 COW/compaction 规则。

状态转换必须是显式、审计和幂等的。不能通过文件存在、KMS Key 可访问或 unwrap 偶然成功自动改变状态。

## 9. 安全不变量

- 不能删除或撤销最后一个已验证的 active Slot。
- production-ready 外部 KMS 模式必须同时存在 active Primary 和 active Recovery Slot；纯 File 模式不受此约束。
- 所有 active Slot 必须解出相同 fingerprint 并通过同一 Vault Key Check。
- 解锁失败不得生成新 Master Key、覆盖 metadata 或启动部分 Runtime。
- fingerprint 只能快速拒绝，不能代替认证。
- Recovery Slot 使用必须产生高严重度 Audit。
- KMS ciphertext、Encryption Context、protected payload、DEK 和 Provider Credential 不得进入日志、Metrics 或错误响应。
- Slot ID、purpose 和 provider 可以进入低基数 Metrics；完整 ARN、账号、ciphertext 不得成为 label。
- 已成功启动的 Runtime 不依赖后续 KMS 可用性；进程重启必须重新解锁。
- 禁止把明文 DEK 写入环境变量、临时文件、Kubernetes Secret 或磁盘缓存作为启动宽限。

## 10. 生命周期

### 10.1 新 KMS 扩展实例初始化

1. 验证配置、目标 KMS Adapter、Workload Identity 和两个目标 KMS Key。
2. 使用 `crypto/rand` 在内存生成 32 字节 Master Key。
3. 创建 Vault Key Check 和初始 Keyring/Audit 材料。
4. 使用 Primary KMS Key 创建 pending Primary Slot。
5. 独立 unwrap Primary Slot，验证 payload、fingerprint 和候选 Master Key。
6. 使用 Recovery KMS Key 创建 pending Recovery Slot。
7. 在受控恢复身份下独立 unwrap Recovery Slot，并确认它解出同一 Master Key。
8. 原子写入 metadata、两个 descriptors 和 Vault Key Check。
9. 通过 Primary Slot 重新解锁并验证已经持久化的 Vault Key Check。
10. 将两个 Slot 原子标记 active，实例才可进入 production-ready。
11. 清理所有明文 DEK 和临时 payload 缓冲区。

任一步失败都不得留下看似完成但只有一个可用 Slot 的生产实例。

### 10.2 新增或更换 Slot

1. 先用现有 active Slot 解锁并通过 Vault Key Check。
2. 使用新 KMS Key 包装同一个 Master Key，创建 pending Slot。
3. 使用目标身份独立 unwrap 并验证。
4. 原子激活新 Slot。
5. 需要替换旧 Slot 时，将旧 Slot 标记 retiring。
6. 完成恢复验证后再显式 revoke。

新增 Slot 不推进 Credential KeyVersion，也不使 Admin Session 失效。

### 10.3 删除或撤销 Slot

- 不能删除最后一个 active Slot；
- 外部 KMS 生产模式不能删除到只剩 Primary 或只剩 Recovery；
- 删除 Primary 前必须先指定和验证新的 Primary；
- 删除 Recovery 前必须先创建和演练新的 Recovery；
- 删除操作必须要求当前 revision 和加强确认；
- KMS Key 的禁用/删除和 Heimdall Slot revoke 是两个独立操作，Runbook 必须规定顺序；
- 必须考虑历史备份仍保存旧 descriptor，删除当前 Slot 不会追溯修改历史备份。

## 11. KEK Rewrap 与 DEK Rotate

### 11.1 KEK Rewrap

rewrap 更换 KMS KEK，但保持 Master Key 不变：

```text
旧 active Slot unwrap
        │
        ▼
同一个 Master Key
        │
        ▼
新 KMS Key wrap
        │
        ▼
pending → verify → active
```

要求：

- 新 Slot 必须独立验证后才能 retire 旧 Slot；
- Master Key fingerprint 不变；
- Credential Ciphertext 和 KeyVersion 不变；
- Admin Session 默认不失效；
- 旧 Slot 在恢复窗口结束后显式 revoke；
- rewrap 不能修复历史备份对旧 KEK 的依赖。

### 11.2 KEK 泄露

如果旧 KEK、Workload Identity 或可执行 Decrypt 的权限疑似泄露，仅 rewrap 不足。攻击者仍可能结合旧 KEK 权限和历史备份解出历史 Master Key。

必须：

1. 隔离泄露身份和 Key Policy；
2. 使用可信 Recovery Slot 解锁；
3. 执行 Master Key/DEK rotate；
4. 使用可信 KMS Keys 包装新 Master Key；
5. 创建新备份；
6. 隔离、重加密或销毁受影响历史备份；
7. revoke 旧 Slots 和权限并保全审计证据。

### 11.3 Master Key/DEK Rotate

DEK rotate 继续是离线重型操作：

- 生成新的 32 字节随机 Master Key；
- 为新 Master Key 创建并验证新的 Primary 和 Recovery Slots；
- 重加密所有 Credential、MFA 和系统保护材料；
- 推进 Keyring 和 Credential KeyVersion；
- 使 Admin Session 失效；
- 保持 Audit HMAC 连续性；
- 复用并扩展现有 COW、crash bridge 和 compaction；
- 在每个 publication kill point 后可以由旧代或新代恢复；
- 新 metadata、Vault Key Check 和新 Slot generation 必须形成一致原子发布代。

KMS 模式不能直接套用 `ReplaceMasterKey(path)`；实现前必须完成 descriptor、metadata 和 Slot generation 的 crash-recovery 设计。

## 12. 启动与恢复选择

### 12.1 正常启动

- 默认只选择配置指定的 active Primary Slot；
- 对 timeout、限流和身份尚未就绪进行有界重试；
- permission denied、Key disabled/deleted、payload/Vault mismatch 快速失败；
- 解锁和 Vault/Audit 校验完成前 readiness 为 false；
- 不启动 Gateway 或 Admin Listener。

### 12.2 Recovery 启动

Recovery Slot 默认不被日常 Runtime 身份自动尝试。恢复流程要求：

- 操作者明确选择 Recovery Slot；
- 使用受控恢复身份；
- 记录恢复原因和 Audit；
- 通过 Vault Key Check；
- 启动后修复或创建新的 Primary Slot；
- 完成恢复演练或真实事故后撤销临时权限。

如果未来需要自动 fallback，必须单独定义威胁模型，确保不会掩盖 Primary Slot 的权限篡改或 Key 异常。

## 13. 备份、恢复与历史密钥

- Key Slot descriptors 和 wrapped Master Key 进入加密备份；
- 云身份 token、明文 DEK 和 Recovery Role 凭据不得进入备份；
- manifest 记录 Master Key fingerprint 和 descriptor digest；
- restore 必须先验证备份外层，再从 staging 读取 descriptor；
- 目标恢复配置必须 allowlist provider、region、account 和 key ID；
- 候选 Master Key 必须通过 protected payload、fingerprint 和 Vault Key Check；
- 只有随后才能验证 bbolt、Audit、WAL、Usage 和 Parquet；
- KEK rewrap 不会修改历史备份中的旧 descriptor；
- 旧 KMS Key、旧 Slot descriptor 和历史备份的保留/销毁必须使用同一份清单管理；
- production-ready 前必须在目标恢复身份和环境完成真实备份恢复演练。

## 14. 配置模型

项目仍在开发期，配置直接采用最终统一结构，不保留旧 `storage.master_key_file` 字段。

File 模式：

```yaml
storage:
  master_key:
    mode: file
    file: /secure/master.key
```

Key Slot 模式：

```yaml
storage:
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

要求：

- `KnownFields(true)` 继续生效；
- `mode` 必须显式为 `file` 或 `key_slots`；
- File 和 Key Slot 字段混用必须失败；
- `primary_slot` 和 `recovery_slot` 必须引用不同 Slot，且分别匹配 `allowed_kms_keys[].purpose`；
- `startup_deadline` 是启动解锁的总时限，`call_timeout` 是单次 KMS 调用时限，且单次时限必须小于总时限；
- `config check` 做静态 allowlist 和 schema 检查，不主动调用 KMS；
- 配置变更需要重启；
- schema version 和严格字段校验必须有测试。

## 15. Audit 与可观测性

必须 Audit：

- Slot create、verify、activate、retire、revoke；
- Primary/Recovery purpose 变更；
- Recovery Slot 使用；
- KEK rewrap；
- DEK rotate；
- KMS 稳定错误分类；
- 恢复演练。

最低 Metrics：

```text
heimdall_kms_calls_total{operation,result}
heimdall_kms_call_duration_seconds{operation}
heimdall_kms_slot_last_verified_timestamp_seconds{purpose}
heimdall_kms_recovery_uses_total{result}
```

不得使用完整 ARN、账号、Slot ID 或 ciphertext 作为 label。

最低告警：

- Primary 解锁持续失败；
- production-ready 状态缺少 active Recovery Slot；
- Recovery Slot 超过规定周期未验证；
- Recovery Slot 被使用；
- rewrap/rotate 卡在 pending 或 retiring；
- Vault Key Check mismatch；
- KMS 调用出现异常速率。

## 16. 当前实施阶段

### Phase 0：统一 Master Key 模型

- 引入统一 Master Key 解锁接口；
- 将 runtime、init、bootstrap、Admin、Audit、Metrics、`doctor`、backup、restore 和 rotate 迁入接口；
- 直接实现新的统一配置 schema；
- 保留现有 File 模式的安全检查，但不保留旧配置字段兼容；
- 不引入任何云 KMS 行为或运行时依赖。

### Phase 1：最小 Key Slot 状态机

- descriptor schema；
- pending/active/retiring/revoked；
- revision 和 bbolt 事务；
- add/verify/retire/revoke；
- Vault Key Check；
- 严格配置和状态校验。

### Phase 2：首个可选 KMS 扩展（AWS）

- 保持通用 Slot 核心不依赖 AWS 类型；
- AWS Workload Identity；
- Primary/Recovery KMS wrap/unwrap；
- Encryption Context 和 protected payload；
- 错误分类和有界重试；
- KMS 模式新实例初始化。

### Phase 3：完整生命周期

- KEK rewrap；
- DEK rotate；
- COW/crash bridge/compaction；
- `doctor`、backup、restore；
- kill-point 和幂等恢复测试。

### Phase 4：AWS 扩展生产验收

- 真实 AWS 账号测试；
- Primary Key disabled/deleted/permission denied；
- Recovery Role break-glass；
- 备份恢复演练；
- Audit、Metrics、告警和 Runbook；
- SBOM、签名和安全评审。

## 17. 后续扩展

以下仅保留概念扩展点，不进入当前 schema、CLI、UI 或验收：

- Passphrase Slot；
- Recovery Seed/助记词 Slot；
- TPM/HSM/KMIP Slot；
- Shamir 多人恢复；
- GCP/Azure KMS Slot（未来仍通过同一云中立接口扩展）；
- 自动跨云 fallback。

未来新增 Slot 类型必须证明：

- 不降低 Vault Key Check 和 fail-closed 不变量；
- 不把低熵密码直接当 Master Key；
- 不扩大默认 Runtime 的秘密持久化；
- 能覆盖备份、恢复、rewrap、rotate、Audit 和 crash recovery；
- 有明确真实生产需求，而不是仅因接口可扩展。

## 18. 验收标准

- [ ] File 模式使用统一新配置完成初始化、启动、轮换、备份和恢复。
- [ ] 不启用云扩展时，Heimdall 保持独立、自包含且不产生云网络调用。
- [ ] 启用 AWS 扩展的生产主机和 Heimdall 备份中不存在明文 Master Key。
- [ ] Primary 和 Recovery Slots 解出同一个 Master Key 并分别通过 Vault Key Check。
- [ ] 启用外部 KMS 时，只有一个 active KMS Slot 不能标记 production-ready；纯 File 模式不受影响。
- [ ] 日常 Runtime 身份不默认拥有 Recovery Slot Decrypt 权限。
- [ ] Recovery Slot 使用需要显式操作者动作、加强确认和 Audit。
- [ ] 不能删除最后一个 active Slot，也不能让外部 KMS 生产配置缺少 Primary 或 Recovery。
- [ ] KEK rewrap 不改变 Master Key fingerprint 或 Credential KeyVersion。
- [ ] KEK 泄露 Runbook 明确要求 DEK rotate 和历史备份处置。
- [ ] DEK rotate 的 descriptor、metadata、Vault Key Check、bridge 和 COW 通过全部 kill-point 测试。
- [ ] `doctor`、backup 和 restore 通过统一 Master Key 接口工作，不硬编码旧配置字段。
- [ ] KMS 解锁失败不会生成新 Key、覆盖 Vault 或启动 Gateway/Admin。
- [ ] 正常 Gateway 请求产生 0 次 KMS 调用。
- [ ] KMS Key disabled/deleted、Policy 拒绝、限流、超时和身份延迟均有真实或等价故障测试。
- [ ] 完成 Primary 失败后使用 Recovery Slot 恢复真实备份的演练。
- [ ] Passphrase、助记词、Shamir、TPM/HSM、GCP 和 Azure 不进入当前发布完成条件。

## 19. 最终决策

Heimdall 当前保留最小多 Key Slot 架构，但不实现数字货币钱包式密码和助记词系统。

当前核心和首个扩展模型为：

```text
File Slot                 独立、自包含的一等模式
External KMS Slot model   可选扩展边界
  └─ AWS Primary Slot     首个扩展的日常无人值守启动
  └─ AWS Recovery Slot    首个扩展的独立 break-glass 恢复
```

该范围保持 Heimdall 核心云中立和 File 模式完整可用，同时允许 AWS 作为当前第一个生产级扩展解决外部根密钥托管和单 KMS Key 恢复风险。未来 provider 可以复用通用 Slot 不变量，但不能使核心系统与 AWS 或任何云绑定。
