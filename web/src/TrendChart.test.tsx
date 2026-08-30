import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Bucket } from "./types";
import TrendChart, { axisGutter } from "./TrendChart";
import { hourlyTrendPoints } from "./trend";

const chartMocks = vi.hoisted(() => ({
  setSize: vi.fn(),
  setData: vi.fn(),
  setScale: vi.fn(),
  destroy: vi.fn(),
}));

vi.mock("uplot", () => ({
  default: class {
    setSize = chartMocks.setSize;
    setData = chartMocks.setData;
    setScale = chartMocks.setScale;
    destroy = chartMocks.destroy;
  },
}));

describe("TrendChart", () => {
  let resize: ResizeObserverCallback;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("ResizeObserver", class {
      constructor(callback: ResizeObserverCallback) { resize = callback; }
      observe() {}
      unobserve() {}
      disconnect() {}
    });
  });

  it("exposes the selected metric summary and respects a narrow container width", () => {
    render(<TrendChart points={hourlyTrendPoints([bucket()], Date.parse(bucket().hour) + 3_600_000)} metric="tokens" />);

    expect(screen.getByRole("img", { name: /词元用量.*18/ })).toBeVisible();

    act(() => {
      resize([{ contentRect: { width: 240 } } as ResizeObserverEntry], {} as ResizeObserver);
    });
    expect(chartMocks.setSize).toHaveBeenLastCalledWith({ width: 240, height: 280 });
  });
});

function bucket(): Bucket {
  return {
    hour: new Date(Math.floor(Date.now() / 1000) * 1000).toISOString(),
    requests: 2,
    request_errors: 0,
    request_latency_samples: 2,
    request_latency_p50_millis: 5,
    request_latency_p95_millis: 10,
    attempts: 2,
    input_tokens: 10,
    output_tokens: 8,
    estimated_input_tokens: 1,
    estimated_output_tokens: 2,
    cost_micros_usd: 0,
    unknown_attempts: 0,
    errors: 0,
    latency_millis: 0,
  };
}

describe("chart axis gutter", () => {
  it("widens for a label that would not fit uPlot's fixed gutter", () => {
    // The success-rate axis draws "100.0%", which is wider than the 50px uPlot
    // reserves by default and was being clipped to "00.0%".
    expect(axisGutter(["0.0%", "100.0%"])).toBeGreaterThan(50);
    expect(axisGutter(["0.0%", "100.0%"])).toBeGreaterThanOrEqual("100.0%".length * 7);
  });

  it("keeps the default gutter for narrow labels", () => {
    expect(axisGutter(["0", "5"])).toBe(50);
    expect(axisGutter(null)).toBe(50);
  });
});
