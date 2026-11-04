# v0.11.0 paired performance comparison

Date: 2026-11-04. Baseline: v0.10.1 (`fc8eb51f2cb84380f0e4f6c584c9a8c4c9370e31`).
Candidate production Go source: `0d5392d2a8e56e8cd421fc01425085cd0f96af96`.
The final release preparation additionally caps initial Responses snapshots and
already-present tool arguments, alongside publication metadata and an unchanged
historical config fixture. Those new Responses codec paths do not exist in the
previous release and are not exercised by these retained component benchmarks;
their allocation/latency is not inferred from this comparison. The benchmarked
implementations remain unchanged after sampling. Codec bounds are covered by
dedicated regressions and the final quality gate.

Host/toolchain: Apple M4, macOS 27.2, Go 1.26.10, darwin/arm64,
`GOMAXPROCS=4`. Six paired samples per version, baseline then candidate for
each package in each round. Both versions' test binaries were compiled before
sampling. Packages ran serially with no concurrent build/test workload and no
provider requests. An existing idle local Halro container remained running.

The benchmark fixtures match the established comparison in
[`2026-10-19-v0.9.0`](../2026-10-19-v0.9.0/README.md): provider candidate
resolution, strict/rolling redaction, Token Guard, project admission, WAL
replay, request lifecycle and deployment price pin. Component benchmarks used
`-test.benchtime=1s`; request lifecycle used `100x`. Raw files, `benchstat`
output and SHA-256 metadata are retained alongside this record.

## Results

- No benchmark's elapsed-time difference is statistically significant.
  Request lifecycle geomean is 5.122 ms to 5.094 ms (-0.56%); allocations
  remain statistically unchanged across all nine project/worker combinations.
- Candidate resolution is 4.995 us to 4.913 us, with 41 allocations unchanged.
  Bytes increase 2.26% (16.59 to 16.97 KiB). Rolling redaction bytes increase
  1.13%; its 79 allocations are unchanged. No previously allocation-free path
  gains an allocation.
- **Flagged raw median:** the 8-worker price-pin benchmark rises from 3.882 ms
  to 4.519 ms (+16.4%), exceeding the 10% review threshold. This is accepted
  as noisy rather than a demonstrated regression: `p=0.818`, baseline interval
  +/-86%, candidate +/-31%; the 1- and 64-worker comparisons are also not
  significant, and the package geomean is +3.13%. The release changes pricing
  readiness/counting of deleted records, not the measured pin implementation.
  The raw samples remain available for later same-host comparisons.
- Token Guard and project admission retain their allocation counts; WAL replay
  elapsed time, bytes and allocations have no significant change.

These are same-host regression measurements, not capacity promises. Durable
benchmarks have wide intervals; neither an improvement nor a production
throughput claim is inferred from an insignificant difference.
