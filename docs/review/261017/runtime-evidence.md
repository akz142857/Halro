# 运行时证据（E01–E09）

> 全部使用真实二进制与真实数据目录。候选版本：从冻结 `main` `2e749db1` 构建的 `halro`（darwin/arm64 与 linux/arm64，`-ldflags` 标记 `v0.8.5-127-g2e749db1`）。基线：GitHub Release 下载的 v0.8.5 二进制（darwin-arm64、linux-arm64），均按 `checksums.txt` 校验通过。Provider 一律指向不可解析的 `https://halro-smoke.invalid`，没有任何计费或外部调用。数据目录与日志留在 scratch，不入仓。

**环境说明**：取证期间主机上一直运行着 owner 的两个容器 `langfuse` 与 `midigi-postgres`（服务于其他业务，按 owner 要求保持运行、未做任何操作）；另有 owner 的开发实例 `halro start`（主检出目录，端口 8080/8081/9090）在运行，取证实例全部使用 18180/18181/19190 端口与独立数据目录。时间类数字（就绪耗时、吞吐）因此不是独占主机下的测量。

## 总表

| ID | 证据 | 结果 |
| --- | --- | --- |
| E01 | 从真实 v0.8.5 数据升级（macOS） | **PASS**，附 V-005、V-006 的实测 |
| E02 | 回滚围栏 + 用升级前备份回到 v0.8.5 | **PASS** |
| E03 | 崩溃恢复（Linux 容器，SIGKILL） | **PASS**（11 轮） |
| E04 | 磁盘异常 | **PASS**（磁盘满失败关闭、无损坏）；只读与慢盘 BLOCKED；E04c 复现 V-001 |
| E05 | 4 小时内存长跑 | 见 E05 节 |
| E06 | 热状态上限触发 | 见 E06 节 |
| E07 | 有规模数据的备份/恢复 | **PASS** |
| E08 | Linux 上的全量测试 | **PASS**（CI） |
| E09 | 真实 Provider smoke | **BLOCKED**：owner 未授权计费 |

## E01 · 从真实 v0.8.5 数据升级

**数据准备**（v0.8.5 写入）：`halro init` + `admin bootstrap` + 两次 `bootstrap`（一个 openai_compatible Provider 指向 `.invalid`），经 Admin API 完成 MFA 注册、开启 Project 的 Run Governance、创建 BOOLEAN outcome definition、创建 8 把带治理 scope 的 Key；`settings.health_probe_interval_seconds` 设为 3600 以免主动探测把 deployment 判为不健康。随后：

- 100,000 个普通 chat 请求（32 并发，全部 502 connect 失败，1,399 s）；
- 1,028 个完整治理周期（建 Work Unit → 建 Run → 2 个归属请求 → 关 Run → 报 Outcome → 关 Work Unit；按每 Key 120 写/分钟的上限限速，8 Key 并行）；另 20 个 Work Unit 保持打开、各带一个 TTL 24 h 的活动 Run 与一个归属请求。

v0.8.5 停机后自检：`ledger verify` 515,580 帧 `ChainVerified: true`；`usage verify` 102,282/102,282；`audit verify`、`doctor` 通过。用 v0.8.5 创建升级前备份（`schema_version: 38`）。目录 965 MB（`halro.db` 381 MB、`ledger` 576 MB、`usage` 7.7 MB）。

**候选版本对该目录（拷贝）**：

| 步骤 | 结果 |
| --- | --- |
| `config check`（未迁移） | exit 1，逐条列出 4 个退休键及其替代 |
| `config migrate --write` → `config check` | 写出 `version: 2`、`routing`，保留 `config.yaml.before-migrate`；exit 0。**`ledger.seal` 仍为 `enabled: false`、`8589934592`**（V-006） |
| 第一次启动（升级） | metadata 38→41；`accounting state opened from_snapshot=false head_sequence=515580`；**11.94 s** 就绪 |
| 升级后流量 | 500 个 chat（502）；24 个治理周期全部 201；已有 Key 调 `GET /v1/models` → 403（无 `discovery` scope）；未知 `gw_` Key → 401，正文不含密钥 |
| 停机后自检 | `ledger verify` 518,416 帧通过；`usage verify` 102,830/102,830；`audit verify`、`doctor` 通过（`healthy: true`） |
| 新文件 | `history.db` 591 MB（约 5.75 KB/请求，与 Ledger 同量级，无保留期）、`archive.db` 8 MB、`state.snap` 11 MB |
| 第二次启动 | `from_snapshot=true`；**5.42 s** 就绪（封存仍关，活动 WAL 606 MB） |
| 第三次启动（移走 `state.snap`） | `from_snapshot=false`；10.86 s 就绪 |
| 三次启动的状态对比 | 通过 Admin API 读出的全部 1,080 个 Work Unit（含已归档）及成本合计、Project 预算占用：**三次完全一致** |

## E02 · 回滚围栏

- 已发布 v0.8.5 二进制以原配置打开迁移后的目录：`halro: metadata schema version 41 is newer than this build supports (38)`，拒绝启动；之后候选版本 `doctor` 仍 `healthy: true`，目录未受损；
- 用 v0.8.5 把升级前备份恢复到新目录：恢复成功；`ledger verify` 515,580 帧、`usage verify` 102,282/102,282、`audit verify`、`doctor` 全部通过；v0.8.5 在恢复目录上 12.18 s 就绪。

结论：回滚只能经由升级前备份；这条路径实测可用。

## E03 · 崩溃恢复（Linux 容器，SIGKILL）

环境：Docker `python:3.13-alpine`（linux/arm64），数据在 Docker volume（ext4），候选 linux 二进制。起点是 E01 升级后的目录，配置只改 `ledger.seal.enabled: true`、`max_active_bytes: 268435456`。每轮：启动 → 就绪 → 16 线程普通请求 + 8 Key 治理周期 → 指定秒数后 `kill -9` → `ledger verify`、`usage verify`、`audit verify`、`doctor`。

| 轮 | 负载秒数后 kill | 命中的写路径 | 就绪 | 恢复方式 | 校验 |
| --- | --- | --- | --- | --- | --- |
| 1 | 6 | 首次封存（启动后 5.4 s 封存 619 MB 的第 1 代）之后 | 6 s | `from_snapshot=true` | 全部通过 |
| 2–5 | 7、9、15、31 | 历史批量写；其间无新快照 | 5–7 s | 快照仍在已封存的第 1 代，按设计从该代首帧重放 | 全部通过 |
| 6–9 | 59、61、62、63 | 每分钟快照与归档刷写；第 6、8 轮期间各封存一代 | 1–3 s | 快照在活动代 | 全部通过 |
| 10–11 | 90、125 | 第 4、5 代封存之后 | 4 s | `from_snapshot=true` | 全部通过 |

最后一次正常启动（`from_snapshot=true`，head 1,671,752）与移走 `state.snap` 后的全量重建（`from_snapshot=false`）读出的 2,648 个 Work Unit 与 Project 预算占用**完全一致**；预算占用 `committed_micros_usd: 576` 来自被杀时在途尝试的保守结算，所以比较覆盖了非零余额。结束时 Ledger 共 6 代（5 代封存未压缩：压缩在之后的维护周期进行，本轮未到达；压缩代内的快照恢复由发布准备时的一次性测试覆盖）、`history.db` 1.87 GB、`archive.db` 52 MB。

说明：`usage verify` 只核对已导出到 Parquet 的前缀（`usage.parquet_interval: 1h`），所以前 11 轮报告的 `ledger_records` 一直是 102,830，最后一轮导出后为 332,278；这是既有语义，不是本范围的变化。

## E04 · 磁盘异常（Linux 容器）

**E04a 磁盘满**：数据放在 1,900 MB 的 tmpfs 上（E01 升级后目录的拷贝，开封存），填充文件只留约 20 MB。

| 阶段 | 结果 |
| --- | --- |
| 16 线程 20,000 请求期间写满 | 2,694 个 502（正常的上游失败），17,306 个 **503**（失败关闭）；`ledger generation was not sealed: accounting is not healthy`；`history.db` 写失败 12 次，待写行 361 |
| 写满之后 | 新请求全部 503 |
| 删除填充文件、**不重启** | 仍全部 503：Ledger 写失败后记账保持"不健康"直到重启（fsync 失败后的保守做法） |
| 停机后离线校验 | `ledger verify`/`usage verify` 拒绝：`ledger has a partial tail; run halro doctor or start Halro to repair it`，`doctor` 不健康——符合文档 |
| 重启 | `from_snapshot=true`，修复尾部；请求恢复 502；四项校验全部通过 |
| 第二次写满（E04b 期间 `history.db` 继续增长） | 快照写入失败计数 +1（`no space left on device`）；Parquet 导出失败；再次启动时租约恢复追加失败，**拒绝启动**并给出 `recover pending accounting leases: … no space left on device` |

没有任何一次出现数据损坏：每次腾出空间并重启后，校验全部通过。只读文件系统与慢盘需要特权容器与块设备节流，本轮 **BLOCKED**。

**E04c 一行用量历史损坏（V-001 的真实二进制复现）**：在普通 volume 上，先写入当前小时，停机后用一次性工具翻转 `history.db` hourly 桶最后一行的首字节，再以 16 线程持续请求 120 s：`flush_failures_total` 与 `row_auth_failures_total` 每 10 s 涨约 450，`pending_records` 始终为 0；日志 `usage history catch-up failed`、`usage parquet export skipped; history write failed`；重启后同样失败。翻转最早一行（E04b）则毫无影响。详见 V-001。

## E07 · 有规模数据的备份/恢复（候选版本）

`backup create`（exit 0）→ `backup verify`（exit 0）→ `backup restore` 到新目录（exit 0；恢复后只有 `history.db`，没有 `archive.db`，符合设计）→ 启动 10.81 s 就绪，`from_snapshot=false`（备份不带 `state.snap`），`archive.db` 重建 → 读出的 1,080 个 Work Unit 与预算占用和 E01 第三次启动**完全一致** → `ledger verify`、`usage verify`、`audit verify`、`doctor` 全部通过。

## E08 · Linux 上的全量测试

`main` 上不存在 `_linux_test.go` 或 `//go:build linux` 的测试（只有 `//go:build linux || darwin` 的 `internal/hostsecurity/harden_test.go`）；`scripts/linux-only-tests.sh` 与 `make linux-only-check` 只存在于已放弃的 `memory-bounded-state` 分支。Linux 侧证据是 CI：冻结 SHA 的 `ci` 运行 39027198445 的 `go` 作业（ubuntu）执行 `go test -count=1 ./...` 与 `go test -race -count=1 -timeout=20m ./...`，结论 success；其余 9 个作业同样 success。CI 输出非 verbose，无法列出跳过的测试。磁盘异常类测试（ENOSPC、只读、慢盘）在 `main` 上不存在，由 E04 补做。

## E09 · 真实 Provider smoke

**BLOCKED**：owner 明确不授权计费调用（方案 §11 第 3 项）。受影响的上游行为变化（#338、#339、#343、#352、#354、#356）只有单元与契约测试证据。
