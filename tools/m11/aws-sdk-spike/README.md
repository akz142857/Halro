# M11 AWS SDK packaging spike

This harness compares the two layouts admitted by ADR 0010 using the same
source commit:

- Option A: the `heimdall` artifact links the official AWS config and KMS SDK;
- Option B: the cloud-neutral `heimdall` artifact stays unchanged and the
  linked binary is released separately as `heimdall-aws`.

The harness copies the repository into temporary worktrees and never changes
the checked-in `go.mod`, `go.sum`, or production source. The overlay is an
experiment only; it is not the production AWS Adapter.

Run from any directory:

```sh
./tools/m11/aws-sdk-spike/run.sh /absolute/result/directory
```

Requirements: Go, Docker, Python 3, network access to the Go module proxy and
the digest-pinned Syft image. It records module diffs, isolated-cache build and
test timings, 25 cold starts, binary/container sizes, SPDX SBOMs,
`govulncheck`, a live metadata-endpoint no-call proof for File mode, and a
Gateway dependency-graph assertion. Raw output and `summary.env` are retained
in the selected result directory.

The vulnerability step explicitly selects the project's Go 1.26.5 toolchain;
this avoids the scanner module's older `toolchain` preference selecting a Go
version that cannot load this repository.

The experiment pins:

- `github.com/aws/aws-sdk-go-v2/config@v1.32.34`
- `github.com/aws/aws-sdk-go-v2/service/kms@v1.55.3`
- the same digest-pinned Syft image used by repository CI

Do not set `HEIMDALL_AWS_SPIKE_EXERCISE=1` during the File-mode no-call test;
that switch exists only to keep the SDK workload-identity path link-reachable
for the packaging measurement.
