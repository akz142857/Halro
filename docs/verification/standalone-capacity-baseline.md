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

Still open: reproducing the table above on Linux with an NVMe-backed data
directory, production build flags, and the race detector disabled.
