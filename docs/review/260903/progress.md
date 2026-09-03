# 260903 整改进度

[260903.md](260903.md) 按日期归档、不再改动;这份是活的对照表 —— 哪些做了、哪些没做,
以及实现过程中对原结论的修正。

评审完成于 2026-09-03,基线 `ff12842`。下面全部为**待处置**,尚未开始整改。

## 阻断 v0.6.0 发布

| # | 事项 | 出处 | 状态 |
|---|---|---|---|
| A1 | 发布管线与 `releasing.md` 二选一并落地 | 260903.md §四 | 待处置。**需要人做决策,不是编辑**:接回门禁,还是把手册改成描述真实管线 |
| A2 | CHANGELOG 补 #253 / #258 / #257 全部,#255 / #259 各半 | 同上 | 待处置 |
| A3 | CHANGELOG 新建 `Operator impact` 段(schema 33→35、重述升级顺序、provider-objects 升级即删、首次启动全量 replay) | 同上 | 待处置。不补则 260901 P16 的缓解措施断掉 |
| A4 | `ledger verify` 等只读语义命令改用 `OpenReadOnly`,或至少在迁移时打一行 stderr | `internal/app/ledger_verify.go:31` | 待处置 |
| — | `CHANGELOG.md:109` 的 checkpoint 版本号 10 → **12**(v0.5.0 是 8) | 主持方核实 | 待处置。一处数字,顺手改 |
| — | `## [Unreleased]` 改名为 `## [0.6.0] - <日期>` | `release.yml:78-81` | 待处置。不改则 `prepare` 阶段直接失败 |

## P1

| # | 事项 | 出处 | 状态 |
|---|---|---|---|
| B1 | 失败列表指向成功那次尝试的 `provider_request_id` | V1 CONFIRMED | 待处置。修法不能照抄日志侧:那边改的是进程内内存,读侧从 ledger 重建,需要 ledger 侧有能区分两种 `provider_error` 的标记 |
| B2 | worker 数可配 + 队列按项目轮转 | V5 CONFIRMED | 待处置 |
| B3 | `retention_days` 下限 1→7 写进发布说明 | 实测 | 待处置 |
| B4 | 首次启动全量 replay 的耗时与内存写进发布说明 | 数据迁移 P1-a | 待处置 |
| B5 | 审计写失败时不再返回 prompt(fail-open → fail-closed) | V3 附带发现 | 待处置。触及 CLAUDE.md 的核心不变量 |
| B6 | `/v1/responses` 端点族 HTTP 测试 + `var _ DeferredResponsesService` 编译期钉子 | 测试盲区 P0-2 | 待处置 |

## P2

| # | 事项 | 状态 |
|---|---|---|
| C1 | 密封孤儿:要么实现清扫,要么把两处注释改成实话 | 待处置。注意 `provider_resources_test.go:64-66` 把「密封孤儿存活」锁成了契约,改行为要连测试一起改 |
| C2 | `read_only` 与返回调用方内容的 GET:要么给该端点加角色门,要么修订 `domain/admin.go` 的角色定义 | 待处置。这是策略决定,两个方向都没有断言 |
| C3 | Admin 冻结路由清单补 5 条并改成双向(260901 P4) | 待处置。本轮已给 gateway router 写了双向守卫,admin router 照抄 |
| C4 | 删掉 `openai-compatibility.md:51-58` 的过期段落与小节标题 | 待处置 |
| C5 | manifest 补 `background`、`Idempotency-Key`、256 KiB 输入上限、Project 开关 | 待处置 |
| C6 | `QueryFailedRequests` 的整表索引与读锁 | 待处置 |
| C7 | 延迟层的全桶扫描 | 待处置 |
| C8 | 未取回记录的 TTL 与 reaper 周期解耦 | 待处置 |
| C9 | requeue 条件补 deployment / provider 并发与熔断 | 待处置。**不能只把错误码加进 switch**:`errDeploymentConcurrency` 发生在 `beginRequestRun` 之后,`RequestAccepted` 已写进 WAL,重排队会让一次提交对应多个 ledger request |
| C10 | `failurecapture.Put` 走 `internal/durable` 的原子重命名序列 | 待处置 |
| C11 | 失败载荷可读期与 `retain` 解耦 | 待处置 |
| C12 | 延迟取回与失败捕获补指标并登记进 metrics-reference | 待处置 |
| C13 | 延迟取回补 en-US 文档,operator-guide 补一节 | 待处置 |
| C14 | 使用手册补四块新界面 | 待处置 |

## 说法与实现不符

| # | 事项 | 状态 |
|---|---|---|
| D1 | `deferred_response.go:351,668` 的「启动时清扫密封孤儿」 | 待处置,与 C1 同源 |
| D2 | 四处注释 + 三处文档点名的 `TestNoEndpointIsServedByATargetThatReasonsUnasked` 不存在 | 待处置。要么建这个测试,要么把七处引用改指向真正生效的守卫(`admin_provider_profiles_golden_test.go` + 写路径 `IsWithheldProfile`) |
| D3 | `internal/compatibility/provider_fields.go:38` 点名 `TestEveryProfileIsRegistered`,真实函数是 `TestEveryProfileRegistersItsOwnFieldRules` | 待处置。名字漂移,一次改名 |
| D4 | `internal/redaction/engine.go:478` 点名 `TestStreamMatchesUnaryRedaction`,最近似的是 `TestRollingStreamMasksMatchSplitAcrossEveryChunkAndFlushesBeforeFinish` | 待处置。需先确认是否同一守卫 |
| D5 | `failures.go:164-167` 的「请求只能以失败结束」 | 待处置,与 B1 同源 |
| D6 | 把「注释点名的测试必须存在」做成 CI 检查 | 待处置。主持方已跑过一次,见 260903.md §六 |

## 从 260901 结转、本轮再次确认的

| 上轮编号 | 事项 | 本轮状态 |
|---|---|---|
| P4 | 新增 Admin 端点不入冻结清单 | **扩大**:1 条 → 5 条,且守卫仍是单向的。四个角色独立指向 |
| P16 | `ledger verify` 静默单向迁移数据目录 | **原样复发且后果加重**:本轮迁移 35 会删除 usage checkpoint 与 rollup。主持方实测复现 |
| P15 | `ledger verify` 在零帧实例上把「没有」说成「不能认证」 | **第四次观测**(v0.3.0、260901、本轮实测两处命令)。同一实例上 `usage verify` 有同样的毛病 |
| P14 | `performance-baseline.md` 对路由解析差 12 倍 | 未处置。直接后果:本轮无法给出任何性能结论,见 runtime-evidence.md「未能取得的证据」 |
| P5 | 汇总端点两段读竞态 | 未复查(本轮 `checkpoint.go` 是新代码,核心逻辑角色确认 checkpoint 与 rollup 同事务同前缀) |
| P10 | `usage rebuild-summary` 用未认证 WAL 重放 | **扩大**:数据迁移角色 P1-c 指出 `InspectReplay` 读封存段时不做 MAC 认证,而 `Log.Replay` 做。仅在 `ledger.seal.enabled` 打开后活跃 |

## 本轮的方法沉淀

- **运行时证据要单独做,而且要跑真升级。** A4 与 B3 都不是读代码能得出的:七个角色里有
  两个独立读到了迁移代码,但「跑一条只读语义的命令会关闭回滚路径」这件事,要把命令
  真的跑一遍才会发现。下一轮固定包含:用上一版二进制建库 → 新二进制 doctor → start →
  再 doctor → 旧二进制回读。
- **对抗验证在两个方向上都有价值。** 本轮一条头号安全发现被证伪(V4),一条三重交叉
  证实的发现被下调并改写性质(V2)。没有这一步,前者会作为漏洞进入发布评估。
- **「注释声称的守卫」需要机械检查。** 本轮出现两次同形问题,其中一次是守卫从未存在。
  见 D6。
