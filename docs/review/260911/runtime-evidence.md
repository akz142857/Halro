# v0.8.0 全面评审：运行时证据

日期：2026-09-11（Asia/Shanghai）
评审对象：`v0.7.1..222d08f84f61493fc9a273d351cc728528d6e30c`
模式：本机 File mode、临时目录、假兼容服务及保留的 `.invalid` Provider 地址；没有真实 Provider、KMS、生产数据或计费调用。

## 1. 候选身份与环境

- `HEAD` 与冻结时的 `origin/main` 均为 `222d08f84f61493fc9a273d351cc728528d6e30c`。
- 基线 tag `v0.7.1` 指向 `84f2638e973f23935b9eda423143f65ff25a852c`。
- Go：`go1.26.6 darwin/arm64`；主机：Apple M4 Pro。
- 根目录 Node/npm：`v22.22.2 / 10.9.7`；进入 `web/` 后本机工具选择器实际使用 `v25.9.0 / 11.12.1`。后者不在 jsdom 30/Vitest 5 声明的 Node 支持集合内，所以本机前端通过只作为补充证据；受支持 Node 22 的 exact-SHA CI 是正式证据。

## 2. 本机静态与测试门禁

| 门禁 | 结果 |
| --- | --- |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS；`internal/app` 217.503s；Ledger、Bolt、Provider matrix、soak/stress 常规测试均通过 |
| 相关 race：gateway/budget/failurecapture/usage | PASS |
| `npm ci --ignore-scripts` | PASS；0 vulnerabilities |
| `npm run typecheck` | PASS |
| `npm test` | PASS；44 files / 583 tests |
| `npm run build` | PASS；29 个浏览器产物 secret scan clean |
| `git diff --exit-code -- internal/webui/dist` | PASS；提交的嵌入 bundle 无漂移 |
| `sh scripts/check-dependency-license-review.sh` | PASS |
| `git diff --check` | PASS |

聚焦测试还覆盖 domain/compatibility/provider、gateway/budget/failurecapture/requestmeta、usage/ledger，以及 BigModel、Subscription、Provider wiring、Usage failure 等 app 契约。`internal/requestmeta` 本身没有 test file，其行为目前由调用方间接覆盖。

## 3. exact-SHA GitHub CI

GitHub Actions run `34569951375` 在精确 SHA `222d08f84f61493fc9a273d351cc728528d6e30c` 上成功。成功作业包括：

- 全量 Go、全量 `-race`、vet、KMS/production boundary、`govulncheck`、trimpath build；
- 8 个 request-path fuzz target（约 5 分 44 秒）；
- Node 22 下前端 test/audit/build/bundle drift；
- Python 3.12、Node 22、Go 三种官方 SDK 黑盒合同；
- repository hygiene/license；非 root distroless container 与 readiness；observability runtime/scan；Prometheus、Alertmanager、dead-man SPDX SBOM。

Run：<https://github.com/akz142857/Halro/actions/runs/34569951375>

这证明自动化门禁对该 SHA 为绿，不反驳本评审通过负向场景发现的语义和生命周期缺陷。

## 4. 官方 SDK 黑盒兼容性

本机以 `tests/compatibility/server` 监听 `127.0.0.1:18088`，没有访问外网 Provider：

- Node SDK：PASS；
- Go SDK：PASS；
- Python 本机隔离 venv 的依赖下载因本机 TLS CA 链失败而未执行；没有通过关闭证书校验绕过。Python 采用上述 exact-SHA、Python 3.12 CI PASS 作为证据。

R4 评审者另在 Python 3.12.7 隔离环境完成了同一锁定 requirements 和 fake-server test，三种 SDK 均 PASS。

## 5. v0.7.1 → 当前 → v0.7.1 → 当前演练

使用 tag 源码独立构建 `v0.7.1` 二进制，并构建当前候选；实例只监听 loopback，Provider 为 `https://halro-review-upgrade.invalid`，SafeTransport 在任何字节发出前拒绝保留地址。

1. v0.7.1 `init`、`bootstrap`、`serve` 成功；合成 Gateway 请求完成鉴权、路由和记账后得到预期 502。旧版 `doctor`、Ledger 和 Usage 验证均通过：Ledger 5/5 authenticated，Usage 1/1。
2. 当前版本在旧数据上 `doctor` 通过并正常启动；第二条合成请求后 Ledger 10/10、Usage 2/2，manifest 提升到 schema 8。
3. v0.7.1 `doctor` 和 `usage verify` 正确报告 schema 8 不支持，但 `ledger verify` 仍通过，因为 Ledger epoch 未变。
4. **确认 fail-open：** v0.7.1 `serve` 没有拒绝新 manifest，而是启动 Gateway/Admin/Metrics 三个监听器；后台每秒报警 `usage manifest schema version 8 is not supported`。向 Gateway 发送第三条合成请求得到 502（说明鉴权、路由、Provider 尝试与 accounting 已执行），Ledger 增至 15，但旧版无法归档新 Usage。
5. 当前版本再次接管后恢复并导出该请求；`doctor` healthy，Ledger 15/15 authenticated，Usage 3/3、无 missing/duplicate/extra。

这不是数据永久损坏，但证明 v0.7.1 回滚不会 fail-closed，并能在缺失 v0.8.0 Offering/Profile/Region 语义的情况下继续写权威账本。发布必须修复启动闸并明确“回滚只能由升级前备份恢复”的边界。

## 6. 备份与恢复

在上述当前数据上执行：

- `backup create`：PASS，format 3，backup ID `bkp_5d2ea86f90cf1e2c6f67726f52e22bf5`，schema 36，Usage manifest 8，Ledger sequence 15 且 chain verified；
- `backup verify`：PASS；
- restore 到全新 data dir：PASS，Vault verified，schema 36→36，恢复 1 个 enabled Gateway Key（只记录 key ID，不归档明文）；
- restored `doctor`：PASS；Ledger 15/15；Usage 3/3；启动后 `/health/ready` 返回 200、accounting healthy。

备份归档、备份密钥、Master Key、配置和数据目录都保留在 `/private/tmp`，没有进入仓库。

## 7. 同机性能与包体

方法：同一 Apple M4 Pro / Go 1.26.6，`-cpu=4`，两边各 6 次；组件 `1s`，请求生命周期 `100x`。没有 Provider I/O、race detector 或并发其他门禁。

| 指标 | v0.7.1 中位数 | 当前中位数 | 结论 |
| --- | ---: | ---: | --- |
| Registry resolve candidates | 4.846 µs | 5.108 µs | +5.4%，低于 10% 调查阈值；B/op 15,888→16,704，allocs 41 不变 |
| Ledger open/replay 100,000 records | 535.286 ms | 536.859 ms | +0.3%；B/op 约 +9.1%，allocs 约 3,000,030 不变 |
| Request lifecycle（9 个 project/worker 组合） | — | — | 各组合中位数变化约 -3.8% 到 +4.5%，无 >10% 时间回退；低并发 B/op 约 +15%，分配次数基本不变 |
| 初始前端 gzip graph + 最大 locale | 220,942 B | 223,815 B | +1.30%，远低于 500 KiB gate |

前端对比两边都由同一 Node 25.9.0 构建，所以适合做相对包体比较；正式可支持性仍由 Node 22 CI 证明。新增归因字段使序列化字节上升，虽未造成时间阈值回退，仍应在修复后重新取样。

## 8. 未验证边界

- 没有真实 BigModel/Z.AI、MiniMax、Kimi 或其他 Provider credential、枚举/chat/stream/error/quota/billing smoke；相关真实性风险必须由 owner 明确接受或另行授权。
- 没有 AWS KMS、CloudTrail、生产拓扑、真实 retained WAL、24h soak、Linux/Windows/macOS 多架构 RC 实机。
- 没有完成真实浏览器的登录→Provider→Deployment→Project/Key→fake-provider→Usage failure 全旅程、200% zoom、screen reader、forced-colors 和慢 Admin 双写实验。
- 没有运行 release workflow dry-run；当前存在 P1 阻断，按计划不应运行。
