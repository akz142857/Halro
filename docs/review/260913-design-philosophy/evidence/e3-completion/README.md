# E3 completion and external-boundary evidence

Date: 2026-09-13 (Asia/Singapore)

This evidence belongs to the assessment remediation working tree based on
`b7edd4d24152582e5978cf58d1917fedd803e542`. It is not evidence for the already
published v0.8.0 tag. The exact candidate executable used below has SHA-256
`053c3231a481d490b255756757e6702520bc9000f07c03aa5810d9d918005f93`.

## Provider primitive contract

- Every registered `(profile, operation)` is compared with an independent,
  literal primitive oracle rather than an expected value generated from the
  production table.
- Every reachable Profile constructs a concrete expected adapter family.
- Gateway reservation, start, settlement and recovery persist
  `provider_primitive`; a new Profile-attributed attempt without a bound
  primitive is refused at the Ledger append boundary.
- Focused Provider, App, Budget, Ledger and Gateway packages pass with
  `-count=1`. The Gateway attribution regression asserts the OpenAI Chat
  Completions primitive, so a same-shape OpenAI/DeepSeek swap no longer remains
  invisible.

This closes `PHIL-A04` at E2. It does not claim a real upstream invocation.

## Fresh local gate

- `go test -count=1 -shuffle=on ./...`: passed; `internal/app` took 286.057s.
- `go test -race -count=1 -timeout=20m ./...`: passed; `internal/app` took
  745.553s. The first run reached Go's implicit 10-minute package timeout while
  still making progress in Argon2, with no assertion or race report; the gate
  now owns an explicit 20-minute budget instead of depending on that default.
- `go vet ./...`: passed.
- frontend: 44/44 files and 605/605 tests passed; the Node 22 production build,
  typecheck, 29-file browser artifact secret scan and repeat-build bundle diff
  passed.
- observability: Go contracts plus the pinned Prometheus/Alertmanager config and
  rule validation passed (13 recording rules and 35 alert rules).

An intermediate bundle check compared an earlier Node 24 artifact with the
repository/CI Node 22 build and correctly rejected different content hashes.
Two consecutive Node 22 builds were byte-stable. The local production gate now
rejects a non-Node-22 runtime before it can mutate the committed bundle.

## Populated v0.7.1 upgrade, backup and rollback

Baseline binary: v0.7.1 commit
`84f2638e973f23935b9eda423143f65ff25a852c`, executable SHA-256
`6097e97fca962e2e490da11f71e9f8d8959a647f53ce53f25f86079bd95ace2c`.

The isolated File-mode fixture contained one credential, Provider, deployment,
route, Project and enabled Gateway Key. A request was authorized and routed to
the reserved `.invalid` endpoint, producing a controlled Provider connection
failure without contacting a real Provider.

| Stage | Result |
| --- | --- |
| v0.7.1 populated state | schema 36; 8 authenticated Ledger frames; 2 Ledger-derived and 2 Parquet Usage rows |
| pre-upgrade backup | format 3; ID `bkp_996b39f014587ff09aba0247031e0820`; archive SHA-256 `1bae9948b117f556439c6c1faf73a1caace9f45505bf27a39017e1ac94ff069d` |
| candidate read-only check before migration | refused schema 36 without opening/migrating it |
| candidate start and existing-key request | migrated to schema 37; readiness passed; 16 authenticated frames; 4 Ledger/4 Parquet rows; missing/duplicate/extra all zero |
| v0.7.1 reader after migration | failed closed on metadata schema 37 and Usage manifest schema 8 |
| candidate backup/restore | ID `bkp_be6c5cab08684c75ede0ca1f50ff9dc7`; archive SHA-256 `7fdedc9cab5bff66614c95036bc3f4a4cacdd8ed7648e4a1d59e0052f7206e0d`; restored doctor/Ledger/Usage all passed |
| rollback restore | v0.7.1 restored the verified pre-upgrade archive; schema 36, 8 authenticated frames and 2/2 Usage rows passed again |

The restore operation preserved each replaced data directory rather than
deleting it. Backup archives, keys, data directories and Gateway secrets remain
under the disposable `/private/tmp` fixture and are not committed.

The full populated backup/upgrade/old-reader-refusal/restore path is E3.
Transaction fault tests remain E2; a real OS `SIGKILL` during the sub-millisecond
schema transaction was not made deterministic and is not represented as passed.

## Admin browser journey

The production embedded bundle was exercised against the same isolated
candidate runtime:

- local admin login and dashboard;
- Provider, deployment, route, Project/Key, Developer Workbench, Usage,
  Operations/Audit and Settings pages with real server data;
- 320, 768 and 1440 CSS-pixel viewports, plus a 720 CSS-pixel viewport as the
  1440-at-200%-zoom equivalent;
- page-level horizontal overflow checks, headings, main landmark and skip link;
- a real Gateway request, Usage attribution, offline Key disable, Admin disabled
  state and a subsequent 401 from the disabled Key.

The first 1440px pass found the Provider row extending the page by 76px because
its breakpoint ignored the still-present 248px sidebar. The working tree adds
the missing intermediate collapse rule; the rebuilt production bundle then
reported `bodyScrollWidth == bodyClientWidth == 1425` and zero overflowing
elements. Existing design-system tests passed before the rebuild.

This is local browser E3. It is not an assistive-technology certification, and
the destructive UI controls were not used; Key disable was performed with the
offline CLI and verified through both UI state and Gateway behavior.

## Paired performance and stress

Host: Apple M4, Darwin arm64, Go 1.26.6, `GOMAXPROCS=4`. Eight rounds alternated
v0.7.1 and candidate order with identical fixtures and `-benchtime=500ms`.
Raw samples are in [bench-v0.7.1.txt](bench-v0.7.1.txt) and
[bench-candidate.txt](bench-candidate.txt); official x/perf output is in
[benchstat.txt](benchstat.txt).

| Benchmark | Time result | Allocation result |
| --- | --- | --- |
| Registry resolve | 11.16µs → 10.95µs, no significant change (`p=0.645`) | 15.52 → 16.31 KiB/op, +5.14%, alloc count unchanged |
| Standard redaction | 97.55µs → 98.55µs, no significant change (`p=0.279`) | no significant change |
| Rolling redaction | 32.85µs → 33.02µs, no significant change (`p=0.959`) | no significant change |
| 100k WAL replay | 871.8ms → 870.1ms, no significant change (`p=0.505`) | 269.1 → 293.6 MiB/op, +9.10%, alloc count unchanged |

The statistically significant byte increases are retained as capacity signals;
they are below the assessment's 10% time-regression threshold and are not
converted into a latency finding. The release stress case also passed with
1,000 concurrent SSE streams: peak heap about 35.6 KiB/connection, peak RSS
about 76.8 KiB/connection, and goroutines/FDs returned from 5,003/2,007 to 3/7.
These are host-specific E3 measurements, not a production SLO or 24-hour soak.

## Package and E4 boundary checks

Read-only public checks at 2026-09-13 21:47 Asia/Singapore found:

- Homebrew Formula still pins v0.7.0 release archives;
- `https://packages.halro.ai/apt/dists/stable/InRelease` returns HTTP 503 with
  `Retry-After: 300`;
- the public install page truthfully advertises v0.7.0 Homebrew as available and
  APT as “接入中”; it does not claim v0.8.0 channel acceptance.

Therefore clean-host v0.8.0 Homebrew/APT acceptance is **BLOCKED by channel
state**, not passed. The local `gh` credential is invalid, and no release App,
real Provider, AWS KMS/PKI, Alertmanager Contact Point, target environment or
production-sized 24-hour workload credential was present. Those E4 rows remain
named `UNVERIFIED`; no fixture or short stress run is promoted to E4.
