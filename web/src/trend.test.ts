import { describe, expect, it } from "vitest";
import { buildTrendSeries, summarizeTrend, TREND_WINDOW_SECONDS } from "./trend";
import type { Bucket } from "./types";

const now = Date.parse("2026-07-31T12:00:00Z");

describe("dashboard trend series", () => {
  it("pins a single data point to a seven-day time range", () => {
    const series = buildTrendSeries([bucket("2026-07-31T11:00:00Z")], "requests", now);

    expect(series.range[1] - series.range[0]).toBe(TREND_WINDOW_SECONDS);
    expect(series.range[1]).toBe(now / 1000);
    expect(series.data[0]).toEqual([Date.parse("2026-07-31T11:00:00Z") / 1000]);
  });

  it("excludes conservative estimates from reported token usage", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.input_tokens = 16;
    point.output_tokens = 16_551;
    point.estimated_input_tokens = 9;
    point.estimated_output_tokens = 16_384;

    expect(buildTrendSeries([point], "tokens", now).data[1]).toEqual([174]);
  });

  it("uses zero anchors for an empty seven-day window", () => {
    const series = buildTrendSeries([], "requests", now);
    expect(series.data).toEqual([
      [now / 1000 - TREND_WINDOW_SECONDS, now / 1000],
      [0, 0],
    ]);
  });

  it("builds cost and attempt error-rate series with explicit units", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.cost_micros_usd = 2_500_000;
    point.attempts = 4;
    point.errors = 1;

    expect(buildTrendSeries([point], "cost", now).data[1]).toEqual([2.5]);
    expect(buildTrendSeries([point], "errors", now).data[1]).toEqual([25]);
  });

  it("summarizes the selected metric for the accessible description", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.requests = 3;
    point.input_tokens = 100;
    point.output_tokens = 50;
    point.estimated_output_tokens = 25;

    expect(summarizeTrend([point], "requests", now)).toEqual({ value: 3 });
    expect(summarizeTrend([point], "tokens", now)).toEqual({ value: 125 });
  });
});

function bucket(hour: string): Bucket {
  return {
    hour,
    requests: 1,
    attempts: 1,
    input_tokens: 0,
    output_tokens: 0,
    cost_micros_usd: 0,
    errors: 0,
    latency_millis: 0,
  };
}
