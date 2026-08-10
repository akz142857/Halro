# Codex Working Instructions

## Validation policy

- During iterative UI work, do not run the full test suite after every edit.
- For CSS-only, copy-only, spacing, or visual hierarchy changes, use a focused visual check; run tests only when behavior, markup semantics, or component logic changes.
- For localized frontend behavior changes, run the directly affected test file first, for example `cd web && npx vitest run src/pages/DeploymentsPage.test.tsx`.
- Run type checking when TypeScript types or component logic change. Do not repeat it when subsequent edits only change CSS or copy.
- Run the full frontend gate (`typecheck`, all tests, and production build) once at final handoff, before a commit, or when the user explicitly requests it.
- Do not repeat a previously successful full gate unless code changed afterward in a way that could affect the result.
- Never run billable real-provider smoke tests unless the user explicitly requests them.

The repository's final validation requirements in `CLAUDE.md` still apply before a change is considered complete; this policy changes when checks run, not which final checks are required.
