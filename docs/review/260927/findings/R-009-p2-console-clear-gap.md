# R-009 — P2 CONFIRMED：Console 没有接入已有的挂起 clear API

后端注册了 `DELETE /admin/api/v1/route-suspensions/{scopeID}`（`internal/app/runtime.go:2280-2284`），列表也返回 `scope_id`/`clearable`。但前端 API 仍声称不存在 clear action（`web/src/api.ts:288-294`），类型省略 `scope_id` 和 `clearable`（`web/src/types.ts:783-800`），`RouteSuspensionsPanel` 只有展示路径。

因此浏览器操作员无法完成已存在的管理动作，只能使用 raw API 或停机 CLI。关闭条件是接入 mutation、权限/step-up/确认、成功后的 query invalidation，并覆盖 clearable=false 与 API 失败状态。
