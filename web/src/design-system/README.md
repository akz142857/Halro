# Heimdall Admin Console Design System

A layered, themeable token system that drives both the **Dark** (default) and
**Light** appearances. Appearance is a per-admin preference persisted server
side; the theme is applied by writing `data-appearance="dark|light"` on the
root element (see `web/src/theme.ts`).

## Layers (one-way dependency)

```
tokens.css            Layer 1 — Primitives: raw color scales, type, spacing,
                                 radius, shadow, motion, z-index. The only place
                                 literal color values may live.
themes/dark.css       Layer 2 — Semantic tokens (color.*, shadow.*, …) for Dark.
themes/light.css                 Also applied to the bare :root so first paint,
                                 login and unauthenticated pages are Dark.
                                 Light values under :root[data-appearance="light"].
(legacy alias layer)  Layer 3 — --bg/--surface/--text/… mapped onto semantic
                                 tokens so existing styles.css themes for free.
styles.css            Business/component CSS. Consumes semantic tokens (or the
                                 legacy aliases), never primitives.
```

Business pages/components may use **semantic tokens** (`--color-*`, `--shadow-*`,
`--space-*`, …) or the legacy aliases. They must **never**:

- consume a primitive (`--p-*`) directly;
- read `data-appearance` to branch business logic;
- hardcode a new color (hex/rgb) — add or reuse a token instead.

## Adding or changing a token

1. Add the raw value to `tokens.css` if a new primitive is needed.
2. Add the **same** semantic token name to **both** `themes/dark.css` and
   `themes/light.css`. The two files must always declare an identical key set —
   this is asserted by `web/src/design-system.test.ts`.
3. Reference the semantic token from component CSS.

## Verifying Light / Dark

- Toggle **Settings → General → Appearance**, or set `data-appearance` on
  `<html>` in devtools.
- Check every component's default / hover / active / focus-visible / disabled /
  loading / selected / error state in both themes.
- Contrast: body text ≥ 4.5:1, large text & key non-text boundaries ≥ 3:1.
- Verify `:focus-visible` is visible on every surface in both themes.

## Do / Don't

```css
/* DO */
.card { background: var(--color-surface-default); border: 1px solid var(--color-border-default); }
.button.primary { background: var(--color-action-primary); color: var(--color-text-inverse); }

/* DON'T */
.card { background: #0b1916; }                 /* hardcoded color */
:root[data-appearance="light"] .some-page { }  /* per-page theme patch */
```

## Known follow-ups (phased migration)

The legacy `styles.css` still contains hardcoded hex/rgba from before the token
system. These are being migrated to semantic tokens in phases; a handful of
lime-as-text accents need darker light-mode values. New code must not add to the
backlog — the hardcoded-color guard (`design-system.test.ts`) fails the build if
the count grows.
