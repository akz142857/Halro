# Halro 1.0.0 review remediation progress

Updated: 2026-08-12

This file is the mutable remediation ledger for
`release-1.0.0-report.md`. The report is frozen evidence and is not edited as
issues are fixed. Findings are interpreted together with the V1–V6
adjudications; a finding that was refuted or narrowed is not restored to its
pre-adjudication severity here.

## Status vocabulary

- `TODO`: repository work has not started.
- `IN PROGRESS`: implementation or verification is underway.
- `EXTERNAL`: requires GitHub environment, secret, approval, tag, release, or
  other state outside this checkout.
- `DECISION`: requires an explicit product or release-policy choice.
- `DONE`: implementation and the directly affected checks are complete.
- `VERIFIED`: the final release gate or external acceptance evidence is also
  complete.

## Report errata

- Section 8 says there are 18 known limitations, but the numbered list contains
  20. R-07 must use all 20 and rewrite/remove the stale-related entries after
  R-03/R-04 close.
- The report's repository-status placeholder predates this remediation ledger.
  This file is intentionally added without changing the frozen report.
- V2 narrowed B1-01 to the single bad `data_dir` instruction in
  `docs/guides/operator-guide.md`. Dockerfile, systemd, and Kubernetes path
  layouts must not be changed as part of R-08.
- V3 refuted the data-plane bypass claim for the Beta capability ceiling; it is
  not a 1.0.0 blocker.
- V6 refuted the broad billing claim and retained only the narrow typed-unsent
  SafeTransport defect.
- Report §9.2 item 7: B3's two evidentiary arguments are withdrawn — "no
  `waiting` deployment status implies no configured reviewers" (`waiting` is not
  a legal deployment status, so the argument reasons from the absence of an
  impossible state) and "7 seconds means it failed on the first line of
  `test -n`" (that line is inside the fourth step, after checkout, an 80–100 MB
  artifact download, and a cosign install). The conclusion stands on the
  replacement evidence V5 supplied: `created_at 23:35:08Z → in_progress
  23:35:12Z`, a 4-second gap no four-party approval can fit inside, plus the two
  preconditions that are verifiably absent today. B3-03 is also out of the G4
  determination as a category error — G4 is about verifiable signatures, not
  byte-identical third-party rebuilds.
- Report §9.3 item 13 and `carry-forward.md` row 5 both stated that
  `provider_metadata` exists only as an enum value and validation, with no
  adapter emitting it. That is factually wrong: at the review HEAD `2cd24a7`,
  `internal/provider/gemini/adapter.go:251`,
  `internal/provider/bedrock/models.go:153`, and
  `internal/provider/anthropic/adapter.go:192` all emit
  `domain.ClaimSourceProviderMetadata`, each covered by its package's
  `DescribeInvocationTargets` tests. The claim source is therefore not a
  defined-but-unemitted placeholder, and the report's recommendation to "either
  retire the enum value or implement it" rests on a premise that does not hold.
  This sub-item needs no adjudication; the other two parts of G7 #13 (browser
  acceptance, real-Provider evidence) still do. `carry-forward.md` row 5 carries
  the same correction inline.
- Report §9.2 item 10, recorded as a method lesson rather than a finding: A5
  routed the stale question to A1 by prior agreement and therefore did not
  independently review the round's heaviest defect. Independence fails silently
  wherever a routing agreement exists. A future round's routing note should read
  "you still review it independently; you just do not file it twice."

## 2026-08-12 verification round

Seven independent read-only roles re-checked every item against the report's
own closure criteria rather than against this ledger, with defect-state reverse
verification (`go build -overlay`, scratchpad fixtures, containers). The
findings and the evidence are in
[remediation-verification.md](remediation-verification.md); everything they
found unmet has since been fixed and is marked there. The load-bearing ones:

- **R-24 had shipped a regression.** The Origin check added to two credential-
  spending GETs cannot pass in a browser (no `Origin` on a same-origin GET,
  `Referrer-Policy: no-referrer` on this router), and the Go tests only passed
  because the shared GET helper had been given an `Origin` header. Both
  endpoints are now POSTs behind `requireAdminMutation`; the helper's header is
  gone, and a test asserts console-shaped reads succeed with neither header.
- **R-03 had closed two of its three clauses.** The lookup-miss fail-open in
  redaction and Token Guard was neither changed nor adjudicated. Both now fail
  closed for a named-but-absent policy, with a single up-front refusal in the
  Gateway, and the deliberate opposite direction (alert delivery must not refuse
  traffic) has the negative test it was missing.
- **R-30 had introduced a metrics silence.** A failed counter read aborted the
  whole exposition after 200 had been implied, which would have muted
  `HalroConfigurationStale` in one of the failures it exists to catch.
- **R-13's criterion asked for a measurement** and the repository had an
  argument. Measured: 64 concurrent logins now cost 256 MiB of peak heap versus
  roughly 4 GiB unbounded.

## Release blockers and P1 closure

| ID | Status | Scope | Acceptance summary |
|---|---|---|---|
| R-01 | EXTERNAL | GitHub release governance | Repository preflight now prevents an unprotected Environment from being auto-created; live `v1-release` reviewers, self-review prevention, and Environment-secret scope still require GitHub configuration. |
| R-02 | DONE | Docs | Publish a complete, copyable cosign verification sequence that also works for a single-platform download. |
| R-03p | PARTIAL | Release workflow | Binary archives are byte-reproducible (measured). Container archives are not, and the scope is now stated in `docs/guides/releasing.md` with the two causes and the pipeline change deferred to an RC that can exercise it. |
| R-04p | EXTERNAL | Release evidence | The workflow now emits a signed run/attempt/commit/artifact manifest plus 90-day artifacts, with a read-only archive verifier; an actual retained rc run is still required. |
| R-03 | DONE | Go runtime/tests | Track stale state per activation domain, recover all four independently, and adjudicate both lookup-miss paths to fail closed. |
| R-04 | DONE | Go/Web/observability/docs | Add stale metrics, alert/test, runbook, and Console status; every activation domain keeps its row and the panel names the recovery path. |
| R-05 | DONE | Release notes | State the rc.1 hard cutover and destructive one-way migration behavior. |
| R-06 | DONE | CHANGELOG | Document migrations 24–27, Admin idempotency, and private-provider endpoint behavior. |
| R-07 | DONE | Release notes | Publish all 20 adjudicated known limitations, each carrying the specifics (error codes, bounds, measured capacity, the zero-settlement guarantee) rather than a summary. |
| R-08 | DONE | Operator guide | Correct only the container `data_dir` instruction. |
| R-09 | DONE | Operator guide | Document externally reachable container TLS/insecure-listen choices and health semantics, including the `gateway_listen` change the example requires. |
| R-10 | DONE | Go/tests/Web | Make onboarding readiness recognize an already successful route, and make each cascaded detail code name what is actually missing. |
| R-11 | DONE | Web/tests | Give five replay codes actionable localized UX; Gateway Key replay must explain plaintext loss. |
| R-12 | DONE | Contract/release notes | Separate optional data-plane idempotency from required Admin create idempotency. |
| R-13 | DONE | Go/tests/docs | Bound process-wide concurrent Argon2 verification, with the measured 64-concurrency figure archived in the performance baseline. |
| R-14 | DONE | License review/CI | Refresh dependency review and add drift protection. |
| R-15 | DONE | Security evidence | Update the incremental security review and its obsolete gates/crash evidence. |
| R-16 | DONE | Packaging/docs | Keep archive delivery, explicitly require an operator-controlled registry, and retain a non-deployable Kubernetes image placeholder. |
| R-17 | DONE | Go/tests | Distinguish unwritable parents, uninitialized data directories, and a genuinely held lock, asserted through the real doctor. |
| R-18 | DONE | Operator guide/Go | Add the container initialization and Master Key state flow, correct the bind-mount ownership step, and make both `init` failures actionable in the error text itself. |
| R-19 | DONE | Observability/docs | Alert on signed catalog degradation, capability drift, and detection failures. |
| R-20 | DONE | Runbooks/example | Mark M11 release-blocked, repair the catalog example, and add `--config` to break-glass commands. |

## Follow-up remediation

| ID | Status | Scope | Acceptance summary |
|---|---|---|---|
| R-21 | DONE | CI | Exercise volume initialization and readiness in the container job. |
| R-22 | DONE | Go/tests | Retry pending Admin audit intents using runtime-owned context. |
| R-23 | DONE | Metrics contract | Document audit-anchor families and validate a static exporter inventory. |
| R-24 | DONE | Go/Web/tests | Move both provider-spending side channels to POST behind role, CSRF, and Origin; plain reads stay GET and stay reachable from the console. |
| R-25 | DONE | Observability | Add the two promised alert rules or weaken the normative contract, not both. |
| R-26 | DONE | Observability docs/CI | Give platform alerts anchored runbooks and validate links. |
| R-27 | DONE | Observability smoke | Derive the notification budget from `group_interval`. |
| R-28 | DONE | Protocol manifests | Declare unknown-field rejection and clarify the SDK gate boundary. |
| R-29 | DONE | Gateway contract/code | Specify stale semantics, use protocol-correct envelopes, and consider `Retry-After`. |
| R-30 | DONE | Config/runtime/metrics/docs | Configure a shutdown budget no shorter than the route timeout and durably count attempts remaining at forced close. |
| R-31 | DONE | Notices | Carry AWS SDK/Smithy notices and missing third-party packages. |
| R-32 | DONE | Release + CI workflows | SHA-pin every action reference in both workflows and gate on it in `check-production-assets.sh`. |
| R-33 | DONE | Signed catalog/release notes | Keep dynamic signed catalog inactive in 1.0.0; zero trust roots and bundled-catalog fallback are the documented expected state. |
| R-34 | DONE | Backup/restore | Report schema transitions and distinguish newer-schema rejection. |
| R-35 | DONE | Restore safety | Report restored active Gateway Keys or add an equivalent doctor warning. |
| R-36 | DONE | Key rotation UX | Make old-backup rejection identify both key fingerprints and the next step. |
| R-37 | DONE | Runbook | Add or index file-mode Master Key rotation. |
| R-38 | DONE | Go/tests | Move Admin-user mutations to durable Admin audit intents. |
| R-39 | DONE | Web copy/tests | State that capability-detection calls bypass project budget/usage accounting. |
| R-40 | DONE | Go/tests | Keep listener validation out of `init`. |
| R-41 | DONE | Tests | Add A4 T-4 through T-12, including the probe-stage call ceiling, a concurrent single-flight assertion, and honest naming for the one case that cannot fail. |
| R-42 | DONE | Release workflow | Pin release `GOTOOLCHAIN` to Go 1.26.5. |
| R-43 | DONE | SBOM/provenance | Add a released-binary SBOM and SHA-pinned GitHub build-provenance attestations, then verify attestations before publication. |
| R-44 | DONE | Visual governance | Keep the AA-compliant Light primary for 1.0.0 with an explicit post-release deferral; enforce typography tokens in CI. |

## Final repository verification

Re-run after the 2026-08-12 verification round's fixes: `go test ./...` clean;
`go test -race -count=1` clean on every package touched (`internal/app` 318s,
`gateway`, `redaction`, `tokenguard`, `adminauth`, `store/bolt`,
`modelcatalog`); `go vet ./...` and `gofmt -l` clean; frontend typecheck plus
32 files / 297 tests; production build with the embedded bundle regenerated and
the browser secret scan clean; `deploy/observability/validate.sh` green with 13
recording and 30 alert rules and all rule unit tests; `tools/m11/check-production-assets.sh`
green, now including a new gate that fails on any unpinned GitHub Action
reference (reverse-verified: unpinning one reference makes it exit 1).

Independently re-run on 2026-08-12 after the verification round, against the
committed tree: `go vet ./...` and `gofmt -l` clean, `go test ./...` with no
failures, frontend typecheck plus 32 files / 297 tests, `npm run build` leaving
`internal/webui/dist` byte-identical (no drift), `deploy/observability/validate.sh`
green, and `tools/m11/check-production-assets.sh` green.

G5 was also re-run end to end on the current code, because R-34/R-35/R-36 changed
`internal/backup/archive.go` and `internal/app/backup.go` after C1 produced the
original evidence. Backup → divergence → refused restore → restore → start, on an
isolated scratchpad instance with its own data directory and Master Key: the
Ledger returned exactly to the backup point (20 frames, sequence 20 at offset
21689, identical chain hash, byte-identical `ledger.wal`), Usage returned to 4/4
with no missing, duplicate, or extra records, audit stayed append-only, and
`doctor` reported healthy with a verified vault. The restore output carried
`schema_version_before/after` and named the one enabled Gateway Key the restore
brought back — the two behaviours R-34 and R-35 added. Evidence table in
[remediation-verification.md](remediation-verification.md) §8.3.

The earlier run recorded below still applies to the pre-verification tree:

- `go test -count=1 ./...` and `go test -race -count=1 ./...`;
- `go vet ./...` and `make fmt-check`;
- frontend typecheck, all 32 test files / 294 tests, and the production build;
- embedded Web UI package verification after regenerating `internal/webui/dist`;
- observability Prometheus/Alertmanager validation, alert rule tests, and
  runbook-link validation;
- release-workflow SHA pinning, reproducibility, binary-SBOM/provenance, M11
  evidence, dependency-license drift, YAML, and patch-whitespace checks.
- release Environment protection parsing and signed run-evidence manifest
  creation/tamper tests, enforced by CI.

The first sandboxed full Go run could not bind an existing `httptest` loopback
listener; the identical command passed in the permitted local test environment.
No billable real-provider smoke test was run.

## G7 item 3, closed by fixing rather than by accepting

The release owner chose to tighten. Step-up now keys on the criterion this
repository already stated for unblocking a Project — what destroys state *or
removes a protection that is in force* — instead of on the HTTP verb. Deleting a
redaction policy asked for re-authentication while editing it down to zero rules
did not, and the data plane cannot tell those apart; the same held for a Token
Guard policy edited to unlimited and for a credential whose material is
replaced.

Now required on `PUT /credentials/{id}`, `PUT /redaction-policies/{id}`,
`PUT /token-guard-policies/{id}`, and `POST
/providers/{id}/model-capability-detections`, the last as credential-spending
work that writes adoptable capability evidence. Every edit asks, rather than
only the ones a comparison judges to be weakening: that predicate is itself
security-critical, fails open on exactly the edit that matters, and cannot be
swept because the router cannot see which branch a request takes.

Enforced by `TestEverySecurityControlEditRequiresStepUp`, a route-family sweep
shaped like the existing DELETE sweep, so a verb added later is in scope the day
it is registered (reverse-verified: removing one call site fails it). Provider
and Deployment connection tests and invocation-target refresh/resolve stay out of
scope, with the reason recorded in `docs/verification/security-review-v1.md`.
The Console collects the material at all four entry points, so the server-side
tightening does not repeat R-24's shape of a guard the browser cannot satisfy.

## D4's three remaining defects, closed

All three were the same shape: something this process knows for certain, recorded
as unknown.

- **A SafeTransport refusal settled as ambiguous.** The four refusal points in
  the pinned dialer returned plain errors, and `provider.Unsent` recognises only
  `*net.DNSError` and a dial `*net.OpError`. Every adapter therefore marked the
  attempt ambiguous, which makes `retryable()` refuse to fail over and settles
  the attempt at its full reservation. A dial that fails against a real host was
  classified correctly; the case where this process guaranteed nothing was sent
  was not — and it is reachable in ordinary operation, because a Provider base
  URL resolving inside the network with private endpoints off refuses every
  request there. `safetransport.ErrRefusedBeforeSend` now marks those four
  points and `Unsent` recognises it. A resolver failure is deliberately left
  unmarked: it is the network's answer rather than this package's refusal, and
  it already carries `*net.DNSError`.
- **A bbolt walk that wrote under its own cursor.**
  `InterruptModelCapabilityDetections` mutated the bucket it was iterating,
  which bbolt leaves undefined; a skipped record leaves a detection in `running`
  with possibly-billable calls that never reach a terminal state, which is the
  one thing that function exists to prevent. It now collects and writes after
  the walk, like every other walk in the package. Its new test is a regression
  guard, not a reproduction — undefined behaviour does not reliably fail.
- **The recovery loop had no test.** It is the only thing that ends a stale
  snapshot, and a stale snapshot refuses the whole data plane, yet deleting the
  line in `Open` that starts it failed nothing. The retry interval is now a
  parameter so a test can drive the loop, and four tests cover replay, the
  audit-intent drain that is deliberately not gated on staleness, cancellation,
  and the startup wiring at the real interval.

Reverse-verified by overlay in every case where the defect state can fail:
removing the `Unsent` clause fails the settlement test, removing the goroutine in
`Open` fails the wiring test, and moving the audit drain inside the staleness
branch — which reads like a tidy-up — fails the drain test.

## Accepted with rationale, not closed

Recorded so the next reader does not have to re-derive the choice from the code.
None of these is a repository TODO.

- **Argon2 slot acquisition has no deadline.** The process-wide bound converts a
  login storm from unbounded heap growth into queueing latency with a goroutine
  held per waiter; arrival is bounded per source by `admin.login_rpm` and not
  globally, and the Admin server sets no write timeout. A deadline would answer a
  storm by failing legitimate operator logins. Rationale is on
  `derivePasswordKey` in `internal/adminauth/password.go` and in
  `docs/verification/performance-baseline.md`; revisit if the Admin surface is
  ever exposed to an untrusted network.
- **The Watchdog delivery budget sits exactly on its minimum.** 150s equals
  `2 x group_interval + 30s`, so raising `group_interval` reds `smoke.sh` on the
  same commit — intended, and now stated there. `validate.sh` still pins the
  Watchdog `repeat_interval`; that assertion covers re-notify cadence only and
  carries a note against re-deriving the budget from it, which is what produced
  the wrong budget before.
- **Two test-coverage gaps stay open, both non-blocking.**
  `activateAuthSnapshot` is exercised only on its success path, with no
  failure-injection case from the call site (the auth domain's stale path is
  covered by direct `markStale` injection instead), and `doctor.go`'s
  `admin_audit_backlog` check has no test. Each is a missing test around behaviour
  that is asserted elsewhere, not an unverified behaviour. The third gap in this
  list, `runActivationRecovery`, was closed rather than accepted — see above.

## External and explicit-decision gates

Repository changes cannot by themselves close R-01/R-04p. All repository-side
controls and archive tooling are now present, but verification still requires a
real protected GitHub Environment, human approval, and a retained workflow run.
Creating a Release is a separate later step and was not performed here. No
billable real-provider smoke test will be run without explicit authorization.

The release owner accepted the recommended outcomes for R-16, R-30, R-33,
R-43, and R-44 on 2026-08-11. The ledger records those chosen contracts rather
than leaving alternative delivery commitments implied.
