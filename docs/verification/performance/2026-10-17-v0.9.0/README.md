# v0.9.0 paired benchmark comparison, 2026-10-17

Baseline: v0.8.5 (`28ea173`, extracted with `git archive`). Candidate: the
v0.9.0 release-preparation commit `8394fd3`, clean.
Host: Apple M4, 10 CPUs, macOS 27.2. Toolchain: Go 1.26.9 on both sides
(`GOTOOLCHAIN=go1.26.9`; v0.8.5 declares 1.26.6), `CGO_ENABLED=1`,
`GOMAXPROCS=4`, default build. Six samples per version; one test binary per
package compiled before sampling; packages run serially, v0.8.5 then the
candidate in each round. Component benchmarks use `-benchtime=1s`; the request
lifecycle uses `-benchtime=100x`. No race detector, no provider calls. An idle
Halro development instance was running on the host throughout.

Files: `v0.8.5.txt` and `candidate.txt` are the raw samples, `benchstat.txt`
is `benchstat v0.8.5.txt candidate.txt`, and `metadata.json` records the
method, versions and the raw files' SHA-256.

## Result

**No time difference is statistically significant** in any of the six
packages (`budget`, `ledger`, `limiter`, `provider`, `redaction`,
`tokenguard`).

- Request lifecycle, nine project/worker combinations: every time comparison
  is `~` (p ≥ 0.31); geomean −1.66%. Allocations fall in every combination,
  −2.6% to −3.5% (p = 0.002), geomean 168.5 → 163.4 per operation; bytes per
  operation fall 1.0% by geomean. The hot-state admission check added by
  #423/#424 is in the candidate's path.
- Replay of 100,000 WAL records: 530.8 ms → 526.9 ms, `~` (p = 0.485), with
  identical bytes and allocations.
- Route candidate resolution: 4.865 µs → 4.824 µs (−0.84%, p = 0.026), 41
  allocations on both sides, 16.31 → 16.59 KiB per operation (+1.72%).
- Project admission (200 B / 5 allocations), Token Guard admission (64 B / 2
  allocations) and both redaction benchmarks are unchanged.

Several request-lifecycle confidence intervals are wide (up to ±96%), as in
the 2026-09-06 comparison, because 100 durable operations are spread across up
to 64 workers.

## Reproduce

For each package below, `go test -c` in a
`git archive v0.8.5` tree and in the candidate checkout, then six rounds of

| Package | Benchmark expression | Time |
|---|---|---|
| provider | `^BenchmarkRegistryResolveCandidates$` | `1s` |
| redaction | `^Benchmark(StandardRedaction\|RollingRedaction)$` | `1s` |
| tokenguard | `^Benchmark(Admit\|AcquireReleaseContended)$` | `1s` |
| limiter | `^BenchmarkProjectAdmission(Contended)?$` | `1s` |
| ledger | `^BenchmarkReplayLargeWAL$` | `1s` |
| budget | `^BenchmarkRequestLifecycle$` | `100x` |

with `GOMAXPROCS=4 <pkg>.test -test.run '^$' -test.bench <expr> -test.benchmem
-test.benchtime <time>`, run from the package directory, then `benchstat`.
