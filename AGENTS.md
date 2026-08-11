# Agent Working Instructions

Applies to every coding agent working in this repository, not one vendor's.
`CLAUDE.md` points here for the verification policy below.

## Validation policy

The rule behind all of it: run what the change can affect. Re-running a suite that
cannot see the change is not thoroughness, it is dead waiting.

### Frontend

- During iterative UI work, do not run the full test suite after every edit.
- For CSS-only, copy-only, spacing, or visual hierarchy changes, use a focused visual check; run tests only when behavior, markup semantics, or component logic changes.
- For localized frontend behavior changes, run the directly affected test file first, for example `cd web && npx vitest run src/pages/DeploymentsPage.test.tsx`.
- Design-system and stylesheet edits are answered by `npx vitest run src/design-system.test.ts` alone; it is the only test that reads `styles.css` and the theme layer.
- Run type checking when TypeScript types or component logic change. Do not repeat it when subsequent edits only change CSS or copy.
- Run the full frontend gate (`typecheck`, all tests, and production build) once before pushing, at final handoff, or when the user explicitly requests it — not once per commit.

### Go

- While iterating, run the narrowest thing that covers the change: `go test ./internal/<pkg>/ -run TestName`, then the package, and only then `./...`. `internal/app` alone is ~80s and the whole suite is minutes, so the difference is not marginal.
- Use `-count=1` whenever a pass is being read as evidence; a cached `ok` proves nothing.
- `-race` is for changes that touch concurrency, goroutines, shared state or lifecycle, and then only for the affected package — `go test -race ./internal/app/` is ~215s. Threading a parameter or editing pure logic does not earn it.
- A full `go test ./...` is warranted once before pushing, and whenever the question itself needs it — deciding whether a failure elsewhere was caused by this change is a legitimate reason to run it.

### Both

- Do not repeat a previously successful full gate unless code changed afterward in a way that could affect the result.
- Never run billable real-provider smoke tests unless the user explicitly requests them.
- Do not read an exit code through a pipe: `cmd | grep ...` leaves `$?` and `PIPESTATUS[0]` describing `grep`, so a suite gets re-run for no reason. Redirect to a file and grep that.

## The full gate runs once before pushing, not once per commit

A series of commits gets one full gate, immediately before the push that publishes
it. What has to be protected is the branch other people and CI read, and that is
what a push changes; a local commit changes nothing anyone else can see.

For each individual commit, run what that commit can affect — the packages and test
files it touches, plus the conditional checks its content earns (`-race`, the
embedded-bundle drift check after `web/` edits).

The cost this avoids is real: a four-commit series used to mean four full Go
suites and, where the frontend was touched, four full frontend gates. The price
paid for it is that an intermediate commit in a series has not been independently
verified, so a `git bisect` landing on one cannot assume a green gate. That is an
accepted trade in this repository.

Two things this does not license. A push still gets a genuine full gate, including
`git diff --exit-code -- internal/webui/dist` when `web/` changed. And a docs-only
commit added on top of already-gated code does not require re-running anything —
markdown cannot change a test result, and re-running it is exactly the dead
waiting this policy exists to prevent.
