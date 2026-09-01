# Halro 文档索引

## 使用与运维 · [`guides/`](guides/)

| 文档 | 内容 |
| --- | --- |
| [User Guide](guides/user-guide.md) | 面向使用者的完整操作说明（英文） |
| [中文使用手册](guides/user-guide.zh-CN.md) | 同一份手册的简体中文版 |
| [Operator Guide](guides/operator-guide.md) | 部署、升级、备份、恢复、加固 |
| [Encrypted backup and restore](guides/backup-restore.md) | 加密备份与恢复流程（含 Docker / Kubernetes） |
| [选择 AWS 接入面](guides/aws-surface-selection.md) | Bedrock Runtime 与 Bedrock Mantle 怎么选，以及两者都不支持什么 |
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
| [Architecture Decision Records](adr/) | 编号决策记录（0001–0016） |
| [多协议 LLM API、Provider 与 Realtime 架构设计](architecture/api-provider-realtime-architecture.zh-CN.md) | 主架构设计 |
| [Distributed state ownership](architecture/distributed-state-ownership.md) | 分布式演进与状态归属 |
| [Threat model](architecture/threat-model.md) | v1 威胁模型 |
| [Experimental EWMA Token Guard](architecture/token-guard-ewma.md) | Token Guard 的 EWMA 实验特性 |
| [Provider 到 Project API 全链路](architecture/provider-to-project-api-call-chain.zh-CN.md) | 从 Credential 到 `/v1/*` 的真实代码路径：配置激活链、提交语义、Registry 装载规则 |

ADR 保留在 `docs/adr/` 顶层：这是业界通用路径，且 `tools/m11/release-evidence/test_verify.py` 按此路径校验证据。

## 产品需求 · [`prd/`](prd/)

历史 PRD 与执行计划，反映当时的需求与取舍，不一定等于当前实现。当前实现以代码和
[实现状态](milestones/implementation-status.md) 为准。已实施完毕、或已被后来的改动取代
而不再作为待办的设计方案，从 [`todo/`](todo/) 归档到这里；归档时按当时代码订正正文，
一份勾选说谎的归档比没有归档更坏。

| 已归档的设计方案 | 落地情况 |
| --- | --- |
| [跨服务商模型选择与能力自动解析方案](prd/provider-model-selection-and-capability-resolution.zh-CN.md) | Phase 0–4 已实施，见 [ADR 0019](adr/0019-invocation-target-capability-resolution.md)、[ADR 0020](adr/0020-dynamic-signed-model-catalog.md)；动态签名目录默认关闭 |
| [模型能力自动识别：保留的安全与任务契约](prd/model-capability-auto-detection.zh-CN.md) | 能力检测已实施；正文只保留检测的安全、预算与任务生命周期契约 |
| [基于服务商与模型的能力选择升级方案](prd/model-aware-capability-selection.zh-CN.md) | 正文已迁入上面的解析方案，仅保留指向它的存根 |
| [模型能力字典演进与四层展示](prd/model-capability-dictionary-evolution.zh-CN.md) | Phase 1 的四层分组已落地，逐项识别结果视图已被检测改版取代；Phase 2–3 未开始，能力字典仍为 v1 |
| [Amazon Bedrock Mantle 接入评估与开发计划](prd/amazon-bedrock-mantle.zh-CN.md) | Phase 0–2 与验收清单已实施，见 [ADR 0007](adr/0007-bedrock-mantle-profiles.md)；Phase 3 的 smoke harness 已交付但从未执行，真实证据归 [provider real matrix](verification/provider-real-matrix.md) 跟踪 |
| [Provider 适配缺口 — 待决与待建](prd/provider-adaptation-gaps.zh-CN.md) | 决定过程的记录。#0 与 §5 十条已修；#4 已关闭；#1/#2 阻塞于凭据、#3b 未排期、能力上限三份真相仍在，逐条见下面的未完成项清单 |
| [Anthropic Message Batches 实施方案](prd/anthropic-batches-plan.zh-CN.md) | 五个切片全部完成，见 [ADR 0021](adr/0021-provider-resource-upstream-twin.md)；真实账号端到端只走到创建成功，结果核对从未完成，§5.1 记的批处理窗口在 2026-08-15 00:10:43 关闭 |
| [适配链条的未完成项](prd/adaptation-open-items.zh-CN.md) | 上面两份归档时留下的未完成项清单：§1 八条已修，§2 「能力上限三份真相」已由下面一份关闭，其余三项等外部凭据或排期，§3 是一处撤销的记录 |
| [分时价位（按日时段折扣）](prd/time-of-day-pricing-review.zh-CN.md) | 采纳方案 B 并已实施，见 [ADR 0023](adr/0023-time-of-day-pricing.md)。价格版本可携带按供应商本地时段的费率表，档位在预留时刻定档并写入价格快照；空规则表 = 全天一档，**不需要迁移，不需要重新初始化数据目录** |
| [适用能力改由服务端统一下发](prd/provider-capability-single-source.zh-CN.md) | 两轮全部实施并合入（PR #182）。六处 switch 收敛成 `internal/domain/provider_table.go`，新增只读端点 `GET /admin/api/v1/provider-profiles`，控制台不再镜像能力矩阵；请求契约改为扁平能力集，`bindings` 由服务端拆分——这是 API 形状变更，见 §10.8 |
| [DeepSeek 适配方案](prd/deepseek-adaptation-plan.zh-CN.md) | 片 1–5 全部完成。片 1–4（缓存用量解码、字段申报按真实子集、thinking 映射、目录订正）2026-08-17 实施；片 5 于 2026-08-20 在真实账号跑通，`thinking` 拼写、`none` 关得掉、`hit + miss = prompt_tokens` 三条推断全部被证实（该文 §11）。仍未测量：思考开着时 `max_tokens` 算不算思维链——现实现按保守读法申报 `max_completion_tokens`。这次不是 GA 发版证据，未走 `tests/provider-matrix` |
| [开发者文档站方案](prd/docs-site-plan.zh-CN.md) | P0 已实施在网站仓库 `Halro-website`（`d6db4de`）：Starlight + 13 页，API 参考由契约生成，link checker 进构建。英文 locale 被实现推翻（只声明不写正文会让 Pagefind 把中文按英文索引）；§5.1 的契约补字段、§5.2 的同步门禁、§8 的 CI 与域名、P1 六页均未做，逐条见该文「归档说明」 |

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

| 评审 | 对象 | 结论 |
| --- | --- | --- |
| [Provider 到 Project API 全链路](review/260811/provider-to-project-api-chain.zh-CN.md) | 管理面配置链与数据面调用链 | 7 条 finding 与 3 项子项全部关闭，无未尽项 |

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

尚未开工或尚未完成的设计提案。全部实施完毕后归档到 [`prd/`](prd/)。

| 文档 | 内容 | 状态 |
| --- | --- | --- |
| [请求失败诊断与错误专用日志](todo/request-failure-diagnostics-plan.zh-CN.md) | 最终失败下钻、错误分类展示、请求级结构化 ERROR 与独立轮转错误文件 | 提案待评审 |
| [告警投递适配方案](todo/alert-delivery-design.md) | 告警契约、平台格式、签名、企业网络与投递结果分类 | 提案待评审 |
| [DLP（脱敏与数据防泄漏）升级方案](todo/dlp-upgrade-plan.zh-CN.md) | 敏感数据标识符、检测配置文件、DLP 策略、Project 绑定与编译快照 | 提案待评审 |
| [Halro Redis-like HA 架构提案](todo/halro-ha-architecture.zh-CN.md) | Standalone 向 Primary/Replica HA 的演进 | 目标 1.1.0，未实现 |

## 草稿 · `drafts/`

已在 `.gitignore` 与 `.dockerignore` 中，不进版本库，也不随镜像分发。
