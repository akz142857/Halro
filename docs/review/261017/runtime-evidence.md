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
| E05 | 4 小时内存长跑 | **PASS**（堆不随历史增长、无时间类泄漏）；第 60 分钟后负载形态因 V-010 改变 |
| E05b | 8 小时内存长跑（假上游，全部到达 Ledger） | **FAIL**：第 7 小时起全部 503，计价被误判时钟回退而永久隔离（V-013）；前 7 小时的内存数据有效 |
| E05d | V-013 修复后在 E05b 终态目录上续跑 2 小时 | **PASS**：0 个 503、隔离始终为 0；四项校验通过；重启 RSS 的构成已测 |
| E06 | 热状态上限触发 | **PASS**（拒绝、收尾、恢复、校验；告警只有规则证据）；实测 V-003 快照峰值 104.4 MiB |
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

## E05 · 内存长跑（Linux 容器，4 h 05 m）

E01 升级后目录的拷贝；封存开（`ledger.seal.enabled=true`，256 MiB），热状态上限为默认 128 MiB，`HALRO_PPROF_LISTEN` 开。负载：32 线程约 142 req/s 的普通请求 + 8 个治理 Key 各每 4 s 一个完整周期（Work Unit → Run → 4 个请求 → 关 Run → Outcome → 关 Work Unit）。每 5 分钟采指标、`/debug/runtime`、VmRSS 与 `heap?gc=1` profile（52 个）。2026-10-18 15:07–19:18 +08。主机上 owner 的开发实例 `halro start` 与两个容器照常运行。

**负载形态在第 60 分钟改变**：每小时一次的主动探测打到不可解析的测试 Provider，deployment 被挂起，此后约 97% 的普通请求在路由层返回 503 `all_candidates_suspended`（V-010）。Ledger 与历史的增长因此降到前一小时的约 1/7。owner 决定保留本轮，不重跑。

| 量 | 值 |
| --- | --- |
| 普通请求 | 586,784 个 502（到达上游失败）+ 1,593,280 个 503（V-010） |
| 治理周期 | 29,383 个，Work Unit/Run/Outcome/两次关闭全部 201；周期内请求 48,172 个 502 + 69,360 个 503 |
| Ledger | 头序号 518,416 → 5,026,771（+450 万条），封存多代并压缩 |
| 数据目录 | 1,567 → 9,463 MiB（前 60 分钟约 1.75 MB/s，之后约 0.25 MB/s） |
| 热状态估算 | 全程 12.0–12.4 MiB，拒绝 0 |
| goroutine | 每次回落到 22（峰值 94，出现在第一小时的封存/导出时刻） |
| `usage_history` 写失败 | 0 |

**堆**（`HeapAlloc`，含未回收垃圾；与 GC 后的存活堆 `heap?gc=1` 分开看）：

| 时段 | HeapAlloc 中位数 / 最大 | GC 后存活堆的底 | RSS 中位数 / 最大 |
| --- | --- | --- | --- |
| 第 1 小时（Ledger 52 万 → 330 万条） | 46 / 113 MiB | 26–36 MiB | 203 / 396 MiB |
| 第 2 小时 | 58 / 179 MiB | 31–36 MiB | 165 / 309 MiB |
| 第 3 小时 | 60 / 197 MiB | 32–35 MiB | 169 / 345 MiB |
| 第 4 小时 | 74 / 187 MiB | 36–39 MiB | 191 / 304 MiB |
| 结束 | 38 MiB | 35.4 MiB | 109 MiB |

- 每小时 :07 的峰值来自每小时一次的 Parquet 导出（`usage.(*Exporter).Export` → `writeParquetAtomic`，zstd 与 parquet-go 缓冲）。导出按 `LastSequence` 增量进行，一次只持有一个 `exportChunkRows = 50000` 行的块（`internal/usage/parquet.go:212`），与历史长度无关；三次峰值时的存活堆为 86 → 65 → 88 MB，没有趋势，`HeapAlloc` 峰值的升高（112 → 178 → 196 MiB）是 GC 时机。
- 结束时对第 1 小时末（`heap-011`）的存活堆差分：**合计 −2.8 MB**。唯一的正项是 bbolt freelist（`freelist.(*array).Allocate` +4.6 MB），随 `history.db` 的空闲页增长；量很小，但本轮没有证明它有上界，记为观察项。
- 第 1 小时 Ledger 增长 280 万条时堆没有跟随。作为对照，P0 基线下旧实现约 4.4 KB 堆/请求（`docs/verification/memory-bounded-baseline.zh-CN.md`），同等请求量约 2 GB。

**收尾**：停机 exit 0；`ledger verify`、`usage verify`、`audit verify` exit 0，`doctor` healthy。重启 10 s 就绪，`from_snapshot=true`、`head_sequence=5026771`，重启后 `HeapAlloc` 30 MiB、热状态 12.0 MiB；同一时刻 RSS 为 516 MiB——远大于堆，推测是 bbolt 对多 GB 数据文件的 mmap 页计入 RSS，本轮**没有单独测量**。

结论 **PASS**：堆不随历史增长（第 1 小时满负载下得到验证），4 小时内没有时间类泄漏；限制是后 3 小时的写入负载只有前 1 小时的约 1/7（V-010），以及 bbolt freelist 的上界与重启后 RSS 的构成未测。

## E05b · 8 小时内存长跑（Linux 容器，假上游）

E05 的负载形态被 V-010 改变，且 Provider 不可达、请求不写满 Ledger，所以重做：E01 升级后目录的拷贝，Provider 经 Admin API 切到同一 Docker 网络内的假上游 `https://fake-upstream:8443`（自签 CA，`security.allow_private_provider_endpoints=true`），每个请求都 200 并结算到 Ledger。二进制为 `main` + V-010 + V-012 修复（`356fe514`）。负载与 E05 相同（32 线程约 150 req/s 普通请求 + 8 个治理 Key 的完整周期），每 5 分钟采指标、`/proc` 的 RssAnon/RssFile 与 `heap?gc=1`。2026-10-19 00:31–08:36 +08，主机除 owner 的两个容器外空闲。

**结果：FAIL（V-013）**。前 7 小时全部正常；23:33:00Z（第 7 小时 01 分）起每个请求都返回 503 `accounting_unavailable`，直到结束。

| 量 | 值 |
| --- | --- |
| 普通请求 | 3,712,079 个 200、3 个 502（假上游）、**562,134 个 503** |
| 治理 | 58,089 个周期全部 201；周期内请求 201,900 个 200、1 个 502、**30,455 个 503** |
| Ledger | 头序号 518,666 → 21,506,115（+2,099 万条），93 个文件 |
| 数据目录 | 47,301 MiB；`history.db` 21.7 GB，`halro.db` 2.9 GB |
| 热状态估算 | 全程 12.0–12.4 MiB，拒绝 0；`usage_history` 写失败 0 |

故障前几分钟吞吐已从约 8.9k 降到 4.5k 请求/分钟，第 7 代 Ledger 段压缩在首个失败前 6 s 完成。长跑后的 `halro.db` 里唯一 deployment 的高水位为 `quarantined: true, quarantine_reason: wall_clock_rollback, latest_selected_at: 23:33:00.944Z`；`pmset` 显示主机这段时间没有睡眠或唤醒。机制与确定性复现见 `findings/V-013`。收尾：`ledger/usage/audit verify` exit 0；`doctor` exit 1（计价隔离）；重启后 `/health/ready` 一直不通过（脚本等满 1200 s；当时日志里的"ready after 1242 s"是放弃时间，脚本已改正）。

**内存（前 7 小时有效）**：

| 小时 | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| GC 后存活堆（MB） | 25 | 40 | 51 | 56 | 64 | 64 | 91 | 89 |
| 整点 RssAnon 低点（MiB） | 47 | 86 | 122 | 126 | 148 | 158 | 231 | 265 |

第 1 小时末（`heap-011`）对第 7 小时末（`heap-083`）的存活堆差分 +44 MB，几乎全部在 bbolt `freelist.(*array).Allocate`（调用路径 `usage.(*HistoryStore).commit`），即 `history.db` 的空闲页表随文件增长；Halro 自身状态没有增长。

> 后续（2026-10-20）：代码与确定性测试确认，增长的是 bbolt freelist 里的 `allocs` map——每复用一页空闲页登记一条，只有该页再次被释放才删除；只追加为主的文件很少再释放，于是随进程启动以来的写入量增长，重启即归零，并不是空闲页表本身随文件增长。修复是每小时重开 `history.db` 与 `archive.db`（`internal/boltreopen`，见 CHANGELOG [Unreleased]）。RssFile 随每小时导出起落，低点降到约 90 MiB，没有趋势。

## E05d · V-013 修复后续跑（Linux 容器，2 小时）

在 E05b 的终态目录上继续（已处于磁盘压力下），二进制为 #434 的 `ca074457`。启动前在 Halro 停机时用一次性离线工具解除 E05b 留下的隔离——产品内没有解除途径（V-014）——改写前后的记录在日志里（revision 4016864 → 4016865）。负载与 E05b 相同，2026-10-19 11:17–13:18 +08。

| 量 | 值 |
| --- | --- |
| 普通请求 | 1,062,500 个 200、3 个 502（假上游）、**0 个 503** |
| 治理 | 14,392 个周期全部 201；周期内请求 57,568 个 200 |
| 计价隔离 | 26 次采样全部 `halro_pricing_quarantined_deployments 0` |
| Ledger | 头序号 21,506,115 → 27,164,038；数据目录 47,301 → 60,366 MiB |
| GC 后存活堆 | 31–62 MB；第 1 次对第 23 次采样差分 +17 MB，其中 bbolt freelist 0.5 → 9.7 MB |

收尾：停机 exit 0；`ledger/usage/audit verify` exit 0，`doctor` healthy（只有 `source_rate_limit` 为 warn）；重启 42 s 就绪，`from_snapshot=true`。

**重启后 RSS 的构成**（E05 的 516 MiB、E05d 的 2.0 GiB 都只记了 VmRSS）：另做一次重启，18 s 就绪后读 `/proc/<pid>/status`：

| 就绪后 | VmRSS | RssAnon | RssFile |
| --- | --- | --- | --- |
| 0 s | 3,118 MiB | 58 MiB | 3,060 MiB |
| 30 s | 85 MiB | 65 MiB | 20 MiB |
| 60 s | 106 MiB | 97 MiB | 9 MiB |
| 120 s | 70 MiB | 65 MiB | 5 MiB |

重启后的大 RSS 是启动时触及的 bbolt mmap 文件页，可回收，30 s 内即被内核收回；进程自身的匿名内存约 60–100 MiB。

## E06 · 热状态上限触发（Linux 容器）

E01 升级后目录的拷贝，`ledger.max_hot_state_bytes=16777216`（配置允许的最小值），基线估算 12,630,944 B。8 个治理 Key 各开 Work Unit，在其下反复创建并关闭 Run，满 32 个 Run（`domain.MaxRunsPerWorkUnit`）换一个新 Work Unit、旧的保持打开，遇 `governance_rate_limited` 按 Retry-After 退避。

| 阶段 | 结果 |
| --- | --- |
| 压到上限（418 s） | 96 个 Work Unit、3,026 个 Run 创建并关闭全部 201；估算到 16,777,984 B 后 Run 创建返回 **503 `accounting_capacity_exhausted`**（6 次，`rejections_total{operation="run"}=6`） |
| 上限处的新网关请求 | **503**，`Retry-After: 5`，`accounting_capacity_exhausted`（`operation="request"` +1） |
| 上限处的结算/关闭 | 已准入的 Run 关闭与 96 个 Work Unit 关闭全部 201——拒绝只落在新准入上，不阻断收尾 |
| 上限附近的快照（70 s、200 ms 采样） | HeapAlloc 最低 26.8 MiB、**峰值 104.4 MiB**，快照 15,048,719 B，失败 0——即 V-003 在真实二进制上的实测（约 6.2 倍） |
| 释放（关闭 Work Unit 后 8 s） | 估算回落到 12,827,776 B；100 个新请求全部到达上游（502，即不可解析的 Provider），不再 503 |
| 停机后 | `ledger verify` exit 0，`doctor` healthy |
| 告警 | 容器内没有 Prometheus，未实际触发；`alert-rules.yml:116`（估算/上限 > 0.8）与 `:123`（10 分钟内拒绝 > 0）按实测值都会成立，规则有 promtool 单元测试 |

结论 **PASS**（告警只有规则与单元测试证据）。前两次运行无效、日志保留：第一次驱动只用一个 Work Unit，32 个 Run 后全部 429，估算只到 12.98 MB（`e06-run1-runcap.log`）；第二次把治理写限流（每 Key 120/min、每 Project 1,000/min）当成停止条件，6.5 s 即停（`e06-run2-ratelimit.log`）。两者都是驱动缺陷，不是产品问题。

## E07 · 有规模数据的备份/恢复（候选版本）

`backup create`（exit 0）→ `backup verify`（exit 0）→ `backup restore` 到新目录（exit 0；恢复后只有 `history.db`，没有 `archive.db`，符合设计）→ 启动 10.81 s 就绪，`from_snapshot=false`（备份不带 `state.snap`），`archive.db` 重建 → 读出的 1,080 个 Work Unit 与预算占用和 E01 第三次启动**完全一致** → `ledger verify`、`usage verify`、`audit verify`、`doctor` 全部通过。

## E08 · Linux 上的全量测试

`main` 上不存在 `_linux_test.go` 或 `//go:build linux` 的测试（只有 `//go:build linux || darwin` 的 `internal/hostsecurity/harden_test.go`）；`scripts/linux-only-tests.sh` 与 `make linux-only-check` 只存在于已放弃的 `memory-bounded-state` 分支。Linux 侧证据是 CI：冻结 SHA 的 `ci` 运行 39027198445 的 `go` 作业（ubuntu）执行 `go test -count=1 ./...` 与 `go test -race -count=1 -timeout=20m ./...`，结论 success；其余 9 个作业同样 success。CI 输出非 verbose，无法列出跳过的测试。磁盘异常类测试（ENOSPC、只读、慢盘）在 `main` 上不存在，由 E04 补做。

## E09 · 真实 Provider smoke

**BLOCKED**：owner 明确不授权计费调用（方案 §11 第 3 项）。受影响的上游行为变化（#338、#339、#343、#352、#354、#356）只有单元与契约测试证据。
