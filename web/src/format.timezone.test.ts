import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

// Vitest resolves import.meta.url to a browser-style URL under the jsdom
// environment, so anchor on the project root instead.
const SOURCE_ROOT = resolve(process.cwd(), "src") + "/";
const ALLOWED = new Set(["format.ts", "timezone.ts"]);

function sourceFiles(directory: string): string[] {
  return readdirSync(directory).flatMap((entry) => {
    const path = join(directory, entry);
    if (statSync(path).isDirectory()) return sourceFiles(path);
    return /\.tsx?$/.test(entry) && !entry.includes(".test.") ? [path] : [];
  });
}

/**
 * Intl.DateTimeFormat defaults to the browser's zone. That default is what put
 * chart axes and the totals beside them on different days, so the console
 * formats instants in exactly one place — where the zone is a required
 * argument. A new caller reaching for Intl directly would reintroduce the bug
 * silently, in whichever page happened to add it.
 */
describe("time rendering is centralized", () => {
  it("formats instants only through format.ts", () => {
    const offenders = sourceFiles(SOURCE_ROOT)
      .filter((path) => !ALLOWED.has(path.slice(SOURCE_ROOT.length)))
      .filter((path) => readFileSync(path, "utf8").includes("Intl.DateTimeFormat"))
      .map((path) => path.slice(SOURCE_ROOT.length));
    expect(offenders).toEqual([]);
  });

  it("never formats an instant without naming a zone", () => {
    const constructions = readFileSync(join(SOURCE_ROOT, "format.ts"), "utf8")
      .split("\n")
      .filter((line) => line.includes("new Intl.DateTimeFormat"));
    expect(constructions.length).toBeGreaterThan(0);
    for (const line of constructions) {
      expect(line).toContain("timeZone");
    }
  });
});
