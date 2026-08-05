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
