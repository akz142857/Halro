import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8");

/** Custom-property names declared (left of `:`) in a CSS file. */
function declaredTokens(css: string): Set<string> {
  const names = new Set<string>();
  for (const match of css.matchAll(/(--[a-z0-9-]+)\s*:/gi)) names.add(match[1]);
  return names;
}

function tokenValues(css: string): Map<string, string> {
  const values = new Map<string, string>();
  for (const match of css.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/gi)) values.set(match[1], match[2].trim());
  return values;
}

function resolveColor(name: string, values: Map<string, string>): string {
  let value = values.get(name) || "";
  const seen = new Set<string>();
  while (value.startsWith("var(")) {
    const next = value.match(/^var\((--[a-z0-9-]+)\)$/i)?.[1];
    if (!next || seen.has(next)) break;
    seen.add(next);
    value = values.get(next) || "";
  }
  return value;
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((start) => Number.parseInt(hex.slice(start, start + 2), 16) / 255)
    .map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
  return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
}

function contrast(first: string, second: string): number {
  const [lighter, darker] = [luminance(first), luminance(second)].sort((a, b) => b - a);
  return (lighter + 0.05) / (darker + 0.05);
}

describe("design system themes", () => {
  const dark = read("./design-system/themes/dark.css");
  const light = read("./design-system/themes/light.css");

  it("declares an identical token key set in Light and Dark", () => {
    const darkTokens = [...declaredTokens(dark)].sort();
    const lightTokens = [...declaredTokens(light)].sort();
    expect(lightTokens).toEqual(darkTokens);
  });

  it("sets color-scheme per theme so native controls match", () => {
    expect(dark).toMatch(/color-scheme:\s*dark/);
    expect(light).toMatch(/color-scheme:\s*light/);
  });

  it("applies Dark to the bare :root so unauthenticated pages default to Dark", () => {
    expect(dark).toMatch(/^:root,\s*$/m);
    expect(light).not.toMatch(/^:root[,{\s]/m); // light only under [data-appearance="light"]
  });

  it("declares the PRD minimum semantic color tokens", () => {
    const required = [
      "--color-canvas",
      "--color-surface-default",
      "--color-surface-subtle",
      "--color-surface-raised",
      "--color-surface-overlay",
      "--color-text-primary",
      "--color-text-secondary",
      "--color-text-tertiary",
      "--color-text-inverse",
      "--color-border-default",
      "--color-border-strong",
      "--color-action-primary",
      "--color-accent",
      "--color-action-primary-hover",
      "--color-action-secondary",
      "--color-focus-ring",
      "--color-status-success-text",
      "--color-status-warning-text",
      "--color-status-danger-text",
      "--color-status-info-text",
      "--color-chart-series-1",
      "--color-chart-series-2",
      "--color-chart-grid",
      "--color-chart-axis",
      "--color-chart-tooltip",
    ];
    const darkTokens = declaredTokens(dark);
    for (const token of required) expect(darkTokens.has(token), token).toBe(true);
  });

  it.each([
    ["Dark", dark],
    ["Light", light],
  ])("keeps %s text and key boundaries at WCAG AA contrast", (_name, theme) => {
    const primitives = tokenValues(read("./design-system/tokens.css"));
    const values = new Map([...primitives, ...tokenValues(theme)]);
    const surfaces = ["--color-canvas", "--color-surface-default"];
    const textTokens = [
      "--color-text-primary",
      "--color-text-secondary",
      "--color-text-tertiary",
      "--color-action-primary",
      "--color-status-success-text",
      "--color-status-warning-text",
      "--color-status-danger-text",
      "--color-status-info-text",
    ];
    for (const foregroundToken of textTokens) {
      for (const backgroundToken of surfaces) {
        const foreground = resolveColor(foregroundToken, values);
        const background = resolveColor(backgroundToken, values);
        expect(contrast(foreground, background), `${foregroundToken} on ${backgroundToken}`).toBeGreaterThanOrEqual(4.5);
      }
    }
    for (const backgroundToken of surfaces) {
      const border = resolveColor("--color-border-strong", values);
      const background = resolveColor(backgroundToken, values);
      expect(contrast(border, background), `strong border on ${backgroundToken}`).toBeGreaterThanOrEqual(3);
    }
    // A floor says every level is legible; it does not say they are in order.
    // Light had tertiary darker than secondary — both above 4.5, so the floor
    // passed while the third level of the hierarchy outranked the second.
    for (const backgroundToken of surfaces) {
      const background = resolveColor(backgroundToken, values);
      const primary = contrast(resolveColor("--color-text-primary", values), background);
      const secondary = contrast(resolveColor("--color-text-secondary", values), background);
      const tertiary = contrast(resolveColor("--color-text-tertiary", values), background);
      expect(secondary, `secondary vs primary on ${backgroundToken}`).toBeLessThanOrEqual(primary);
      expect(tertiary, `tertiary vs secondary on ${backgroundToken}`).toBeLessThanOrEqual(secondary);
    }
  });

  // The colour layer is a contract the tests enforce; the size layer is not.
  // --space-* and --radius-* are declared and almost never consumed, so spacing
  // is decided by hand at every call site. Converting 748 values in one change
  // would be a rewrite of every layout with no way to verify it, so this is a
  // ratchet instead: the count may fall, never rise. Lower the baseline as you
  // convert; a rise means a new hand-picked value went in beside a token that
  // already says the same thing.
  //
  // The baseline was written as 758 when the ratchet was added, while the file
  // actually held 754 — four units of slack that later commits spent without
  // anyone deciding to. It is now the exact count, so any new hand-picked value
  // fails and lowering it is a deliberate edit.
  //
  // 743 → 724 when the resource-list row spec moved row padding, column gap and
  // cell gaps onto --resource-row-* / --space-* tokens.
  //
  // resource-list.css is counted alongside styles.css, and contributes zero.
  // The ratchet used to read the business stylesheet alone, which was the whole
  // file when it was written; once a design-system stylesheet started owning
  // spacing that pages had declared by hand, reading only styles.css would have
  // scored a value as converted for moving out of the file the count is taken
  // from. A stylesheet that owns spacing has to be inside the ratchet, or the
  // next one to own some is where the hand-picked values go.
  const bareSizeValueBaseline = 724;

  it("does not add bare spacing or radius values beyond the current baseline", () => {
    const styles = read("./styles.css") + read("./design-system/resource-list.css") + read("./design-system/resource-card.css");
    const declarations = styles.matchAll(
      /\b(?:gap|row-gap|column-gap|padding|padding-top|padding-right|padding-bottom|padding-left|margin|margin-top|margin-right|margin-bottom|margin-left|border-radius|inset)\s*:\s*([^;{}]*)/g,
    );
    const bare: string[] = [];
    for (const declaration of declarations) {
      bare.push(...(declaration[1].match(/(?<![\w.-])\d+px/g) ?? []));
    }
    expect(bare.length).toBeLessThanOrEqual(bareSizeValueBaseline);
  });

  // tokens.css states a 12px floor because CJK glyphs lose stroke separation
  // below it at 1x. For a long time that was a comment and nothing else: the
  // business stylesheet carried 106 declarations under the floor, and the pages
  // that needed reading most — the audit timeline, System Config, the developer
  // workbench — were the ones densest in 10px text. This is not a ratchet. A
  // size under the floor is a defect, so the bound is zero, and an exception has
  // to be argued for here rather than added quietly at a call site.
  const typeFloorPx = 12;

  /**
   * Absolute font sizes declared by `font-size` or the `font` shorthand. Sizes
   * reached through var(--font-size-*) resolve inside the design system and are
   * checked by the token assertion below instead.
   */
  function absoluteFontSizes(css: string): { declaration: string; px: number }[] {
    const sizes: { declaration: string; px: number }[] = [];
    for (const match of css.matchAll(/\bfont(?:-size)?\s*:\s*([^;{}]+)/g)) {
      const declaration = match[1].trim();
      // The size is the only length a font declaration carries: a `font`
      // shorthand's line-height is unitless here, and weight and family are not
      // lengths at all.
      for (const length of declaration.matchAll(/(?<![\w.-])(\d*\.?\d+)(px|rem)(?![\w-])/g)) {
        const amount = Number(length[1]);
        sizes.push({ declaration, px: length[2] === "px" ? amount : amount * 16 });
      }
    }
    return sizes;
  }

  it("declares the type floor as a token so business CSS has something to reach for", () => {
    const tokens = tokenValues(read("./design-system/tokens.css"));
    expect(tokens.get("--font-size-xs")).toBe(`${typeFloorPx}px`);
  });

  it("declares no visible text below the type floor in business CSS", () => {
    const styles = read("./styles.css");
    const below = absoluteFontSizes(styles)
      .filter((size) => size.px < typeFloorPx)
      .map((size) => size.declaration);
    expect(below, `font sizes below ${typeFloorPx}px: ${below.join(" | ")}`).toEqual([]);
  });

  // Body and control text must use the six-level type scale. A small explicit
  // allowlist keeps display headlines and glyph-only marks reviewable without
  // pretending their responsive/art-directed sizes are ordinary body tokens.
  // Adding an exception is therefore a design-system decision in this test,
  // not a literal that can arrive unnoticed in business CSS.
  const nonScaleTypeAllowlist = new Map<string, string[]>([
    ["h1", ["font-size:clamp(32px, 4vw, 42px)"]],
    [".brand strong", ["font:var(--font-weight-bold) 14px/1.2 var(--mono)"]],
    [".metric strong", ["font:var(--font-weight-medium) clamp(20px, 2vw, 30px)/1 var(--mono)"]],
    [".custody-summary-primary h2", ["font-size:25px"]],
    [".system-card h3", ["font-size:20px"]],
    [".login-story h1", ["font-size:clamp(34px, 4.2vw, 46px)"]],
    [".login-story > div > p:last-child", ["font-size:16px"]],
    [".login-panel h2", ["font-size:34px"]],
    [".count", ["font:20px var(--mono)"]],
    [".empty-mark", ["font:20px var(--mono)"]],
    [".first-run-action-arrow", ["font:var(--font-weight-medium) 1rem/1 var(--mono)"]],
  ]);

  it("keeps business font sizes on type tokens or the reviewed allowlist", () => {
    const styles = read("./styles.css");
    const offenders: string[] = [];
    for (const rule of styles.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      const selector = rule[1].trim();
      for (const declaration of rule[2].matchAll(/(?:^|;)\s*(font(?:-size)?)\s*:\s*([^;]+)/g)) {
        const property = declaration[1];
        const value = declaration[2].trim();
        if (value === "inherit" || value.includes("var(--font-size-")) continue;
        const normalized = `${property}:${value}`;
        if (!(nonScaleTypeAllowlist.get(selector) ?? []).includes(normalized)) {
          offenders.push(`${selector} { ${normalized} }`);
        }
      }
    }
    expect(offenders, `unreviewed type sizes:\n${offenders.join("\n")}`).toEqual([]);
  });

  // 400/500/600/800 are the four weights the system has. The stylesheet also
  // carried 650, 700, 750 and 900, written by hand at individual call sites.
  // 650 is the instructive one: it renders here because the macOS system face is
  // variable, and on a Windows CJK face it snaps to 600 or 700 — a platform
  // difference nobody sees while developing. Naming the token instead means the
  // weight is a decision the design system made once.
  it("declares no literal font weights in business CSS", () => {
    const styles = read("./styles.css");
    const literal = [
      ...Array.from(styles.matchAll(/font-weight\s*:\s*(\d+)/g), (match) => match[1]),
      // The `font` shorthand puts the weight first, before the size.
      ...Array.from(styles.matchAll(/\bfont\s*:\s*(\d{3})\b/g), (match) => match[1]),
    ];
    expect(literal, `literal font weights: ${literal.join(", ")}`).toEqual([]);
  });

  it("keeps literal colors and primitive-token consumption inside the design-system layer", () => {
    const stylesPath = "./styles.css";
    const sourceRoot = dirname(fileURLToPath(new URL(stylesPath, import.meta.url)));
    const offenders: string[] = [];
    const visit = (directory: string) => {
      for (const entry of readdirSync(directory)) {
        const path = join(directory, entry);
        const rel = relative(sourceRoot, path);
        if (rel === "design-system" || rel.startsWith(`design-system${process.platform === "win32" ? "\\" : "/"}`)) continue;
        if (statSync(path).isDirectory()) {
          visit(path);
          continue;
        }
        if (!/\.(css|ts|tsx)$/.test(path)) continue;
        if (/\.test\.(ts|tsx)$/.test(path)) continue;
        const source = readFileSync(path, "utf8");
        if (/#[0-9a-f]{3,8}\b|rgba?\(/i.test(source) || /var\(--p-/i.test(source)) offenders.push(rel);
      }
    };
    visit(sourceRoot);
    expect(offenders).toEqual([]);
  });
});

describe("component styling reaches the markup", () => {
  // A status variant says what a message means. Where it sits is the
  // container's business, because only the container knows its own header
  // padding — .notice.success once carried one panel's 22px inset and, being
  // declared later than every container rule, applied it in all of them. The
  // result was a success box that lined up with nothing.
  it("keeps horizontal insets out of the notice status variants", () => {
    const styles = read("./styles.css");
    // Every rule, not the first one that mentions the variant: the stylesheet
    // has several, and checking only the first passes without ever reading the
    // one that carries the colour.
    let checked = 0;
    for (const rule of styles.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      const [, selector, body] = rule;
      // A container qualifying the variant (".settings-form .notice.success")
      // is the container deciding layout, which is the arrangement this is
      // protecting. Only an unqualified variant rule is in scope.
      if (!/^\s*\.notice\.(success|error|warning)\s*$/.test(selector)) continue;
      checked++;
      const name = selector.trim();
      expect(body, `${name} sets a horizontal margin`).not.toMatch(/margin-(?:left|right)\s*:/);
      const shorthand = body.match(/(?:^|;)\s*margin\s*:\s*([^;]+)/);
      const values = shorthand ? shorthand[1].trim().split(/\s+/) : [];
      // margin: A            -> all four sides, horizontal included
      // margin: A B          -> B is horizontal
      // margin: A B C[ D]    -> B (and D) are horizontal
      const horizontal = values.length === 1 ? values[0] : values[1];
      expect(
        horizontal === undefined || horizontal === "0" || horizontal === "auto",
        `${name} sets a horizontal margin of ${horizontal} — layout belongs to the container`,
      ).toBe(true);
    }
    expect(checked, "no unqualified .notice status variant rules were found to check").toBe(3);
  });

  // A class name in the markup with no rule anywhere renders at whatever the
  // cascade happens to give it, which is how an introductory sentence ended up
  // outweighing the dialog heading above it. jsdom cannot see that, so this is
  // a ratchet like the spacing one: the count may fall, never rise.
  const unstyledClassBaseline = 12;

  it("does not add class names the stylesheets never style", () => {
    const cssFiles = ["./styles.css", "./design-system/index.css", "./design-system/components.css", "./design-system/resource-list.css", "./design-system/resource-card.css", "./design-system/tokens.css"];
    let css = "";
    for (const file of cssFiles) {
      try {
        css += read(file);
      } catch {
        // A stylesheet that does not exist contributes nothing.
      }
    }
    const defined = new Set(Array.from(css.matchAll(/\.([a-zA-Z][\w-]*)/g), (match) => match[1]));

    const used = new Set<string>();
    const visit = (path: string) => {
      for (const entry of readdirSync(path)) {
        const full = join(path, entry);
        if (statSync(full).isDirectory()) {
          visit(full);
          continue;
        }
        if (!entry.endsWith(".tsx") || entry.includes(".test.")) continue;
        const source = readFileSync(full, "utf8");
        for (const match of source.matchAll(/className=(?:"([^"]*)"|\{`([^`]*)`\})/g)) {
          const raw = (match[1] ?? match[2] ?? "").replace(/\$\{[^}]*\}/g, " ");
          for (const name of raw.split(/\s+/)) if (name) used.add(name);
        }
      }
    };
    // Held in a variable on purpose: Vite statically rewrites a literal
    // new URL("./x", import.meta.url) into an asset URL, which is not a file
    // path. The sibling test above resolves its root the same way.
    const stylesPath = "./styles.css";
    visit(dirname(fileURLToPath(new URL(stylesPath, import.meta.url))));

    const unstyled = Array.from(used).filter((name) => !defined.has(name)).sort();
    expect(unstyled.length, `unstyled classes: ${unstyled.join(", ")}`).toBeLessThanOrEqual(unstyledClassBaseline);
  });

  // A length flex-basis binds to the parent's main axis, so one declaration is a
  // width inside a row and a height inside a column. This control is used in
  // both: a row of buttons on the provider and deployment lists, and
  // .inline-test-cell — a column — in the route table. A 196px basis was read as
  // a height there and stretched every route row from 77px to 229px. The width
  // is declared outright, so the basis has to stay axis-neutral.
  it("keeps the inline test control's flex basis axis-neutral", () => {
    const rule = read("./styles.css").match(/^\.inline-test-control \{([^}]*)\}/m)?.[1];
    expect(rule, ".inline-test-control base rule not found").toBeDefined();
    expect(rule).toMatch(/width:\s*196px/);
    expect(rule).toMatch(/flex:\s*0\s+0\s+auto/);
  });
});
