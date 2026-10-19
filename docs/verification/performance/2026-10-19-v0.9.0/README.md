# v0.9.0 paired benchmark comparison, 2026-10-19

Baseline: v0.8.5 (`28ea173`, extracted with `git archive`). Candidate: `main`
at `2a54dab2`, clean — the commit the v0.9.0 release preparation is based on.
It replaces the 2026-10-17 comparison against `8394fd3`, which predates the
261017 review's fixes (#428–#434). Host: Apple M4, 10 CPUs, macOS 27.2.
Toolchain: Go 1.26.9 on both sides (`GOTOOLCHAIN=go1.26.9`; v0.8.5 declares
1.26.6), `CGO_ENABLED=1`, `GOMAXPROCS=4`, default build. Six samples per
version; one test binary per package compiled before sampling; packages run
serially, v0.8.5 then the candidate in each round. Component benchmarks use
`-benchtime=1s`; the request lifecycle uses `-benchtime=100x`. No race
detector, no provider calls. An idle Halro development instance and two
unrelated containers were running on the host throughout.

The package list is the 2026-10-17 one plus `store/bolt`'s
`BenchmarkDeploymentPricePinCeiling`, because #434 (261017 V-013) changed the
price-pin path it measures.

Files: `v0.8.5.txt` and `candidate.txt` are the raw samples, `benchstat.txt`
is `benchstat v0.8.5.txt candidate.txt`, and `metadata.json` records the
method, versions and the raw files' SHA-256.

## Result

**No hot-path regression.** No time difference above 2% is statistically
significant in any of the seven packages.

- Request lifecycle, nine project/worker combinations: eight are `~`; one
  (`projects=8/workers=1`) is −4.06% (p = 0.026). Geomean −1.65%.
  Allocations fall in every combination, −2.9% to −3.6% (p = 0.002), geomean
  168.7 → 163.1 per operation; bytes fall 0.96% by geomean.
- Price pin ceiling (1, 8, 64 workers): every time comparison is `~`; geomean
  −0.37%, one allocation fewer per pin. The drift check #434 added (one
  `time.Now()` per selection) does not show.
- Token Guard admission: +1.56% (`fixed`, p = 0.015) and +0.84% (`ewma`,
  p = 0.039), 2 allocations and 64 B unchanged. The contended acquire/release
  is `~` in time, with the same 3 allocations at 160 B instead of 128 B: since
  #428 (V-011) each lease carries the 32-byte sample it admitted, so a
  requeued deferred request can be retracted.
- Replay of 100,000 WAL records: 519.8 ms → 519.0 ms, `~`, identical bytes
  and allocations.
- Route candidate resolution: `~` in time, 41 allocations on both sides,
  16.31 → 16.59 KiB per operation (+1.72%), as on 2026-10-17.
- Project admission (200 B / 5 allocations) and both redaction benchmarks are
  unchanged.

Several request-lifecycle and price-pin confidence intervals are wide (up to
±94%), as before, because a few durable operations are spread across up to 64
workers.

## Reproduce

For each package below, `go test -c` in a `git archive v0.8.5` tree and in the
candidate checkout, then six rounds of

| Package | Benchmark expression | Time |
|---|---|---|
| provider | `^BenchmarkRegistryResolveCandidates$` | `1s` |
| redaction | `^Benchmark(StandardRedaction\|RollingRedaction)$` | `1s` |
| tokenguard | `^Benchmark(Admit\|AcquireReleaseContended)$` | `1s` |
| limiter | `^BenchmarkProjectAdmission(Contended)?$` | `1s` |
| ledger | `^BenchmarkReplayLargeWAL$` | `1s` |
| budget | `^BenchmarkRequestLifecycle$` | `100x` |
| store/bolt | `^BenchmarkDeploymentPricePinCeiling$` | `1s` |

with `GOMAXPROCS=4 <pkg>.test -test.run '^$' -test.bench <expr> -test.benchmem
-test.benchtime <time>`, run from the package directory, then `benchstat`.
