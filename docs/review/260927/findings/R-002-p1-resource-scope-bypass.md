# R-002 — P1 CONFIRMED：资源型接口绕过 `inference` scope

- 基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897`
- 影响：F04、资源型 API、INV-07
- 裁决：CONFIRMED；原则上阻断

普通推理在 `resolveRequest` 中明确要求 `GatewayScopeInference`（`internal/gateway/service.go:326-338`）。Files、Batches、Async、Deferred 等资源路径共用 `resourcePrincipal`，该函数只做 Authenticate、source/policy/store 和 key limiter，没有 scope 检查（`internal/gateway/inference_resources_store.go:265-291`）。HTTP `GuardOpenAI` 也只做 key 存在/认证前置拒绝，不承担 scope 授权（`internal/gatewayapi/handler.go:1023-1079`）。

例如 `CreateFile` 经 `resourcePrincipal` 后可解析 route 并执行 adapter Provider I/O（`internal/gateway/inference_resources_store.go:303-418`）。因此一个显式只授予 `governance:read` 或 `run:create`、没有 `inference` 的有效 Gateway Key，仍可能访问资源平面并触发外部调用。Project allowlist、source policy、limiter 和预算仍生效，所以这是同一 Project 内的用途授权绕过，不是跨租户泄露；按本轮定义评为 P1。

## 关闭条件

在资源平面的统一认证入口强制 `inference` scope，并为每个资源族补充显式非 inference key 的 403 测试；同时保留 nil scopes 的既有兼容语义测试，避免误改默认 scope 契约。
