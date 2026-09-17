# Visual System v0.1 Acceptance Report

> Correction (2026-09-17 re-check): this report's evidence stands — the gates, the browser
> matrix and the ten closed findings were re-verified. Its scope claim did not. What was
> accepted is Phase 0–1 of the parent plan plus one round of P1/P2 fixes, not Phase 2–5.
> The "explicitly accepted" raw-spacing and breakpoint debt below is the plan's Foundation and
> migration work itself; VS-05 and VS-09 are reopened in `findings.md`. Read the "Delivered
> system" list as what this round shipped, not as a completed visual system.

Date: 2026-09-17

Baseline: `364db696cd199a178d9a77d82e0ba397b23f7d1f`

Candidate: baseline plus the uncommitted scoped changes listed by `git status`

Runtime: isolated loopback-only synthetic instance; no production data, real Provider credential or billable smoke call

## Outcome

Visual System v0.1 is accepted for repository handoff. P0/P1 count is zero; the baseline score moved from 67.75 to 91.75. Remaining raw-spacing and component-breakpoint debt is explicitly accepted and kept behind exact automated gates.

## Delivered system

- Formal `docs/design-system/` principles, foundations, component contracts, patterns and migration map.
- Semantic page-title, layout, control and touch-target roles.
- Compact/Small primary-navigation disclosure and Settings section Select.
- Adaptive provider-family tabs/resource rows and 320px-safe resource-card track.
- Tokenized 12px chart axes and 32/28px page-title hierarchy.
- Shared themed navigation confirmation dialog replacing `window.confirm`.
- Locale-correct save feedback after language application.
- Contributor and PR checks, exact spacing/radius ratchet, regression tests and rebuilt embedded bundle.

## Browser evidence

| Matrix | Result |
| --- | --- |
| Dark + zh-CN, 11 routes × 1440/1024/820/720/390/320 | 66/66, no body/root horizontal overflow |
| Light + en-US, 11 routes × 1440/390 | 22/22, correct locale/theme and no body/root overflow |
| Compact primary navigation | all 11 routes and account controls reachable; Enter opens and Tab enters Overview |
| Settings navigation | all seven sections present in a labelled Select; current pane selected |
| Language feedback | English success message rendered after switching to English |
| Browser console | zero warning/error entries after final route pass |

Real-browser iteration additionally found and closed 1024px Providers, 1024px Projects, exact-320px root and 320px Deployment card overflow that source review alone did not reveal.

## Automated gate

Final candidate, after the last source change:

```text
npm run typecheck  PASS
npm test           PASS — 44 files, 614 tests
npm run build      PASS — bundle and artifact/secret checks
git diff --check   PASS
```

Two consecutive production builds produced the same entry asset names (`index-CcU7pD56.js`, `index-CJl7Bkbg.css`), and the generated `internal/webui/dist` is present with the source changes.

## Evidence boundaries

- Read-only semantics are covered by component/API tests and source review; the browser pass used one disposable administrator account and did not create a persistent second account solely for screenshots.
- The in-app browser ignored page-zoom keyboard commands and exposes no forced-colors/reduced-motion emulation. Equivalent 720/390/320 CSS reflow was verified; reduced-motion and forced-colors remain stylesheet/test evidence. This is not an assistive-technology certification.
- The isolated localhost acceptance is not production or deployment validation.

These boundaries are P2 evidence limitations, not open P0/P1 product defects.
