import { afterEach, describe, expect, it } from "vitest";
import { formatInstant } from "./format";
import {
  DEFAULT_TIME_ZONE,
  accountingTimeZone,
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
