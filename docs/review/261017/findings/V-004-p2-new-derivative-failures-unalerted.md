# V-004 — P2：新增派生物的失败信号只有指标，没有告警和运行手册

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M01、M02、M04、M06；F10 复核
- 分类：【问题】，代码证据

## 现象

P1–P3 为新的派生文件导出了失败指标（`internal/app/metrics.go`）：

| 指标 | 含义 |
| --- | --- |
| `halro_usage_history_flush_failures_total` | `history.db` 写入失败（见 V-001） |
| `halro_usage_history_row_auth_failures_total` | 读到认证失败的历史行 |
| `halro_usage_history_pending_records` | 内存中待写的历史行 |
| `halro_usage_history_reconcile_mismatches_total` | 每小时对账发现的差异 |
| `halro_ledger_state_snapshot_*`（含失败计数） | 状态快照写入 |

`deploy/observability/prometheus/alert-rules.yml` 中没有任何告警引用 `halro_usage_history_*` 或 `halro_ledger_state_snapshot_*`；`docs/observability/operations-runbook.md` 也没有对应处置步骤。唯一新增的告警是热状态的两条（`HalroLedgerHotStateHigh`/`Rejecting`）。

## 影响

- 快照持续写不出：下次启动退回全量重放（百万请求约 48–53 s），且归档队列持续增长直到热状态上限开始拒绝请求——运维事先看不到原因；
- 历史行认证失败、对账差异：设计（`docs/todo/memory-bounded-redesign.zh-CN.md` §11）说"报错并计数，由运维重建"，但没有任何东西通知运维；
- V-001 的无界增长同样无人知晓。

## 建议

为上述每个计数器的增长、快照时间戳过旧、待写行持续高位各加一条告警及规则测试（沿用 R-011 的写法），并在运行手册写明重建 `history.db` 的步骤。
