import { describe, expect, it } from "vitest";
import { buildTrendSeries, hourlyTrendPoints, periodTrendPoints, summarizeTrend, TREND_WINDOW_SECONDS } from "./trend";
import type { Bucket, SummaryBucket } from "./types";

const now = Date.parse("2026-07-31T12:00:00Z");

describe("dashboard trend series", () => {
  it("pins the chart to seven days and zero-fills missing request hours", () => {
    const series = buildTrendSeries(hourlyTrendPoints([bucket("2026-07-31T11:00:00Z")], now), "requests");
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
    const series = buildTrendSeries(hourlyTrendPoints([point], now), "tokens");
    const index = series.data[0].indexOf(Date.parse(point.hour) / 1000);
    expect(series.data[1][index]).toBe(174);
    expect(series.data[2][index]).toBe(16_393);
  });

  it("leaves no-traffic quality hours blank instead of implying perfect health", () => {
    const series = buildTrendSeries(hourlyTrendPoints([], now), "success_rate");
    expect(Array.from(series.data[1]).every((value) => value == null)).toBe(true);
  });

  it("separates confirmed and estimated known cost", () => {
    const point = bucket("2026-07-31T11:00:00Z");
    point.cost_micros_usd = 2_500_000;
    point.estimated_cost_micros_usd = 500_000;
    const series = buildTrendSeries(hourlyTrendPoints([point], now), "cost");
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
    expect(summarizeTrend(hourlyTrendPoints([first, second], now), "success_rate")).toEqual({ value: 90 });
    expect(summarizeTrend(hourlyTrendPoints([first, second], now), "latency_p95")).toEqual({ value: 900 });
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

describe("summary trend series", () => {
  it("charts accounting periods that ended long before now", () => {
    const series = buildTrendSeries(periodTrendPoints([
      summaryBucket("2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-01"),
      summaryBucket("2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z", "2026-02"),
    ]), "cost");
    expect(series.data[0]).toEqual([
      Date.parse("2026-01-01T00:00:00Z") / 1000,
      Date.parse("2026-02-01T00:00:00Z") / 1000,
    ]);
    expect(series.range[1]).toBe(Date.parse("2026-02-01T00:00:00Z") / 1000);
  });

  it("keeps a 25-hour accounting day as a single point", () => {
    const points = periodTrendPoints([
      summaryBucket("2026-10-24T22:00:00Z", "2026-10-25T23:00:00Z", "2026-10-25"),
    ]);
    expect(points).toHaveLength(1);
    expect(points[0].startSeconds).toBe(Date.parse("2026-10-24T22:00:00Z") / 1000);
  });

  it("reads a period with no traffic as zero spend, not as a missing measurement", () => {
    const series = buildTrendSeries(periodTrendPoints([
      summaryBucket("2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z", "2026-08-01"),
      summaryBucket("2026-08-03T00:00:00Z", "2026-08-04T00:00:00Z", "2026-08-03"),
    ]), "cost");
    expect(series.data[0]).toHaveLength(3);
    expect(series.data[0][1]).toBe(Date.parse("2026-08-02T00:00:00Z") / 1000);
    expect(series.data[1][1]).toBe(0);
  });
});

function summaryBucket(start: string, end: string, period: string): SummaryBucket {
  return {
    period, start, end,
    requests: 1, request_errors: 0, request_latency_samples: 1, request_latency_p95_millis: 10,
    attempts: 1, errors: 0, input_tokens: 0, output_tokens: 0,
    estimated_input_tokens: 0, estimated_output_tokens: 0,
    provider_cached_input_tokens: 0, provider_cache_write_input_tokens: 0, provider_reasoning_tokens: 0,
    cost_micros_usd: 1_000_000, estimated_cost_micros_usd: 0, unknown_attempts: 0,
    latency_millis: 10, attempt_latency_samples: 1, attempt_latency_p95_millis: 10,
    latency_approximate: true,
  };
}
