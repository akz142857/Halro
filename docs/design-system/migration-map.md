# Visual System v0.1 migration map

| Legacy / local rule | v0.1 target | Status |
| --- | --- | --- |
| `h1 clamp(32px…42px)` | `--type-page-title-size` / compact role | Migrated |
| `TrendChart` `11px ui-monospace` | computed `--font-size-xs` + `--font-mono` | Migrated |
| narrow `.sidebar nav` 1452px horizontal strip | `.mobile-navigation-toggle` disclosure | Migrated |
| narrow `.settings-nav` horizontal strip | `.settings-section-select` | Migrated |
| `window.confirm` dirty-form navigation | shared `Modal` alertdialog | Migrated |
| provider/usage/policy tab horizontal strip | adaptive grid at Small | Migrated |
| compact 30–34px interactive controls | `--control-touch-block-size` at Compact/Small | Migrated |
| page header 54/34px hand values | spacing tokens | Migrated |
| page-local resource typography | `resource-list.css` / `resource-card.css` | Existing shared contract |
| page-local colors / theme branches | semantic tokens in both themes | Enforced |
| raw spacing/radius baseline 699 | exact baseline 693, decrease-only review | In progress debt |
| mixed content breakpoints | four shell modes + documented component breakpoints | Governed; incremental cleanup |

## Deprecation rules

- Do not add new horizontal navigation scrollers below 820px.
- Do not add page-level color primitives, `window.confirm`, sub-12px text, or an unreviewed breakpoint.
- Existing aliases (`--bg`, `--surface`, `--text` 等) remain compatible while semantic color migration continues; do not add new aliases.
- A migration is complete only after source, focused test, browser evidence and embedded bundle are synchronized.

## Remaining intentional debt

The 693-value raw spacing/radius ledger is not an endorsement. Each affected component change should convert nearby values and lower the exact baseline. Component-specific 760/720/640 breakpoints remain only where content, not device naming, justifies them.
