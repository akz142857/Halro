import { afterEach, describe, expect, it } from "vitest";
import { formatInstant } from "./format";
import {
  DEFAULT_TIME_ZONE,
  accountingTimeZone,
  zonedInputToISO,
  adoptTimeContext,
  isSupportedTimeZone,
  resetAccountingTimeZone,
  setAccountingTimeZone,
} from "./timezone";
import { timeContext } from "./test/fixtures";

afterEach(() => resetAccountingTimeZone());

describe("accounting time zone", () => {
  it("adopts the zone the server reports", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "America/New_York" }));
    expect(accountingTimeZone()).toBe("America/New_York");
  });

  it("falls back to UTC rather than throwing on a zone this browser cannot format", () => {
    setAccountingTimeZone("Mars/Olympus_Mons");
    expect(accountingTimeZone()).toBe(DEFAULT_TIME_ZONE);
  });

  it("recognizes a real zone", () => {
    expect(isSupportedTimeZone("Asia/Shanghai")).toBe(true);
    expect(isSupportedTimeZone("")).toBe(false);
  });
});

describe("formatInstant", () => {
  // The whole point of threading a zone through: the same instant is a
  // different calendar day depending on where the day is measured from.
  it("renders one instant on different days in different zones", () => {
    const instant = "2026-08-05T17:30:00Z";
    expect(formatInstant(instant, "Asia/Shanghai", "date")).not.toBe(
      formatInstant(instant, "UTC", "date"),
    );
    expect(formatInstant(instant, "Asia/Shanghai", "date")).toContain("2026");
  });

  it("does not fall back to the host zone", () => {
    const instant = "2026-08-05T17:30:00Z";
    const shanghai = formatInstant(instant, "Asia/Shanghai");
    const utc = formatInstant(instant, "UTC");
    expect(shanghai).not.toBe(utc);
  });

  it("renders a placeholder for missing and unparseable values", () => {
    expect(formatInstant(undefined, "UTC")).toBe("—");
    expect(formatInstant("", "UTC")).toBe("—");
    expect(formatInstant("not a date", "UTC")).toBe("—");
  });
});

describe("zonedInputToISO", () => {
  // A datetime-local control has no zone, so the browser's Date constructor
  // resolves its value against the viewer's. Every timestamp beside the filter
  // is rendered in the accounting zone, so reading the input any other way asks
  // for a window offset from the one on screen, silently.
  it("reads the wall clock in the accounting zone, not the viewer's", () => {
    expect(zonedInputToISO("2026-08-07T00:00", "Asia/Shanghai")).toBe("2026-08-06T16:00:00.000Z");
    expect(zonedInputToISO("2026-08-07T00:00", "UTC")).toBe("2026-08-07T00:00:00.000Z");
    expect(zonedInputToISO("2026-08-07T00:00", "America/New_York")).toBe("2026-08-07T04:00:00.000Z");
  });

  // The offset in force differs on either side of a transition, so resolving
  // with the offset guessed from the wall-clock reading alone lands an hour out
  // for readings near one.
  it("resolves readings around a daylight-saving transition", () => {
    // New York moves to -04:00 at 02:00 local on 2026-03-08.
    expect(zonedInputToISO("2026-03-08T01:00", "America/New_York")).toBe("2026-03-08T06:00:00.000Z");
    expect(zonedInputToISO("2026-03-08T03:00", "America/New_York")).toBe("2026-03-08T07:00:00.000Z");
  });

  it("returns an empty string for an incomplete value", () => {
    expect(zonedInputToISO("", "UTC")).toBe("");
    expect(zonedInputToISO("2026-08-07", "UTC")).toBe("");
  });
});
