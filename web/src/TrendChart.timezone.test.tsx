import { render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Bucket } from "./types";
import TrendChart from "./TrendChart";
import { adoptTimeContext, resetAccountingTimeZone } from "./timezone";
import { timeContext } from "./test/fixtures";

const captured = vi.hoisted(() => ({ options: [] as Record<string, unknown>[] }));

vi.mock("uplot", () => {
  class FakeUPlot {
    constructor(options: Record<string, unknown>) {
      captured.options.push(options);
    }
    static tzDate(date: Date, tzName: string) {
      // Stands in for uPlot's real shifting helper; the assertions only need to
      // see which zone name reached it.
      FakeUPlot.lastZone = tzName;
      return date;
    }
    static lastZone = "";
    setSize = vi.fn();
    setData = vi.fn();
    setScale = vi.fn();
    destroy = vi.fn();
  }
  return { default: FakeUPlot };
});

describe("TrendChart time zone", () => {
  beforeEach(() => {
    captured.options.length = 0;
    vi.stubGlobal("ResizeObserver", class {
      observe() {}
      unobserve() {}
      disconnect() {}
    });
  });
  afterEach(() => resetAccountingTimeZone());

  // The defect this guards: uPlot draws a time axis in the browser's zone
  // unless given tzDate, so the chart was labelling a different day from the
  // totals rendered beside it, which are summed over the server's day.
  it("draws its axis in the server's accounting zone, not the browser's", async () => {
    adoptTimeContext(timeContext({ accounting_timezone: "Asia/Shanghai" }));
    render(<TrendChart buckets={[bucket()]} metric="requests" />);

    const options = captured.options.at(-1);
    expect(options).toBeDefined();
    const tzDate = options?.tzDate as ((timestamp: number) => Date) | undefined;
    expect(typeof tzDate).toBe("function");

    const uPlotModule = await import("uplot");
    const FakeUPlot = uPlotModule.default as unknown as { lastZone: string };
    tzDate?.(Date.parse("2026-08-05T17:30:00Z") / 1000);
    expect(FakeUPlot.lastZone).toBe("Asia/Shanghai");
  });

  it("rebuilds the chart when the accounting zone changes", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "UTC" }));
    const view = render(<TrendChart buckets={[bucket()]} metric="requests" />);
    const before = captured.options.length;

    adoptTimeContext(timeContext({ accounting_timezone: "America/New_York" }));
    view.rerender(<TrendChart buckets={[bucket()]} metric="requests" />);

    expect(captured.options.length).toBeGreaterThan(before);
  });
});

function bucket(overrides: Partial<Bucket> = {}): Bucket {
  return {
    hour: new Date(Date.now() - 3_600_000).toISOString(),
    requests: 5,
    attempts: 5,
    input_tokens: 10,
    output_tokens: 5,
    cost_micros_usd: 1_000,
    errors: 0,
    latency_millis: 100,
    ...overrides,
  };
}
