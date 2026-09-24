# Halro 生产验证执行记录 · 2026-09-18

> - 执行方案：[生产验证执行方案](production-validation-plan.zh-CN.md)
> - 候选提交：`f09ed2d768bf2cd335478223beb4d57af326013b`（冻结时工作树只含未提交的 `docs/` 修改；
>   本次执行随后在同一工作树上落了修复，见第 9 节与 260918-PV-F-01）
> - 运行版本串：`v0.8.3-11-gf09ed2d-dirty`
> - 执行环境：单台 macOS 26.6 / Darwin arm64 开发机，Go 1.26.6，Node v22.22.2
> - 执行角色：Application / Security / SRE / Platform 四个独立评审角色（由 agent 承担，
>   条件见[里程碑评审计划 4.5](../review/milestone-professional-review-plan.zh-CN.md)）
> - **结论：`NO-GO / PRODUCTION UNVERIFIED`**
> - 角色原始报告：[`260918-production-validation/roles/`](260918-production-validation/roles/)
>   （[application](260918-production-validation/roles/application.md) ·
>   [security](260918-production-validation/roles/security.md) ·
>   [sre](260918-production-validation/roles/sre.md) ·
>   [platform](260918-production-validation/roles/platform.md)）

## 0. 这份记录是什么

方案的 G0–G7 需要隔离的生产形态环境、真实 Provider 账户、真实 KMS/PKI、真实 Contact Point、
连续 24 小时窗口和四方人工签署。本次执行没有这些条件，因此按方案 §11 的约束，只做 E1–E3：
**没有任何计费 Provider 调用、没有破坏性实验、没有正式发布、没有把结论升级为生产验证完成。**

本记录的作用是把「还差什么」变成精确的、可排期的清单，并把此刻能取得的 E1–E3 证据固定到
候选 SHA 上，让真正执行 G0–G7 时不必重做这一层。

## 1. 门禁结论总表

| 门禁 | 结论 | 取得的最高证据等级 | 阻塞原因 |
| --- | --- | --- | --- |
| G0 候选冻结与仓库门禁 | `CONDITIONAL PASS` | E3（本机）/ E2（CI 未在本 SHA 重跑） | 工作树不干净导致版本串带 `-dirty`；制品、SBOM、签名未产出 |
| G1 目标环境与观测基线 | `PARTIAL` | E3（单机 loopback） | 无隔离目标环境、无 TLS/IAM/存储类别对齐、无抓取链路 |
| G2 真实 Provider primitive 矩阵 | `BLOCKED` | E2 | 计费调用未授权，无测试账户 |
| G3 身份、密钥、策略与网络边界 | `PARTIAL` | E2（大量）/ E3（少量） | mTLS、真实 KMS/Secret Store、证书吊销与轮换需目标环境 |
| G4 告警、故障注入、备份与恢复 | `PARTIAL` | E3（备份/恢复/崩溃/升级回滚） | 真实 Contact Point、独立故障域 dead-man、只读磁盘/磁盘满需目标环境 |
| G5 容量、退化与 24 小时浸泡 | `BLOCKED` | E2/E3（单点） | 无生产形态负载环境；24 小时窗口未预约；§5 阈值仍未签署 |
| G6 正式发布与干净主机验证 | `BLOCKED` | E1 | 发布授权、干净主机矩阵、下游仓库验证均不可得 |
| G7 非作者演练与四方签署 | `BLOCKED` | — | 需真实值班人员与四方签署人 |

方案 §10 的完成定义要求 G0–G7 全部 PASS。本次有 4 项 `BLOCKED`、3 项 `PARTIAL`，
因此结论保持 `NO-GO / PRODUCTION UNVERIFIED`，不存在可放行的 waiver 路径。

## 2. G0：候选冻结与仓库门禁

### 2.1 冻结的验证单元

| 字段 | 本次值 |
| --- | --- |
| `CANDIDATE_SHA` | `f09ed2d768bf2cd335478223beb4d57af326013b` |
| 工作树状态 | **不干净**。冻结时只有文档改动；本次执行随后在同一工作树上落了第 9 节的修复，涉及 `cmd/`、`internal/app/`、`internal/ledger/`、`.github/workflows/release.yml`、`packaging/`、`tools/` 与新增测试。**因此 2.1 的 digest 与 2.2 的门禁结果只对冻结时的树成立**，必须随 260918-PV-F-01 一并重跑 |
| `RELEASE_VERSION` | 未确定（本次不发布） |
| `ARTIFACT_DIGESTS` | `bin/halro` `sha256:59b35f6ff7dffffa4de335f3f75bbb276b5a5014c7841df2f54a1e0e773ca8f2`；`bin/halro-deadman` `sha256:148b343985ed215edfdb12530ab104c029b2f15258667b5ff20347813907bd5e`（本机构建，非发布制品） |
| `TARGET_ID` | 无；本机 loopback（`127.0.0.1:18080/18081/19090`） |
| `CONFIG_DIGEST` | 未生成（无目标环境配置） |
| `TEST_PLAN_REVISION` | `f09ed2d` |

### 2.2 本机完整门禁（E3）

| 检查 | 命令 | 退出码 | 结果 |
| --- | --- | --- | --- |
| Go 静态检查 | `go vet ./...` | 0 | PASS |
| 构建 | `make build` | 0 | `bin/halro`、`bin/halro-deadman` |
| Go 测试 | `go test ./...` | 0 | 58 个包 `ok`，无 FAIL，耗时约 3m44s |
| 前端依赖 | `npm ci --ignore-scripts` | 0 | PASS |
| 前端类型 | `npm run typecheck` | 0 | PASS |
| 前端测试 | `npm test -- --run` | 0 | 44 文件 / 618 用例全部通过 |
| 前端构建 | `npm run build` | 0 | PASS |
| 生成物漂移 | `git diff --exit-code -- internal/webui/dist` | 0 | 无漂移 |
| SSE 资源回收 | `HALRO_STRESS=1 go test ./tests/stress -run TestThousandConcurrentSSEConnectionsCleanup -count=1` | 0 | 1000 并发流：goroutine 3 → 5003 → 3，FD 7 → 2007 → 7，heap 峰值 37.3MB → 回落 8.5MB；RSS 106MB 未回落（分配器保留） |

`-race` 在修复落地后按 `AGENTS.md` 的分层策略补跑了 `go test -race ./internal/app/ -count=1`
（本轮改动触及 `Runtime.Close()` 的关闭生命周期）；它同时是 `make check` 的一项，不只是 CI 面。
未执行：全树 `go test -race ./...`、`govulncheck`、`npm audit`、仓库卫生检查、`tools/` python
单测——这些属于 CI 面，本 SHA 的 CI 运行状态见第 7 节 Platform 部分。

### 2.3 版本可追溯性

`./bin/halro version` 输出 `{"build":{"version":"v0.8.3-11-gf09ed2d-dirty","commit":"f09ed2d",...}}`，
能回指候选 SHA，但 `-dirty` 说明构建输入不是一个干净的提交，**不满足 G0「所有制品可追溯到同一
SHA」的严格含义**（260918-PV-F-01）。方案要求发布前用干净候选重跑本节全部内容。

## 3. G1/G4 本机运行证据（E3）

全部在会话隔离目录执行，未接触仓库根目录的 `data/`、`master.key` 与 `config.yaml`。

| # | 实验 | 命令 | 退出码 | 观察 |
| --- | --- | --- | --- | --- |
| R-01 | 配置校验 | `halro config check` | 0 | `configuration valid` |
| R-02 | 空目录初始化 | `halro init` | 0 | 生成 `audit/`、`ledger/`、`halro.db` |
| R-03 | 只读诊断 | `halro doctor` | 0 | `healthy=true`，`vault_status=verified`，bbolt schema v38；`ledger=unverified`、`parquet=warn`（新装预期） |
| R-04 | 账务链验证 | `halro ledger verify` | **1** | 空 ledger 被判为失败，见 260918-PV-F-02 |
| R-05 | 审计链验证 | `halro audit verify` | 0 | 初始 0 条；一次启停后 3 条；恢复后 5 条；崩溃后 7 条，链始终可认证 |
| R-06 | 用量对账（首启前） | `halro usage verify` | **1** | `no such file or directory`，见 260918-PV-F-03 |
| R-07 | 启动与就绪 | `halro start` + `halro healthcheck -url .../health/ready` | 0 | 三个 listener 起于 loopback |
| R-08 | 指标端点未授权访问 | `curl /metrics` | — | HTTP **401**，fail-closed 符合预期 |
| R-09 | 运行态摘要 | `halro stats` | 0 | WAL 队列 0/4096 |
| R-10 | 优雅停止 | `SIGTERM` | — | 进程退出；审计记录 `system.shutdown`；**日志没有任何关闭行**，见 260918-PV-F-04 |
| R-11 | 用量对账（启停后） | `halro usage verify` | 0 | `ledger_records=0 parquet_records=0 missing=0 duplicates=0 extra=0` |
| R-12 | 加密备份 | `halro backup create -key-file ...` | 0 | `format_version=3`，`backup_id=bkp_3a58…`，含 6 个文件的 sha256 与 `master_key_fingerprint` |
| R-13 | 备份校验 | `halro backup verify` | 0 | manifest 完整 |
| R-14 | 缺 master key 的恢复 | `halro backup restore`（目标无 `master.key`） | 1 | `open master key: no such file or directory`，fail-closed 正确：归档不含 master key |
| R-15 | 恢复到全新目录 | `halro backup restore -confirm-backup-id ...` | 0 | `vault_verified=true`，旧目录保留为回滚副本 |
| R-16 | 恢复结果诊断 | `halro doctor`（恢复目录） | 0 | `healthy=true`，审计链延续到 5 条 |
| R-17 | 从恢复结果启动 | `halro start` + readiness | 0 | 就绪 |
| R-18 | 崩溃注入 | `SIGKILL` | — | 无残留锁；`doctor` 仍 `healthy`，审计链可认证，用量对账一致 |
| R-19 | 崩溃后重启 | `halro start` | 0 | 就绪，锁被正确回收 |
| R-20 | 升级 | v0.8.3 写入的数据目录 → 候选二进制 `doctor` + `start` | 0 | `healthy=true`，就绪；两版同为 bbolt schema v38 |
| R-21 | 回滚 | 候选写入的数据目录 → v0.8.3 二进制 `doctor` + `start` | 0 | 可读、可启动，无静默误读 |

**这些证据的边界**：R-01…R-21 全部在**没有任何流量、没有账务记录**的数据目录上取得。崩溃恢复
真正要证明的是在途 reservation/settlement 的终态一致性，那需要请求流量，而流量需要 Provider——
因此这一层的价值止于「进程、锁、链、备份与升级路径成立」，账务终态一致性仍只有 E2 证据
（见第 6 节 SRE 部分的崩溃恢复矩阵映射）。

升级演练的说服力同样有限：候选与 v0.8.3 之间只有依赖升级与文档变更，durable format 未变，
因此 R-20/R-21 证明的是「没有回归」，不是「格式迁移安全」。

## 4. 方案 §5 服务目标：可签署的草案

SRE 角色把九行 `TBD` 全部填成**待签署草案**，每一项标注依据与证据等级。完整表格与推导见
[`260918-production-validation/roles/sre.md`](260918-production-validation/roles/sre.md)，
此处只记结论与必须先解决的矛盾：

- **有仓库依据、可直接签署（5 行）**：队列/重试/failure capture/WAL 上限（均为代码中生效的硬
  上限）、24 小时浸泡事件预算（`tests/soak/main.go:361-386` 已固化）、错误率（`HalroHighErrorRate`
  5%，浸泡收紧到 1%）、TSDB 增长（3.5/4.25 GiB，假设 5 GiB 卷）、goroutine/FD/RSS 增长容差
  （+20/+20/max(64 MiB, 25%)）。
- **无依据、必须四方签署临时阈值（4 行）**：流式首字节、超时率与限流率、Ledger/Audit/Parquet
  每日增长字节数、Provider 费用与 token 预算。
- **三条阈值是占位符**，G1 必须在目标环境重新基线化，否则它们在生产上直接是错的：
  `HalroWALSyncSlow`（0.05s）、`HalroAccountingProjectLockSaturated`（0.25s）、TSDB 的 3.5/4.25 GiB。

### 4.1 三个必须在签署前解决的测量矛盾

1. ~~**方案 §5 要求签署流式首字节 p95/p99，而 Halro 没有任何首字节指标**~~
   —— **2026-09-24 已解决**，选了「新增埋点」：`halro_stream_first_byte_seconds{operation,le}`，
   已进 `docs/contracts/metrics-reference.md` 的导出清单（该清单有契约测试守门）。

   几个定义上的决定，签署 §5 前要知道：
   - **起点是请求到达**，不是流式 handler 入口——由 `WithArrival` 中间件打戳，所以来源限流、
     key 守门、限长读体、JSON 解码都算在里面。SLO 签的是调用方等了多久。
   - **终点是首个事件写出并 flush 之后**。flush 之前字节还在缓冲区，那不等于调用方看见了。
   - **只记成功的数据事件，不记错误事件**。首字节之前就被拒的流不产生样本——否则快速失败
     会拉好这条 SLO 自己的分位数，这是延迟指标被自己的仪器做掉的经典方式。
   - **按四个 face 分列**（`chat_completions` / `responses` / `messages` / `messages_native`）：
     Anthropic 的翻译面要从语义模型渲染，原生面是直通，首字节前的工作量不同。
   - **进程内计数，重启即忘**，与拒绝分类计数器同形，不进 Ledger——这是性能观测，不是账。

   仍然缺的那一半：把 Halro 自己的耗时与上游耗时分开，需要在 Provider 分发处再取一个点。
   那属于 260913 设计评审的「Gateway 附加延迟」，不属于本条，未做。
2. **延迟被量化到 12 个桶边界**（`internal/usage/aggregate.go:25-27`，上界之上只能说“大于 120s”），
   因此签署的 p95/p99 必须落在桶边界上，否则无法用 Halro 自己的指标评判。
3. **Halro 不导出 CPU、RSS、FD 指标**（只有 `halro_process_goroutines`、`go_goroutines`、
   heap、GC、启动时间）。§5 的该行
   只能靠 node_exporter/cAdvisor 之类的外部采集，这使它成为 **G1 的硬前置**，而不是可选项；
   当前 `alert-rules.yml` 也没有任何 CPU/RSS/FD 规则。

## 5. 告警与 dead-man（G4.1–G4.3）

- `make observability-check` 退出码 0：13 条 recording 规则、35 条告警规则、Alertmanager 配置有效。
- 35 条规则的阈值、`for`、severity 与 `runbook_url` 已逐条列出；**runbook 链接与锚点全部可解析，
  零断链**（15 critical / 19 warning / 1 none）。
- **没有任何费用或 token 告警规则**，尽管 `halro_cost_usd_total` 与 `halro_tokens_total` 已导出——
  预算超支目前对 Prometheus 不可见（260918-PV-F-07）。
- dead-man 检查 Halro/Prometheus/Alertmanager 就绪、Prometheus 样本新鲜度与 ADR-0015 审计锚点，
  通过持久 outbox 发送心跳与状态迁移。**并发独立性（探测慢不阻塞心跳）有测试；故障域独立性是
  部署属性，仓库自己也这么写**（`deploy/observability/README.md:128-130`）——G4.2 无法在本仓库关闭。

## 6. Provider 与兼容性（G2）

Application 角色以 `internal/domain/provider_table.go` 为准清点：**34 个 profile 行，其中 10 个
`Withheld`，对外提供 24 个**。结论：

- **没有任何一个已提供的 profile，对 Halro 自身请求路径有 E4 证据。** `provider-real-matrix.md`
  的开篇是 harness 说明（E1）；各真实账户小节要么标注为「适配而非 GA」（DeepSeek、MiniMax、Kimi），
  要么是对服务本身的 `curl` 探测而非经过 Halro（Mantle），要么明确写着未运行（BigModel、订阅面、
  Mantle smoke）。
- **Gemini 没有任何真实账户证据**，而 README 以 Beta 对外提供（260918-PV-F-08）。
- **超时在任何厂商都没有 adapter 级测试**，只有合成的 `DeadlineExceeded` 分类与 408/504 状态映射；
  BigModel 没有任何 HTTP 错误测试。真实 smoke 中只有一条覆盖负向类别（MiniMax 未知模型）。
- **两条 G2 不变量是健康的**：首字节/外部副作用后不跨 Provider 重试（`internal/gateway/service.go:2302`、
  `:2417`、`:2434`，加 `:2617` 的 `Ambiguous` 处理与持久创建状态机）与 `provider_metadata` 能力闸门
  （`internal/app/admin_invocation_targets.go:630`），均有带负向对照的 E2 测试。
- 「正式声明支持」这个词在仓库里没有唯一定义：四个 BigModel profile 与两个 MiniMax 订阅 profile
  写路径可达但不在 README/CLAUDE.md 的清单里（260918-PV-F-09）。
- 已核对**不是**缺陷的一项：endpoint manifest 为 withheld profile 发布 portable 端点的字段规则，
  是 `internal/compatibility/manifest.go:465-470` 的显式设计（保留字段规则可评审，同时跳过
  anthropic 原生覆盖），不是泄漏。

## 7. 供应链与发布（G0/G6）

Platform 角色的结论里有本轮**唯一一条已确认的高危缺陷**：

- **260918-PV-F-10（P1）：自 v0.7.1 起的每一个 release，主 `.deb` 都不在 `checksums.txt` 里，也没有 Sigstore
  签名。** 原因是 `.github/workflows/release.yml:464` 与 `:470` 用 `halro-*`（连字符）匹配，而
  Debian 包名是 `halro_<version>-1_<arch>.deb`（下划线）。已通过下载 v0.7.0–v0.8.3 的真实
  `checksums.txt` 复核。后果：`packaging/apt-repository/scripts/verify-release.sh:36-38` 因缺少
  bundle 硬失败，而 `:29` 的 `--ignore-missing` 会静默跳过校验和——**方案 G6 第 3 步今天就过不了**。
  `.deb` 仍有有效的 SLSA provenance（`release/*.deb` 在 `:460` 的 attestation 里），因此 provenance
  是目前唯一成立的绑定。
- **260918-PV-F-11（P2）：`halro-deadman` 没有任何版本身份**——`release.yml:266-268` 只给了 `-ldflags "-s -w"`（在 `:267`），
  没有 `buildinfo`，也没有 `-version` 标志。`halro` 自身可追溯（release 构建注入完整 40 位 SHA）。
- **260918-PV-F-12（P2）：`CLAUDE.md` 落后 7 个 release**，声称最新为 `v0.5.0`；`gh release list` 显示
  **v0.8.3（2026-09-17）**。方案里建议的候选版本号 `v0.8.1-rc.1` 已被占用。
- fuzz 目标交叉核对 **8/8 一致，零漂移**；但守护是单向的（`ci.yml:130-133` 能抓「已列出但不存在」，
  抓不到「新增但未列出」）(260918-PV-F-13)。
- 本 SHA 的 CI 是绿的（run `35316876983`，7m18s），最近 10 次运行全部成功。
- ~~`make check` 不等于 `CLAUDE.md` 描述的完整门禁~~ —— **2026-09-24 已修**，并加了比对二者的门禁。
- 其他：release-run evidence manifest 只作为 90 天的 workflow artifact 存在、从未发布；仓库内
  Homebrew Formula 镜像仍指向 v0.7.0；容器归档因根 `Dockerfile` 使用浮动 base tag 而不可复现。

### §11 启动清单核实结果

**9 项全部未就绪（0/9）**：4 项需要外部授权（Provider 计费、隔离环境、真实 Contact Point、
指定签署人），5 项需要尚未开始的工作（候选冻结、§5 的九个 TBD、不可变证据存储、发布凭证与
干净主机矩阵、24 小时窗口预约）。

## 8. 安全边界（G3 / G1 敏感信息）

Security 角色的结论里最重要的一条是**结构性的**：G3 不是一个门禁而是十二个——它的六项里有四项
各分成「Halro 半边」（仓库内可测）与「Prometheus/Alertmanager Core 半边」（没有任何 Halro 代码，
见 `docs/observability/admission-checklist.md:13`）。**把 Halro 半边签成整项，是最容易错误通过 G3
的方式。**

覆盖情况：

- **KMS / Secret Store 是覆盖最好的一项**：正常、不可用、恢复、轮换、错误密钥五个场景全部有
  针对 `fakekms` 的故障注入测试；真实 AWS KMS 的 harness 存在但默认跳过（`HALRO_AWS_KMS_DUAL_REAL=1`），
  属于 REQUIRES-E4。
- **预算/账务 fail-closed 是证据最充分的边界**；RBAC 拒绝矩阵有穷举 sweep；指标标签有结构化
  allowlist 测试。
- **secret canary 是双实现**（`web/scripts/check-artifacts.mjs` 加一份 Go 实现），13 个字面量，
  覆盖日志、指标、审计、错误响应体与 `dist`。备份证明的是**加密**而不是脱敏；追踪没有覆盖，
  因为仓库里根本没有 tracing 子系统。
- **mTLS 客户端证书吊销不存在**：全仓库没有 CRL/OCSP/`VerifyPeerCertificate`，G3 第 1 项的
  「revoked identity」只能靠撤销 CA 这种全有全无的手段达成（260918-PV-F-17）。
- ~~**审计乱序/序号断裂检测只有 E1**~~ —— **2026-09-24 已补**。关键在于：要够到 MAC 之后的分支，
  测试必须**持密钥用包自己的编码器造帧**；只会改字节的测试永远走不过 MAC 那一关。
- ~~**没有任何审计完整性告警**，也没有「管理面变更都被审计」的 sweep~~ —— **2026-09-24 两半都补**。
  告警盯的是「变更已提交、记录还没落」的积压：管理面变更与它的 intent 同事务落库，追加在其后，
  所以积压不消退，就意味着变更仍在被接受而描述它们的记录没有落地。锚点告警看不见这件事。
  穷举用静态扫描而不是动态遍历：要观察「变更被记录」，变更必须**成功**，那就是 63 条路由的
  合法 body、合法 revision 和既有资源——那不是 sweep，那是第二套测试。
- **AWS KMS SDK 客户端是 safetransport 之外的真实出网路径**（`internal/kms/awskms/adapter.go:31-47`），
  遵循 env proxy、可达 IMDS。这是 ADR 0010 的既定设计，但必须作为**具名例外**写进 G3 的答案，
  而不是让「safetransport 是唯一出网路径」这句话在评审里被整句接受（260918-PV-F-20）。

### 8.1 对抗裁决：两条被提出的红线

| 发现 | 裁决 | 依据 |
| --- | --- | --- |
| 「备份恢复会复活已撤销的 Gateway Key」构成方案第 7 节未关闭的红线 | **PARTIAL** | 机制上确实不阻止：撤销只是 bbolt 里的 `Enabled bool`（`internal/domain/models.go:462-474`），归档没有防回滚水位。但这不是无人察觉的 fail-open：restore 返回并打印 `restored_enabled_gateway_key_count` / `_ids`（`internal/app/backup.go:138-139`），`docs/guides/backup-restore.md:284-289` 把逐一比对撤销记录列为接受流量前的**强制**步骤。真正的缺口是**这个控制针对的场景没有测试**——已有测试恢复的是一把全程启用的密钥。已补 `TestRestoreNamesAGatewayKeyThatWasDisabledAfterTheBackup`。 |
| 「`CLAUDE.md` 说 failurecapture 是唯一存储调用方内容的地方，这在本 SHA 是假的」 | **CONFIRMED（文档缺陷）** | `data/provider-objects/` 确实持有调用方写入的内容，但它是**加密后**落盘的（`internal/gateway/inference_resources_store.go:147-182`，按 resource+role 作用域、绑定 project、0600、durable rename），并进入备份。文档角色进一步核实后又纠正了三点：落盘的是 Files、**batch 结果**（不是 batch 输入，输入本身就是一个普通 file 资源）与 deferred Responses 的答案（请求对象在答案到达后即被抹去）；「bounded」对 Files 只成立于大小与生存期，**每 Project 的文件数量没有上限**；封装与上限出自 ADR 0024，ADR 0021 只授权了这个存储的存在。此外还有两处**明文**的调用方元数据——上传的 filename/purpose/content-type 存在 `halro.db`，Outcome 的 `evidence_ref` 存在治理日志与导出里。`CLAUDE.md` 已按这些事实改写。 |

另一条被提出但**不成立**的：endpoint manifest 为 withheld profile 发布字段规则，见第 6 节。

## 9. 本轮发现与处置

编号沿用 [里程碑评审计划](../review/milestone-professional-review-plan.zh-CN.md) 第 9.1 节的格式，
严重度按「影响 × 可达性 × 暴露范围 × 可恢复性」，并有两处**明示的偏离**：

- 该节要求里程碑标识与证据目录名一致。这里的 ID 用短前缀 `260918-PV-`，证据目录是
  `260918-production-validation/`；保持短前缀是为了在表格里可读，不是遗漏。
- 该节要求 P0/P1 与被延后的 P2/P3 各绑定一个 GitHub Issue。本轮尚未建 Issue：这批发现是执行
  过程中即时产生的，建 Issue 属于第 10 节的下一步动作。在 Issue 建好之前，下面的 OPEN 项按该
  计划第 9.3 节的定义**不构成已接受的风险**，只是待处理项。

该计划第 12 节的目录规范是为 `docs/review/<YYMMDD>/` 下的评审写的；生产验证执行记录归
`docs/verification/`，只带 `roles/` 证据目录，这是两类产物的既有分工，不是对第 12 节的违反。

| ID | 严重度 | 发现 | 状态 |
| --- | --- | --- | --- |
| 260918-PV-F-01 | P2 | 候选工作树不干净，版本串带 `-dirty`，G0 的「制品可追溯到同一 SHA」不成立 | OPEN（发布前必须用干净提交重跑 G0） |
| 260918-PV-F-02 | P3（降级） | 全新安装上 `halro ledger verify` 退出 1，且信息是「ledger chain could not be authenticated」，读起来像损坏 | **已修为信息问题**：退出码**仍然非零**——安全评审证明放行会把「擦掉 WAL + 清零 checkpoint」变成干净体检（见 260918-PV-R-10）；改为区分两种状态的措辞，并补 `ledger.ChainReport.HoldsFrames` 与擦除场景回归测试 |
| 260918-PV-F-03 | P3（降级） | 首次启动前 `halro usage verify` 以裸 `no such file or directory` 退出 1 | **已修为信息问题**：同样保持非零退出，改为说明是哪种状态；并保留「manifest 在、分区文件丢失」必须失败的反向测试 |
| 260918-PV-F-04 | P3 | 优雅停止不写任何日志行，日志上「停止」与「被杀」不可区分 | **已修**（`internal/app/runtime.go` 增加 shutdown started/complete） |
| 260918-PV-F-05 | P2 | `operator-guide.md` 与 `user-guide.md` 声称「删除一个键即恢复默认值」，与内嵌模板 `internal/config/default.yaml:3-7` 的说明相反。真实行为是三分的：`server`/`storage`/`gateway`/`usage.durability`/`usage.timezone` 删掉会校验失败拒绝启动，其余多数被 `Normalize` 补回默认值，而**删掉布尔值会静默变成 `false`**（如 `metrics.require_auth`） | **已修**（两份指南；第一版修正矫枉过正，见 260918-PV-R-13） |
| 260918-PV-F-06 | P1（方案） | 方案 §5 要求签署流式首字节 p95/p99，而 Halro 没有任何首字节指标 | **CLOSED 2026-09-24**：选了「新增埋点」。`halro_stream_first_byte_seconds{operation,le}`，观测点在 HTTP 层 flush 之后（`internal/gatewayapi/first_byte_metrics.go`），起点由 `WithArrival` 中间件在请求到达时打戳。不选压测端测量的理由：那样生产里看的数和签的数来自两台不同的仪器 |
| 260918-PV-F-07 | P2 | 没有任何费用或 token 告警规则，预算超支对 Prometheus 不可见 | **已修 2026-09-24**，但**修法与最初设想不同**。先做的是「加一个实例级日预算 + 阈值写进规则文件」，运营者指出重复：**日预算在控制台的项目里早就有**（`Project.DailyBudgetMicrosUSD`），而且它**真拒绝**（准入前扣预留），比告警强；Token Guard 策略还另有 `cost_micros_per_minute` 与 EWMA 版本，并且已经走 webhook 发告警。缺的从来不是预算，是**没人对已有的那个写规则**。所以实例级设置整个退掉，改为对 `halro_policy_rejections_total{reason="budget"}` 与 `{reason="run_budget"}` 告警——**规则文件里没有任何阈值**，阈值就是运营者在项目上设的那个数。外加 `absent(halro_cost_usd_total)` 兜底：费用信号不能在自己的数据源坏掉时静默 |
| 260918-PV-F-08 | P2 | Gemini 以 Beta 对外提供，却没有任何真实账户证据 | OPEN（G2，需授权） |
| 260918-PV-F-09 | P3 | 「正式声明支持」无唯一定义：BigModel 四个、MiniMax 订阅两个 profile 写路径可达但不在 README/CLAUDE.md 清单 | OPEN |
| 260918-PV-F-10 | **P1** | 自 v0.7.1 起每个 release 的主 `.deb` 都不在 `checksums.txt`、也没有 Sigstore 签名（`halro-*` 匹配不到 `halro_*.deb`），且发布流水线的校验步骤读同一份清单所以从未报警 | **已修**（`release.yml` 三处清单 + `tools/release/test_release_workflow_contract.py` 契约测试，已做反向验证） |
| 260918-PV-F-11 | P2 | `halro-deadman` 没有版本身份（release 只给 `-s -w`） | **已修**（tarball、`.deb` 与容器镜像统一注入 buildinfo + `-version` 标志 + 测试 + 契约断言；容器部分见 260918-PV-R-01/R-02） |
| 260918-PV-F-12 | P2 | `CLAUDE.md` 落后 7 个 release（称最新 `v0.5.0`，实为 `v0.8.3`）；同处的前端用例数 276 也已过期（实为 618） | **已修** |
| 260918-PV-F-13 | P2 | fuzz 清单守护是单向的：新增但未登记的目标永远不会被 fuzz，CI 仍是绿的 | **已修**（反向清点测试，已做反向验证） |
| 260918-PV-F-14 | P3 | `make check` 不等于 `CLAUDE.md` 描述的完整门禁（缺 typecheck、生产构建、bundle 漂移），只有 `make full-check` 覆盖 | **已修 2026-09-24**：改的是文档不是 Makefile —— `full-check` 从 v0.8.2 起就是发版证据里记录的门禁名，动它会断掉证据链。`CLAUDE.md` 现在指向 `make full-check` 并注明要 Node 22；`make check` 明确标为「迭代循环，不是门禁」。**并加了防漂移门禁**：`tools/gates/test_documented_gate_contract.py` 比对文档指名的目标与 Makefile 的实际依赖（该目标必须够得到 `frontend-production-check`），已进 CI。一次性改文档不够——这两者当初分开，正是因为没有东西在比对它们 |
| 260918-PV-F-15 | P2 | `CLAUDE.md` 的「failurecapture 是唯一存储调用方内容之处」在本 SHA 不成立，`data/provider-objects/` 是第二处（加密、有界、有 ADR） | **已修** |
| 260918-PV-F-16 | P2 | 「恢复不得复活已撤销身份」这条红线的唯一控制（restore 列出被重新启用的 Gateway Key）没有针对性测试 | **已修**（新增回归测试） |
| 260918-PV-F-17 | P2 | 没有 mTLS 客户端证书吊销机制（无 CRL/OCSP/`VerifyPeerCertificate`） | OPEN（设计决定，需在 G3 明确接受或补齐） |
| 260918-PV-F-18 | P2 | 审计乱序/序号断裂检测只有代码没有测试，篡改测试先被 HMAC 拦截 | **已修 2026-09-24**：`internal/audit/sequence_test.go` 5 条，都持密钥用包自己的编码器构造帧，才够得到 MAC 之后的分支——删掉中间一帧（序号缺口）、交换两帧（乱序）、合法签名但序号放错、**重签一条记录**（链断，MAC 全部有效）、以及带缺口的日志 `Open` 时拒绝而不是当成半帧修复。两个分支分别反向验证：禁掉序号检查挂 3 条，禁掉链检查挂第 4 条 |
| 260918-PV-F-19 | P2 | 没有审计完整性告警，也没有「管理面变更都被审计」的穷举证明 | **已修 2026-09-24**：两半都补。**告警**：新增 `halro_admin_audit_intents_pending`（读失败报 -1，因为 0 是健康值，读不到的库不能冒充健康）与 `halro_admin_audit_delivery_failures_total`，配三条规则 `HalroAdminAuditBacklogStuck` / `...Unreadable` / `...DeliveryFailing`，各带 runbook 小节。锚点告警看不见这件事——锚点见证的是**存在的链**，不是**缺失的记录**。**穷举**：`TestEveryAdminMutationRouteCanReachTheAuditTrail` 静态扫 `requireAdminMutation` 注册的全部 handler，在包内调用图里找审计落点；反向验证过（加一条无审计的变更路由即失败） |
| 260918-PV-F-22 | P2 | **F-19 的 sweep 新发现**：`refreshAdminInvocationTargets` 与 `refreshAdminModelCatalog` 拿运营者凭据对外发起调用却不留任何审计，而同样拨打 Provider 的 `createAdminModelCapabilityDetection` 是审计的。两者都不改控制面状态，所以 sweep 可以靠具名豁免通过——但这是**不一致**，不是结论 | OPEN（要不要补事件属于审计契约变更，`docs/contracts/audit-integrity.md` 固定了事件形状，留给四方裁决而不是由一个测试决定） |
| 260918-PV-F-20 | P3 | AWS KMS SDK 是 safetransport 之外的出网路径（ADR 0010 既定），必须在 G3 作为具名例外记录 | OPEN（文档化，不是缺陷） |
| 260918-PV-F-21 | P2 | 没有 `SIGKILL` 测试（全仓零命中）；只读文件系统与真实磁盘满同样无自动化覆盖 | **大部分已修 2026-09-24**，见下方说明。剩余：真实 EROFS 挂载与真实磁盘满仍需目标环境（G4） |

**F-21 的处置（2026-09-24）**

- **`SIGKILL`：已自动化，进普通测试套件**。`TestSIGKILLLeavesEveryReportedRequestRecoverable`
  用测试二进制自重入：子进程打开数据目录、循环跑完整的计费生命周期（begin → reserve →
  start → settle → finalize），**每条全部持久化之后才报告**；父进程在中途 `kill -9`，然后必须
  把子进程声称结算过的每一条都找回来。断言子进程确实死于 `SIGKILL`（而不是自己崩的），否则
  这个测试会在什么都没证明的情况下通过。
  它补上了 R-18 的两个短板：R-18 是手工的，而且跑在**没有流量、没有账务记录**的目录上。
  两条断言都做了反向验证：截断 Ledger → doctor 报错；删掉一条已恢复记录 → 「已报告却找不回」
  精确触发。
- **只读文件系统：一半已自动化，一半点名留给目标环境**。用权限位（EACCES）覆盖了
  「初始化被拒」「`Open` fail-closed」「`doctor` 仍可读」。
  写这组测试时得到一个**与预期相反**的结果并保留了下来：账本**目录**只读时实例照常启动并继续
  追加——目录写位管的是创建/删除条目，不是写已打开的文件。这正是权限位替代不了真实 EROFS 挂载
  的原因，已作为一行记进崩溃恢复矩阵，而不是假装覆盖了。
- **真实磁盘满：仍需目标环境**，但可自动化的那部分补上了。给 metadata journal 加了与 Ledger
  同形的 `WrapDurability` 故障注入缝（HA 设计 §17 的门禁表本来就要求为 journal 补这条缝），
  于是「帧写不下去时事务必须一起失败」这条不变量现在有测试：`ENOSPC` 与 fsync `EIO` 两种，
  外加「失败的 append 不推进链、文件仍可验签、之前的写仍在」。反向验证过：把 append 的错误
  忽略掉让事务照常提交，测试以「a metadata write committed while its frame could not be made
  durable」失败。
| 260918-PV-F-22 | P3 | release-run evidence manifest 只是 90 天 workflow artifact、从未发布；仓库内 Homebrew Formula 镜像仍指向 v0.7.0；容器归档因浮动 base tag 不可复现 | OPEN |

已修 10 项：260918-PV-F-02、F-03、F-04、F-05、F-10、F-11、F-12、F-13、F-15、F-16。其中 7 项带
回归测试，F-05、F-12、F-15 是纯文档修正。F-03、F-10、F-13 与 F-16 做了反向验证（先把缺陷放回去，
确认测试变红，再恢复修复）。

### 9.1 对这批修复的独立评审（第二轮）

修复本身又由四个不看修复作者结论的角色评审了一遍，抓到的问题按同一标准处置：

| ID | 严重度 | 发现 | 处置 |
| --- | --- | --- | --- |
| 260918-PV-R-01 | **P1（自造回归）** | 让 `cmd/halro-deadman` 引入 `internal/buildinfo` 直接弄坏了 dead-man 容器镜像：`external-probe/Dockerfile` 只 COPY 它当时需要的三个目录，仓库里本来就有守护测试 `TestDeadmanImageCopiesEveryPackageItNeeds`，它变红了——而我第一轮只跑了改动包，没跑 `./deploy/observability/` | 已修：Dockerfile 补 COPY；本轮把该包纳入受影响集合重跑 |
| 260918-PV-R-02 | P2 | 版本身份只注入了 tarball 与 `.deb`，容器镜像仍写死 `-ldflags "-s -w"`——而容器正是 dead-man 最常见的部署形态，同一次发布的两个产物会对自己的身份给出不同答案 | 已修：Dockerfile 加三个 `ARG`，`release.yml` 传 build-arg，并补 `docker run ... -version` 冒烟（该镜像此前没有任何运行时冒烟） |
| 260918-PV-R-03 | **P1（修复不完整）** | `usage verify` 的豁免用 `errors.Is(err, os.ErrNotExist)` 判断「还没导出过」，但 `Verify` 会逐个哈希 manifest 列出的分区，分区文件丢失时返回的是同一个 `*os.PathError`——账本为空而 usage 目录残留的场景下，**丢失分区会被判为干净** | 已修：改为直接 `os.Stat` manifest；新增 `TestVerifyUsageFailsWhenAnEmptyLedgerSitsUnderAManifestMissingItsPartition`，反向验证确认旧写法会放行 |
| 260918-PV-R-04 | P2 | `TestRestoreNamesAGatewayKeyThatWasDisabledAfterTheBackup` 的头号断言是**可被平凡满足**的：该列表完全由归档中的 store 计算，与实时状态无关 | 已修：加入对照密钥（备份时即禁用），备份后把两把钥匙状态互换，断言只点名 A；注入「列出所有密钥」的缺陷后测试变红 |
| 260918-PV-R-05 | P2 | `verify-release.sh` 的 `sha256sum --check --ignore-missing` 只要有一个文件通过就退出 0，所以同一个缺陷复发时它依然抓不到；`release.yml` 的 container-push 是同一形状 | 已修：两处都先用 `awk`+`grep -Fx` 断言成员资格再校验（`publish-ghcr.yml` 早就是这个写法） |
| 260918-PV-R-06 | P3 | 关闭日志的措辞不准：listener 排空发生在 `Serve` 里，`Close()` 之前，所以「shutdown started」写在 `Close()` 里是错位的 | 已修：`Serve` 记 `draining listeners`，`Close` 改为 `closing runtime` / `runtime closed`，测试同步 |
| 260918-PV-R-07 | P3 | `HoldsFrames()` 只对「补齐了 sealed 计数」的报告有意义，裸 `VerifyChain` 报告会答错 | 已修：在 doc comment 写明前置条件 |
| 260918-PV-R-08 | P3 | fuzz 反向清点会走到 `tests/compatibility/go` 这个独立模块，产生无解的假阳性 | 已修：范围收窄到 `internal/` |
| 260918-PV-R-09 | P3 | `snapshotHoldsUsage` 用五个近似冗余的信号，邀请字段漂移；`-version` 测试用固定 512 字节缓冲读取 | 已修：收敛为 watermark + floor 两个真正的权威信号；改用 `io.ReadAll` |
| 260918-PV-R-10 | **P1（修复方向错误）** | 安全角色在真实数据目录上构建两侧二进制并实测：**把 WAL 截断为 0 + 用 `init` 写入的零值覆盖 bbolt 里的 ledger chain checkpoint + 删除 `data/usage/`**，基线二进制 `ledger verify`、`usage verify` 都退出 1，而我改过的二进制两条都退出 0、`doctor` 也是 0——一个几分钟前还在计费的目录，Halro 提供的每条只读命令都报告成功。checkpoint 是明文 JSON（`internal/store/bolt/store.go:1707`），Go 侧的单调性守卫挡不住直接写 bbolt | **已回退放行逻辑**：两条命令恢复非零退出，只把「不可读的 errno / 像损坏的措辞」改成区分两种状态的说明；新增 `TestVerifyLedgerReportsNoAuthenticatedChainAfterAWipedWALAndZeroedCheckpoint` 复现该擦除并记录「只有调用方的拒绝把它和全新安装分开」 |
| 260918-PV-R-11 | P3 | `halro-deadman -check-config -version` 会先答版本再退出 0，组合式部署门禁会被「什么都没校验」的运行满足 | 已修：`-check-config` 在场时先加载校验配置；补测试 |
| 260918-PV-R-12 | P2 | `CLAUDE.md` 改写后仍有一句无依据：「deferred 与 batch 两层有上限」——batch 没有任何每 Project 上限；另外上传件是**无条件**本地封存，不只是本地 Files 模式 | 已修：改为只点名 `max_deferred_queue`，并写明无条件封存 |
| 260918-PV-R-13 | **P1（文档矫枉过正）** | 我对两份指南的「删除键即恢复默认」修正走到了另一个极端：`Normalize` 确实把大多数省略标量补回默认值（实测 15 个顶层块里 11 个可整块删除后 `config check` 仍然 valid），真正危险的是**布尔值被删会静默变成 `false`**（如 `metrics.require_auth`），这一点两版措辞都没写 | 已修：两份指南改为分别说明「哪些补默认、哪些拒绝启动、布尔静默关闭」，与 `internal/config/default.yaml:3-7` 的既有说法一致 |

另有两条评审意见**记录但不改**：`halro ledger verify` 对「有帧但链不可认证」比 doctor 更严格
（退出码只有两个状态，严格方向是安全的）；以及「WAL 与段清单同时消失且 checkpoint 仍为 0」的
残余盲区——它在 `reconcileLedgerChainCheckpoint` 与 doctor 里本来就存在，本次改动只是让 CLI 与它们
一致，真正的修法是让 `app.VerifyLedger` 显式区分「已验证为空」与「无从验证」，属于单独的设计改动。

## 10. 下一步可执行动作

按依赖顺序，前三项不需要任何外部授权：

1. 提交当前工作树，用干净提交重新冻结候选并重跑 G0（260918-PV-F-01）。
2. ~~决定 260918-PV-F-06 的处理方式~~ —— **2026-09-24 已做**（新增埋点，见第 4.1 节）。§5 的首字节行不再是阻塞项；该行的**阈值**仍待四方签署。
3. ~~补 260918-PV-F-18、260918-PV-F-19 的自动化覆盖~~ —— **2026-09-24 已做**（F-21 同日处置，剩余部分需目标环境）。新增 260918-PV-F-22 待四方裁决。给 260918-PV-F-07 定阈值后补告警规则仍待办。
4. 申请并记录授权：Provider 测试账户与费用上限、隔离目标环境、真实 Contact Point、
   四方签署人、24 小时窗口（方案 §4.2 / §11）。
5. 授权到位后按 G0 → G7 顺序执行，每个实验一个不可变证据 ID（方案 §8 的 YAML 格式）。
6. 本记录的 E1–E3 结论在候选变化后**不可沿用**：260918-PV-F-01 一旦修复即产生新候选，第 2 节必须重跑。
