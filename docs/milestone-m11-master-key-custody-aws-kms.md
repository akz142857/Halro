# M11：生产级 Master Key 托管与 AWS KMS 扩展

状态：In Progress

最后更新：2026-08-04

当前完成度：0/8 PR slices，0/16 release gates

1.1.0 本地实现状态：KS-1 Slot revoke、KS-2 Recovery 离线修复契约、KS-3 只读 Custody API/UI 已完成；UI 只声明 local descriptor/custody readiness，并明确显示 KS-4 真实 AWS、独立操作者、最终 RC 供应链证据与四方签署仍 blocked。该本地实现状态不改变下面的 PR merge/reviewer 和 release gate 计数。

## 1. 里程碑目标

M11 将以下三项作为一个不可拆分的生产里程碑交付：

```text
生产级 Master Key 托管与 AWS KMS 扩展
│
├── Master Key Key Slot
│   └── 定义云中立的密钥模型、状态机和生命周期
│
├── AWS KMS Envelope
│   └── 实现首个可选生产 Adapter、安全边界和运维能力
│
└── SDK 依赖与发布决策
    └── 决定 AWS SDK 如何进入构建、SBOM、签名和发布
```

里程碑完成后：

- Heimdall 核心仍然独立、自包含、云中立；
- File 模式不安装、不调用也不依赖任何云服务；
- AWS KMS 是首个可选的生产级扩展，不是核心架构前提；
- 启用 AWS KMS 的生产实例不在主机磁盘或 Heimdall 备份中保存明文 Master Key；
- Primary 和 Recovery KMS Slots 可以独立解锁同一个 Vault；
- KMS 不进入 Gateway 请求热路径；
- 初始化、轮换、诊断、备份和恢复形成完整生产闭环。

本里程碑是当前计划使用 AWS KMS 的生产发布门禁。在全部 release gates 完成前，不得把 AWS KMS 模式标记为 production-ready。

## 2. 设计依据

- `docs/prd-master-key-key-slots.zh-CN.md`
- `docs/prd-kms-envelope-integration.zh-CN.md`
- `docs/adr/0010-kms-sdk-dependency-isolation.md`
- `docs/adr/0001-single-process-architecture.md`
- `docs/threat-model.md`
- `docs/backup-restore.md`
- `docs/operator-guide.md`

三份 M11 设计文档保持独立职责，不合并成一个超大文档。实现也必须拆成多个可独立审核、测试和回退的 PR，不允许一次性大提交。

## 3. 已冻结的产品边界

### 3.1 核心不与 AWS 绑定

- File 模式是一等独立运行模式；
- 通用 Slot 核心不得出现 AWS ARN、region、IAM 或 Encryption Context 类型；
- AWS 通过窄、显式、编译期 Adapter 接入；
- 不实现插件发现、动态加载、热更新、插件目录或插件市场；
- 是否采用 Adapter 抽象以真实第二实现为前提，本里程碑中 File 与 AWS KMS 已构成该前提。

### 3.2 直接采用最终模型

项目仍在开发期，不承担不存在的历史兼容责任：

- 不保留旧 `storage.master_key_file` 配置别名；
- 不实现 Legacy File Slot；
- 不实现 File → KMS 数据迁移；
- 不实现开发期 schema 回滚矩阵；
- 配置直接采用最终 `storage.master_key.mode` 结构；
- 实例初始化时明确选择 `file` 或 `key_slots` 模式。

### 3.3 当前必须实现

- 统一 Master Key 解锁接口；
- File Slot 独立模式；
- 最小 Key Slot descriptor 和状态机；
- AWS KMS Primary Slot；
- AWS KMS Recovery Slot；
- AWS Workload Identity/IAM Role；
- AWS Encryption Context；
- protected key payload；
- Vault Key Check；
- 配置 allowlist；
- KMS 调用 timeout、retry、jitter 和稳定错误分类；
- 新 File 实例和新 AWS KMS 实例初始化；
- KEK rewrap；
- Master Key/DEK rotate；
- descriptor、Keyring、COW、crash bridge 和 compaction；
- `doctor`、backup、restore、bootstrap 和 Admin 离线命令；
- Audit、基础 Metrics、告警和 Runbook；
- fake KMS、kill-point 和真实 AWS 恢复演练；
- AWS SDK 构建、SBOM、签名和发布决策。

### 3.4 当前明确不实现

- GCP Adapter；
- Azure Adapter；
- Passphrase/Argon2id Slot；
- 12/24 词 Recovery Seed；
- 数字货币钱包式密码本；
- Shamir Secret Sharing；
- TPM、Secure Enclave、HSM 或 KMIP；
- 跨云自动恢复；
- Provider Credential 的 Secrets Manager 外部引用；
- 插件系统；
- active-active、多写者、HPA；
- FIPS 或其他合规认证。

这些能力不能阻塞 M11，也不能在没有独立 PRD 和真实需求时顺带进入实现。

## 4. 关键生产决策

### 4.1 两个独立 KMS Slots

AWS KMS 模式的 production-ready 状态必须同时存在：

```text
AWS KMS Primary Slot
        +
AWS KMS Recovery Slot
```

Recovery Slot 至少在以下一个或多个维度与 Primary 隔离：

- 不同 KMS Key；
- 不同 Key Policy 管理者；
- 不同 AWS 账号；
- 不同恢复角色或授权流程；
- 存在区域恢复目标时，目标区域可访问。

日常 Runtime 身份默认不拥有 Recovery Key 的 Decrypt 权限。Recovery 使用必须由操作者显式触发、通过 Vault Key Check、记录高严重度 Audit，并在完成后撤销临时授权。

### 4.2 Rewrap 与 DEK Rotate

- KEK rewrap 只替换 KMS 包装，不改变 Master Key、Credential Ciphertext 或 KeyVersion；
- KEK 或可执行 Decrypt 的身份疑似泄露时，仅 rewrap 不足；
- 泄露响应必须执行 DEK rotate、创建新备份并处置仍受旧 KEK 影响的历史备份；
- DEK rotate 必须保留现有 Audit 连续性、Admin Session 失效、COW、crash bridge 和 compaction 不变量。

### 4.3 SDK 与发布方式已冻结

ADR 0010 已根据 Phase 0 的可复现测量接受方案 A；本次决策只比较当前真实需要的两种方案：

| 方案 | 描述 | 当前状态 |
|---|---|---|
| A | 主 Heimdall 二进制直接包含 AWS SDK；未启用时不访问 AWS | Accepted |
| B | `heimdall` 与 `heimdall-aws` 两个签名 artifact | Rejected for M11 |

Spike 必须测量：

- `go.mod`/`go.sum` 增量；
- 二进制和容器体积；
- 构建、测试和冷启动影响；
- SBOM 与漏洞面；
- Workload Identity 接入复杂度；
- 一个与两个 artifact 的 CI、签名和发布成本。

GCP/Azure artifact 不进入本次决策。Spike 证据已归档，ADR 0010 已更新为 `Accepted`；生产 AWS SDK Adapter 只能建立在该冻结契约和单 artifact 发布布局之上。

## 5. 进度状态定义

| 状态 | 含义 |
|---|---|
| Not Started | 尚未开始，前置条件可能未满足 |
| In Progress | 已有负责人和活动分支/PR |
| Blocked | 存在明确阻塞，必须记录原因和解除条件 |
| In Review | 代码完成，等待安全/工程/运维评审 |
| Complete | 合并且所有 exit evidence 已归档 |

只有“代码已合并 + 测试通过 + 文档/证据存在”才能标记 Complete。设计讨论完成或单元测试局部通过不等于完成。

## 6. PR Slice 进度

| ID | PR Slice | 状态 | 前置依赖 | Exit evidence | Issue / PR / Commit | 备注 |
|---|---|---|---|---|---|---|
| M11-PR1 | 统一 Master Key 核心与最终配置 | In Review | 无 | File 模式完整测试；所有直接文件调用收口；新 schema 严格校验；`config check` 零 KMS 调用 | [#56](https://github.com/akz142857/Heimdall/issues/56)；[PR #64](https://github.com/akz142857/Heimdall/pull/64)；`3066541` | `go test ./...`、关键包 Race、Vet 已通过；不引入 AWS SDK |
| M11-PR2 | Key Slot descriptor 与状态机 | In Review | PR1 | pending/active/retiring/revoked 事务、revision、Vault Key Check 和错误测试 | [#57](https://github.com/akz142857/Heimdall/issues/57)；[PR #65](https://github.com/akz142857/Heimdall/pull/65)；`510581a`、`eb8fc83` | provider-neutral 状态机与 CI 全部通过；待独立 review/merge |
| M11-PR3a | AWS SDK spike、契约评审与 ADR 决策 | In Review | PR1 | A/B module/binary/container/build/test/cold-start/SBOM/vuln 实测；KMSWrapper/错误分类；fake KMS fault tests；ADR 0010 Accepted | [#58](https://github.com/akz142857/Heimdall/issues/58)；[PR #66](https://github.com/akz142857/Heimdall/pull/66)；`512d517`、`258c430` | 选择单 module/单 `heimdall` artifact；不含生产 AWS Adapter；完整 Test、Race、Vet、vuln 与边界检查通过 |
| M11-PR3b | AWS KMS Adapter 实现 | In Progress | PR3a | Workload Identity、wrap/unwrap、Context、allowlist、重试和真实 AWS smoke tests | [#59](https://github.com/akz142857/Heimdall/issues/59)；[PR #67](https://github.com/akz142857/Heimdall/pull/67)；`docs/evidence/m11-03b-aws-kms-adapter-2026-08-03.md` | 本地实现与门禁通过；真实 AWS/CloudTrail evidence 待有效 Workload Identity 与现有 KMS Key |
| M11-PR4 | AWS KMS 初始化与双 Slot 恢复 | In Progress | PR2、PR3b | 新实例原子初始化；Primary/Recovery 独立验证；失败清理；CLI 测试 | [#60](https://github.com/akz142857/Heimdall/issues/60)；[PR #69](https://github.com/akz142857/Heimdall/pull/69)；`docs/evidence/m11-04-dual-slot-initialization-2026-08-03.md` | 本地实现、Secret Canary 与 kill-point 门禁通过；真实 AWS 双 Key evidence 待有效身份和现有 Keys；不含 File→KMS 迁移 |
| M11-PR5 | Rewrap、DEK Rotate 与崩溃恢复 | In Progress | PR2、PR4 | COW/bridge/Keyring/compaction；全部 publication kill points；幂等恢复 | [#61](https://github.com/akz142857/Heimdall/issues/61)；[Draft PR #71](https://github.com/akz142857/Heimdall/pull/71)；`docs/evidence/m11-05-key-lifecycle-2026-08-03.md` | 本地实现与 kill-point matrix 已落地；真实 AWS 证据待补 |
| M11-PR6 | Doctor、Backup、Restore 与 DR | In Progress | PR4、PR5 | 完整/静态 doctor；备份 manifest；KMS restore；真实 Recovery Slot 恢复演练 | [#62](https://github.com/akz142857/Heimdall/issues/62)；[Draft PR #72](https://github.com/akz142857/Heimdall/pull/72)；`docs/evidence/m11-06-kms-dr-2026-08-03.md` | 本地实现与门禁通过；真实 AWS 与独立操作者证据待补 |
| M11-PR7 | 生产交付与发布门禁 | In Progress | PR1–PR6（含 PR3a/PR3b） | Audit/Metrics/alerts；主机加固；IAM/Key Policy；VM/K8s Runbook；SBOM、签名、安全评审 | [#63](https://github.com/akz142857/Heimdall/issues/63)；[Draft PR #73](https://github.com/akz142857/Heimdall/pull/73)；`docs/evidence/m11-07-production-readiness-2026-08-03.md` | 本地生产基线与门禁通过；真实 AWS、RC artifact 与四方签署待补，尚不可 production-ready |

## 7. PR Slice 详细范围

### M11-PR1：统一 Master Key 核心与最终配置

- 定义最小 `MasterKeyProvider`/`MasterKeyUnlocker` 边界；
- 统一 runtime、init、bootstrap、Admin、Audit、Metrics、`doctor`、backup、restore 和 rotate 的 Key 获取；
- 配置直接采用：

```yaml
storage:
  master_key:
    mode: file
    file: /secure/master.key
```

Key Slot 模式采用同一份冻结 schema：

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

- 未知字段和模式专属字段混用必须失败；
- Key Slot 模式统一使用 `primary_slot`、`recovery_slot`、`startup_deadline`、`call_timeout` 和带 `purpose` 的 `allowed_kms_keys`；
- `config check` 只做 schema 和 allowlist 静态检查，并以测试证明不会初始化 Adapter 或调用 KMS；
- 保留 File 模式现有 `0600`、普通文件、no-follow、32 字节和原子发布安全要求；
- DEK、payload 和 Credential 可变字节缓冲区必须尽快清理，禁止不必要的 `string` 转换和长生命周期副本；
- 不保留开发期旧字段兼容；
- 不引入云类型、SDK 或网络调用。

### M11-PR2：Key Slot descriptor 与状态机

- 版本化 descriptor；
- `pending → active → retiring → revoked`；
- Primary/Recovery purpose；
- revision 和乐观并发；
- add、verify、retire、revoke；
- 禁止删除最后一个 active Slot；
- KMS 模式禁止缺少 active Primary 或 Recovery；
- fingerprint 只能快速拒绝，Vault Key Check 是最终信任锚；
- bbolt COW、Audit 和 compaction 边界。

### M11-PR3a：AWS SDK spike、契约评审与 ADR 决策

- 对方案 A/B 完成可复现测量；
- 更新 ADR 0010 为最终 Accepted 决策；
- 冻结最小 `KMSWrapper`、请求/响应大小边界和 typed error taxonomy；
- 使用 fake KMS 完成 timeout、cancellation、fault-injection 和 contract tests；
- spike 实验代码不得作为生产 AWS SDK 实现随本 PR 合并；
- G3 完成后才允许 PR3b 合入。

### M11-PR3b：AWS KMS Adapter

- 使用官方 AWS SDK，不自行重写 IAM、SigV4 或 token refresh；
- Workload Identity/IAM Role；
- Encrypt/Decrypt 或经 spike 确认的 AWS Envelope API；
- Encryption Context；
- 固定、版本化、受大小限制的 protected payload；
- KMS Key/account/region/endpoint allowlist；
- AWS 原生错误到已冻结 typed error taxonomy 的映射；
- 单次 timeout、总 deadline、bounded retry 和 full jitter；
- 真实 AWS smoke evidence；
- 本切片不依赖 PR2 的 Slot 状态机，可在 PR3a 完成后与 PR2 并行；集成在 PR4 收口。

### M11-PR4：初始化与双 Slot 恢复

- KMS 模式新实例生成随机 32 字节 Master Key；
- Primary 和 Recovery 分别 wrap；
- 两个 Slot 分别 unwrap 并验证同一 Master Key；
- 原子发布 descriptors、Keyring、Vault Key Check 和 Audit 材料；
- 发布后从 Primary 重新解锁并验证持久化 Vault；
- 只有一个有效 Slot 时不得 production-ready；
- 任意失败都不得留下部分初始化实例；
- Recovery 默认要求显式操作者和受控恢复身份。

### M11-PR5：生命周期与崩溃恢复

- 新 Key Slot add/verify/activate；
- KEK rewrap；
- Master Key/DEK rotate；
- Credential、MFA 和系统材料全量重加密；
- Credential KeyVersion 前进；
- Admin Session 失效；
- Audit HMAC 连续；
- descriptor generation、Vault Key Check、COW database 和 crash bridge 一致发布；
- 每个 kill point 可以由旧代或新代恢复；
- KEK 泄露测试证明 rewrap 不被错误当作事故修复。

### M11-PR6：诊断、备份与恢复

- `doctor` 完整模式允许只读 KMS unwrap；
- `doctor --no-kms` 只做静态检查并明确报告 `vault_unverified`；
- backup manifest 包含 descriptor digest 和 Master Key fingerprint；
- 备份不包含明文 DEK、云身份 token 或恢复凭据；
- restore 从 staging descriptor 和目标 allowlist 解锁；
- Vault Key Check 成功后才验证 bbolt、Audit、WAL、Usage 和 Parquet；
- rewrap 不追溯修改历史备份；
- 使用 Recovery Slot 在隔离环境完成真实恢复演练。

### M11-PR7：生产交付

- AWS 与 Heimdall Audit 关联；
- 低基数 KMS Metrics；
- Primary 解锁失败、Recovery 缺失/过期/使用、Vault mismatch 和 pending rotation 告警；
- IAM Role、Key Policy、Recovery Role 和授权生命周期示例；
- KMS bootstrap、break-glass、rewrap、DEK rotate、备份恢复和事故 Runbook；
- Kubernetes 单副本、`Recreate`、身份就绪和 CrashLoop 测试；
- VM/systemd 部署；
- 生产禁用 pprof、core dump 和自动 crash upload；设置或验证 `RLIMIT_CORE=0`，评估并在支持的平台启用 `MADV_DONTDUMP`；
- Runbook 和部署基线限制 ptrace、调试权限、swap 与容器 capabilities，并记录 Go GC 下不能保证确定性擦除的边界；
- artifact SBOM、漏洞扫描、checksums 和签名；
- 安全、后端、SRE 和发布四方评审。

## 8. Release Gates

| Gate | 状态 | 必需证据 | Evidence link/commit |
|---|---|---|---|
| G1 File 模式保持云中立且完整可用 | In Review | File init/start/rotate/backup/restore tests | PR #64 起的全仓 File 回归与 KMS boundary 持续通过；待堆叠 PR review/merge |
| G2 最终配置 schema 冻结 | In Review | 统一字段 config tests + reviewed example + `config check` zero-KMS-call test | PR #64；严格 schema 与 CLI zero-call test 已落地 |
| G3 AWS SDK 发布决策冻结 | In Review | spike report + Accepted ADR 0010 | [M11-03A evidence](evidence/m11-03a-aws-sdk-spike-2026-08-03.md)；[PR #66](https://github.com/akz142857/Heimdall/pull/66) |
| G4 Primary/Recovery 双 Slot | In Progress | 两个 Key 独立 unwrap + Vault Check | 本地双独立 fake KMS、原子初始化和 Recovery Audit 通过；真实 AWS 双 Key 待执行 |
| G5 密钥材料不落盘且主机边界加固 | In Progress | filesystem/backup inspection；logs/errors/Audit/Metrics/bbolt/heap canary；core/pprof/RLIMIT/ptrace evidence | Canary、`RLIMIT_CORE=0`、Linux non-dumpable 与 K8s/systemd 基线已落地；最终 RC/目标主机证据待补 |
| G6 KMS 不进入请求热路径 | In Review | request-path zero-call test | `TestKMSBootstrapAndRuntimeUsePrimaryOnlyOutsideRequestPath` 与 boundary script 通过 |
| G7 错误分类和有界重试 | In Review | timeout/throttle/identity/IAM/key-state tests | PR #66/#67 contract、retry、Adapter tests 通过；真实 AWS throttle/identity evidence 待补 |
| G8 KEK rewrap 正确 | In Progress | fingerprint/KeyVersion 不变证据 | `docs/evidence/m11-05-key-lifecycle-2026-08-03.md`；真实 AWS 待补 |
| G9 DEK rotate 正确 | In Progress | 全量重加密 + session/audit/keyring evidence | `docs/evidence/m11-05-key-lifecycle-2026-08-03.md`；真实 AWS 待补 |
| G10 Crash recovery 完整 | In Progress | 全 publication kill-point matrix | `docs/evidence/m11-05-key-lifecycle-2026-08-03.md` |
| G11 Doctor 完整/静态模式 | In Progress | local no-mutation hashes + KMS evidence | `docs/evidence/m11-06-kms-dr-2026-08-03.md`；真实 AWS 待补 |
| G12 Backup/restore 完整 | In Progress | Primary 与 Recovery restore evidence | `docs/evidence/m11-06-kms-dr-2026-08-03.md`；真实 AWS 待补 |
| G13 Recovery break-glass 可执行 | In Progress | 独立操作者按 Runbook 完成演练 | Runbook 已落地；独立操作者签署待补 |
| G14 Audit/Metrics/Alerts 完整 | In Progress | contract tests + alert fixtures + observability secret-canary | `docs/evidence/m11-07-production-readiness-2026-08-03.md`；本地 contract/fixtures 已落地，真实关联待补 |
| G15 AWS artifact 供应链通过 | In Progress | SBOM/vuln/checksum/signature evidence | release workflow 已具备 SBOM/checksum/Sigstore；最终 RC 产物审核待补 |
| G16 四方发布评审通过 | Not Started | Security/Backend/SRE/Release sign-off | 签署模板已落地，禁止预签 |

## 9. 风险登记

| ID | 风险 | 严重度 | 缓解 | 状态 |
|---|---|---|---|---|
| R1 | Primary KMS Key 被禁用、删除或 Policy 配坏 | Critical | 独立 Recovery Slot + 定期演练 | Open |
| R2 | 两个 Slots 实际共享同一失效域 | Critical | Key/Policy/Role/account 至少一项隔离并评审 | Open |
| R3 | KMS 短暂故障导致冷启动失败 | High | deadline、retry、jitter、readiness 和告警 | Open |
| R4 | KMS 解包值与当前 Vault 不匹配 | Critical | protected payload + fingerprint + 强制 Vault Key Check | Open |
| R5 | rotate 崩溃造成 descriptor/metadata 失配 | Critical | COW、bridge、generation、kill-point 测试 | Open |
| R6 | KEK 泄露后只做 rewrap | Critical | 事故 Runbook 强制 DEK rotate 和历史备份处置 | Open |
| R7 | AWS SDK 扩大依赖与发布面 | High | Phase 0 实测后冻结 artifact 策略 | Open |
| R8 | 核心被 AWS 类型污染 | High | provider-neutral contract tests 和包依赖检查 | Open |
| R9 | Recovery 权限长期开放 | High | 日常 Runtime 无 Recovery 权限；临时授权和 Audit | Open |
| R10 | Go 运行时、core dump 或调试器读取 DEK | High | 禁 core/pprof、限制 ptrace、清理缓冲区、明确边界 | Open |
| R11 | 单写者被误配多副本造成 KMS 调用风暴 | High | replicas=1、Recreate、先锁后解包、CrashLoop 测试 | Open |

## 10. 真实 AWS 测试矩阵

| 场景 | 状态 | 预期 |
|---|---|---|
| Primary 正常 unwrap | Not Started | 启动成功，Vault Check 通过 |
| Recovery 正常 unwrap | Not Started | 显式恢复成功并产生高严重度 Audit |
| Workload Identity 尚未就绪 | Not Started | deadline 内有界重试 |
| IAM permission denied | Not Started | 快速失败，不无限重试 |
| KMS throttling | Not Started | 尊重 provider 提示并有界退避 |
| Primary Key disabled | Not Started | Primary fail closed，按 Runbook 使用 Recovery |
| Primary Key pending deletion/deleted | Not Started | 稳定错误，Recovery 可恢复 |
| Encryption Context 不匹配 | Not Started | 解包失败并告警 |
| ciphertext/payload 被篡改 | Not Started | 解包或 payload 校验失败 |
| 错误 Vault 的合法 Master Key | Not Started | Vault Key Check失败 |
| 已启动后 KMS 不可用 | Not Started | Gateway 正常请求不受影响 |
| 节点/Pod 冷重启 | Not Started | 身份和 KMS 正常时恢复启动 |
| rotate 每个 kill point | Not Started | 旧代或新代可恢复，无中间失配 |
| Primary/Recovery 备份恢复 | Not Started | 两条路径均恢复相同 Vault |

## 11. Runbook 交付清单

- [x] AWS KMS 模式首次初始化；
- [x] IAM Role 与 Workload Identity 配置；
- [x] Primary/Recovery Key Policy；
- [x] 启动失败稳定错误排障；
- [x] Primary disabled/deleted break-glass；
- [x] Recovery 临时授权、使用和撤销；
- [x] KEK 正常 rewrap；
- [x] KEK/Decrypt 身份泄露后的 DEK rotate；
- [x] 历史备份受旧 KEK 影响的处置；
- [x] backup create/verify/restore；
- [x] `doctor` 完整与静态模式；
- [x] pending/崩溃轮换恢复；
- [x] K8s 单副本、`Recreate` 和身份就绪；
- [x] VM/systemd 部署；
- [x] Metrics、告警和 AWS Audit 关联排障；
- [x] 定期 Recovery Slot 和灾备恢复演练。

## 12. 决策日志

| 日期 | 决策 | 状态 | 影响 |
|---|---|---|---|
| 2026-08-03 | M11 作为一个里程碑，设计文档和 PR 保持拆分 | Accepted | 防止安全链路割裂和巨型 PR |
| 2026-08-03 | Heimdall 核心保持云中立，AWS 只是首个可选生产扩展 | Accepted | File 模式不得产生云依赖 |
| 2026-08-03 | 不实现插件系统，只允许窄、显式、编译期 Adapter | Accepted | 无动态加载、插件市场或第三方 ABI |
| 2026-08-03 | 不承担开发期旧配置和数据迁移 | Accepted | 直接实现最终 schema，无 Legacy/File→KMS 流程 |
| 2026-08-03 | 不实现 Passphrase、助记词和钱包式恢复 | Accepted | 当前 Recovery 使用独立 KMS Slot |
| 2026-08-03 | AWS KMS 生产模式必须有 Primary 和 Recovery | Accepted | 单 Slot 不能 production-ready |
| 2026-08-03 | AWS SDK artifact 方案采用单 module、单签名 `heimdall` artifact，ADR 0010 已 Accepted | Accepted | AWS SDK 引入必须遵守冻结契约、File 零云调用和单 artifact 发布门禁 |
| 2026-08-03 | PR3 拆为决策门 PR3a 与实现 PR3b | Accepted | G3/Accepted ADR 成为 AWS SDK 实现的显式前置 |
| 2026-08-03 | Key Slot 配置统一为 Primary/Recovery + timeout 字段 | Accepted | 两份 PRD 使用同一最终 schema，`config check` 保持纯静态 |
| 2026-08-04 | Recovery 收敛为离线修复 Primary 后冷启动，不提供 Recovery Runtime | Accepted | Runtime/Bootstrap/Admin 永不选择 Recovery；恢复身份不得长期运行 Listener |
| 2026-08-04 | 增加离线 Slot revoke 与只读 Custody 页面 | Implemented locally | revoke 使用 staged intent/exact revisions/compaction；UI 不返回 ARN、ciphertext、fingerprint 或 Slot ID，不执行 KMS 调用，也不声称外部生产准入已完成 |

## 13. 更新规则

每次 M11 相关 PR 或设计决策变化后更新本文件：

1. 更新“最后更新”和整体完成度；
2. 更新对应 PR Slice 状态、PR/Commit 和 Exit evidence；
3. 新风险加入风险登记，关闭风险不得删除历史记录；
4. Release Gate 只有在证据可重复验证时才能标记 Complete；
5. 任何范围新增必须先检查是否属于“当前明确不实现”；
6. 影响安全不变量的变更必须同步更新两个 PRD、ADR 和 Runbook；
7. 阻塞项必须写明解除条件，不能只标记 Blocked。

## 14. 完成定义

M11 只有在以下条件全部成立时完成：

- 8 个 PR slices 全部 Complete；
- 16 个 Release Gates 全部 Complete；
- 风险 R1–R11 已关闭或由发布评审书面接受；
- Primary 和 Recovery 两条路径完成真实 AWS 恢复；
- 独立于实现作者的操作者可以仅依照 Runbook 恢复服务；
- File 模式仍然独立、自包含、无云调用；
- AWS KMS 模式通过生产安全、供应链和运维评审；
- `docs/implementation-status.md`、Operator Guide、Backup/Restore 和 Release Notes 已同步；
- 当前发布候选提交上的完整 CI、Race、Vet、漏洞扫描、Secret canary、kill-point 和真实 AWS evidence 通过。
