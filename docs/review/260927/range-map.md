# Range Map 与功能裁决

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

## 高风险交叉点

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
