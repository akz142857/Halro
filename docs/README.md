# Halro 文档索引

## 使用与运维 · [`guides/`](guides/)

| 文档 | 内容 |
| --- | --- |
| [User Guide](guides/user-guide.md) | 面向使用者的完整操作说明（英文） |
| [中文使用手册](guides/user-guide.zh-CN.md) | 同一份手册的简体中文版 |
| [Operator Guide](guides/operator-guide.md) | 部署、升级、备份、恢复、加固 |
| [Encrypted backup and restore](guides/backup-restore.md) | 加密备份与恢复流程（含 Docker / Kubernetes） |
| [选择 AWS 接入面](guides/aws-surface-selection.md) | Bedrock Runtime 与 Bedrock Mantle 怎么选，以及两者都不支持什么 |
| [异步提交与延迟取回](guides/deferred-responses.zh-CN.md) | `background: true` 的提交、取回、取消、删除，以及重启时正在执行的请求为什么会 failed |
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
| [BigModel / Z.AI 适配方案](prd/bigmodel-adaptation-plan.zh-CN.md) | 中国大陆 Chat/Stream/Embeddings 与海外 Z.AI Chat/Stream 首期代码已实施：地域 profile/凭据隔离、严格方言渲染、模型目录、目标级推理约束、控制台与契约快照均已落地。当前标记为实验性；真实账号验证未运行，Anthropic 兼容 profile 仍按方案保持未注册 |
| [Provider Offering 与订阅接入统一方案](prd/provider-offering-subscription-access-plan.zh-CN.md) | 阶段 1 + 国内阶段 0/2 已实施：Offering 与产品地域声明在 Access Surface 层（凭据的解析键），Profile 派生；控制台凭据与连接表单改为元数据驱动，`type === "bedrock"` 分支清除；GLM Coding Plan 作为首个 subscription Offering 落地（独立 surface/scheme/profile 与 `/api/coding/paas/v4`）。真实账号实测推翻了"上游 /models 即权威"这条前提——该 endpoint 列 10 个模型只服务 2 个，其余静默替换，故枚举保持动态、目录只承担能力证据，并新增 `model_substituted` 护栏（该文 §5.1、§14）。同时修掉一处存量缺陷：凭据创建不发 access_surface，海外 BigModel 凭据一律落在 CN surface 上。海外 Coding Plan、Kimi Code、MiniMax Token Plan 与 OAuth 仍缺真实账号证据，未实施 |
| [开发者文档站方案](prd/docs-site-plan.zh-CN.md) | P0 已实施在网站仓库 `Halro-website`（`d6db4de`）：Starlight + 13 页，API 参考由契约生成，link checker 进构建。英文 locale 被实现推翻（只声明不写正文会让 Pagefind 把中文按英文索引）；§5.1 的契约补字段、§5.2 的同步门禁、§8 的 CI 与域名、P1 六页均未做，逐条见该文「归档说明」 |
| [异步提交与延迟取回（Deferred Response）](prd/deferred-response-plan.zh-CN.md) | S0–S9 全部落地（2026-09-03），见 [ADR 0024](adr/0024-deferred-response-tier.md)。`POST /v1/responses` 的 `background` 由该 ADR 解禁，对象目录改为按资源与 Project 密封；提案正文与实现不一致之处在该文「实施记录」逐条订正 |
| [请求失败诊断与错误专用日志](prd/request-failure-diagnostics-plan.zh-CN.md) | S0–S5 全部已实施（2026-09-02）。`internal/failurecapture`（默认关闭、加密、限时限量）、独立轮转的 ERROR 文件（`logging.error_file`）、Ledger 增加受约束的 `provider_code`/`provider_request_id`/`failure_phase` 三字段。正文两处偏差已就地订正：非 success 终态是六种，`accounting_error` 也写 ERROR |
| [数据保留与压缩](prd/data-retention-plan.zh-CN.md) | 四个阶段全部收口。S0–S5 裁剪与口径、S7 WAL 封存（`internal/ledger/seal.go`，默认关闭）、S8 窗口交设置中心（`PUT /admin/api/v1/settings/usage`，缩短需 `acknowledge_trim`）、S9 增量 checkpoint 均已落地；S6 checkpoint 压缩已量并否决。归档时订正了过期的状态行 |
| [TLS 部署形态与证书热重载](prd/tls-acme-plan.zh-CN.md) | §6 全部落地：多证书配置、SNI 选择与热重载（`internal/app` 的 tlsreload）。§7 的内置 ACME **维持不做**——那是决定，不是遗漏 |
| [控制台无法创建 `openai.responses.v1` 连接](prd/console-profile-anchor-plan.zh-CN.md) | 纯前端修复已落地：`ProvidersPage.tsx` 里三处把「能力实现」选择器锁死在 `type === "bedrock"` 的条件已移除，`connectionChoices` 对所有服务商类型返回选项，Responses profile 可作为 anchor 选择。服务端与持久化未变。归档时订正了过期的状态行 |

## 里程碑与证据 · [`milestones/`](milestones/)

| 文档 | 内容 |
| --- | --- |
| [Implementation status](milestones/implementation-status.md) | 各能力的实现状态 |
| [v1.0.0 release notes](milestones/release-notes-v1.0.0.md) | 由 `.github/workflows/release.yml` 引用 |
| [M11 Master Key 托管与 AWS KMS](milestones/milestone-m11-master-key-custody-aws-kms.md) | M11 里程碑 |
| [`milestones/evidence/`](milestones/evidence/) | 各里程碑的验收证据 |

## 验证与基线 · [`verification/`](verification/)

性能基线、浸泡测试、崩溃恢复矩阵、真实服务商矩阵、安全评审、依赖与许可证评审。

| 文档 | 内容 |
| --- | --- |
| [生产验证执行方案](verification/production-validation-plan.zh-CN.md) | 从候选冻结开始，依次完成真实 Provider、安全边界、告警与恢复、容量与 24 小时浸泡、正式发布和四方签署 |
| [生产验证执行记录 · 2026-09-18](verification/production-validation-run-260918.zh-CN.md) | 在候选 `f09ed2d` 上执行该方案中不需要外部授权的部分（G0 条件通过、G1/G3/G4 的本机 E3 子集）。首轮 22 条发现修掉 10 条，对修复本身的第二轮评审再出 13 条（含一条把 fail-closed 放行的自造回归，已回退）；G2/G5/G6/G7 因缺真实账户、生产形态环境与四方签署而 `BLOCKED`，结论维持 `NO-GO / PRODUCTION UNVERIFIED` |
| [上游拒绝形态取证矩阵](verification/route-eligibility-refusal-matrix.zh-CN.md) | Route eligibility（#318/#319）定参数所依赖的上游取证记录：额度耗尽、普通限流、凭证失效、订阅未开通四种拒绝各自的状态码与 `Retry-After`，以及**耗尽与限流是否同码**——那一格决定 `FailureReason` 要不要进路由路径。九个入口目前只有 MiniMax 一行有内容，且其中额度那格仍是第三方报告等级 |

## 评审 · [`review/`](review/)

周期性多角色代码评审报告，按日期命名。[`review/README.md`](review/README.md) 定义评审框架：
有哪些角色、一次评审该选哪些、以及"发现型评审 → 对抗证伪"两段式流程。

与 `verification/` 的分工：那里是发版证据门禁，回答"能不能发"；这里是主动找问题的评审
记录，回答"哪里还不够好"。评审结论会随修复推进而过时，读的时候以文中 `文件:行号`
索引回代码为准。

跨产品、工程、数据、安全、运维和发布的里程碑评审，使用
[《Halro 里程碑专业评审计划》](review/milestone-professional-review-plan.zh-CN.md) 作为常设程序
与实例化模板：范围冻结、RACI、分层裁剪、阶段门禁、严重度、评分与 GO/NO-GO 决策规则。它的证据
分级沿用 `verification/production-validation-plan.zh-CN.md`，角色定义沿用 `review/README.md`。

| 评审 | 对象 | 结论 |
| --- | --- | --- |
| [里程碑专业评审计划](review/milestone-professional-review-plan.zh-CN.md) | 常设程序与模板 | 待具体里程碑实例化后执行 |
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
| [告警投递适配方案](todo/alert-delivery-design.md) | 告警契约、平台格式、签名、企业网络与投递结果分类 | 提案待评审；`internal/alert` 今天只有 dispatcher |
| [DLP（脱敏与数据防泄漏）升级方案](todo/dlp-upgrade-plan.zh-CN.md) | 敏感数据标识符、检测配置文件、DLP 策略、Project 绑定与编译快照 | 提案待评审；四层拆分尚未进 `internal/domain` |
| [路由准入设计](todo/route-eligibility-design.zh-CN.md) | 把熔断器、探针健康与额度/订阅挂起合成一个准入门；作用域由失败自己声明，`FailureReason` 成为路由输入 | 提案待评审；`internal/routegate` 不存在，额度用尽今天不触发回退 |
| [Halro HA 架构设计](todo/halro-ha-architecture.zh-CN.md) | 三节点同步复制 + 人工提升；账务只在 Provider I/O 前的两个事件与吊销类写上等待，RPO=0；自动故障切换是 §19 的未决问题 | 提案待评审，未实现 |

## 草稿 · `drafts/`

已在 `.gitignore` 与 `.dockerignore` 中，不进版本库，也不随镜像分发。
