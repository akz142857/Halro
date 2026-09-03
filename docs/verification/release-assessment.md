# Pre-release assessment (v0.x line)

Status: standing procedure for every v0.x tag, first applied to v0.3.0.

This is an **assessment, not a gate** (owner decision 2026-08-16: the v0.x
line has no release candidates and no CI-enforced release gates). Nothing here
blocks a tag mechanically; the output is a filled record and an explicit
go/no-go by the owner. The retired 1.0.0 gates (24h soak, full real-account
Provider matrix, signed-tag governance) stay retired; where a lightweight
descendant of one of them appears below, it is triggered by what the release
actually changed, not run unconditionally.

Run it on the exact commit the tag will point at, before pushing the tag.
`AGENTS.md` still owns day-to-day verification scope; this document only
governs the release moment.

## 0. Scope the range

Everything below is judged against the release range, not the whole tree:

```sh
PREV=$(git describe --tags --abbrev=0)
git log --oneline ${PREV}..HEAD
git diff --stat ${PREV}..HEAD
```

Classify what the range touched. This decides which deep passes run:

| Range touches | Deep pass triggered |
|---|---|
| `internal/ledger`, WAL frames, `internal/store` schemas | Recovery pass (§1c) with a populated data dir; re-check the 10 GiB replay bound if frame layout changed |
| `internal/provider/*` wire behavior, `semantic` mapping | Real-account smoke for the affected Provider(s) only (billable, opt-in; see `provider-real-matrix.md` for harness) |
| `auth`, `adminauth`, `redaction`, `contentscan`, `safetransport` | Focused security re-read of the diff against `security-review-v1.md` checklist sections |
| Request hot path (`gatewayapi`/`openaiapi`/`anthropicapi` → router → provider), `budget`, `limiter`, `tokenguard` | Benchmark comparison (§2) is mandatory, not sampled |
| `web/` only | Frontend scope + bundle drift; Go-side passes reduce to the full gate |
| Durable format of any kind (ledger frames, bbolt buckets, Parquet manifest, config schema, backup archive) | Upgrade-in-place check (§1d) is mandatory |

A release that touches none of the trigger rows still runs §1a–b, §3, §4, §5.

## 1. Defect pass (bugs)

**a. Full gate on the release commit.** `make check` (fmt, test, race, vet,
frontend tests, observability check), plus `git diff --exit-code -- internal/webui/dist`
after a fresh `make frontend` — the embedded bundle must be the one `web/src`
builds.

**b. Range review.** A code review over `${PREV}..HEAD` as one diff — not
per-PR, because bugs at the seams between PRs are exactly what per-PR review
missed. Findings triage: correctness findings on the request path, accounting,
or auth are release-blocking; everything else becomes an issue and is listed
in the record.

**c. Real-binary smoke.** Unit tests passing is not evidence that the binary
works (see CLAUDE.md, "Verify, never assume"):

```sh
make build
bin/halro start   # fresh data dir; then exercise one real request through a fake or real provider
bin/halro doctor
bin/halro ledger verify
make backup       # then restore into a scratch dir and start from it
```

**d. Upgrade-in-place.** Start the release binary against a data dir written
by the *previous released version* (keep one per release for this purpose; a
scratch copy, never the live dir). Two acceptable outcomes: it loads cleanly,
or it refuses cleanly with the fail-closed message. A refusal means the
release notes must say "requires re-initialising the data directory" — the
pre-1.0 rules allow breaking the format, never breaking it silently.

**e. Known-defect triage.** Open issues labelled as bugs, plus TODO/FIXME
introduced inside the range. Each is either fixed, or explicitly accepted in
the record with a reason.

## 2. Performance pass

Baseline: `docs/verification/performance-baseline.md` (regression baseline,
same-host comparison only).

- Re-run the baseline benchmarks on the reference host; compare with
  `benchstat`. If the published table is stale, measure both the previous tag
  and the candidate on the same host and toolchain with identical fixtures;
  an obsolete table is not a reason to omit the comparison. Archive raw samples,
  source revisions (and hashes for uncommitted code), commands and tool versions.
  Flag any hot-path regression >10% time or any new allocation on a path that
  was allocation-free in that same-host baseline. Token Guard admission and
  route resolution already allocate; compare their measured counts rather than
  describing them as allocation-free. A flagged regression is not automatically
  blocking — it is explained in the record or reverted.
- Diff scan for serialization: new locks, channels, or single-goroutine
  funnels on the request path. #13's finding stands — the first bottleneck is
  apply serialization/CPU, not fsync — so added serialization is the thing to
  catch, not added I/O.
- If ledger/WAL was touched: recovery time against a populated WAL, checked
  against the 10 GiB recovery bound.
- Frontend: compare built bundle size against the previous release; the
  release record notes the delta. Unexplained growth >10% gets a reason.
- The `stress` job in `release.yml` must be green on the release run.

## 3. Correctness pass (logic and invariants)

Walk the release diff against the invariant list — each item is answered
"untouched" or "touched, and here is why it still holds", in the record:

1. **Single-writer / single data dir** — no new path writes the data dir
   outside the owning process; no implicit shared-directory behavior.
2. **Ledger WAL is the accounting authority** — no new read path treats
   bbolt aggregates or Parquet as a balance source; reservation is durable
   before any Provider request; settlement is atomic; ambiguous outcomes are
   conservatively accounted, never refunded.
3. **Fail-closed** — every new failure path in auth, budget, redaction,
   transport, and startup refuses rather than degrades.
4. **No secrets in logs/errors/metrics/audit** — new log lines and error
   wraps in the range re-read for key material, prompts, response bodies,
   raw source IPs. The web bundle secret-canary scan passes.
5. **Determinism on replay** — new randomness / wall-clock reads on replayed
   paths are captured, not regenerated.
6. **Bounded retry/fallback** — no new retry loop is unbounded; no provider
   switch after response bytes reached the client.
7. **Capability filtering rejects, never drops** — new fields/params either
   pass through or produce an explicit rejection.
8. **Compatibility manifests** — `docs/compatibility/endpoint-manifests.json`
   reflects any facade behavior change in the range; existing wire behavior
   is backward compatible or the change is called out in release notes.
9. **Config semantics** — new keys distinguish absent from zero where that
   matters, have defaults in `default.yaml` + `config.example.yaml`, and
   carry the bilingual reference metadata the config-reference gate wants.

## 4. Design pass

- **Fix-in-place discipline** (pre-1.0): the range contains no `FooV2` beside
  `Foo`, no frozen legacy struct with a second read path, no retired-but-
  accepted placeholder. A wrong construct replaced is a construct removed.
- **Format versions**: every durable-format change bumped its version so
  stale state is refused, not misread.
- **One-way doors**: new public API fields, config keys, metric names, event
  kind numbers, frame epochs — identifiers that must never be reused — get a
  deliberate read: is this the name/shape we can live with? This is the one
  place where "it works" is not enough.
- **ADRs**: decisions in the range that changed an architectural rule have a
  record under `docs/adr/`, or the record says why not.
- **Threat surface**: new endpoints or outbound calls are inside
  `safetransport` and consistent with `docs/architecture/threat-model.md`.

## 5. Verdict and record

The record is short — one file per release, `docs/verification/assessments/vX.Y.Z.md`:

```markdown
# vX.Y.Z pre-release assessment
Range: <prev>..<sha>   Date:   Assessor:
Deep passes triggered: <from §0 table>
1 Defects:   gate <sha of run> | review findings: blocking 0, logged N (#issues)
2 Performance: benchstat summary | bundle delta | flagged: none/...
3 Invariants: <numbered list, each "untouched" or one-line justification>
4 Design:    <findings or "clean">
Re-init required: yes/no (stated in release notes: yes/no)
Verdict: GO / NO-GO — <owner>, <date>
```

**Blocking classes** (NO-GO until fixed): accounting incorrectness, fail-open
behavior, secret leakage, data loss or silent misread of existing state,
unbounded buffering/retry, silent capability drop, upgrade that breaks
without a fail-closed refusal.

**Non-blocking**: everything else — filed as issues, listed in the record,
shipped with eyes open.

## 6. Publish

The verdict is a judgement; publishing is a button. Once `chore(release): vX.Y.Z`
is on the default branch — the changelog section, the README image tags, the web
package version, and the dependency-license drift hashes that version bump moves
— open **Actions → release → Run workflow**, leave the branch on `main`, and type
the version. `dry_run` runs every gate and builds, signs and attests every
artifact while publishing nothing, which is how a release is rehearsed.

The run refuses before spending the build matrix if the version is not `vX.Y.Z`,
if that tag already exists, if the run is not on the default branch, or if
`CHANGELOG.md` has no section for it. The tag is created in the publish job, so
it appears only once every gate has passed and every artifact has been built,
signed and verified — a release that fails leaves no tag to retract and none that
a reader could mistake for a published one.

Pushing a `vX.Y.Z` tag publishes the same way, for a release driven from a
terminal. Both entry points resolve the version in the same `prepare` job, so
they cannot disagree about what is being released.
