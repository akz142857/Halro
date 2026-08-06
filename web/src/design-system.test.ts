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
  // is decided by hand at every call site. Converting 758 values in one change
  // would be a rewrite of every layout with no way to verify it, so this is a
  // ratchet instead: the count may fall, never rise. Lower the baseline as you
  // convert; a rise means a new hand-picked value went in beside a token that
  // already says the same thing.
  const bareSizeValueBaseline = 758;

  it("does not add bare spacing or radius values beyond the current baseline", () => {
    const styles = read("./styles.css");
    const declarations = styles.matchAll(
      /\b(?:gap|row-gap|column-gap|padding|padding-top|padding-right|padding-bottom|padding-left|margin|margin-top|margin-right|margin-bottom|margin-left|border-radius|inset)\s*:\s*([^;{}]*)/g,
    );
    const bare: string[] = [];
    for (const declaration of declarations) {
      bare.push(...(declaration[1].match(/(?<![\w.-])\d+px/g) ?? []));
    }
    expect(bare.length).toBeLessThanOrEqual(bareSizeValueBaseline);
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
    const cssFiles = ["./styles.css", "./design-system/index.css", "./design-system/components.css", "./design-system/tokens.css"];
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
});
