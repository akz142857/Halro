# Page and State Inventory

## Primary routes

| Route | Primary task | Source review | 1440px | 390px | 320px | Key pressure |
| --- | --- | --- | --- | --- | --- | --- |
| `/admin` | health and first-run decision | done | done | done | measured | metrics, onboarding |
| `/admin/providers` | credentials/providers/proxies | done | done | done | measured | tabs, resource row, actions |
| `/admin/deployments` | model deployment lifecycle | done | measured | measured | measured | card density, capability evidence |
| `/admin/routes` | alias routing | done | measured | measured | measured | wide table, grouped rows |
| `/admin/policies` | security policy configuration | done | measured | measured | measured | complex forms, destructive changes |
| `/admin/projects` | project and Gateway key governance | done | measured | measured | measured | nested facts, one-time secret |
| `/admin/developer` | compose and execute a request | done | measured | measured | measured | editor, code, sticky actions |
| `/admin/usage` | cost/usage/failure analysis | done | done | done | measured | tabs, filters, tables, charts |
| `/admin/run-governance` | Work Unit/Run/Outcome evidence | done | done | measured | measured | hierarchy, master-detail |
| `/admin/operations` | alerts, webhook and audit | done | measured | measured | measured | event density, pagination |
| `/admin/settings` | personal and instance settings | done | done | done | measured | section navigation, forms |

## Shell and exceptional states

| Surface/state | Coverage | Notes |
| --- | --- | --- |
| Login | source + browser | system language control and keyboard-labelled fields present |
| Setup | source + component tests | isolated runtime used offline Admin bootstrap; setup submission was not repeated in browser |
| Boot/loading | source + browser | shell remains stable while lazy page/data loads |
| 404 | source | one action back to Overview |
| Read-only administrator | source + existing tests | final browser acceptance remains required after migration |
| Empty/partial/error | source + synthetic empty/failure data | Usage empty interval and Provider unavailable observed |
| Modal/Drawer/Menu | source + existing focused tests | final focus/scroll acceptance remains required |

## Final candidate coverage

- Every primary route passed the Dark/zh-CN matrix at 1440, 1024, 820, 720, 390 and 320 CSS px with no body/root horizontal overflow.
- Every primary route passed Light/en-US at 1440 and 390 CSS px.
- Administrator browser state covered normal, empty and synthetic Provider failure data. Read-only rendering and server refusal semantics are covered by existing focused tests; a second live browser account was not persisted solely for this review.
- Keyboard acceptance covered skip-link/focus infrastructure through automated tests and the compact menu through real Enter/Tab operation.
- 720/390/320 CSS reflow provides 200%/high-zoom-equivalent layout pressure. The in-app browser did not expose literal page-zoom or forced-colors emulation, so those remain code/test evidence rather than native assistive-technology certification.

## Viewport measurements

All 11 primary routes had `body.scrollWidth == body.clientWidth` at 1440, 390 and 320 CSS px. This proves no page-level horizontal overflow for the sampled states; it does not prove every nested data state is readable.

At 390px the primary navigation measured `347px client / 1452px scroll`; at 320px it measured `292px client / 1452px scroll`. The current horizontal strip therefore exposes only a fraction of the 11 destinations at once.

Known contained overflow:

- Routes keeps its wide table inside a scroll container.
- Usage keeps its dimension table and metric tabs inside local overflow regions.
- Provider and Settings tabs currently use horizontal scrolling but provide insufficient navigation affordance; tracked in findings.

## Required final combinations

- Light and Dark on representative routes.
- Chinese and English on representative routes and all shared navigation.
- Administrator and read-only states where controls differ.
- 1440, 1024, 820, 720, 390 and 320 CSS px.
- Keyboard-only task paths, 200% and representative 400% reflow.
- Default, hover, focus-visible, selected, disabled, loading, empty, error and destructive confirmation states.
