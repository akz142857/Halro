# v0.12.2 same-host performance comparison

Baseline: v0.12.1 commit `510bbe317e8ac0c207c8e32cc43c8cc8d08c48dd`
(annotated tag object `83fdfef47a4b453493e2fced5ef3ed28b5ef9438`).
Candidate runtime source: `9e4b01278312d86462470bc7154adfb8219d8452`.
Release preparation changes only metadata, documentation and an unchanged config
snapshot. Both sides use Go 1.26.10 / darwin-arm64 on the same Apple M4 host,
`GOMAXPROCS=4`, six paired rounds, serial package execution, baseline followed
by candidate for each package. This release task did not launch additional tests or builds during sampling;
the host was not reserved against unrelated workloads. Raw samples and benchstat output are retained here.

| Path | Baseline | Candidate | Result |
| --- | --- | --- | --- |
| Durable price pin, 1 worker | 29.30 ms | 29.23 ms | No significant difference, p=0.937 |
| Durable price pin, 8 workers | 3.843 ms | 3.917 ms | No significant difference, p=0.310 |
| Durable price pin, 64 workers | 1.0095 ms | 0.9919 ms | No significant difference, p=0.180 |
| Request lifecycle, nine combinations, geomean | 5.225 ms | 5.192 ms | -0.64%; individual medians within the 10% threshold |
| 100,000 WAL record replay | 562.8 ms | 562.0 ms | No significant difference, p=1.000 |
| Route candidate resolution | 5.214 microseconds | 5.091 microseconds | No significant difference, p=0.699 |

No time-regression median exceeds 10%; no previously allocation-free path gained
an allocation. Request-lifecycle allocation geomean is unchanged. Price-pin
allocation geomean changes 377.5 to 377.7 (+0.05%) with no significant per-case
allocation increase; bytes increase 0.52%, including the optional pin diagnostic.
The ordinary pricing path keeps the existing shared deployment gate and batch
writer. The new exclusive lock applies only to explicit operator recovery.

Benchmark expressions: RegistryResolveCandidates; StandardRedaction and
RollingRedaction; Admit and AcquireReleaseContended; ProjectAdmission and
ProjectAdmissionContended; ReplayLargeWAL; RequestLifecycle;
DeploymentPricePinCeiling. Compile each package with `go test -c`, then run
`-test.run '^$' -test.bench EXPRESSION -test.benchmem -test.benchtime 1s`;
RequestLifecycle uses `100x`. Six serial paired rounds were compared with
benchstat. See `bench-metadata.json` for exact revisions, toolchain and raw hashes.

These are same-host regression measurements, not universal throughput promises.
There are wide confidence intervals on some durable/concurrent cases. No new
10 GiB replay bound, live-provider performance or long-soak claim is made.
