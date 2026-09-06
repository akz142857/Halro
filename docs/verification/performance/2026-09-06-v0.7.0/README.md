# v0.6.0 → v0.7.0 candidate benchmark evidence

Host: Apple M4, 10 cores, 24 GiB RAM, macOS 26.7, darwin/arm64.
Toolchain: Go 1.26.6, `CGO_ENABLED=1`, `GOMAXPROCS=4`, default build
flags, no race detector.

## Identity and method

- Baseline: `v0.6.0`, commit
  `381743f6613607dc256828f4776b52af8bdd232c`.
- Candidate: parent commit
  `a3634d761d1b9364115ae009ee5e7a8189d6675e` plus the uncommitted Go
  patch whose SHA-256 is recorded in `metadata.json`. The parent commit alone
  does not identify the measured candidate.
- Test binaries were compiled before sampling. Six rounds ran each package
  serially, baseline then candidate, with no tests or builds in parallel.
  Component benchmarks use `-benchtime=1s`; request lifecycle benchmarks use
  `-benchtime=100x` because each operation performs five durable Ledger writes.
- `v0.6.0.txt` and `candidate.txt` are the retained raw samples.
  `benchstat.txt` is the cross-version comparison. `benchstat-attribution.txt`
  compares the candidate's ordinary and Run-attributed production request
  paths after giving both rows the same name. File hashes and the exact
  `benchstat` version are in `metadata.json`.

## Result

No candidate time regression is statistically significant at α=0.05. Ledger
open plus replay of 100,000 records is 542.2 ms → 555.7 ms (`p=0.240`). Route
resolution, redaction, Token Guard, and project admission also have no
significant slowdown. Several ordinary request-lifecycle concurrency rows are
significantly faster; none is significantly slower.

The new production `BenchmarkRunAttributedRequestLifecycle` covers Work
Unit/Run attribution and dual project/run admission. Compared within the same
candidate, none of its nine project/worker combinations differs significantly
from the ordinary lifecycle. The candidate geomean is 4.25% faster, but that
aggregate is descriptive, not a claimed improvement.

The first sampling pass was intentionally discarded after it found two release
defects: zero `RunExpiresAt` values were serialized on ordinary events, making
the WAL larger, and attached request acceptance held an exclusive lifecycle
lock across the durable append, serializing requests in one Run. The candidate
now uses zero-value omission and an RW lifecycle lock: request acceptance shares
the read side so Ledger group commit remains available, while Work Unit/Run
create and close retain the write side and ordering guarantee. The archived raw
samples are only the post-fix run.

The built web entry set plus its heaviest locale is 205,977 → 213,586 gzip
bytes, +3.69%. This remains below the bundle gate.

## Reproduce

Extract `v0.6.0` with `git archive` into a temporary directory. In it and a
candidate checkout matching the recorded source identity, compile one test
binary per package with `go test -c`. Run these expressions six times with
`GOMAXPROCS=4` and no concurrent workload:

| Package | Benchmark expression | Time |
|---|---|---|
| provider | `^BenchmarkRegistryResolveCandidates$` | `1s` |
| redaction | `^Benchmark(StandardRedaction\|RollingRedaction)$` | `1s` |
| tokenguard | `^Benchmark(Admit\|AcquireReleaseContended)$` | `1s` |
| limiter | `^BenchmarkProjectAdmission(Contended)?$` | `1s` |
| ledger | `^BenchmarkReplayLargeWAL$` | `1s` |
| budget | `^BenchmarkRequestLifecycle$` | `100x` |
| budget, candidate only | `^BenchmarkRunAttributedRequestLifecycle$` | `100x` |

Run `benchstat v0.6.0.txt candidate.txt` for the release comparison. These are
local component measurements; they do not measure provider latency, KMS, large
retained production WALs, or an end-to-end service SLO.
