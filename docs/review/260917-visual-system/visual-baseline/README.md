# Visual Baseline Index

Baseline: `364db696cd199a178d9a77d82e0ba397b23f7d1f`

Runtime: isolated synthetic Admin instance, Chinese/Dark unless stated otherwise

Capture method: Codex in-app browser against the production embedded bundle

## Captured representative views

| View | Viewport | Theme/locale | Observed result |
| --- | --- | --- | --- |
| Dashboard first-run | 1440×900 | Dark/zh-CN | stable two-column shell, large page title and onboarding surface |
| Providers list | 1440×900 | Dark/zh-CN | no page overflow; resource row hierarchy visible |
| Providers list | 390×844 | Dark/zh-CN | primary nav and provider tabs become long horizontal strips |
| Usage summary | 1440×900 | Dark/zh-CN | filters, four metrics and empty chart state visible |
| Usage summary | 390×844 | Dark/zh-CN | page reflows; nested metric tabs/table retain local overflow |
| Run Governance first-run | 1440×900 | Dark/zh-CN | explicit 0/6 onboarding hierarchy and project context |
| Settings General | 1440×900 | Dark/zh-CN | left section nav with two preference panels |
| Settings General | 1440×900 | Light/zh-CN | semantic theme switches without page reload |
| Settings General | 390×844 | Light/en-US | four settings sections outside visible horizontal strip |

The images were inspected in the browser session; this index records reproducible URL/state/viewport metadata. No screenshot contains production data or real credentials. Binary screenshots are deliberately not committed because the review repository treats source-controlled evidence as text and deterministic measurements; final before/after screenshots are delivered inline with browser acceptance.

## Final candidate views

| View | Viewport | Theme/locale | Accepted result |
| --- | --- | --- | --- |
| Settings menu open | 390×900 | Light/en-US | all 11 destinations, account and sign-out visible through a vertical disclosure; no horizontal strip |
| Settings General | 320×900 | Dark/zh-CN | 28px title and current-section Select; root `305/305` client/scroll width |
| Providers | 1024×900 | Dark/zh-CN | tabs and resource rows adapt inside the 680px content region |
| Deployments | 320×900 | Dark/zh-CN | resource card track and overflow action remain inside the page |

## Measured layout facts

- Final Dark/zh-CN: 11 routes × 1440/1024/820/720/390/320 = **66 checks**, zero body/root overflow.
- Final Light/en-US: 11 routes × 1440/390 = **22 checks**, zero body/root overflow and correct theme/locale state.
- Compact navigation exposes all destinations via a 44px labelled disclosure; keyboard Enter opens it and Tab reaches Overview first.
- Settings exposes all seven sections through a labelled native Select at Compact/Small widths.
