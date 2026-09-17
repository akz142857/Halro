# Halro Admin Visual System Review Plan

- Date: 2026-09-17
- Baseline: `364db696cd199a178d9a77d82e0ba397b23f7d1f`
- Scope: authenticated and unauthenticated Admin UI, 11 primary routes, shared components, Light/Dark, Chinese/English, 320–1440 CSS px
- Parent plan: [`docs/prd/admin-visual-system-review-plan.zh-CN.md`](../../prd/admin-visual-system-review-plan.zh-CN.md)
- Runtime: isolated synthetic instance on `127.0.0.1:28080/28081`; no live data, real credentials, or billable Provider calls

## Evidence levels

| Level | Meaning |
| --- | --- |
| Source | Current TSX/CSS/test behavior traced to exact files |
| Component | Focused Vitest/jsdom contract evidence |
| Browser | Production embedded bundle exercised against an isolated runtime |
| Acceptance | Full candidate matrix and final frontend gate after implementation |

No source or component result is promoted to browser acceptance. The isolated browser run is not production acceptance or an assistive-technology certification.

## Review sequence

1. Freeze commit, runtime, roles, themes, locales, routes and viewports.
2. Inventory visual assets, tokens, components, raw dimensions and breakpoints.
3. Capture representative Dashboard, Providers, Usage, Run Governance and Settings views.
4. Measure every primary route at 1440×900, 390×844 and 320×800.
5. Review information hierarchy, typography, color, spacing, component states, adaptivity and accessibility.
6. Classify each finding as IA, Pattern, Component, Token, Local, Content, A11Y or Data.
7. Fix system-level causes before page-local symptoms.
8. Re-run the same browser matrix and final repository gates.

## Baseline commands

```bash
cd web
npx vitest run src/design-system.test.ts src/accessibility.test.tsx
```

Baseline result: 2 files, 33 tests passed. The runtime binary was built from the baseline SHA and used a disposable `/private/tmp/halro-visual-review-260917.*` data directory.

## Constraints

- Preserve Halro's security, accounting, evidence and read-only semantics.
- Do not introduce a parallel UI framework or webfont dependency.
- Do not edit generated `internal/webui/dist` by hand.
- Source and rebuilt embedded assets must ship together.
- During iteration run the narrowest affected tests; run the complete frontend gate once for the final candidate.
