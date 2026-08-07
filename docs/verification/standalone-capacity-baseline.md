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

Accounting throughput is governed by `min(in-flight requests, distinct
projects)`, because one project's five events serialize on `lockProject` and so
contribute exactly one concurrent WAL appender:

| Projects \ workers | 1 | 8 | 64 |
|---|---:|---:|---:|
| 1 | 45 | 45 | 42 |
| 8 | 44 | 182 | 181 |
| 64 | 43 | 167 | 1252 |

Request lifecycles per second. At `min` = 1/8/64 the path achieves 227/910/6261
Ledger events per second against 232/946/7034 for a raw concurrent append — 89
to 98 percent of the WAL's own group-commit rate. The accounting path is
therefore already at the WAL's efficiency frontier for a given number of
concurrently appending projects; the open item is that one project cannot be
more than one of them, which is ADR 0018.

Two consequences worth stating before quoting any number from this table:

- **The per-project ceiling is independent of upstream latency.** The lock is
  held only across the durable append, not across the Provider call, so a single
  project cannot exceed it however many requests are in flight.
- **A single-project load cannot observe the pricing gate improvement** in ADR
  0012's amendment, because the accounting ceiling binds first at a nearly
  identical rate. A load matrix without multiple projects will report that
  change as ineffective.

## Reading the same numbers on a real host

The metrics endpoint exposes the durable write path directly, so a baseline run
does not need to be re-derived from benchmarks:
`heimdall_wal_sync_seconds`, `heimdall_wal_append_{records,batches}_total`,
`heimdall_accounting_project_lock_{wait,held}_seconds`, and
`heimdall_metadata_batch_{calls,transactions}_total`.

`heimdall stats` prints the derived means without a Prometheus install, and
`heimdall stats -interval 10s` reports a window rather than the lifetime
average — the distinction matters during an incident, since the counters are
cumulative since start. The same summary appears under Settings → Diagnostics.

Still open: reproducing the table above on Linux with an NVMe-backed data
directory, production build flags, and the race detector disabled.
