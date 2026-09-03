# Run Governance S0 容量与契约验证

- 日期：2026-09-04
- 基线提交：`1cddd9c65a88d0225f6919e3abee812a1b182d2c`
- 分支：`codex/run-governance-prd-review`
- 主机：Apple M4，darwin/arm64，10 logical CPUs
- Go：`go1.26.6`
- 性质：设计 spike；没有修改生产持久格式、生产请求路径或用户可见配置

## 结论

S0 没有发现需要推翻 PRD 核心边界的证据。双预算可以在同一个 Project
准入临界区内原子判断；Governance Journal 可以与 Accounting Ledger 分开
认证和失败；100 万条紧凑 Outcome 事件可在参考机上于 0.69 秒内全量重放。

这些结果允许进入 S1，但不能被解释成生产 SLA：治理日志 probe 使用固定长
二进制帧和紧凑数值 ID，未包含生产 JSON/API、bbolt、备份、查询索引和完整
Outcome 字段。它测的是方案下界与数量级，不是最终实现成绩。

首版容量裁决如下：

| 项目 | 冻结值 | 裁决依据 |
| --- | ---: | --- |
| 每 Project active Runs | 1,000 hard max | 保留 PRD 候选值；比 1m 历史重放规模小三个数量级，并限制尚未实现的生产状态对象与攻击面 |
| 每 Project open Work Units | 1,000 hard max | 与 active Run 相同；关闭不删除历史 |
| 默认 / 最大 Run TTL | 24 小时 / 30 天 | TTL 只派生准入状态；容量仍由 active hard max 封顶，不能靠 TTL 单独防滥用 |
| 每 Work Unit Runs | 32 | 保持可解释下钻和有界 payload |
| 每 Work Unit Definitions | 8 | 与低基数 cohort rollup 一致 |
| 每 Project active Definitions | 64 | 不把 Definition ID 作为 metrics label；Admin 查询分页 |
| 每 Outcome head 修订 | 20 | 追加历史最多产生 20 倍事件，current-head 内存仍为一个条目；超过后拒绝 |
| 控制面 JSON body | 16 KiB | 当前字段集合无原始业务内容，16 KiB 留有充足余量并限制解析放大 |
| 控制面写入限流 | Key 120 RPM，Project 1,000 RPM | Work Unit、Run、close 与 Outcome 共享写桶；另受 active/revision hard max 约束 |
| 控制面读取限流 | Key 600 RPM，Project 5,000 RPM | 所有列表强制 cursor 与最大 200 items/page；Summary 另限 60 RPM/Project |
| 内置 Summary cohort | 最多 90 天且最多 100,000 Work Units | 任一边界超出则要求 export；100k 线性下界 probe 中位数约 33.1 µs，生产实现仍以 2 秒服务端上限验收 |
| close 后 Outcome 窗口 | 30 天 | 允许迟到与纠错，同时限制长期写入攻击；到期后只读历史 |
| Governance segment / RTO | 4 MiB；1m events ≤30 秒 | 参考机紧凑下界 0.68 秒，门槛保留约 44 倍实现与主机余量 |

限流是首版默认值，允许管理员下调，不允许配置超过上述上界。真实生产负载若
需要提高，必须在最终结构上重跑同类容量和滥用测试，不能只改常量。

## 实测结果

### 双预算准入

`BenchmarkS0AdmissionPrototype` 的中位数：

| workers | 当前 Project-only | Project + Run probe |
| ---: | ---: | ---: |
| 1 | 257.0 ns/op | 45.4 ns/op |
| 8 | 438.2 ns/op | 222.7 ns/op |
| 64 | 538.6 ns/op | 208.5 ns/op |

probe 比现有 Manager 简单，数字不能作为“新路径更快”的结论。可采信的结论是：
一个 mutex 下同时维护 Project/Run pending，在 64 workers 下没有出现数据 race、
over-admission 或失控的锁开销；持久 append 仍必须在锁外完成。

### 无 Run 热路径基线

当前 `BenchmarkRequestLifecycle` 在不改生产路径的 HEAD 上记录了 1/8/64
workers、1/8/64 Projects 的完整基线。单 Project 的中位吞吐约为 39.4、154.3、
1,203 lifecycles/s。S1/S2 最终实现必须在同主机同命令下对比；无 Run 请求不得
新增状态读取或持久写，任何实质回退都应退回设计。

### Governance Journal 恢复

| 事件数 | current heads | 文件字节 | 写入 | 全量重放 | 重放后 heap delta |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 9,000 | 650,012 | 15.0 ms | 11.8 ms | 2.61 MiB |
| 100,000 | 90,000 | 6,500,012 | 43.4 ms | 63.4 ms | 6.30 MiB |
| 1,000,000 | 900,000 | 65,000,012 | 314.8 ms | 681.9 ms | 65.07 MiB |

帧是 33-byte payload 加 32-byte HMAC-SHA256 chain digest。日志字节约
65 bytes/event、head 的实测 heap 下界约 76 bytes/head。生产实现必须重新测量，
因为字符串 ID、完整字段、索引和 allocator 开销都会放大结果。

### 当前 Accounting Ledger State 下界

使用现有 `ledger.State.Apply` 重放唯一 `RequestAccepted` 事件，10k/100k/1m 的耗时
分别约 19.7 ms、101.7 ms、1.02 s；1m 后 resident heap delta 为 127.18 MiB，约
133 bytes/event。该 fixture 主要测现有 event digest 去重集合，没有 reservation、
settlement 或未来 Run balance，因此仍是状态成本下界。它证明当前 State 本身已经随
权威事件线性增长，Run/Work Unit active 状态必须受 1,000 hard max 约束，历史分析
继续依赖分段投影/export，不能再复制一份无界费用事实。

### 现有 Usage Aggregate / checkpoint

现有契约测试测得：resident Attempt + RequestSummary 约 1,014 bytes/attempt，
checkpoint 约 1,120 bytes/attempt；40,000 requests 的首次 checkpoint 为
41,755,032 bytes，随后只增加一个 request 的 checkpoint 为 1,821,556 bytes。
因此 Run/Outcome 投影不能重新塞进当前高基数 Attempt segment，也不能让每个
tick 重写全窗口。

### Cohort Summary 下界

对 10,000/100,000 个紧凑 Work Unit rows 做故意保守的线性汇总，中位数分别约
3.34/33.05 µs，均为 0 alloc/op。生产查询包含索引、双 watermark、多个 Definition、
并发和序列化，不能引用这个数作为 API 延迟；它只支持把 100,000 Work Units 作为
内置查询 cardinality hard max。正式 S3 门槛为服务端 2 秒，超过 90 天或 100,000
Work Units 直接返回有界错误并引导 export，不能退化为全 Ledger scan。

## Checkpoint 与 export 裁决

Accounting 继续使用 Usage checkpoint 的不可变 4 MiB segments；下一版本只在
Attempt/Request record 增加 nullable `run_id`/`work_unit_id`。Run/Work Unit 权威
状态从 Accounting Ledger 重建，并使用独立的 accounting-governance join head，
不把 Outcome head 写进同一个 checkpoint transaction。

Governance 使用自己的 checkpoint：小型 head 保存 governance watermark、rollup
和 segment refs；Outcome current-head 与幂等索引使用不可变、按 sequence 切分的
4 MiB segments。一次 checkpoint 只重写 head 与 open tail；head 和 tail 在一个
bbolt transaction 内前进。查询捕获一对 watermarks 后连接两个读模型，不制造
跨日志全局原子顺序。

Export 选择规范化、多数据集 NDJSON：`work_units.ndjson`、`runs.ndjson`、
`outcomes.ndjson`、`outcome_definitions.ndjson`。Usage Attempt 仍沿用已配置的
Parquet/NDJSON 格式并只增加 nullable 归属字段。统一 manifest 为每个文件记录
dataset、schema、format、checksum、sequence range、record count，并记录一对
watermarks。费用只存在 Attempt 数据集；其他数据集只引用 ID。这个选择复用
ADR 0017 的低依赖 NDJSON 和按文件 format 语义，也避免把异构实体压进一个
宽而含混的 JSON 流。

## 故障与并发证明

- `TestS0DualAdmissionNeverOverAdmitsSameRun`：64 个并发准入只能接受 cap 内 10 个；
- `TestS0DualAdmissionManyRunsShareProjectCap`：多个 Run 不能绕过 Project cap；
- `TestS0DualAdmissionSettlementCanRestoreHeadroom`：低于 reservation 的结算恢复额度；
- `TestS0RunCloseSerializesWithAdmission`：close 后拒绝新准入，已准入 Attempt 可结算；
- `go test -race` 覆盖上述预算并发；
- `TestS0GovernanceJournalRejectsTampering`：修改单字节后重放失败；
- `TestS0GovernanceFailureDoesNotPoisonAccountingLog`：治理日志损坏后独立 Accounting Ledger 仍可追加。

## 原始证据与复现

- `admission-benchmark.txt`：双预算 probe 原始 benchmark；
- `current-request-lifecycle-benchmark.txt`：无 Run 当前热路径基线；
- `governance-recovery-profile.txt`：10k/100k/1m 恢复数据；
- `accounting-state-profile.txt`：当前 Accounting State 10k/100k/1m 基数下界；
- `current-usage-capacity.txt`：现有 Usage resident/checkpoint 基数成本；
- `cohort-summary-benchmark.txt`：10k/100k cohort 线性汇总下界；
- `environment.txt`：环境、提交与 probe 源码摘要。

复现命令记录在 `commands.txt`。这些命令不调用真实 Provider，也不产生付费流量。
