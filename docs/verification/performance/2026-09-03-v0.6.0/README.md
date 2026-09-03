# v0.5.0 → v0.6.0 candidate benchmark evidence

Host: Apple M4, 10 cores, 24 GiB RAM, macOS 26.7, darwin/arm64.
Toolchain: Go 1.26.6, `CGO_ENABLED=1`, `GOMAXPROCS=4`, default build flags,
no race detector. This is a different host and toolchain from the July baseline;
the July numbers are not used for the comparison.

## Identity and method

- Baseline: `v0.5.0`, commit `556f1d7992fe9e2be8e5b2a26ad54c5d44319f90`.
- Candidate: `cc14e1865754387d7c45ca073c161048e59d54bc` plus the uncommitted
  P10/P14 follow-up. `metadata.json` records both source-set digests;
  `source-sha256.json` contains the corresponding individual Go source,
  `go.mod` and `go.sum` hashes. The parent commit alone does not identify the
  measured candidate.
- Existing route-resolution, redaction, Token Guard and WAL benchmark bodies
  are unchanged between the two versions. The new
  `internal/limiter/benchmark_test.go` was copied byte-for-byte into the baseline
  checkout to measure the same synchronous API and workload on both sides.
- Test binaries were compiled before sampling. Eight rounds run each package
  serially, with the baseline/candidate order reversed on alternate rounds.
  Each sample uses `-test.run=^$ -test.benchmem -test.benchtime=1s
  -test.count=1 -test.cpu=4`. No tests or builds ran concurrently with sampling.
- `commands.json` records every sample command, duration and exit code.
  `v0.5.0.txt` and `candidate.txt` are the unedited benchmark output.
  `benchstat.txt` compares those files using
  `golang.org/x/perf@v0.0.0-20260825160852-19be9d8e6c70`.

## Reproduce

Extract `v0.5.0` with `git archive` into a temporary directory and use a candidate
checkout whose Go source matches the recorded hashes. Copy the limiter harness
from the candidate to the baseline. In each checkout, compile one binary per
package using `go test -c -o <binary> ./internal/<package>` with the environment
above. Run the binaries serially for eight rounds, alternating version order:

| Package | `-test.bench` expression |
|---|---|
| provider | `^BenchmarkRegistryResolveCandidates$` |
| redaction | `^Benchmark(StandardRedaction\|RollingRedaction)$` |
| tokenguard | `^Benchmark(Admit\|AcquireReleaseContended)$` |
| limiter | `^BenchmarkProjectAdmission(Contended)?$` |
| ledger | `^BenchmarkReplayLargeWAL$` |

Append each version's output to its own file, then run
`benchstat v0.5.0.txt candidate.txt`. The sample commands use temporary absolute
binary paths; substitute the new paths when reproducing.

## Scope

These measurements compare local component costs. They do not measure provider
latency, end-to-end request capacity, deferred queue drain rate, or a production
SLO. The WAL fixture contains 100,000 legacy reservation frames and measures
open plus full replay; it is not an authenticated epoch-4 replay benchmark or
a repetition of the historical 10 GiB recovery experiment. That historical
measurement retains its original date, host and workload limitations.

The limiter's synchronous path still takes one project-state lock for admission;
the new request/execution flags do not add a second admission lock. The deferred
dispatcher introduces separate scheduling state, whose queue-index limitation
remains the explicitly deferred C7 decision.
