# Range Map 与功能裁决

## 当前功能清单与裁决（2026-09-27）

本轮审阅的 `main` 基线为 `c8b97b1b62ab9fd654a72e00ec19218ad734b973`，相对
`v0.8.5` 的 `28ea173b8ccfa2a8043a767c4c4d2782018a4d1c` 共 53 个提交。
下表按运行时能力、操作能力和非功能性工作列出本轮增量；“仓库通过”指
代码、确定性测试和仓库门禁，不代表目标环境或生产验收通过。
R-001–R-014 的关闭证据见 [整改进度](progress.md)与
[复验报告](remediation-verification.md)。

| 功能 | 当前功能范围 | 当前裁决 | 尚需的独立证据或边界 |
| --- | --- | --- | --- |
| F01 RouteGate | 按拒绝原因和真实作用域准入、挂起与安全 fallback | 仓库通过 | 真实 Provider 拒绝矩阵与目标环境路由验收 |
| F02 suspension | 长时挂起持久化、Admin/CLI/Console 查看和审计清除 | 仓库通过 | 真实运行中的并发恢复与操作旅程 |
| F03 Models discovery | Project 内 alias 列表/详情及 `discovery` scope | 仓库通过；API 仍标实验性 | 官方 SDK 和真实客户端的兼容验收；枚举不保证可用性或能力 |
| F04 fixed key limiter | 认证后读取的 Key/Project 固定速率上限 | 仓库通过 | 目标负载下的基数与公平性 |
| F05 Chat/Embeddings idempotency | `Idempotency-Key` 提供上游至多一次执行，无响应重放 | 仓库通过 | 与真实提升、分区及 Provider 副作用联合验收 |
| F06 config v2/migrate | 配置版本 2、退休键表、显式 dry-run/写入迁移 | 仓库通过；升级需操作员执行 | 相邻发布配置及真实文件系统上的升级/回退演练 |
| F07 subscription offerings | Kimi Code 和显式启用的 Claude 订阅产品、credential 类型隔离 | 仓库通过；真实证据仅覆盖已记录账户/模型/日期 | 未测计划、模型和上游契约的新鲜度；不得外推到所有账户 |
| F08 Admin explainability | alias 分组、上游拒绝/修复、Usage 估算 token、挂起清除 | 组件与 API 仓库通过 | 真实浏览器、窄屏、角色和缓存旅程 |
| F09 Advisor | CLI/Admin/Console 的配置预算与路由 finding | 仓库通过 | 真实运行数据下的操作有效性 |
| F10 observability/audit | 首字节时间、拒绝/预算/审计指标告警、SIGKILL 门禁 | 仓库通过 | 目标环境告警阈值、日志投递与长稳验收 |
| F11 metadata journal | 权威元数据 journal 与可重建 bbolt 投影 | 仓库通过 | Linux 故障与性能、真实备份恢复 |
| F12 HA | Primary/Replica、手工提升/交接、播种、对象复制、备份、K8s 资产 | 仓库通过；目标环境/生产 BLOCKED | kind、三节点分区与 fencing、Pod/SSE drain、Linux 故障/性能、相邻版本、G0–G7、72 小时 soak、RTO |
| F13 delivery gates | 配置引用、构建身份、SBOM、release assessment 和 bundle 门禁 | 仓库通过 | 实际签名制品、provenance 与生产运行 SHA 的闭环 |
| F14 dependency/SDK refresh | Go/Node/Python SDK 与 Admin 依赖维护 | 仓库通过；非新产品功能 | 在线漏洞情报和真实上游兼容性仍按发布流程核验 |
| F15 design-only | Agent 自描述方案；自动 failover 不在当前交付范围 | 正确标为未交付 | 不列入已上线功能或 HA 自动切换承诺 |

**当前总裁决：仓库范围 Go；HA 目标环境与生产启用仍 BLOCKED。**
本轮没有运行新的真实 Provider 计费测试。以下矩阵保留整改前的原始裁决，
用于追溯问题的发现与关闭，不能用作当前状态。

## 原始裁决快照（整改前）

基线范围：`28ea173b8ccfa2a8043a767c4c4d2782018a4d1c..0c4a270d7aae2b3d4562c35cd60ff14907278897`。

| 功能 | 主要实现/证据 | 本轮裁决 | Finding / 边界 |
| --- | --- | --- | --- |
| F01 RouteGate | `internal/routegate`、Gateway failure paths | PASS（仓库/单节点） | streaming 首 payload 前后边界定向测试通过 |
| F02 suspension | routegate persistence、Admin/CLI/UI | **FAIL** | R-007、R-008、R-009 |
| F03 Models discovery | `gateway/models.go`、scope/domain/Admin | PARTIAL | R-013；编码与隔离主路径通过 |
| F04 fixed key limiter | `internal/keylimit`、key rate paths | PARTIAL | R-014 |
| F05 Chat/Embeddings idempotency | idempotency lifecycle、provider resources | **FAIL** | R-001、R-002 |
| F06 config v2/migrate | retirement table、CLI writer、release fixtures | PARTIAL | R-006 |
| F07 subscription offerings | Kimi/Claude profiles、evidence docs | PARTIAL | R-005；实时外部契约 BLOCKED |
| F08 Admin explainability | Routes/Providers/Usage pages | PARTIAL | focused tests PASS；真实浏览器 BLOCKED；R-009 |
| F09 Advisor | `internal/advisor`、doctor/Admin/UI | PASS（仓库） | 真实运行数据验收未做 |
| F10 observability/audit | metrics、alerts、SIGKILL、runbooks | PARTIAL | R-007、R-011；外部阈值/长稳未验 |
| F11 metadata journal | journal、Bolt recorder/classification | PARTIAL | 基础机制 PASS；R-001、R-004 confirmation 分类不完整 |
| F12 HA | replication/runtime/seed/backup/K8s | **FAIL / No-Go** | R-001、R-003、R-004、R-012；目标环境仍 BLOCKED |
| F13 delivery/supply chain | gates、CI/release、SBOM | PARTIAL | 当前 HEAD CI PASS；R-010；实际 artifact/provenance 未验 |
| F14 dependency/SDK refresh | compatibility suites、license review | PASS（仓库） | 在线 vulnerability/provider acceptance 未执行 |
| F15 design-only | Agent self-description plan | PASS | 正确标为设计稿，未冒充已交付 |

## 原始高风险交叉点（整改前）

| ID | 裁决 |
| --- | --- |
| X01 RouteGate × streaming × idempotency | PASS（单节点）；HA 组合由 X03 阻断 |
| X02 suspension × journal × HA | PARTIAL；分类/online intent 正确，offline clear 原子性失败 |
| X03 idempotency × promotion | **FAIL（R-001）** |
| X04 revocation × promotion | **FAIL（R-004）** |
| X05 config migrate × HA identity | PASS（代码级）/ BLOCKED（真实故障迁移） |
| X06 subscription × enumeration × capability | PASS（结构级）/ BLOCKED（实时 Provider） |
| X07 Advisor × metrics × operator action | PARTIAL（R-011） |
| X08 backup/restore × epoch/incarnation | PASS（仓库）/ BLOCKED（真实 PVC/kind） |
| X09 build identity × release evidence | PARTIAL/BLOCKED；CI gate 通过，实际发布制品链未验 |
