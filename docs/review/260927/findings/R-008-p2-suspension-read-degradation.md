# R-008 — P2 CONFIRMED：Admin 把挂起存储读失败伪装成 200

`listAdminRouteSuspensions` 对 `ListRouteSuspensions` 的 error 仅记录日志，随后仍基于 live gate 返回 200；没有 live rows 时甚至返回空列表（`internal/app/admin_route_suspensions.go:53-80`）。这把“持久状态不可读”表达成“没有 clearable row / 没有挂起”，违反操作面真实性。

关闭条件：返回 5xx 或显式 unknown/degraded 状态，并加入 persistent store read failure 测试；Console 必须把该状态显示为不可用而不是空态。
