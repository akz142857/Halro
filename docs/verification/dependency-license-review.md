# Dependency and License Review

Date: 2026-09-11

Halro is distributed under Apache-2.0. The source tree includes the project
license in `LICENSE`, required attribution in `NOTICE`, and the runtime
inventory in `THIRD_PARTY_NOTICES.md`. Release archives and the container carry
those files together with a versioned SPDX SBOM.

The root module currently declares 12 direct Go dependencies. Eleven are linked
into the `halro` runtime; `github.com/google/jsonschema-go` is used by tests and
release validation only. The embedded Admin UI has 10 direct runtime npm
dependencies. Versions below are the exact versions pinned by the reviewed
module and lock files.

## Go direct dependencies

| Module | Version | License | Distribution scope |
|---|---:|---|---|
| `github.com/aws/aws-sdk-go-v2` | 1.47.3 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/config` | 1.33.9 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/credentials` | 1.20.9 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/service/kms` | 1.61.5 | Apache-2.0 | runtime |
| `github.com/aws/smithy-go` | 1.28.5 | Apache-2.0 | runtime |
| `github.com/go-chi/chi/v5` | 5.5.0 | MIT | runtime |
| `github.com/google/jsonschema-go` | 0.4.3 | MIT | test/release tooling |
| `github.com/parquet-go/parquet-go` | 0.32.0 | Apache-2.0 | runtime |
| `go.etcd.io/bbolt` | 1.5.0 | MIT | runtime |
| `golang.org/x/crypto` | 0.58.0 | BSD-3-Clause | runtime |
| `golang.org/x/sys` | 0.49.0 | BSD-3-Clause | runtime |
| `gopkg.in/yaml.v3` | 3.0.1 | MIT and Apache-2.0 | runtime |

The 2026-10-23 refresh (Dependabot #460) moves
`github.com/aws/aws-sdk-go-v2/service/kms` 1.61.4 to 1.61.5 and
`github.com/go-chi/chi/v5` 5.4.0 to 5.5.0. Only those two modules move in
`go.mod` and `go.sum`, the module-path set is unchanged, and both LICENSE files
are byte-identical across the bump, compared in the module cache. chi's root
package, which is all the runtime links, is byte-identical between the two
releases; 5.5.0 changes only `chi/middleware` (a new `Deprecation` middleware
and edits to `ContentCharset`, `Maybe`, `Sunset`, `SupressNotFound` and
`Throttle`), which Halro does not import.

The 2026-10-21 refresh moves the `go` directive in `go.mod` from 1.26.9 to
1.26.10 for GO-2026-6809 in `crypto/x509`, which govulncheck reaches through
`internal/safetransport`. No module was added, removed, upgraded or
relicensed; `go.sum` is unchanged. The same refresh moves `certifi` in the
Python compatibility lock from 2026.7.22 to 2026.10.19 (PYSEC-2026-4320); it
stays MPL-2.0 and test-only, and only its two hashes change, both checked
against the published PyPI wheel and sdist.

A second 2026-10-21 refresh moves the transitive `github.com/klauspost/compress`
(reached through `parquet-go`'s zstd codec in `internal/usage`) from 1.17.9 to
1.18.7 for CVE-2026-63209, reported by the release workflow's Trivy scan of the
Halro image. No other module moves in `go.mod` or `go.sum`, and the module's
LICENSE file is byte-identical across the bump (BSD-3-Clause, with its bundled
MIT and Apache-2.0 notices unchanged).

The 2026-10-15 refresh changes only the `go` directive in `go.mod`, from
1.26.6 to 1.26.9, for standard-library fixes in `net/http` and `crypto/tls`.
No module was added, removed, upgraded or relicensed; `go.sum` is unchanged.
The same refresh moves the transitive `source-map-js` in
`web/package-lock.json` from 1.2.1 to 1.2.2 (GHSA-68fv-2mgg-jv7q); it stays
BSD-3-Clause and is a build/test-time dependency of Vite and jsdom.

The 2026-08-28 refresh moved six direct versions and added, removed, and
relicensed nothing: the module path sets in `go.mod` and `go.sum` are identical
to the reviewed tree before it, so every row above is a version change rather
than an inventory change, and every one of the six module LICENSE files is
byte-identical across its bump. Five are AWS SDK and smithy patch releases on
the KMS custody path described below. The sixth is `github.com/go-chi/chi/v5`
5.3.1 to 5.3.2, which is the one that moves behaviour and not only a version:
it de-duplicates the method list chi records behind a 405, and it changes what
`Routes()` and `Walk()` report for a `Mount()` stub handler. The runtime links
only the root `chi` package and not `chi/middleware`, where the rest of that
release landed, and the frozen Admin route contract — which enumerates the
router through `chi.Walk` and compares it against an exact expected set — still
matches.

The 2026-09-05 AWS refresh then moved the five AWS SDK and smithy rows above,
plus ten version-pinned AWS transitive modules, and again added, removed, and
relicensed nothing. The old and new LICENSE files are byte-identical for all 15
changed modules; the root AWS SDK and smithy NOTICE files are also
byte-identical. The shared transport changes move content-length calculation
into `SetStream`, add an opt-in connection read timeout, and fix the logger
middleware insertion point. The KMS client also moves credential-source user
agent attribution from per-request middleware to client construction. Halro
does not set `AWS_ENABLE_DEFAULT_SOCKET_TIMEOUT_2026`; its KMS Encrypt/Decrypt
surface, encryption context, retry policy, error classification, and file-mode
no-cloud-call boundary remain covered by the KMS and application tests.

The 2026-09-11 Go refresh moved four direct AWS SDK modules, nine
version-pinned AWS transitive modules, `golang.org/x/crypto`, and
`golang.org/x/sys`. It added, removed, and relicensed nothing: the module path
sets in `go.mod` and `go.sum` are unchanged. The AWS SDK modules remain
Apache-2.0, while the two Go subrepositories remain BSD-3-Clause. The KMS
custody path, credential discovery boundary, cryptographic helpers, and
platform syscall surface remain covered by the full Go and compatibility test
suites.

The 2026-09-18 Go refresh moved the same four direct AWS SDK modules, nine
version-pinned AWS transitive modules, and `golang.org/x/crypto`. It again
added, removed, and relicensed nothing: the module path sets in `go.mod` and
`go.sum` are unchanged. The AWS modules remain Apache-2.0 and `x/crypto`
remains BSD-3-Clause. The KMS custody path, Bedrock SigV4 vectors, and Argon2
administrator password path remain covered by focused tests and the full CI
gate.

The 2026-10-17 Go refresh (Dependabot #420) moved the four direct AWS SDK
modules, `github.com/aws/smithy-go`, ten version-pinned AWS transitive modules,
`golang.org/x/crypto`, `golang.org/x/sys`, and `github.com/go-chi/chi/v5` 5.3.2
to 5.4.0. It added, removed, and relicensed nothing: the module path sets in
`go.mod` and `go.sum` are unchanged, and every LICENSE and NOTICE file of the 18
moved modules is byte-identical across its bump, compared in the module cache.
chi is the one minor release. In the root package, which is all the runtime
links, it adds `Context.Clone`, panics when a router is mounted onto itself
(including an inline `With()`/`Group()` mux sharing its tree), and otherwise
rewrites loops with `slices`. Halro mounts no router onto itself; every router
is built by the full Go suite, and the frozen Admin route contract still
matches.

The AWS KMS custody path is part of this review. The linked AWS SDK config and
credential modules can resolve environment, shared-file, web-identity,
ECS/container, and EC2 IMDS workload credentials when Key Slot mode creates the
AWS adapter. File-mode startup is separately tested not to initialize the AWS
SDK or perform cloud calls. The SDK and smithy NOTICE text is carried in the
root `NOTICE`; all exact runtime modules, including transitive parquet helpers,
are listed in `THIRD_PARTY_NOTICES.md` and the release SBOM.

Resolved Go runtime modules were checked from the module cache. Licenses are
permissive MIT, BSD, or Apache-2.0; no GPL, AGPL, LGPL, MPL, SSPL, or BUSL module
is linked into the Go runtime.

## Admin UI direct runtime dependencies

| Package | Version | License |
|---|---:|---|
| `@hookform/resolvers` | 6.0.0 | MIT |
| `@tanstack/react-query` | 5.104.5 | MIT |
| `i18next` | 26.5.3 | MIT |
| `qrcode` | 1.5.4 | MIT |
| `react` | 19.3.1 | MIT |
| `react-dom` | 19.3.1 | MIT |
| `react-hook-form` | 7.90.0 | MIT |
| `react-i18next` | 17.1.0 | MIT |
| `uplot` | 1.6.32 | MIT |
| `zod` | 4.6.7 | MIT |

The same 2026-08-28 refresh moved five of the rows above — `@hookform/resolvers`
5.7.1 to 5.9.1, `@tanstack/react-query` 5.101.4 to 5.102.3, `i18next` 26.3.6 to
26.4.0, `react-hook-form` 7.85.0 to 7.86.0, `react-i18next` 17.0.11 to 17.0.12 —
and every one stayed MIT. It also moved four dev dependencies that are absent
from the table because they are not shipped: `@types/react-dom`,
`@vitejs/plugin-react`, `vite` 8.2.1 to 8.2.2 and `vitest`. The
`@hookform/resolvers` minor releases adopt Joi 18 and Vest 6 in resolvers this
project does not import, and neither package is in the lockfile, so no entry was
added to the dependency surface. The build-tooling bump did rewrite
`internal/webui/dist` — chunk contents and hashed names moved, and one
auto-named shared chunk regrouped — with no change to which packages reach the
bundle.

The 2026-09-26 Admin UI refresh moved two runtime dependencies —
`@tanstack/react-query` 5.102.8 to 5.103.2 and `react-i18next` 17.0.14 to
17.0.15 — and two dev-only ones absent from the table because they are not
shipped: `jsdom` 30.0.1 to 30.1.0 and `vitest` 5.0.0 to 5.0.1.

Nineteen lockfile entries moved version in total once transitives are counted,
and **no license changed on any of them**; the comparison was made entry by
entry against the previous lock rather than by reading the four direct bumps,
because a transitive is exactly where a license would change unnoticed. The set
of licenses in the lock is unchanged — MIT throughout, plus the MIT-0 and
BlueOak-1.0.0 entries that were already there. One package left the lock:
`symbol-tree`, which `jsdom` 30.1.0 no longer depends on. Nothing new entered
it.

`internal/webui/dist` was rebuilt under Node 22, the version the gate uses, and
is committed with this change. Chunk contents and hashed names moved; the
initial bundle went from 246109 to 246119 bytes gzipped, and the browser
artifact secret scan reports 29 files clean.

The 2026-09-05 Admin UI refresh then moved five runtime dependencies:
`@tanstack/react-query` 5.102.3 to 5.102.8, `i18next` 26.4.0 to 26.4.1,
`react-hook-form` 7.86.0 to 7.87.0, `react-i18next` 17.0.12 to 17.0.13, and
`zod` 4.4.3 to 4.5.4. It also moved the dev-only
`@testing-library/react` 16.3.2 to 16.3.3 and `@vitejs/plugin-react` 6.1.0 to
6.1.1. The lockfile package-node set is unchanged; the only transitive version
movement is `@tanstack/query-core` alongside `@tanstack/react-query`. All eight
changed package entries remain MIT, their installed packages carry MIT license
files, and none changed distribution scope. The embedded Admin UI bundle was
rebuilt from the reviewed lockfile so its content-hashed assets match the
source dependency tree.

The 2026-09-11 Admin UI refresh moved the runtime `i18next` package from
26.4.1 to 26.4.2 and the dev-only `@types/react-dom` from 19.2.5 to 19.2.7
and `vitest` from 4.1.11 to 5.0.0. All three remain MIT licensed. The Vitest
major update changes only test tooling; its lockfile adds the MIT-licensed
`@jridgewell/resolve-uri` and `@jridgewell/trace-mapping` packages and removes
four dev-only MIT packages. No runtime package was added, removed, or
relicensed, and the embedded Admin UI bundle was rebuilt from the updated
lockfile.

The 2026-09-18 Admin UI refresh moved the runtime `react` and `react-dom`
packages from 19.2.8 to 19.3.0, `react-hook-form` from 7.87.0 to 7.88.0,
`react-i18next` from 17.0.13 to 17.0.14, and `zod` from 4.5.4 to 4.6.5. It also
moved the dev-only `@types/react` from 19.2.18 to 19.3.0,
`@types/react-dom` from 19.2.7 to 19.3.0, and `vite` from 8.2.2 to 8.3.0. All
eight remain MIT licensed, the 193-node lockfile package set is unchanged, and
the embedded Admin UI bundle was rebuilt from the reviewed lockfile.

The 2026-10-17 Admin UI refresh (Dependabot #419) moved five runtime
dependencies — `@tanstack/react-query` 5.103.2 to 5.104.2 (with
`@tanstack/query-core`), `i18next` 26.4.2 to 26.4.4, `react-hook-form` 7.88.0 to
7.90.0, `react-i18next` 17.0.15 to 17.0.16, and `zod` 4.6.5 to 4.6.7 — and the
dev-only `@vitejs/plugin-react`, `jsdom`, `vite` and `vitest`. Compared entry by
entry against the previous lock, 53 entries moved version and none changed
license or distribution scope; all six runtime entries remain MIT. Three
dev-only entries left the lock (`siginfo` ISC, `stackback` MIT, and the nested
`jsdom/node_modules/whatwg-url` MIT) and nothing entered it.
`internal/webui/dist` was rebuilt under Node 22 and committed with this change;
the initial bundle is 246994 bytes gzipped and the browser artifact secret scan
reports 29 files clean.

The 2026-10-23 Admin UI refresh (Dependabot #459) moved six runtime
dependencies — `@hookform/resolvers` 5.9.1 to 6.0.0, `@tanstack/react-query`
5.104.2 to 5.104.5 (with `@tanstack/query-core`), `i18next` 26.4.4 to 26.5.3,
`react` and `react-dom` 19.3.0 to 19.3.1, and `react-i18next` 17.0.16 to
17.1.0 — and the dev-only `jsdom` and `vitest`. Compared entry by entry against
the previous lock, 13 entries moved version, none changed license, and the
lockfile package set is unchanged. `@hookform/resolvers` 6.0.0 is a major only
for the resolvers this project does not import: it moves the `effect` peer to
4 and edits the `ajv` resolver. Its `zod` resolver, the only one imported, and
its shared core are byte-identical to 5.9.1 in the published tarballs, as is
its LICENSE. `internal/webui/dist` was rebuilt under Node 22 and committed with
this change; the initial bundle is 248642 bytes gzipped and the browser
artifact secret scan reports 30 files clean.

The Admin UI lockfile contains no CC-BY package. Its 12 MPL-2.0 entries are
`lightningcss` 1.33.0 plus eleven platform-specific optional binaries. They are
dev-only CSS build tooling and are not present in the generated Admin UI bundle
or final container. Source and build environments still retain their upstream
license metadata; this review does not relabel them as runtime dependencies.

The independent packages under `tests/compatibility/` install official SDK test
clients in CI. They are excluded from the embedded Admin UI inventory and are
not copied into release archives or the runtime container. Their lock files and
licenses remain part of the source/CI dependency surface and the corresponding
SDK jobs continue to audit them.

## Official SDK compatibility dependencies

The compatibility clients are now a first-class CI supply-chain surface. Their
Go module and npm package/lock files, plus the Python direct input and fully
hashed transitive lock, are included in the drift gate below. Both ordinary CI
and the release workflow run ecosystem-native vulnerability checks before the
black-box contracts: `govulncheck` for the nested Go module, `npm audit` for the
Node lock, and `pip-audit` for the complete hashed Python lock.

| Ecosystem | Reviewed direct dependencies | License |
|---|---|---|
| Go | `github.com/anthropics/anthropic-sdk-go` 1.82.0 | MIT |
| Go | `github.com/openai/openai-go/v3` 3.88.0 | Apache-2.0 |
| Node | `@anthropic-ai/sdk` 0.135.0 | MIT |
| Node | `openai` 7.45.0 | Apache-2.0 |
| Python | `anthropic` 1.15.0 | MIT |
| Python | `openai` 3.35.1 | Apache-2.0 |
| Python tooling | `pip-audit` 2.10.1 | Apache-2.0 |

The resolved Go compatibility graph is MIT, BSD-3-Clause, Apache-2.0, or ISC — the last of these arrived on 2026-09-26 and is described below. The
Node lock contains MIT, Apache-2.0, and Unlicense packages. The 43-package
Python lock contains MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, PSF/PSFL,
and one MPL-2.0 certificate bundle (`certifi`); all are test-only and none are
distributed in Halro artifacts. The `pip-audit` tool and its transitive packages
are deliberately in the same hash-checked lock, so the scanner is not fetched
through an unreviewed side channel during the job.

The 2026-09-30 main CI recovery moved the transitive `urllib3` pin from 2.7.0
to 2.8.0 after `pip-audit` reported CVE-2026-97687 and CVE-2026-97689 with
2.8.0 as the fix. The wheel and source archive hashes match the published PyPI
release. `urllib3` remains MIT licensed, the 42-package lock set is unchanged,
and this dependency is installed only for SDK compatibility tests, not shipped
in Halro artifacts.

The 2026-10-24 PR #467 CI recovery moved `filelock` in the Python compatibility lock
from 3.32.6 to 4.2.4 (CVE-2026-112907, CVE-2026-112913 and CVE-2026-112940,
fixed in 4.2.1). `filelock` is reached only through `cachecontrol` for `pip-audit`,
stays MIT and test-only, and requires Python 3.11 or later, which CI's 3.12
meets. Only its pin and two hashes changed, both checked against the published
PyPI wheel and sdist; `requirements.in` is unchanged.

The 2026-09-18 Go compatibility refresh moved
`github.com/anthropics/anthropic-sdk-go` from 1.71.0 to 1.72.0. The module
remains MIT licensed, the resolved module-path set is unchanged, and this SDK
is used only by the compatibility contracts rather than the shipped runtime.

The 2026-09-18 OpenAI Go compatibility refresh moved
`github.com/openai/openai-go/v3` from 3.56.0 to 3.61.0. The module remains
Apache-2.0 licensed, the resolved module-path set is unchanged, and this SDK
is used only by the compatibility contracts rather than the shipped runtime.

The 2026-09-26 Anthropic Python compatibility refresh moved `anthropic` from
1.5.0 to **1.8.0**, not to the 1.7.0 its pull request is titled after: the bot
regenerated the lock after the OpenAI bump landed, and 1.8.0 had been released
by then. The package remains MIT, read from the installed distribution's
metadata, the 42-package lock set is unchanged, and the SDK stays confined to
compatibility CI. The lock is the one Dependabot's own run produced, for the
reason given below.

The 2026-09-26 OpenAI Python compatibility refresh moved `openai` from 3.14.0
to 3.16.2. The package remains Apache-2.0, read from the installed
distribution's metadata rather than from the bot's summary, the 42-package lock
set is unchanged, and the SDK remains confined to compatibility CI.

This one was not combined with the `anthropic` bump beside it, unlike the Go
and Node pairs. The Python lock is fully hashed and compiled by `uv`, and
recompiling it here resolved a different package set — `cachecontrol` and `pip`
appeared — because this machine's `uv` and platform are not the ones that
produced the committed file. Rewriting a hashed lock with a different toolchain
would change the dependency set as a side effect of a version bump, which is
the thing this review exists to catch. So each Python bump keeps the lock its
own Dependabot run generated, and they land one after another.

The 2026-09-26 runtime refresh moved `github.com/aws/aws-sdk-go-v2/service/kms`
from 1.60.0 to 1.61.0 and `github.com/aws/smithy-go` from 1.28.1 to 1.28.2.
Both LICENSE files are byte-identical across their bumps and the resolved
module-path set is unchanged. Unlike the compatibility SDKs, these two are in
the shipped binary: the KMS client is the named exception to "SafeTransport is
the only path to the network" that ADR 0010 records, so a change in its
transport reaches production rather than only CI. Nothing moved in either
`go.mod` except its own version line, and the KMS boundary, Master Key and
Vault suites were run against the result.

The 2026-09-26 Go compatibility refresh moved
`github.com/anthropics/anthropic-sdk-go` from 1.72.0 to 1.74.0 and
`github.com/openai/openai-go/v3` from 3.61.0 to 3.64.2, taken together because
both edit the same two files and neither can land without the other conflicting.
Both LICENSE files are byte-identical across their bumps, and both SDKs remain
confined to the compatibility contracts rather than the shipped runtime.

**The resolved module-path set changed this time**, which every previous
refresh here has been able to say it did not. `github.com/coder/websocket`
1.8.15 is now an indirect dependency, pulled in by the OpenAI SDK's realtime
surface. It is ISC licensed — the full text is the standard ISC permission
grant with no additional terms — which is why the paragraph above now names a
fourth license. It reaches no Halro artifact: this module is a separate
`go.mod` outside `./...`, built only by the SDK compatibility job, and Halro's
own realtime paths do not use it.

The 2026-09-26 Node compatibility refresh moved `@anthropic-ai/sdk` from
0.125.0 to 0.127.0 and `openai` from 7.15.0 to 7.20.0, taken together for the
same reason as the Go pair: both edit the same two files. `@anthropic-ai/sdk`
remains MIT and `openai` remains Apache-2.0, read from the installed packages
rather than from the bot's summary, the lockfile package set is unchanged, and
both stay confined to compatibility CI. Both packages are pinned exactly, with
no range, which is this module's convention and was preserved.

The 2026-10-17 compatibility refresh took the three SDK pairs together
(Dependabot #409 and #413 for Go, #412 and #414 for Node, #416 and #418 for
Python), each pair editing the same files. Go: `anthropic-sdk-go` 1.74.0 to
1.80.0 and `openai-go/v3` 3.64.2 to 3.78.0, with the indirect
`golang.org/x/sync` 0.16.0 to 0.23.0; all three LICENSE files are byte-identical
across their bumps and the module-path set is unchanged. Node: `@anthropic-ai/sdk`
0.127.0 to 0.133.0 (MIT) and `openai` 7.20.0 to 7.33.0 (Apache-2.0), pinned
exactly, lock regenerated with `npm install`; the lockfile package set is
unchanged. Python: `anthropic` 1.8.0 to 1.13.0 (MIT) and `openai` 3.16.2 to
3.31.0 (Apache-2.0), licenses and dependency lists read from PyPI metadata and
unchanged. The hashed lock was not recompiled here, for the reason below: each
Dependabot run changed only its own package's block, and the two blocks were
applied side by side, leaving the 42-package set unchanged.

The 2026-10-23 compatibility refresh again took the three SDK pairs together
(Dependabot #453 and #455 for Go, #454 and #457 for Node, #456 and #458 for
Python). Go: `anthropic-sdk-go` 1.80.0 to 1.82.0 and `openai-go/v3` 3.78.0 to
3.82.0, bumped together with `go get`; Node: `@anthropic-ai/sdk` 0.133.0 to
0.135.0 (MIT) and `openai` 7.33.0 to 7.39.0 (Apache-2.0), pinned exactly, lock
regenerated with `npm install`; Python: `anthropic` 1.13.0 to 1.15.0 (MIT) and
`openai` 3.31.0 to 3.35.1 (Apache-2.0), each Dependabot run's lock block applied
side by side as before, installed with `--require-hashes` and `pip check`
clean. Every SDK LICENSE is unchanged across its bump.

**The dependency sets grew this time, in all three ecosystems, for one reason:
the Anthropic SDKs now depend on the OpenTelemetry API.** In Go,
`go.opentelemetry.io/otel`, `otel/metric`, `otel/trace`, `go.opentelemetry.io/auto/sdk`,
`github.com/go-logr/logr` and `go-logr/stdr` are now compiled into the
compatibility tests, all Apache-2.0 per their LICENSE files in the module
cache; `go.sum` also gained hashes for the rest of the SDK's module graph (its
AWS, Azure, Google Cloud, gRPC and MCP integrations), which the tests do not
build — `go list -deps -test` names exactly the six above as new. Dependabot's
own #455 produced the same graph. In Node, `@opentelemetry/api` 1.10.0
(Apache-2.0) entered the lock, also present in Dependabot's own #457 lock. In
Python, `opentelemetry-api` 1.45.1 (Apache-2.0, read from the installed
distribution's metadata) entered the hashed lock, which is now 43 packages. No
new license family entered any lock, and none of it reaches a Halro artifact.

The 2026-09-18 Node compatibility refresh moved `@anthropic-ai/sdk` from
0.124.0 to 0.125.0. The package remains MIT licensed, the lockfile package set
is unchanged, and the SDK remains confined to compatibility CI.

The 2026-09-18 OpenAI Node compatibility refresh moved `openai` from 7.10.0
to 7.15.0. The package remains Apache-2.0 licensed, the lockfile package set is
unchanged, and the SDK remains confined to compatibility CI.

The 2026-09-18 Python tooling refresh moved `pip-audit` from 2.9.0 to
2.10.1. The resolved lock remains 42 packages: it replaces the MIT-licensed
`pip` and `toml` entries with the MIT-licensed `tomli` and `tomli-w` entries,
and adds no new license family or distribution obligation. This tooling remains
confined to compatibility CI and is not included in Halro release artifacts.

The 2026-09-18 Python compatibility refresh moved `anthropic` from 1.4.0 to
1.5.0. The package remains MIT licensed, and applying the reviewed wheel hashes
to the `pip-audit` 2.10.1 lock leaves the 42-package transitive set unchanged.
The client remains test-only and is not distributed in Halro artifacts.

The 2026-09-18 OpenAI Python compatibility refresh moved `openai` from 3.8.0
to 3.14.0. The package remains Apache-2.0 licensed, and the 42-package
transitive set is unchanged. The client remains test-only and is not
distributed in Halro artifacts.

The 2026-10-30 PR #471 refresh moves the test-only OpenAI Go SDK from
3.82.0 to 3.88.0. Only its version and checksums move; the module-path set
is unchanged. The SDK remains Apache-2.0 and outside Halro runtime artifacts.
The same repair moves the test-only `msgpack` pin from 1.2.2 to 1.2.3 for
CVE-2026-114613 reported by CI. The replacement block uses the SHA-256 hashes
of all 91 published PyPI artifacts, preserves the rest of the hashed lock,
and retains the Apache-2.0 license reported by the distribution metadata.

The 2026-10-30 PR #472 refresh moves the test-only Node `openai` SDK from
7.39.0 to 7.45.0. The lockfile package set is unchanged and its license stays
Apache-2.0. Its new `google-auth-library` peer is optional and does not enter
this lock. The SDK remains confined to compatibility tests.

## Distribution requirements

- Preserve the project license plus dependency copyright/license notices in
  binary archives and container images.
- Carry the AWS SDK for Go and smithy-go upstream NOTICE text, plus Apache-2.0
  notices for Parquet and Apache-covered YAML portions.
- Generate the SPDX SBOM from the final clean release tree; this review is not
  a substitute for the artifact-specific SBOM.
- Keep `LICENSE`, `NOTICE`, and `THIRD_PARTY_NOTICES.md` in every binary archive
  and container image.
- Repeat this review whenever any dependency input below changes.

## Drift gate

CI runs `scripts/check-dependency-license-review.sh`. These are Git blob hashes
of the reviewed dependency inputs; a dependency change cannot pass until this
document is deliberately refreshed with the new inventory and hashes.

- `go.mod`: `3a6ae3889cf47baf9d350666bd7c8e1112d5de50`
- `go.sum`: `7e3df8d6a1c7322fc9b3c15e7772097d211b5911`
- `web/package.json`: `0f2ea89317ab04c46436babda5aebc00ec79a4b6`
- `web/package-lock.json`: `58e6b3935d3082a80a9d709d9d4914ad03c1fb11`
- `tests/compatibility/go/go.mod`: `1585b96dbac31039469d2424f806ff31c2b3f756`
- `tests/compatibility/go/go.sum`: `0a6c7841b02c48b247a861c31f27bd166c296784`
- `tests/compatibility/node/package.json`: `e60579e1af1b4b8a43b00e01c21facc547110ba2`
- `tests/compatibility/node/package-lock.json`: `af95e720b52bb21cde9610ec8ab9c99af651a5eb`
- `tests/compatibility/python/requirements.in`: `e7aead8aed67406d553563a198a17af1a6b1ba96`
- `tests/compatibility/python/requirements.txt`: `054fb003126c0f0ece5b942acaf2f4bc6c5db5e6`

The Python lock hash last moved for the 2026-10-24 `filelock` refresh. All ten
hashes moved before that for the 2026-10-23 refreshes recorded above, and
before that for the 2026-10-17 refreshes. Before
that the Go hashes moved for the 2026-09-18 Go refresh, and the two web hashes
for the 2026-09-18 Admin UI refresh, before
that for the 2026-09-11 Admin UI refresh, before that for the 2026-09-05
seven-direct-package Admin UI refresh, and before that for the nine-package
Admin UI bump,
and before that only
for `chore(release): v0.2.0`, again for `v0.3.0`, again for `v0.4.0`, and again
for `v0.5.0`, `v0.6.0`, `v0.7.0`, `v0.7.1`, `v0.8.0`, `v0.8.3`, `v0.8.4`, `v0.8.5`, `v0.9.0`, `v0.9.1`, `v0.9.2`, `v0.10.0`, and now `v0.10.1`, each of which bumped the
`version`
field in both files and changed nothing else.
Nothing in any of it added, removed, or relicensed a dependency, so the
inventory above still describes the reviewed tree. The gate hashes whole files
rather than dependency sections, which is the right trade: it cannot be talked
out of noticing a change, at the cost of occasionally flagging one that carries
no dependency in it.

CI also runs `govulncheck`, npm audits, bundle scanning, repository notice
checks, and artifact SBOM generation. Those checks complement license review;
none individually replaces it.
