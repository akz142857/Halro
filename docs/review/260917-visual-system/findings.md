# Visual System Findings

These findings are against baseline `364db696`. Resolution status is updated as implementation proceeds; the evidence statement remains baseline-specific.

| ID | Severity | Class | Finding | Evidence | Resolution target | Status |
| --- | --- | --- | --- | --- | --- | --- |
| VS-01 | P1 | Pattern/A11Y | Narrow primary navigation is a 1452px horizontal strip inside a 292–347px viewport. Only the first destinations are visible and the account block consumes another row before content. | Browser at 390/320; `styles.css` 820px rule | Accessible menu/disclosure with all destinations and account controls reachable without horizontal discovery | **Closed** — disclosure exposes all 11 destinations and account actions; Enter/Tab verified |
| VS-02 | P1 | Component/A11Y | Narrow settings navigation repeats the horizontal-strip problem; English leaves four sections fully outside the viewport. | English Settings at 390px | Small-screen select/disclosure or wrapped grid with current section visible | **Closed** — one `SETTINGS_PANES` source drives desktop nav and compact Select |
| VS-03 | P2 | Token/Typography | Chart axes use hard-coded `11px ui-monospace`, below the repository's 12px CJK/type floor and outside typography tokens. | `TrendChart.tsx` | Tokenized 12px minimum chart type | **Closed** — computed chart font consumes 12px/mono tokens |
| VS-04 | P2 | Token/Typography | Page titles use `clamp(...42px)` and login titles reach 46px, producing a marketing-scale hierarchy in a dense operator console. | source + browser | 32px desktop / 28px narrow page-title role | **Closed** — shared page-title roles implemented; login remains an intentional entry-screen display role |
| VS-05 | P2 | Foundation | Tokens exist, but `styles.css` still owns 699 raw spacing/radius values and 16 media queries; the 13-line shared component stylesheet does not govern most components. | `design-system.test.ts`, line counts | semantic layout/component tokens and a decreasing ratchet | **Accepted debt** — layout/type/control roles added; exact ratchet reduced 699 → 693 and remains enforced |
| VS-06 | P2 | Pattern | Provider, Usage and similar tab bars retain local horizontal scrolling at narrow widths; visible browser scrollbars or clipped actions make the interaction look broken. | browser at 390px | shared adaptive tab treatment with hidden native bar, current-item visibility and overflow cue/menu | **Closed** — provider-family tabs grid/wrap; Governance tabs wrap; 320–1440 matrix has no page overflow |
| VS-07 | P2 | Component | Unsaved navigation uses native `window.confirm`, outside the themed Modal/focus contract. | `navigation.tsx` | shared navigation confirmation dialog without changing dirty-form safety | **Closed** — navigation emits a guarded action rendered by shared Modal `alertdialog` |
| VS-08 | P2 | Content/i18n | Switching English → Chinese leaves the already-created success notification in English. The action succeeds but feedback language no longer matches the active UI. | browser Settings locale switch | emit confirmation after locale application using the resulting locale | **Closed** — notification resolves through `i18n.t` after preference application; regression test added |
| VS-09 | P2 | Foundation | Breakpoints mix 580/640/720/760/820/1120/1520px and rem equivalents. Similar shell/tab/layout problems are solved by unrelated local rules. | CSS inventory | four documented layout modes plus component-owned content breakpoints | **Accepted debt** — four shell modes frozen; remaining intermediate points documented as component content breakpoints |
| VS-10 | P2 | Component | Several compact interactive controls remain 30–34px on narrow/touch layouts, below the target 44px touch area. | CSS inventory | 44px narrow interaction target; keep compact visual treatment through padding/layout | **Closed** — shared compact breakpoint applies 44px control target |
| VS-11 | P3 | Visual hierarchy | Large page headers and repeated bordered surfaces use vertical space before decision content, especially on Settings and first-run pages. | representative screenshots | reduced page-title/header scale and fewer nested full borders | **Closed** — page title and header insets reduced and tokenized |
| VS-12 | P3 | Governance | Foundation, component and page-pattern documentation is split between code comments and old PRDs, so new contributors cannot discover one current contract. | repository inventory | maintained `docs/design-system/` specification and migration map | **Closed** — formal five-document system plus contributor/PR gates added |

## Positive baseline evidence

- No sampled primary route produced page-level horizontal overflow at 1440, 390 or 320 CSS px.
- Light/Dark semantic token parity and current contrast assertions pass.
- Skip link, focus-visible, reduced-motion and forced-colors foundations exist.
- Resource rows/cards, Modal, ConfirmButton, Field, Combobox, Tabs, Empty/Error/Loading already provide a strong migration base.
- Parent/child Run metrics and narrow-screen Run actions already use visible hierarchy rather than ARIA-only grouping.

## Resolution order

No P0/P1 remains. VS-05 and VS-09 are deliberately accepted migration debt, constrained by exact tests and the documented component-breakpoint rule; neither blocks v0.1 acceptance.
