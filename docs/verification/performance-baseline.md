# Performance Baseline

Release comparisons use the previous tag and the candidate on the **same host
and toolchain**, with identical benchmark fixtures. The current paired evidence
is in [2026-09-03 v0.6.0](performance/2026-09-03-v0.6.0/README.md), including
raw samples, exact source hashes, commands and benchstat output.

## Current comparison, 2026-09-03

Apple M4 / macOS 26.7 / 24 GiB / Go 1.26.6 / `GOMAXPROCS=4`.
Eight samples per version, `-benchtime=1s`, serial alternating version order.
Baseline: v0.5.0 (`556f1d7`); candidate: `cc14e18` plus the P10/P14 follow-up,
identified by the source hashes in the linked evidence.

| Benchmark | v0.5.0 median | Candidate median | benchstat time comparison |
|---|---:|---:|---|
| Route candidate resolution, 8 targets | 5.198 µs | 5.233 µs | no significant difference, p=0.505 |
| Strict response redaction | 74.29 µs | 80.19 µs | no significant difference, p=0.065 |
| Bounded streaming redaction | 17.80 µs | 17.42 µs | no significant difference, p=0.798 |
| Token Guard admit, fixed | 172.0 ns | 179.9 ns | +4.59%, p=0.021 |
| Token Guard admit, EWMA | 189.9 ns | 185.5 ns | no significant difference, p=0.574 |
| Token Guard acquire/release, contended | 331.1 ns | 342.2 ns | no significant difference, p=0.279 |
| Project admission/reconcile/release, unlimited | 100.8 ns | 110.8 ns | +9.82%, p=0.004 |
| Project admission/reconcile/release, limited | 130.9 ns | 139.2 ns | +6.30%, p=0.007 |
| Project admission/reconcile/release, contended | 288.4 ns | 282.6 ns | no significant difference, p=0.505 |
| Open and replay 100,000 WAL records | 562.3 ms | 551.7 ms | no significant difference, p=0.574 |

No measured median time increase exceeds the release procedure's 10% threshold.
That is not a claim that every path is unchanged: unlimited project admission
is statistically slower and close to the threshold (candidate interval ±13%).
The redaction intervals are also wide; retain the raw data and uncertainty.

Route resolution remains **15,888 B / 41 allocations**, Token Guard admit
**64 B / 2 allocations**, and project admission **200 B / 5 allocations** on
both sides. WAL open/replay allocates **220.1 → 232.4 MiB per operation (+5.58%)**,
with roughly 3 million allocations on both sides. This is cumulative allocation,
not retained heap or peak RSS. Exact medians and counts are in `medians.json`.

## Historical microbenchmarks, 2026-07-31 — superseded

These figures have no recorded source revision and must not be used to judge a
current release. The route and WAL drift was confirmed on both v0.4.0 and its
successor in review 260901/P14; it was already present before that release.

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

These are historical measurements, not cross-machine release promises. In
particular, the 405 ns routing figure does not describe current routing. Use the
paired measurements above for release regression decisions and measure the
retained WAL distribution before estimating recovery capacity.

The experimental EWMA check adds about 11.3 ns (7.3%) to an ordinary Token
Guard admission on this host and no additional allocation. Completed-window
evaluation is deliberately off the per-request steady-state path.

## 10 GiB WAL recovery bound

Historical workload measurement; the 2026-09-03 comparison does not remeasure
this experiment or turn it into a bound for a different host or frame mix.

Date: 2026-07-31  
Host: Apple M4 Pro, darwin/arm64, Go 1.24  
Command: `HALRO_10GIB_WAL_RTO=1 go test ./internal/ledger -run TestTenGiBWALRecoveryProfile -count=1 -v`

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
Command: `HALRO_STRESS=1 go test ./tests/stress -run TestThousandConcurrentSSEConnectionsCleanup -count=1 -v`

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

No Halro handler, request, SSE chunk, or Provider object appeared as retained
in-use space. The retained profile is therefore explained by Go scheduler/thread
structures, goroutine stacks, and fixed runtime/HTTP initialization rather than
live connection ownership.

## Single-instance capacity, 2026-08-11

Measured on Apple M4 Pro / darwin-arm64 / 14 cores / 64 GiB / APFS. APFS
`F_FULLFSYNC` is one to two orders of magnitude more expensive than a Linux NVMe
`fdatasync`, so **every throughput figure here is a floor, not a ceiling**; the
shape transfers across hosts, the absolute numbers do not.

| Measurement | Result | What transfers |
|---|---|---|
| Accounting lifecycles | 1,223/s at 64 concurrency | Scales with concurrency, not with project count — the shape ADR 0018 predicts |
| Admin write path | ~31 mutations/s | Floor set by one full bbolt fsync per transaction |
| Topology commit protocol overhead | −25.3% on the Admin write path (41.69 → 31.13 mut/s, p=0.008) | Measured by building both sides and running the same harness against the same input, not inferred from the diff |
| Long run | 2,588 real Admin mutations → RSS +1.4 MiB, flat after ~900; `go_goroutines` steady at 18 | No memory or goroutine leak |

These are capacity references, not commitments. The 24-hour `release_24h`
artifact is a separate gate and is still unarchived (see below).

Release benchmark policy:

- compare on the same host/toolchain;
- investigate any hot-path latency regression above 10%, matching `release-assessment.md`;
- profile before optimizing allocations;
- run load/soak with the race detector disabled and production build flags;
- keep correctness, bounded memory, and secret safety ahead of synthetic throughput.

Admin password hashing is process-wide bounded to two concurrent Argon2id
operations. With the 64 MiB memory parameter this caps the Argon2 working set at
approximately 128 MiB across login, step-up, dummy verification, and password
creation; additional work waits for a slot. Deployment memory limits still need
headroom for the gateway, caches, provider buffers, and Go runtime and must not
be sized to 128 MiB alone.

Measured rather than reasoned, because the pre-fix figure was also measured and
the point of the fix is that the two differ:

| Concurrent logins | Peak heap growth | Per concurrent login |
|---|---|---|
| 64 (bounded, current) | **256 MiB** | 4.0 MiB |
| 64 (unbounded, before the slot limit) | ~4,096 MiB | 64.2 MiB |

Command, on the same host as the rest of this baseline (Apple M4 Pro,
darwin/arm64, go1.26.5):

```
HALRO_MEASURE_ARGON2=1 go test ./internal/adminauth/ \
  -run TestArgon2MemoryUnderAConcurrentLoginStorm -count=1 -v
```

The measurement is opt-in (`HALRO_MEASURE_ARGON2=1`) because it allocates
hundreds of MiB and samples the heap, which makes it a poor neighbour in the
ordinary suite. The structural guarantee — that the slot count is two and that
work above it queues — is asserted unconditionally by
`TestArgon2WorkIsBoundedProcessWide` in the same package. Growth no longer
scales with concurrency, which is the property a memory limit can be sized
against; absolute values on Linux/NVMe will differ.

What the bound converts the failure into, accepted for 1.0.0: waiting for a slot
has no deadline, so a sustained login storm becomes queueing latency on the Admin
login path with a goroutine held per waiter, rather than heap growth. Arrival is
bounded per source by `admin.login_rpm` (5/min) and not globally, and the Admin
server sets no write timeout, so the wait ends only when a slot frees or the
client gives up. A deadline would answer a storm by failing legitimate operator
logins; if the Admin surface is ever put on an untrusted network that trade
inverts and this needs revisiting.

The exact-RC 24-hour workload, measurements, explicit limits, and artifact
format are defined in `docs/verification/soak-testing.md`. This baseline does not claim that
gate has passed until its `release_24h` artifact is archived.
