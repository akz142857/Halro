import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Bucket } from "./types";
import TrendChart from "./TrendChart";

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
    render(<TrendChart buckets={[bucket()]} metric="tokens" />);

    expect(screen.getByRole("img", { name: /令牌用量.*18/ })).toBeVisible();

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
