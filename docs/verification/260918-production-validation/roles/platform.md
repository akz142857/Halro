# Platform role — G0 / G1 / G6 survey and §11 reality check

**Plan:** `docs/verification/production-validation-plan.zh-CN.md`
**Candidate SHA:** `f09ed2d768bf2cd335478223beb4d57af326013b` (verified: `git log -1 --format='%H %ci %s'` → `f09ed2d768bf2cd335478223beb4d57af326013b 2026-09-18 14:53:32 +0800 build(deps): bump the go-dependencies group with 5 updates (#302)`)
**Working tree:** dirty — `M docs/review/README.md`, `?? docs/review/milestone-professional-review-plan.zh-CN.md` (docs only)
**Mode:** read-only. No repo file created/edited/deleted. No build, no `go test ./...`, no `npm ci`, no tag/release/push. All `gh` calls were read-only queries or asset downloads into the scratchpad.
**Date of survey:** 2026-09-18

---

## 0. Headline

One **BLOCKER** was found and confirmed empirically against published artifacts, not inferred from the diff:

> **The main `halro_<version>-1_<arch>.deb` packages are absent from `checksums.txt` and carry no Sigstore signature, in every release that has ever shipped a `.deb` (v0.7.1 → v0.8.3).** The documented APT import verification path (`packaging/apt-repository/scripts/verify-release.sh`) cannot succeed against them. Details in §2.4 and Gap 1.

Also: **CLAUDE.md is stale about the release line.** It states "Releases `v0.1.0` through `v0.5.0` are published (latest `v0.5.0`, 2026-09-01)". Actual latest is **v0.8.3, 2026-09-17** (§5). The plan document's own §3 table likewise suggests `v0.8.1-rc.1` as the next candidate and warns against reusing `v0.8.0` — both predate v0.8.2 and v0.8.3.

---

## 1. G0 — 候选冻结与仓库门禁

### 1.1 What the repo's "full gate" actually consists of

There are three different things called a gate here, and they are not the same set.

**(a) The gate written in `CLAUDE.md`** (the copy an agent reads):
`go test ./...`; `go vet ./...`; `cd web && npm ci --ignore-scripts && npm run typecheck && npm test -- --run && npm run build`; `git diff --exit-code -- internal/webui/dist`.

**(b) `make check`** — `Makefile:181`:

| Target | Command | Line |
| --- | --- | --- |
| `fmt-check` | `gofmt -l ./cmd ./internal ./tools`, fail if non-empty | `Makefile:187-191` |
| `test` | `go test -count=1 -shuffle=on ./...` | `Makefile:159-160` |
| `race` | `go test -race -count=1 -timeout=20m ./...` | `Makefile:168-173` |
| `vet` | `go vet ./...` | `Makefile:175-176` |
| `frontend-test` | `cd web && npm test` (after `npm ci --ignore-scripts` stamp) | `Makefile:141-151` |
| `observability-check` | `./deploy/observability/validate.sh` | `Makefile:178-179` |

**(c) `make full-check`** — `Makefile:183` = `check` + `frontend-production-check` (`./scripts/check-web-bundle.sh`, `Makefile:152-153`).

Note the discrepancy: `make check` does **not** run `npm run typecheck`, `npm run build`, or the embedded-bundle drift check. Only `make full-check` reaches the bundle gate, and only via `scripts/check-web-bundle.sh` rather than the literal `git diff --exit-code` in CLAUDE.md. For a G0 candidate freeze the correct local invocation is `make full-check`, not `make check`.

`AGENTS.md:6-31` is the normative verification policy (CLAUDE.md defers to it, `AGENTS.md:4`): run what the change can affect; `-race` only for concurrency/lifecycle changes and only for the affected package (`AGENTS.md:24`); the full gate runs **once before the push**, not per commit (`AGENTS.md:33-53`). For G0 this is the relevant carve-out — the plan wants a full gate on the exact frozen SHA, which is precisely the "push gate" case.

### 1.2 What CI runs beyond the local gate

`.github/workflows/ci.yml` (9 jobs, triggers: push to `main` and all `pull_request`, `ci.yml:11-15`). Items **not** reachable from `make full-check`:

| Job | Beyond-local content | Line |
| --- | --- | --- |
| `repository-hygiene` | LICENSE/NOTICE/THIRD_PARTY_NOTICES present; `v1.0.0.md` absent; `git ls-files` must not match `data/`, `node_modules/`, `bin/`, `master.key`, `config.yaml`, `*.hmbk|pem|p12|pprof|prof|test`; dependency-license review currency | `ci.yml:25-37` |
| `web` | `npm audit --audit-level=moderate`, `npm run typecheck`, `npm run build`, `git diff --exit-code -- internal/webui/dist` | `ci.yml:52-58` |
| `go` | `bash -n`/`sh -n` over `tools/release/build_deb.sh`, `test_deb.sh`, 3 debian maintainer scripts, 5 `tools/m11` smoke scripts; 6 python unittest modules; `check-kms-boundaries.sh`; `check-production-assets.sh`; **`govulncheck@v1.6.0`** (`GOTOOLCHAIN=go1.26.6`); cross-build of both cmds | `ci.yml:71-94` |
| `fuzz` | 8 targets × `-fuzztime 40s`, with a stale-list guard and a one-shot retry for the fuzztime-expiry race | `ci.yml:101-184` |
| `container` | distroless image build; `halro version` inside it; `Config.User == 65532:65532`; healthcheck config exactly `["CMD","/usr/local/bin/halro","healthcheck"]`; **full init → serve → `/health/ready` → docker `healthy` on a mounted volume** | `ci.yml:186-258` |
| `observability` | compose topology assertions (Core = exactly `alertmanager prometheus`), macOS override, external-probe topology, `smoke.sh`, dead-man image non-root, **Trivy HIGH/CRITICAL vuln+secret+misconfig, `exit-code: 1`** | `ci.yml:260-296` |
| `observability-sbom` | SPDX SBOMs for digest-pinned Prometheus/Alertmanager via digest-pinned Syft | `ci.yml:298-319` |
| `deadman-sbom` | SPDX SBOM for the dead-man image | `ci.yml:321-341` |
| `sdk-compatibility` | `pip_audit`, `npm audit`, `govulncheck` on `tests/compatibility/go`; then official OpenAI Python/Node/Go SDKs black-box against `tests/compatibility/server` | `ci.yml:343-388` |

Important for G0 sequencing: **`release.yml` does not rerun fuzz.** `docs/guides/releasing.md:39-40` states this explicitly — the required exact-commit ordinary CI run is what supplies the fuzz evidence. So G0's "对精确 SHA 运行…普通 CI" is not optional bookkeeping; it is the only fuzz evidence a release has. `release.yml:66-72` enforces it: `prepare` queries `actions/workflows/ci.yml/runs?head_sha=${GITHUB_SHA}&status=success&event=push` and fails closed if no successful `main` push run exists for the exact commit.

### 1.3 Fuzz target cross-check (CI list vs. tree)

Command run: `grep -rn "^func Fuzz" --include="*.go" .` → 8 functions. CI list: `ci.yml:114-121` → 8 entries.

| Target | Declared in | CI line | Status |
| --- | --- | --- | --- |
| `FuzzEncodeDecodeRoundTrip` | `internal/sse/fuzz_test.go:14` | `ci.yml:114` | listed ✅ |
| `FuzzDecoderNeverPanics` | `internal/sse/fuzz_test.go:58` | `ci.yml:115` | listed ✅ |
| `FuzzDecodeRequestsNeverPanic` | `internal/openaiapi/fuzz_test.go:16` | `ci.yml:116` | listed ✅ |
| `FuzzDecodeInferenceResourcesRequestsNeverPanic` | `internal/openaiapi/fuzz_test.go:58` | `ci.yml:117` | listed ✅ |
| `FuzzRuleCompilerNeverPanics` | `internal/redaction/fuzz_test.go:80` | `ci.yml:118` | listed ✅ |
| `FuzzRedactNeverLeaksSeededSecret` | `internal/safelog/fuzz_test.go:18` | `ci.yml:119` | listed ✅ |
| `FuzzBoundedStreamMatchesNonStream` | `internal/redaction/fuzz_test.go:12` | `ci.yml:120` | listed ✅ |
| `FuzzRestoreCheckpointNeverPanics` | `internal/tokenguard/fuzz_test.go:10` | `ci.yml:121` | listed ✅ |

**Result: zero drift in both directions at `f09ed2d`.** No target exists in code but unlisted; no listed name has disappeared.

**But the guard is one-directional.** `ci.yml:130-133` checks *listed → exists* (`grep -rq "func ${target}(" …`, hard `::error::` if missing). Nothing anywhere checks *exists → listed*: `grep -rn "Fuzz" tools/ .github/workflows/*.yml scripts/` returns exactly one hit outside the list itself (`ci.yml:109`, a step name). So the repo-policy failure mode CLAUDE.md warns about — a **newly added** target silently never fuzzed — has no mechanical detection. It is clean today by discipline alone. See Gap 4.

### 1.4 G0 verdict on this candidate

- Exact-commit ordinary CI: **green.** `gh run list --limit 10` shows run `35316876983`, workflow `ci`, branch `main`, event `push`, conclusion `success`, 7m18s, started 2026-09-18T06:53:36Z, for commit message `build(deps): bump the go-dependencies group with 5 updates (#302)` = `f09ed2d`. All 10 most recent runs are `success`.
- Working tree is **not** frozen: two uncommitted `docs/` files. Per plan §3 the candidate must be a clean 40-char SHA; a dirty tree means a locally built binary does not identify the candidate (proved in §3 below).

---

## 2. G0 artifacts — what a release produces, how, and how a third party verifies it

There is **no** `.goreleaser*` in this repo (verified: `ls .goreleaser*` → `no matches found`). All packaging is hand-written in `.github/workflows/release.yml` (710 lines) plus `tools/release/` and `packaging/`.

### 2.1 Release entry and preconditions

`workflow_dispatch` on `main` only (`release.yml:3-19`, enforced `release.yml:62-65`). Inputs: `version`, `dry_run`, `publish_packages`. `prepare` (`release.yml:35-114`) enforces, all fail-closed:
1. event is `workflow_dispatch` and ref is the default branch (`:62-65`);
2. a successful ordinary `ci.yml` **push** run exists for the exact `GITHUB_SHA` on the default branch (`:66-72`);
3. version matches strict SemVer-with-optional-prerelease (`:73-76`);
4. an existing tag is only tolerated as a resume of the same SHA with no published release (`:77-88`);
5. `CHANGELOG.md` contains `## [<version without v>]` (`:89-93`).

13 jobs: `prepare`, `quality`, `sdk-compatibility`, `stress`, `web`, `binaries`, `container`, `debian-packages`, `provenance`, `downstream-preflight`, `publish`, `container-push`, `downstream-package-repositories` (confirmed by `grep -n` outline; matches `docs/guides/releasing.md:5-7`).

### 2.2 Artifact inventory

Confirmed against the real v0.8.3 release (`gh release view v0.8.3 --json assets`), 36 assets:

| Artifact | Count | Produced by | Line |
| --- | --- | --- | --- |
| `halro-{linux,darwin}-{amd64,arm64}.tar.gz` | 4 | `go build -trimpath -ldflags "-s -w -X …buildinfo.{Version,Commit,Date}"`, `CGO_ENABLED=0`, matrix build; bundles `halro`, `halro-deadman`, dead-man schemas + unit + receiver contract, `halro.config.example.yaml`, LICENSE/NOTICE/THIRD_PARTY_NOTICES/README; tar pinned `--sort=name --owner=0 --group=0 --numeric-owner --mtime=@SOURCE_DATE_EPOCH` piped to `gzip -n` | `release.yml:250-280` |
| `halro-container-{amd64,arm64}.tar.gz` | 2 | `docker buildx build --platform linux/<arch> --load` from root `Dockerfile`, then `docker save … \| gzip -n`; asserts `halro version` runs in-image, `Config.User == 65532:65532`, `.Architecture` matches | `release.yml:297-332` |
| `halro-deadman-container-{amd64,arm64}.tar.gz` | 2 | same, from `deploy/observability/external-probe/Dockerfile` | `release.yml:325-332` |
| `halro_<v>-1_{amd64,arm64}.deb` | 2 | `tools/release/build_deb.sh` **repacking the already-gated Linux tarballs** (no recompile), `dpkg-deb --build --root-owner-group -Zxz` | `release.yml:391-409`, `tools/release/build_deb.sh:146-149` |
| `halro-deadman_<v>-1_{amd64,arm64}.deb` | 2 | same | `tools/release/build_deb.sh:148-149` |
| `halro.spdx.json` (source/dependency SBOM) | 1 | `anchore/sbom-action` over the repo root | `release.yml:429-436` |
| `halro-binaries.spdx.json` | 1 | `anchore/sbom-action` over the **extracted released binaries** | `release.yml:437-453` |
| `halro-container-{amd64,arm64}.spdx.json`, `halro-deadman-container-{amd64,arm64}.spdx.json` | 4 | digest-pinned Syft (`anchore/syft@sha256:bd5357d2…`) against each loaded image | `release.yml:335-349` |
| `checksums.txt` | 1 | `sha256sum halro-* halro.spdx.json > checksums.txt` | `release.yml:462-464` |
| `*.sigstore.json` (Sigstore keyless bundles) | 19 | `cosign sign-blob --yes --bundle "$a.sigstore.json" "$a"` over `halro-* halro.spdx.json checksums.txt` | `release.yml:465-472` |
| GitHub build-provenance attestation (SLSA) | not a file | `actions/attest-build-provenance@v4.2.2`, `subject-path` = `release/halro-*.tar.gz`, `release/*.deb`, `release/halro*.spdx.json` | `release.yml:455-461` |
| `release-run-evidence.json` (+ its bundle) | 1 | `tools/release/run_evidence.py create` over `release/` (`rglob("*")`, `run_evidence.py:38`), cosign-signed | `release.yml:478-493` |
| GHCR multi-arch images `ghcr.io/<owner>/{halro,halro-deadman}:<tag>` and `:latest` | 4 refs | `docker load` from the **published release assets**, push per-arch, `docker buildx imagetools create` for the index | `release.yml:643-665` |

Trivy HIGH/CRITICAL `exit-code: 1` on both release images gates the build (`release.yml:350-367`).

### 2.3 How a third party verifies each

| Artifact | Third-party verification | Automated in-pipeline? |
| --- | --- | --- |
| 4 binary archives | `sha256sum --check checksums.txt`; `cosign verify-blob --certificate-identity "https://github.com/akz142857/Halro/.github/workflows/release.yml@refs/heads/main" --certificate-oidc-issuer https://token.actions.githubusercontent.com --bundle <a>.sigstore.json <a>`; `gh attestation verify <a> --repo akz142857/Halro`. Also **byte-reproducible from the tag** (`releasing.md:444-450`) | Yes — `release.yml:573-590` runs all three before publishing |
| 4 container tarballs | same three; **not byte-reproducible** (`releasing.md:452-470`, floating base tags + layer timestamps) | Yes |
| `halro-deadman_*.deb` | same three | Yes |
| **`halro_*.deb`** | **attestation only.** No checksum line, no Sigstore bundle — see §2.4 | attestation yes; checksum/signature **no** |
| SBOMs | checksum (`halro.spdx.json`, `halro-binaries.spdx.json`, container SBOMs all match `halro-*` or are named explicitly) + cosign + attestation | Yes |
| `checksums.txt` | `cosign verify-blob --bundle checksums.txt.sigstore.json` (no attestation — confirmed: `gh attestation verify checksums.txt --repo akz142857/Halro` → exit 1, HTTP 404, which is the expected control) | Yes |
| GHCR images | digest equality against the release tarballs; `sha256sum --check --ignore-missing checksums.txt` before push (`release.yml:652`) | Yes |
| `release-run-evidence.json` | cosign bundle; ties run id/attempt/commit/ref/workflow-ref to the digests | Signed, but **not published as a release asset** — it is a 90-day workflow artifact (`release.yml:495-504`); v0.8.3's asset list does not contain it |

### 2.4 Confirmed defect — main `.deb` is unchecksummed and unsigned

Evidence chain, all empirical:

1. `gh release view v0.8.3 --json assets` lists `halro-deadman_0.8.3-1_amd64.deb` **and** `halro-deadman_0.8.3-1_amd64.deb.sigstore.json`, but lists `halro_0.8.3-1_amd64.deb` and `halro_0.8.3-1_arm64.deb` with **no `.sigstore.json` companion**.
2. Downloaded the real `checksums.txt` for v0.8.3 (`gh release download v0.8.3 --pattern checksums.txt`). It has 16 lines. **Neither `halro_0.8.3-1_amd64.deb` nor `halro_0.8.3-1_arm64.deb` appears.** `halro-deadman_0.8.3-1_{amd64,arm64}.deb` both do.
3. Root cause is a glob/underscore mismatch: `release.yml:464` `sha256sum halro-* halro.spdx.json > checksums.txt` and `release.yml:470` `for artifact in halro-* halro.spdx.json checksums.txt`. Debian filenames are `<package>_<version>_<arch>.deb` (`tools/release/build_deb.sh:146-149` → `halro_${package_version}_${arch}.deb`). `halro-*` (hyphen) matches `halro-deadman_*`, `halro-linux-*`, `halro-darwin-*`, `halro-container-*` but **never** `halro_*`.
4. The in-pipeline publish gate does not catch it: `release.yml:575` `sha256sum --check checksums.txt` only checks listed lines; `release.yml:576-581` `gh attestation verify` iterates `release/*.deb` so it *does* cover the file; `release.yml:582-590` `cosign verify-blob` iterates `release/halro-*` and so skips it. Every gate passes.
5. Attestation coverage confirmed positive: `gh attestation verify halro_0.8.3-1_amd64.deb --repo akz142857/Halro` → **exit 0**, valid Sigstore bundle returned with `--format json`. Control: same command on `checksums.txt` → exit 1, HTTP 404. So the attestation is real and the control is meaningful.
6. **Downstream breakage:** `packaging/apt-repository/scripts/verify-release.sh:32-39` loops `for package in "$download_dir"/*.deb` and runs `cosign verify-blob … --bundle "${package}.sigstore.json" "$package"`. For `halro_*.deb` that bundle file does not exist → hard failure under `set -euo pipefail` (`verify-release.sh:2`). And `verify-release.sh:29` uses `sha256sum --check --ignore-missing`, which silently skips a file that is not listed — so the checksum omission produces no warning at all, only a silent non-check. Per `packaging/apt-repository/README.md:13-14, 20-23`, only a successful `verify-release.sh` output directory may be passed to `import-release.sh`.
7. **Scope:** not a v0.8.3 regression. Checked `checksums.txt` for v0.7.0, v0.7.1, v0.8.0, v0.8.1, v0.8.2 — `grep -c "halro_"` returns **0** for every one, while v0.7.1/v0.8.0/v0.8.1/v0.8.2 each ship `halro_<v>-1_{amd64,arm64}.deb` assets (v0.7.0 shipped no `.deb`). So **every Debian package release to date** has this hole.

### 2.5 Automated vs manual

**Automated** (one `workflow_dispatch`): every build, SBOM, Trivy scan, checksum, cosign signature, provenance attestation, in-pipeline verification, annotated tag creation (after gates), GitHub Release creation, GHCR multi-arch push, and the `repository_dispatch` fan-out to `halro-ai/homebrew-tap` and `halro-ai/apt-repository`.

**Manual / procedural, enforced by nothing in the workflow** (`docs/guides/releasing.md:42-45`): CHANGELOG *completeness* (only section existence is checked, `release.yml:89-93`); the pre-release assessment under `docs/verification/assessments/`; any second pair of eyes. The go/no-go is the owner's (`releasing.md:13-19`; v1.0.0 governance gates — `v1-release` environment, `release-governance` preflight, signed-tag requirement, `M11_RELEASE_EVIDENCE_JSON` — were **retired for the v0.x line on 2026-08-16** and are explicitly *not* in today's workflow; `releasing.md:415-420` says the tools exist and their unit tests run in CI but nothing calls them).

**Manual one-time setup** (`releasing.md:95-126`): the GitHub App across 4 repos with Contents/Issues/PRs read-write; `HALRO_RELEASE_APP_CLIENT_ID` / `HALRO_RELEASE_APP_PRIVATE_KEY` in each; the protected `apt-production` environment with `HALRO_APT_ARCHIVE_SIGNING_KEY` + fingerprint; the cluster `ghcr-credentials` pull secret. `releasing.md:209-211` is explicit that **a green dry run does not prove any of these credentials are configured**, because a dry run never executes `publish`, `container-push`, or the downstream jobs.

---

## 3. Version traceability

**Storage:** `internal/buildinfo/buildinfo.go:3-7` — package-level vars `Version = "dev"`, `Commit = "unknown"`, `Date = "unknown"`; `Current()` at `:15-21`.

**Command:** `cmd/halro/main.go:377-378` — `case "version": return json.NewEncoder(os.Stdout).Encode(versionReport())`. `versionReport()` at `cmd/halro/main.go:125-135` returns `{"build": buildinfo.Current(), "tzdata": …}` (tzdata deliberately folded into the same question — `cmd/halro/main.go:121-124`). Registered in the help table at `cmd/halro/main.go:201`.

**ldflags:**
- Local: `Makefile:23-24` — `-X …buildinfo.Version=$(RELEASE_VERSION) -X …Commit=$(RELEASE_COMMIT) -X …Date=$(RELEASE_DATE)`, where `RELEASE_VERSION = git describe --tags --always --dirty` (`Makefile:19`), `RELEASE_COMMIT = git rev-parse --short HEAD` (`Makefile:20`), `RELEASE_DATE` honours `SOURCE_DATE_EPOCH` (`Makefile:22`). Applied at `Makefile:133` (`bin/halro`).
- Release: `release.yml:264-268` — `RELEASE_VERSION` = `needs.prepare.outputs.version`, `RELEASE_COMMIT` = `${{ github.sha }}` (**full 40-char SHA**), `RELEASE_DATE` derived from `git show -s --format=%ct` of that commit so it is deterministic.
- Container: same three passed as `--build-arg` (`release.yml:310-313`; `Dockerfile:16-18` declares `ARG RELEASE_VERSION/RELEASE_COMMIT/RELEASE_DATE`), and the image is smoke-tested with `halro version` at `release.yml:317`.

**Observed, on this candidate:** `./bin/halro version` →
```json
{"build":{"version":"v0.8.3-11-gf09ed2d-dirty","commit":"f09ed2d","date":"2026-09-18T07:22:57Z"}, "tzdata":{…"version":"2026b"…}}
```

**Can it be tied back to a candidate SHA?**
- **Release builds: yes, exactly.** `commit` is the full 40-char `github.sha`, and `gh attestation verify` independently binds each artifact to that same commit (`release.yml:576-580` pins `--source-digest ${GITHUB_SHA}` and `--source-ref refs/heads/main`).
- **Local `make build`: partially.** `Commit` is the *short* SHA (`Makefile:20`), which does not satisfy plan §3's "完整 40 位 Git SHA" without a separate mapping step. And the `-dirty` suffix above is the honest signal that this tree is **not** a frozen candidate.

**Traceability gap — `halro-deadman` has no version at all.** `release.yml:269-272` builds it with `-ldflags "-s -w"` only — no `-X` stamps. And `cmd/halro-deadman/` (single `main.go`) contains **no** occurrence of `version`, `Version`, or `buildinfo` (verified by `grep -rn "version\|Version" cmd/halro-deadman/` → no output; `grep -ln buildinfo` over the tree does not list it). So the dead-man binary — shipped in all four archives, in its own `.deb`, and as its own container — reports no version and offers no command to ask. G0 pass criterion 4 ("验证运行时版本信息能回指同一 SHA") is satisfiable for `halro` and **not** for `halro-deadman`. See Gap 2.

---

## 4. G6 downstream — Homebrew tap and APT repo

### 4.1 Publishing path

`release.yml:666-710` `downstream-package-repositories`: mints a short-lived App token scoped to `owner: halro-ai`, `repositories: homebrew-tap, apt-repository`, `permission-contents: write` (`:680-688`), validates that `RELEASE_COMMIT` is a full 40-hex SHA (`:695-698`), then POSTs `repository_dispatch` `event_type: halro-release-published` with `{version, commit, release_url, run_url}` to both (`:699-710`). It runs only if `publish` and `container-push` both succeeded and `publish_packages == true` (`:668-673`).

`release.yml:506-548` `downstream-preflight` runs **early**, as a sibling of the expensive jobs (comment at `:507-509`), with `permissions: {}` — no ambient `GITHUB_TOKEN`. It asserts the App vars/secrets are non-empty (`:517-524`) and that `.permissions.push == true` on both repos (`:536-548`), read-only.

### 4.2 Homebrew

- **Tap repo:** `halro-ai/homebrew-tap` — verified public via `gh repo view halro-ai/homebrew-tap --json name,visibility,pushedAt` → `{"name":"homebrew-tap","pushedAt":"2026-09-06T16:34:19Z","visibility":"PUBLIC"}`.
- **Reviewed source mirrored in-repo** at `packaging/homebrew/` (`packaging/homebrew/README.md:1-9`: "the tap owns only Formula metadata and must never rebuild or replace a published Halro release artifact").
- **Update job:** `packaging/homebrew/.github/workflows/update.yml` — `repository_dispatch: [halro-release-published]` (`:10-11`), validates the version regex (`:25-30`), downloads **`checksums.txt` from the immutable release** (`:31-32`), runs `ruby scripts/update-formula.rb` (`:33-34`), opens/updates PR `package/<version>` (`:35-52`). Because the tap reads `checksums.txt` and the four `halro-*.tar.gz` lines **are** present there, Homebrew is unaffected by the §2.4 defect.
- **Tap CI:** `packaging/homebrew/.github/workflows/test.yml` — matrix `macos-14` and `ubuntu-24.04`; `brew style`, `brew audit --strict`, `brew install`, `brew test`, `halro version` (`:16-34`).

### 4.3 APT

- **Control-plane repo:** `halro-ai/apt-repository` — verified **PRIVATE** via `gh repo view` → `{"name":"apt-repository","pushedAt":"2026-09-06T16:27:59Z","visibility":"PRIVATE"}`. Public tree served at `https://packages.halro.ai/apt` (`packaging/apt-repository/README.md:3-6`).
- **Verify-then-import:** `scripts/verify-release.sh` downloads `*.deb`, `*.deb.sigstore.json`, `checksums.txt` + bundle; verifies the checksum file's cosign bundle, `sha256sum --check --ignore-missing`, then per-`.deb` `gh attestation verify` + `cosign verify-blob` (`verify-release.sh:18-39`). Only its successful output may feed `import-release.sh` (`README.md:20-23`). **This is the path §2.4 breaks.**
- **Control-plane CI:** `packaging/apt-repository/.github/workflows/ci.yml:17-25` — `bash -n` + `shellcheck` on both scripts, and asserts the bootstrap signing placeholder is still in `conf/distributions` (i.e. the real fingerprint is *not* configured in the mirrored source).
- **Pre-publication prerequisites, all unmet in-repo** (`packaging/apt-repository/README.md:8-18`): replace `HALRO_ARCHIVE_SIGNING_KEY_FINGERPRINT`; configure a protected publishing environment and secret manager; import only verified `.deb`s; upload immutable pool/index first and `dists/stable/InRelease` last; complete clean-host amd64+arm64 acceptance before advertising. Also: "The final object-store upload is intentionally deployment-specific: configure it only after the storage provider, atomic publication strategy, and rollback snapshot location have been approved."

### 4.4 Provable locally vs. requires a clean host

**Provable from this laptop, read-only:**
- Which assets a release actually carries, and their checksums/signature companions (done, §2.2/§2.4).
- That `halro_*.deb` lacks a checksum line and a Sigstore bundle, across 5 releases (done).
- That `halro_*.deb` **does** carry a valid SLSA provenance attestation (done, with a negative control).
- The exact workflow logic, glob shapes, permission scopes, and dispatch payloads (done).
- That both downstream repos exist and their visibility (done).
- `halro version` output shape and ldflags plumbing (done).
- Tap Formula content in the in-repo mirror.

**Requires a clean host of each supported OS/arch — cannot be produced here:**
- Plan §G6 step 4: install/version/start/upgrade/rollback/uninstall on never-installed **macOS Apple Silicon**, **macOS Intel**, and supported **Debian/Ubuntu amd64 and arm64**. This machine is Darwin 25.6.0 arm64 only, and is not clean — Halro is developed here (`data/`, `master.key`, `config.yaml` all present in the repo root).
- Plan §G6 step 5: that `brew install halro-ai/tap/halro` and the documented `apt` install actually **resolve to** the new version. `releasing.md:310-326` gives the exact commands (`curl https://packages.halro.ai/apt/repository-version.json`, `curl …homebrew-tap/main/Formula/halro.rb | grep '/releases/download/v0.8.0/'`) and warns not to infer client availability from a green package job.
- Whether `halro-ai/homebrew-tap@main` currently advertises v0.8.3. **UNVERIFIED** — the in-repo mirror `packaging/homebrew/Formula/halro.rb:7-23` is pinned to **v0.7.0** with v0.7.0 SHA-256 values, i.e. three releases behind. That mirror is a reviewed source copy, not the served tap, so its staleness proves nothing about the live tap either way; it does mean the repo cannot answer the question. See Gap 5.
- Whether `packages.halro.ai/apt` is published at all, and with which signing key. **UNVERIFIED** — private repo, deployment-specific upload step, and the mirrored `conf/distributions` still carries the placeholder fingerprint.
- Byte-reproducibility of the container archives: known **not** reproducible (`releasing.md:452-470`), root `Dockerfile:3,10,28` uses floating tags `node:22-bookworm-slim`, `golang:1.26.6-bookworm`, `gcr.io/distroless/static-debian12:nonroot`, while `deploy/observability/external-probe/Dockerfile:1,19` is digest-pinned. Measured 2026-08-12: same commit, same `SOURCE_DATE_EPOCH`, different image IDs, identical `halro` binary inside.

---

## 5. Live GitHub state (commands run, not assumed)

`gh release list`:

| Release | Tag | Published |
| --- | --- | --- |
| **Halro v0.8.3 (Latest)** | `v0.8.3` | **2026-09-17T17:51:32Z** |
| Halro v0.8.2 | `v0.8.2` | 2026-09-17T07:04:00Z |
| Halro v0.8.1 | `v0.8.1` | 2026-09-16T07:42:57Z |
| Halro v0.8.0 | `v0.8.0` | 2026-09-11T12:30:46Z |
| Halro v0.7.1 | `v0.7.1` | 2026-09-07T07:19:40Z |
| Halro v0.7.0 | `v0.7.0` | 2026-09-06T11:04:20Z |
| Halro v0.6.0 | `v0.6.0` | 2026-09-03T16:30:22Z |
| Halro v0.5.0 | `v0.5.0` | 2026-09-01T07:32:57Z |
| v0.4.0 / v0.3.0 / v0.2.0 / v0.1.0 | — | 2026-08-28 / 08-24 / 08-19 / 08-15 |

12 releases. `gh release view v0.8.3` → `isDraft: false`, `targetCommitish: main`, 36 assets.

Tag→commit (`git ls-remote --tags origin`): `v0.8.1^{} = 4a97a4fb…`, `v0.8.2^{} = 85bfb780…`, `v0.8.3^{} = 9a5113bcfae1488122613cde20b61526338bc836`. All annotated (distinct tag object and peeled commit), consistent with `release.yml:607` `git tag -a`.

`gh run list --limit 10`: 10/10 `completed success`; 5 `ci` runs on `main` (push) and 4 `ci` runs on dependabot PR branches, plus one `Dependabot Updates`. Most recent main run: `35316876983`, 7m18s, 2026-09-18T06:53:36Z, for `f09ed2d`.

`gh workflow list`: `ci`, `publish signed model catalog`, `publish-ghcr`, `release`, `Dependabot Updates` — all active.

**Consequence for the plan:** CLAUDE.md's "latest `v0.5.0`, 2026-09-01" is wrong by 8 releases / 16 days, and the plan's §3 candidate guidance (`v0.8.1-rc.1`, "不得移动或复用 `v0.8.0` 标签") is written against a world where v0.8.1 did not yet exist. `f09ed2d` is 11 commits past `v0.8.3` and all 11 visible in `git log` are dependency bumps. A candidate version must be chosen fresh — `v0.8.4` or `v0.9.0-rc.1`, never any of the 12 published tags (`release.yml:77-88` will refuse anyway).

---

## 6. §11 当前启动清单 — nine items

Legend: **READY** = satisfiable now with what is in the repo/session. **NOT READY** = a concrete artifact or decision is missing and someone must produce it. **REQUIRES EXTERNAL AUTHORIZATION** = blocked on a human authority or spend approval that plan §4.2 says cannot be inferred from "we are executing this plan".

| # | Item | Status | Reason (evidence) | What unblocks it |
| --- | --- | --- | --- | --- |
| 1 | 冻结 `CANDIDATE_SHA`、候选版本和制品 digest | **NOT READY** | SHA is knowable (`f09ed2d768bf…`) but the tree is **dirty** (2 uncommitted `docs/` files) and the local binary proves it: `version = v0.8.3-11-gf09ed2d-dirty`. Candidate *version* is undecided and the plan's suggestion (`v0.8.1-rc.1`) is already consumed — v0.8.1/2/3 are published (§5). **ARTIFACT_DIGESTS do not exist**: no release run has been made from this SHA, and per `releasing.md:384-385` container digests cannot be computed ahead of a run. | Commit or stash the docs edits; pick an unused version (≥ v0.8.4); run `release.yml` with `dry_run=true` on the frozen SHA and record the digests it produces. |
| 2 | 填写全部服务目标、容量阈值、费用上限和 RPO/RTO | **NOT READY** | Plan §5's table is **nine rows of `TBD`** (`production-validation-plan.zh-CN.md:78-91`), including p50/p95/p99, error/timeout/throttle budgets, CPU/RSS/goroutine/FD ceilings, Ledger/Audit/Parquet/TSDB growth, provider cost, and RPO/RTO. §5 forbids adjusting thresholds after seeing results. | Application/SRE/Product/Platform fill the table **before first measurement**; absent business SLOs, all four sign a time-boxed provisional admission threshold, as §5 requires. Baselines exist to draw on: `docs/verification/performance-baseline.md`, `standalone-capacity-baseline.md`. |
| 3 | 确认真实 Provider 测试账户、声明矩阵和计费授权 | **REQUIRES EXTERNAL AUTHORIZATION** | Plan §4.2 lists billable provider calls as needing separate authorization. `AGENTS.md:30`: never run billable real-provider smoke tests unless explicitly requested. Harness exists (`tests/provider-matrix`, `docs/verification/provider-real-matrix.md`) and is opt-in and never enabled in ordinary CI. No account, credential, or spend cap is in the repo — correctly, since none may be. | Per-provider dedicated test accounts with least-privilege credentials, synthetic data, and a **hard** cost ceiling; a named authorizer, scope, cap, window and rollback recorded before G2 starts. |
| 4 | 准备隔离目标环境、KMS/PKI/Secret Store/RBAC 与故障窗口 | **REQUIRES EXTERNAL AUTHORIZATION** | Plan §4.1 requires an isolated environment matching production topology/TLS/IAM/KMS/Secret Store/network policy/storage class. This session has a developer laptop with a live dev `data/` + `master.key`. Deploy *templates* exist and are testable (`deploy/kubernetes/*.yaml` incl. `manifests_test.go`, bootstrap/init/verify jobs, default-deny NetworkPolicy, `halro-aws-kms.yaml`; `deploy/systemd/halro-aws-kms.service`; `deploy/observability/` compose + `validate.sh` + `smoke.sh`) — templates are not an environment. Real-AWS KMS smokes under `tools/m11/` are billable and out of scope for CI. | Provision the isolated target, record `TARGET_ID` and `CONFIG_DIGEST` (plan §3), and obtain an authorized fault-injection window (§4.2 bars disk-fill / network-block / kill / cert-revoke / restore drills outside an isolated environment). |
| 5 | 配置真实 Contact Point 和独立故障域 dead-man | **REQUIRES EXTERNAL AUTHORIZATION** | §4.2 explicitly bars sending alerts to real people/Pager/Slack/email without separate authorization. The dead-man exists and is CI-verified as an image (`ci.yml:282-296`, non-root + Trivy) and its compose topology validates (`ci.yml:277-279`) — but by design it must live **outside Halro's own failure domain**, which no laptop can provide. Compounding: the dead-man binary has **no version command at all** (§3), so "which build is the watchdog running" is unanswerable in the field. | Authorized real contact point with verified `firing` **and** `resolved`; dead-man deployed in a genuinely separate failure domain with its own receiver; ideally fix the dead-man version stamp first (Gap 2). |
| 6 | 确认不可变证据存储及敏感数据处理规则 | **NOT READY** | Tooling exists — `tools/release/run_evidence.py` (cosign-signed manifest over `release/` via `rglob`, `:38`) and `scripts/archive-release-run.sh` (downloads run metadata, logs, assets, evidence manifest and verifies the binding, `releasing.md:473-478`). But the manifest is a **90-day workflow artifact, not a release asset** (`release.yml:495-504`; absent from v0.8.3's 36 assets), and no immutable external store is named or configured anywhere in the repo. | Designate and provision the immutable evidence store; define the rule that the repo records only evidence IDs, digests and non-sensitive conclusions (§4.1); archive each run **inside** the 90-day retention window. |
| 7 | 确认 GitHub App、Homebrew、APT、GHCR 权限和干净主机矩阵 | **NOT READY**, with a **BLOCKER** attached | Permission *checks* are automated and fail-closed (`release.yml:506-548`), and both repos exist (homebrew-tap PUBLIC, apt-repository PRIVATE). But: (a) `releasing.md:209-211` — a green dry run proves **none** of these credentials; the preflight only runs on a publishing run; (b) whether the App, `apt-production` environment, signing key and `ghcr-credentials` are actually configured is **UNVERIFIED** from here — it needs `gh secret list` / `gh variable list` on four repos, one of them private; (c) the mirrored `conf/distributions` still holds the placeholder fingerprint (`apt-repository/.github/workflows/ci.yml:22-25`); (d) **§2.4 — the main `halro_*.deb` has no checksum line and no Sigstore bundle, so `verify-release.sh` cannot pass, so the APT channel cannot legitimately import the main package.** No clean-host matrix is possible from this machine. | Fix the `halro-*` glob first (Gap 1) and re-verify on a fresh release; then run the `releasing.md:181-206` credential inventory; then stand up the four clean hosts. |
| 8 | 指定 Application、Security、SRE、Platform 签署人及非作者演练人员 | **REQUIRES EXTERNAL AUTHORIZATION** | Naming four sign-off owners plus a non-author drill operator is an organizational decision. Nothing in the repo names them. Note `releasing.md:42-45`: the v0.x pipeline enforces **nobody other than the tagger** has looked at a release; the four-party sign-off exists only as procedure (`docs/verification/release-assessment.md`), and the v1.0.0 reviewer environment was retired on 2026-08-16 (`releasing.md:13-19`). | Named individuals recorded per role, plus a drill operator who did not implement the change (plan §G7 step 1: 15-minute controlled drill from the runbook alone). |
| 9 | 预约连续 24 小时浸泡与至少一个修复/重验证窗口 | **NOT READY** | Harness exists (`go run ./tests/soak …` against a running instance, `docs/verification/soak-testing.md`; `tests/` are `main` packages so `go test ./...` never reaches their real work). A 24-hour soak needs item 4's isolated environment and item 2's event budget, neither of which exists. And any fix during the window re-freezes the candidate and re-runs the affected gates (plan §6 preamble). | Book a contiguous 24h window on the isolated target **plus** at least one remediation/re-verification window, after items 2 and 4 land. |

**Net: 0 of 9 READY.** Four are blocked on external authority (3, 4, 5, 8) and five on work that has not been done (1, 2, 6, 7, 9). Item 7 additionally carries a blocking product defect. Per §11's closing sentence, E1–E3 preparation may continue, but no billable call, no destructive experiment, no formal release, and no claim that production validation is complete.

---

## 7. Gaps, most severe first

1. **BLOCKER — the main Halro `.deb` is neither checksummed nor signed, in every release since v0.7.1.** `release.yml:464` and `:470` glob `halro-*`; Debian names the file `halro_<v>-1_<arch>.deb` (`tools/release/build_deb.sh:147`). Confirmed against the real v0.8.3 `checksums.txt` (16 lines, no `halro_` entry) and the real asset list (no `halro_*.deb.sigstore.json`); reconfirmed for v0.7.1, v0.8.0, v0.8.1, v0.8.2. The provenance attestation *is* present (verified exit 0, with a 404 negative control on `checksums.txt`), so the only surviving binding is SLSA. Effects: `packaging/apt-repository/scripts/verify-release.sh:36-38` hard-fails on the missing bundle under `set -euo pipefail`; `:29`'s `--ignore-missing` silently no-ops the checksum; the release's own publish gate misses it because `release.yml:576` attests `release/*.deb` while `:582` signs only `release/halro-*`. **Plan §G6 step 3 cannot pass today.** Fix: make the checksum and cosign loops enumerate `*.deb` explicitly (or glob `halro*`), and add an assertion that every published asset except `*.sigstore.json` appears in `checksums.txt`. Re-release before any APT acceptance.

2. **HIGH — `halro-deadman` carries no version identity.** Built with `-ldflags "-s -w"` only (`release.yml:269-272`); `cmd/halro-deadman/main.go` contains no `buildinfo` reference and no `version` subcommand. It ships in all four archives, its own `.deb`, and its own container. G0 criterion 4 ("运行时版本信息能回指同一 SHA") is unmet for half the shipped surface, and during an incident the watchdog's own build is unidentifiable. Fix: stamp the same three `-X` values and add a `version` command before G0 is signed.

3. **HIGH — the signed release-run evidence manifest is not published.** `run_evidence.py` hashes everything in `release/` (`:38`), so it *does* cover `halro_*.deb` — it is the only signed artifact that does. But it is uploaded as a 90-day workflow artifact (`release.yml:495-504`), not a release asset (absent from v0.8.3's 36). A third party cannot reach it, and after 90 days neither can anyone. This is also §11 item 6's missing half. Fix: attach it to the release, or name and provision the immutable evidence store and archive every run with `scripts/archive-release-run.sh` inside the window.

4. **MEDIUM — the fuzz-list guard is one-directional.** `ci.yml:130-133` fails when a *listed* target is missing, but nothing detects a *new* `Fuzz*` function that was never added to `ci.yml:114-121` — and `go test -fuzz` exits 0 when its pattern matches nothing, which is the exact silent failure CLAUDE.md documents. Verified clean today (8/8, §1.3), by discipline only. Fix: assert `grep -rl "^func Fuzz" internal/` yields exactly the listed set.

5. **MEDIUM — the in-repo Homebrew Formula mirror is three releases stale.** `packaging/homebrew/Formula/halro.rb:7-23` pins v0.7.0 URLs and SHA-256s while v0.8.3 is current. The mirror is documented as the reviewed source for the live tap (`packaging/homebrew/README.md:1-6`), so the repo can no longer answer "what does the tap serve" — and a reviewer reading the mirror would review the wrong Formula. Whether `halro-ai/homebrew-tap@main` itself is current is **UNVERIFIED** (requires fetching the live tap or a clean-host `brew install`). Fix: re-sync the mirror, or state explicitly that it is a bootstrap artifact and not kept current.

6. **MEDIUM — CLAUDE.md and the plan both describe a release line that no longer exists.** CLAUDE.md says "latest `v0.5.0`, 2026-09-01"; actual latest is v0.8.3, 2026-09-17 (§5). The plan's §3 proposes `v0.8.1-rc.1` as the candidate version and warns against reusing `v0.8.0` — both already published. CLAUDE.md's own instruction is "Check `gh release list` before assuming what exists"; the file itself did not. Fix: correct both before the candidate version is chosen, so nobody freezes against a consumed tag.

7. **LOW/known — container archives are not byte-reproducible.** `releasing.md:452-470`; root `Dockerfile:3,10,28` uses floating base tags while `deploy/observability/external-probe/Dockerfile:1,19` is digest-pinned; `SOURCE_DATE_EPOCH` reaches the image config but not layer timestamps. Measured 2026-08-12: differing image IDs, byte-identical inner binary. The four `halro-<os>-<arch>.tar.gz` **are** reproducible. Consequence for G6 step 3: digest equality must be asserted from the published assets, never from an independent rebuild of the images.

8. **LOW — `make check` is not the gate CLAUDE.md describes.** `Makefile:181` omits `npm run typecheck`, `npm run build`, and the embedded-bundle drift check; only `make full-check` (`:183`) reaches them. A G0 operator running `make check` would believe they ran the full gate. Fix: point the G0 instruction at `make full-check`, or fold `frontend-production-check` into `check`.

9. **INFO — the v0.x pipeline has no second pair of eyes, by decision.** `releasing.md:13-19, 42-45`: the `v1-release` environment, `release-governance` preflight, signed-tag requirement and `M11_RELEASE_EVIDENCE_JSON` verification were retired for v0.x on 2026-08-16; the tools still exist and their unit tests still run in CI (`ci.yml:84-89`) but nothing calls them (`releasing.md:415-420`). Neither CHANGELOG completeness nor a filled assessment is enforced. Not a defect — a recorded owner decision — but plan §G7's four-party sign-off is therefore **entirely procedural** and has no mechanical gate behind it.

### UNVERIFIED (stated as such, not assumed)

- Whether the release GitHub App, its four installations, `HALRO_RELEASE_APP_*`, the `apt-production` environment, `HALRO_APT_ARCHIVE_SIGNING_KEY`/`_FINGERPRINT`, and the cluster `ghcr-credentials` are actually configured — needs `gh secret list`/`gh variable list` across four repos including a private one.
- Whether `packages.halro.ai/apt` is published, and with which archive-signing key.
- Whether `halro-ai/homebrew-tap@main` currently advertises v0.8.3.
- Whether GHCR carries `ghcr.io/<owner>/halro:v0.8.3` and `:latest` at the expected digests.
- Any clean-host install/upgrade/rollback/uninstall behaviour on macOS arm64, macOS amd64, or Debian/Ubuntu amd64/arm64 — this host is Darwin arm64 and is a development machine, not clean.
