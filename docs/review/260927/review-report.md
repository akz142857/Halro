# Halro v0.8.5 → 当前 main 多角色 Review 报告

> **整改后状态（2026-09-27）**：本报告保留初次 Review 的 No-Go 快照。R-001–R-014 已完成代码、测试和契约整改，并通过仓库完整组成门禁；当前仓库候选转为 **Go（repository scope）**。真实 kind/三节点故障、Linux fault/performance、相邻版本升级、G0–G7、72 小时 soak、真实 RTO 与发布 artifact/provenance 仍为 **BLOCKED**，因此 HA 目标环境与生产启用仍不是 Go。详见 [整改复验报告](remediation-verification.md) 和 [进度表](progress.md)。

> 日期：2026-09-27
>
> 基线：`28ea173b8ccfa2a8043a767c4c4d2782018a4d1c..0c4a270d7aae2b3d4562c35cd60ff14907278897`
>
> 方法：路由/协议、HA/数据、Admin/交付三个独立角色并行审查，主 Reviewer 交叉核验严重 finding。

## 初始最终结论（修复前）：No-Go

当前仓库候选不满足发布或 HA 启用条件。共确认 14 个 finding：

- **1 × P0**：幂等 `in_flight` 未在 Provider I/O 前获得 quorum confirmation，切主后同一 key 可再次调用 Provider（R-001）。
- **3 × P1**：资源接口绕过 `inference` scope（R-002）；planned stepdown 未 drain active requests（R-003）；Admin session/MFA 一次性状态可在 promotion 后复活（R-004）。
- **10 × P2**：Claude expiry、config migrate、route clear/Audit、Admin degraded read、Console clear、CHANGELOG、alert tests、seed index、discovery-only key、limiter denial gap（R-005–R-014）。

R-001 满足计划中的 P0 和 No-Go 定义；R-004 也直接命中“撤销/降权在提升后复活”的 No-Go 条件。即使当前 HEAD 的本地定向测试和 GitHub CI 全绿，也不能覆盖这些缺少 fault-injection oracle 的生命周期窗口。

## 阻断项

| ID | 等级 | 影响 | 最小关闭方向 |
| --- | --- | --- | --- |
| [R-001](findings/R-001-p0-idempotency-confirmation-gap.md) | P0 | F05/F12、Provider at-most-once | Provider 前确认不可重放状态；双 Runtime promotion 回归 |
| [R-002](findings/R-002-p1-resource-scope-bypass.md) | P1 | 资源平面用途授权 | 统一 inference scope gate；403/no-I/O tests |
| [R-003](findings/R-003-p1-stepdown-drain-gap.md) | P1 | planned handoff 外部副作用边界 | promise 前 drain/终止，或 Provider 前 term fence |
| [R-004](findings/R-004-p1-admin-auth-state-revival.md) | P1 | logout/MFA replay、权限复活 | 同步 confirmation 或 promotion 整体失效 |

## 功能裁决

| 状态 | 功能 |
| --- | --- |
| FAIL | F02、F05、F12 |
| PARTIAL | F03、F04、F06、F07、F08、F10、F11、F13 |
| PASS（仓库范围） | F01、F09、F14 |
| PASS（边界标识） | F15 |

完整功能与交叉点矩阵见 [range-map.md](range-map.md)，不变量裁决见 [architecture-and-invariants.md](architecture-and-invariants.md)。

## 已证明的部分

- RouteGate 单节点拒绝、fallback、ambiguous 与 streaming 首 payload 边界通过定向测试。
- metadata journal 的 fsync-before-bbolt、rollback、unknown classification fail-closed、Replica authoritative-write refusal、跨类白名单等基础机制通过。
- replication ordering/ACK/state、partial tail、same-index digest、confirmed-only apply、promotion/seed/backup 基础测试通过。
- Advisor、前端目标组件、typecheck、三语言官方 SDK compatibility、build identity、SBOM/许可证、observability syntax/rule tests 通过。
- 精确 HEAD 的 [GitHub Actions run 36297620187](https://github.com/akz142857/Halro/actions/runs/36297620187) 为 success，并包含 Go 全量/race 与 frontend build/bundle drift。

这些证据的适用范围和首次 sandbox 失败后的同命令重跑记录见 [runtime-evidence.md](runtime-evidence.md)。

## 尚未证明的部分

目标环境结论维持 BLOCKED：真实 Provider、浏览器 E2E、kind 故障、三节点非对称 partition 与物理 fencing、Linux 磁盘故障/性能、相邻版本升级、G0–G7、72 小时 soak、真实 RTO、签名 artifact/provenance 与生产实例同 SHA。未运行 billable Provider smoke。

## 后续方案

1. 先关闭 R-001，再并行关闭 R-003/R-004；这三项决定 HA 和外部副作用正确性。
2. 关闭 R-002 后再宣称 scope 矩阵完整。
3. 处理 R-005–R-014，并同步代码、测试、文档/API/UI/告警契约。
4. 由独立 Reviewer 对 P0/P1 复验；所有代码稳定后按仓库政策只运行一次完整 gate。
5. 仓库候选转 Go 后，再进入目标环境 HA 验收；仓库通过不自动授权生产启用。

严重 finding 的独立证伪过程见 [adversarial-verdicts.md](adversarial-verdicts.md)，整改状态见 [progress.md](progress.md)。
