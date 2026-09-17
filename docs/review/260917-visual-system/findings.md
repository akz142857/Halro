# Visual System Findings

These findings are against baseline `364db696`. Resolution status is updated as implementation proceeds; the evidence statement remains baseline-specific.

| ID | Severity | Class | Finding | Evidence | Resolution target | Status |
| --- | --- | --- | --- | --- | --- | --- |
| VS-01 | P1 | Pattern/A11Y | Narrow primary navigation is a 1452px horizontal strip inside a 292–347px viewport. Only the first destinations are visible and the account block consumes another row before content. | Browser at 390/320; `styles.css` 820px rule | Accessible menu/disclosure with all destinations and account controls reachable without horizontal discovery | **Closed** — disclosure exposes all 11 destinations and account actions; Enter/Tab verified |
| VS-02 | P1 | Component/A11Y | Narrow settings navigation repeats the horizontal-strip problem; English leaves four sections fully outside the viewport. | English Settings at 390px | Small-screen select/disclosure or wrapped grid with current section visible | **Closed** — one `SETTINGS_PANES` source drives desktop nav and compact Select |
| VS-03 | P2 | Token/Typography | Chart axes use hard-coded `11px ui-monospace`, below the repository's 12px CJK/type floor and outside typography tokens. | `TrendChart.tsx` | Tokenized 12px minimum chart type | **Closed** — computed chart font consumes 12px/mono tokens |
| VS-04 | P2 | Token/Typography | Page titles use `clamp(...42px)` and login titles reach 46px, producing a marketing-scale hierarchy in a dense operator console. | source + browser | 32px desktop / 28px narrow page-title role | **Closed** — shared page-title roles implemented; login remains an intentional entry-screen display role |
| VS-05 | P2 | Foundation | Tokens exist, but `styles.css` still owns 699 raw spacing/radius values and 16 media queries; the 13-line shared component stylesheet does not govern most components. | `design-system.test.ts`, line counts | semantic layout/component tokens and a decreasing ratchet | **Open** — type/control/shell-width roles added, but the plan's §9.3 spacing tokens are unimplemented (0 call sites), the ratchet moved 699 → 692 (1%), `styles.css` went 2901 → 2909 lines and `components.css` is still 13 lines. This is plan Phase 2 and Phase 5, not residual debt. |
| VS-06 | P2 | Pattern | Provider, Usage and similar tab bars retain local horizontal scrolling at narrow widths; visible browser scrollbars or clipped actions make the interaction look broken. | browser at 390px | shared adaptive tab treatment with hidden native bar, current-item visibility and overflow cue/menu | **Closed** — provider-family tabs grid/wrap; Governance tabs wrap; 320–1440 matrix has no page overflow |
| VS-07 | P2 | Component | Unsaved navigation uses native `window.confirm`, outside the themed Modal/focus contract. | `navigation.tsx` | shared navigation confirmation dialog without changing dirty-form safety | **Closed** — navigation emits a guarded action rendered by shared Modal `alertdialog` |
| VS-08 | P2 | Content/i18n | Switching English → Chinese leaves the already-created success notification in English. The action succeeds but feedback language no longer matches the active UI. | browser Settings locale switch | emit confirmation after locale application using the resulting locale | **Closed** — notification resolves through `i18n.t` after preference application; regression test added |
| VS-09 | P2 | Foundation | Breakpoints mix 580/640/720/760/820/1120/1520px and rem equivalents. Similar shell/tab/layout problems are solved by unrelated local rules. | CSS inventory | four documented layout modes plus component-owned content breakpoints | **Partially open** — the four shell modes are frozen and documented, but no intermediate value was actually merged: 580/640/720/760/820/1120/1520 plus 36.25rem/64rem/70rem all remain, the same set as the baseline. The documentation landed; the convergence did not. |
| VS-10 | P2 | Component | Several compact interactive controls remain 30–34px on narrow/touch layouts, below the target 44px touch area. | CSS inventory | 44px narrow interaction target; keep compact visual treatment through padding/layout | **Closed** — shared compact breakpoint applies 44px control target |
| VS-11 | P3 | Visual hierarchy | Large page headers and repeated bordered surfaces use vertical space before decision content, especially on Settings and first-run pages. | representative screenshots | reduced page-title/header scale and fewer nested full borders | **Closed** — page title and header insets reduced and tokenized |
| VS-13 | P2 | Token/Data | `cost_completeness` distinguishes complete / partial / unknown, but `.governance-completeness` paints anything not complete with the warning role. §9.5 requires `status-unknown` not to share a visual semantic with warning: "the number is incomplete" and "something needs attention" are different statements to an operator reading a cost figure. | `styles.css` `.governance-completeness`; `RunGovernancePage` fixtures carry `cost_completeness: "partial"` | decide whether incomplete cost is an unknown state or a warning, then implement `--color-status-unknown-*` or record why warning is correct | **Closed** — `partial` keeps warning ("units still unevaluated" is an attention state); `unknown` gets `--color-status-unknown-*`, a dashed boundary over no fill and a hollow badge dot. The base rule now carries no status treatment, so a fourth state cannot inherit one. Gated by `gives each completeness state its own status treatment` |
| VS-14 | P2 | Component/A11Y | Five disabled treatments are `opacity` alone. Measured, four of them are legible (4.02–6.93:1) and one is not: `.developer-response-mode button:disabled { opacity: .4 }` renders at **2.66:1 on Dark and 1.90:1 on Light**. The defect is not the ratio at any one call site — it is that `opacity` makes contrast a property of the backdrop, so four rules written the same way landed anywhere between 1.90 and 6.93 and nothing in the declaration said which. | `styles.css`, five `:disabled { opacity }` rules; ratios computed from the primitive palette | `--color-action-disabled` plus a disabled treatment that keeps text and boundary legible; verify contrast in both themes | **Closed** — named foreground and border replace opacity on the four text-bearing controls; held at AA by the contrast assertion (Dark 5.93–6.30, Light 5.16–5.83) and by `does not express a disabled control through opacity`. The native checkbox keeps `opacity` as a reviewed exception: it carries no text and the user agent draws its own disabled state |
| VS-15 | P3 | A11Y/Content | `a { color: inherit; text-decoration: none }` is global, so the Provider documentation link renders identically to body text. A link that is only discoverable by hovering fails §8.6's "hover 不能是发现信息或完成任务的唯一方式". | `styles.css:28`; `ProvidersPage.tsx:257` | `--color-text-link` and a link affordance that survives forced-colors | **Closed** — `.text-link` carries colour *and* underline, so the affordance survives forced-colors and colour vision deficiency. The global `a` rule is untouched because navigation, settings panes and button-shaped anchors depend on inheriting. `gives every anchor a class that says what it is` stops the next bare one |
| VS-12 | P3 | Governance | Foundation, component and page-pattern documentation is split between code comments and old PRDs, so new contributors cannot discover one current contract. | repository inventory | maintained `docs/design-system/` specification and migration map | **Closed** — formal five-document system plus contributor/PR gates added |

## Correction

VS-14 was first recorded as "nine disabled treatments". That count came from every
`opacity` declaration in the stylesheet, not from the disabled ones; there are five, and
measuring them showed four already clearing 4:1. The finding's substance survives the
correction — one control at 1.90:1, and an idiom whose contrast nobody can read off the
rule — but its size did not, and the entry above is rewritten to the measured facts.

## Positive baseline evidence

- No sampled primary route produced page-level horizontal overflow at 1440, 390 or 320 CSS px.
- Light/Dark semantic token parity and current contrast assertions pass.
- Skip link, focus-visible, reduced-motion and forced-colors foundations exist.
- Resource rows/cards, Modal, ConfirmButton, Field, Combobox, Tabs, Empty/Error/Loading already provide a strong migration base.
- Parent/child Run metrics and narrow-screen Run actions already use visible hierarchy rather than ARIA-only grouping.

## Resolution order

No P0/P1 remains. Thirteen findings are closed and verified by tests plus browser evidence;
VS-05 and VS-09 remain open as the outstanding Phase 2 and Phase 5 work.

VS-05 and VS-09 were previously recorded as accepted debt. A 2026-09-17 re-check against the
code found that framing wrong: both are the substance of the parent plan's Phase 2 and Phase 5,
not residue left over from executing them. They are reopened here so the outstanding work stays
visible.

What is still required, by parent-plan section:

- §9.3 — **adjudicated 2026-09-17.** Two roles implemented and consumed
  (`--layout-page-inline`, `--layout-page-block-end`); `--component-row-padding` redirected to
  the existing `--resource-row-*` tokens; the remaining five struck as aliases of `--space-*`.
  The speculative eight-token table was never the measure of this finding — the ratchet is.
- §9.6 — actually merge 640 / 720 / 760 / 1520 / 64rem / 70rem into the four frozen shell modes
  or into explicitly component-owned content breakpoints.
- §14.3 — bring the exact raw spacing/radius ratchet down materially from 691; the moves so far
  (699 → 691) do not satisfy "裸 spacing/radius 基线显著下降".
- Phase 5 — migrate repeated page styling out of `styles.css` (2909 lines, 121 class prefixes)
  into the shared layer (`components.css` is 13 lines).

**Closed 2026-09-17:** the gate in `design-system.test.ts` used to ratify the delivered token set
rather than the specified one — its required list equalled what shipped, so it could not report a
specification nobody implemented. It is now written from §9.5 (`declares the specified semantic
color roles`), and the three roles the specification asks for but nobody built are tracked as
VS-13, VS-14 and VS-15 rather than declared as unused tokens. A second gate,
`declares no semantic role the product never reads`, asserts the unconsumed-role set exactly, so
a token can no longer be added without a call site. Reverse-verified: injecting an unconsumed
`--color-text-link` fails it.
