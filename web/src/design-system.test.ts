import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8");

/** Custom-property names declared (left of `:`) in a CSS file. */
function declaredTokens(css: string): Set<string> {
  const names = new Set<string>();
  for (const match of css.matchAll(/(--[a-z0-9-]+)\s*:/gi)) names.add(match[1]);
  return names;
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
});
