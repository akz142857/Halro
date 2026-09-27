# R-003 — P1 CONFIRMED：planned stepdown 在签发 promise 前未 drain

- 基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897`
- 影响：F12、INV-02
- 裁决：CONFIRMED；阻断 planned handoff

HA 设计要求先撤 readiness、把在飞请求 drain 到最后一帧 confirmed，再 freeze 并签发 higher-term promise（`docs/todo/halro-ha-architecture.zh-CN.md:690-702`）。实现收到 planned proposal 后只调用 `FreezeForStepdown`、撤 `startupReady`、持久化 promise/demotion 并触发退出（`internal/app/replication_runtime.go:681-712`）。Freeze 只阻止后续 ordering append/ACK（`internal/replication/primary.go:283-301`），没有等待 active requests。

已经通过 middleware 且 `AttemptStarted` 已 confirmed、但尚未进入 Provider 的 handler 不会被 readiness 变化拦截。`http.Server.Shutdown` 等待现有 handler，不主动取消其 context；目标节点完成 promotion 后，旧 handler 仍可发起 Provider 副作用，之后的 settlement 才因 frozen coordinator 失败。当前集成测试只在没有 active request 时执行 stepdown（`internal/app/replication_runtime_integration_test.go:242-264`）。

## 关闭条件

要么 promise 前真正 drain/终止 active requests，要么在每次 Provider I/O 前做不可绕过的 current-term/role fence。回归测试需在 adapter 前暂停请求，完成 stepdown/promotion 后再释放；旧 adapter 调用数必须为 0，或 stepdown 必须在请求安全结束后才返回 promise。
