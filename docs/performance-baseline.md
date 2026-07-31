# Performance Baseline

Date: 2026-07-31  
Host: Apple M4 Pro, darwin/arm64, Go 1.24  
Command: `go test ... -bench ... -benchmem -benchtime=1s`

| Benchmark | Result | Allocation |
|---|---:|---:|
| Route candidate resolution, 8 round-robin targets | 405 ns/op | 2,880 B/op, 1 alloc |
| Strict response redaction, 921-byte sample | 71.8 µs/op, 12.83 MB/s | 10,392 B/op, 27 allocs |
| Bounded streaming redaction, 244-byte sample | 16.8 µs/op, 14.53 MB/s | 18,110 B/op, 79 allocs |
| Replay 100,000 WAL records | 401 ms/op, 57.07 MB/s | 173 MB/op, 2.4M allocs |
| Token Guard admit, fixed thresholds | 154.5 ns/op | 64 B/op, 2 allocs |
| Token Guard admit, EWMA enabled (no boundary crossing) | 165.8 ns/op | 64 B/op, 2 allocs |

These numbers are a regression baseline, not cross-machine release promises. Provider network latency dominates ordinary requests; routing overhead is sub-microsecond. Redaction throughput is adequate for token streams but allocation reduction is a useful post-RC optimization. WAL replay restores 100,000 records in under half a second on the reference host, while its allocation volume should be watched during multi-million-record recovery tests.

The experimental EWMA check adds about 11.3 ns (7.3%) to an ordinary Token
Guard admission on this host and no additional allocation. Completed-window
evaluation is deliberately off the per-request steady-state path.

## 10 GiB WAL recovery bound

Date: 2026-07-31  
Host: Apple M4 Pro, darwin/arm64, Go 1.24  
Command: `HEIMDALL_10GIB_WAL_RTO=1 go test ./internal/ledger -run TestTenGiBWALRecoveryProfile -count=1 -v`

| WAL bytes | Records | Frame profile | Open/verify | State replay | Total RTO | Effective two-scan throughput |
|---:|---:|---|---:|---:|---:|---:|
| 10,737,948,420 | 10,245 | valid frames with payloads near the 1 MiB limit | 29.638 s | 38.939 s | **68.578 s** | 298.65 MiB/s |

Post-recovery `HeapAlloc` was 12,090,296 bytes and `HeapSys` was 24,608,768
bytes with 10,245 pending reservations. WAL generation and fsync are excluded
from RTO; the timed path is startup checksum/JSON validation followed by full
`ledger.State.Apply` replay. The temporary 10 GiB file is deleted after the
test.

This measured profile is the published v1 bound and is slightly above the
60-second target. It is not a universal bound for every event-size distribution:
many small records increase JSON and map-operation cost, while a recent Usage
checkpoint reduces the separate analytics suffix replay. Operators should use
69 seconds as the reference-host minimum recovery allowance for this profile
and measure their retained WAL distribution before setting restart SLOs.

## 1,000 concurrent SSE connections

Date: 2026-07-31  
Host: Apple M4 Pro, darwin/arm64, Go 1.24  
Command: `HEIMDALL_STRESS=1 go test ./tests/stress -run TestThousandConcurrentSSEConnectionsCleanup -count=1 -v`

The test uses real loopback TCP connections. Each server stream sends one SSE event,
then remains open while all 1,000 clients deliberately pause response-body reads.
After the release barrier, every stream completes and every client drains the body.

| Sample | Heap allocated | Process max RSS | Goroutines | Open FD |
|---|---:|---:|---:|---:|
| Before | 305,944 B | 12,648,448 B | 3 | 7 |
| 1,000 held streams | 36,431,312 B | 89,669,632 B | 5,003 | 2,007 |
| After cleanup | 21,730,720 B | 95,666,176 B | 3 | 7 |

- held-stream heap delta: about 36.1 KiB per connection;
- held-stream max-RSS delta: about 77.0 KiB per connection;
- total process CPU during open, release, drain, and cleanup: 0.635 seconds;
- active streams, goroutines, and file descriptors returned to their starting values;
- the residual Go heap is bounded retained runtime/HTTP allocation, not a live
  connection leak; `Maxrss` is a high-water mark and cannot fall after cleanup;
- this is an in-process client/server measurement, so per-connection deltas include
  both ends and are a conservative Gateway-only buffer estimate.

The release workflow reruns this gate on Linux. It fails if FD enumeration is not
available, if any stream remains active, or if post-cleanup goroutine/FD counts grow
by more than 40 over baseline.

### Repeated cleanup and heap profile

The same test was run ten times in one process (`-count=10`), creating 10,000
total SSE connections. Every round returned from 5,003 to 3 goroutines and from
2,007 to 7 file descriptors. Post-cleanup HeapAlloc varied between 8.7 MiB and
22.8 MiB without monotonic growth. Process `Maxrss` rose during runtime warm-up,
then plateaued at about 106.9 MiB for the final three rounds; as a high-water
mark it cannot decline.

An in-use heap profile captured after cleanup reported 8,203.81 KiB sampled:

| Allocation owner | In-use | Share |
|---|---:|---:|
| `runtime.allocm` | 5,643.01 KiB | 68.79% |
| `runtime.malg` | 1,536.66 KiB | 18.73% |
| `net/http.init` | 512.10 KiB | 6.24% |
| runtime scavenger state | 512.05 KiB | 6.24% |

No Heimdall handler, request, SSE chunk, or Provider object appeared as retained
in-use space. The retained profile is therefore explained by Go scheduler/thread
structures, goroutine stacks, and fixed runtime/HTTP initialization rather than
live connection ownership.

Release benchmark policy:

- compare on the same host/toolchain;
- fail investigation if route resolution or redaction latency regresses by more than 20%;
- profile before optimizing allocations;
- run load/soak with the race detector disabled and production build flags;
- keep correctness, bounded memory, and secret safety ahead of synthetic throughput.

The exact-RC 24-hour workload, measurements, explicit limits, and artifact
format are defined in `docs/soak-testing.md`. This baseline does not claim that
gate has passed until its `release_24h` artifact is archived.
