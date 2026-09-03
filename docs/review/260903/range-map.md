# 260903 评审范围图

基线 `v0.5.0`(2026-09-01)→ `ff12842`(main,2026-09-03)。
231 文件 / +25697 / −903(含 `internal/webui/dist` 生成物与文档)。

本文件是各评审角色的共同起点:说明这一轮改了什么、改动集中在哪、以及哪些
位置的风险由改动本身决定而非由历史决定。它不含结论。

## 六个提交

| 提交 | 主题 | 主要落点 |
|---|---|---|
| `38b2bfd` #253 | 服务 Kimi,并让真实账号推翻其文档 | `internal/compatibility/kimi.go`、`internal/domain/provider_table.go`、`internal/modelcatalog` |
| `b3a65a8` #254 | 请求失败诊断:从一个计数变成一个解释 | `internal/usage/failures.go`、`internal/gateway/failure.go`、`web/src/pages/UsageFailuresPanel.tsx` |
| `8c7639d` #255 | 给每一层记录请求的地方划界 | `internal/failurecapture`、`internal/gateway/capture.go`、`internal/usage/retention.go`、`internal/logging/errorfan.go` |
| `fa82282` #257 | 补上留存评审留下的后续项 | 同上各处 |
| `aab4f17` #258 | 在一条连接上提交,在另一条上取回 | `internal/gateway/deferred_response.go`、`internal/gatewayapi/deferred_response.go`、ADR 0024 |
| `ff12842` #259 | 拒绝该路由不服务的模型,而不是拨过去 | `internal/domain/provider_table.go`、`internal/app/admin_deployments.go`、`internal/app/health.go` |

## 改动量,按区域

| 区域 | 文件 | + | − |
|---|---|---|---|
| `internal/gateway` | 13 | 3753 | 137 |
| `internal/app` | 39 | 3475 | 88 |
| `internal/usage` | 14 | 2512 | 171 |
| `web/src` | 26 | 2315 | 43 |
| `internal/ledger` | 10 | 2043 | 47 |
| `internal/compatibility` | 12 | 1873 | 42 |
| `internal/failurecapture` | 2 | 780 | 0 |
| `internal/domain` | 6 | 476 | 63 |
| `internal/config` | 4 | 468 | 4 |
| `internal/provider` | 9 | 452 | 28 |
| `internal/logging` | 3 | 418 | 11 |
| `internal/store` | 7 | 316 | 30 |
| `internal/modelcatalog` | 2 | 169 | 1 |
| `internal/gatewayapi` | 3 | 143 | 1 |
| `internal/limiter` | 2 | 115 | 7 |

余下为文档(`docs/prd` +1972、`docs/todo` +1743、`docs/compatibility` +623)与生成物
(`internal/webui/dist`)。

## 新增的源文件(非测试)

```
internal/app/admin_usage_settings.go      internal/gateway/capture.go
internal/app/failure_capture.go           internal/gateway/deferred_response.go
internal/app/ledger_seal.go               internal/gateway/failure.go
internal/app/ledger_seal_command.go       internal/gatewayapi/deferred_response.go
internal/compatibility/kimi.go            internal/ledger/seal.go
internal/compatibility/reasoning_reachability.go   internal/ledger/segment.go
internal/domain/usage_settings.go         internal/logging/errorfan.go
internal/durable/directory.go             internal/usage/checkpoint.go
internal/failurecapture/failurecapture.go internal/usage/failures.go
                                          internal/usage/retention.go
web/src/failure.ts                        web/src/pages/UsageFailuresPanel.tsx
web/src/pages/FailureDetailDrawer.tsx     web/src/pages/UsageWindowForm.tsx
```

## 四个由改动决定的高危区

**一、两个新的静态留存面。** 在此之前,调用方写的内容不落盘。现在有两处:
`gateway.failure_capture`(失败调用的请求与上游回答,默认关)和延迟取回(成功回答,
按项目开)。两者都以主密钥密封、绑定 request+project、限量限时。风险面是密封、
绑定、清扫、读取授权与审计,以及「关掉之后已收集的东西怎么办」。

**二、新的北向端点族与新的记账时序。** 延迟取回引入 `POST /v1/responses`
(`background: true`)、`GET /v1/responses/{id}`、`.../cancel`、`DELETE`。提交占 RPM
不占并发、队列有上限、24h TTL、单次执行受 `route_total_timeout` 约束、取消
`in_progress` 是尽力而为且按 ADR 0011 保守结算。风险面是预留与结算的时序、
重启时在途请求、幂等、以及跨连接取回的授权边界。

**三、记账权威与其派生物同时被改动。** `internal/ledger` +2043(新增 `seal.go`、
`segment.go`)、`internal/usage` +2512(新增 `checkpoint.go`、`failures.go`、
`retention.go`)。WAL 是权威,usage 与 bbolt 聚合是可重建派生物;这一轮两侧同时动,
是派生物悄悄变成第二真相的典型时机。

**四、新增 Admin 端点与配置项。** `/admin/api/v1/usage/failures`、
`.../failures/{requestID}/payload`(唯一写审计的 admin GET)、
`/admin/api/v1/usage/settings`,以及 `gateway.failure_capture`、`logging.error_file`、
留存窗口配置。260901 的 P4 记过同类问题:上一轮新增的 `usage/summary` 端点族不在
冻结路由清单、不在任何 manifest,本轮又增三个。

## 上一轮评审仍开着的条目

`docs/review/260901/progress.md` 的「未处置」表有 P2b–P17 共 15 条,当时判定
均不阻断 v0.5.0。其中与本轮范围直接相交的:

- **P4** 新增 Admin 端点不入冻结清单/manifest —— 本轮又增三个端点。
- **P5** 汇总端点两段读竞态(`watermark_sequence` 宣称覆盖到未计入的 sequence)——
  本轮 `internal/usage/checkpoint.go` 是新代码。
- **P10** `usage rebuild-summary` 用未认证 WAL 重放并 durably 写两个派生物 ——
  本轮 ledger 新增 `seal.go`。
- **P14** `performance-baseline.md` 对路由解析差 12 倍,已不能作回归判据 ——
  性能类结论若不先处理它,会把旧漂移误报成本轮新回归。
- **P16** `ledger verify` 静默单向迁移数据目录,当时的缓解是「在发布说明写死操作
  顺序」—— v0.6.0 的发布说明需要再写一次,否则缓解措施断掉。

## 不在本轮范围

- `internal/webui/dist`:生成物,按源码评审,不按产物评审。
- `docs/prd`、`docs/todo`:设计过程记录,只在与实现不符时作为证据引用。
- 真实服务商冒烟:计费,且需外部凭据,不在评审内触发。
