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
- Run the full frontend gate (`typecheck`, all tests, and production build) once at final handoff, before a commit, or when the user explicitly requests it.

### Go

- While iterating, run the narrowest thing that covers the change: `go test ./internal/<pkg>/ -run TestName`, then the package, and only then `./...`. `internal/app` alone is ~80s and the whole suite is minutes, so the difference is not marginal.
- Use `-count=1` whenever a pass is being read as evidence; a cached `ok` proves nothing.
- `-race` is for changes that touch concurrency, goroutines, shared state or lifecycle, and then only for the affected package — `go test -race ./internal/app/` is ~215s. Threading a parameter or editing pure logic does not earn it.
- A full `go test ./...` is warranted before a commit, and whenever the question itself needs it — deciding whether a failure elsewhere was caused by this change is a legitimate reason to run it.

### Both

- Do not repeat a previously successful full gate unless code changed afterward in a way that could affect the result.
- Never run billable real-provider smoke tests unless the user explicitly requests them.
- Do not read an exit code through a pipe: `cmd | grep ...` leaves `$?` and `PIPESTATUS[0]` describing `grep`, so a suite gets re-run for no reason. Redirect to a file and grep that.

`CLAUDE.md`'s full gate still runs before a change is committed. This policy decides
when checks run, not which ones are ultimately required.
