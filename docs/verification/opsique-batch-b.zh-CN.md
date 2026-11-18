# Opsique 批次 B 联调与容量验收

验收日期为 2026-11-19。Halro 基于 `main` 的 `24acd586` 加本次补丁。Platform 联调基于 `master` 的 `e95a935`，运行其现有 `TestBotRunOnGateway`。Halro 在同一次请求内处理了上游 429，并向 Temporal 返回 200。

## 主备切换

Platform Temporal worker 调用真实 Halro HTTP 入口。测试通过 Halro Admin API 创建独立的 Mantle Provider、Deployment 和有序 Route，并尽可能沿用实际适配器。两个上游通过测试 transport 接到回环 HTTP fixture。主路由在 `/v1/chat/completions` 返回 429 和 `Retry-After: 300`。备路由采用 `bedrock.mantle.openai.chat.v1`，校验 `/openai/v1/chat/completions` 路径、Bearer 头和模型 `openai.gpt-5.6-sol`。

在总尝试上限 4、单目标上限 1 的配置下，主备各实际收到一次调用。Platform 收到最终业务 JSON 并保存原始幂等键，模型步骤与 Halro 账本记录了同一个请求 ID。主路由费用为零，备路由按 fixture 的版本化定价结算 20 µUSD。两次实际本地 HTTP 调用约 0.53 ms 和 0.67 ms，中间约 160.8 ms，包括主路由拒绝结算、备用目标准入和记录开始。该次联调检查了回复和 Gateway Key 不写进 Temporal 历史。独立的 Halro 请求验证了工具定义和严格 JSON Schema 随请求送达 Mantle fixture。

AWS 模型卡列出该模型的 Mantle OpenAI Chat 路径，生产接入仍需由实际账号的 `GET /v1/models`、项目配额及业务评测确认。模拟响应验证了协议转发与结算路径。真实工具调用 ID、多个工具回填、长上下文、修复轮、严格 Schema、实际用量和价格均待账号侧评测。

[AWS 模型卡](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-56-sol.html) · [AWS 模型枚举](https://docs.aws.amazon.com/bedrock/latest/userguide/models-get-info.html) · [Halro AWS 接入](../guides/aws-surface-selection.md)

## 生产配置待核对

- **AWS 备用模型**：核对生产账号、区域和 Bedrock Project 的可用模型、配额与定价，并验证候选模型的实际工具调用和结构化输出。
- **Platform 项目限额**：核对生产别名对应的 OpenAI 模型、Mantle 备用部署，以及 Halro Project 的 `rpm`、`tpm` 和 `max_concurrency`。

压测 Project 的 `rpm=300`、`tpm=2000000`、`max_concurrency=60`。入口每来源限额为 600 RPM，Provider 与 Deployment 并发限额均为 0，日预算为 0，Token Guard 未绑定。这里的 0 表示对应维度不限制。测试 TPM 来自本地负载预算，生产目标须结合实际模型配额和请求 token 分布。

## 60 个在途请求

单进程 Halro、独立 bbolt 和账本目录、`balanced` WAL、生产 Gateway handler 与回环 HTTP 上游。Linux arm64 环境为 Mac16,1 上的 OrbStack，有 10 个 CPU 线程可用。测试逐请求用 `X-Request-ID` 关联总耗时和上游 HTTP 耗时，扣除上游后再求分位数。结果仍包含客户端到 Halro 的回环 HTTP 开销。

| 场景 | 成功数 | 实际峰值在途 | p50 增量 | p95 增量 | p99 增量 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 20 会话，3 轮 | 60 | 20 | 80.0 ms | 102.6 ms | 103.4 ms |
| 60 并发快速返回，3 轮 | 180 | 60 | 95.5 ms | 111.6 ms | 112.6 ms |
| 分钟级等待 | 56 | 60（含超时边缘组） | 94.0 ms | 102.8 ms | 103.0 ms |
| 接近 75 秒上游响应头超时 | 4 | 同一批 60 | 80.1 ms | 88.8 ms | 88.8 ms |

分钟级测试在最后一个请求进入上游后维持 60 个在途请求 65 秒，再释放其中 56 个，其余 4 个继续等 8 秒。300 个请求全部写入请求终态及尝试结算，项目 429 为 0，活跃请求、待结算租约及预留均清空。上游连接复用 240/300，结束时 Go heap 约 20.3 MiB，活动 WAL 约 1.65 MiB。累计 WAL 每批约 20.5 条，平均同步时间 2.33 ms，账务项目锁平均等待小于 1 µs。

macOS 基线快速组 p95 为 3.13 秒，进入上游前的 p95 为 2.26 秒，上游返回后的 p95 为 1.12 秒。同步推理的独立幂等写在并发下串行提交。改动使单节点沿用现有元数据批处理，批次仍需完成 journal 和 bbolt 提交。持久化前置要求和同键冲突校验继续生效，复制模式继续逐请求等待各自的派发确认。合批后的第一次 macOS 快速组 p95 约 206 ms，高于 200 ms 门槛。表中 Linux 结果通过该门槛。

## 复现

在 Halro 根目录执行。两个模拟上游与网关只监听回环地址。

```sh
HALRO_RUN_OPSIQUE_CAPACITY=1 go test ./internal/app \
  -run '^TestOpsique(MantleFallback|SixtyInflight)$' -count=1 -timeout 3m -v

go test -race ./internal/store/bolt ./internal/metadatajournal -count=1
go test ./internal/gateway ./internal/provider/... -count=1
```

Platform 联调使用本地 Temporal dev server。在 Halro 上以 `HALRO_MANTLE_FIXTURE_READY_FILE` 指定私有临时目录中的文件，并运行 `TestOpsiqueMantleFallback`。测试会把回环地址、别名和临时 key 写入该文件。从文件读入 `HARNESS_MODEL_TEST_BASE_URL`、`HARNESS_MODEL_TEST_NAME` 和 `HARNESS_MODEL_TEST_API_KEY`，再设置 `HARNESS_TEMPORAL_TEST_ADDRESS` 与 `HARNESS_TEMPORAL_TEST_NAMESPACE`。从 Platform 的 `services/platform` 目录运行以下命令。

```sh
go test ./internal/runtime/workflows -run '^TestBotRunOnGateway$' -count=1 -v
```

[测试 fixture](../../internal/app/opsique_capacity_acceptance_test.go) · [有序回退规则](../guides/alias-failover.zh-CN.md) · [服务商能力表](../contracts/provider-capabilities.md)
