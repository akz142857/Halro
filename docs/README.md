# Heimdall 文档索引

## 使用与运维 · [`guides/`](guides/)

| 文档 | 内容 |
| --- | --- |
| [中文使用手册](guides/user-guide.zh-CN.md) | 面向使用者的完整操作说明 |
| [Operator Guide](guides/operator-guide.md) | 部署、升级、备份、恢复、加固 |
| [Encrypted backup and restore](guides/backup-restore.md) | 加密备份与恢复流程（含 Docker / Kubernetes） |
| [Release Process](guides/releasing.md) | 发版流程与证据门禁 |

## 契约 · [`contracts/`](contracts/)

对外承诺的接口与数据形状。改动这些文件等于改动对外契约。

| 文档 | 内容 |
| --- | --- |
| [Gateway correctness contract](contracts/gateway-correctness.md) | 网关正确性保证 |
| [OpenAI compatibility contract](contracts/openai-compatibility.md) | OpenAI 兼容层承诺 |
| [Provider capability contract](contracts/provider-capabilities.md) | 服务商能力矩阵语义 |
| [Gateway idempotency contract](contracts/idempotency-contract.md) | 幂等语义 |
| [Alert webhook payloads](contracts/webhook-payloads.md) | 告警 Webhook payload 结构与平台适配立场 |
| [Metrics reference](contracts/metrics-reference.md) | 指标清单（由 `internal/app/metrics_contract_test.go` 断言） |
| [Audit integrity](contracts/audit-integrity.md) | 审计链完整性保证 |
| [Usage storage and retention](contracts/usage-storage.md) | 用量存储与保留策略 |

## 架构 · [`architecture/`](architecture/) 与 [`adr/`](adr/)

| 文档 | 内容 |
| --- | --- |
| [Architecture Decision Records](adr/) | 编号决策记录（0001–0014） |
| [多协议 LLM API、Provider 与 Realtime 架构设计](architecture/api-provider-realtime-architecture.zh-CN.md) | 主架构设计 |
| [Distributed state ownership](architecture/distributed-state-ownership.md) | 分布式演进与状态归属 |
| [Threat model](architecture/threat-model.md) | v1 威胁模型 |
| [Experimental EWMA Token Guard](architecture/token-guard-ewma.md) | Token Guard 的 EWMA 实验特性 |

ADR 保留在 `docs/adr/` 顶层：这是业界通用路径，且 `tools/m11/release-evidence/test_verify.py` 按此路径校验证据。

## 产品需求 · [`prd/`](prd/)

历史 PRD 与执行计划，反映当时的需求与取舍，不一定等于当前实现。当前实现以代码和
[实现状态](milestones/implementation-status.md) 为准。

## 里程碑与证据 · [`milestones/`](milestones/)

| 文档 | 内容 |
| --- | --- |
| [Implementation status](milestones/implementation-status.md) | 各能力的实现状态 |
| [v1.0.0 release notes](milestones/release-notes-v1.0.0.md) | 由 `.github/workflows/release.yml` 引用 |
| [M11 Master Key 托管与 AWS KMS](milestones/milestone-m11-master-key-custody-aws-kms.md) | M11 里程碑 |
| [`milestones/evidence/`](milestones/evidence/) | 各里程碑的验收证据 |

## 验证与基线 · [`verification/`](verification/)

性能基线、浸泡测试、崩溃恢复矩阵、真实服务商矩阵、安全评审、依赖与许可证评审。

## 评审 · [`review/`](review/)

周期性多角色代码评审报告，按日期命名。[`review/README.md`](review/README.md) 定义评审框架：
有哪些角色、一次评审该选哪些、以及"发现型评审 → 对抗证伪"两段式流程。

与 `verification/` 的分工：那里是发版证据门禁，回答"能不能发"；这里是主动找问题的评审
记录，回答"哪里还不够好"。评审结论会随修复推进而过时，读的时候以文中 `文件:行号`
索引回代码为准。

## 可观测性 · [`observability/`](observability/)

指标契约、告警规则、容量模型、运维 runbook、准入清单。
`deploy/observability/prometheus/alert-rules.yml` 的 `runbook_url` 指向本目录，改名前先看那里。

## 运行手册 · [`runbooks/`](runbooks/)

**这是一个 Go package，不是纯文档目录。** `docs/runbooks/embed.go` 用 `//go:embed` 把这些
Markdown 编进二进制，由 `internal/app/admin_master_key_runbook.go` 提供给控制台。
`go:embed` 只能引用同目录或子目录的文件，因此这些文件不能移动或改名。

## 兼容性 · [`compatibility/`](compatibility/)

`endpoint-manifests.json` 是 `internal/compatibility/manifest_test.go` 的 golden 文件，
路径固定。

## 参考分析 · [`reference-analysis/`](reference-analysis/)

对同类产品（LiteLLM、Makai）的深度分析，用于设计取舍参考。

## 待办 · [`todo/`](todo/)

尚未开工的设计提案。开工后移出本目录。

## 草稿 · `drafts/`

已在 `.gitignore` 与 `.dockerignore` 中，不进版本库，也不随镜像分发。
