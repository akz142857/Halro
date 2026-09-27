# R-013 — P2 CONFIRMED：管理面允许创建永久不可用的 discovery-only key

`/v1/models` 同时要求 `inference` 与 `discovery`（`internal/gateway/models.go:66-100`），但 Gateway Key 的 domain/Admin/UI validation 只校验 scope 枚举、重复和非空，没有约束 `discovery => inference`（`internal/domain/models.go:1483-1508`、`internal/app/admin_projects.go:260-292,366-380`）。

因此操作员可以成功持久化仅含 discovery scope 的 key，而它对唯一 discovery API 永远得到 403。运行时保持 fail closed，没有越权；问题是管理面接受一个无法兑现的配置。

关闭条件：在统一 domain validation 层强制 scope 蕴含关系，并为 Admin API、CLI/UI 和历史数据加载补充测试与迁移/诊断策略。
