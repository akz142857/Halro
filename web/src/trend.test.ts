import { describe, expect, it } from "vitest";
import { buildTrendSeries, summarizeTrend, TREND_WINDOW_SECONDS } from "./trend";
import type { Bucket } from "./types";

const now = Date.parse("2026-07-31T12:00:00Z");

describe("dashboard trend series", () => {
  it("pins the chart to seven days and zero-fills missing request hours", () => {
    const series = buildTrendSeries([bucket("2026-07-31T11:00:00Z")], "requests", now);
    expect(series.range[1] - series.range[0]).toBe(TREND_WINDOW_SECONDS);
    expect(series.range[1]).toBe(now / 1000);
    expect(series.data[0]).toHaveLength(169);
    const point = series.data[0].indexOf(Date.parse("2026-07-31T11:00:00Z") / 1000);
    expect(series.data[1][point]).toBe(1);
    expect(series.data[2][point]).toBe(0);
    expect(series.data[1][point - 1]).toBe(0);
  });

  it("separates reported and conservatively estimated token usage", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.input_tokens = 16;
    point.output_tokens = 16_551;
    point.estimated_input_tokens = 9;
    point.estimated_output_tokens = 16_384;
    const series = buildTrendSeries([point], "tokens", now);
    const index = series.data[0].indexOf(Date.parse(point.hour) / 1000);
    expect(series.data[1][index]).toBe(174);
    expect(series.data[2][index]).toBe(16_393);
  });

  it("leaves no-traffic quality hours blank instead of implying perfect health", () => {
    const series = buildTrendSeries([], "success_rate", now);
    expect(Array.from(series.data[1]).every((value) => value == null)).toBe(true);
  });

  it("separates confirmed and estimated known cost", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.cost_micros_usd = 2_500_000;
    point.estimated_cost_micros_usd = 500_000;
    const series = buildTrendSeries([point], "cost", now);
    const index = series.data[0].indexOf(Date.parse(point.hour) / 1000);
    expect(series.data[1][index]).toBe(2);
    expect(series.data[2][index]).toBe(.5);
  });

  it("summarizes request success as a weighted rate and latency as peak hourly P95", () => {
    const first = bucket("2026-07-31T10:00:00Z");
    first.requests = 9;
    first.request_errors = 0;
    first.request_latency_p95_millis = 200;
    const second = bucket("2026-07-31T11:00:00Z");
    second.requests = 1;
    second.request_errors = 1;
    second.request_latency_p95_millis = 900;
    expect(summarizeTrend([first, second], "success_rate", now)).toEqual({ value: 90 });
    expect(summarizeTrend([first, second], "latency_p95", now)).toEqual({ value: 900 });
  });
});

function bucket(hour: string): Bucket {
  return {
    hour,
    requests: 1,
    request_errors: 0,
    request_latency_samples: 1,
    request_latency_p50_millis: 10,
    request_latency_p95_millis: 10,
    attempts: 1,
    input_tokens: 0,
    output_tokens: 0,
    cost_micros_usd: 0,
    unknown_attempts: 0,
    errors: 0,
    latency_millis: 0,
  };
}
