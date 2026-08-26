import { describe, expect, it } from "vitest";
import { formatAge } from "./format";

// The card's one line about a deployment ends in how long ago the verdict was
// reached, because "通过 · 151ms" with no time attached reads as current for a
// test run two months back against a model since withdrawn.
//
// `now` is a parameter so that can be asserted at all — a function reading the
// wall clock mid-run has no output a test can name. It had the parameter and no
// tests: every unit boundary, and the rule about clocks that run ahead, went
// unchecked.
const NOW = Date.parse("2026-08-26T12:00:00Z");
const ago = (millis: number) => new Date(NOW - millis).toISOString();

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

describe("formatAge", () => {
  // The console renders in the session locale, and these are the strings the
  // default one produces. Asserting the rendered text rather than a shape is
  // deliberate: the units are chosen by a table in this file, and a table that
  // silently picks "hour" for a two-day-old verdict is exactly the defect the
  // caller could not see.
  it("names each unit at its own boundary", () => {
    expect(formatAge(ago(MINUTE), NOW)).toBe("1分钟前");
    expect(formatAge(ago(59 * MINUTE), NOW)).toBe("59分钟前");
    expect(formatAge(ago(HOUR), NOW)).toBe("1小时前");
    expect(formatAge(ago(25 * HOUR), NOW)).toBe("昨天");
    expect(formatAge(ago(30 * DAY), NOW)).toBe("上个月");
    expect(formatAge(ago(365 * DAY), NOW)).toBe("去年");
  });

  // The step below the smallest unit is not "0 minutes ago" spelled out; it is
  // the resting phrase, and it also absorbs the case below.
  it("says just now below a minute", () => {
    expect(formatAge(ago(0), NOW)).toBe("此刻");
    expect(formatAge(ago(MINUTE - 1), NOW)).toBe("此刻");
  });

  // A browser clock a few seconds ahead of the server is ordinary, and the card
  // must not answer it with "in 4 seconds" — a verdict that has not happened
  // yet reads as a bug in Halro rather than as a clock.
  it("does not render a future for a clock that runs ahead", () => {
    expect(formatAge(new Date(NOW + 4_000).toISOString(), NOW)).toBe("此刻");
    expect(formatAge(new Date(NOW + 30_000).toISOString(), NOW)).toBe("此刻");
  });

  // Past a minute the sign is real information, not skew, and is kept.
  it("keeps a genuinely future instant in the future", () => {
    expect(formatAge(new Date(NOW + 2 * HOUR).toISOString(), NOW)).toBe("2小时后");
  });

  // Absent is not zero: a deployment never tested has no age, and the caller
  // joins this into a line with a separator, where "" drops out and "0" does not.
  it("returns nothing rather than something for a missing or unreadable value", () => {
    expect(formatAge(undefined, NOW)).toBe("");
    expect(formatAge("", NOW)).toBe("");
    expect(formatAge("not a date", NOW)).toBe("");
  });

  it("takes the instant in each form the records use", () => {
    expect(formatAge(new Date(NOW - 2 * HOUR), NOW)).toBe("2小时前");
    expect(formatAge(NOW - 2 * HOUR, NOW)).toBe("2小时前");
  });
});
