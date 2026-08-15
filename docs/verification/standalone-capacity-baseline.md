# Standalone capacity baseline

Distribution decisions must use measured bottlenecks. A publishable run records
the exact commit, Go version, OS/architecture, CPU count, durability mode, WAL
batch settings, concurrency, duration, and whether the run is smoke or release
evidence. It must never contain credentials, prompts, responses, or headers.

## Workloads

1. Ledger request/reserve/start/settle/finalize throughput and latency.
2. Admission-only RPM/TPM/concurrency and Token Guard cost.
3. Local deterministic non-streaming Provider throughput.
4. Active SSE streams, memory per stream, and goroutine/FD cleanup.
5. Concurrent Admin mutations and bbolt contention.
6. Startup replay at increasing Ledger sizes.

Short CI runs prove harness correctness only. Release evidence uses the exact RC
commit and reference host, reports p50/p95/p99, allocations, RSS, goroutines,
FDs, WAL bytes, fsync mode, and recovery time, and identifies the first observed
bottleneck. Host-specific throughput numbers are not CI pass thresholds.

Existing entry points are `go test -bench`, `tests/stress`, and `tests/soak`.
Machine-readable soak output remains the release evidence format. Compare two
runs only when workload, host, durability, and configuration match.

## Committed harnesses

Workloads 1 and 5 have committed entry points, so their ceilings are regression
gates rather than one-off observations:

| Workload | Benchmark |
|---|---|
| 1 — accounting lifecycle | `internal/budget`: `BenchmarkRequestLifecycle` (worker count × project count) |
| 1 — raw WAL append | `internal/ledger`: `BenchmarkConcurrentAppend` (reports observed batch size) |
| 5 — bbolt contention | `internal/store/bolt`: `BenchmarkMetadataWriteTransaction`, `BenchmarkMetadataBatchDelay` |
| 5 — pricing durable path | `internal/store/bolt`: `BenchmarkDeploymentPricePinCeiling` |

## Observed governing model

Measured on Apple M4 Pro, darwin/arm64, APFS, where `os.File.Sync` issues
`F_FULLFSYNC`. **These absolute numbers do not transfer to a Linux NVMe host**,
where fsync is typically one to two orders of magnitude cheaper. The shapes —
flat versus scaling — are the transferable part.

Accounting throughput is governed by offered concurrency alone. It was governed
by `min(in-flight requests, distinct projects)` until ADR 0018: one project's
five events serialized on `lockProject`, so a project contributed exactly one
concurrent WAL appender however many requests it had in flight. The table below
is that earlier shape, kept because it is what the ADR argues from:

| Projects \ workers | 1 | 8 | 64 |
|---|---:|---:|---:|
| 1 | 45 | 45 | 42 |
| 8 | 44 | 182 | 181 |
| 64 | 43 | 167 | 1252 |

Request lifecycles per second, **before ADR 0018**. At `min` = 1/8/64 the path
achieved 227/910/6261 Ledger events per second against 232/946/7034 for a raw
concurrent append — 89 to 98 percent of the WAL's own group-commit rate, but
only reachable by adding projects.

After ADR 0018 the project axis is gone:

| Projects \ workers | 1 | 8 | 64 |
|---|---:|---:|---:|
| 1 | 46 | 166 | **1011** |
| 8 | 40 | 170 | 1068 |
| 64 | 44 | 165 | 1021 |

A single project went from flat at ~45 to 1011 lifecycles per second, and the
rows now agree within noise. The remaining gap to a raw append (about 5,100
events per second at 64 workers against 7,034) is apply serialization, not a
lock.

Two consequences worth stating before quoting any number from this table:

- **The per-project ceiling described above is gone** (ADR 0018), so a
  single-project load can now observe ADR 0012's pricing-gate improvement. Before
  that change the accounting ceiling bound first at a nearly identical rate and
  reported the pricing work as ineffective.
- **Vary the project count anyway.** The multi-project row is what caught a data
  race in ADR 0018's first implementation: state guarded by a per-project lock
  had been stored per *manager*, which only two projects running at once can
  expose.

## Reading the same numbers on a real host

The metrics endpoint exposes the durable write path directly, so a baseline run
does not need to be re-derived from benchmarks:
`halro_wal_sync_seconds`, `halro_wal_append_{records,batches}_total`,
`halro_accounting_project_lock_{wait,held}_seconds`, and
`halro_metadata_batch_{calls,transactions}_total`.

`halro stats` prints the derived means without a Prometheus install, and
`halro stats -interval 10s` reports a window rather than the lifetime
average — the distinction matters during an incident, since the counters are
cumulative since start. The same summary appears under Settings → Diagnostics.

## Linux/NVMe reference run (2026-08-15)

Host: bare-metal (no virtualization), Intel Xeon E3-1270 v6 (4 cores / 8
threads @ 3.80 GHz), 32 GiB RAM, two Intel SSDPE2MX450G7 NVMe drives in md
RAID1, ext4, Debian 13 (kernel 6.12.69), `intel_pstate` `powersave` governor.
Go 1.26.6 linux/amd64, commit `213d9ec`, production flags, race detector
disabled, median of `-count=3 -benchtime=1s`. The bench data directory was
explicitly placed on the NVMe-backed ext4 filesystem because Debian 13 mounts
`/tmp` as tmpfs, which would void every fsync measurement. Raw machine-readable
output and host metadata are attached to issue #13.

`BenchmarkRequestLifecycle`, request lifecycles per second:

| Projects \ workers | 1 | 8 | 64 |
|---|---:|---:|---:|
| 1 | 1021 | 4182 | 7051 |
| 8 | 1029 | 4197 | 7002 |
| 64 | 1028 | 4118 | 6991 |

`BenchmarkConcurrentAppend`, raw WAL events per second (observed batch size):

| Appenders | 1 | 8 | 64 | 256 |
|---|---:|---:|---:|---:|
| events/s | 5,119 | 22,113 | 116,019 | 153,689 |
| events/batch | 1.0 | 4.1 | 32.6 | 64.0 |

What transfers from darwin and what does not:

- **The shape transfers exactly.** The project axis is flat within noise, so
  ADR 0018's removal of the per-project ceiling holds on Linux; single-project
  load scales with offered concurrency alone, as on darwin.
- **The absolute numbers do not, as predicted.** A single-worker lifecycle went
  from 45/s (darwin `F_FULLFSYNC`, ~4.3 ms per durable event) to 1,021/s
  (~196 µs per durable event on md-RAID1 NVMe) — about 22×, inside the
  documented one-to-two-orders-of-magnitude expectation.
- **The first measured bottleneck moves.** On darwin the lifecycle path reached
  73 % of the raw group-commit rate, so fsync governed both. On this host the
  raw WAL absorbs 116k events/s at 64 appenders while the lifecycle path
  saturates near 35k events/s (~7,000 lifecycles/s) — about 30 %. With fsync
  cheap, serialized `ledger.State.Apply` plus per-event CPU work (this host has
  4 physical cores) governs before the WAL does. Distribution pressure on
  Linux/NVMe therefore appears first in the apply path, not in storage.

bbolt on the same host: batch-mode metadata writes reach ~155k tx/s at 64
workers against ~830 tx/s for a single fsync-bound writer, and the pricing
durable path (`BenchmarkDeploymentPricePinCeiling`) reaches ~6,100 attempts/s
at 64 workers; neither is the governing constraint.
