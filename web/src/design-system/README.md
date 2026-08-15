# Halro Admin Console Design System

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
                                 Also applied to the bare :root so first paint,
                                 login and unauthenticated pages are Dark.
themes/light.css                 Light values under
                                 :root[data-appearance="light"].
(legacy alias layer)  Layer 3 — --bg/--surface/--text/… mapped onto semantic
                                 tokens so existing styles.css themes for free.
components.css        Shared design-system component contracts and previews.
styles.css            Business/page CSS. Consumes semantic tokens (or the
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

## Resource list rows

`resource-list.css` owns the row that Providers, Credentials and Deployments all
render — and that Projects, Routes and any later list should render too. Pages
had each grown their own copy of this typography and the copies drifted: the
same kind of value was 15px bold on one list and 13px semibold on the next, a
fact label was sans on one and mono on another.

The contract is the hierarchy, not the page:

| Part | Class | Size | Weight | Colour |
| --- | --- | --- | --- | --- |
| identity name | `.resource-identity > strong` | `--font-size-md` | semibold | `--color-text-primary` |
| fact value | `.resource-fact > strong` / `code` / `.resource-link` | `--font-size-sm` | semibold | `--color-text-primary` |
| identity subtitle | `.resource-identity > small` | `--font-size-xs` | regular | `--color-text-secondary` |
| fact label | `.resource-fact > small` | `--font-size-xs` | regular | `--color-text-tertiary` |

The name is the largest thing in the row because it is what the operator is
scanning for; a fact answers one step down; the label naming the fact is quieter
than the fact itself. Machine values (`<code>`: endpoints, ids, model names)
keep the mono face at the value size. Labels never do — a mono CJK label falls
back to a CJK face anyway and only loses the metrics.

Spacing comes from `--resource-row-padding-block` / `-inline`,
`--resource-row-column-gap`, `--resource-row-min-height` and
`--resource-cell-gap`. A page owns its **columns** — how many facts, how wide,
which collapse at which breakpoint — and nothing else about the row. When a
breakpoint hides a fact, hide it by a placement class (`.credential-endpoint`),
not by `:nth-of-type`, which counts elements rather than facts.

## Where an outcome is reported

`NotificationProvider` (`web/src/notifications.tsx`) owns the top-right column.
It is for outcomes that have **no anchor left on the page**, and only those.

Goes to the notification column:

- a save whose modal has already closed; a delete whose row is gone;
- a toggle or bare button with nowhere to render a rejection;
- results of work the operator is no longer looking at (background refresh);
- the transient acknowledgement of a settings save — the panel stays on screen
  and already shows the saved value, so a second inline banner only repeats it.

Stays where it happened, and must not be replaced by a notification:

- field-level validation — the reason and the input are one thing;
- fail-closed refusals (auth, budget, read-only) — an auto-dismissing overlay
  must never be the only signal;
- persisted row state, such as a connection test result that survives a reload;
- destructive confirmations, which state their consequence in the dialog.

Two rules on top of that: a message is reported **once** — never both inline and
in the column — and notifications carry no secret, prompt or response body, the
same as logs. Errors never auto-dismiss; success and info clear themselves.

## Enforcement

`design-system.test.ts` requires exact Dark/Light semantic-token parity, checks
the documented WCAG contrast thresholds, and recursively rejects color literals
or primitive-token access outside the design-system directory. There is no
business-CSS color-literal baseline or growth allowance.
