# 内存有界重设计 · P0 基线

> 对应 [内存有界重设计](../todo/memory-bounded-redesign.zh-CN.md) §6、§7 的 P0。
> 测量日期 2026-10-15。后续每个阶段用**同一个探针、同一个环境**重跑，与本页对比。

## 1. 怎么测的

**探针：** `internal/app/memory_baseline_test.go` 里的 `TestMemoryBaseline`，默认跳过。

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o app.test ./internal/app
docker volume create halro-p0
docker run --rm -v "$PWD":/bin-in:ro -v halro-p0:/work -e TMPDIR=/work/tmp \
  -e HALRO_MEMORY_BASELINE=/work/out golang:1.26.9-bookworm \
  bash -c 'mkdir -p /work/tmp && /bin-in/app.test -test.run "^TestMemoryBaseline$" -test.v -test.timeout=6h'
```

- **请求：** 每个请求走默认 Gateway 路径的完整计费生命周期：begin → reserve → start → settle → finalize。
  - 这些步骤写 Ledger WAL，并更新 Usage 聚合。
  - 维度为 16 个 Project、64 个 Key、4 个模型。
  - Run governance 需要按 Project 显式开启，探针不覆盖。
- **历史：** 依次填到 1 万、10 万、100 万请求，填充时并发 64。所有历史都在控制台的 30 天窗口内，这是窗口常驻聚合最坏的情况。
- **每一档测量什么：**
  1. 关闭后冷启动 `Open`，计时，同时采一份 CPU profile；
  2. 连续两次 GC 后读 `HeapAlloc`，并采一份 heap profile；
  3. 在并发 1、16、64 下各跑 10 秒，记录吞吐、p50、p99，以及每请求的 Ledger fsync 次数。
- **每请求总 fsync：** 另跑一遍 1 万档，用 `strace -f -c` 统计全部子系统的 fsync 和 fdatasync。
- **环境：**
  - macOS 宿主上的 OrbStack Linux VM：Linux 7.2.9，aarch64，10 核，11.7 GiB 内存；
  - 数据目录放在 Docker 命名卷上，文件系统是 btrfs；
  - Go 1.26.9，`usage.durability: balanced`（默认值）；
  - 代码为 `main` @ `f317d077` 加上探针本身，探针不改变生产行为。

绝对数值只在这个环境里有意义，阶段之间比的是相对值。

## 2. 结果

| 历史请求数 | 常驻 heap（GC 后） | 冷启动 | Ledger WAL | 数据目录 |
| ---: | ---: | ---: | ---: | ---: |
| 1 万 | 50.2 MiB | 0.71 s | 31 MiB | 47 MiB |
| 10 万 | 439.2 MiB | 7.56 s | 307 MiB | 460 MiB |
| 100 万 | 4177.7 MiB | 231 s（复测 205 s） | 3073 MiB | 4463 MiB |

| 历史 | 并发 | 吞吐（req/s） | p50 | p99 | 每请求 Ledger fsync |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 万 | 1 | 25 | 29.6 ms | 396.6 ms¹ | 5 |
| 1 万 | 16 | 506 | 31.4 ms | 41.8 ms | 0.31 |
| 1 万 | 64 | 1933 | 32.6 ms | 54.7 ms | 0.079 |
| 10 万 | 1 | 34 | 28.8 ms | 37.5 ms | 5 |
| 10 万 | 16 | 493 | 32.4 ms | 42.1 ms | 0.31 |
| 10 万 | 64 | 1920 | 32.7 ms | 51.3 ms | 0.079 |
| 100 万 | 1 | 31 | 31.6 ms | 49.8 ms | 5 |
| 100 万 | 16 | 445 | 35.3 ms | 57.2 ms | 0.31 |
| 100 万 | 64 | 2042 | 30.7 ms | 52.3 ms | 0.082 |

¹ 第一次测量窗口的预热离群值；10 万、100 万两档的并发 1 都在 50 ms 以内。

- **每请求总 fsync：** 在 1 万档的混合负载下（填充加三个测量窗口，共 21046 个请求），strace 统计到 3088 次 fsync 和 71 次 fdatasync，合计 3159 次，平均每请求 0.15 次。
- **Ledger 组提交：** 并发 1 时每个生命周期事件一次 fsync，每请求 5 次；并发越高，组提交摊得越薄。

## 3. 观察

1. **内存随历史线性增长，约每请求 4.4 KB**（4128 MiB / 99 万请求）。 吞吐和延迟基本不随历史变化，所以问题只在内存和启动时间上。
2. **100 万历史时，常驻 heap 的组成：** heap 共 4.07 GB，下面四项占 98%。

   | 消费者 | 位置 | 大小 | 说明 |
   | --- | --- | ---: | --- |
   | Ledger `leaseRecords` | `internal/ledger/event.go:700` | 0.90 GB | 结算后仍保留 |
   | Ledger `settled` | `event.go:809` | 0.86 GB | 每个已结算尝试一条，永久保留 |
   | Ledger `eventDigests` | `event.go:830` | 0.44 GB | 每个事件一条 |
   | Usage 窗口明细 | `internal/usage/checkpoint.go:457/466`，加上 JSON 解码出的字符串 | 1.72 GB | attempts 和 summaries |

   Ledger 合计约 2.3 GB，Usage 约 1.7 GB。这和设计稿 §1.2 从代码得出的判断一致。
3. **启动时间超线性。** 历史从 10 万到 100 万增加了 10 倍，启动时间从 7.6 s 增加到 205～231 s，约 30 倍。100 万档 `Open` 的 CPU profile 采样共 682 s CPU：
   - GC 标记约 502 s，占 73%。回放过程中，越来越大的常驻 heap 被反复扫描。
   - `Open` 自身约 166 s，其中：
     - WAL 被完整扫描两遍：`ledger.OpenWithOptions` 约 51 s，`Replay` 约 78 s；
     - `read` 系统调用约 32 s；
     - Usage 检查点的 JSON 解码约 37 s。

   P2 的"快照 + 只回放后缀"同时消除了全量重放和常驻 heap，所以这两项会一起下降。
4. **默认请求路径上的 fsync 基本都来自 Ledger 的组提交。** 按各段请求数乘以每请求 Ledger fsync 估算：填充 1 万 × 0.08，加上 170 × 5、2364 × 0.31、8512 × 0.08，约 3070 次；strace 实测合计 3159 次。差出的约 90 次来自初始化和检查点，没有发现其他子系统在每个请求上 fsync。

## 4. 与 §6 预算对照

| 项 | 预算 | P0 基线 |
| --- | --- | --- |
| Go heap 增量（1 万 → 100 万） | ≤ 32 MiB | 4128 MiB |
| 热状态上限 | ≤ 128 MiB | 无上限 |
| 吞吐（并发 1 / 16 / 64） | ≥ 基线的 95% | 基线：100 万档 31 / 445 / 2042 req/s |
| p99 | ≤ 基线的 110% | 基线：100 万档 49.8 / 57.2 / 52.3 ms |
| 每请求 fsync | ≤ 基线 | 基线：Ledger 5 / 0.31 / 0.08；strace 合计 0.15 |
| 启动时间（100 万历史，有快照） | ≤ 10 s | 205～231 s |
| 历史索引追赶延迟 | ≤ 5 s | 尚无索引 |

吞吐、p99、fsync 三项以 100 万档为比较基线：后续阶段要在同样大的历史下比较，才能看出历史是否还影响请求路径。
