import type { AlignedData } from "uplot";
import type { Bucket } from "./types";

export const TREND_WINDOW_SECONDS = 7 * 24 * 60 * 60;

export interface TrendSeries {
  data: AlignedData;
  range: [number, number];
}

export type TrendMetric = "requests" | "tokens" | "cost" | "errors";

export interface TrendSummary {
  value: number;
}

export function buildTrendSeries(
  buckets: Bucket[],
  metric: TrendMetric = "requests",
  nowMillis = Date.now(),
): TrendSeries {
  const end = Math.floor(nowMillis / 1000);
  const start = end - TREND_WINDOW_SECONDS;
  const points = buckets
    .map((bucket) => ({ bucket, timestamp: Date.parse(bucket.hour) / 1000 }))
    .filter(({ timestamp }) => Number.isFinite(timestamp) && timestamp >= start && timestamp <= end)
    .sort((left, right) => left.timestamp - right.timestamp);

  if (points.length === 0) {
    return { data: [[start, end], [0, 0]], range: [start, end] };
  }

  return {
    data: [
      points.map(({ timestamp }) => timestamp),
      points.map(({ bucket }) => trendValue(bucket, metric)),
    ],
    range: [start, end],
  };
}

function trendValue(bucket: Bucket, metric: TrendMetric) {
  switch (metric) {
    case "tokens": return reportedTokens(bucket);
    case "cost": return bucket.cost_micros_usd / 1_000_000;
    case "errors": return bucket.attempts ? bucket.errors / bucket.attempts * 100 : 0;
    default: return bucket.requests;
  }
}

export function reportedTokens(bucket: Bucket) {
  return Math.max(
    0,
    bucket.input_tokens + bucket.output_tokens -
      (bucket.estimated_input_tokens ?? 0) - (bucket.estimated_output_tokens ?? 0),
  );
}

export function summarizeTrend(
  buckets: Bucket[],
  metric: TrendMetric,
  nowMillis = Date.now(),
): TrendSummary {
  const { data } = buildTrendSeries(buckets, metric, nowMillis);
  let value = 0;
  for (const point of data[1]) value += point ?? 0;
  return { value };
}
