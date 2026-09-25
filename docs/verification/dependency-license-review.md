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
| `github.com/aws/aws-sdk-go-v2` | 1.47.0 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/config` | 1.33.5 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/credentials` | 1.20.5 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/service/kms` | 1.60.0 | Apache-2.0 | runtime |
| `github.com/aws/smithy-go` | 1.28.1 | Apache-2.0 | runtime |
| `github.com/go-chi/chi/v5` | 5.3.2 | MIT | runtime |
| `github.com/google/jsonschema-go` | 0.4.3 | MIT | test/release tooling |
| `github.com/parquet-go/parquet-go` | 0.32.0 | Apache-2.0 | runtime |
| `go.etcd.io/bbolt` | 1.5.0 | MIT | runtime |
| `golang.org/x/crypto` | 0.57.0 | BSD-3-Clause | runtime |
| `golang.org/x/sys` | 0.48.0 | BSD-3-Clause | runtime |
| `gopkg.in/yaml.v3` | 3.0.1 | MIT and Apache-2.0 | runtime |

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
| `@hookform/resolvers` | 5.9.1 | MIT |
| `@tanstack/react-query` | 5.102.8 | MIT |
| `i18next` | 26.4.2 | MIT |
| `qrcode` | 1.5.4 | MIT |
| `react` | 19.3.0 | MIT |
| `react-dom` | 19.3.0 | MIT |
| `react-hook-form` | 7.88.0 | MIT |
| `react-i18next` | 17.0.14 | MIT |
| `uplot` | 1.6.32 | MIT |
| `zod` | 4.6.5 | MIT |

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
| Go | `github.com/anthropics/anthropic-sdk-go` 1.74.0 | MIT |
| Go | `github.com/openai/openai-go/v3` 3.64.2 | Apache-2.0 |
| Node | `@anthropic-ai/sdk` 0.127.0 | MIT |
| Node | `openai` 7.20.0 | Apache-2.0 |
| Python | `anthropic` 1.8.0 | MIT |
| Python | `openai` 3.16.2 | Apache-2.0 |
| Python tooling | `pip-audit` 2.10.1 | Apache-2.0 |

The resolved Go compatibility graph is MIT, BSD-3-Clause, Apache-2.0, or ISC — the last of these arrived on 2026-09-26 and is described below. The
Node lock contains MIT, Apache-2.0, and Unlicense packages. The 42-package
Python lock contains MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, PSF/PSFL,
and one MPL-2.0 certificate bundle (`certifi`); all are test-only and none are
distributed in Halro artifacts. The `pip-audit` tool and its transitive packages
are deliberately in the same hash-checked lock, so the scanner is not fetched
through an unreviewed side channel during the job.

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

- `go.mod`: `d424d793437ee2b237f9c9861deaea5e76a8bd76`
- `go.sum`: `b2b51b4b4ab3cb03f792df71afa520d1b936ce43`
- `web/package.json`: `32594ca3980b25dab677bb4a54edf6ddb058c022`
- `web/package-lock.json`: `00e8554b7e862dc3507d9f2ec4b27bcce579f2df`
- `tests/compatibility/go/go.mod`: `738e441acd384f01b95150407cfad761111a63aa`
- `tests/compatibility/go/go.sum`: `e41910b1dfe53890a0f3d9caa912db7a3f00c6c7`
- `tests/compatibility/node/package.json`: `08df1b6dbf9e6b758d28cfbb16cba47d42d409f2`
- `tests/compatibility/node/package-lock.json`: `2b98506e0adbb5c5b77c96b731f752b1e0c1b105`
- `tests/compatibility/python/requirements.in`: `178571770ce0d9f229f1bd770be6cbb207089226`
- `tests/compatibility/python/requirements.txt`: `f66337c33b96dce08370cd18a6e5b99ac310dfb0`

The Go hashes last moved for the 2026-09-18 Go refresh recorded above. The two
web hashes last moved for the 2026-09-18 Admin UI refresh recorded above, before
that for the 2026-09-11 Admin UI refresh, before that for the 2026-09-05
seven-direct-package Admin UI refresh, and before that for the nine-package
Admin UI bump,
and before that only
for `chore(release): v0.2.0`, again for `v0.3.0`, again for `v0.4.0`, and again
for `v0.5.0`, `v0.6.0`, `v0.7.0`, `v0.7.1`, `v0.8.0`, `v0.8.3`, `v0.8.4`, and now `v0.8.5`, each of which bumped the
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
