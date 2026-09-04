# Run Governance S1 实现记录

状态：**已实现，内部里程碑，实验性接口**
依据：[Run Governance 与业务结果 PRD](../prd/prd-run-governance-and-business-outcomes.zh-CN.md)、[ADR 0025](../adr/0025-run-budget-authority-and-dual-admission.md)、[ADR 0026](../adr/0026-business-outcome-evidence-and-cohort-reporting.md)、[S0 契约清单](run-governance-s0-contracts.zh-CN.md)

## 1. 本阶段交付边界

S1 建立 `Request / Attempt → Run → Work Unit` 的可信归因，并让管理员读取直接模型成本。它不启用 Run 金额准入，也不实现 Outcome Definition、Outcome 上报、Governance Journal 或 cohort 报表。

没有 `X-Halro-Run-ID` 的旧调用继续走原推理路径。旧 Gateway Key 在迁移后只得到 `inference` scope；Project 的 `run_governance.enabled` 默认关闭。

## 2. 持久格式与恢复

| 项目 | S1 格式 | 行为 |
|---|---:|---|
| metadata schema | 36 | 回填旧 Key 的 `inference` scope，创建治理派生 bucket，Project 开关默认关闭 |
| Accounting Ledger epoch | 5 | 写入 Work Unit/Run 生命周期、请求归属和幂等证据；认证链禁止从 v5 回退到 v4 |
| Usage checkpoint | 13 | Attempt 与 Request 保留 nullable `work_unit_id`、`run_id` |
| Parquet attempt export | 6 | 导出相同的两个 nullable 归因字段；旧版本窄化时显式清空 |

Accounting Ledger 是 Work Unit、Run 及成本归属的唯一权威。bbolt 中的索引和 checkpoint 均不构成第二份权威；恢复后运行状态由 Ledger 重放得到。备份同时保留 metadata 配置、Ledger epoch/minimum reader gate 和归因事件。

## 3. 控制面

应用控制面提供：

- `POST /halro/v1/work-units`
- `GET /halro/v1/work-units/{id}`
- `POST /halro/v1/work-units/{id}/close`
- `POST /halro/v1/runs`
- `GET /halro/v1/runs/{id}`
- `POST /halro/v1/runs/{id}/close`

创建与关闭操作要求 `Idempotency-Key`，使用严格 JSON、16 KiB body 上限、Project 来源限制和 Gateway Key scopes。治理写请求按 Key 120 RPM、Project 1,000 RPM 限流；读请求按 Key 600 RPM、Project 5,000 RPM 限流。跨 Project 读取与附加统一返回不可枚举的 not-found。

推理协议通过 `X-Halro-Run-ID` 传递归属。OpenAI、Anthropic 与异步 Responses 在 Provider I/O 前检查 Project 开关、`run:attach` scope、Project 归属和 Run active/expiry 状态。异步提交把 Run 写入持久资源和幂等指纹，Worker 执行前再次按当前权限与状态校验。

## 4. Admin Console

Project 编辑页可配置 Run Governance 开关、创建上限、默认/最大预算和 TTL。关闭开关前如仍有 active Run，服务端返回 409 和 active count；旧 Admin 客户端省略该字段时保留当前值。

Gateway Key 创建页可选择 `inference`、`work_unit:create`、`run:create`、`run:attach` 和 `governance:read`。旧客户端更新 Key 时省略 scopes 会保留当前值；显式空数组被拒绝，避免误用 legacy inference 回退。

只读的“运行治理”页面支持：

1. 按 Project 与状态筛选 Work Units；
2. 查看 Work Unit 聚合的 Run 数、committed/reserved 成本和未知 Attempt 数；
3. 从 Work Unit 筛选 Runs；
4. 从 Run 下钻到带相同 `run_id` 的 Attempt 明细。

Run 到期状态由服务端读取时钟派生为 `expired`，不写虚假的生命周期事件，也不会继续显示为 active。

## 5. 验证证据

| 验证 | 结果 |
|---|---|
| `GOCACHE=/tmp/halro-go-cache go test -count=1 ./...` | 通过 |
| `GOCACHE=/tmp/halro-go-cache go test -race -count=1 ./internal/budget` | 通过 |
| `GOCACHE=/tmp/halro-go-cache go test -race -count=1 ./internal/app -run 'TestGovernanceRate'` | 通过 |
| `cd web && npm run typecheck && npm test && npm run build` | 通过；532 项测试，生成并检查嵌入式 bundle |

覆盖重点包括迁移中断原子性、v5→v4 同文件及跨 segment 降级拒绝、跨 Project/scope 拒绝、无 Run 旧调用兼容、异步 Responses 提交到结算归因、生命周期幂等、资源硬上限、Usage/Parquet round-trip、Admin 下钻，以及备份恢复后配置和 Ledger 状态重建。

## 6. 下一阶段

S2 才把 Project 日预算与 Run lifetime 预算组成双重原子准入，并返回实时 `budget_state` / `run_budget_exceeded`。S3 才引入独立 Governance Journal、Outcome Definition、Outcome 修订链和 Work Unit cohort 报表。本阶段的 `budget_micros_usd` 是已记录且可读取的上限数据，不会拒绝超出该值的模型请求。
