# R-007 — P2 CONFIRMED：离线路由挂起 clear 与 Audit 不原子

`ClearStoredRouteSuspension` 先以 nil intent 删除 durable suspension（`internal/app/route_suspensions.go:72-101`），再调用 `appendOfflineAudit`。后者仍可能在 Vault、Audit open/reconcile/append/checkpoint 任一阶段失败（`internal/app/keys.go:82-134`）。此时命令返回错误，但路由已重新启用且没有审计记录或待处理 intent。

Store 已支持同事务 delete + audit intent（`internal/store/bolt/store_route_suspensions.go:75-107`），运行手册还声称 API 与 CLI 都在同一事务写 Audit（`docs/observability/operations-runbook.md:143`），因此这是实现与契约不一致。关闭条件是离线 clear 使用 durable intent，并加入 Audit 失败注入测试。
