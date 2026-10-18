# V-002 — P2：延迟请求遇到热状态上限被永久判失败，而不是放回队列

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M05（#423）× 既有延迟层；交叉点 X12
- 分类：【问题】，代码证据

## 现象

延迟层（deferred responses）在执行时若被"时间能清除、且发生在写 Ledger 之前"的限流拒绝，会把记录放回队列（`internal/gateway/deferred_response.go:600-606`）。可放回的拒绝码由 `deferredAdmissionRefusal`（`:681`）列出：`concurrency_limit_exceeded`、`token_rate_limit_exceeded`、`rate_limit_exceeded`。

#423 新增的 `accounting_capacity_exhausted` 恰好满足同样的两个条件：

- 它在 `BeginRequestAttributed` 中、`appendApply` 之前返回（`internal/budget/manager.go:893` → `internal/budget/hot_state.go:75` `admitRequest`），Ledger 中没有任何痕迹，放回队列不会让一次提交分散成多个 Ledger 请求；
- 它随已准入工作结算而自行清除（带 `Retry-After`）。

但它不在列表中，因此落入通用失败分支，记录被 `finishDeferred(..., DeferredFailed, "accounting_capacity_exhausted", ...)` 终结。`deferredRetryableFailure`（`:697`）也不包含它，调用方拿不到"重新提交即可"的提示。

## 影响

延迟层存在的意义就是吸收这种短时压力。热状态到达上限时，正排队的延迟请求会被成批永久判失败，而它们从未到达上游、也未计费。

## 建议

把 `accounting_capacity_exhausted` 加入 `deferredAdmissionRefusal`，并补一个"上限清除后放回的请求能完成"的测试（与 `deferred_response_test.go:463` 的并发上限测试同形）。
