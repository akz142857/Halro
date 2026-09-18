# SRE role report — Halro production validation plan (§5, G4, G5)

- Candidate SHA under review: `f09ed2d768bf2cd335478223beb4d57af326013b`
- Plan under execution: `docs/verification/production-validation-plan.zh-CN.md`
- Mode: **read-only survey**. No repo file was edited, no binary built, no Halro instance started, no network call made.
- Only command executed against the tree: `make observability-check` → **exit code 0** (`deploy/observability/validate.sh`; reported `13` recording rules, `35` alert rules, Alertmanager config valid with 2 inhibit rules / 2 receivers).

**Standing caveat for the whole document.** Everything below is E1/E2 evidence mined from the repository. Nothing here is a measured result from a target environment. Where this report proposes a threshold for §5, that is a *proposal for four-party signature*, not an observation — the plan (`docs/verification/production-validation-plan.zh-CN.md:78`) requires §5 be filled **before** the first measurement, which is exactly what these proposals are for.

---

## 1. §5 预先填写的服务目标 — proposed fills

Evidence grades follow the plan's own ladder (`production-validation-plan.zh-CN.md:29-34`): E1 doc/static, E2 repo test/contract, E3 local or CI end-to-end, E4 target environment.

| 指标 | 建议值 (proposed, for signature) | 依据 (path:line / rule) | 证据等级 | 负责人 |
|---|---|---|---|---|
| 同步请求吞吐与 p50/p95/p99 | **吞吐**: 门禁不设绝对值；以 `BenchmarkRequestLifecycle` 在目标主机的实测值为准入基线，并要求 ≥ 参考 Linux/NVMe 主机的 50% (参考值 64 workers ≈ **7,051 lifecycles/s**, 1 worker ≈ **1,021/s**)。**延迟**: p50 ≤ **250 ms**, p95 ≤ **1000 ms**, p99 ≤ **2500 ms** — 必须落在直方图桶边界上，见下方限制说明 | 吞吐参考: `docs/verification/standalone-capacity-baseline.md:110-116`(Linux/NVMe), `:99-108`(主机规格); darwin floor `docs/verification/performance-baseline.md:155`; 延迟桶边界: `internal/usage/aggregate.go:25-27` | 吞吐 E3(参考主机基准，非本候选 SHA)；延迟 **E1 提案** | Application / SRE |
| 流式首字节与完整响应 p95/p99 | **首字节 (TTFB): NO BASIS — 需四方签署临时阈值，且当前不可测量**。仓库没有任何首字节指标。完整响应: 受 `stream_max_duration` 上限约束，建议 p95 ≤ **10 s**, p99 ≤ **30 s**(桶边界)，硬上限 `600 s` | 无 TTFB 指标: `internal/app/metrics.go` 全文无 first_byte/ttfb 系列；导出清单 `docs/contracts/metrics-reference.md:160-164`；硬上限 `gateway.stream_max_duration: 10m0s` at `internal/config/default.yaml:165`；桶 `internal/usage/aggregate.go:25-27` | TTFB **NO BASIS**；完整响应 E1 提案 | Application / SRE |
| 允许错误率、超时率和限流率 | **错误率 ≤ 5%**(与既有告警阈值一致，浸泡期收紧至 **≤ 1%**)。**回退率 ≤ 25%**。**超时率 / 限流率(429): NO BASIS — 需四方签署临时阈值**（无独立指标，无告警规则） | 5%: `deploy/observability/prometheus/alert-rules.yml:142` (`HalroHighErrorRate`, `halro:requests_errors:ratio5m > 0.05`, `for: 10m`, 且 `increase(halro_requests_total[10m]) >= 20`); 1%: 浸泡门禁 `tests/soak/main.go:361` (`RequestFailurePct: 1`), `docs/verification/soak-testing.md:41`; 25%: `alert-rules.yml:105` (`HalroFallbackSaturation`) | E2 (阈值已在规则与门禁中固化)；超时/限流 **NO BASIS** | Application / SRE |
| CPU、RSS/heap、goroutine、FD 上限 | **goroutine 增长 ≤ +20**、**FD 增长 ≤ +20**(24 h 起止差)。**RSS 增长 ≤ max(64 MiB, 起始值的 25%)**。**绝对 RSS 上限**: 建议 ≥ **1.0 GiB** 容器上限(= Argon2 峰值 256 MiB + 1,000 路 SSE ≈ 77 MiB + Go 运行时/缓冲余量)。**绝对 CPU/FD 上限: NO BASIS — 需四方签署临时阈值** | +20/+20/RSS: `tests/soak/main.go:361`, `docs/verification/soak-testing.md:38-41`; 压测侧 +40 容差 `tests/stress/stream_test.go:154,159`; SSE 每连接 36.1 KiB heap / 77.0 KiB maxRSS: `docs/verification/performance-baseline.md:110-113`; Argon2 64 并发登录峰值 256 MiB: `performance-baseline.md:181-184`, 进程级 2 槽 ≈128 MiB: `:171-176` | 增长门禁 E2；绝对上限 **部分 NO BASIS** | SRE |
| 队列、重试、failure capture 和 WAL 上限 | **WAL 队列**: 容量 4096，峰值占用 ≤ **75%**，终态深度 = **0**，`wal_append_errors` 增量 = **0**。**分析队列**: 容量 4096，终态非 lagging。**告警队列**: 容量 1024 / 2 workers。**重试**: 每目标 ≤ 2 次，全链路 ≤ **3 次尝试**，退避 100 ms→2 s；熔断 5 连败 / 开启 30 s。**Failure capture**: ≤ 64 KiB/条、≤ 1000 条/天、24 h 过期 ⇒ 稳态磁盘上限 **≈ 62.5 MiB**。**WAL 单帧** ≤ 1 MiB；**活动段** ≤ 8 GiB(需显式开启 sealing) | 队列: `internal/config/default.yaml:112-115`, 门禁 `tests/soak/main.go:361`, 告警 `alert-rules.yml:77` (`halro:ledger_queue:ratio > 0.75`, `for: 5m`); 告警队列 `default.yaml:219-227`; 重试 `default.yaml:209-213` + `gateway.max_total_attempts: 3` `default.yaml:168`; 熔断 `default.yaml:215-218`; failure capture `internal/config/config.go:486-488` + `default.yaml:204-208` (上限校验 1 KiB–1 MiB / 1–1,000,000 / 1 h–720 h at `config.go:1399-1410`); WAL 帧 `internal/ledger/log.go:52`; 段 `internal/config/config.go:318` + `default.yaml:153-156` | E2 (全部为代码中生效的硬上限) | Application / SRE |
| Ledger、Audit、Parquet、TSDB 增长预算 | **TSDB**: 警告 **3.5 GiB**、严重 **4.25 GiB**，规划卷 5 GiB / 7 天保留。**Ledger/Audit/Parquet 每日字节数: NO BASIS — 需四方签署临时阈值**（必须在 G5 浸泡中实测 `bytes/series/day` 与每事件字节数后回填）。可用的结构性约束: Parquet 分区保留 **90 天**、导出周期 1 h、checkpoint 1 min；sealed 段压缩 ≈5.6× | TSDB: `alert-rules.yml:211-224` (`PrometheusTSDBDiskHigh` `>3758096384` `for:10m`; `PrometheusTSDBDiskCritical` `>4563402752` `for:5m`), 卷/保留假设 `docs/observability/capacity-model.md:33-39`; 待测占位 `capacity-model.md:41-51` ("measure in soak"); 保留/周期 `internal/config/default.yaml:117-122`; 压缩比 `default.yaml:149` | TSDB E1(模板阈值，非本环境实测)；应用侧 **NO BASIS** | SRE / Platform |
| Provider 费用和 token 预算 | **NO BASIS — 需四方签署临时阈值**。机制存在但无任何默认值或告警：每 Project 有 `daily_budget_micros_usd` / `RPM` / `TPM` / `max_concurrency`，`halro_cost_usd_total` 与 `halro_tokens_total` 已导出，但**没有任何费用或 token 相关的告警规则**。浸泡专用建议(非业务 SLO): 专用 Project + 显式日预算，默认 10 s 间隔 ⇒ 24 h ≤ **8,640** 次小请求 | 字段: `internal/domain/models.go:345-348`; 指标: `internal/app/metrics.go:100-110`; 无费用告警: `deploy/observability/prometheus/alert-rules.yml` 全文无 cost/token 规则; 浸泡请求上界: `docs/verification/soak-testing.md:27` | **NO BASIS**(浸泡上界 E2) | Product / Application |
| RPO / RTO | **RPO**: 目标 **0**(已提交事务)—— Ledger WAL 是记账权威且预留在 Provider 调用前落盘；`usage.durability: balanced` 下批量写入意味着崩溃可能丢失未 fsync 的尾部，**若签署 RPO=0 必须要求 `durability: strict`**。**RTO**(仅进程重启，非备份恢复): 参考主机 10 GiB WAL、近 1 MiB 帧剖面 **68.578 s**；建议签署 **RTO ≤ 15 min**（含备份恢复、校验与就绪检查），**恢复演练实测值为 NO BASIS，必须在 G4.6 目标环境测量** | RPO 机制: `CLAUDE.md` 不变量 + `internal/config/default.yaml:103-106`; RTO 参考: `docs/verification/performance-baseline.md:68-92` (`TestTenGiBWALRecoveryProfile`, `internal/ledger/rto_test.go:15`); 文档明确该值"不是通用界"且要求按自身 WAL 分布测量 `performance-baseline.md:87-92`；全仓库 `docs/` 无任何 RPO/RTO 目标值(grep 零命中) | RTO 参考值 E3(opt-in 测试，参考主机)；**目标值 NO BASIS** | SRE / Platform |
| 24 小时浸泡允许事件预算 | **硬性 0 事件**(以下任一即失败): WAL append error 增量 > 0；终态 WAL 队列 ≠ 0；峰值 WAL 队列 > 75% 容量；分析持续 lagging；无任何请求完成；请求失败率 > 1%；RSS 增长 > max(64 MiB, 25%)；goroutine 或 FD 增长 > 20。**外加人工判读**: 即便落在容差内，单调无界增长仍是发布阻断项。**critical 告警允许数 = 0；warning 告警允许数: NO BASIS — 需四方签署临时阈值** | 全部来自 `tests/soak/main.go:361-386` 与 `docs/verification/soak-testing.md:38-45`；单调增长条款 `soak-testing.md:42-45` | E2 (门禁已在代码中) | 四方签署 |

### Hard measurability constraints that must be signed off with §5

These are not opinions; they are properties of the shipped instrumentation and they bound what §5 can even ask for.

1. **Latency percentiles are quantized to 12 bucket edges.** `internal/usage/aggregate.go:25-27` fixes the bounds at `10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 120000` ms. `internal/usage/rollup.go:277-296` (`ApproximateLatencyPercentile`) returns *the upper bound of the bucket the percentile lands in*, and its own comment says the truthful statement above the last bound is "greater than 120 s". **A signed p95/p99 that is not one of those twelve values cannot be evaluated against Halro's own exposition.** Every latency target in the table above was chosen to land on a bucket edge for that reason.
2. **There is no time-to-first-byte metric.** `internal/app/metrics.go` exports `halro_request_duration_seconds` (summary), `halro_request_latency_seconds` and `halro_attempt_latency_seconds` (12-bucket classic histograms) — nothing for stream first byte. `docs/contracts/metrics-reference.md:160-164` confirms the process-level inventory. G5's requirement to record 流式首字节 p95/p99 (`production-validation-plan.zh-CN.md:84,180`) **cannot be satisfied from the gateway's own metrics** and needs either client-side measurement in the load harness or new instrumentation.
3. **Halro exports no CPU, RSS or FD metric.** Only `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_gc_cycles_total`, `process_start_time_seconds` (`internal/app/metrics.go:245-254`; `docs/contracts/metrics-reference.md:160-164`). There is no `process_resident_memory_bytes`, no `process_open_fds`, no `process_cpu_seconds_total`. `tests/soak/main.go:310-350` gets RSS and FD counts out-of-band from `/proc/<pid>/status`, `ps`, `/proc/<pid>/fd` or `lsof`. **So the §5 CPU/RSS/FD row can be alerted on only via node_exporter/cAdvisor in the target environment (E4), not from Halro.** No alert rule in `alert-rules.yml` covers CPU, RSS or FD today.
4. **The only throughput numbers in the repo are microbenchmarks on two reference hosts**, explicitly disclaimed as non-transferable: `standalone-capacity-baseline.md:41-43` ("These absolute numbers do not transfer to a Linux NVMe host"), `:20` ("Host-specific throughput numbers are not CI pass thresholds"), `performance-baseline.md:149-151` ("every throughput figure here is a floor, not a ceiling"). None is attributable to `f09ed2d`.
5. **No end-to-end request-latency measurement exists anywhere in the repo.** `performance-baseline.md` is entirely microbenchmarks (route resolution, redaction, token guard, WAL replay). The `docs/verification/performance/` evidence directories (`2026-09-03-v0.6.0`, `2026-09-04-run-governance-s0`, `2026-09-06-v0.7.0`) are benchstat comparisons, not gateway latency distributions.

---

## 2. G4.1–G4.3 — alerting closure

`make observability-check` = **exit 0**. `deploy/observability/runbook_links_test.go:12` (`TestEveryAlertHasAResolvableRunbook`) already enforces that every alert has a `runbook_url` under `/docs/`, and that alerts pointing at the operations runbook use anchor `strings.ToLower(alertname)` backed by a `### <AlertName>` heading.

I independently re-checked all 35 rules — resolving each `runbook_url` file *and* verifying the anchor against every markdown heading slug in the target file.

**Result: 0 broken runbook links. All 35 resolve; all 29 anchored links resolve to an existing heading.**

| # | Alert (file:line) | Threshold / expression | `for` | severity | runbook_url | link |
|---|---|---|---|---|---|---|
| 1 | `HalroTargetDown` (alert-rules.yml:5) | `up{job="halro",expected_target="true"} == 0` or absent | 2m | critical | operations-runbook.md#halrotargetdown | OK |
| 2 | `HalroConfigurationStale` (:12) | `halro_activation_stale == 1` | 1m | critical | /docs/runbooks/configuration-stale.md | OK (file) |
| 3 | `HalroAuditAnchorStale` (:19) | silence > `3 × halro_audit_anchor_interval_seconds` | 2m | critical | operations-runbook.md#halroauditanchorstale | OK |
| 4 | `HalroDeploymentCapabilityEvidenceDegraded` (:40) | absent(...) or `state="conflicting" > 0` | — | warning | ...#halrodeploymentcapabilityevidencedegraded | OK |
| 5 | `HalroWALAppendErrors` (:46) | `increase(halro_wal_append_errors_total[5m]) > 0` | 0m | critical | ...#halrowalappenderrors | OK |
| 6 | `HalroShutdownTruncatedAttempts` (:53) | `increase(...[10m]) > 0` | — | critical | ...#halroshutdowntruncatedattempts | OK |
| 7 | `HalroAccountingLeaseStale` (:59) | oldest lease age > configured normal max | 2m | critical | ...#halroaccountingleasestale | OK |
| 8 | `HalroUsageAnalyticsLagging` (:69) | `halro_usage_analytics_lagging == 1` | 10m | warning | ...#halrousageanalyticslagging | OK |
| 9 | `HalroLedgerQueueHigh` (:76) | `halro:ledger_queue:ratio > 0.75` | 5m | warning | ...#halroledgerqueuehigh | OK |
| 10 | `HalroAlertDeliveryFailing` (:83) | `increase(...{status=~"failed\|dropped"}[10m]) > 0` | 10m | warning | ...#halroalertdeliveryfailing | OK |
| 11 | `HalroDeploymentUnhealthy` (:90) | `halro_deployment_up == 0` | 5m | warning | ...#halrodeploymentunhealthy | OK |
| 12 | `HalroMultipleDeploymentsUnhealthy` (:97) | `halro:deployments_unhealthy:count >= 2` | 3m | critical | ...#halromultipledeploymentsunhealthy | OK |
| 13 | `HalroFallbackSaturation` (:104) | `halro:fallbacks:ratio5m > 0.25` and ≥20 req/10m | 5m | warning | ...#halrofallbacksaturation | OK |
| 14 | `HalroProviderCapacityPressure` (:111) | `halro:provider_capacity:ratio > 0.85` | 5m | warning | ...#halroprovidercapacitypressure | OK |
| 15 | `HalroSignedModelCatalogDegraded` (:118) | `halro_signed_model_catalog_degraded == 1` | 15m | warning | ...#halrosignedmodelcatalogdegraded | OK |
| 16 | `HalroCapabilityDriftDetected` (:125) | `increase(halro_capability_drift_total[1h]) > 0` | — | warning | ...#halrocapabilitydriftdetected | OK |
| 17 | `HalroCapabilityDetectionFailureRateHigh` (:131) | ratio1h > 0.5 and ≥5 detections/1h | — | warning | ...#halrocapabilitydetectionfailureratehigh | OK |
| 18 | `HalroHighErrorRate` (:141) | `halro:requests_errors:ratio5m > 0.05` and ≥20 req/10m | 10m | warning | ...#halrohigherrorrate | OK |
| 19 | `HalroKMSPrimaryUnlockFailure` (:148) | `increase(halro_kms_calls_total{operation="unwrap",status="error"}[5m]) > 0` | — | critical | /docs/runbooks/m11-production-operations.md | OK (file) |
| 20 | `HalroKMSRecoveryNotReady` (:154) | `halro_kms_recovery_ready == 0` | 5m | critical | m11-production-operations.md | OK (file) |
| 21 | `HalroKMSRecoveryVerificationExpired` (:161) | verified older than `7776000 s` (90 d) | 5m | warning | m11-production-operations.md | OK (file) |
| 22 | `HalroKMSRecoveryUsed` (:168) | used within last `900 s` | — | critical | m11-production-operations.md | OK (file) |
| 23 | `HalroKMSVaultMismatch` (:174) | `increase(...{error_class="kms_vault_mismatch"}[5m]) > 0` | — | critical | m11-production-operations.md | OK (file) |
| 24 | `HalroKMSPendingRotation` (:180) | `halro_kms_pending_rotation_slots > 0` | 15m | warning | m11-production-operations.md | OK (file) |
| 25 | `Watchdog` (:187) | `vector(1)` | — | none | operations-runbook.md#watchdog | OK |
| 26 | `AlertmanagerNotificationFailing` (:196) | `increase(alertmanager_notifications_failed_total[5m]) > 0` | 2m | critical | ...#alertmanagernotificationfailing | OK |
| 27 | `PrometheusRuleEvaluationFailing` (:201) | `increase(prometheus_rule_evaluation_failures_total[5m]) > 0` | 2m | warning | ...#prometheusruleevaluationfailing | OK |
| 28 | `PrometheusConfigReloadFailing` (:206) | `prometheus_config_last_reload_successful == 0` | 2m | critical | ...#prometheusconfigreloadfailing | OK |
| 29 | `PrometheusTSDBDiskHigh` (:211) | blocks+WAL > `3758096384` B (3.5 GiB) | 10m | warning | ...#prometheustsdbdiskhigh | OK |
| 30 | `PrometheusTSDBDiskCritical` (:218) | blocks+WAL > `4563402752` B (4.25 GiB) | 5m | critical | ...#prometheustsdbdiskcritical | OK |
| 31 | `HalroWALSyncSlow` (:228) | `halro:wal_sync:mean5m > 0.05` s | 10m | warning | ...#halrowalsyncslow | OK |
| 32 | `HalroAccountingProjectLockSaturated` (:235) | `halro:accounting_project_lock_wait:mean5m > 0.25` s | 15m | warning | ...#halroaccountingprojectlocksaturated | OK |
| 33 | `HalroTLSCertificateExpiringSoon` (:245) | expiry − now < `2592000` s (30 d) | 15m | warning | ...#halrotlscertificateexpiringsoon | OK |
| 34 | `HalroTLSCertificateExpired` (:252) | expiry − now ≤ 0 | 5m | critical | ...#halrotlscertificateexpired | OK |
| 35 | `HalroReloadFailing` (:263) | `increase(halro_reload_total{status="error"}[15m]) > 0` | 5m | warning | ...#halroreloadfailing | OK |

**Severity split**: 14 critical, 20 warning, 1 `none` (`Watchdog`).

### Observations on alert coverage (not defects, but G4 inputs)

- **Two thresholds are self-declared as non-universal.** `alert-rules.yml:225-227` says the WAL-sync and project-lock thresholds "are starting points, not universal truths … re-baseline them per host from `halro:wal_sync:mean5m`". G1/G5 must re-baseline `HalroWALSyncSlow` (0.05 s) and `HalroAccountingProjectLockSaturated` (0.25 s) on the target host before they count as pass criteria.
- **The TSDB thresholds are absolute byte values tied to an assumed 5 GiB local volume** (`capacity-model.md:36-39`), and that document says production "must either provision a real 5 GiB bound or replace the expression with filesystem capacity metrics". If the target environment's TSDB volume is not 5 GiB, rules 29–30 are wrong in the target environment.
- **`HalroWALAppendErrors` has `for: 0m`** (`:48`) — correct for an accounting-integrity tripwire, worth noting for the notification-lifecycle drill because it will fire on the first scrape after injection.
- **Alertmanager routing**: `deploy/observability/alertmanager/alertmanager.yml:4-15` — default receiver `operations-webhook` (`group_wait 30s`, `group_interval 5m`, `repeat_interval 4h`, `send_resolved: true`); `Watchdog` routes to `independent-deadman-webhook` with `group_wait 0s / group_interval 1m / repeat_interval 1m` and **`send_resolved: false`** — the comment at `:33-36` is explicit that Prometheus disappearing must produce silence, not a synthetic resolved. Both receivers read their URL from `url_file` under `/run/secrets/`, so no webhook URL is in the repo. Two inhibit rules (`:17-23`).
- **G4.1 "notification contains no secret canary" is not provable here.** The repo has canary scanning for the frontend bundle and metrics exposition, but the notification payload check is a target-environment observation (E4).

---

## 3. Dead-man monitor — what is code-verifiable vs. deployment property

### What it checks (`internal/deadman/`)

Three target kinds are **mandatory** — config validation refuses to start unless exactly one each of `halro`, `prometheus`, `alertmanager` is present (`internal/deadman/config.go:135-136,173-177`).

- **Readiness probe per target** over HTTPS only, with per-target CA and either a bearer-token file or mTLS client cert (`config.go:181-202`; `validateHTTPS` refuses non-https, empty host, or userinfo). Redirects are forbidden and the bearer is presented per request — `TestSecureClientUsesBearerAndForbidsRedirect` (`deadman_test.go:188`), `TestMutualTLSIsPresented` (`:385`).
- **Prometheus sample freshness**, mandatory on a `prometheus` target (`config.go:158-160`): mode `prometheus_scalar_age` only, with a `max_age`. The example query is `time()-scalar(max(timestamp(up{job="halro"} == 1)))` with `max_age: 2m` (`deploy/observability/external-probe/config.example.yaml:48-54`). Non-finite and stale scalars are failures — `TestPrometheusFreshnessRejectsStaleScalar` (`deadman_test.go:402`), `TestPrometheusFreshnessRejectsNonFiniteScalar` (`:421`).
- **Audit-anchor pulling** from the Halro target (ADR 0015), incremental by `LastAnchorSequence` so a restart cannot replay a stale anchor (`types.go:23-27`). Pulling is deliberately fail-open so it cannot stall a heartbeat, and every tick is audited so "gone quiet" is still visible (`engine.go:147-160`). Rewind and gap detection: `TestAnchorSequenceRewindIsReported` / `TestAnchorSequenceGapIsReported` (`anchor_health_test.go:76,112`).

### What it notifies

- `heartbeat` every tick, queued **before** the probes run so a slow probe cannot delay it (`engine.go:131-142`), carrying an explicit `HeartbeatTTL` (`types.go:48`).
- `state_transition` with hysteresis: `failures_to_down` consecutive failures to go down, `successes_to_up` to come back (`engine.go:220-250`). `pending_*` is never sent; `down` → `firing`, `up` → `resolved`, idempotent by event ID (`RECEIVER-CONTRACT.md:32-34`).
- `anchor_status` on every change of the anchor problem string (`engine.go:165-175`).
- Delivery is a durable outbox written before sending, bounded `16..65536`, rolled back on save failure (`config.go:126-128`; `TestDurableEnqueueCommitsBeforeDeliveryAndRollsBackOnSaveFailure` `deadman_test.go:93`; `TestStateLoadRejectsOversizeAndOutboxBeyondConfiguredLimit` `:141`).
- Config floors: `interval ≥ 10s`, `timeout < interval`, `heartbeat_ttl ≥ 2 × interval` (`config.go:114-122`). The shipped example is `interval 30s / timeout 5s / heartbeat_ttl 90s / failures_to_down 3 / successes_to_up 2` (`config.example.yaml:7-11`) ⇒ **detection window ≈ 90 s of probe failure before `down` is emitted**, which is the number G4.3 should predeclare.

### Verifiable in code/tests (E2) vs. REQUIRES-E4 deployment property

**Verifiable in the repo (E2):** hysteresis and no-false-recovery on first observation (`deadman_test.go:28,48,55`); durable state and corruption/sequence rejection (`:123,141,165`); bounded, coalesced heartbeat outbox (`:72`); retry delay never exceeding max (`:250`); probes continue while delivery retries (`:264`); a slow receiver does not block a probe tick and a slow probe does not block heartbeat delivery (`:482,584`) — these two are the *concurrency* half of independence, and they are real tests; HTTPS-only + no-redirect + bearer/mTLS enforcement; the payload schema (`event.schema.json`) and `-check-config` validation (`cmd/halro-deadman/main.go:27,35-37`); credential scrubbing in its own logs via `safelog` (`main.go:38-40`).

**REQUIRES-E4 — deployment properties no test in this repo can establish:**

1. The probe runs on a **different host / failure domain** from Halro, Prometheus and Alertmanager. `deploy/observability/README.md:109-110`: "Install the probe on a different host or failure domain, never beside the Core stack as production evidence."
2. The **receiver** is independent: durable acknowledgement, TTL alarm, replay rejection, recovery, separate credential and Contact Point. `RECEIVER-CONTRACT.md:39-41`: "Production admission must demonstrate … This repository contract is not that evidence."
3. The **notification identity and final Contact Point are not reused from Alertmanager** (`README.md:112-114`).
4. The **network path** from probe to targets and to the receiver does not share the Core stack's fate.
5. The Alertmanager `Watchdog` receiver's grace window — "the local baseline is three minutes" (`README.md:122-123`) — must be an approved production value, and must emit both missing-heartbeat firing and recovery.

The repo states this boundary itself, unusually plainly: `README.md:128-130` — "Repository tests prove state transitions, persistence, retry, TLS/authentication and payload contracts. **They do not prove failure-domain independence.**" That sentence is the honest answer to plan item G4.2, and it means **G4.2 cannot be closed from this repository under any amount of local testing.**

---

## 4. G4.4–G4.5 — crash recovery matrix → automated test mapping

`docs/verification/crash-recovery-matrix.md` has **19 rows**. I resolved every named test to a `func Test...` definition; **all 21 named tests exist** (two rows name multiple tests). The matrix's own framing is honest about its level: `crash-recovery-matrix.md:5` — "exercised by package and integration tests, **not by fault injection in a production host**" (i.e. E2, not E4).

| Matrix row (line) | Automated test → location |
|---|---|
| Stop after reservation, before settlement (:9) | `TestPendingReservationSurvivesReopen` — `internal/ledger/log_test.go:347` |
| WAL stops at every byte offset (:10) | `TestCrashRecoveryAcrossEveryByteTruncationPoint` — `internal/ledger/log_test.go:402` |
| 10,000 random WAL crash cuts (:11) | `TestTenThousandRandomCrashInjectionsRecoverCompleteRecordsWithoutDuplicateEventIDs` — `internal/ledger/log_test.go:460` |
| WAL write ENOSPC / partial-write EIO (:12) | `TestWriteAndSyncFailuresMakeAccountingUnavailable` — `internal/ledger/log_test.go:234` (cases at `:241`) |
| WAL fsync EIO → 503 before Provider call (:13) | `TestDurabilityFailurePreventsCurrentAndFutureProviderCalls` — `internal/gateway/service_test.go:638` |
| WAL committed bytes modified (:14) | `TestChecksumCorruptionRequiresRecovery` — `internal/ledger/log_test.go:515` |
| Audit final record partial (:15) | `TestOpenTruncatesOnlyPartialAuditTail` — `internal/audit/log_test.go:212` |
| Audit committed record/key modified (:16) | `TestAuditDetectsTamperingAndWrongKey` — `internal/audit/log_test.go:180` |
| Usage checkpoint absent (:17) | `TestDeletingUsageCheckpointRebuildsIdenticalAggregateFromLedger` — `internal/app/checkpoint_test.go:20` |
| Stop at 126 checkpoint boundaries (:18) | `TestCheckpointRecoveryMatchesFullReplayAcrossOneHundredKillPoints` — `internal/usage/aggregate_test.go:15` |
| Checkpoint moves behind/ahead (:19) | `TestCheckpointWatermarkRejectsAlreadyAggregatedLedgerPrefix` — `internal/app/checkpoint_test.go:173`; `TestUsageCheckpointAheadOfLedgerHeadIsDiscarded` — `internal/app/ledger_chain_checkpoint_test.go:86`; `TestUsageCheckpointPersistenceAndMonotonicity` — `internal/store/bolt/store_test.go:807` |
| Parquet partition modified (:20) | `TestExporterDetectsParquetTampering` — `internal/usage/parquet_test.go:139` |
| bbolt metadata newer than binary (:21) | `TestMetadataNewerSchemaIsRejectedWithoutMutation` — `internal/store/bolt/store_test.go:747` |
| Master Key rotation, 9 kill boundaries (:22) | `TestMasterKeyRotationRecoversFromEveryPublicationKillPoint` — `internal/app/key_rotation_test.go:177` |
| 100 Ledger appends overlap backup snapshot (:23) | `TestSnapshotIsExactDuringOneHundredConcurrentAppends` — `internal/ledger/log_test.go:130`; `TestBackupRestoreMatchesManifestDuringOneHundredConcurrentLedgerWrites` — `internal/app/backup_test.go:512` |
| 10 GiB WAL recovery (:24) | `TestTenGiBWALRecoveryProfile` — `internal/ledger/rto_test.go:15` — **opt-in**, gated on `HALRO_10GIB_WAL_RTO=1` (`performance-baseline.md:75`); not run by `go test ./...` |
| Backup truncated/tampered (:25) | `TestEncryptedBackupRejectsTamperAndTruncation` — `internal/backup/archive_test.go:79` |
| Restore confirmation wrong (:26) | `TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory` — `internal/app/backup_test.go:634` |
| Restore succeeds (:27) | same test — `internal/app/backup_test.go:634` |

### G4.4 failure points with NO automated coverage

The plan's G4.4 (`production-validation-plan.zh-CN.md:157`) names six injections. Mapping them against the matrix:

| G4.4 injection | Coverage |
|---|---|
| 只读磁盘 (read-only filesystem / EROFS) | **NONE.** grep for `EROFS` across `internal/` returns zero hits. Only `ENOSPC` and injected write/sync errors exist (`internal/ledger/log_test.go:241`, `internal/gateway/service_test.go:644`). A real read-only mount at startup — where the data-dir lock, master key read, and durable-rename path all differ from a mid-write ENOSPC — is untested. |
| 磁盘满 (disk full) | **SIMULATED ONLY (E2).** `syscall.ENOSPC` is injected through a fake writer; no test fills a real filesystem. Behaviour of bbolt, Parquet export, backup creation and the `durable` directory-fsync under a genuinely full disk is unobserved. |
| TSDB 不可写 | **NONE in this repo.** This is a Prometheus-side failure; covered only by `PrometheusTSDBDiskHigh/Critical` thresholds (alert-rules.yml:211-224), which is detection, not recovery behaviour. |
| 网络中断 | **NONE as a crash-recovery row.** There are circuit-breaker and retry tests, but the matrix has no network-partition row and no test named for it. |
| `SIGKILL` | **NONE.** grep for `SIGKILL` across `internal/`, `cmd/`, `tests/` returns **zero hits**. Every "crash" in the matrix is a *simulated* truncation of a file at a byte offset or a kill-point in a protocol — never an actual signal to a running process. The distinction matters for anything held only in OS buffers, for the data-dir lock release, and for `halro_shutdown_truncated_attempts_total` (which `HalroShutdownTruncatedAttempts` alerts on, `alert-rules.yml:53`, and whose graceful path is bounded by `server.shutdown_timeout: 2m0s`, `internal/config/default.yaml:29`). |
| 关键持久化点故障 | **PARTIAL (well covered).** The kill-point families are the strongest part of the matrix: 126 checkpoint boundaries, 9 master-key publication boundaries, every WAL byte offset, 10,000 random cuts. |

**Additionally uncovered, and named as a No-Go condition by the plan** (`production-validation-plan.zh-CN.md:196` "备份恢复使已撤销身份复活"): there is **no test that a restored backup does not resurrect a revoked Gateway Key or Admin identity**. The nearest neighbours are `TestStaleRefreshCannotResurrectARevokedKey` (`internal/auth/snapshot_test.go:59`) — about a stale in-memory snapshot refresh, not restore — and `TestRotateOverlapRevokeAndRestore` (`internal/bearercred/credentials_test.go:12`) — about bearer-credential file rotation, not archive restore. **The restore-resurrection red line has no automated coverage.**

---

## 5. G4.6 — backup / restore and RPO / RTO

### CLI surface (`cmd/halro/main.go`)

- `halro backup create --config <f> --output <f.hmbk> --key-file <32-byte 0600 file>` → `internal/app.CreateBackup`, prints the manifest as JSON (`main.go:719-748`).
- `halro backup verify --file <f.hmbk> --key-file <f>` (`main.go:750`).
- `halro backup restore ...` (`main.go:770`); `halro restore` is a legacy alias that re-dispatches to `backup restore` (`main.go:190,712-713`).
- The backup key is a **dedicated 32-byte key file with mode 0600**, independent of `master.key` — enforced by `backup.LoadKeyFile` and `TestLoadBackupKeyFileRequiresExactLengthAndPrivateMode` (`internal/backup/archive_test.go:201`).
- `make backup` requires Halro to be stopped (`CLAUDE.md`; `docs/guides/backup-restore.md`).

### Tests

| Property | Test |
|---|---|
| Create/verify round trip, secrets stay confidential in the archive | `TestEncryptedBackupCreateVerifyAndSecretConfidentiality` — `internal/backup/archive_test.go:17` |
| Tamper and truncation rejected (AEAD final record / checksum) | `TestEncryptedBackupRejectsTamperAndTruncation` — `archive_test.go:79` |
| Newer metadata schema explained rather than silently accepted | `TestVerifyExplainsNewerMetadataSchemaVersion` — `archive_test.go:118` |
| Publication does not overwrite, unsafe sources rejected | `TestBackupPublicationDoesNotOverwriteAndRejectsUnsafeSources` — `archive_test.go:146` |
| Backup key file: exact length + private mode | `TestLoadBackupKeyFileRequiresExactLengthAndPrivateMode` — `archive_test.go:201` |
| Snapshot is an exact fsynced prefix during 100 concurrent appends | `TestSnapshotIsExactDuringOneHundredConcurrentAppends` — `internal/ledger/log_test.go:130` |
| Restore reproduces the manifest watermark under concurrent writes | `TestBackupRestoreMatchesManifestDuringOneHundredConcurrentLedgerWrites` — `internal/app/backup_test.go:512` |
| Atomic staging, wrong confirmation leaves live dir untouched, rollback dir preserved | `TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory` — `internal/app/backup_test.go:634` |
| CLI restore path reachable | `cmd/halro/main_test.go:244` |

### E3 (single laptop) vs. E4 (target environment)

**Measurable on one laptop (E3), and worth doing before Day 3 to de-risk the drill:**
- Wall-clock of `backup create` on a representative data directory, and its archive size — feeds the RPO conversation (how often can you afford to take one).
- Wall-clock of `backup verify` and of `backup restore` into a fresh directory, plus the post-restore `halro doctor` / `config check` / `audit verify` / `usage verify` sequence the operator policy requires (`crash-recovery-matrix.md:32-34`). That sum is the **restore-mechanics half of RTO**.
- Startup replay time as a function of retained WAL size — the opt-in `TestTenGiBWALRecoveryProfile` gives a reference-host figure of **68.578 s** for a near-1-MiB-frame, 10 GiB profile with 10,245 records (`performance-baseline.md:79`). The doc is explicit that this is *not* a universal bound and that operators must measure their own frame distribution (`:87-92`).
- That the rollback directory (`previous_data_dir`) survives and that a wrong confirmation is a no-op — already asserted by `internal/app/backup_test.go:634`.

**REQUIRES-E4 — cannot be established on a laptop:**
- **Actual RPO.** It depends on the production backup *cadence* and on `usage.durability` (`balanced` batches accounting writes; `strict` fsyncs each one — `internal/config/default.yaml:103-106`). Signing RPO = 0 is only honest under `strict`, and the throughput cost of `strict` has not been measured for this candidate.
- **Actual RTO.** Restore to a *new environment* includes provisioning, KMS/Secret Store availability, TLS/IAM reissue, DNS/traffic cutover and readiness — none of which exist locally. The laptop measures the archive-handling segment only.
- **Storage-class fidelity.** fsync cost spans one to two orders of magnitude between APFS and Linux NVMe (`standalone-capacity-baseline.md:41-43,125-133`), so a darwin restore time is not a target-environment restore time.
- **The revoked-identity-after-restore red line** (`production-validation-plan.zh-CN.md:196`) — no local test, see §4.
- **Upgrade and rollback after restore** (G4.6 second half) — needs the real artifact digests and the real environment.

---

## 6. Soak and stress harnesses

### `tests/soak` (a `main` package; `go test ./...` only runs its unit test)

Invocation, verbatim from `docs/verification/soak-testing.md:10-21`:

```bash
export HALRO_GATEWAY_KEY='gw_...'
export HALRO_METRICS_TOKEN='...'
go run ./tests/soak \
  -pid "$(pgrep -n halro)" \
  -commit '<exact-40-character-RC-commit>' \
  -model chat \
  -duration 24h \
  -sample-interval 1m \
  -request-interval 10s \
  -output "soak-artifacts-$(date -u +%Y%m%dT%H%M%SZ)"
```

Flags at `tests/soak/main.go:84-93` (`-gateway-url` defaults to `http://127.0.0.1:8080/v1/chat/completions`, `-metrics-url` to `http://127.0.0.1:9090/metrics`). Secrets come only from env, are held in memory, never passed as process arguments, never written to artifacts (`soak-testing.md:23-26`).

**What it measures** (`main.go:37-50`): per-sample RSS, goroutines, open FDs, WAL queue depth/capacity/append-errors, analytics queue depth and lagging flag, and cumulative request success/failure counters. RSS comes from `/proc/<pid>/status` or `ps -o rss=` (`main.go:310-328`); FDs from `/proc/<pid>/fd` or `lsof` (`main.go:331-350`) — i.e. **out-of-band, because Halro exports neither** (see §1 constraint 3).

**Outputs**: `samples.jsonl`, `requests.jsonl`, `summary.json` (`soak-testing.md:29-36`).

**Pass/fail** (`main.go:355-387`): RSS growth ≤ max(64 MiB, 25% of start); goroutine growth ≤ 20; FD growth ≤ 20; final WAL queue = 0; peak WAL queue ≤ 75% capacity; WAL append-error counter unchanged; analytics not lagging at the end; at least one request completed and failure rate ≤ 1%.

**The status field is duration-gated and cannot be forged by renaming**: `main.go:353-356` sets `status = "release_24h"` only when `opts.duration >= 24h`, otherwise `"smoke_only"` (`releaseDuration` at `main.go:24`). `soak-testing.md:4-5,47-49` reinforces that a smoke result is never promoted.

### `tests/stress`

```bash
HALRO_STRESS=1 go test ./tests/stress -run TestThousandConcurrentSSEConnectionsCleanup -count=1 -v
```

Skips unless `HALRO_STRESS=1` (`tests/stress/stream_test.go:61-63`). Opens `concurrentStreams = 1000` (`:24`) real loopback SSE connections whose clients deliberately stall body reads, then releases and drains. Asserts zero active streams and post-cleanup goroutine/FD counts within **+40** of baseline (`:154,159`). Published figures: 5,003 goroutines and 2,007 FDs at peak returning to 3 and 7; ~36.1 KiB heap and ~77.0 KiB max-RSS per connection; 0.635 s total process CPU (`performance-baseline.md:104-117`). It is an **in-process** client+server measurement, so per-connection deltas include both ends and are a conservative gateway-only estimate (`:115-117`).

### What a short local run can and cannot establish

**A short local run (e.g. `-duration 2m -sample-interval 10s -request-interval 2s`) CAN establish:**
- that the harness itself works against this candidate — metrics scraping, PID sampling, FD counting on this OS, artifact writing;
- that the Gateway/Metrics credentials and the bounded-cost Route are wired correctly;
- gross wiring faults (WAL queue not draining at all, analytics permanently lagging, 100% request failure);
- the **starting** values of RSS / goroutines / FDs, which is what the 24-hour deltas are computed against.

**It CANNOT establish, and must not be reported as establishing:**
- any leak with a slow time constant — RSS, goroutine, FD, or unbounded buffer growth that only separates from GC sawtooth over hours. `soak-testing.md:42-45` requires inspecting time-series *shape*, and two minutes has no shape;
- day-boundary behaviour: Parquet export runs hourly and retention/pruning is day-dated (`internal/config/default.yaml:117-122`), the failure-capture sweep is per-record against a 24 h window (`internal/config/config.go:488`), and daily budgets reset — none of which a 2-minute run crosses;
- Ledger / Audit / Parquet / TSDB growth rates, which is precisely the §5 row the capacity model marks "measure in soak" (`docs/observability/capacity-model.md:51`);
- resource fall-back after load removal (plan G5 step 5, `production-validation-plan.zh-CN.md:176`);
- anything under injected Provider slowness, throttling or transient network failure (G5 step 4) — **the soak harness has no fault-injection mode**; those injections are manual/external;
- the **`release_24h` status itself**, which the harness refuses to emit below 24 h (`tests/soak/main.go:353-356`).

`docs/verification/standalone-capacity-baseline.md:17` states the same rule independently: "Short CI runs prove harness correctness only."

**Current status of the 24-hour gate in this repo: not passed.** `performance-baseline.md:160-162` — "The 24-hour `release_24h` artifact is a separate gate and is still unarchived" — and `:211-213` — "This baseline does not claim that gate has passed until its `release_24h` artifact is archived." No `release_24h` artifact exists anywhere under `docs/verification/`. **UNVERIFIED for `f09ed2d`: no soak artifact for any commit, let alone this one.**

---

## 7. Gaps — most severe first

1. **G4.2 (dead-man failure-domain independence) is structurally unclosable from this repo, and the repo says so.** `deploy/observability/README.md:128-130` and `external-probe/RECEIVER-CONTRACT.md:39-41` both state that repository tests do not prove it. Five properties are REQUIRES-E4 (separate host, independent receiver with durable ack/TTL/replay rejection, non-reused notification identity and Contact Point, independent network path, approved Watchdog grace window). **No amount of local work advances this; it needs the target environment and a receiver that does not exist in this repo.** Plan No-Go condition "主监控失效时独立 dead-man 同时失明" (`production-validation-plan.zh-CN.md:198`) is therefore untested today.

2. **The plan asks G5 to record 流式首字节 p95/p99, and Halro has no first-byte metric at all.** `internal/app/metrics.go` exports no TTFB series; `docs/contracts/metrics-reference.md:160-164` confirms. Either the load harness measures TTFB client-side, or the metric is added, or §5 row 2 is signed as unmeasurable. This is a **plan-vs-implementation contradiction that must be resolved before §5 is signed**, not during G5.

3. **No `SIGKILL` test exists anywhere** (`internal`, `cmd`, `tests`: zero hits). Every crash in `crash-recovery-matrix.md` is a simulated file truncation or a protocol kill-point — strong evidence, but not the same failure. G4.4 explicitly requires `SIGKILL`, and the graceful-shutdown accounting path (`halro_shutdown_truncated_attempts_total`, `server.shutdown_timeout: 2m0s`) is exactly what a `SIGKILL` bypasses.

4. **The "restore must not resurrect a revoked identity" red line has no automated coverage.** It is a No-Go condition (`production-validation-plan.zh-CN.md:196`) and a G4 pass criterion, and neither `internal/app/backup_test.go` nor `internal/auth` tests it. The two similarly-named tests cover different mechanisms.

5. **Four §5 rows are NO BASIS and need four-party temporary thresholds**: streaming TTFB; timeout rate and throttle (429) rate; Ledger/Audit/Parquet per-day growth bytes; Provider cost and token budget. There is **no cost or token alert rule at all** in `alert-rules.yml`, despite `halro_cost_usd_total` and `halro_tokens_total` being exported — a budget overrun is currently invisible to Prometheus.

6. **Halro exports no CPU, RSS, or FD metric**, so the §5 "CPU、RSS/heap、goroutine、FD 上限" row cannot be alerted on from the gateway's own exposition and no alert rule covers it. This makes node_exporter/cAdvisor (or equivalent) a **hard prerequisite of G1**, not an optional extra, and it means the soak harness's out-of-band sampling is currently the only source for three of those four quantities.

7. **Read-only filesystem and genuine disk-full are untested.** `EROFS` appears nowhere in `internal/`; `ENOSPC` is only ever injected through a fake writer. Startup on a read-only data dir (lock acquisition, master-key read, `durable` directory fsync) is a distinct code path from a mid-write ENOSPC, and G4.4 names both.

8. **Two alert thresholds and the TSDB budget are self-declared as non-transferable placeholders.** `HalroWALSyncSlow` (0.05 s) and `HalroAccountingProjectLockSaturated` (0.25 s) carry an in-file instruction to re-baseline per host (`alert-rules.yml:225-227`); the 3.5/4.25 GiB TSDB thresholds assume a 5 GiB volume that `capacity-model.md:36-39` says production must actually provision or replace with filesystem-capacity metrics. If G1 does not re-baseline these, three of the 35 rules are wrong in the target environment.

9. **No end-to-end latency evidence exists for any commit, and none for `f09ed2d`.** All performance evidence is microbenchmarks on reference hosts (`performance-baseline.md`, the three `docs/verification/performance/*` directories), all explicitly disclaimed as non-transferable, and the newest is against candidate `a3634d7`, not this SHA. §5's throughput and latency rows therefore start from *zero* measured baseline for the candidate.

10. **Latency SLOs are quantized to 12 bucket edges** (`internal/usage/aggregate.go:25-27`), and anything above 120 s is only expressible as "greater than". A signed p95/p99 off a bucket edge is not evaluable. This is a constraint on how §5 may be *written*, and it needs to be understood before signature rather than discovered during G5.

11. **The `release_24h` soak artifact has never been archived** (`performance-baseline.md:160-162,211-213`); no such artifact exists under `docs/verification/`. G5 starts from nothing, and the plan's 24-hour window is genuinely a first execution, not a re-confirmation.

12. **The 10 GiB WAL recovery profile is opt-in and not in the ordinary gate** (`HALRO_10GIB_WAL_RTO=1`, `internal/ledger/rto_test.go:15`), so the single number the RPO/RTO row could lean on (68.578 s) is not re-verified per commit and is not attributable to `f09ed2d`. It is also 8.578 s above the 60-second target the doc itself references (`performance-baseline.md:87-88`).

---

### Compliance note

No repository file was created, modified or deleted. No build, `go test ./...`, `npm ci` or `npm run build` was run. No network call was made; Halro was not started; `data/` and `master.key` were not touched. The single command run against the tree was `make observability-check` (exit 0), as authorized.
