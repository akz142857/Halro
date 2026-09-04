# Run Governance S2 实现与验证记录

- 日期：2026-09-04
- 分支：`codex/run-governance-prd-review`
- 状态：S2 已实现并完成本地受控验证；尚未推送、部署或执行真实 Provider smoke
- 上游契约：[PRD](../prd/prd-run-governance-and-business-outcomes.zh-CN.md)、[ADR 0025](../adr/0025-run-budget-authority-and-dual-admission.md)、[S0 契约矩阵](run-governance-s0-contracts.zh-CN.md)

## 交付范围

S2 将 Run lifetime cap 接入唯一的 Attempt reservation 入口。带 Run 的调用在一个
Project 临界区内同时检查：

```text
Project committed + reserved + pending + new reservation
Run committed + reserved + pending + new reservation
```

Project 日预算先判断；它和 Run 同时不足时仍返回 `budget_exceeded`。Project 日预算为
零只取消日金额上限，有限 Run cap 继续生效。双预算中的任意加法溢出、Run 归属不匹配、
权威状态不可读或 durable append/apply 失败都会在 Provider I/O 前失败。

Run close 使用同一 Project admission state 建立顺序。已经在临界区中获准的 reservation
先完成 durable append/apply，随后 close 才写入；close 先取得顺序时设置短暂 closing
barrier，后来的 reservation 返回 `run_not_active`。Ledger fsync 仍在金额临界区外，
没有恢复同 Project 请求随 fsync 串行的旧路径。

expiry 在 reservation 当下重新读取时钟。Request 曾经成功接受并不保证较晚的 Attempt
仍可执行；到达 `expires_at` 的新 reservation 被拒绝，已经 durable 的 Attempt 仍按原
恢复与结算规则完成。

## API 与可观测性

Run 公共和 Admin 读模型新增：

- `remaining_micros_usd`：扣除 committed、durable reserved 与 admission pending 后的实时余额；
- `budget_state=available|fully_reserved|depleted`：派生读模型，不写 `RunExhausted` 事件；
- `fully_reserved` 表示 durable balance 尚未耗尽，但尚在 append/apply 窗口的 pending 已占满余额；
- `depleted` 表示 durable `committed + reserved >= cap`。

新的 Attempt 超出 Run cap 返回 HTTP 403 `run_budget_exceeded`。Run 预算没有可用冻结价格
时返回 HTTP 409 `price_unavailable`，不能借 Project 的 unknown-price 例外绕过 lifetime
cap。权威 Run 状态在最终准入点不可用时返回 `run_governance_unavailable`。

指标 `halro_policy_rejections_total{reason="run_budget"}` 与 Dashboard 的 Run 预算拒绝项
独立于 Project 日预算拒绝，便于判断拒绝发生在哪一层。

## 恢复与单一费用权威

S2 没有新增费用日志。reservation 与 settlement 仍只写 Accounting Ledger；Run 只是同一
Attempt 的归属投影。已开始 Attempt 在恢复时继续使用冻结价格与 prepared token bounds
保守结算，其 reserved/committed 转移同时更新 Project 和 Run。Run close、expiry 或跨日
不会退款，也不会把费用复制到另一份账本。

## 验证证据

已通过的受控验证包括：

- 64 个并发 reservation 对同一 Run 只能接受 cap 内 10 个，未发生 over-admission；
- Project 与 Run 同时拒绝时 Project 错误优先；Project cap 为零时 Run cap 仍拒绝；
- settlement 低于 reservation 后恢复 headroom，下一日仍累计同一 Run lifetime spend；
- close/admission barrier 与精确 expiry 边界；
- unknown price、`math.MaxInt64` 溢出和失败准入不改变 durable balance；
- 已开始 Attempt 恢复后仍结算到原 Work Unit/Run；
- Gateway `run_budget_exceeded` 时 fake Provider 调用数为零；
- `go test -race -count=1 ./internal/budget` 通过；
- Run Governance、Gateway attribution、Admin API 与相关前端测试通过；
- 前端 typecheck 和 production build 通过，生成 bundle 的 secret scan 通过。

无 Run 热路径在同一 Apple M4、相同 `200x` 命令下与 S0 提交 `1cddd9c` 各运行三次。
单 Project 中位吞吐如下：

| workers | S0 基线提交 | S2 当前实现 | 变化 |
| ---: | ---: | ---: | ---: |
| 1 | 43.97 lifecycles/s | 40.73 lifecycles/s | -7.4% |
| 8 | 170.5 lifecycles/s | 172.3 lifecycles/s | +1.1% |
| 64 | 1048 lifecycles/s | 1061 lifecycles/s | +1.2% |

1 worker 的短样本受本地 WAL fsync 抖动影响较大；8/64 workers 没有回退。无 Run 请求仍不
读取 Work Unit/Run state、不维护 Run pending，也没有新增 Ledger event。当前证据未显示
应退回设计的实质回退，但这些数字不是生产 SLA。

## 发布边界

S1 与 S2 共同构成可面向用户评估的 Run Governance 单元。当前记录只证明本地实现、
并发性质和受控 fake Provider 路径；在实际部署、备份恢复演练和应用团队验收前，不将其
描述为生产验收完成。S3 Outcome 能力还需单独实现，并在真实业务试点前保持实验性。
